package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// GetCurrentOverrides projects the current override state.
//
// The order of the two steps is load-bearing: pick the latest revision per
// override_id FIRST, then drop tombstones. Filtering `deleted` before the
// grouping would let the revision preceding a delete win the MAX and
// resurrect an override the user removed.
func (t *scheduleConfigTx) GetCurrentOverrides(ctx context.Context, scheduleID string) ([]scheduleconfig.OverrideRevision, error) {
	rows, err := t.tx.QueryContext(ctx,
		`SELECT o.revision_id, o.override_id, o.schedule_id, o.revision, o.layer, o.user_id,
		        o.valid_from, o.valid_to, o.reason, o.deleted, o.recorded_at, o.recorded_by
		 FROM schedule_override_revisions o
		 JOIN (SELECT override_id, MAX(revision) AS revision
		       FROM schedule_override_revisions
		       WHERE schedule_id = $1
		       GROUP BY override_id) last
		   ON last.override_id = o.override_id AND last.revision = o.revision
		 WHERE o.schedule_id = $1 AND NOT o.deleted
		 ORDER BY o.valid_from, o.override_id`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []scheduleconfig.OverrideRevision
	for rows.Next() {
		var (
			rev        scheduleconfig.OverrideRevision
			reason     sql.NullString
			recordedBy sql.NullString
		)
		if err := rows.Scan(&rev.RevisionID, &rev.OverrideID, &rev.ScheduleID, &rev.Revision,
			&rev.Layer, &rev.UserID, &rev.ValidFrom, &rev.ValidTo, &reason, &rev.Deleted,
			&rev.RecordedAt, &recordedBy); err != nil {
			return nil, err
		}
		if reason.Valid {
			v := reason.String
			rev.Reason = &v
		}
		if recordedBy.Valid {
			v := recordedBy.String
			rev.RecordedBy = &v
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// InsertOverrideRevision appends one version of a logical override. Nothing
// here ever updates or deletes an earlier row: create writes revision 1, edit
// appends the next revision, delete appends a tombstone.
func (t *scheduleConfigTx) InsertOverrideRevision(ctx context.Context, rev *scheduleconfig.OverrideRevision) error {
	if rev == nil {
		return fmt.Errorf("%w: nil override revision", scheduleconfig.ErrInvariantViolation)
	}
	if rev.RevisionID == "" {
		rev.RevisionID = generateUUID()
	}
	if rev.OverrideID == "" {
		return fmt.Errorf("%w: override revision needs a logical override id", scheduleconfig.ErrInvariantViolation)
	}
	if rev.Revision < 1 {
		return fmt.Errorf("%w: override revision number must start at 1, got %d",
			scheduleconfig.ErrInvariantViolation, rev.Revision)
	}
	if rev.Layer == "" {
		rev.Layer = scheduleconfig.LayerL1
	}

	rev.ValidFrom = scheduleconfig.NormalizeTimestamp(rev.ValidFrom)
	rev.ValidTo = scheduleconfig.NormalizeTimestamp(rev.ValidTo)
	rev.RecordedAt = normalizeRecordedAt(rev.RecordedAt)

	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO schedule_override_revisions
		 (revision_id, override_id, schedule_id, revision, layer, user_id,
		  valid_from, valid_to, reason, deleted, recorded_at, recorded_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		rev.RevisionID, rev.OverrideID, rev.ScheduleID, rev.Revision, rev.Layer, rev.UserID,
		rev.ValidFrom, rev.ValidTo, rev.Reason, rev.Deleted, rev.RecordedAt, rev.RecordedBy)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return nil
}
