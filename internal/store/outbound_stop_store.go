package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// A step of the policy that does not continue on failure stops the steps
// after it: what has not gone out yet is withdrawn, from whichever door the
// failure came through.

// afterBeginGroupLock is a test hook, called by a begin that took the group
// first because the refusal it records may stop the escalation. A test counts
// it to prove the ordinary begin takes no such lock, and puts an
// acknowledgement exactly there.
var afterBeginGroupLock func()

// stopsEscalation reads the policy's word out of a commitment before its row
// is locked: the flag is in the payload, which does not change, and the lock
// order depends on the answer. Anything but an escalation of a numbered step
// answers no - the firehose is no step and a satellite is not a page.
func stopsEscalation(intent outbound.Intent) (keys.Slot, bool) {
	if intent.KeyKind != keys.KindEscalation && intent.KeyKind != keys.KindEscalationReplay {
		return keys.Slot{}, false
	}
	if intent.Satellite() || intent.AlertGroupID == "" {
		return keys.Slot{}, false
	}
	payload, err := keys.DecodeEscalationPayload(intent.PayloadSchemaVersion, intent.Payload)
	if err != nil || !payload.StopOnFailure || payload.Slot.Kind != keys.SlotPolicy {
		// An unreadable payload is refused by the attempt itself; it stops
		// nothing on the way.
		return keys.Slot{}, false
	}
	return payload.Slot, true
}

// stopEscalationTx withdraws what the policy would have sent after a step
// that failed for good: every commitment of the same claim with a later
// step of the policy and no message yet, and the satellites that follow
// such a card. Under the group's lock, taken first by the door that calls
// this - it writes a line into the alert's history. Best effort by
// construction: a later step already out stays out.
//
// The three updates are the withdrawal's own (cancelIntentsAtTx): a step
// waiting is withdrawn with its lease, one in flight is asked to stop, one
// waiting for a person is withdrawn with the doubt kept. A second call for
// the same step finds nothing and writes nothing.
func stopEscalationTx(ctx context.Context, tx *sql.Tx, failed outbound.Intent,
	step keys.Slot) (int, error) {

	const later = `
		WHERE batch_id = $1 AND NOT receipt_recorded
		  AND payload->'slot'->>'kind' = 'policy'
		  AND (payload->'slot'->>'index')::int > $2`
	reason := fmt.Sprintf("step %d failed and the policy stops there", step.Index)

	notSent, err := stopRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', lease_token = NULL, locked_until = NULL,
		    worker_id = NULL, updated_at = now()`+later+`
		  AND status = 'pending' `+notFollowingASentCard+`
		RETURNING id`, failed.BatchID, step.Index)
	if err != nil {
		return 0, err
	}
	inFlight, err := stopRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET cancellation_requested = TRUE, updated_at = now()`+later+`
		  AND status = 'sending' `+notFollowingASentCard+`
		RETURNING id`, failed.BatchID, step.Index)
	if err != nil {
		return 0, err
	}
	waiting, err := stopRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', updated_at = now()`+later+`
		  AND status = 'manual_review' `+notFollowingASentCard+`
		RETURNING id`, failed.BatchID, step.Index)
	if err != nil {
		return 0, err
	}

	for _, id := range notSent {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason, outbound.ActorWorker); err != nil {
			return 0, err
		}
	}
	for _, id := range inFlight {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "cancellation_requested",
			reason, outbound.ActorWorker); err != nil {
			return 0, err
		}
	}
	for _, id := range waiting {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason+"; the outcome of the previous attempt stays unknown", outbound.ActorWorker); err != nil {
			return 0, err
		}
	}

	// One line in the alert's history, for the pages: the satellites of a
	// card are not ones, and a stop that found only mirrors to withdraw
	// writes nothing. The line is dated one microsecond after this
	// transaction's instant, which is the instant the failure's own line
	// carries: written at now() it would share it, and the history would
	// say in half the cases that the escalation stopped before the step
	// failed.
	all := append(append(append([]string(nil), notSent...), inFlight...), waiting...)
	var pages int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM outbound_intents
		WHERE id = ANY($1) AND parent_intent_id IS NULL`, pq.Array(all)).Scan(&pages); err != nil {
		return 0, fmt.Errorf("count the pages the stop withdrew: %w", err)
	}
	withdrawn := len(notSent) + len(waiting)
	if pages == 0 {
		return withdrawn, nil
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return 0, err
	}
	if err := addTimelineEventsTx(ctx, tx, []*model.TimelineEvent{{
		ID:           uuid.New().String(),
		AlertGroupID: failed.AlertGroupID,
		Type:         model.TimelineEventNotificationFailed,
		Message:      fmt.Sprintf("Escalation stopped: step %d failed and the policy does not continue", step.Index),
		Actor:        "worker",
		CreatedAt:    now.Add(time.Microsecond),
	}}); err != nil {
		return 0, err
	}
	return withdrawn, nil
}

func stopRowsTx(ctx context.Context, tx *sql.Tx, query, batchID string, after int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, batchID, after)
	if err != nil {
		return nil, fmt.Errorf("stop the escalation of claim %s: %w", batchID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
