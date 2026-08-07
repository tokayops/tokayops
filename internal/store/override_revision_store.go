package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// normalizeBound keeps an optional query bound at database resolution. A
// bound carrying nanoseconds would compare differently than the same value
// does once stored.
func normalizeBound(t *time.Time) any {
	if t == nil {
		return nil
	}
	return scheduleconfig.NormalizeTimestamp(*t)
}

// getOverrideProjection returns the winning revision per override_id.
//
// Three steps, and their order is load-bearing:
//
//  1. pick the latest revision per override_id, bounded by asOf when the
//     caller asks for the state as it was known at a system time;
//  2. only THEN drop tombstones - filtering `deleted` before the grouping
//     would let the revision preceding a delete win the MAX and resurrect an
//     override the user removed;
//  3. only THEN apply the validity range - filtering it inside the subquery
//     would pick the latest revision that happens to overlap the range rather
//     than the latest revision, and an earlier version of an override may
//     well have covered a different interval.
//
// The winner is chosen by MAX(revision) rather than by recorded_at order:
// two revisions of one override can share a microsecond, and revision numbers
// are the only strictly ordered thing about them.
//
// A nil bound means unbounded on that side. The ::timestamptz casts are
// required, not decorative: without them PostgreSQL cannot infer the type of
// a nil parameter.
func getOverrideProjection(ctx context.Context, q sqlQueryer, scheduleID string,
	from, until, asOf *time.Time) ([]scheduleconfig.OverrideRevision, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT o.revision_id, o.override_id, o.schedule_id, o.revision, o.layer, o.user_id,
		        o.valid_from, o.valid_to, o.reason, o.deleted, o.recorded_at, o.recorded_by
		 FROM schedule_override_revisions o
		 JOIN (SELECT override_id, MAX(revision) AS revision
		       FROM schedule_override_revisions
		       WHERE schedule_id = $1
		         AND ($2::timestamptz IS NULL OR recorded_at <= $2)
		       GROUP BY override_id) last
		   ON last.override_id = o.override_id AND last.revision = o.revision
		 WHERE o.schedule_id = $1
		   AND NOT o.deleted
		   AND ($3::timestamptz IS NULL OR o.valid_to > $3)
		   AND ($4::timestamptz IS NULL OR o.valid_from < $4)
		 ORDER BY o.valid_from, o.override_id`,
		scheduleID, normalizeBound(asOf), normalizeBound(from), normalizeBound(until))
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
	if err := scheduleconfig.PrepareOverrideRevision(rev); err != nil {
		return err
	}
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
