package store

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What the two outbound gauges are allowed to say.
//
// The counters elsewhere answer "did anything end badly". These answer "is
// anything owed that should already have gone out", and the whole difficulty is
// in the second half of that sentence: an escalation step scheduled for ten
// minutes' time and a retry waiting out its backoff are the system doing what it
// was told. Counted as a backlog they would wake somebody for a working system,
// which is how the previous edition of this metric was wrong.

func latenessOf(t *testing.T, s *Store, family string) (float64, bool) {
	t.Helper()
	snap, err := s.GetMetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	for _, row := range snap.OutboundLatenessSeconds {
		if row.Family == family {
			return row.Seconds, true
		}
	}
	return 0, false
}

func intentsByStatus(t *testing.T, s *Store, family, status string) int {
	t.Helper()
	snap, err := s.GetMetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	for _, row := range snap.OutboundIntentsByStatus {
		if row.Family == family && row.Status == status {
			return row.Count
		}
	}
	return 0
}

// dueSince backdates a commitment so it has been due for a known time, which is
// the only way to assert on a number the database computes.
func dueSince(t *testing.T, s *Store, intentID string, ago time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`
		UPDATE outbound_intents SET not_before = now() - $2::interval,
		       next_attempt_at = now() - $2::interval
		WHERE id = $1`, intentID, ago.String()); err != nil {
		t.Fatalf("backdate %s: %v", intentID, err)
	}
}

func TestWhatCountsAsLateness(t *testing.T) {
	s := setupTestDB(t)

	t.Run("nothing owed is zero, not absent", func(t *testing.T) {
		late, reported := latenessOf(t, s, outbound.FamilyNotification)
		if !reported {
			t.Fatal("the paging family is missing from the snapshot, so a dashboard " +
				"has no series until the first alert")
		}
		if late != 0 {
			t.Fatalf("an empty queue is %v seconds late", late)
		}
	})

	t.Run("work due now is late by how long it has been due", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		dueSince(t, s, intentID, 120*time.Second)

		late, _ := latenessOf(t, s, outbound.FamilyNotification)
		if late < 110 || late > 130 {
			t.Fatalf("a commitment due two minutes ago reports %v seconds", late)
		}
	})

	t.Run("work scheduled for later is not late", func(t *testing.T) {
		s := setupTestDB(t)
		agID := outboundGroup(t, s)
		// A policy step with a delay: admitted now, due in five minutes.
		admitOne(t, s, agID, channelCommitment("C0009", 5*time.Minute))

		late, _ := latenessOf(t, s, outbound.FamilyNotification)
		if late != 0 {
			t.Fatalf("a step the policy scheduled for later reports %v seconds of "+
				"lateness, which would page somebody for a working escalation", late)
		}
	})

	t.Run("work somebody is already holding is still late", func(t *testing.T) {
		s := setupTestDB(t)
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		dueSince(t, s, intentID, 90*time.Second)
		claimOne(t, s, intentID)

		late, _ := latenessOf(t, s, outbound.FamilyNotification)
		if late < 80 {
			t.Fatalf("a commitment claimed by a worker and never begun reports %v "+
				"seconds; a worker that hung is exactly what this must not hide", late)
		}
	})

	t.Run("the family reports its worst provider, not its last one", func(t *testing.T) {
		s := setupTestDB(t)
		agID := outboundGroup(t, s)
		owed := admitOne(t, s, agID,
			channelCommitment("C0001", 0), telegramCommitment("T0001"))
		// Slack two minutes behind, Telegram five seconds. One label covers
		// both, and a fold that took the last row it saw would hide the one
		// that matters behind the one that is fine.
		dueSince(t, s, owed[0], 120*time.Second)
		dueSince(t, s, owed[1], 5*time.Second)

		late, _ := latenessOf(t, s, outbound.FamilyNotification)
		if late < 110 || late > 130 {
			t.Fatalf("the family reports %v seconds while one of its providers is "+
				"two minutes behind", late)
		}
	})

	t.Run("a commitment waiting out its backoff is not late", func(t *testing.T) {
		s := setupTestDB(t)
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		// A failed attempt puts the next one in the future. That is the retry
		// policy working, not a queue falling behind.
		if _, err := s.db.Exec(`
			UPDATE outbound_intents
			SET next_attempt_at = now() + interval '30 seconds', failure_streak = 1
			WHERE id = $1`, intentID); err != nil {
			t.Fatalf("put the commitment on backoff: %v", err)
		}

		if late, _ := latenessOf(t, s, outbound.FamilyNotification); late != 0 {
			t.Fatalf("a commitment on backoff reports %v seconds of lateness", late)
		}
	})

	t.Run("a backlog that was worked off stops ringing", func(t *testing.T) {
		s := setupTestDB(t)
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		dueSince(t, s, intentID, 120*time.Second)

		if late, _ := latenessOf(t, s, outbound.FamilyNotification); late == 0 {
			t.Fatal("the fixture is not late to begin with")
		}
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET status = 'succeeded' WHERE id = $1`,
			intentID); err != nil {
			t.Fatalf("finish the commitment: %v", err)
		}

		late, reported := latenessOf(t, s, outbound.FamilyNotification)
		if !reported || late != 0 {
			t.Fatalf("after the backlog was worked off the gauge says %v (reported=%v)",
				late, reported)
		}
	})
}

// TestTheSnapshotCountsCommitmentsByStatus is the other half: what is owed, and
// what has been finished with.
func TestTheSnapshotCountsCommitmentsByStatus(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	owed := admitOne(t, s, agID,
		channelCommitment("C0001", 0), channelCommitment("C0002", time.Minute))

	if got := intentsByStatus(t, s, outbound.FamilyNotification, string(outbound.StatusPending)); got != 2 {
		t.Fatalf("the snapshot says %d commitments are owed, want 2", got)
	}

	if _, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	if got := intentsByStatus(t, s, outbound.FamilyNotification, string(outbound.StatusCanceled)); got != len(owed) {
		t.Fatalf("the snapshot says %d commitments were withdrawn, want %d", got, len(owed))
	}
	if got := intentsByStatus(t, s, outbound.FamilyNotification, string(outbound.StatusPending)); got != 0 {
		t.Fatalf("%d commitments are still owed after the alert was acknowledged", got)
	}
}

// TestTheAdmissionLatencyIsMeasuredOnlyWhereItMeansSomething.
//
// The histogram is the promise: how long a page took to become a call. A step
// the policy scheduled for ten minutes' time waited exactly as long as it was
// told to, and a retry has a backoff behind it - measured, either would report
// the plan as delay and make a healthy escalation look slow.
func TestTheAdmissionLatencyIsMeasuredOnlyWhereItMeansSomething(t *testing.T) {
	s := setupTestDB(t)

	t.Run("the first call of a commitment due at once", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)

		begun, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if begun.FirstAttemptLatency == nil {
			t.Fatal("nothing was measured for the first call of an immediately due commitment")
		}
		if *begun.FirstAttemptLatency < 0 || *begun.FirstAttemptLatency > 60 {
			t.Fatalf("the measurement is %v seconds", *begun.FirstAttemptLatency)
		}
	})

	t.Run("a step the policy scheduled for later", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, channelCommitment("C0009", 5*time.Minute))[0]
		// Due now as far as the claim is concerned, but it was admitted with a
		// delay - which is what the measurement has to notice.
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`,
			intentID); err != nil {
			t.Fatalf("bring the step forward: %v", err)
		}
		token := claimOne(t, s, intentID)

		begun, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if begun.FirstAttemptLatency != nil {
			t.Fatalf("a delayed step reported %v seconds of admission latency",
				*begun.FirstAttemptLatency)
		}
	})

	t.Run("a retry", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]

		token := claimOne(t, s, intentID)
		first, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: first.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}

		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`,
			intentID); err != nil {
			t.Fatalf("bring the retry forward: %v", err)
		}
		retryToken := claimOne(t, s, intentID)
		again, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: retryToken, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
		})
		if err != nil {
			t.Fatalf("begin the retry: %v", err)
		}
		if again.FirstAttemptLatency != nil {
			t.Fatalf("a retry reported %v seconds of admission latency",
				*again.FirstAttemptLatency)
		}
	})
}

// telegramCommitment is the second provider, so a fold over providers has
// something to fold.
func telegramCommitment(ref string) keys.EscalationCommitment {
	c := channelCommitment(ref, 0)
	c.Provider = "telegram"
	c.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: 1}
	return c
}

// TestEveryFamilyReportsLatenessFromTheFirstScrape: the series exists at zero
// before the first commitment of the family, for every family this build
// executes. A graph that only starts when the thing it watches first happens is
// a graph nobody can alert on until then.
func TestEveryFamilyReportsLatenessFromTheFirstScrape(t *testing.T) {
	s := setupTestDB(t)
	for _, family := range []string{outbound.FamilyNotification, outbound.FamilyHandoff, outbound.FamilyWebhook} {
		seconds, found := latenessOf(t, s, family)
		if !found {
			t.Errorf("%s reports no lateness before its first commitment", family)
		} else if seconds != 0 {
			t.Errorf("%s reports %v seconds of lateness on an empty queue", family, seconds)
		}
	}
	if _, found := latenessOf(t, s, "carrier_pigeon"); found {
		t.Error("a family nobody executes reports lateness")
	}
}
