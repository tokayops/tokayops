package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// An instance dying with an announcement in flight.
//
// The call went to the provider and nothing came back to be written down, so
// the row says "sending" with a lease nobody holds. What that means for a
// handover is not what it means for a page, and the difference is the deadline:
// an announcement is bounded, so the same crash is a retry while the shift is
// still ahead and an expiry once it is not.

// abandonedAnnouncement leaves behind exactly what a crash leaves behind: an
// open attempt, no completion, and a lease that has run out.
func abandonedAnnouncement(t *testing.T, s *Store, schedule string) (intentID, attemptID string) {
	t.Helper()
	seedUsers(t, s, "u-alice")
	result := mustSubmit(t, s, handoffAnnouncedFor(t, schedule,
		time.Now().Add(time.Hour), announceTo("slack", "u-alice")))
	if result.Outcome != outbound.SubmitCreated {
		t.Fatalf("the announcement answered %q", result.Outcome)
	}
	intentID = result.IntentIDs[0]

	token := claimHandoff(t, s, intentID)
	begun, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "U0001",
	})
	if err != nil {
		t.Fatalf("begin an attempt: %v", err)
	}
	if begun.Outcome != outbound.BeginStarted {
		t.Fatalf("opening an attempt on the announcement answered %q", begun.Outcome)
	}
	expireLease(t, s, intentID)
	return intentID, begun.AttemptID
}

func attemptOutcome(t *testing.T, s *Store, attemptID string) (outcome, reason string) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT COALESCE(outcome, ''), COALESCE(finish_reason, '')
		 FROM outbound_attempts WHERE id = $1`, attemptID).Scan(&outcome, &reason); err != nil {
		t.Fatalf("read the attempt: %v", err)
	}
	return outcome, reason
}

// TestAnAnnouncementSurvivesTheInstanceThatWasSendingIt. The shift is still
// ahead, so the announcement is still worth making: the doubt is written down
// and the commitment goes back to the queue.
//
// Announced twice is the accepted cost of that, and it is the handover's own
// answer rather than a default - its ambiguity policy is retry. A person who
// reads the same shift notice twice is mildly annoyed; a person who is on call
// and was never told is the failure this domain exists to prevent.
func TestAnAnnouncementSurvivesTheInstanceThatWasSendingIt(t *testing.T) {
	s := setupTestDB(t)
	intentID, attemptID := abandonedAnnouncement(t, s, "sched-crash")

	recovered, err := s.RecoverStaleAttempts(context.Background(),
		string(keys.FamilyHandoff), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].AttemptID != attemptID {
		t.Fatalf("recovery took %d attempts of the handover family", len(recovered))
	}

	// The call may have landed, and that is what is recorded - not a failure,
	// which would be a claim about an external effect nobody observed.
	if outcome, reason := attemptOutcome(t, s, attemptID); outcome !=
		string(outbound.OutcomeAmbiguous) || reason != "lease_lost" {
		t.Errorf("the abandoned announcement was closed as %q/%q", outcome, reason)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusPending {
		t.Fatalf("after the crash the announcement is %s, and nobody will make it", got)
	}

	// And what comes back is the same announcement, not a fresh one: the second
	// attempt is opened from the payload the first was admitted with.
	exec(t, s, `UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID)
	token := claimHandoff(t, s, intentID)
	second, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-2",
		Preparation: outbound.PreparationReady, BoundEndpoint: "U0001",
	})
	if err != nil {
		t.Fatalf("begin the second attempt: %v", err)
	}
	if !bytes.Equal(second.Content.Digest(), attemptFingerprint(t, s, attemptID)) {
		t.Errorf("the retry announces something else: %x, the first sent %x",
			second.Content.Digest(), attemptFingerprint(t, s, attemptID))
	}
}

// TestAnAnnouncementNobodyCanStillUseIsNotRetried.
//
// The other side of the same crash, and the reason a handover carries a
// deadline at all. The instance was away long enough for the shift to have
// changed; announcing it now would tell somebody to start a duty that is
// already under way, from a message that reads as if it were not.
//
// The attempt is still closed as ambiguous. That the deadline passed in the
// meantime says nothing about whether the provider delivered the first call,
// and a row that recorded the expiry over the doubt would be claiming knowledge
// of an external effect nobody has.
func TestAnAnnouncementNobodyCanStillUseIsNotRetried(t *testing.T) {
	s := setupTestDB(t)
	intentID, attemptID := abandonedAnnouncement(t, s, "sched-crash-late")

	// The process was away for longer than the announcement was good for.
	exec(t, s, `UPDATE outbound_intents SET expires_at = now() - interval '1 minute'
	            WHERE id = $1`, intentID)

	if _, err := s.RecoverStaleAttempts(context.Background(),
		string(keys.FamilyHandoff), 10); err != nil {
		t.Fatalf("recover: %v", err)
	}

	if outcome, reason := attemptOutcome(t, s, attemptID); outcome !=
		string(outbound.OutcomeAmbiguous) || reason != "lease_lost" {
		t.Errorf("a call whose fate is unknown was recorded as %q/%q", outcome, reason)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusExpired {
		t.Fatalf("the announcement is %s after its shift had already changed", got)
	}
}

// TestOneFamilysRecoveryLeavesTheOtherAlone. Recovery is a sweep per partition,
// the same as the claim is, and a sweep that ignored the family would be a
// paging instance quietly reopening announcements - the exact coupling the two
// families were split to remove.
func TestOneFamilysRecoveryLeavesTheOtherAlone(t *testing.T) {
	s := setupTestDB(t)
	intentID, attemptID := abandonedAnnouncement(t, s, "sched-crash-other")

	recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	for _, r := range recovered {
		if r.AttemptID == attemptID {
			t.Fatal("the paging family recovered an announcement")
		}
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusSending {
		t.Fatalf("the announcement is %s after a sweep of the other family", got)
	}
	if outcome, _ := attemptOutcome(t, s, attemptID); outcome != "" {
		t.Errorf("the attempt was closed as %q by the other family's sweep", outcome)
	}
}
