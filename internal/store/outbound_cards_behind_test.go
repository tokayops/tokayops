package store

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/outbound"
)

// What "the card is out of date" is worth telling somebody, and what it is not.

// TestACardBehindItsAlertIsCountedByWhoCanFixIt.
//
// Three messages, all showing something older than their alert, and the number
// alone would say the same thing about all three. It is not the same thing: one
// is a queue that will drain, one needs a person, and one is a person having
// already decided. An alert written on the total would fire on the third.
func TestACardBehindItsAlertIsCountedByWhoCanFixIt(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	// Queued: aimed at a revision, waiting for a worker.
	queuedAG := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, queuedAG)
	aim(t, s, queuedAG)

	// Stuck: the change failed for a reason no retry mends.
	stuckAG := desiredGroup(t, s, "Disk slow")
	stuckID := changeableCard(t, s, stuckAG)
	aim(t, s, stuckAG)
	token := claimOne(t, s, stuckID)
	begun := beginOne(t, s, stuckID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "invalid_auth"),
	}); err != nil {
		t.Fatalf("finalize the change: %v", err)
	}

	// Abandoned: still behind, and somebody decided to leave it that way.
	goneAG := desiredGroup(t, s, "Disk noisy")
	goneID := changeableCard(t, s, goneAG)
	aim(t, s, goneAG)
	goneToken := claimOne(t, s, goneID)
	goneAttempt := beginOne(t, s, goneID, goneToken)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: goneAttempt.AttemptID, LeaseToken: goneToken,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "cant_update_message"),
	}); err != nil {
		t.Fatalf("finalize the change: %v", err)
	}
	result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: goneID, Decision: outbound.DecisionCancel,
		Actor: "nina", Reason: "the channel is being retired",
	})
	if err != nil || result.Outcome != outbound.ResolveResolved {
		t.Fatalf("withdraw the card: %s (%v)", result.Outcome, err)
	}

	// And the abandoned one is by far the oldest, which is the whole point of
	// leaving it out of the age.
	if _, err := s.db.Exec(`
		UPDATE outbound_intent_events SET created_at = now() - interval '6 hours'
		WHERE intent_id = $1 AND kind = 'desired_raised'`, goneID); err != nil {
		t.Fatalf("age the withdrawn card: %v", err)
	}

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	behind := map[string]int{}
	for _, b := range snap.OutboundCardsBehind {
		behind[b.State] = b.Count
	}
	for state, want := range map[string]int{"queued": 1, "stuck": 1, "abandoned": 1} {
		if behind[state] != want {
			t.Errorf("%s = %d, want %d (all states: %v)", state, behind[state], want, behind)
		}
	}

	// Six hours belong to the card nobody is going to catch up. What is
	// reported is the age of the ones that are still owed.
	if snap.OutboundCardStalenessSeconds <= 0 {
		t.Error("two messages are behind and the age is zero")
	}
	if snap.OutboundCardStalenessSeconds > 3600 {
		t.Errorf("the age is %.0fs, so the withdrawn card is being counted",
			snap.OutboundCardStalenessSeconds)
	}
}

// TestEveryStateReportsEvenWithNothingBehind. A gauge nobody writes keeps its
// last value, so a backlog that has been caught up would go on ringing.
func TestEveryStateReportsEvenWithNothingBehind(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	seen := map[string]bool{}
	for _, b := range snap.OutboundCardsBehind {
		seen[b.State] = true
	}
	for _, state := range []string{"queued", "stuck", "abandoned"} {
		if !seen[state] {
			t.Errorf("%q never reports, so its last value stands forever", state)
		}
	}
}

// TestASettledCommitmentBehindItsAlertIsSaidOutLoud.
//
// idle means applied equals desired, and an editable commitment that succeeded
// applied the final revision, after which there are no more. A row in either
// state with a revision outstanding is this build contradicting itself.
//
// It is not swept into one of the three answers. The gauge would then be quietly
// wrong about a card somebody is looking at, and nothing would say why.
func TestASettledCommitmentBehindItsAlertIsSaidOutLoud(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET status = $2 WHERE id = $1`,
		intentID, string(outbound.StatusIdle)); err != nil {
		t.Fatalf("settle the commitment: %v", err)
	}

	before := testutil.ToFloat64(
		metrics.StorageContractFailuresTotal.WithLabelValues("desired_revision"))

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	after := testutil.ToFloat64(
		metrics.StorageContractFailuresTotal.WithLabelValues("desired_revision"))
	if after <= before {
		t.Error("a settled commitment behind its alert was scraped and said nothing")
	}
	for _, b := range snap.OutboundCardsBehind {
		if b.Count != 0 {
			t.Errorf("it was counted as %q instead", b.State)
		}
	}
}
