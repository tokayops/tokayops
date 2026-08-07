package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// legacyScheduleResetMarker is the permanent record that the destructive
// schedule reset has run. Its presence makes a second run a no-op.
const legacyScheduleResetMarker = "legacy_schedule_reset"

// legacyScheduleResetLockKey is an arbitrary but fixed advisory-lock key, so
// two operators starting the reset at once serialize instead of racing.
const legacyScheduleResetLockKey int64 = 1953980275

// ErrLegacyResetUnexpectedState means the reset found schedule data that only
// the new write path could have produced, without its marker. Deleting in that
// state would destroy schedules an operator has already recreated, so the
// reset refuses rather than overwriting silently.
var ErrLegacyResetUnexpectedState = errors.New("store: schedule reset marker missing but revision data is present")

// ResetResult reports what the reset did.
type ResetResult struct {
	// SchedulesDeleted counts the schedule roots removed. Their dependent
	// rows go with them by cascade.
	SchedulesDeleted int

	// AlreadyApplied is true when the marker was already there and nothing
	// was touched.
	AlreadyApplied bool
}

// ResetLegacySchedules runs the destructive schedule reset against this
// store's connection. It exists so callers do not have to reach for GetDB.
func (s *Store) ResetLegacySchedules() (*ResetResult, error) {
	return RunLegacyScheduleReset(s.db)
}

// RunLegacyScheduleReset wipes pre-upgrade schedule data in one transaction.
//
// It is deliberately NOT part of InitDB: InitDB runs on every start, and a
// destructive step that runs on every start would eventually delete schedules
// an operator recreated after the upgrade. The only entry point is the
// dedicated CLI subcommand.
//
// The transaction is:
//
//  1. take a global advisory lock;
//  2. marker present -> no-op;
//  3. marker absent but revision data present -> refuse;
//  4. DELETE FROM schedules, cascading to schedule_users, schedule_overrides
//     and rotation_epochs;
//  5. write the marker.
//
// Preconditions the operator owns: every instance stopped, backup taken.
func RunLegacyScheduleReset(db *sql.DB) (*ResetResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after a successful commit is a no-op

	// Held until the transaction ends; no explicit unlock to forget.
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, legacyScheduleResetLockKey); err != nil {
		return nil, err
	}

	var markerExists bool
	if err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM migration_markers WHERE name = $1)`,
		legacyScheduleResetMarker).Scan(&markerExists); err != nil {
		return nil, err
	}
	if markerExists {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &ResetResult{AlreadyApplied: true}, nil
	}

	// Any of these tables holding rows means the new write path has already
	// been used against this database.
	for _, table := range []string{"schedule_revisions", "schedule_override_revisions", "schedule_events"} {
		var populated bool
		// nosemgrep: string-formatted-query - table names are hardcoded, not user input
		if err := tx.QueryRow(fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s)`, table)).Scan(&populated); err != nil {
			return nil, err
		}
		if populated {
			return nil, fmt.Errorf("%w: %s is not empty", ErrLegacyResetUnexpectedState, table)
		}
	}

	res, err := tx.Exec(`DELETE FROM schedules`)
	if err != nil {
		return nil, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`INSERT INTO migration_markers (name) VALUES ($1)`,
		legacyScheduleResetMarker); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ResetResult{SchedulesDeleted: int(deleted)}, nil
}
