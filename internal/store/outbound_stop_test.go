package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// A step that does not continue on failure stops the steps after it. What
// the store does for it: withdraw what has not gone out from either door a
// failure comes through, under the group's lock, with one line in the alert's
// history. What it refuses to do: wait for the outcome, or touch a page that
// is already out.

func dmStep(ref string, index int, stop bool) keys.EscalationCommitment {
	c := dmCommitment(ref)
	c.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: index}
	c.StopOnFailure = stop
	return c
}

func cardStep(ref string, index int, stop bool) keys.EscalationCommitment {
	c := stepCard(ref, index)
	c.StopOnFailure = stop
	return c
}

// refusedBeforeTheNetwork is the first door: the channel could not prepare
// the call at all, and the store records the refusal.
func refusedBeforeTheNetwork(t *testing.T, s *Store, id string) outbound.BeginAttemptResult {
	t.Helper()
	result, err := s.BeginAttempt(context.Background(),
		outbound.Impossible("identity_not_linked", "no Slack account").Request(id, takeOne(t, s, id), "worker-1"))
	if err != nil {
		t.Fatalf("refuse %s before the network: %v", id, err)
	}
	if result.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("the refusal of %s was recorded as %s", id, result.Outcome)
	}
	return result
}

func stoppedLines(t *testing.T, s *Store, agID string) int {
	t.Helper()
	return countWhere(t, s, `SELECT count(*) FROM timeline_events WHERE alert_group_id = $1
		AND message LIKE 'Escalation stopped: step % failed and the policy does not continue'`, agID)
}

func stopped(agID string) []keys.EscalationCommitment {
	out := []keys.EscalationCommitment{
		channelCommitment("C-fire", 0),
		dmStep("u-1", 1, true), dmStep("u-1b", 1, false),
		dmStep("u-2", 2, false),
	}
	out = append(out, withSatellites(cardStep("C-2", 2, false))...)
	return append(out, dmStep("u-3", 3, false))
}

// TestAStepThatFailsForGoodStopsTheStepsAfterIt is the rule through the
// first door: a refusal before the network. The steps after the failed one
// are withdrawn - with the satellites of a card among them - and the steps
// before it, its own other recipients and the firehose are not.
func TestAStepThatFailsForGoodStopsTheStepsAfterIt(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, stopped(agID)...)
	u1 := intentAddressedTo(t, s, agID, "u-1")

	refusedBeforeTheNetwork(t, s, u1)
	if got := statusOf(t, s, u1); got != outbound.StatusPermanentFailed {
		t.Fatalf("the failed step is %s", got)
	}
	for _, ref := range []string{"u-2", "u-3"} {
		id := intentAddressedTo(t, s, agID, ref)
		if got := statusOf(t, s, id); got != outbound.StatusCanceled {
			t.Fatalf("the later step %s is %s", ref, got)
		}
		if lines := journalOf(t, s, id); !hasJournalLine(lines, "canceled|step 1 failed and the policy stops there|") {
			t.Fatalf("the journal of %s does not say why: %v", ref, lines)
		}
	}
	card, thread, reply := satellitesOf(t, s, agID)
	for _, id := range []string{card, thread, reply} {
		if got := statusOf(t, s, id); got != outbound.StatusCanceled {
			t.Fatalf("the later card or its satellite %s is %s", id, got)
		}
	}
	for _, ref := range []string{"C-fire", "u-1b"} {
		if got := statusOf(t, s, intentAddressedTo(t, s, agID, ref)); got != outbound.StatusPending {
			t.Fatalf("%s, which the failure does not stop, is %s", ref, got)
		}
	}
	if got := stoppedLines(t, s, agID); got != 1 {
		t.Fatalf("the alert's history says the escalation stopped %d time(s)", got)
	}
}

// TestAFailureOnTheNetworkStopsTheStepsAfterItToo is the second door: the
// provider refused for good.
func TestAFailureOnTheNetworkStopsTheStepsAfterItToo(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, cardStep("C-1", 1, true), dmStep("u-2", 2, false))
	refusedForGood(t, s, intentAddressedTo(t, s, agID, "C-1"))

	u2 := intentAddressedTo(t, s, agID, "u-2")
	if got := statusOf(t, s, u2); got != outbound.StatusCanceled {
		t.Fatalf("the later step is %s after the card failed on the network", got)
	}
	if lines := journalOf(t, s, u2); !hasJournalLine(lines, "canceled|step 1 failed and the policy stops there|") {
		t.Fatalf("the journal does not say why: %v", lines)
	}
	if got := stoppedLines(t, s, agID); got != 1 {
		t.Fatalf("the alert's history says the escalation stopped %d time(s)", got)
	}
}

// TestTheStopIsBestEffortAndSaysSo. Without the flag nothing is withdrawn.
// A later step already out stays out; one in flight is asked to stop and
// decides by its outcome; the flag on a later step reaches only what comes
// after it; and a person withdrawing a step is not a failure.
func TestTheStopIsBestEffortAndSaysSo(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	t.Run("no flag, no stop", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, dmStep("u-1", 1, false), dmStep("u-2", 2, false))
		refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-1"))
		if got := statusOf(t, s, intentAddressedTo(t, s, agID, "u-2")); got != outbound.StatusPending {
			t.Fatalf("a step after one that continues on failure is %s", got)
		}
		if stoppedLines(t, s, agID) != 0 {
			t.Fatal("the history says the escalation stopped")
		}
	})

	t.Run("a page already out stays out", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, dmStep("u-1", 1, true), dmStep("u-2", 2, false))
		postedAs(t, s, intentAddressedTo(t, s, agID, "u-2"), "D-2/1700000000.000100")
		refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-1"))
		if got := statusOf(t, s, intentAddressedTo(t, s, agID, "u-2")); got != outbound.StatusSucceeded {
			t.Fatalf("a page that was out is %s", got)
		}
		if stoppedLines(t, s, agID) != 0 {
			t.Fatal("the history says the escalation stopped when nothing was withdrawn")
		}
	})

	t.Run("a step in flight is asked to stop", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, dmStep("u-1", 1, true), dmStep("u-3", 3, false))
		u3 := intentAddressedTo(t, s, agID, "u-3")
		beginOne(t, s, u3, takeOne(t, s, u3))
		refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-1"))
		if got := statusOf(t, s, u3); got != outbound.StatusSending {
			t.Fatalf("the step in flight is %s", got)
		}
		if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND cancellation_requested`, u3) != 1 {
			t.Fatal("the step in flight was not asked to stop")
		}
	})

	t.Run("the flag reaches only what comes after", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, dmStep("u-1", 1, false), dmStep("u-2", 2, true), dmStep("u-3", 3, false))
		refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-2"))
		if got := statusOf(t, s, intentAddressedTo(t, s, agID, "u-1")); got != outbound.StatusPending {
			t.Fatalf("the step before the failed one is %s", got)
		}
		if got := statusOf(t, s, intentAddressedTo(t, s, agID, "u-3")); got != outbound.StatusCanceled {
			t.Fatalf("the step after the failed one is %s", got)
		}
	})

	t.Run("a person withdrawing a step is not a failure", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk filling up")
		doubtful := dmStep("u-1", 1, true)
		doubtful.AmbiguityPolicy = keys.PolicyManualReview
		admitOne(t, s, agID, doubtful, dmStep("u-2", 2, false))
		u1 := intentAddressedTo(t, s, agID, "u-1")
		token := takeOne(t, s, u1)
		begun := beginOne(t, s, u1, token)
		if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomeAmbiguous, "timeout"),
		}); err != nil {
			t.Fatal(err)
		}
		if got := statusOf(t, s, u1); got != outbound.StatusManualReview {
			t.Fatalf("the doubtful step is %s", got)
		}
		resolve(t, s, outbound.ResolveAmbiguityRequest{IntentID: u1, Decision: outbound.DecisionCancel, Reason: "not needed"})
		if got := statusOf(t, s, intentAddressedTo(t, s, agID, "u-2")); got != outbound.StatusPending {
			t.Fatalf("a person's withdrawal stopped the escalation: the later step is %s", got)
		}
	})
}

// TestTheOrdinaryBeginTakesNoGroupLock. The group is taken first only by a
// begin whose refusal may stop the escalation: every other attempt would
// otherwise wait behind an acknowledgement.
func TestTheOrdinaryBeginTakesNoGroupLock(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, dmStep("u-1", 1, true), dmStep("u-2", 2, false), dmStep("u-3", 3, true))
	locks := 0
	afterBeginGroupLock = func() { locks++ }
	defer func() { afterBeginGroupLock = nil }()

	u1 := intentAddressedTo(t, s, agID, "u-1")
	beginOne(t, s, u1, takeOne(t, s, u1)) // the ordinary begin of a flagged step
	if locks != 0 {
		t.Fatalf("the ordinary begin took the group %d time(s)", locks)
	}
	refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-2")) // no flag
	if locks != 0 {
		t.Fatalf("a refusal that stops nothing took the group %d time(s)", locks)
	}
	refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-3"))
	if locks != 1 {
		t.Fatalf("a refusal that may stop the escalation took the group %d time(s)", locks)
	}
}

// TestTheStopAndAnAcknowledgementConverge. A refusal that stops the
// escalation and an acknowledgement of the same alert both withdraw the
// steps that have not gone out, and both take the group first: whichever
// goes first, every late step ends canceled exactly once, with one line in
// its journal, and nothing deadlocks.
func TestTheStopAndAnAcknowledgementConverge(t *testing.T) {
	for _, first := range []string{"the stop", "the acknowledgement"} {
		t.Run(first+" first", func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")
			agID := desiredGroup(t, s, "Disk filling up")
			admitOne(t, s, agID, dmStep("u-1", 1, true), dmStep("u-2", 2, false), dmStep("u-3", 3, false))
			u1 := intentAddressedTo(t, s, agID, "u-1")
			token := takeOne(t, s, u1)

			acked := make(chan error, 1)
			ack := func() {
				_, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
				acked <- err
			}
			refusal := outbound.Impossible("identity_not_linked", "no Slack account").Request(u1, token, "worker-1")
			var begun outbound.BeginAttemptResult
			var beginErr error
			if first == "the stop" {
				// The acknowledgement arrives while the refusal holds the
				// group, and waits for it.
				afterBeginGroupLock = func() {
					go ack()
					time.Sleep(300 * time.Millisecond)
				}
				defer func() { afterBeginGroupLock = nil }()
				begun, beginErr = s.BeginAttempt(context.Background(), refusal)
			} else {
				ack()
				begun, beginErr = s.BeginAttempt(context.Background(), refusal)
			}
			if beginErr != nil {
				t.Fatalf("the begin: %v", beginErr)
			}
			select {
			case err := <-acked:
				if err != nil {
					t.Fatalf("the acknowledgement: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the acknowledgement never came back")
			}
			if begun.Outcome != outbound.BeginPreparedPermanent && begun.Outcome != outbound.BeginIntentFinalized {
				t.Fatalf("the begin came back %s", begun.Outcome)
			}
			for _, ref := range []string{"u-2", "u-3"} {
				id := intentAddressedTo(t, s, agID, ref)
				if got := statusOf(t, s, id); got != outbound.StatusCanceled {
					t.Fatalf("%s is %s", ref, got)
				}
				withdrawals := 0
				for _, line := range journalOf(t, s, id) {
					if strings.HasPrefix(line, "canceled|") {
						withdrawals++
					}
				}
				if withdrawals != 1 {
					t.Fatalf("%s was withdrawn %d time(s): %v", ref, withdrawals, journalOf(t, s, id))
				}
			}
			if got := groupStatusOf(t, s, agID); got != model.AlertGroupStatusAcknowledged {
				t.Fatalf("the group is %s", got)
			}
		})
	}
}

// TestALaterCardThatWentOutKeepsItsSatellites is the named best-effort case
// through the parent rule: a later step's card that went out before the
// failure stays out, and its thread and reply stay with it - a card in Slack
// with its history withdrawn forever would be the worse outcome.
func TestALaterCardThatWentOutKeepsItsSatellites(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	commitments := []keys.EscalationCommitment{dmStep("u-1", 1, true)}
	admitOne(t, s, agID, append(commitments, withSatellites(cardStep("C-2", 2, false))...)...)
	card, thread, reply := satellitesOf(t, s, agID)
	postedAs(t, s, card, "C-2/1700000000.000100")

	refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-1"))
	if got := statusOf(t, s, card); got != outbound.StatusIdle {
		t.Fatalf("the later card that went out is %s", got)
	}
	for _, id := range []string{thread, reply} {
		if got := statusOf(t, s, id); got != outbound.StatusPending {
			t.Fatalf("a satellite of the card that went out is %s", got)
		}
	}
	if got := stoppedLines(t, s, agID); got != 0 {
		t.Fatalf("the history says the escalation stopped %d time(s) when nothing was withdrawn", got)
	}
}

// TestTheStopCountsPagesNotMirrors. A later card that had already ended
// without a message leaves its satellites waiting; the stop withdraws them,
// and the alert's history says nothing - no page was withdrawn.
func TestTheStopCountsPagesNotMirrors(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	commitments := []keys.EscalationCommitment{dmStep("u-1", 1, true)}
	admitOne(t, s, agID, append(commitments, withSatellites(cardStep("C-2", 2, false))...)...)
	card, thread, reply := satellitesOf(t, s, agID)
	refusedForGood(t, s, card)

	refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-1"))
	for _, id := range []string{thread, reply} {
		if got := statusOf(t, s, id); got != outbound.StatusCanceled {
			t.Fatalf("a satellite of the card that ended is %s", got)
		}
	}
	if got := stoppedLines(t, s, agID); got != 0 {
		t.Fatalf("the history says the escalation stopped %d time(s) over two mirrors", got)
	}
}

// TestTheStopIsWrittenAfterTheFailure. Both lines are written in one
// transaction, whose instant is one; read newest first, the stop has to come
// after the failure that caused it, through both doors.
func TestTheStopIsWrittenAfterTheFailure(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	order := func(t *testing.T, agID string) {
		t.Helper()
		var failedAt, stoppedAt time.Time
		if err := s.db.QueryRow(`SELECT created_at FROM timeline_events WHERE alert_group_id = $1
			AND message = 'Notification failed permanently'`, agID).Scan(&failedAt); err != nil {
			t.Fatalf("the failure's line: %v", err)
		}
		if err := s.db.QueryRow(`SELECT created_at FROM timeline_events WHERE alert_group_id = $1
			AND message LIKE 'Escalation stopped:%'`, agID).Scan(&stoppedAt); err != nil {
			t.Fatalf("the stop's line: %v", err)
		}
		if !stoppedAt.After(failedAt) {
			t.Fatalf("the stop is dated %v and the failure %v: read newest first, the order is a coin toss",
				stoppedAt, failedAt)
		}
	}

	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, dmStep("u-1", 1, true), dmStep("u-2", 2, false))
	refusedBeforeTheNetwork(t, s, intentAddressedTo(t, s, agID, "u-1"))
	order(t, agID)

	agID = desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, cardStep("C-1", 1, true), dmStep("u-2", 2, false))
	refusedForGood(t, s, intentAddressedTo(t, s, agID, "C-1"))
	order(t, agID)
}
