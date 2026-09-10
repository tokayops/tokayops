package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a direct message takes from the card is settled when its generation
// opens and never again inside it.

// stepCard is a channel card posted by a numbered step of the policy.
func stepCard(ref string, index int) keys.EscalationCommitment {
	c := channelCommitment(ref, 0)
	c.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: index}
	return c
}

// intentAddressedTo is the card or the message sent to a recipient - never
// the thread or the reply that follow a card, which carry the card's channel
// as their own recipient and would otherwise be found by scan order.
func intentAddressedTo(t *testing.T, s *Store, agID, ref string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM outbound_intents
		WHERE alert_group_id = $1 AND target_ref = $2 AND parent_intent_id IS NULL`,
		agID, ref).Scan(&id); err != nil {
		t.Fatalf("find the commitment to %s: %v", ref, err)
	}
	return id
}

// takeOne claims one commitment after letting go of every lease an earlier
// claim of the test still holds on work that has not begun: a claim takes
// everything due, as a worker's does.
func takeOne(t *testing.T, s *Store, id string) string {
	t.Helper()
	letGoOfLeases(t, s)
	return claimOne(t, s, id)
}

// letGoOfLeases ends every lease an earlier claim of the test still holds on
// work that has not begun, so the queue reads as a worker that just died
// left it.
func letGoOfLeases(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE outbound_intents SET locked_until = now() - interval '1 second'
		WHERE lease_token IS NOT NULL AND status = 'pending'`); err != nil {
		t.Fatalf("let go of the leases: %v", err)
	}
}

// postedAs delivers a card and records the receipt Slack gave it.
func postedAs(t *testing.T, s *Store, id, ref string) {
	t.Helper()
	token := takeOne(t, s, id)
	begun := beginOne(t, s, id, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: conclusion(outbound.ConclusionInput{
			Outcome: outbound.OutcomeAccepted, Status: "ok",
			Receipt: receiptOf(ref, `{"channel":"C","ts":"1"}`),
		}),
	}); err != nil {
		t.Fatalf("post %s: %v", id, err)
	}
}

func boundContextOf(t *testing.T, begun outbound.BeginAttemptResult) outbound.BoundContext {
	t.Helper()
	return begun.BoundContext
}

func slackWorkspaceIntegration(t *testing.T, s *Store, config string) string {
	t.Helper()
	integration := &model.Integration{
		Type: model.IntegrationTypeSlack, Name: "Slack", Enabled: true, Config: []byte(config),
	}
	if err := s.CreateIntegration(integration); err != nil {
		t.Fatalf("create the Slack integration: %v", err)
	}
	return integration.ID
}

// rejectedOnce drives a card through one attempt that the provider refused
// for now: the card is back in pending on its backoff, with a failure to its
// name and no receipt.
func rejectedOnce(t *testing.T, s *Store, id string) {
	t.Helper()
	token := takeOne(t, s, id)
	begun := beginOne(t, s, id, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatalf("reject %s once: %v", id, err)
	}
	if got := statusOf(t, s, id); got != outbound.StatusPending {
		t.Fatalf("a card rejected once is %s, want pending on its backoff", got)
	}
}

// awaitedBy is the array a message carries: the cards it waits for and
// links to, sorted, or nil.
func awaitedBy(t *testing.T, s *Store, id string) []string {
	t.Helper()
	intent, err := s.GetIntent(context.Background(), id)
	if err != nil || intent == nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return intent.AwaitsIntentIDs
}

func sortedIDs(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func claimableDue(t *testing.T, s *Store) int {
	t.Helper()
	due, err := s.DueSnapshot(context.Background(), testFamily)
	if err != nil {
		t.Fatalf("due snapshot: %v", err)
	}
	for _, row := range due {
		if row.Provider == "slack" {
			return row.ClaimableDue
		}
	}
	return 0
}

// TestADirectMessageWaitsForTheCardsItCouldLinkTo. The message names, at
// admission, the channel cards of its batch due no later than it, and the
// claim does not hand it to a worker until every one of them has had its
// first attempt: its link is part of its words, and the words are fixed
// when it is first sent. Once offered it binds the card it waited for - the
// policy's own step over the firehose - in the workspace the integration
// names. While it waits it is not late, and its demand is not counted; the
// wait is measured where a page's latency is, in the admission histogram.
func TestADirectMessageWaitsForTheCardsItCouldLinkTo(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), stepCard("C-step", 1), dmCommitment("u-1"))
	fire := intentAddressedTo(t, s, agID, "C-fire")
	step := intentAddressedTo(t, s, agID, "C-step")
	dm := intentAddressedTo(t, s, agID, "u-1")

	if got, want := awaitedBy(t, s, dm), sortedIDs(fire, step); !reflect.DeepEqual(got, want) {
		t.Fatalf("the message waits for %v, want %v", got, want)
	}
	for _, card := range []string{fire, step} {
		if awaitedBy(t, s, card) != nil {
			t.Fatalf("a card waits for %v", awaitedBy(t, s, card))
		}
	}

	// Nothing has gone out: the cards are offered, the message is not.
	if _, ok := claimable(t, s)[dm]; ok {
		t.Fatal("the message was offered before any of its cards had been tried")
	}
	postedAs(t, s, fire, "C-fire/1700000000.000100")
	if _, ok := claimable(t, s)[dm]; ok {
		t.Fatal("the message was offered while the policy's own card had not been tried")
	}

	// An hour past its time by the clock, and the queue is not late by an
	// hour, nor is the message counted as demand: it could not be taken.
	if _, err := s.db.Exec(`UPDATE outbound_intents SET next_attempt_at = now() - interval '1 hour'
		WHERE id = $1`, dm); err != nil {
		t.Fatal(err)
	}
	letGoOfLeases(t, s)
	if late, _ := latenessOf(t, s, testFamily); late > 60 {
		t.Fatalf("the queue is %.0fs late on a message that is waiting for its card", late)
	}
	if n := claimableDue(t, s); n != 1 {
		t.Fatalf("the demand counts %d, want the step alone", n)
	}

	// The step is out: the message goes, late from when it was due, and
	// binds the step over the firehose.
	postedAs(t, s, step, "C-step/1700000000.000200")
	letGoOfLeases(t, s)
	if late, _ := latenessOf(t, s, testFamily); late < 3600 {
		t.Fatalf("the queue is %.0fs late on a message that has been due for an hour", late)
	}
	if n := claimableDue(t, s); n != 1 {
		t.Fatalf("the demand counts %d, want the message alone", n)
	}
	token, ok := claimable(t, s)[dm]
	if !ok {
		t.Fatal("the message was not offered once its cards had been tried")
	}
	begun := beginOne(t, s, dm, token)
	if got := boundContextOf(t, begun); got != (outbound.BoundContext{
		CardReceiptRef: "C-step/1700000000.000200", TeamURL: "https://acme.slack.com/"}) {
		t.Fatalf("the message was bound to %+v", got)
	}
	if begun.FirstAttemptLatency == nil {
		t.Fatal("the wait left no mark in the admission latency, where a page's delay is measured")
	}
}

// TestADirectMessageIsReleasedByACardStillRetrying. The wait is one attempt
// of the card and no more: a card the provider refused for now is back in
// pending on its backoff, and the message goes at once, without the link,
// rather than through the retries.
func TestADirectMessageIsReleasedByACardStillRetrying(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-1"))
	fire := intentAddressedTo(t, s, agID, "C-fire")
	dm := intentAddressedTo(t, s, agID, "u-1")

	rejectedOnce(t, s, fire)
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents
		WHERE id = $1 AND status = 'pending' AND failure_streak = 1 AND NOT receipt_recorded
		  AND next_attempt_at > now()`, fire) != 1 {
		t.Fatal("the card is not pending on its backoff with one failure to its name")
	}
	token, ok := claimable(t, s)[dm]
	if !ok {
		t.Fatal("the message was not offered after its card's first attempt")
	}
	if got := boundContextOf(t, beginOne(t, s, dm, token)); !got.Empty() {
		t.Fatalf("the message was bound to %+v, want nothing", got)
	}
}

// TestADirectMessageLinksToTheCardThatDidGoOut. Among the cards it waited
// for, the message links to one that has a message: the policy's step was
// refused for now and the firehose went out, so it links to the firehose.
func TestADirectMessageLinksToTheCardThatDidGoOut(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), stepCard("C-step", 1), dmCommitment("u-1"))
	fire := intentAddressedTo(t, s, agID, "C-fire")
	step := intentAddressedTo(t, s, agID, "C-step")
	dm := intentAddressedTo(t, s, agID, "u-1")

	rejectedOnce(t, s, step)
	postedAs(t, s, fire, "C-fire/1700000000.000100")
	token, ok := claimable(t, s)[dm]
	if !ok {
		t.Fatal("the message was not offered after both cards had been tried")
	}
	if got := boundContextOf(t, beginOne(t, s, dm, token)).CardReceiptRef; got != "C-fire/1700000000.000100" {
		t.Fatalf("the message links to %q, want the firehose that went out", got)
	}
}

// TestADirectMessageIsReleasedByACardThatEndedWithoutAMessage. A card that
// failed for good, that waits for a person, or that was withdrawn has been
// tried: the message goes without the link. A page does not wait for an
// operator's decision on a card.
func TestADirectMessageIsReleasedByACardThatEndedWithoutAMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, s *Store, card string)
	}{
		{"failed for good", func(t *testing.T, s *Store, card string) { refusedForGood(t, s, card) }},
		{"waiting for a person", func(t *testing.T, s *Store, card string) {
			token := takeOne(t, s, card)
			begun := beginOne(t, s, card, token)
			if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
				AttemptID: begun.AttemptID, LeaseToken: token,
				Conclusion: concluded(outbound.OutcomeAmbiguous, "no_response"),
			}); err != nil {
				t.Fatal(err)
			}
			if got := statusOf(t, s, card); got != outbound.StatusManualReview {
				t.Fatalf("the card is %s, want manual_review", got)
			}
		}},
		{"withdrawn", func(t *testing.T, s *Store, card string) {
			refusedForGood(t, s, card)
			resolve(t, s, outbound.ResolveAmbiguityRequest{
				IntentID: card, Decision: outbound.DecisionCancel, Reason: "the channel is gone",
			})
			if got := statusOf(t, s, card); got != outbound.StatusCanceled {
				t.Fatalf("the card is %s, want canceled", got)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")
			slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
			agID := desiredGroup(t, s, "Disk filling up")
			card := channelCommitment("C-fire", 0)
			card.AmbiguityPolicy = keys.PolicyManualReview
			admitOne(t, s, agID, card, dmCommitment("u-1"))
			fire := intentAddressedTo(t, s, agID, "C-fire")
			dm := intentAddressedTo(t, s, agID, "u-1")

			if _, ok := claimable(t, s)[dm]; ok {
				t.Fatal("the message was offered before its card had been tried")
			}
			letGoOfLeases(t, s)
			tc.end(t, s, fire)
			token, ok := claimable(t, s)[dm]
			if !ok {
				t.Fatal("the message was not offered once its card had ended without a message")
			}
			if got := boundContextOf(t, beginOne(t, s, dm, token)); !got.Empty() {
				t.Fatalf("the message was bound to %+v, want nothing", got)
			}
		})
	}
}

// TestADirectMessageDoesNotWaitForACardDueLater. A step that comes five
// minutes after the message is not a card the message could link to: it is
// not named, not waited for, and the message links to the card that was due
// with it.
func TestADirectMessageDoesNotWaitForACardDueLater(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	agID := desiredGroup(t, s, "Disk filling up")
	late := channelCommitment("C-late", 5*time.Minute)
	late.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: 1}
	admitOne(t, s, agID, channelCommitment("C-fire", 0), late, dmCommitment("u-1"))
	fire := intentAddressedTo(t, s, agID, "C-fire")
	dm := intentAddressedTo(t, s, agID, "u-1")

	if got, want := awaitedBy(t, s, dm), []string{fire}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the message waits for %v, want the firehose alone", got)
	}
	postedAs(t, s, fire, "C-fire/1700000000.000100")
	token, ok := claimable(t, s)[dm]
	if !ok {
		t.Fatal("the message was held for a card that is not due for five minutes")
	}
	if got := boundContextOf(t, beginOne(t, s, dm, token)).CardReceiptRef; got != "C-fire/1700000000.000100" {
		t.Fatalf("the message links to %q", got)
	}
}

// TestADirectMessageDoesNotNameTheFirehoseWhenTheFallbackIsOff. With the
// setting off the firehose is not a card the message could link to: nothing
// is named, nothing is waited for, nothing is bound.
func TestADirectMessageDoesNotNameTheFirehoseWhenTheFallbackIsOff(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	s.SetDMFallbackToFirehose(false)
	t.Cleanup(func() { s.SetDMFallbackToFirehose(true) }) // the store is shared by the package's tests
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-1"))
	dm := intentAddressedTo(t, s, agID, "u-1")

	if got := awaitedBy(t, s, dm); got != nil {
		t.Fatalf("with the fallback off the message waits for %v", got)
	}
	token, ok := claimable(t, s)[dm]
	if !ok {
		t.Fatal("the message was held for a firehose it may not link to")
	}
	if got := boundContextOf(t, beginOne(t, s, dm, token)); !got.Empty() {
		t.Fatalf("the message was bound to %+v", got)
	}
}

// TestABoundGenerationDoesNotWait. A message whose generation is bound
// carries what it carries: when its card, tried once and left for a person,
// is later retried by an operator and is pending again with no failure to
// its name, the message's retry does not wait for it.
func TestABoundGenerationDoesNotWait(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	agID := desiredGroup(t, s, "Disk filling up")
	card := channelCommitment("C-fire", 0)
	card.AmbiguityPolicy = keys.PolicyManualReview
	admitOne(t, s, agID, card, dmCommitment("u-1"))
	fire := intentAddressedTo(t, s, agID, "C-fire")
	dm := intentAddressedTo(t, s, agID, "u-1")
	if got := awaitedBy(t, s, dm); !reflect.DeepEqual(got, []string{fire}) {
		t.Fatalf("the message waits for %v, want the card: %s", got, rowsOf(t, s, fire, dm))
	}

	// The card's fate is unknown and a person is asked; the message goes.
	token := takeOne(t, s, fire)
	begun := beginOne(t, s, fire, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeAmbiguous, "no_response"),
	}); err != nil {
		t.Fatal(err)
	}
	token, ok := claimable(t, s)[dm]
	if !ok {
		t.Fatal("the message was not offered once its card waited for a person: " + rowsOf(t, s, fire, dm))
	}
	begun = beginOne(t, s, dm, token) // bound, to nothing
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatal(err)
	}

	// The card is tried again: pending, no receipt, no failure to its name -
	// a card the message would wait for, were its generation not bound.
	resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: fire, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again",
	})
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents
		WHERE id = $1 AND status = 'pending' AND failure_streak = 0 AND NOT receipt_recorded`, fire) != 1 {
		t.Fatal("the retried card is not pending with a clean name")
	}
	due(t, s, dm)
	if _, ok := claimable(t, s)[dm]; !ok {
		t.Fatal("a bound generation's retry was held for its card")
	}
}

// TestTheContextNamesTheCardThatGotAReceipt. Two workers, one queue, thirty
// alerts: whichever holds the card posts it, whichever holds the message
// begins it as soon as it is offered. Every message whose card ended with a
// receipt names that card - there is no order of claims in which it begins
// before the card has been tried.
func TestTheContextNamesTheCardThatGotAReceipt(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)

	const rounds = 30
	for round := 0; round < rounds; round++ {
		agID := desiredGroup(t, s, "Disk filling up")
		admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-1"))
		fire := intentAddressedTo(t, s, agID, "C-fire")
		dm := intentAddressedTo(t, s, agID, "u-1")
		ref := fmt.Sprintf("C-fire/1700000000.%06d", round)
		if got := awaitedBy(t, s, dm); !reflect.DeepEqual(got, []string{fire}) {
			t.Fatalf("round %d: the message waits for %v, want the card: %s", round, got, rowsOf(t, s, fire, dm))
		}

		contexts := make(chan outbound.BoundContext, 1)
		failures := make(chan error, 2)
		var wg sync.WaitGroup
		for _, worker := range []string{"worker-a", "worker-b"} {
			wg.Add(1)
			go func(worker string) {
				defer wg.Done()
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
					leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
						Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
						Limit: 1, Lease: outbound.NotificationLease, WorkerID: worker,
					})
					if err != nil {
						failures <- err
						return
					}
					for _, l := range leased {
						begun, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
							IntentID: l.Intent.ID, LeaseToken: l.LeaseToken, WorkerID: worker,
							Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
						})
						if err != nil {
							failures <- err
							return
						}
						verdict := concluded(outbound.OutcomePermanentRejection, "user_not_found")
						if l.Intent.ID == fire {
							verdict = conclusion(outbound.ConclusionInput{
								Outcome: outbound.OutcomeAccepted, Status: "ok",
								Receipt: receiptOf(ref, `{"channel":"C","ts":"1"}`),
							})
						} else {
							contexts <- begun.BoundContext
						}
						if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
							AttemptID: begun.AttemptID, LeaseToken: l.LeaseToken, Conclusion: verdict,
						}); err != nil {
							failures <- err
							return
						}
					}
					if intent, err := s.GetIntent(context.Background(), dm); err == nil && intent != nil &&
						intent.Status != outbound.StatusPending {
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}(worker)
		}
		wg.Wait()
		select {
		case err := <-failures:
			t.Fatalf("round %d: %v", round, err)
		default:
		}
		select {
		case got := <-contexts:
			if got.CardReceiptRef != ref {
				t.Fatalf("round %d: the message names %q, the card got %q: %s", round, got.CardReceiptRef, ref, rowsOf(t, s, fire, dm))
			}
		default:
			t.Fatalf("round %d: the message never began", round)
		}
	}
}

// rowsOf is the state of some commitments, for a failure message.
func rowsOf(t *testing.T, s *Store, ids ...string) string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, status, failure_streak, receipt_recorded, create_key IS NOT NULL,
		locked_until, next_attempt_at, awaits_intent_ids FROM outbound_intents WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var id, status string
		var streak int
		var receipt, bound bool
		var locked, next sql.NullTime
		var awaits []string
		if err := rows.Scan(&id, &status, &streak, &receipt, &bound, &locked, &next, pq.Array(&awaits)); err != nil {
			t.Fatal(err)
		}
		out += fmt.Sprintf("\n  %s: %s streak=%d receipt=%v bound=%v locked=%v next=%v awaits=%v",
			id, status, streak, receipt, bound, locked.Time, next.Time, awaits)
	}
	return out
}

// TestTheFirehoseIsAFallbackTheInstallationCanRefuse. With the setting off,
// a message whose policy posted no channel card of its own points to nothing;
// and a card in a workspace no integration names is bound without one, so the
// message goes out without a link rather than with a broken one.
func TestTheFirehoseIsAFallbackTheInstallationCanRefuse(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	integration := slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)

	s.SetDMFallbackToFirehose(false)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), stepCard("C-step", 1), dmCommitment("u-1"))
	postedAs(t, s, intentAddressedTo(t, s, agID, "C-fire"), "C-fire/1700000000.000100")
	rejectedOnce(t, s, intentAddressedTo(t, s, agID, "C-step")) // tried, no message
	dm := intentAddressedTo(t, s, agID, "u-1")
	if begun := beginOne(t, s, dm, takeOne(t, s, dm)); !boundContextOf(t, begun).Empty() {
		t.Fatalf("with the fallback off the message was bound to %+v", begun.BoundContext)
	}

	s.SetDMFallbackToFirehose(true)
	if _, err := s.UpdateIntegration(context.Background(), integration,
		IntegrationPatch{Enabled: boolPtr(false)}, "nina"); err != nil {
		t.Fatalf("disable the integration: %v", err)
	}
	agID = desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-2"))
	postedAs(t, s, intentAddressedTo(t, s, agID, "C-fire"), "C-fire/1700000000.000300")
	dm = intentAddressedTo(t, s, agID, "u-2")
	begun := beginOne(t, s, dm, takeOne(t, s, dm))
	if got := boundContextOf(t, begun); got != (outbound.BoundContext{CardReceiptRef: "C-fire/1700000000.000300"}) {
		t.Fatalf("without an enabled integration the message was bound to %+v", got)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestAContextNobodyCanReadIsRefusedBeforeTheNetwork. The bound context is
// written by this build's store and read back at every begin of the
// generation; a row this build cannot read is damage, and it ends the
// commitment where a person will see it - not inside the call, where it
// would be an attempt that never touched the network, retried forever.
func TestAContextNobodyCanReadIsRefusedBeforeTheNetwork(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-1"))
	postedAs(t, s, intentAddressedTo(t, s, agID, "C-fire"), "C-fire/1700000000.000100")
	dm := intentAddressedTo(t, s, agID, "u-1")

	token := takeOne(t, s, dm)
	begun := beginOne(t, s, dm, token) // the generation is open and bound
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE outbound_intents
		SET bound_context = '{"card_receipt_ref":"C-fire/1700000000.000100","louder":true}' WHERE id = $1`, dm); err != nil {
		t.Fatal(err)
	}
	due(t, s, dm)

	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: dm, LeaseToken: takeOne(t, s, dm), WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "D0001",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if result.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("a context this build cannot read began as %s", result.Outcome)
	}
	if got := statusOf(t, s, dm); got != outbound.StatusPermanentFailed {
		t.Fatalf("the message is %s", got)
	}
	if got := lastErrorClass(t, s, dm); got != "bound_context_unreadable" {
		t.Fatalf("the refusal is classed %q", got)
	}
}
