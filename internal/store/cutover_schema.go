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
		"(see epic10-upgrade-checklist.md)")

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

func requireCutoverSchema(db *sql.DB) error {
	var problems []string

	for _, table := range legacyScheduleTables {
		var present bool
		if err := db.QueryRow(
			`SELECT to_regclass($1) IS NOT NULL`, "public."+table).Scan(&present); err != nil {
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

	var nullable sql.NullString
	if err := db.QueryRow(`
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'schedules'
		  AND column_name = 'history_complete_from'`).Scan(&nullable); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if nullable.Valid && nullable.String != "NO" {
		problems = append(problems, "schedules.history_complete_from is still nullable")
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w [%s]", ErrCutoverSchemaIncomplete, strings.Join(problems, "; "))
	}
	return nil
}

func scheduleColumnExists(db *sql.DB, column string) (bool, error) {
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'schedules'
			  AND column_name = $1)`, column).Scan(&exists)
	return exists, err
}
