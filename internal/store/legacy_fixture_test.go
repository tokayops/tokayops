package store

import (
	"database/sql"
	"errors"
	"fmt"
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

// makeSchemaLegacy puts the database back into its pre-revision shape and
// registers the cleanup that takes it out again.
//
// The cleanup is not hygiene. The three tables it creates are no longer in any
// truncate list - they do not exist on a normal test database - so nothing else
// would remove them, and InitDB does not drop columns. A fixture that left them
// behind would make every later assertion about "the final schema shape" depend
// on the order the tests happen to run in.
//
// It restores in the order the restoration requires: rows first, because a
// pre-revision row has no horizon and SET NOT NULL would refuse it; then the
// schema; then the constraint. And it finishes by asking RequireCutoverSchema
// whether it succeeded, so an incomplete restore fails THIS test rather than a
// later one that did nothing wrong.
func makeSchemaLegacy(t *testing.T, s *Store) {
	t.Helper()

	t.Cleanup(func() { restoreCutoverSchema(t, s) })

	if _, err := s.db.Exec(legacyScheduleFixtureDDL); err != nil {
		t.Fatalf("build the pre-revision schema: %v", err)
	}
}

func restoreCutoverSchema(t *testing.T, s *Store) {
	t.Helper()

	// A test may have failed before the reset ran, or after it: either the
	// legacy shape is still there or it is long gone. Both are expected, so
	// every statement below tolerates its subject being absent.
	if _, err := s.db.Exec(`DELETE FROM schedules WHERE history_complete_from IS NULL`); err != nil {
		t.Fatalf("clear pre-revision rows: %v", err)
	}
	for _, table := range legacyScheduleTables {
		// nosemgrep: string-formatted-query - table names are hardcoded, not user input
		if _, err := s.db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	for _, column := range legacyScheduleColumns {
		// nosemgrep: string-formatted-query - column names are hardcoded, not user input
		if _, err := s.db.Exec(fmt.Sprintf(
			`ALTER TABLE schedules DROP COLUMN IF EXISTS %s`, column)); err != nil {
			t.Fatalf("drop schedules.%s: %v", column, err)
		}
	}
	if _, err := s.db.Exec(
		`ALTER TABLE schedules ALTER COLUMN history_complete_from SET NOT NULL`); err != nil {
		t.Fatalf("restore NOT NULL on the history horizon: %v", err)
	}
	// The self-check. Without it a half-restored schema is a green test here
	// and a baffling failure somewhere else.
	if err := s.RequireCutoverSchema(); err != nil {
		t.Fatalf("the fixture did not restore the schema it borrowed: %v", err)
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
