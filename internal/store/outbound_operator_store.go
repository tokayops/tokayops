package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// Withdrawal and operator decisions: the two ways a commitment ends without the
// provider having anything to say about it.

// cancelIntentsTx withdraws what an alert group still owes, inside the same
// transaction that acknowledges or resolves it.
//
// The scope is decided by the external effect, not by the kind of message. A
// commitment with a receipt is not cancelled - something exists out there and
// pretending otherwise would be a lie the history has to keep. A send already
// in flight is flagged rather than cancelled, and the flag is consumed when
// that send finishes: it may already have landed, and the two outcomes have to
// converge on the same visible result.
func cancelIntentsTx(ctx context.Context, tx *sql.Tx, alertGroupID, reason, actor string) error {
	// Nothing has gone out and nothing will: these are withdrawn outright, and
	// the lease goes with them so the worker holding one finds out at its next
	// compare-and-set.
	notSent, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', lease_token = NULL, locked_until = NULL,
		    worker_id = NULL, updated_at = now()
		WHERE alert_group_id = $1 AND receipt IS NULL AND status = 'pending'
		RETURNING id`, alertGroupID)
	if err != nil {
		return err
	}

	// In flight. The outcome decides: a send that failed becomes a withdrawal,
	// one that succeeded becomes a message the group will update instead.
	inFlight, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET cancellation_requested = TRUE, updated_at = now()
		WHERE alert_group_id = $1 AND receipt IS NULL AND status = 'sending'
		RETURNING id`, alertGroupID)
	if err != nil {
		return err
	}

	// Waiting for a person about a call that never produced anything: the
	// question is moot now, but the history keeps the doubt.
	waiting, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', updated_at = now()
		WHERE alert_group_id = $1 AND receipt IS NULL AND status = 'manual_review'
		RETURNING id`, alertGroupID)
	if err != nil {
		return err
	}

	for _, id := range notSent {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason, actor); err != nil {
			return err
		}
	}
	for _, id := range inFlight {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "cancellation_requested",
			reason, actor); err != nil {
			return err
		}
	}
	for _, id := range waiting {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason+"; the outcome of the previous attempt stays unknown", actor); err != nil {
			return err
		}
	}

	touched := len(notSent) + len(inFlight) + len(waiting)
	if touched == 0 {
		return nil
	}

	// One line in the alert's history, and a line each in the commitments' own:
	// the group's timeline says what happened to the alert, and the journal says
	// what happened to every promise it had made.
	return addTimelineTx(ctx, tx, alertGroupID, model.TimelineEventNotificationFailed,
		fmt.Sprintf("%d pending notification(s) withdrawn: %s", touched, reason), actor)
}

func cancelRowsTx(ctx context.Context, tx *sql.Tx, query, alertGroupID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, alertGroupID)
	if err != nil {
		return nil, fmt.Errorf("withdraw the notifications of %s: %w", alertGroupID, err)
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

// ResolveAmbiguity is a person deciding what a stuck commitment does.
//
// Every path through it is an explicit decision with an audit record. Nothing
// here happens automatically, and nothing here is reachable for a commitment
// that is still being worked on: the machine refuses those, and this returns
// what the commitment is doing instead.
func (s *Store) ResolveAmbiguity(ctx context.Context,
	req outbound.ResolveAmbiguityRequest) (outbound.ResolveAmbiguityResult, error) {

	var groupID string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(alert_group_id, '') FROM outbound_intents WHERE id = $1`,
		req.IntentID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveNotFound}, nil
	}
	if err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}
	defer tx.Rollback()

	if err := setLockTimeoutTx(ctx, tx); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	// The group first for every decision that could settle a delivery or start
	// a new external effect - which is every decision except a plain
	// withdrawal, and taking it unconditionally is simpler than being clever
	// about which is which.
	groupStatus := ""
	if groupID != "" {
		if err := lockAlertGroupTx(ctx, tx, groupID); err != nil {
			return outbound.ResolveAmbiguityResult{}, err
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM alert_groups WHERE id = $1`, groupID).Scan(&groupStatus); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return outbound.ResolveAmbiguityResult{}, err
		}
	}

	intent, _, err := lockIntentTx(ctx, tx, req.IntentID)
	if err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}
	if intent == nil {
		return outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveNotFound}, nil
	}

	switch intent.Status {
	case outbound.StatusManualReview, outbound.StatusPermanentFailed, outbound.StatusExpired:
	default:
		// Somebody got there first - another operator, or the acknowledgement
		// of the alert. The current state comes back so the caller can see what
		// happened rather than guess.
		return outbound.ResolveAmbiguityResult{
			Outcome: outbound.ResolveAlreadyResolved, Status: intent.Status,
		}, nil
	}

	if closed, err := decisionIsStale(*intent, req.Decision, groupStatus); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	} else if closed {
		return outbound.ResolveAmbiguityResult{
			Outcome: outbound.ResolveBusinessClosed, Status: intent.Status,
		}, nil
	}

	lastKind, ambiguous, err := lastAttemptFactsTx(ctx, tx, req.IntentID, intent.GenerationNo)
	if err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:                *intent,
		Trigger:               outbound.TriggerOperator,
		Decision:              req.Decision,
		LastAttemptKind:       lastKind,
		AmbiguousInGeneration: ambiguous,
		AcceptedDuplicateRisk: req.AcceptedDuplicateRisk,
		ResourceLossConfirmed: req.ResourceLossConfirmed,
		NewExpiryProvided:     req.NewExpiresAt != nil,
	})
	if errors.Is(err, outbound.ErrInvalidTransition) {
		return outbound.ResolveAmbiguityResult{
			Outcome: outbound.ResolveInvalidDecision, Status: intent.Status,
		}, nil
	}
	if err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	if err := applyTransitionTx(ctx, tx, transitionWrite{
		Intent:          *intent,
		Transition:      transition,
		AppliedRevision: intent.DesiredRevision,
		NewExpires:      req.NewExpiresAt,
		Actor:           req.Actor,
		Reason:          req.Reason,
	}); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	if err := appendIntentEventTx(ctx, tx, req.IntentID, nextEventSeq, "operator_decision",
		fmt.Sprintf("%s: %s", req.Decision, req.Reason), req.Actor); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	if intent.GroupBound() {
		if err := groupEffectsTx(ctx, tx, intent.AlertGroupID, transition); err != nil {
			return outbound.ResolveAmbiguityResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}
	return outbound.ResolveAmbiguityResult{
		Outcome: outbound.ResolveResolved, Status: transition.To, Row: transition.Row,
	}, nil
}

// decisionIsStale asks whether the alert this commitment belongs to has moved
// on.
//
// Acknowledgement does not touch a terminal commitment, so without this an
// operator could revive a page for an incident somebody finished hours ago. The
// one exception is a card that already exists: bringing it to the current
// revision is not a new page, it is the correction of one that is still on
// somebody's screen.
func decisionIsStale(intent outbound.Intent, decision outbound.Decision, groupStatus string) (bool, error) {
	switch decision {
	case outbound.DecisionAssumeAccepted, outbound.DecisionCancel:
		return false, nil
	case outbound.DecisionRetryCurrentGeneration, outbound.DecisionRetryNewGeneration:
	default:
		return false, nil
	}

	if !intent.GroupBound() {
		// A commitment with no alert group - a handover notification - carries
		// its own deadline instead, and that is what says whether it still
		// matters.
		return false, nil
	}

	switch model.AlertGroupStatus(groupStatus) {
	case model.AlertGroupStatusAcknowledged, model.AlertGroupStatusResolved,
		model.AlertGroupStatusClosed:
	default:
		return false, nil
	}

	if intent.Form == outbound.FormEditable &&
		decision == outbound.DecisionRetryCurrentGeneration && intent.HasReceipt {
		return false, nil
	}
	return true, nil
}

// lastAttemptFactsTx reads what the operator's guards need: what the last
// attempt was trying to do, and whether anything in this generation ended in
// doubt.
//
// The last attempt is chosen by its number rather than by its timestamp: two
// records written in one transaction share a clock reading, and "the last one"
// has to be an answer rather than a coin toss.
func lastAttemptFactsTx(ctx context.Context, tx *sql.Tx, intentID string,
	generation int) (outbound.AttemptKind, bool, error) {

	var kind sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT attempt_kind FROM outbound_attempts
		WHERE intent_id = $1 ORDER BY attempt_no DESC LIMIT 1`, intentID).Scan(&kind)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}

	var ambiguous bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM outbound_attempts
			WHERE intent_id = $1 AND generation_no = $2 AND outcome = 'ambiguous')`,
		intentID, generation).Scan(&ambiguous); err != nil {
		return "", false, err
	}

	return outbound.AttemptKind(kind.String), ambiguous, nil
}

// OutboundSnapshot is what the health signals read: how many commitments are in
// each state, and how late the queue is.
//
// The lateness is the maximum across providers and zero when nothing is due.
// Both halves matter: a gauge that is left untouched when the backlog clears
// keeps alerting about a queue that no longer exists, and one that reports the
// last provider it happened to see hides the one that is stuck behind a healthy
// one.
func (s *Store) OutboundSnapshot(ctx context.Context, family string) ([]outbound.StatusCount, float64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT status, count(*) FROM outbound_intents
		WHERE delivery_family = $1 GROUP BY status ORDER BY status`, family)
	if err != nil {
		return nil, 0, fmt.Errorf("count the commitments: %w", err)
	}
	defer rows.Close()

	var counts []outbound.StatusCount
	for rows.Next() {
		var c outbound.StatusCount
		if err := rows.Scan(&c.Status, &c.Count); err != nil {
			return nil, 0, err
		}
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	due, err := s.DueSnapshot(ctx, family)
	if err != nil {
		return nil, 0, err
	}
	lateness := 0.0
	for _, d := range due {
		if d.LatenessSeconds > lateness {
			lateness = d.LatenessSeconds
		}
	}
	return counts, lateness, nil
}
