package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// ScheduleReadRepository exposes the read side of schedule configuration.
// Like the command repository it is deliberately not part of StoreInterface:
// the revision model is not mirrored into MockStore.
func (s *Store) ScheduleReadRepository() scheduleconfig.ScheduleReadRepository {
	return &scheduleReadRepo{db: s.db}
}

type scheduleReadRepo struct {
	db *sql.DB
}

// WithinSnapshot runs fn against one immutable view of the database.
//
// REPEATABLE READ is the requirement, not a precaution: PostgreSQL's default
// READ COMMITTED takes a new snapshot per statement, so a Save committing
// between the revision query and the override query would let one rendered
// answer mix configuration from before the edit with overrides from after it.
// The transaction is read-only and always rolled back - there is nothing to
// commit, and rollback is the cheaper end for a snapshot.
func (r *scheduleReadRepo) WithinSnapshot(ctx context.Context, fn func(scheduleconfig.ScheduleReadView) error) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	return fn(&scheduleReadView{q: tx})
}

// scheduleReadView serves reads from whatever queryer it was given: the
// read-only snapshot above, or the command transaction, which embeds it.
type scheduleReadView struct {
	q sqlQueryer
}

func (v *scheduleReadView) GetScheduleRoot(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRoot, error) {
	return scanScheduleRootRow(v.q.QueryRowContext(ctx,
		`SELECT `+scheduleRootColumns+` FROM schedules WHERE id = $1`, scheduleID))
}

func (v *scheduleReadView) GetScheduleRootByTeam(ctx context.Context, teamID string) (*scheduleconfig.ScheduleRoot, error) {
	return scanScheduleRootRow(v.q.QueryRowContext(ctx,
		`SELECT `+scheduleRootColumns+` FROM schedules WHERE team_id = $1`, teamID))
}

// ListScheduleRoots returns every schedule root, soft-deleted ones included.
//
// It selects the root columns only. Reading the legacy mutable columns here is
// what makes the old list query unusable for a revision-managed schedule: they
// are nullable now and nothing on the revision path writes them, so a single
// revision row fails the scan and takes the whole result set with it.
//
// The ORDER BY is not cosmetic: the notifier tick and its log lines walk this
// list, and an unordered one would reorder them run to run for no reason.
func (v *scheduleReadView) ListScheduleRoots(ctx context.Context) ([]scheduleconfig.ScheduleRoot, error) {
	rows, err := v.q.QueryContext(ctx, `SELECT `+scheduleRootColumns+` FROM schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []scheduleconfig.ScheduleRoot
	for rows.Next() {
		root, err := scanScheduleRoot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *root)
	}
	return out, rows.Err()
}

// GetEffectiveRevision returns the revision whose half-open interval contains
// `at`. The lower bound is part of the predicate on purpose: without it a
// future revision would answer a query about the past.
//
// Two matching rows are refused rather than resolved. The exclusion constraint
// forbids them, so this can only happen to a database somebody edited by hand
// or restored badly - and then picking one of the two would mean the notifier
// waking an arbitrary group and nobody ever learning. The row limit is 2
// because the answer is the same whether there are two or two hundred.
func (v *scheduleReadView) GetEffectiveRevision(ctx context.Context, scheduleID string, at time.Time) (*scheduleconfig.ScheduleRevision, error) {
	rows, err := v.q.QueryContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1
		   AND effective_from <= $2
		   AND (effective_to IS NULL OR effective_to > $2)
		 LIMIT 2`,
		scheduleID, scheduleconfig.NormalizeTimestamp(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, scheduleconfig.ErrRevisionNotFound
	}
	first, err := scanScheduleRevision(rows)
	if err != nil {
		return nil, err
	}
	if rows.Next() {
		second, err := scanScheduleRevision(rows)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: schedule %s has revisions %s and %s both in force at %s",
			scheduleconfig.ErrRevisionOverlap, scheduleID, first.ID, second.ID,
			at.Format(time.RFC3339))
	}
	return first, rows.Err()
}

// GetRevisionsInRange returns every revision overlapping [from, until).
//
// The predicate is the half-open overlap test in both directions: a revision
// starting exactly at `until` is outside the range, and one ending exactly at
// `from` is too. Both kinds are returned - filtering deleted periods out here
// would hand the caller back the very hole that kind exists to remove.
func (v *scheduleReadView) GetRevisionsInRange(ctx context.Context, scheduleID string, from, until time.Time) ([]scheduleconfig.ScheduleRevision, error) {
	rows, err := v.q.QueryContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1
		   AND effective_from < $3
		   AND (effective_to IS NULL OR effective_to > $2)
		 ORDER BY effective_from`,
		scheduleID, scheduleconfig.NormalizeTimestamp(from), scheduleconfig.NormalizeTimestamp(until))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []scheduleconfig.ScheduleRevision
	for rows.Next() {
		rev, err := scanScheduleRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rev)
	}
	return out, rows.Err()
}

func (v *scheduleReadView) GetOverrideProjectionInRange(ctx context.Context, scheduleID string, from, until *time.Time) ([]scheduleconfig.OverrideRevision, error) {
	return getOverrideProjection(ctx, v.q, scheduleID, from, until)
}

// GetRevisionByID scopes the lookup by schedule as well as by revision. A
// revision ID belonging to another team's schedule has to answer "not found":
// the RBAC check upstream authorized one schedule, not one identifier.
func (v *scheduleReadView) GetRevisionByID(ctx context.Context, scheduleID, revisionID string) (*scheduleconfig.ScheduleRevision, error) {
	return scanScheduleRevisionRow(v.q.QueryRowContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1 AND id = $2`, scheduleID, revisionID))
}

// ListRevisions pages the audit trail newest first. The cursor is the version
// rather than an offset: versions are dense and strictly increasing per
// schedule, so a page cannot shift under a reader the way an OFFSET can.
func (v *scheduleReadView) ListRevisions(ctx context.Context, scheduleID string, limit int, beforeVersion *int64) ([]scheduleconfig.ScheduleRevision, error) {
	var before any
	if beforeVersion != nil {
		before = *beforeVersion
	}
	rows, err := v.q.QueryContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1
		   AND ($2::bigint IS NULL OR version < $2)
		 ORDER BY version DESC
		 LIMIT $3`, scheduleID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []scheduleconfig.ScheduleRevision
	for rows.Next() {
		rev, err := scanScheduleRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rev)
	}
	return out, rows.Err()
}

// GetTeamMemberIDs lists the ACTIVE members of a team.
//
// The join to users and the deleted_at filter are what stop an erased person
// being validated back into a rotation: team_members alone still holds the row
// until erasure removes it, and even after that a stale editor payload naming
// the ID must be rejected rather than accepted because the ID "looks fine".
func (v *scheduleReadView) GetTeamMemberIDs(ctx context.Context, teamID string) ([]string, error) {
	rows, err := v.q.QueryContext(ctx,
		`SELECT u.id
		 FROM team_members tm
		 JOIN users u ON u.id = tm.user_id
		 WHERE tm.team_id = $1 AND u.deleted_at IS NULL
		 ORDER BY u.id`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Compile-time proof the store satisfies the read contract, and that both
// *sql.Tx and *sql.DB are usable as the shared queryer.
var (
	_ scheduleconfig.ScheduleReadRepository = (*scheduleReadRepo)(nil)
	_ scheduleconfig.ScheduleReadView       = (*scheduleReadView)(nil)
	_ sqlQueryer                            = (*sql.Tx)(nil)
	_ sqlQueryer                            = (*sql.DB)(nil)
)

// scanScheduleRootRow adapts the single-row lookups: only there does "no rows"
// mean the schedule does not exist. In a list query an empty result is an
// ordinary answer.
func scanScheduleRootRow(row *sql.Row) (*scheduleconfig.ScheduleRoot, error) {
	root, err := scanScheduleRoot(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	return root, err
}

func scanScheduleRoot(row rowScanner) (*scheduleconfig.ScheduleRoot, error) {
	var (
		root                 scheduleconfig.ScheduleRoot
		historyFrom, deleted sql.NullTime
	)
	if err := row.Scan(&root.ID, &root.TeamID, &root.ConfigVersion, &historyFrom, &deleted); err != nil {
		return nil, err
	}
	if historyFrom.Valid {
		v := historyFrom.Time
		root.HistoryCompleteFrom = &v
	}
	if deleted.Valid {
		v := deleted.Time
		root.DeletedAt = &v
	}
	return &root, nil
}
