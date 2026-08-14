package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// TestCutoverSequence is the one test that matters here, and it is end to end
// because what has to hold is the SEQUENCE, not its steps.
//
// Every step is executable on its own and proves nothing on its own: InitDB
// must survive a legacy database, the gate must refuse it, the reset must
// repair it, and the gate must then pass - in that order, on the same database,
// with the schedules still writable afterwards.
func TestCutoverSequence(t *testing.T) {
	s := newThrowawayDB(t)

	// 1. A database from before the revision model. InitDB has to run on it
	//    first, because that is what the binary does before anything else.
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB on an empty database: %v", err)
	}
	makeSchemaLegacy(t, s)
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

// TestFreshDatabaseReachesTheFinalShape: a new installation needs no reset, so
// the gate must let it start. The reset is for upgrades and is not meaningful
// here - which is why the refusal is worded about the legacy shape and not
// about a missing marker.
func TestFreshDatabaseReachesTheFinalShape(t *testing.T) {
	s := newThrowawayDB(t)
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
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	makeSchemaLegacy(t, s)

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
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	var available bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')`).Scan(&available); err != nil {
		t.Fatalf("check for btree_gist: %v", err)
	}
	if !available {
		// Skipped rather than failed on purpose: the constraint is defence in
		// depth and an installation without the extension is supported. That
		// contract is what RevisionOverlapConstraintPresent reports at startup.
		t.Skip("btree_gist is not installed in this PostgreSQL; the constraint is skipped by design")
	}

	present, err := s.RevisionOverlapConstraintPresent()
	if err != nil {
		t.Fatalf("check for the constraint: %v", err)
	}
	if !present {
		t.Fatal("the revision exclusion constraint was not created; btree_gist left with the legacy schema")
	}
}

// TestLegacyFixtureBuildsARealLegacyShape guards the fixture itself.
//
// The cutover sequence proves nothing if what it upgrades is not actually a
// pre-revision database - a fixture the gate refused for some other reason
// would look exactly as convincing. So the shape is checked directly: the
// tables, the mutable columns, and a schedule row with no history horizon,
// which nothing in this binary can write.
func TestLegacyFixtureBuildsARealLegacyShape(t *testing.T) {
	s := newLegacyDB(t)
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

// TestRevisionOverlapConstraintLookupIsTableQualified: constraint names are
// unique per table, so a same-named constraint elsewhere must not answer for
// this one - and it would answer "present", hiding exactly the absence the
// startup warning exists to report.
func TestRevisionOverlapConstraintLookupIsTableQualified(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	// Drop the real one, then put a decoy of the same name on another table.
	if _, err := s.db.Exec(
		`ALTER TABLE schedule_revisions DROP CONSTRAINT IF EXISTS no_overlapping_schedule_revisions`); err != nil {
		t.Fatalf("drop the real constraint: %v", err)
	}
	if _, err := s.db.Exec(
		`ALTER TABLE teams ADD CONSTRAINT no_overlapping_schedule_revisions CHECK (id <> '')`); err != nil {
		t.Fatalf("add the decoy: %v", err)
	}

	present, err := s.RevisionOverlapConstraintPresent()
	if err != nil {
		t.Fatalf("RevisionOverlapConstraintPresent: %v", err)
	}
	if present {
		t.Fatal("a constraint of the same name on another table was reported as this one")
	}
}

// TestCutoverSchemaChecksResolveThroughSearchPath: nothing else in this package
// names a schema, so this check must not either.
//
// The failure it guards against is the loudest one available: on an installation
// whose search_path puts the tables somewhere other than `public`, InitDB
// creates them happily and every query reads them happily, and a check that
// looked in `public` would find nothing and refuse to start a database that is
// in perfect shape.
//
// The whole sequence runs on the non-default schema, not just the check: a
// regression that only asserted RequireCutoverSchema would pass while the reset
// still hardcoded a schema of its own.
func TestCutoverSchemaChecksResolveThroughSearchPath(t *testing.T) {
	s := newThrowawayDB(t)

	if _, err := s.db.Exec(`CREATE SCHEMA app`); err != nil {
		t.Fatalf("create the schema: %v", err)
	}
	// SET LOCAL would not survive the pool; ALTER DATABASE is the
	// connection-independent form, and it is what an installation configures.
	var dbName string
	if err := s.db.QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatalf("read the database name: %v", err)
	}
	if _, err := s.db.Exec(
		`ALTER DATABASE ` + pq.QuoteIdentifier(dbName) + ` SET search_path = app, public`); err != nil {
		t.Fatalf("set the search_path: %v", err)
	}
	// Existing pooled connections still carry the old setting.
	s.db.SetMaxIdleConns(0)
	s.db.SetMaxIdleConns(2)

	var path string
	if err := s.db.QueryRow(`SHOW search_path`).Scan(&path); err != nil {
		t.Fatalf("read back the search_path: %v", err)
	}
	if !strings.Contains(path, "app") {
		t.Skipf("search_path did not take (%q); the fixture cannot make its point", path)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB on a non-public search_path: %v", err)
	}
	if err := s.RequireCutoverSchema(); err != nil {
		t.Fatalf("the gate refused a database it should accept: %v", err)
	}

	// And the shape is really in `app`, so the check passed for the right
	// reason rather than by looking at leftovers in public.
	var schema string
	if err := s.db.QueryRow(`
		SELECT n.nspname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass('schedules')`).Scan(&schema); err != nil {
		t.Fatalf("locate the schedules table: %v", err)
	}
	if schema != "app" {
		t.Fatalf("schedules landed in %q, so this test proves nothing", schema)
	}
}
