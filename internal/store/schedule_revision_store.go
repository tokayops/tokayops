package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// scheduleTeamUniqueConstraint is the UNIQUE(team_id) constraint on schedules.
// Only this constraint means "the team already has a schedule"; any other
// unique violation (a repeated primary key, say) has different semantics and
// must not be laundered into a caller-recoverable conflict.
const scheduleTeamUniqueConstraint = "schedules_team_id_key"

// scheduleRevisionColumns is the SELECT list every revision scan expects.
const scheduleRevisionColumns = `id, schedule_id, version, snapshot, effective_from, effective_to,
	recorded_at, created_by, change_reason, change_summary`

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

	if err := fn(&scheduleConfigTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

type scheduleConfigTx struct {
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
	if root == nil || initial == nil {
		return fmt.Errorf("%w: nil root or initial revision", scheduleconfig.ErrInvariantViolation)
	}
	if root.ID == "" || root.TeamID == "" {
		return fmt.Errorf("%w: schedule root needs an id and a team id", scheduleconfig.ErrInvariantViolation)
	}
	if initial.ScheduleID != root.ID {
		return fmt.Errorf("%w: initial revision belongs to schedule %q, not %q",
			scheduleconfig.ErrInvariantViolation, initial.ScheduleID, root.ID)
	}
	if initial.Version != 1 {
		return fmt.Errorf("%w: initial revision must be version 1, got %d",
			scheduleconfig.ErrInvariantViolation, initial.Version)
	}
	if initial.EffectiveTo != nil {
		return fmt.Errorf("%w: initial revision must be open-ended", scheduleconfig.ErrInvariantViolation)
	}

	effectiveFrom := scheduleconfig.NormalizeTimestamp(initial.EffectiveFrom)
	if effectiveFrom.IsZero() {
		return fmt.Errorf("%w: initial revision needs an effective_from", scheduleconfig.ErrInvariantViolation)
	}
	recordedAt := normalizeRecordedAt(initial.RecordedAt)

	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO schedules (id, team_id, config_version, history_complete_from, created_at, updated_at)
		 VALUES ($1, $2, 1, $3, $4, $4)`,
		root.ID, root.TeamID, effectiveFrom, recordedAt)
	if err != nil {
		return mapScheduleWriteError(err)
	}

	initial.EffectiveFrom = effectiveFrom
	initial.RecordedAt = recordedAt
	if err := t.InsertRevision(ctx, initial); err != nil {
		return err
	}

	root.ConfigVersion = 1
	root.HistoryCompleteFrom = &effectiveFrom
	root.DeletedAt = nil
	return nil
}

// LockSchedule takes the row lock that serializes writes to one schedule.
func (t *scheduleConfigTx) LockSchedule(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRoot, error) {
	row := t.tx.QueryRowContext(ctx,
		`SELECT id, team_id, config_version, history_complete_from, deleted_at
		 FROM schedules WHERE id = $1 FOR UPDATE`, scheduleID)

	var root scheduleconfig.ScheduleRoot
	var historyFrom, deletedAt sql.NullTime
	err := row.Scan(&root.ID, &root.TeamID, &root.ConfigVersion, &historyFrom, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	if err != nil {
		return nil, err
	}
	if historyFrom.Valid {
		v := historyFrom.Time
		root.HistoryCompleteFrom = &v
	}
	if deletedAt.Valid {
		v := deletedAt.Time
		root.DeletedAt = &v
	}
	return &root, nil
}

// GetEffectiveRevision returns the revision whose half-open interval contains
// `at`. The lower bound is part of the predicate on purpose: without it a
// future revision would answer a query about the past.
func (t *scheduleConfigTx) GetEffectiveRevision(ctx context.Context, scheduleID string, at time.Time) (*scheduleconfig.ScheduleRevision, error) {
	row := t.tx.QueryRowContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1
		   AND effective_from <= $2
		   AND (effective_to IS NULL OR effective_to > $2)`,
		scheduleID, scheduleconfig.NormalizeTimestamp(at))
	return scanScheduleRevision(row)
}

// GetTailRevision returns the open-ended revision of a schedule.
func (t *scheduleConfigTx) GetTailRevision(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRevision, error) {
	row := t.tx.QueryRowContext(ctx,
		`SELECT `+scheduleRevisionColumns+`
		 FROM schedule_revisions
		 WHERE schedule_id = $1 AND effective_to IS NULL`, scheduleID)
	return scanScheduleRevision(row)
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
	if revision == nil {
		return fmt.Errorf("%w: nil revision", scheduleconfig.ErrInvariantViolation)
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

	effectiveFrom := scheduleconfig.NormalizeTimestamp(revision.EffectiveFrom)
	var effectiveTo *time.Time
	if revision.EffectiveTo != nil {
		v := scheduleconfig.NormalizeTimestamp(*revision.EffectiveTo)
		effectiveTo = &v
	}
	recordedAt := normalizeRecordedAt(revision.RecordedAt)

	_, err = t.tx.ExecContext(ctx,
		`INSERT INTO schedule_revisions
		 (id, schedule_id, version, snapshot, effective_from, effective_to, recorded_at, created_by, change_reason, change_summary)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		revision.ID, revision.ScheduleID, revision.Version, snapshot,
		effectiveFrom, effectiveTo, recordedAt,
		revision.CreatedBy, revision.ChangeReason, nullableJSON(summary))
	if err != nil {
		return mapScheduleWriteError(err)
	}

	revision.EffectiveFrom = effectiveFrom
	revision.EffectiveTo = effectiveTo
	revision.RecordedAt = recordedAt
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

func scanScheduleRevision(row *sql.Row) (*scheduleconfig.ScheduleRevision, error) {
	var (
		rev         scheduleconfig.ScheduleRevision
		snapshot    []byte
		summary     []byte
		effectiveTo sql.NullTime
		createdBy   sql.NullString
		reason      sql.NullString
	)
	err := row.Scan(&rev.ID, &rev.ScheduleID, &rev.Version, &snapshot,
		&rev.EffectiveFrom, &effectiveTo, &rev.RecordedAt, &createdBy, &reason, &summary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, scheduleconfig.ErrRevisionNotFound
	}
	if err != nil {
		return nil, err
	}

	// A corrupt snapshot surfaces as a decode error, never as an empty
	// rotation: empty is a valid explicit configuration, not a fallback.
	if rev.Snapshot, err = rotation.DecodeSnapshot(snapshot); err != nil {
		return nil, err
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

// normalizeRecordedAt keeps recorded time at database resolution and fills in
// a value for callers that left it zero.
func normalizeRecordedAt(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return scheduleconfig.NormalizeTimestamp(t)
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
	case "check_violation", "exclusion_violation":
		return fmt.Errorf("%w: constraint %q: %s", scheduleconfig.ErrInvariantViolation, pqErr.Constraint, pqErr.Message)
	}
	return err
}
