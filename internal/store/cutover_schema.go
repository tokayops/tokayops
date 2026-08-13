package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrCutoverSchemaIncomplete means the database still carries the pre-revision
// schedule schema: the destructive upgrade step was never run against it.
//
// The message is written for an operator mid-cutover, not for a developer. The
// alternative - letting the process fail on whatever the first schedule query
// hits - names a column or a table and leaves the one person who can fix it
// with no idea which step they skipped.
var ErrCutoverSchemaIncomplete = errors.New(
	"store: the schedule schema still has its pre-revision shape; " +
		"run `tokayops migrate reset-schedules` before starting the server " +
		"(see the upgrade documentation for this release)")

// RequireCutoverSchema refuses to let the server start against a database that
// has not been through the schedule cutover.
//
// It asks about the SHAPE of the schema, never about its contents. The tempting
// check - counting rows with a NULL history horizon - passes on an empty legacy
// database, which is exactly the state where starting is least safe: the old
// schema is intact and the first schedule anyone creates lands in a table whose
// mutable configuration columns nothing maintains.
//
// A freshly created database already has the final shape and needs no reset;
// the error is for a legacy one. Because the cleanup commits as a single
// transaction, no half-migrated shape exists to report.
func (s *Store) RequireCutoverSchema() error {
	return requireCutoverSchema(s.db)
}

// Every lookup below resolves through search_path rather than naming a schema.
//
// Nothing else in this package qualifies one: the DDL creates unqualified
// tables and every query reads them unqualified, so the schema an installation
// puts them in is whatever its search_path says. A check that insisted on
// `public` would be the only opinionated thing in the codebase, and it would
// express that opinion by refusing to start - the loudest failure available -
// on a database every other line of code is perfectly happy with.
func requireCutoverSchema(db *sql.DB) error {
	var problems []string

	for _, table := range legacyScheduleTables {
		var present bool
		if err := db.QueryRow(
			`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil {
			return err
		}
		if present {
			problems = append(problems, "table "+table+" still exists")
		}
	}

	for _, column := range legacyScheduleColumns {
		present, err := scheduleColumnExists(db, column)
		if err != nil {
			return err
		}
		if present {
			problems = append(problems, "schedules."+column+" still exists")
		}
	}

	// The live columns are asserted present rather than assumed: a cleanup that
	// took one of them with the mutable block would leave a database that is
	// not legacy and not usable either, and the first failure would come from a
	// schedule write rather than from here.
	for _, column := range []string{
		"id", "team_id", "config_version", "history_complete_from",
		"deleted_at", "created_at", "updated_at",
	} {
		present, err := scheduleColumnExists(db, column)
		if err != nil {
			return err
		}
		if !present {
			problems = append(problems, "schedules."+column+" is missing")
		}
	}

	var notNull sql.NullBool
	if err := db.QueryRow(`
		SELECT attnotnull FROM pg_attribute
		WHERE attrelid = to_regclass('schedules')
		  AND attname = 'history_complete_from'
		  AND attnum > 0 AND NOT attisdropped`).Scan(&notNull); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if notNull.Valid && !notNull.Bool {
		problems = append(problems, "schedules.history_complete_from is still nullable")
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w [%s]", ErrCutoverSchemaIncomplete, strings.Join(problems, "; "))
	}
	return nil
}

// RevisionOverlapConstraintPresent reports whether the exclusion constraint
// that forbids two revisions of one schedule covering the same instant exists.
//
// It is asked at startup and REPORTED, not enforced, and the difference is a
// deliberate contract rather than a compromise. The constraint is defence in
// depth: non-overlap is guaranteed by the schedule row lock inside a single
// transaction, which is what the write path relies on and what its tests pin.
// The constraint needs btree_gist, which needs privileges an installation may
// not grant, and refusing to start there would take a working deployment down
// for the loss of a second line of defence rather than the first.
//
// What it must not do is disappear silently. The DDL that creates it swallows a
// missing extension with a RAISE NOTICE, which no operator sees; a database
// without the constraint should be a fact somebody can read in the log.
//
// The lookup names the table and the kind, not just the constraint name.
// Constraint names are unique per table, not per database, so a same-named
// constraint on anything else would answer this question - and it would answer
// it "present", which is the direction that hides the absence this exists to
// report. to_regclass rather than a cast: a cast raises when the table is
// missing, and a missing table is a different failure than a missing backstop.
func (s *Store) RevisionOverlapConstraintPresent() (bool, error) {
	var present bool
	err := s.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conname = $1
			  AND conrelid = to_regclass('schedule_revisions')
			  AND contype = 'x')`,
		"no_overlapping_schedule_revisions").Scan(&present)
	return present, err
}

// scheduleColumnExists asks pg_attribute rather than information_schema: the
// former is addressed by relation oid, which to_regclass resolves through
// search_path, while the latter is addressed by schema name and would have to
// be told which one. `NOT attisdropped` matters too - a dropped column keeps
// its slot forever, and information_schema hides that while pg_attribute does
// not.
func scheduleColumnExists(db *sql.DB, column string) (bool, error) {
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM pg_attribute
			WHERE attrelid = to_regclass('schedules')
			  AND attname = $1 AND attnum > 0 AND NOT attisdropped)`, column).Scan(&exists)
	return exists, err
}
