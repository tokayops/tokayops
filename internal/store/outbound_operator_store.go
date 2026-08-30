package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
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
//
// "Has a receipt" is receipt_recorded, not the coordinates. Erasure removes the
// coordinates of a message that WAS sent and leaves the fact behind it; asked
// the other way round, an acknowledgement would withdraw a delivery that had
// already happened and the history would say it never did.
// It returns how many commitments it WITHDREW outright, for the caller to count
// once its transaction has committed. Counted inside, a transaction that then
// rolled back would report an ending that never happened - and the alert on
// that counter fires on any increment at all.
func cancelIntentsTx(ctx context.Context, tx *sql.Tx, alertGroupID, reason, actor string) (int, error) {
	return cancelIntentsAtTx(ctx, tx, alertGroupID, reason, actor, time.Time{})
}

// cancelIntentsAtTx is the same withdrawal with the instant its history line
// takes.
//
// A caller that has already written history in this transaction has to hand one
// in. Those lines carry microsecond offsets from a single instant, and a line
// that took now() instead would sort BEFORE them - so the history would say the
// notifications were withdrawn before the alert that withdrew them cleared. The
// zero value means "whenever this lands", which is right for a transition that
// wrote nothing before it.
func cancelIntentsAtTx(ctx context.Context, tx *sql.Tx, alertGroupID, reason, actor string,
	at time.Time) (int, error) {
	// Nothing has gone out and nothing will: these are withdrawn outright, and
	// the lease goes with them so the worker holding one finds out at its next
	// compare-and-set.
	notSent, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', lease_token = NULL, locked_until = NULL,
		    worker_id = NULL, updated_at = now()
		WHERE alert_group_id = $1 AND NOT receipt_recorded AND status = 'pending'
		RETURNING id`, alertGroupID)
	if err != nil {
		return 0, err
	}

	// In flight. The outcome decides: a send that failed becomes a withdrawal,
	// one that succeeded becomes a message the group will update instead.
	inFlight, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET cancellation_requested = TRUE, updated_at = now()
		WHERE alert_group_id = $1 AND NOT receipt_recorded AND status = 'sending'
		RETURNING id`, alertGroupID)
	if err != nil {
		return 0, err
	}

	// Waiting for a person about a call that never produced anything: the
	// question is moot now, but the history keeps the doubt.
	waiting, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', updated_at = now()
		WHERE alert_group_id = $1 AND NOT receipt_recorded AND status = 'manual_review'
		RETURNING id`, alertGroupID)
	if err != nil {
		return 0, err
	}

	for _, id := range notSent {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason, actor); err != nil {
			return 0, err
		}
	}
	for _, id := range inFlight {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "cancellation_requested",
			reason, actor); err != nil {
			return 0, err
		}
	}
	for _, id := range waiting {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason+"; the outcome of the previous attempt stays unknown", actor); err != nil {
			return 0, err
		}
	}

	withdrawn := len(notSent) + len(waiting)
	touched := withdrawn + len(inFlight)
	if touched == 0 {
		return 0, nil
	}

	// One line in the alert's history, and a line each in the commitments' own:
	// the group's timeline says what happened to the alert, and the journal says
	// what happened to every promise it had made.
	line := fmt.Sprintf("%d pending notification(s) withdrawn: %s", touched, reason)
	if at.IsZero() {
		if err := addTimelineTx(ctx, tx, alertGroupID,
			model.TimelineEventNotificationFailed, line, actor); err != nil {
			return 0, err
		}
	} else if err := addTimelineEventsTx(ctx, tx, []*model.TimelineEvent{{
		ID:           uuid.New().String(),
		AlertGroupID: alertGroupID,
		Type:         model.TimelineEventNotificationFailed,
		Message:      line,
		Actor:        actor,
		CreatedAt:    at,
	}}); err != nil {
		return 0, err
	}
	return withdrawn, nil
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

	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
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

	// The person is gone, and reviving this would need an address. Cancelling
	// is still allowed: it ends a commitment rather than sending anything, and
	// leaving one stuck in manual_review forever is the state this refusal
	// exists to avoid, not to create.
	if intent.RecipientErased && req.Decision != outbound.DecisionCancel {
		return outbound.ResolveAmbiguityResult{
			Outcome: outbound.ResolveRecipientErased, Status: intent.Status,
		}, nil
	}

	if closed, err := decisionIsStale(*intent, req.Decision, groupStatus); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	} else if closed {
		return outbound.ResolveAmbiguityResult{
			Outcome: outbound.ResolveBusinessClosed, Status: intent.Status,
		}, nil
	}

	facts, err := lastAttemptFactsTx(ctx, tx, req.IntentID, intent.GenerationNo)
	if err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}

	// What an assumed delivery would be assuming: the revision the doubtful
	// attempt was applying, and whether that revision was the last one. Taken
	// from the attempt and the stored state, never from the operator - "assume
	// it arrived" is a statement about a message that was already composed, and
	// recording today's revision instead would claim a card shows something it
	// never showed.
	//
	// Only the decisions that need the state read it. A withdrawal claims
	// nothing and sends nothing, and state nobody can read must not be able to
	// trap a commitment whose one remaining option is to be called off.
	final := false
	if intent.Form == outbound.FormEditable && intent.GroupBound() &&
		decisionNeedsState(req.Decision, intent.Form) {
		stored, err := lockedSnapshotTx(ctx, tx, intent.AlertGroupID)
		if err != nil {
			return outbound.ResolveAmbiguityResult{}, err
		}
		final = stored.Final && facts.Revision != nil && stored.Revision == *facts.Revision

		// An editable card being brought back to life has to aim at the state
		// the group is in NOW, or it would reapply a revision from before the
		// alert moved on.
		if intent.Form == outbound.FormEditable &&
			req.Decision == outbound.DecisionRetryCurrentGeneration &&
			stored.Revision != intent.DesiredRevision {
			if _, err := tx.ExecContext(ctx,
				`UPDATE outbound_intents SET desired_revision = $2 WHERE id = $1`,
				req.IntentID, stored.Revision); err != nil {
				return outbound.ResolveAmbiguityResult{}, err
			}
			intent.DesiredRevision = stored.Revision
		}
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:                *intent,
		Trigger:               outbound.TriggerOperator,
		Decision:              req.Decision,
		AttemptRevision:       facts.Revision,
		AttemptIsFinal:        final,
		LastAttemptKind:       facts.Kind,
		AmbiguousInGeneration: facts.Ambiguous,
		AcceptedDuplicateRisk: req.AcceptedDuplicateRisk,
		ResourceLossConfirmed: facts.ResourceLost,
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
		AppliedRevision: facts.Revision,
		AttemptIsFinal:  final,
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

	if err := tx.Commit(); err != nil {
		return outbound.ResolveAmbiguityResult{}, err
	}
	// An operator ends commitments too - assume_accepted finishes one, cancel
	// withdraws one - and this door was missed when the counter lived in the
	// worker, which never sees an operator at all.
	countTerminal(intent.Family, transition.To)
	return outbound.ResolveAmbiguityResult{
		Outcome: outbound.ResolveResolved, Status: transition.To, Row: transition.Row,
	}, nil
}

// decisionNeedsState says whether a decision has to look at the state the
// commitment renders from.
func decisionNeedsState(decision outbound.Decision, form outbound.Form) bool {
	switch decision {
	case outbound.DecisionAssumeAccepted:
		// It claims a message arrived. Whether that was the LAST revision is a
		// fact of the state rather than of the person deciding - and only a
		// card has later revisions, so only a card has to ask. A one-shot
		// message renders what its admission froze and is done either way.
		return form == outbound.FormEditable
	case outbound.DecisionRetryCurrentGeneration:
		// A card that is going to be sent again has to be aimed at where the
		// alert is now. A one-shot has nothing to re-aim.
		return form == outbound.FormEditable
	default:
		// cancel sends nothing; retry_new_generation reads the state when the
		// attempt it leads to is opened, not now.
		return false
	}
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

// attemptFacts is what the operator's guards and an assumed delivery need to
// know about the journal: what the last attempt was doing, which revision it
// was applying, and whether anything in this generation ended in doubt.
type attemptFacts struct {
	Kind outbound.AttemptKind
	// Revision is what the last attempt applied, and nil when it applied none.
	Revision  *int64
	Ambiguous bool

	// ResourceLost is proof, from the journal, that the message this commitment
	// made is gone. It is what a decision to make a second one rests on, and it
	// is not something the person deciding may assert.
	ResourceLost bool
}

// lastAttemptFactsTx reads them, for the CURRENT generation only.
//
// Every one of these facts is about the effect being decided on now. An
// abandoned generation's create attempt would answer "yes, something exists"
// about an object this generation never made, and its revision would name a
// message this one never sent.
//
// The last attempt is chosen by its number rather than by its timestamp: two
// records written in one transaction share a clock reading, and "the last one"
// has to be an answer rather than a coin toss.
// resourceLostTx is the proof that the object this commitment made is gone.
//
// Read from the journal rather than taken from whoever is deciding. It is what
// allows a second external message to be created, and a person cannot be asked
// to remember whether a provider once said the first had disappeared - the
// answer existed for one moment, at one attempt, and this is where it was
// written down.
//
// Both places count. An attempt closed by its own worker holds its result; one
// closed by recovery as doubtful holds nothing, and the answer that arrived
// afterwards is in the observation beside it. Within a generation the fact does
// not expire either: nothing this build does can put the object back, so proof
// found anywhere in it stands until the generation ends.
//
// Only a CHANGE can have proved it. A create that claimed the message was gone
// would be licensing a second copy of something it had just made, and the pair
// is refused where it is written, in one place, for both tables. So the read
// asks what the attempt ended as and not what it was: re-deriving the rule here
// would defend a row that no writer in this build can produce.
func resourceLostTx(ctx context.Context, tx *sql.Tx, intentID string,
	generation int) (bool, error) {

	var lost bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM outbound_attempts a
			WHERE a.intent_id = $1 AND a.generation_no = $2
			  AND a.outcome = $3 AND a.provider_result_detail = $4
			UNION ALL
			SELECT 1 FROM outbound_attempt_observations o
			JOIN outbound_attempts a ON a.id = o.attempt_id
			WHERE a.intent_id = $1 AND a.generation_no = $2
			  AND o.outcome = $3 AND o.provider_result_detail = $4
		)`, intentID, generation, string(outbound.OutcomePermanentRejection),
		string(keys.DetailDefinitelyAbsent)).Scan(&lost)
	if err != nil {
		return false, fmt.Errorf("read what became of the message of %s: %w", intentID, err)
	}
	return lost, nil
}

func lastAttemptFactsTx(ctx context.Context, tx *sql.Tx, intentID string,
	generation int) (attemptFacts, error) {

	var (
		kind     sql.NullString
		revision sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT attempt_kind, applied_revision FROM outbound_attempts
		WHERE intent_id = $1 AND generation_no = $2
		ORDER BY attempt_no DESC LIMIT 1`, intentID, generation).
		Scan(&kind, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return attemptFacts{}, err
	}

	var ambiguous bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM outbound_attempts
			WHERE intent_id = $1 AND generation_no = $2 AND outcome = 'ambiguous')`,
		intentID, generation).Scan(&ambiguous); err != nil {
		return attemptFacts{}, err
	}

	lost, err := resourceLostTx(ctx, tx, intentID, generation)
	if err != nil {
		return attemptFacts{}, err
	}

	return attemptFacts{
		Kind:         outbound.AttemptKind(kind.String),
		Revision:     nullableRevision(revision),
		Ambiguous:    ambiguous,
		ResourceLost: lost,
	}, nil
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
