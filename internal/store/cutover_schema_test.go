package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// newCutoverDB gives the caller a database of its own, created empty.
//
// The shared test database cannot answer the question this file asks. Two of
// the assertions below are about what a database looks like when InitDB runs on
// it for the FIRST time, and on the shared one TestMain has already run InitDB
// (main_test.go) and any neighbouring test may have reshaped it since. "Fresh"
// there is a fiction, and a test that believes it would be pinning the order
// the package happens to run in.
func newCutoverDB(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open the admin connection: %v", err)
	}
	defer admin.Close()

	// One database per test, named after it: a leftover from a killed run is
	// then traceable to the test that made it rather than anonymous.
	name := "cutover_" + fmt.Sprintf("%x", time.Now().UnixNano())
	// nosemgrep: string-formatted-query - the name is generated here, not user input
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("drop a stale %s: %v", name, err)
	}
	// nosemgrep: string-formatted-query - the name is generated here, not user input
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("cannot create a database for the cutover test (%v); "+
			"TEST_DB_DSN must name a user allowed to CREATE DATABASE", err)
	}

	s, err := NewStore(replaceDBName(t, dsn, name))
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		s.Close()
		dropper, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Errorf("reopen to drop %s: %v", name, err)
			return
		}
		defer dropper.Close()
		// nosemgrep: string-formatted-query - the name is generated here, not user input
		if _, err := dropper.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return s
}

func replaceDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DB_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// TestCutoverSequence is the one test that matters here, and it is end to end
// because what has to hold is the SEQUENCE, not its steps.
//
// Every step is executable on its own and proves nothing on its own: InitDB
// must survive a legacy database, the gate must refuse it, the reset must
// repair it, and the gate must then pass - in that order, on the same database,
// with the schedules still writable afterwards.
func TestCutoverSequence(t *testing.T) {
	s := newCutoverDB(t)

	// 1. A database from before the revision model. InitDB has to run on it
	//    first, because that is what the binary does before anything else.
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB on an empty database: %v", err)
	}
	makeLegacyShapeWithoutCleanup(t, s)
	seedLegacySchedule(t, s, "devops", "sched-legacy", "alice")

	// 2. InitDB again, as the upgraded binary runs it. It must not fail, and
	//    it must not tighten anything: the reset has not run yet.
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB on a legacy database: %v", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules`); n != 1 {
		t.Fatalf("InitDB changed the legacy rows: %d schedules", n)
	}

	// 3. Starting the server here is the deploy mistake this whole sprint is
	//    about, and it has to be refused in words an operator can act on.
	requireLegacyShapeRefused(t, s.RequireCutoverSchema())

	// 4. The step that was skipped.
	res, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if res.SchedulesDeleted != 1 || !res.PhysicalCleanup {
		t.Fatalf("reset result = %+v, want one schedule deleted and the cleanup done", res)
	}

	// 5. InitDB and the gate both pass now, and InitDB does not undo the
	//    cleanup by recreating what it just dropped.
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB after the reset: %v", err)
	}
	if err := s.RequireCutoverSchema(); err != nil {
		t.Fatalf("the schema is not final after the reset: %v", err)
	}

	// 6. The database is not merely clean, it works: a schedule can be created
	//    and rendered on the shape the cleanup left behind.
	seedTeam(t, s, "devops2", "alice", "bob")
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev, err := createViaSave(context.Background(),
		newTestScheduleService(s, start), "devops2", revTestConfig(), "", nil)
	if err != nil {
		t.Fatalf("create a schedule on the final schema: %v", err)
	}
	var root *scheduleconfig.ScheduleRoot
	if err := withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
		var err error
		root, err = view.GetScheduleRootByTeam(context.Background(), "devops2")
		return err
	}); err != nil {
		t.Fatalf("read the root back: %v", err)
	}
	if !root.HistoryCompleteFrom.Equal(start) {
		t.Fatalf("history_complete_from = %v, want %v", root.HistoryCompleteFrom, start)
	}

	// 7. A second reset must not touch what was just created. This is the
	//    guarantee that makes the command safe to rerun on a live system.
	again, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if !again.AlreadyApplied {
		t.Fatalf("second reset = %+v, want AlreadyApplied", again)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE id = $1`, rev.ScheduleID); n != 1 {
		t.Fatal("the second reset deleted the schedule created after the first")
	}
}

// makeLegacyShapeWithoutCleanup is makeSchemaLegacy without the restore: this
// database is thrown away whole, so restoring it would be work done for nobody.
func makeLegacyShapeWithoutCleanup(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(legacyScheduleFixtureDDL); err != nil {
		t.Fatalf("build the pre-revision schema: %v", err)
	}
}

// TestFreshDatabaseReachesTheFinalShape: a new installation needs no reset, so
// the gate must let it start. The reset is for upgrades and is not meaningful
// here - which is why the refusal is worded about the legacy shape and not
// about a missing marker.
func TestFreshDatabaseReachesTheFinalShape(t *testing.T) {
	s := newCutoverDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	if err := s.RequireCutoverSchema(); err != nil {
		t.Fatalf("a fresh database must start without a reset: %v", err)
	}
	for _, table := range legacyScheduleTables {
		if legacyTableExists(t, s.db, table) {
			t.Errorf("InitDB created the legacy table %s", table)
		}
	}
	for _, column := range legacyScheduleColumns {
		if scheduleColumnPresent(t, s.db, column) {
			t.Errorf("InitDB created the mutable column schedules.%s", column)
		}
	}
	// The live columns the write path needs. created_at and updated_at are the
	// two most likely to be lost with the mutable block, and losing them fails
	// nothing until the first schedule is created.
	for _, column := range []string{
		"id", "team_id", "config_version", "history_complete_from",
		"deleted_at", "created_at", "updated_at",
	} {
		if !scheduleColumnPresent(t, s.db, column) {
			t.Errorf("schedules.%s is missing from a fresh database", column)
		}
	}
}

// TestEmptyLegacyDatabaseIsRefused is why the gate asks about the shape and not
// about the data.
//
// The obvious check - `SELECT count(*) FROM schedules WHERE
// history_complete_from IS NULL` - returns zero here and would wave this
// database through with its pre-revision schema fully intact.
func TestEmptyLegacyDatabaseIsRefused(t *testing.T) {
	s := newCutoverDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	makeLegacyShapeWithoutCleanup(t, s)

	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE history_complete_from IS NULL`); n != 0 {
		t.Fatalf("the fixture must be empty for this test to mean anything, got %d rows", n)
	}
	requireLegacyShapeRefused(t, s.RequireCutoverSchema())
}

// TestFreshDatabaseHasTheRevisionExclusionConstraint: btree_gist used to be
// created by the pre-revision schema, which is gone. The constraint swallows a
// missing extension with a NOTICE, so losing the extension would remove the
// non-overlap invariant silently, and only on fresh databases.
func TestFreshDatabaseHasTheRevisionExclusionConstraint(t *testing.T) {
	s := newCutoverDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	var available bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')`).Scan(&available); err != nil {
		t.Fatalf("check for btree_gist: %v", err)
	}
	if !available {
		t.Skip("btree_gist is not installed in this PostgreSQL; the constraint is skipped by design")
	}

	var present bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = 'no_overlapping_schedule_revisions')`).
		Scan(&present); err != nil {
		t.Fatalf("check for the constraint: %v", err)
	}
	if !present {
		t.Fatal("the revision exclusion constraint was not created; btree_gist left with the legacy schema")
	}
}

// TestLegacyFixtureBuildsARealLegacyShape guards the fixture itself, on the
// SHARED database it borrows.
//
// Its other half - that the borrowing is given back - is asserted by the
// cleanup, which ends in RequireCutoverSchema and therefore fails the test that
// borrowed rather than some later test that did nothing wrong. What is left to
// check here is that the shape is genuinely the pre-revision one, and not
// merely something the gate happens to dislike: a fixture that refused for the
// wrong reason would make the cutover sequence prove nothing.
func TestLegacyFixtureBuildsARealLegacyShape(t *testing.T) {
	s := setupTestDB(t)
	makeSchemaLegacy(t, s)
	seedLegacySchedule(t, s, "devops", "sched-fixture", "alice")

	for _, table := range []string{"rotation_epochs", "schedule_users", "schedule_overrides"} {
		if !legacyTableExists(t, s.db, table) {
			t.Errorf("the fixture did not create %s", table)
		}
	}
	for _, column := range []string{"timezone", "l1_rotation_start", "slack_usergroup_id"} {
		if !scheduleColumnPresent(t, s.db, column) {
			t.Errorf("the fixture did not restore schedules.%s", column)
		}
	}
	// The row the whole thing exists for: a schedule with no history horizon,
	// which nothing in this binary can write.
	if n := countRows(t, s,
		`SELECT COUNT(*) FROM schedules WHERE history_complete_from IS NULL`); n != 1 {
		t.Fatalf("got %d pre-revision rows, want exactly the one the fixture wrote", n)
	}
	requireLegacyShapeRefused(t, s.RequireCutoverSchema())
}
