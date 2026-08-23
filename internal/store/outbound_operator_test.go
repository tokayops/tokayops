package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// An operator's decision is the only way out of a state the system will not
// leave on its own, so every one of them is an explicit act with an audit trail
// - and every one of them can be raced by an acknowledgement, by another
// operator, or by the alert simply being over.

// stuckInReview drives a commitment to the state that waits for a person: a
// call whose fate is unknown, under a policy that refuses to guess.
func stuckInReview(t *testing.T, s *Store, agID string) string {
	t.Helper()

	commitment := dmCommitment("U0001")
	commitment.AmbiguityPolicy = keys.PolicyManualReview
	intentID := admitOne(t, s, agID, commitment)[0]

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)

	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Completion: keys.Completion{Outcome: keys.OutcomeAmbiguous},
	}); err != nil {
		t.Fatalf("finalize as ambiguous: %v", err)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusManualReview {
		t.Fatalf("an unresolved doubt left the commitment %s", got)
	}
	return intentID
}

func resolve(t *testing.T, s *Store, req outbound.ResolveAmbiguityRequest) outbound.ResolveAmbiguityResult {
	t.Helper()
	if req.Actor == "" {
		req.Actor = "nina"
	}
	result, err := s.ResolveAmbiguity(context.Background(), req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return result
}

// TestOperatorDecisionsThatApply walks the decisions a person can make about a
// commitment nobody could resolve automatically.
func TestOperatorDecisionsThatApply(t *testing.T) {
	s := setupTestDB(t)

	t.Run("accept the risk that it arrived", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionAssumeAccepted,
			Reason: "the recipient confirmed out of band",
		})
		if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusSucceeded {
			t.Fatalf("assuming delivery answered %q into %s", result.Outcome, result.Status)
		}

		// The risk is on the record, and the history says what kind of success
		// this was: assumed, not observed.
		var risk bool
		if err := s.db.QueryRow(
			`SELECT accepted_duplicate_risk FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&risk); err != nil {
			t.Fatalf("read the commitment: %v", err)
		}
		if !risk {
			t.Fatal("an assumed delivery recorded no risk")
		}

		var message string
		if err := s.db.QueryRow(`
			SELECT message FROM timeline_events
			WHERE alert_group_id = $1 AND type = $2 ORDER BY created_at DESC LIMIT 1`,
			agID, model.TimelineEventNotificationSent).Scan(&message); err != nil {
			t.Fatalf("read the history: %v", err)
		}
		if message == "Notification sent" {
			t.Fatal("an assumed delivery was written into the history as an observed one")
		}
	})

	t.Run("decide not to deliver it", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionCancel,
			Reason: "the incident was handled another way",
		})
		if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusCanceled {
			t.Fatalf("withdrawing answered %q into %s", result.Outcome, result.Status)
		}
	})

	t.Run("try the same effect again", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionRetryCurrentGeneration,
			Reason: "the provider is back",
		})
		if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusPending {
			t.Fatalf("retrying answered %q into %s", result.Outcome, result.Status)
		}

		// The same effect means the same external identity: nothing about the
		// generation moved.
		var generation int
		if err := s.db.QueryRow(
			`SELECT generation_no FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&generation); err != nil {
			t.Fatalf("read the generation: %v", err)
		}
		if generation != 0 {
			t.Fatalf("retrying the same effect moved to generation %d", generation)
		}
	})

	t.Run("start a new effect, duplicate accepted", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
			AcceptedDuplicateRisk: true, Reason: "send it again to the new address",
		})
		if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusPending {
			t.Fatalf("a new effect answered %q into %s", result.Outcome, result.Status)
		}

		// A new effect means a new external identity: the address and the key of
		// the previous one are released, and its receipt stays in the journal
		// rather than on the commitment.
		var generation int
		var endpoint, createKey, receipt []byte
		if err := s.db.QueryRow(`
			SELECT generation_no, bound_endpoint, create_key, receipt
			FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&generation, &endpoint, &createKey, &receipt); err != nil {
			t.Fatalf("read the commitment: %v", err)
		}
		if generation != 1 || endpoint != nil || createKey != nil || receipt != nil {
			t.Fatalf("the previous effect was not released: generation %d, endpoint %v",
				generation, endpoint)
		}

		var events int
		if err := s.db.QueryRow(`
			SELECT count(*) FROM outbound_intent_events
			WHERE intent_id = $1 AND kind IN ('operator_decision', 'duplicate_risk_accepted',
			                                  'generation_started')`, intentID).Scan(&events); err != nil {
			t.Fatalf("count the audit: %v", err)
		}
		if events < 3 {
			t.Fatalf("the decision left %d audit records", events)
		}
	})
}

// TestOperatorDecisionsThatAreRefused pins the guards. Each of these would
// either claim something known to be false or create an external effect nobody
// can account for.
func TestOperatorDecisionsThatAreRefused(t *testing.T) {
	s := setupTestDB(t)

	t.Run("a new effect while the old one may exist", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)

		// The last attempt ended in doubt, and nobody has accepted the
		// duplicate: a second message may land on top of a first one that did.
		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
		})
		if result.Outcome != outbound.ResolveInvalidDecision {
			t.Fatalf("an unaccounted duplicate answered %q", result.Outcome)
		}
		if statusOf(t, s, intentID) != outbound.StatusManualReview {
			t.Fatal("a refused decision moved the commitment anyway")
		}
	})

	t.Run("assuming delivery of something that expired unsent", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`,
			intentID); err != nil {
			t.Fatalf("set a deadline in the past: %v", err)
		}
		if _, err := s.ExpireDueIntents(context.Background(), testFamily, 10); err != nil {
			t.Fatalf("expire: %v", err)
		}

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionAssumeAccepted,
		})
		if result.Outcome != outbound.ResolveInvalidDecision {
			t.Fatalf("assuming delivery of an expired commitment answered %q", result.Outcome)
		}
	})

	t.Run("reviving an expired commitment with no new deadline", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`,
			intentID); err != nil {
			t.Fatalf("set a deadline in the past: %v", err)
		}
		if _, err := s.ExpireDueIntents(context.Background(), testFamily, 10); err != nil {
			t.Fatalf("expire: %v", err)
		}

		if result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionRetryCurrentGeneration,
		}); result.Outcome != outbound.ResolveInvalidDecision {
			t.Fatalf("reviving without a deadline answered %q", result.Outcome)
		}

		// With one, it goes back into the queue and carries the new deadline.
		deadline := time.Now().Add(time.Hour)
		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionRetryCurrentGeneration,
			NewExpiresAt: &deadline,
		})
		if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusPending {
			t.Fatalf("reviving with a deadline answered %q into %s", result.Outcome, result.Status)
		}

		var expiresAt time.Time
		if err := s.db.QueryRow(
			`SELECT expires_at FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&expiresAt); err != nil {
			t.Fatalf("read the deadline: %v", err)
		}
		if !expiresAt.After(time.Now()) {
			t.Fatalf("the revived commitment kept a deadline in the past: %s", expiresAt)
		}
	})

	t.Run("a commitment still being worked on", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionCancel,
		})
		if result.Outcome != outbound.ResolveAlreadyResolved {
			t.Fatalf("deciding about a live commitment answered %q", result.Outcome)
		}
		if result.Status != outbound.StatusPending {
			t.Fatalf("the answer reported the state as %s", result.Status)
		}
	})

	t.Run("a commitment nobody has", func(t *testing.T) {
		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: "00000000-0000-0000-0000-000000000000",
			Decision: outbound.DecisionCancel,
		})
		if result.Outcome != outbound.ResolveNotFound {
			t.Fatalf("deciding about nothing answered %q", result.Outcome)
		}
	})
}

// TestOperatorAgainstAnAlertThatIsOver is the guard acknowledgement cannot
// provide by itself: it does not touch terminal commitments, so without this an
// operator could page somebody about an incident that was closed hours ago.
func TestOperatorAgainstAnAlertThatIsOver(t *testing.T) {
	s := setupTestDB(t)

	t.Run("a page for a resolved alert is refused", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)
		forceAlertGroupStatus(t, s, agID, model.AlertGroupStatusResolved)

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionRetryCurrentGeneration,
		})
		if result.Outcome != outbound.ResolveBusinessClosed {
			t.Fatalf("paging for a resolved alert answered %q", result.Outcome)
		}
	})

	t.Run("withdrawing one is still allowed", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := stuckInReview(t, s, agID)
		forceAlertGroupStatus(t, s, agID, model.AlertGroupStatusResolved)

		result := resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionCancel,
		})
		if result.Outcome != outbound.ResolveResolved {
			t.Fatalf("closing the question on a resolved alert answered %q", result.Outcome)
		}
	})
}

// TestTwoOperatorsAtOnce: the first decision stands, and the second is told
// what happened rather than being applied on top of it.
func TestTwoOperatorsAtOnce(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := stuckInReview(t, s, agID)

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]outbound.ResolveAmbiguityResult, 2)
	errs := make([]error, 2)
	decisions := []outbound.Decision{
		outbound.DecisionCancel, outbound.DecisionRetryCurrentGeneration,
	}

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.ResolveAmbiguity(context.Background(),
				outbound.ResolveAmbiguityRequest{
					IntentID: intentID, Decision: decisions[i], Actor: "operator",
				})
		}(i)
	}
	close(start)
	wg.Wait()

	resolved, already := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("operator %d: %v", i, err)
		}
		switch results[i].Outcome {
		case outbound.ResolveResolved:
			resolved++
		case outbound.ResolveAlreadyResolved:
			already++
		default:
			t.Fatalf("operator %d got %q", i, results[i].Outcome)
		}
	}
	if resolved != 1 || already != 1 {
		t.Fatalf("two decisions produced %d applied and %d refused", resolved, already)
	}
}

// TestOutboundSnapshotReadsTheQueue is what the health signals see: how many
// commitments are in each state, and a lateness that goes back to zero when the
// backlog clears rather than staying wherever it last was.
func TestOutboundSnapshotReadsTheQueue(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	ids := admitOne(t, s, agID, channelCommitment("C0001", 0), channelCommitment("C0002", 0))

	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET next_attempt_at = now() - interval '3 minutes' WHERE id = $1`,
		ids[0]); err != nil {
		t.Fatalf("age a commitment: %v", err)
	}

	counts, lateness, err := s.OutboundSnapshot(context.Background(), testFamily)
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if len(counts) != 1 || counts[0].Status != outbound.StatusPending || counts[0].Count != 2 {
		t.Fatalf("the queue reports %+v", counts)
	}
	if lateness < 150 {
		t.Fatalf("lateness is %.0fs, want about 180", lateness)
	}

	// Nothing due any more: the signal has to say so, not keep its last value.
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET next_attempt_at = now() + interval '1 hour'`); err != nil {
		t.Fatalf("clear the backlog: %v", err)
	}
	_, lateness, err = s.OutboundSnapshot(context.Background(), testFamily)
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if lateness != 0 {
		t.Fatalf("with nothing due, lateness is %.0fs", lateness)
	}
}
