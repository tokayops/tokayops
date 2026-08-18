package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// legacyScheduleResetMarker records that the destructive schedule reset has
// deleted the pre-revision rows.
const legacyScheduleResetMarker = "legacy_schedule_reset"

// legacyScheduleDropMarker records that the pre-revision schema itself is gone
// and the history horizon has been tightened to NOT NULL.
//
// It is a second marker rather than a flag on the first because the two answer
// different questions, and a database can have the first without the second:
// every installation that ran the reset before this release has deleted its
// legacy rows but still carries the legacy tables. Collapsing them would make
// that installation indistinguishable from one that has finished, and it would
// never receive the physical cleanup.
const legacyScheduleDropMarker = "legacy_schedule_physical_drop"

// legacyScheduleResetLockKey is an arbitrary but fixed advisory-lock key, so
// two operators starting the reset at once serialize instead of racing.
const legacyScheduleResetLockKey int64 = 1953980275

// legacyResetLockTimeout bounds every lock this transaction waits for.
//
// The physical phase is DDL: DROP TABLE, DROP COLUMN and SET NOT NULL all take
// ACCESS EXCLUSIVE. The upgrade checklist requires every instance to be
// stopped, so on a real cutover nothing contends. When something does, waiting
// is the worst available answer: a cutover window is finite, and in CI an
// unbounded wait burns the whole job timeout and reports nothing about why.
// Failing names the blocker instead.
//
// It is set before the advisory lock rather than after, and the advisory lock
// is taken with the try form: the two together are what leave no unbounded wait
// in the transaction.
const legacyResetLockTimeout = "15s"

// legacyScheduleTables are the pre-revision tables the cleanup removes.
//
// schedule_events belongs here even though nothing has created it since the
// revision model landed: a database that ran an intermediate build of the epic
// still has the table.
var legacyScheduleTables = []string{
	"rotation_epochs",
	"schedule_users",
	"schedule_overrides",
	"schedule_events",
}

// legacyScheduleColumns are the mutable configuration columns of `schedules`.
// Configuration lives in revision snapshots; these are what the pre-revision
// model wrote instead.
var legacyScheduleColumns = []string{
	"timezone",
	"l1_rotation_type",
	"l1_handoff_time",
	"l1_handoff_day",
	"l1_rotation_start",
	"l2_enabled",
	"l2_escalation_timeout_min",
	"l2_rotation_type",
	"l2_handoff_time",
	"l2_handoff_day",
	"l2_rotation_start",
	"slack_usergroup_id",
}

// ErrLegacyResetUnexpectedState means the reset found schedule data that only
// the new write path could have produced, without its marker. Deleting in that
// state would destroy schedules an operator has already recreated, so the
// reset refuses rather than overwriting silently.
var ErrLegacyResetUnexpectedState = errors.New("store: schedule reset marker missing but revision data is present")

// ErrLegacyResetRootWithoutHorizon means a database whose rows were already
// reset still holds a schedule with no history horizon.
//
// It is reported on its own rather than left to the SET NOT NULL below, because
// the bare DDL error names a column and an operator reading it has no way to
// reach the schedules it is about.
var ErrLegacyResetRootWithoutHorizon = errors.New("store: schedule has no history horizon and cannot be tightened")

// ErrLegacyResetInProgress means another process holds the reset lock. Re-run
// once it has finished; the marker makes the second run a no-op.
var ErrLegacyResetInProgress = errors.New(
	"store: another schedule reset is already running against this database")

// ResetResult reports what the reset did.
//
// The outcomes are kept distinct rather than inferred from the count, because
// two of them share a count and mean opposite things. A database whose rows
// were reset by an earlier upgrade deletes nothing because there is nothing it
// is ALLOWED to delete; an empty database deletes nothing because there is
// nothing there. Reporting the first as "0 schedules deleted" would suggest the
// cleanup found the database empty when in fact it deliberately left live
// schedules alone - in the one moment an operator reads the output carefully.
type ResetResult struct {
	// SchedulesDeleted counts the pre-revision schedule roots removed. Their
	// dependent rows go with them by cascade.
	SchedulesDeleted int

	// RowsAlreadyReset is true when an earlier upgrade had already deleted the
	// pre-revision rows, so this run touched no data at all.
	RowsAlreadyReset bool

	// PhysicalCleanup is true when this run dropped the legacy schema and
	// tightened the history horizon.
	PhysicalCleanup bool

	// AlreadyApplied is true only when the physical cleanup had already run and
	// nothing was touched.
	AlreadyApplied bool
}

// ResetLegacySchedules runs the destructive schedule reset against this
// store's connection. It exists so callers do not have to reach for GetDB.
func (s *Store) ResetLegacySchedules() (*ResetResult, error) {
	return RunLegacyScheduleReset(s.db)
}

// RunLegacyScheduleReset completes the cutover in one transaction.
//
// It is deliberately NOT part of InitDB: InitDB runs on every start, and a
// destructive step that runs on every start would eventually delete schedules
// an operator recreated after the upgrade. The only entry point is the
// dedicated CLI subcommand.
//
// The transaction is:
//
//  1. bound every lock wait, then take the global advisory lock or refuse;
//  2. physical marker present -> no-op;
//  3. reset marker absent (a database from before the epic) -> refuse if
//     revision data exists, otherwise DELETE FROM schedules and record it;
//  4. reset marker present (the rows are already gone) -> touch no data, and
//     refuse if any root still lacks a history horizon;
//  5. drop the legacy tables and columns, tighten the horizon, record it.
//
// Step 4 is what keeps the cleanup non-destructive on a database that upgraded
// earlier: the schedules there are the recreated, revision-managed ones the
// whole epic exists for, and deleting them would be the worst outcome this
// command can produce.
//
// DDL and DML share the transaction because PostgreSQL makes that safe, and
// that is what leaves exactly two reachable shapes - legacy and final - for
// RequireCutoverSchema to tell apart.
//
// Preconditions the operator owns: every instance stopped, backup taken.
func RunLegacyScheduleReset(db *sql.DB) (*ResetResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after a successful commit is a no-op

	// lock_timeout goes first, before anything can wait: set after the advisory
	// lock it would not bound the one wait that has no other bound.
	if _, err := tx.Exec(`SET LOCAL lock_timeout = '` + legacyResetLockTimeout + `'`); err != nil {
		return nil, err
	}

	// Try, not wait. Held until the transaction ends, so there is no unlock to
	// forget - and a second operator gets a sentence naming the situation
	// instead of a wait that ends in `canceling statement due to lock timeout`,
	// which says nothing about who is holding it or what to do. Re-running
	// after the first finishes is a no-op by then.
	var acquired bool
	if err := tx.QueryRow(
		`SELECT pg_try_advisory_xact_lock($1)`, legacyScheduleResetLockKey).Scan(&acquired); err != nil {
		return nil, err
	}
	if !acquired {
		return nil, ErrLegacyResetInProgress
	}

	dropped, err := markerExists(tx, legacyScheduleDropMarker)
	if err != nil {
		return nil, err
	}
	if dropped {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return &ResetResult{AlreadyApplied: true}, nil
	}

	reset, err := markerExists(tx, legacyScheduleResetMarker)
	if err != nil {
		return nil, err
	}

	result := &ResetResult{PhysicalCleanup: true}
	if reset {
		// The rows are already the recreated ones. Nothing here may delete;
		// the only question left is whether they can be tightened.
		if err := requireEveryScheduleHasHorizon(tx); err != nil {
			return nil, err
		}
		result.RowsAlreadyReset = true
	} else {
		deleted, err := deleteLegacySchedules(tx)
		if err != nil {
			return nil, err
		}
		result.SchedulesDeleted = deleted
	}

	if err := dropLegacySchema(tx); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO migration_markers (name) VALUES ($1)`, legacyScheduleDropMarker); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func markerExists(tx *sql.Tx, name string) (bool, error) {
	var exists bool
	err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM migration_markers WHERE name = $1)`, name).Scan(&exists)
	return exists, err
}

// deleteLegacySchedules empties the pre-revision rows and records that it did.
func deleteLegacySchedules(tx *sql.Tx) (int, error) {
	// Any of these tables holding rows means the new write path has already
	// been used against this database.
	for _, table := range []string{"schedule_revisions", "schedule_override_revisions"} {
		var populated bool
		// nosemgrep: string-formatted-query - table names are hardcoded, not user input
		if err := tx.QueryRow(fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s)`, table)).Scan(&populated); err != nil {
			return 0, err
		}
		if populated {
			return 0, fmt.Errorf("%w: %s is not empty", ErrLegacyResetUnexpectedState, table)
		}
	}

	res, err := tx.Exec(`DELETE FROM schedules`)
	if err != nil {
		return 0, err
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`INSERT INTO migration_markers (name) VALUES ($1)`, legacyScheduleResetMarker); err != nil {
		return 0, err
	}
	return int(deleted), nil
}

// requireEveryScheduleHasHorizon refuses ahead of the tightening, naming the
// schedules that block it.
//
// Named for what it checks rather than for the predicate that was deleted with
// the cutover: "initialized root" is the vocabulary that went away with the
// concept, and a helper carrying it would keep the phrase alive in a grep that
// exists to prove it is gone.
func requireEveryScheduleHasHorizon(tx *sql.Tx) error {
	rows, err := tx.Query(
		`SELECT id FROM schedules WHERE history_complete_from IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var blocking []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		blocking = append(blocking, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(blocking) > 0 {
		return fmt.Errorf("%w: %s", ErrLegacyResetRootWithoutHorizon, strings.Join(blocking, ", "))
	}
	return nil
}

// dropLegacySchema removes the pre-revision schema and tightens the horizon.
//
// DROP TABLE carries no CASCADE: nothing refers to these tables today, and if
// something ever does, refusing loudly beats taking the dependency with them.
func dropLegacySchema(tx *sql.Tx) error {
	for _, table := range legacyScheduleTables {
		// nosemgrep: string-formatted-query - table names are hardcoded, not user input
		if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)); err != nil {
			return err
		}
	}
	for _, column := range legacyScheduleColumns {
		// nosemgrep: string-formatted-query - column names are hardcoded, not user input
		if _, err := tx.Exec(fmt.Sprintf(
			`ALTER TABLE schedules DROP COLUMN IF EXISTS %s`, column)); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`ALTER TABLE schedules ALTER COLUMN history_complete_from SET NOT NULL`)
	return err
}
