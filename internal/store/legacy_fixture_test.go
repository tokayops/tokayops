package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// legacyFixtureStart is the instant every pre-revision fixture row is anchored
// to. Fixed rather than relative to now, so a test asserting what survived the
// reset compares against a value it chose.
var legacyFixtureStart = time.Date(2026, 1, 5, 11, 0, 0, 0, time.UTC)

// The pre-revision schedule schema, as a test fixture.
//
// It used to be production code and is not any more: nothing creates these
// tables, and RequireCutoverSchema refuses to serve a database that still has
// them. The cutover path still has to be testable, though, and the only way to
// test "a legacy database is upgraded correctly" is to be able to build one.
//
// This is the whole of it: enough shape for the reset to have something to
// remove, and enough for a pre-revision row to be insertable.
const legacyScheduleFixtureDDL = `
ALTER TABLE schedules ALTER COLUMN history_complete_from DROP NOT NULL;

ALTER TABLE schedules
	ADD COLUMN IF NOT EXISTS timezone TEXT DEFAULT 'UTC',
	ADD COLUMN IF NOT EXISTS l1_rotation_type TEXT NOT NULL DEFAULT 'weekly',
	ADD COLUMN IF NOT EXISTS l1_handoff_time TIME NOT NULL DEFAULT '11:00',
	ADD COLUMN IF NOT EXISTS l1_handoff_day INTEGER DEFAULT 1,
	ADD COLUMN IF NOT EXISTS l1_rotation_start TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS l2_enabled BOOLEAN DEFAULT FALSE,
	ADD COLUMN IF NOT EXISTS l2_escalation_timeout_min INTEGER DEFAULT 5,
	ADD COLUMN IF NOT EXISTS l2_rotation_type TEXT DEFAULT 'weekly',
	ADD COLUMN IF NOT EXISTS l2_handoff_time TIME DEFAULT '11:00',
	ADD COLUMN IF NOT EXISTS l2_handoff_day INTEGER DEFAULT 1,
	ADD COLUMN IF NOT EXISTS l2_rotation_start TIMESTAMPTZ,
	ADD COLUMN IF NOT EXISTS slack_usergroup_id TEXT;

CREATE TABLE IF NOT EXISTS schedule_users (
	schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
	layer       TEXT NOT NULL,
	user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	PRIMARY KEY (schedule_id, layer, user_id)
);

CREATE TABLE IF NOT EXISTS schedule_overrides (
	id          TEXT PRIMARY KEY,
	schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
	user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	start_time  TIMESTAMPTZ NOT NULL,
	end_time    TIMESTAMPTZ NOT NULL,
	reason      TEXT,
	created_by  TEXT REFERENCES users(id),
	created_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
	CHECK (end_time > start_time)
);

CREATE TABLE IF NOT EXISTS rotation_epochs (
	id          TEXT PRIMARY KEY,
	schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
	layer       TEXT NOT NULL,
	user_ids    TEXT NOT NULL,
	start_time  TIMESTAMPTZ NOT NULL,
	end_time    TIMESTAMPTZ,
	created_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);
`

// newThrowawayDB gives the caller a database of its own, created empty.
//
// Two different needs land here. Tests that assert what a database looks like
// when InitDB runs on it for the FIRST time cannot use the shared one, where
// TestMain has already run InitDB (main_test.go) and any neighbour may have
// reshaped it since - "fresh" there is a fiction. And tests that reshape the
// schema must not do it to a database anything else will use afterwards.
func newThrowawayDB(t *testing.T) *Store {
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

	// One database per test. The name carries a common prefix so a leftover
	// from a killed run is recognizable as test scaffolding.
	name := fmt.Sprintf("tokay_tmp_%x", time.Now().UnixNano())
	// nosemgrep: string-formatted-query - the name is generated here, not user input
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("drop a stale %s: %v", name, err)
	}
	// nosemgrep: string-formatted-query - the name is generated here, not user input
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("cannot create a throwaway database (%v); "+
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

// newLegacyDB gives the caller a throwaway database already in its
// pre-revision shape: schema created by the real InitDB, then reshaped.
//
// It has to be a database of its own, and not because of test order. Building
// the legacy shape means ADD COLUMN, and taking it down means DROP COLUMN -
// and PostgreSQL keeps a dropped column's slot forever, counting it against the
// 1600-column limit of the table. On a shared database that is not a mess to
// clean up afterwards, it is damage that accumulates across runs until
// `schedules` can no longer take a column at all. A borrowed database cannot be
// given back; a throwaway one is dropped with the slots still in it.
func newLegacyDB(t *testing.T) *Store {
	t.Helper()
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	makeSchemaLegacy(t, s)
	return s
}

// makeSchemaLegacy reshapes a database into its pre-revision form. Only ever
// called on a throwaway one - see newLegacyDB.
func makeSchemaLegacy(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(legacyScheduleFixtureDDL); err != nil {
		t.Fatalf("build the pre-revision schema: %v", err)
	}
}

// seedLegacySchedule writes a schedule in the pre-upgrade shape, with a row in
// every table that hangs off it by cascade.
func seedLegacySchedule(t *testing.T, s *Store, teamID, scheduleID, userID string) {
	t.Helper()
	// No members: this fixture writes a pre-upgrade row directly, so there is
	// no save pipeline here to validate membership for.
	seedTeam(t, s, teamID)
	if err := s.CreateUser(&model.User{ID: userID, Email: userID + "@example.com", Name: userID}); err != nil {
		t.Fatalf("CreateUser %s: %v", userID, err)
	}

	start := legacyFixtureStart
	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, team_id, timezone, l1_rotation_start) VALUES ($1, $2, 'UTC', $3)`,
		scheduleID, teamID, start); err != nil {
		t.Fatalf("insert legacy schedule: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO schedule_users (schedule_id, layer, user_id, position) VALUES ($1, 'l1', $2, 0)`,
		scheduleID, userID); err != nil {
		t.Fatalf("insert legacy schedule user: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO schedule_overrides (id, schedule_id, user_id, start_time, end_time, created_by)
		 VALUES ($1, $2, $3, $4, $5, $3)`,
		"legacy-ovr-"+scheduleID, scheduleID, userID, start.Add(24*time.Hour), start.Add(30*time.Hour)); err != nil {
		t.Fatalf("insert legacy override: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO rotation_epochs (id, schedule_id, layer, user_ids, start_time)
		 VALUES ($1, $2, 'l1', $3, $4)`,
		"legacy-epoch-"+scheduleID, scheduleID, `[["`+userID+`"]]`, start); err != nil {
		t.Fatalf("insert legacy epoch: %v", err)
	}
}

// legacyTableExists reports whether a pre-revision table is still in the schema.
func legacyTableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var present bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&present); err != nil {
		t.Fatalf("to_regclass(%s): %v", table, err)
	}
	return present
}

// scheduleColumnPresent reports whether `schedules` still has a column.
func scheduleColumnPresent(t *testing.T, db *sql.DB, column string) bool {
	t.Helper()
	var present bool
	if err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'schedules' AND column_name = $1)`,
		column).Scan(&present); err != nil {
		t.Fatalf("information_schema(%s): %v", column, err)
	}
	return present
}

// requireLegacyShapeRefused asserts the gate says no, and says why in terms an
// operator can act on.
func requireLegacyShapeRefused(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrCutoverSchemaIncomplete) {
		t.Fatalf("error = %v, want ErrCutoverSchemaIncomplete", err)
	}
	if !strings.Contains(err.Error(), "migrate reset-schedules") {
		t.Fatalf("the refusal must name the step that was skipped: %v", err)
	}
}
