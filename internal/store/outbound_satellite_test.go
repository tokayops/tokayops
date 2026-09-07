package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The two messages that follow a Slack channel card: the thread, which waits
// for the card to have a message, and the reply, which waits for the alert to
// be over as well. What the store does for them is a claim predicate, a lock
// order and a revival; what it refuses to do is invent a message where the
// card made none.

// withSatellites is a Slack channel card with the thread and the reply that
// follow it, as the producer admits one.
func withSatellites(card keys.EscalationCommitment) []keys.EscalationCommitment {
	out := []keys.EscalationCommitment{card}
	for _, kind := range []keys.TargetKind{keys.TargetThread, keys.TargetThreadReply} {
		out = append(out, keys.EscalationCommitment{
			Slot: card.Slot, Provider: card.Provider,
			Target:          keys.Target{Kind: kind, Ref: card.Target.Ref},
			Editable:        kind == keys.TargetThread,
			Timing:          card.Timing,
			CompletionMode:  keys.CompletionOnAcceptance,
			AmbiguityPolicy: keys.PolicyRetry,
		})
	}
	return out
}

// satellitesOf finds the card and its two satellites in a group that holds
// one Slack channel card.
func satellitesOf(t *testing.T, s *Store, agID string) (card, thread, reply string) {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, target_kind FROM outbound_intents WHERE alert_group_id = $1`, agID)
	if err != nil {
		t.Fatalf("read the group's commitments: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		switch keys.TargetKind(kind) {
		case keys.TargetChannel:
			card = id
		case keys.TargetThread:
			thread = id
		case keys.TargetThreadReply:
			reply = id
		}
	}
	if card == "" || thread == "" || reply == "" {
		t.Fatalf("the group holds card %q, thread %q, reply %q", card, thread, reply)
	}
	return card, thread, reply
}

// cardWithSatellites admits a Slack channel card with its two satellites and
// posts the card, so the thread can be claimed.
func cardWithSatellites(t *testing.T, s *Store, agID string) (card, thread, reply string) {
	t.Helper()
	admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
	card, thread, reply = satellitesOf(t, s, agID)
	postOne(t, s, card, claimOne(t, s, card))
	return card, thread, reply
}

// claimable claims everything Slack has due and returns the lease of each
// commitment by id. The leases are kept, as a worker keeps them.
func claimable(t *testing.T, s *Store) map[string]string {
	t.Helper()
	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	leases := map[string]string{}
	for _, l := range leased {
		leases[l.Intent.ID] = l.LeaseToken
	}
	return leases
}

// postOne begins a leased commitment and accepts the attempt.
func postOne(t *testing.T, s *Store, id, token string) outbound.BeginAttemptResult {
	t.Helper()
	begun := beginOne(t, s, id, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
	}); err != nil {
		t.Fatalf("post %s: %v", id, err)
	}
	return begun
}

// refusedForItsCard begins a satellite with the refusal the worker states for
// a card that ended without a message.
func refusedForItsCard(t *testing.T, s *Store, id, token string) outbound.BeginAttemptResult {
	t.Helper()
	result, err := s.BeginAttempt(context.Background(),
		outbound.Impossible(outbound.ParentEndedWithoutMessage, "the card ended without a message").
			Request(id, token, "worker-1"))
	if err != nil {
		t.Fatalf("refuse %s for its card: %v", id, err)
	}
	return result
}

// endTheAlert resolves the group and raises the final revision.
func endTheAlert(t *testing.T, s *Store, agID string) int64 {
	t.Helper()
	moveGroup(t, s, agID, model.AlertGroupStatusResolved)
	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredResolve, Actor: byUser("nina"),
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if result.Outcome != outbound.DesiredApplied {
		t.Fatalf("the resolve came back %s", result.Outcome)
	}
	return result.Revision
}

func lastErrorClass(t *testing.T, s *Store, intentID string) string {
	t.Helper()
	var class string
	if err := s.db.QueryRow(`SELECT COALESCE(error_class, '') FROM outbound_attempts
		WHERE intent_id = $1 ORDER BY attempt_no DESC LIMIT 1`, intentID).Scan(&class); err != nil {
		t.Fatalf("read the last attempt of %s: %v", intentID, err)
	}
	return class
}

func historyLines(t *testing.T, s *Store, agID string) int {
	t.Helper()
	return countWhere(t, s, `SELECT count(*) FROM timeline_events WHERE alert_group_id = $1`, agID)
}

func hasJournalLine(lines []string, prefix string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// TestASatelliteWaitsForItsCard is the claim's predicate. The thread is not
// claimed until the card has a message; the reply not until the alert is over
// AND the card has applied its last revision - a resolution announced under a
// card still saying "acknowledged" made Slack clients keep the old card. While
// they wait they are not late either, because waiting is what they are for.
func TestASatelliteWaitsForItsCard(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
	card, thread, reply := satellitesOf(t, s, agID)

	// Nothing has gone out: the card is the only thing to claim.
	leases := claimable(t, s)
	if _, ok := leases[thread]; ok {
		t.Fatal("the thread was claimed before its card had a message")
	}
	if _, ok := leases[reply]; ok {
		t.Fatal("the reply was claimed before its card had a message")
	}
	token, ok := leases[card]
	if !ok {
		t.Fatal("the card was not claimed")
	}
	postOne(t, s, card, token)

	// The card has a message: the thread may go. The reply waits for the end.
	leases = claimable(t, s)
	if _, ok := leases[thread]; !ok {
		t.Fatal("the thread was not claimed once its card had a message")
	}
	if _, ok := leases[reply]; ok {
		t.Fatal("the reply was claimed before the alert was over")
	}
	postOne(t, s, thread, leases[thread])

	// Only the reply is left, an hour past its time by the clock, and the
	// queue is not late by an hour: a commitment that cannot go is not owed.
	if _, err := s.db.Exec(`UPDATE outbound_intents SET next_attempt_at = now() - interval '1 hour'
		WHERE id = $1`, reply); err != nil {
		t.Fatal(err)
	}
	if late, _ := latenessOf(t, s, testFamily); late > 60 {
		t.Fatalf("the queue is %.0fs late on a reply that is waiting for the end", late)
	}

	// The alert ends: the card and the thread are aimed at the last revision,
	// and the reply still waits - for the card to show the end first.
	endTheAlert(t, s, agID)
	leases = claimable(t, s)
	if _, ok := leases[reply]; ok {
		t.Fatal("the reply was claimed before the card applied the last revision")
	}
	if _, ok := leases[card]; !ok {
		t.Fatal("the card was not claimed for its last revision")
	}
	postOne(t, s, card, leases[card])
	if got := statusOf(t, s, card); got != outbound.StatusSucceeded {
		t.Fatalf("the card is %s after its last revision", got)
	}

	// The card says the end: the reply goes, late from when it was due.
	if late, _ := latenessOf(t, s, testFamily); late < 3600 {
		t.Fatalf("the queue is %.0fs late on a reply that has been due for an hour", late)
	}
	if _, ok := claimable(t, s)[reply]; !ok {
		t.Fatal("the reply was not claimed once the card had said the end")
	}
}

// TestASatelliteFollowsItsCardIntoFailure. A card that ended without a message
// leaves nothing to write under: the thread is claimed, the worker states the
// refusal, and the store records it - once the card is checked again under
// its lock. The reply does the same, but only when the alert is over, since
// until then a retry of the card may still give it something to close.
// Neither writes a line into the alert's history: the card's failure is the
// event, and it is already there.
func TestASatelliteFollowsItsCardIntoFailure(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
	card, thread, reply := satellitesOf(t, s, agID)
	refusedForGood(t, s, card)
	lines := historyLines(t, s, agID)

	leases := claimable(t, s)
	if _, ok := leases[thread]; !ok {
		t.Fatal("the thread of a card that ended was not claimed")
	}
	if _, ok := leases[reply]; ok {
		t.Fatal("the reply of an alert that is not over was claimed")
	}
	if begun := refusedForItsCard(t, s, thread, leases[thread]); begun.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("the refusal was recorded as %s", begun.Outcome)
	}
	if got := statusOf(t, s, thread); got != outbound.StatusPermanentFailed {
		t.Fatalf("the thread is %s", got)
	}
	if got := lastErrorClass(t, s, thread); got != outbound.ParentEndedWithoutMessage {
		t.Fatalf("the thread failed as %q", got)
	}
	if got := historyLines(t, s, agID); got != lines {
		t.Fatalf("the thread's failure wrote %d line(s) into the alert's history", got-lines)
	}

	endTheAlert(t, s, agID)
	leases = claimable(t, s)
	if _, ok := leases[reply]; !ok {
		t.Fatal("the reply was not claimed once the alert was over")
	}
	refusedForItsCard(t, s, reply, leases[reply])
	if got := statusOf(t, s, reply); got != outbound.StatusPermanentFailed {
		t.Fatalf("the reply is %s", got)
	}
	if got := historyLines(t, s, agID); got != lines {
		t.Fatalf("the alert's history grew by %d line(s) over the reply's failure", got-lines)
	}
}

// TestARetryOfTheCardRevivesItsThread. An operator retrying the card is
// giving it another chance to make a message; the thread that failed because
// it had none goes back to waiting for it, with a line saying why. The reply,
// which never failed, is not touched.
func TestARetryOfTheCardRevivesItsThread(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
	card, thread, reply := satellitesOf(t, s, agID)
	refusedForGood(t, s, card)
	refusedForItsCard(t, s, thread, claimable(t, s)[thread])

	result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: card, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "the channel is back",
	})
	if result.Outcome != outbound.ResolveResolved {
		t.Fatalf("the retry came back %s", result.Outcome)
	}
	if got := statusOf(t, s, thread); got != outbound.StatusPending {
		t.Fatalf("the thread is %s after the card was retried", got)
	}
	if lines := journalOf(t, s, thread); !hasJournalLine(lines, "revived|the card was retried|") {
		t.Fatalf("the thread's journal does not say it was revived: %v", lines)
	}
	if lines := journalOf(t, s, reply); hasJournalLine(lines, "revived|") {
		t.Fatalf("the reply, which never failed, was revived: %v", lines)
	}

	// And it waits for the card again, then goes out under it.
	leases := claimable(t, s)
	if _, ok := leases[thread]; ok {
		t.Fatal("the revived thread was claimed before the card had a message")
	}
	postOne(t, s, card, leases[card])
	leases = claimable(t, s)
	postOne(t, s, thread, leases[thread])
	if got := statusOf(t, s, thread); got != outbound.StatusIdle {
		t.Fatalf("the thread is %s after it went out", got)
	}
}

// TestTheWithdrawalFollowsTheCard. When the alert is acknowledged, a card
// that never made a message is withdrawn, and its satellites with it. A card
// with a message stays and is updated - and so do its satellites: the thread
// is edited like the card, and the reply still has an end to announce.
func TestTheWithdrawalFollowsTheCard(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	unsent := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, unsent, withSatellites(channelCommitment("C0001", 0))...)
	card, thread, reply := satellitesOf(t, s, unsent)
	acknowledge(t, s, unsent)
	for _, id := range []string{card, thread, reply} {
		if got := statusOf(t, s, id); got != outbound.StatusCanceled {
			t.Fatalf("%s is %s after the alert was acknowledged with nothing sent", id, got)
		}
	}

	sent := desiredGroup(t, s, "Disk filling up")
	card, thread, reply = cardWithSatellites(t, s, sent)
	beginOne(t, s, thread, claimable(t, s)[thread]) // the thread is in flight
	acknowledge(t, s, sent)
	if got := statusOf(t, s, card); got == outbound.StatusCanceled {
		t.Fatal("a card with a message was withdrawn")
	}
	if got := statusOf(t, s, thread); got != outbound.StatusSending {
		t.Fatalf("the thread in flight is %s after the acknowledgement", got)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND cancellation_requested`, thread) != 0 {
		t.Fatal("the thread in flight under a sent card was asked to stop")
	}
	if got := statusOf(t, s, reply); got != outbound.StatusPending {
		t.Fatalf("the reply is %s after the acknowledgement", got)
	}
}

// TestACardThatWinsTheRaceAgainstTheWithdrawalBringsItsSatellitesBack. The
// withdrawal finds the card in flight without a message and takes its thread
// and its reply, as it takes those of any card without one; the card's send
// then wins the race and the card lives (T16). A card with a message keeps
// its satellites, and whether it got its message a moment before the
// acknowledgement or a moment after must not decide whether the thread
// exists: the satellites come back in the card's own transaction, the thread
// aimed at the revision the acknowledgement raised.
func TestACardThatWinsTheRaceAgainstTheWithdrawalBringsItsSatellitesBack(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
	card, thread, reply := satellitesOf(t, s, agID)
	token := claimOne(t, s, card)
	begun := beginOne(t, s, card, token) // the card is in flight

	acknowledge(t, s, agID)
	for _, id := range []string{thread, reply} {
		if got := statusOf(t, s, id); got != outbound.StatusCanceled {
			t.Fatalf("%s is %s while its card is in flight without a message", id, got)
		}
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND cancellation_requested`, card) != 1 {
		t.Fatal("the card in flight was not asked to stop")
	}

	// The send wins: the card has a message - and a revision to apply, since
	// the acknowledgement raised the group - and its satellites are back.
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
	}); err != nil {
		t.Fatalf("the send that won: %v", err)
	}
	if got := statusOf(t, s, card); got != outbound.StatusPending {
		t.Fatalf("the card that went out is %s", got)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND receipt_recorded`, card) != 1 {
		t.Fatal("the card that went out has no message")
	}
	revision, _ := storedRevision(t, s, agID)
	for _, id := range []string{thread, reply} {
		if got := statusOf(t, s, id); got != outbound.StatusPending {
			t.Fatalf("%s is %s after its card went out", id, got)
		}
		if lines := journalOf(t, s, id); !hasJournalLine(lines, "revived|the card went out alongside the withdrawal|") {
			t.Fatalf("the journal of %s does not say why it is back: %v", id, lines)
		}
	}
	if _, aimed := cardAim(t, s, thread); aimed != revision {
		t.Fatalf("the thread is aimed at revision %d, and the acknowledgement raised the group to %d", aimed, revision)
	}

	// And the thread goes out under the card, as any thread does.
	leases := claimable(t, s)
	if _, ok := leases[thread]; !ok {
		t.Fatal("the thread that came back was not claimed")
	}
	postOne(t, s, thread, leases[thread])
	if got := statusOf(t, s, thread); got != outbound.StatusIdle {
		t.Fatalf("the thread is %s after it went out", got)
	}
}

// TestARetryOfTheCardAndARefusalOfItsThreadConverge. The refusal a worker
// states for a thread whose card ended, and an operator's retry of that card,
// can cross. Whichever goes first, the thread ends up waiting for the card:
// the refusal is recorded only after the card is checked again under its
// lock, and a retry revives a refusal already recorded.
func TestARetryOfTheCardAndARefusalOfItsThreadConverge(t *testing.T) {
	t.Run("the refusal first", func(t *testing.T) {
		s := setupTestDB(t)
		s.SetRenderEnvironment("https://tokay.example", "UTC")
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
		card, thread, _ := satellitesOf(t, s, agID)
		refusedForGood(t, s, card)
		refusedForItsCard(t, s, thread, claimable(t, s)[thread])
		resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: card, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again",
		})
		threadWaitsForTheCard(t, s, thread)
	})

	t.Run("the retry first", func(t *testing.T) {
		s := setupTestDB(t)
		s.SetRenderEnvironment("https://tokay.example", "UTC")
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
		card, thread, _ := satellitesOf(t, s, agID)
		refusedForGood(t, s, card)
		token := claimable(t, s)[thread]

		// The worker has decided the refusal and begins while the operator
		// holds the card: the begin re-checks the card under a share lock
		// and waits for the retry to commit.
		// A begin waits a bounded time for a lock and is retried on the
		// family's backoff when it runs out; the retry is what a worker does,
		// so the test does it too.
		begun := make(chan outbound.BeginAttemptResult, 1)
		failed := make(chan error, 1)
		afterOperatorLock = func() {
			go func() {
				for {
					result, err := s.BeginAttempt(context.Background(),
						outbound.Impossible(outbound.ParentEndedWithoutMessage, "the card ended").
							Request(thread, token, "worker-1"))
					if err != nil && strings.Contains(err.Error(), "lock timeout") {
						continue
					}
					if err != nil {
						failed <- err
						return
					}
					begun <- result
					return
				}
			}()
			time.Sleep(300 * time.Millisecond) // the begin is now waiting on the card
		}
		defer func() { afterOperatorLock = nil }()

		resolve(t, s, outbound.ResolveAmbiguityRequest{
			IntentID: card, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again",
		})
		select {
		case err := <-failed:
			t.Fatalf("the begin: %v", err)
		case result := <-begun:
			if result.Outcome != outbound.BeginPreparedRetry {
				t.Fatalf("the refusal was recorded as %s over a card that is pending again", result.Outcome)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the begin never came back")
		}
		threadWaitsForTheCard(t, s, thread)
	})

	t.Run("the retry crossing the refusal", func(t *testing.T) {
		s := setupTestDB(t)
		s.SetRenderEnvironment("https://tokay.example", "UTC")
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, withSatellites(channelCommitment("C0001", 0))...)
		card, thread, _ := satellitesOf(t, s, agID)
		refusedForGood(t, s, card)
		token := claimable(t, s)[thread]

		// The begin has checked the card and holds it shared; the operator's
		// retry arrives now and has to wait for the refusal to be recorded,
		// so that its revival sees it.
		// An operator's decision waits a bounded time for the lock and is
		// answered "try again" when it runs out; the operator does, and so
		// does the test.
		retried := make(chan outbound.ResolveAmbiguityResult, 1)
		failed := make(chan error, 1)
		afterParentCheck = func() {
			go func() {
				for {
					result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
						IntentID: card, Decision: outbound.DecisionRetryCurrentGeneration,
						Actor: byUser("nina"), Reason: "again",
					})
					if errors.Is(err, ErrCommitmentBusy) {
						continue
					}
					if err != nil {
						failed <- err
						return
					}
					retried <- result
					return
				}
			}()
			time.Sleep(300 * time.Millisecond) // the retry is now waiting on the card
		}
		defer func() { afterParentCheck = nil }()

		if begun := refusedForItsCard(t, s, thread, token); begun.Outcome != outbound.BeginPreparedPermanent {
			t.Fatalf("the refusal was recorded as %s over a card that had ended", begun.Outcome)
		}
		select {
		case err := <-failed:
			t.Fatalf("the retry: %v", err)
		case result := <-retried:
			if result.Outcome != outbound.ResolveResolved {
				t.Fatalf("the retry came back %s", result.Outcome)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the retry never came back")
		}
		if lines := journalOf(t, s, thread); !hasJournalLine(lines, "revived|") {
			t.Fatalf("the retry did not revive the refusal it crossed: %v", lines)
		}
		threadWaitsForTheCard(t, s, thread)
	})
}

// threadWaitsForTheCard is the invariant every serialisation reaches: the
// thread is pending, was never left failed, and holds no lease.
func threadWaitsForTheCard(t *testing.T, s *Store, thread string) {
	t.Helper()
	if got := statusOf(t, s, thread); got != outbound.StatusPending {
		t.Fatalf("the thread is %s", got)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND lease_token IS NOT NULL`, thread) != 0 {
		t.Fatal("the thread still holds a lease")
	}
	if _, ok := claimable(t, s)[thread]; ok {
		t.Fatal("the thread was claimed while its card is pending without a message")
	}
}

// TestTheReplyIsDrawnFromTheEnd. The reply is one message, said once, about
// how the alert ended: what it renders is the final revision, whatever the
// group was at when the reply was admitted.
func TestTheReplyIsDrawnFromTheEnd(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	card, _, reply := cardWithSatellites(t, s, agID)
	final := endTheAlert(t, s, agID)
	postOne(t, s, card, claimable(t, s)[card]) // the card shows the end first

	begun := beginOne(t, s, reply, claimable(t, s)[reply])
	if got := revisionOf(t, begun); got != final {
		t.Fatalf("the reply renders revision %d, and the alert ended at %d", got, final)
	}
}

// TestASatelliteLeavesNoLineInTheHistory. The alert's history says when the
// card went out; the messages under the card are not events of the alert,
// and a group is not moved by them either.
func TestASatelliteLeavesNoLineInTheHistory(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	_, thread, _ := cardWithSatellites(t, s, agID)
	lines := historyLines(t, s, agID)
	status := groupStatusOf(t, s, agID)

	postOne(t, s, thread, claimable(t, s)[thread])
	if got := statusOf(t, s, thread); got != outbound.StatusIdle {
		t.Fatalf("the thread is %s after it went out", got)
	}
	if got := historyLines(t, s, agID); got != lines {
		t.Fatalf("the thread wrote %d line(s) into the alert's history", got-lines)
	}
	if got := groupStatusOf(t, s, agID); got != status {
		t.Fatalf("the thread moved the group from %s to %s", status, got)
	}
}

// TestAStartGivesTheCardsOfThePreviousVersionTheirSatellites. Every Slack
// channel card admitted by a version that had no satellites gets its thread
// and its reply at the first start of this one, in the card's own claim and
// aimed at the group's revision: with a message or without, live or failed.
// A card of an alert that is over, a withdrawn card, and a card on another
// provider get none. Once: a second start finds nothing to do.
func TestAStartGivesTheCardsOfThePreviousVersionTheirSatellites(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	live := desiredGroup(t, s, "Disk filling up")
	liveCard := admitOne(t, s, live)[0]
	postOne(t, s, liveCard, claimOne(t, s, liveCard))

	waiting := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, waiting)

	failed := desiredGroup(t, s, "Disk filling up")
	refusedForGood(t, s, admitOne(t, s, failed)[0])

	over := desiredGroup(t, s, "Disk filling up")
	overCard := admitOne(t, s, over)[0]
	postOne(t, s, overCard, claimOne(t, s, overCard))
	endTheAlert(t, s, over)

	withdrawn := desiredGroup(t, s, "Disk filling up")
	withdrawnCard := admitOne(t, s, withdrawn)[0]
	acknowledge(t, s, withdrawn)
	if got := statusOf(t, s, withdrawnCard); got != outbound.StatusCanceled {
		t.Fatalf("the card of the acknowledged alert is %s", got)
	}

	elsewhere := desiredGroup(t, s, "Disk filling up")
	telegram := channelCommitment("C0001", 0)
	telegram.Provider = "telegram"
	admitOne(t, s, elsewhere, telegram)

	before := countWhere(t, s, `SELECT count(*) FROM outbound_intents`)
	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := countWhere(t, s, `SELECT count(*) FROM outbound_intents`); got != before+6 {
		t.Fatalf("the start added %d commitment(s), want 6: two for each of three cards", got-before)
	}

	for _, agID := range []string{live, waiting, failed} {
		card, thread, reply := satellitesOf(t, s, agID)
		revision, _ := storedRevision(t, s, agID)
		for _, id := range []string{thread, reply} {
			var parent string
			var aimed int64
			var claim string
			if err := s.db.QueryRow(`SELECT COALESCE(parent_intent_id, ''), desired_revision, batch_id
				FROM outbound_intents WHERE id = $1`, id).Scan(&parent, &aimed, &claim); err != nil {
				t.Fatal(err)
			}
			if parent != card {
				t.Fatalf("%s follows %q, not its card %s", id, parent, card)
			}
			if aimed != revision {
				t.Fatalf("%s is aimed at revision %d, and the group is at %d", id, aimed, revision)
			}
			if got := journalOf(t, s, id); len(got) != 1 || !strings.HasPrefix(got[0], "created|") ||
				!strings.HasSuffix(got[0], "|system") {
				t.Fatalf("the journal of %s reads %v", id, got)
			}
			var form string
			if err := s.db.QueryRow(`SELECT form FROM outbound_intents WHERE id = $1`, id).Scan(&form); err != nil {
				t.Fatal(err)
			}
			if want := map[string]string{thread: "editable", reply: "one_shot"}[id]; form != want {
				t.Fatalf("%s was given as %s, want %s", id, form, want)
			}
			if got := countWhere(t, s, `SELECT intent_count FROM outbound_batches WHERE id = $1`, claim); got != 3 {
				t.Fatalf("the card's claim counts %d commitment(s), want the card and its two satellites", got)
			}
		}
	}
	for _, agID := range []string{over, withdrawn, elsewhere} {
		if got := countWhere(t, s, `SELECT count(*) FROM outbound_intents
			WHERE alert_group_id = $1 AND parent_intent_id IS NOT NULL`, agID); got != 0 {
			t.Fatalf("a card that gets no satellites got %d", got)
		}
	}

	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := countWhere(t, s, `SELECT count(*) FROM outbound_intents`); got != before+6 {
		t.Fatalf("the second start added %d commitment(s)", got-before-6)
	}

	// The claim treats them as it treats any satellite: the live card's
	// thread goes, the waiting card's waits.
	_, liveThread, _ := satellitesOf(t, s, live)
	_, waitingThread, _ := satellitesOf(t, s, waiting)
	leases := claimable(t, s)
	if _, ok := leases[liveThread]; !ok {
		t.Fatal("the thread given to a card with a message was not claimed")
	}
	if _, ok := leases[waitingThread]; ok {
		t.Fatal("the thread given to a card without a message was claimed")
	}
}

// TestARaiseAndASatellitesBeginDoNotDeadlock is the interleaving the pipeline
// found: a begin holds the thread and updates it twice - the generation is
// bound, then the row is marked sending - while a merge, holding the group,
// raises the revision and updates the card and the thread in one statement,
// card first. The second update of the thread checks its key to the group
// again and takes KEY SHARE on the group's row; held FOR UPDATE by the merge,
// that was a cycle - the merge waiting for the thread, the begin waiting for
// the group. The group is held FOR NO KEY UPDATE, which KEY SHARE does not
// wait behind, and the begin goes through while the merge waits its turn.
//
// The begin is raw so the interleaving is exact; the raise is the real door.
func TestARaiseAndASatellitesBeginDoNotDeadlock(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	_, thread, _ := cardWithSatellites(t, s, agID)
	ctx := context.Background()
	group, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatal(err)
	}

	begin, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer begin.Rollback()
	if _, err := begin.ExecContext(ctx, `SELECT 1 FROM outbound_intents WHERE id = $1 FOR UPDATE`, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := begin.ExecContext(ctx, `UPDATE outbound_intents SET bound_endpoint = 'C0001/1' WHERE id = $1`, thread); err != nil {
		t.Fatal(err)
	}

	raised := make(chan error, 1)
	go func() {
		// An alert joins the group: the merge takes the group, raises the
		// revision and aims the card and the thread - and waits for the
		// thread the begin holds.
		_, err := s.ApplyAlertmanagerUpdateAtomic(ctx, group.AlertKey, []model.Alert{
			{Fingerprint: "fp-1", Status: model.AlertStatusFiring, StartsAt: time.Unix(1700000000, 0),
				Labels: map[string]string{"alertname": "DiskWillFill"}},
			{Fingerprint: "fp-2", Status: model.AlertStatusFiring, StartsAt: time.Unix(1700000600, 0),
				Labels: map[string]string{"alertname": "DiskSlow"}},
		}, "alertmanager")
		raised <- err
	}()
	time.Sleep(300 * time.Millisecond) // the merge is now waiting on the thread

	if _, err := begin.ExecContext(ctx, `UPDATE outbound_intents SET worker_id = 'w-1', updated_at = now() WHERE id = $1`,
		thread); err != nil {
		t.Fatalf("the begin's second update: %v", err)
	}
	if err := begin.Commit(); err != nil {
		t.Fatalf("commit the begin: %v", err)
	}
	select {
	case err := <-raised:
		if err != nil && strings.Contains(err.Error(), "deadlock") {
			t.Fatalf("the merge deadlocked against the begin: %v", err)
		}
		if err != nil {
			t.Fatalf("the merge: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the merge never got the thread")
	}
	if _, aimed := cardAim(t, s, thread); aimed != 1 {
		t.Fatalf("the thread is aimed at %d after the merge", aimed)
	}
}
