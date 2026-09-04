package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The sweep: one chunk of delivery history older than the cutoff, removed in
// one transaction.

// retentionLockKey is the advisory lock one pass of the sweep holds for its
// transaction. Several instances tick; one sweeps and the others answer busy
// without waiting. Its own key, shared with nothing else in this database.
const retentionLockKey int64 = 0x746f6b61795f7277 // "tokay_rw", the retention writer

// afterSweepLock is a test hook, called once the transaction holds the
// advisory lock and the chunk's row locks and before anything is deleted.
// Production leaves it nil. It exists so a test can hold a pass here while a
// second instance ticks, an operator revives a row, or a replay reads one.
var afterSweepLock func()

// terminalStatusList is the vocabulary of what the sweep removes, as one SQL
// literal, written once so the partial index and the predicate that uses it
// are the same text - which is what lets the planner use the index.
var terminalStatusList = func() string {
	list := ""
	for _, status := range outbound.Statuses() {
		if !status.Terminal() {
			continue
		}
		if list != "" {
			list += ", "
		}
		list += "'" + string(status) + "'"
	}
	return list
}()

// finishedOutboxStatusList is the same for events: what the fan-out marked
// finished, and what the old worker finished before this build.
const finishedOutboxStatusList = "'fanned_out', 'completed', 'failed'"

// SweepDeliveryHistory removes one chunk of history older than the cutoff.
//
// The order is the foreign keys' - observations, events, attempts, then the
// commitments - none of which cascades, on purpose: a row that goes only by an
// explicit statement is a row nothing removes by accident. The commitments are
// chosen with FOR UPDATE SKIP LOCKED, and that lock is part of the predicate:
// a commitment an operator is reviving right now is skipped, and after their
// commit it is pending and no longer qualifies; a commitment a replay is
// reading under FOR SHARE is skipped too, and the event under it stays.
//
// An event goes only when it is older than the cutoff by its fan-out AND no
// claim on it - the fan-out's, any replay's - holds a commitment. A margin of
// time would not say that; a replay can arrive on the last day of the window,
// and a worker can stand still for longer than any margin.
func (s *Store) SweepDeliveryHistory(ctx context.Context, cutoff time.Time, chunk int) (outbound.SweepResult, error) {
	if chunk <= 0 {
		return outbound.SweepResult{}, outboundContractf("a sweep chunk is at least one row, got %d", chunk)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.SweepResult{}, err
	}
	defer tx.Rollback()

	var held bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, retentionLockKey).Scan(&held); err != nil {
		return outbound.SweepResult{}, fmt.Errorf("take the retention lock: %w", err)
	}
	if !held {
		return outbound.SweepResult{Busy: true}, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM outbound_intents
		WHERE status IN (`+terminalStatusList+`) AND updated_at < $1
		ORDER BY updated_at, id
		LIMIT $2
		FOR UPDATE SKIP LOCKED`, cutoff, chunk)
	if err != nil {
		return outbound.SweepResult{}, fmt.Errorf("choose the commitments to remove: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return outbound.SweepResult{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return outbound.SweepResult{}, err
	}

	if afterSweepLock != nil {
		afterSweepLock()
	}

	var deleted outbound.SweepCounts
	if len(ids) > 0 {
		list := pq.Array(ids)
		for _, step := range []struct {
			count *int64
			sql   string
		}{
			{&deleted.Observations, `DELETE FROM outbound_attempt_observations
				WHERE attempt_id IN (SELECT id FROM outbound_attempts WHERE intent_id = ANY($1))`},
			{&deleted.Events, `DELETE FROM outbound_intent_events WHERE intent_id = ANY($1)`},
			{&deleted.Attempts, `DELETE FROM outbound_attempts WHERE intent_id = ANY($1)`},
			{&deleted.Intents, `DELETE FROM outbound_intents WHERE id = ANY($1)`},
		} {
			n, err := execCount(ctx, tx, step.sql, list)
			if err != nil {
				return outbound.SweepResult{}, err
			}
			*step.count = n
		}
	}

	outbox, err := execCount(ctx, tx, `
		WITH doomed AS (
			SELECT e.id FROM event_outbox e
			WHERE e.status IN (`+finishedOutboxStatusList+`)
			  AND e.fanned_out_at < $1
			  AND NOT EXISTS (
			      SELECT 1 FROM outbound_batches b
			      JOIN outbound_intents i ON i.batch_id = b.id
			      WHERE b.event_id = e.id)
			ORDER BY e.fanned_out_at, e.id
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		DELETE FROM event_outbox e USING doomed WHERE e.id = doomed.id`, cutoff, chunk)
	if err != nil {
		return outbound.SweepResult{}, err
	}
	deleted.Outbox = outbox

	if err := tx.Commit(); err != nil {
		return outbound.SweepResult{}, err
	}
	return outbound.SweepResult{Deleted: deleted}, nil
}

func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("remove delivery history: %w", err)
	}
	return result.RowsAffected()
}
