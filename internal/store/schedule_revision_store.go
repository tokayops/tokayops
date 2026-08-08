package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

const (
	// scheduleTeamUniqueConstraint is the UNIQUE(team_id) constraint on
	// schedules. Only this constraint means "the team already has a
	// schedule"; any other unique violation (a repeated primary key, say) has
	// different semantics and must not be laundered into a caller-recoverable
	// conflict.
	scheduleTeamUniqueConstraint = "schedules_team_id_key"

	// scheduleTeamFKConstraint is the schedules -> teams foreign key. Only
	// this one means "the caller named a team that does not exist"; every
	// other foreign key on the revision tables points at rows this package
	// writes itself, so violating one is a bug.
	scheduleTeamFKConstraint = "schedules_team_id_fkey"
)

// scheduleRevisionColumns is the SELECT list every revision scan expects.
const scheduleRevisionColumns = `id, schedule_id, version, kind, snapshot, effective_from, effective_to,
	recorded_at, created_by, change_reason, change_summary`

// scheduleRootColumns is the SELECT list every schedule root scan expects.
const scheduleRootColumns = `id, team_id, config_version, history_complete_from, deleted_at`

// sqlQueryer is the read surface shared by *sql.Tx and *sql.DB. Every query
// below is written once against it and called from both the command-side
// transaction and the read-only snapshot: a second copy of the same SQL is a
// second thing to keep correct.
type sqlQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// rowScanner unifies *sql.Row and *sql.Rows so one scan function serves both
// the single-row and the multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

// ScheduleConfigRepository exposes the schedule configuration unit of work.
// It is intentionally not part of StoreInterface: the revision model is not
// mirrored into the legacy mock.
func (s *Store) ScheduleConfigRepository() scheduleconfig.ScheduleConfigRepository {
	return &scheduleConfigRepo{db: s.db}
}

type scheduleConfigRepo struct {
	db *sql.DB
}

func (r *scheduleConfigRepo) WithinTx(ctx context.Context, fn func(scheduleconfig.ScheduleConfigTx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(&scheduleConfigTx{tx: tx, scheduleReadView: scheduleReadView{q: tx}}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// scheduleConfigTx is the command-side unit of work. It embeds the read view
// over the same transaction, so a command reads through exactly the contract
// the renderer uses.
type scheduleConfigTx struct {
	scheduleReadView
	tx *sql.Tx
}

// CreateInitialSchedule writes the root, revision 1, config_version = 1 and
// history_complete_from as one indivisible operation. The low-level root
// insert is deliberately not reachable on its own: there is no code path that
// can leave a schedule without a revision behind.
//
// Legacy mutable configuration columns are left to their defaults; the runtime
// no longer reads them.
func (t *scheduleConfigTx) CreateInitialSchedule(ctx context.Context, root *scheduleconfig.ScheduleRoot, initial *scheduleconfig.ScheduleRevision) error {
	if err := scheduleconfig.PrepareInitialSchedule(root, initial); err != nil {
		return err
	}

	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO schedules (id, team_id, config_version, history_complete_from, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)`,
		root.ID, root.TeamID, root.ConfigVersion, initial.EffectiveFrom, initial.RecordedAt)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return t.InsertRevision(ctx, initial)
}

// LockSchedule takes the row lock that serializes writes to one schedule. It
// is the same projection as the read-side root lookup, plus the lock.
func (t *scheduleConfigTx) LockSchedule(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRoot, error) {
	return scanScheduleRootRow(t.tx.QueryRowContext(ctx,
		`SELECT `+scheduleRootColumns+` FROM schedules WHERE id = $1 FOR UPDATE`, scheduleID))
}

// GetTailRevision returns the open-ended revision of a schedule.
func (t *scheduleConfigTx) GetTailRevision(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRevision, error) {
	row := t.tx.QueryRowContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1 AND effective_to IS NULL`, scheduleID)
	return scanScheduleRevisionRow(row)
}

// CloseRevision closes exactly the revision it names. Anything other than one
// affected row - the revision was already closed, belongs to another schedule,
// or the caller passed a stale ID - is a mismatch, never a silent success.
func (t *scheduleConfigTx) CloseRevision(ctx context.Context, scheduleID, expectedRevisionID string, at time.Time) error {
	if expectedRevisionID == "" {
		return fmt.Errorf("%w: no revision id to close", scheduleconfig.ErrInvariantViolation)
	}
	res, err := t.tx.ExecContext(ctx,
		`UPDATE schedule_revisions SET effective_to = $1
		 WHERE id = $2 AND schedule_id = $3 AND effective_to IS NULL`,
		scheduleconfig.NormalizeTimestamp(at), expectedRevisionID, scheduleID)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return requireOneRow(res, scheduleconfig.ErrRevisionMismatch)
}

func (t *scheduleConfigTx) InsertRevision(ctx context.Context, revision *scheduleconfig.ScheduleRevision) error {
	if err := scheduleconfig.PrepareRevision(revision); err != nil {
		return err
	}
	snapshot, err := rotation.EncodeSnapshot(revision.Snapshot)
	if err != nil {
		return err
	}
	var summary []byte
	if revision.ChangeSummary != nil {
		if summary, err = json.Marshal(revision.ChangeSummary); err != nil {
			return err
		}
	}

	_, err = t.tx.ExecContext(ctx,
		`INSERT INTO schedule_revisions
		 (id, schedule_id, version, kind, snapshot, effective_from, effective_to, recorded_at, created_by, change_reason, change_summary)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		revision.ID, revision.ScheduleID, revision.Version, revision.Kind, snapshot,
		revision.EffectiveFrom, revision.EffectiveTo, revision.RecordedAt,
		revision.CreatedBy, revision.ChangeReason, nullableJSON(summary))
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return nil
}

// AdvanceVersion is the optimistic-concurrency compare-and-set. A mismatch
// means someone else committed a change since the caller read the version.
func (t *scheduleConfigTx) AdvanceVersion(ctx context.Context, scheduleID string, expected int64, at time.Time) error {
	res, err := t.tx.ExecContext(ctx,
		`UPDATE schedules SET config_version = config_version + 1, updated_at = $1
		 WHERE id = $2 AND config_version = $3`,
		scheduleconfig.NormalizeTimestamp(at), scheduleID, expected)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return requireOneRow(res, scheduleconfig.ErrVersionConflict)
}

// SetScheduleDeleted moves the soft-delete projection. A missing row is
// ErrScheduleNotFound rather than a silent no-op: the caller believed it held
// a locked schedule, and being wrong about that is not something to swallow.
func (t *scheduleConfigTx) SetScheduleDeleted(ctx context.Context, scheduleID string, deletedAt *time.Time) error {
	var at any
	if deletedAt != nil {
		at = scheduleconfig.NormalizeTimestamp(*deletedAt)
	}
	res, err := t.tx.ExecContext(ctx,
		`UPDATE schedules SET deleted_at = $1 WHERE id = $2`, at, scheduleID)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return requireOneRow(res, scheduleconfig.ErrScheduleNotFound)
}

// MaxOverrideRecordedAt is the newest recorded_at across every override
// revision of a schedule, tombstones included. Nil means the schedule has no
// override history at all.
func (t *scheduleConfigTx) MaxOverrideRecordedAt(ctx context.Context, scheduleID string) (*time.Time, error) {
	var max sql.NullTime
	err := t.tx.QueryRowContext(ctx,
		`SELECT MAX(recorded_at) FROM schedule_override_revisions WHERE schedule_id = $1`,
		scheduleID).Scan(&max)
	if err != nil {
		return nil, err
	}
	if !max.Valid {
		return nil, nil
	}
	v := scheduleconfig.NormalizeTimestamp(max.Time)
	return &v, nil
}

// LockUsers takes a shared row lock on the named users, in ID order.
//
// FOR SHARE, not FOR UPDATE: two commands naming overlapping users have no
// reason to wait for each other, and only erasure - which takes FOR UPDATE on
// the one user it erases - needs to be excluded. Sorting the IDs first is
// belt and braces for a lock that cannot self-deadlock anyway.
//
// IDs that match nothing are silently absent from the result. That is correct:
// this is a serialization point, and rejecting an unknown user is membership
// validation's job, which runs later and against fresher state.
func (t *scheduleConfigTx) LockUsers(ctx context.Context, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	sorted := append([]string(nil), userIDs...)
	sort.Strings(sorted)

	rows, err := t.tx.QueryContext(ctx,
		`SELECT id FROM users WHERE id = ANY($1) ORDER BY id FOR SHARE`, pq.Array(sorted))
	if err != nil {
		return mapScheduleWriteError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ActiveUserIDs filters a set of IDs down to the users that have not been
// erased. It is a command-side read: the caller holds the shared lock on these
// rows already, so the answer cannot change before it is acted on.
func (t *scheduleConfigTx) ActiveUserIDs(ctx context.Context, userIDs []string) ([]string, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := t.tx.QueryContext(ctx,
		`SELECT id FROM users WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id`,
		pq.Array(userIDs))
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

// DeleteTeamMembership removes one membership.
//
// A membership that is not there is not an error: the caller asked for the
// person to end up outside the team, and they are. The guard that decides
// whether the removal is allowed at all lives in the service, above this.
func (t *scheduleConfigTx) DeleteTeamMembership(ctx context.Context, teamID, userID string) error {
	_, err := t.tx.ExecContext(ctx,
		`DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, userID)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return nil
}

// scanScheduleRevisionRow adapts the single-row queries: only there does "no
// rows" mean the named revision does not exist. In a range query an empty
// result is an ordinary answer.
func scanScheduleRevisionRow(row *sql.Row) (*scheduleconfig.ScheduleRevision, error) {
	rev, err := scanScheduleRevision(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, scheduleconfig.ErrRevisionNotFound
	}
	return rev, err
}

func scanScheduleRevision(row rowScanner) (*scheduleconfig.ScheduleRevision, error) {
	var (
		rev         scheduleconfig.ScheduleRevision
		snapshot    []byte
		summary     []byte
		effectiveTo sql.NullTime
		createdBy   sql.NullString
		reason      sql.NullString
	)
	err := row.Scan(&rev.ID, &rev.ScheduleID, &rev.Version, &rev.Kind, &snapshot,
		&rev.EffectiveFrom, &effectiveTo, &rev.RecordedAt, &createdBy, &reason, &summary)
	if err != nil {
		return nil, err
	}

	// A corrupt snapshot surfaces as a decode error, never as an empty
	// rotation: empty is a valid explicit configuration, not a fallback. The
	// sentinel is what the API error mapper turns into a 500 with an alert
	// rather than into a plausible-looking answer.
	if rev.Snapshot, err = rotation.DecodeSnapshot(snapshot); err != nil {
		return nil, fmt.Errorf("%w: revision %s: %w", scheduleconfig.ErrSnapshotDecode, rev.ID, err)
	}
	if effectiveTo.Valid {
		v := effectiveTo.Time
		rev.EffectiveTo = &v
	}
	if createdBy.Valid {
		v := createdBy.String
		rev.CreatedBy = &v
	}
	if reason.Valid {
		v := reason.String
		rev.ChangeReason = &v
	}
	if len(summary) > 0 {
		var parsed rotation.ChangeSummary
		if err := json.Unmarshal(summary, &parsed); err != nil {
			return nil, fmt.Errorf("store: revision %s has undecodable change_summary: %w", rev.ID, err)
		}
		rev.ChangeSummary = &parsed
	}
	return &rev, nil
}

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func requireOneRow(res sql.Result, mismatch error) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return mismatch
	}
	return nil
}

// mapScheduleWriteError converts PostgreSQL integrity errors into the typed
// errors of the scheduleconfig contract. Everything that is not a recognized
// contract conflict becomes an invariant violation rather than leaking a raw
// SQL error to the application layer.
func mapScheduleWriteError(err error) error {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return err
	}
	switch pqErr.Code.Name() {
	case "unique_violation":
		if pqErr.Constraint == scheduleTeamUniqueConstraint {
			return scheduleconfig.ErrScheduleExists
		}
		return fmt.Errorf("%w: unique constraint %q: %s", scheduleconfig.ErrInvariantViolation, pqErr.Constraint, pqErr.Message)
	case "foreign_key_violation":
		if pqErr.Constraint == scheduleTeamFKConstraint {
			return scheduleconfig.ErrTeamNotFound
		}
		return fmt.Errorf("%w: foreign key %q: %s", scheduleconfig.ErrInvariantViolation, pqErr.Constraint, pqErr.Message)
	case "check_violation", "exclusion_violation", "not_null_violation":
		return fmt.Errorf("%w: constraint %q on column %q: %s",
			scheduleconfig.ErrInvariantViolation, pqErr.Constraint, pqErr.Column, pqErr.Message)
	}
	return err
}
