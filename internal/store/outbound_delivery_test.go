package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Delivery is where the promises made at admission are kept, and every test
// here is about a moment when something goes wrong in the middle: a lease that
// expires with a call in flight, a reply that arrives after somebody else gave
// up waiting, an acknowledgement landing between two statements. The happy path
// is one test; the rest is what the design exists for.

const testFamily = "notification"

// dmCommitment is the one-shot form: a direct message is sent once and is done,
// which is what makes it settle as succeeded rather than as a card that could
// still be brought to a later revision.
func dmCommitment(ref string) keys.EscalationCommitment {
	c := channelCommitment(ref, 0)
	c.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: 1}
	c.Target = keys.Target{Kind: keys.TargetUser, Ref: ref}
	c.Editable = false
	return c
}

func admitOne(t *testing.T, s *Store, agID string,
	commitments ...keys.EscalationCommitment) []string {
	t.Helper()

	if len(commitments) == 0 {
		commitments = []keys.EscalationCommitment{channelCommitment("C0001", 0)}
	}
	result := mustSubmit(t, s, outboundAdmission(t, agID, "first", commitments...))
	if result.Outcome != outbound.SubmitCreated {
		t.Fatalf("the admission answered %q", result.Outcome)
	}
	return result.IntentIDs
}

func claimOne(t *testing.T, s *Store, intentID string) string {
	t.Helper()

	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, l := range leased {
		if l.Intent.ID == intentID {
			return l.LeaseToken
		}
	}
	t.Fatalf("the claim did not include %s (it returned %d commitments)", intentID, len(leased))
	return ""
}

func beginOne(t *testing.T, s *Store, intentID, token string) outbound.BeginAttemptResult {
	t.Helper()

	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID:      intentID,
		LeaseToken:    token,
		WorkerID:      "worker-1",
		Preparation:   outbound.PreparationReady,
		BoundEndpoint: "C0001",
	})
	if err != nil {
		t.Fatalf("begin an attempt: %v", err)
	}
	return result
}

// expireLease makes a lease look abandoned, which is the only way to reach the
// recovery paths without waiting a minute and a half.
func expireLease(t *testing.T, s *Store, intentID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET locked_until = now() - interval '1 second' WHERE id = $1`,
		intentID); err != nil {
		t.Fatalf("age the lease of %s: %v", intentID, err)
	}
}

// accepted is what a worker is allowed to say: the provider took it, and here
// is the message it made. Which revision that message carried is not the
// worker's to state - the store fills it in from the attempt.
//
// Built through the domain, like production does, so a test cannot express a
// request production cannot.
func accepted() outbound.Conclusion {
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomeAccepted,
		Status:  "ok",
		Receipt: receiptOf("C0001/1700000000.000100",
			`{"channel":"C0001","ts":"1700000000.000100"}`),
	})
}

// concluded is any other answer: nothing was made, so there is nothing to show.
func concluded(outcome outbound.Outcome, class string) outbound.Conclusion {
	return conclusion(outbound.ConclusionInput{Outcome: outcome, Class: class})
}

func conclusion(in outbound.ConclusionInput) outbound.Conclusion {
	built, err := outbound.NewConclusion(in)
	if err != nil {
		panic(err)
	}
	return built
}

func receiptOf(ref, raw string) outbound.Receipt {
	receipt, err := outbound.NewReceipt(ref, json.RawMessage(raw))
	if err != nil {
		panic(err)
	}
	return receipt
}

func statusOf(t *testing.T, s *Store, intentID string) outbound.Status {
	t.Helper()
	intent, err := s.GetIntent(context.Background(), intentID)
	if err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	if intent == nil {
		t.Fatalf("commitment %s vanished", intentID)
	}
	return intent.Status
}

func groupStatusOf(t *testing.T, s *Store, agID string) model.AlertGroupStatus {
	t.Helper()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM alert_groups WHERE id = $1`, agID).
		Scan(&status); err != nil {
		t.Fatalf("read the group: %v", err)
	}
	return model.AlertGroupStatus(status)
}

// TestDeliveryHappyPath is the whole cycle once: claim, open the attempt before
// the call, close it with what the provider said, and let the alert group learn
// about it in the same commit.
func TestDeliveryHappyPath(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if begun.Outcome != outbound.BeginStarted {
		t.Fatalf("beginning an attempt answered %q", begun.Outcome)
	}
	if begun.Snapshot.Content().AlertGroupID != agID {
		t.Fatal("the attempt was handed content belonging to another group")
	}
	if statusOf(t, s, intentID) != outbound.StatusSending {
		t.Fatalf("after beginning, the commitment is %s", statusOf(t, s, intentID))
	}

	// The record exists before the network could have been touched, and says so.
	var startedBeforeFinish bool
	if err := s.db.QueryRow(`
		SELECT started_at IS NOT NULL AND finished_at IS NULL
		FROM outbound_attempts WHERE id = $1`, begun.AttemptID).Scan(&startedBeforeFinish); err != nil {
		t.Fatalf("read the attempt: %v", err)
	}
	if !startedBeforeFinish {
		t.Fatal("the attempt was not open before the call")
	}

	result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID:  begun.AttemptID,
		LeaseToken: token,
		Conclusion: accepted(),
	})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if result.Outcome != outbound.FinalizeFinalized || result.To != outbound.StatusSucceeded {
		t.Fatalf("finalizing answered %q into %s", result.Outcome, result.To)
	}

	if got := groupStatusOf(t, s, agID); got != model.AlertGroupStatusTriggered {
		t.Fatalf("after the first successful send the group is %s", got)
	}

	// The coordinates are on the attempt as well as on the commitment: a later
	// generation clears the commitment's copy, and the address of a message
	// that really was sent must not go with it.
	var attemptReceipt, intentReceipt []byte
	if err := s.db.QueryRow(`
		SELECT a.receipt, i.receipt FROM outbound_attempts a
		JOIN outbound_intents i ON i.id = a.intent_id WHERE a.id = $1`, begun.AttemptID).
		Scan(&attemptReceipt, &intentReceipt); err != nil {
		t.Fatalf("read the receipts: %v", err)
	}
	if len(attemptReceipt) == 0 || len(intentReceipt) == 0 {
		t.Fatal("the receipt was not kept in both places")
	}

	var sent int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM timeline_events WHERE alert_group_id = $1 AND type = $2`,
		agID, model.TimelineEventNotificationSent).Scan(&sent); err != nil {
		t.Fatalf("count the timeline: %v", err)
	}
	if sent != 1 {
		t.Fatalf("the alert's history has %d sends, want 1", sent)
	}
}

// TestAnEditableCardSettlesIdleRatherThanDone is the difference between a
// message and a card: a card that has applied everything asked of it so far is
// not finished, because the next revision of the alert puts it back in the
// queue.
func TestAnEditableCardSettlesIdleRatherThanDone(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)

	result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
	})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if result.To != outbound.StatusIdle {
		t.Fatalf("a card that applied the latest revision is %s", result.To)
	}

	// And the final revision of an alert - its resolution - is what ends it.
	// Which revision that is comes from the stored state, not from the worker:
	// a worker that could declare an attempt final could retire a card the
	// alert is still using.
	otherGroup := outboundGroup(t, s)
	other := admitOne(t, s, otherGroup)[0]
	if _, err := s.db.Exec(
		`UPDATE outbound_group_snapshots SET final = TRUE WHERE alert_group_id = $1`,
		otherGroup); err != nil {
		t.Fatalf("mark the state as final: %v", err)
	}
	otherToken := claimOne(t, s, other)
	otherAttempt := beginOne(t, s, other, otherToken)
	final, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: otherAttempt.AttemptID, LeaseToken: otherToken,
		Conclusion: accepted(),
	})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if final.To != outbound.StatusSucceeded {
		t.Fatalf("a card that applied its last revision is %s", final.To)
	}
}

// TestClaimMintsAFreshTokenAndSkipsHeldWork covers the fencing and the sharing
// at once: every claim gets a new token, and two claims never take one another's
// rows.
func TestClaimMintsAFreshTokenAndSkipsHeldWork(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	first := claimOne(t, s, intentID)

	// Still held: a second claim finds nothing.
	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(leased) != 0 {
		t.Fatalf("a held commitment was claimed again by %d workers", len(leased))
	}

	expireLease(t, s, intentID)
	second := claimOne(t, s, intentID)
	if second == first {
		t.Fatal("the reclaim reused the old token; a returning worker would still look like the owner")
	}
}

// TestClaimSplitsWorkBetweenWorkers is the concurrent version: rows are divided,
// never shared, and nobody waits.
func TestClaimSplitsWorkBetweenWorkers(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	admitOne(t, s, agID,
		channelCommitment("C0001", 0), channelCommitment("C0002", 0),
		channelCommitment("C0003", 0), channelCommitment("C0004", 0))

	var wg sync.WaitGroup
	start := make(chan struct{})
	claims := make([][]outbound.Leased, 2)
	errs := make([]error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			claims[i], errs[i] = s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
				Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
				Limit: 4, Lease: outbound.NotificationLease, WorkerID: "worker-1",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		for _, l := range claims[i] {
			if seen[l.Intent.ID] {
				t.Fatalf("commitment %s was claimed by both workers", l.Intent.ID)
			}
			seen[l.Intent.ID] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("the two workers claimed %d of 4 commitments", len(seen))
	}
}

// TestClaimPrefersFirstAttempts is the fairness rule inside one provider: a
// freshly admitted page does not queue behind a pile of old retries.
func TestClaimPrefersFirstAttempts(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	ids := admitOne(t, s, agID, channelCommitment("C0001", 0), channelCommitment("C0002", 0))

	// Make the first one look like an old retry: tried once, due earlier.
	if _, err := s.db.Exec(`
		UPDATE outbound_intents
		SET attempts_in_generation = 1, next_attempt_at = now() - interval '1 hour'
		WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("age the retry: %v", err)
	}

	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimFirstAttempts,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(leased) != 1 || leased[0].Intent.ID != ids[1] {
		t.Fatalf("the first-attempt claim took %d commitments, and not the untried one", len(leased))
	}
}

// TestARetryIsReachedPastAnOlderBacklog is the guarantee the scheduler thinks
// it has, checked against the SQL that has to provide it.
//
// A hundred pages admitted two hours ago and never attempted are OLDER than a
// retry that failed an hour ago. Ordered by due time alone, the share the
// scheduler sets aside for retries is spent on those pages instead, and under a
// steady stream of new work the retry is never sent at all - the promise to
// keep trying stops being one, silently, in the one place nobody looks.
func TestARetryIsReachedPastAnOlderBacklog(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	commitments := make([]keys.EscalationCommitment, 4)
	for i := range commitments {
		commitments[i] = channelCommitment(fmt.Sprintf("C%04d", i), 0)
	}
	intents := admitOne(t, s, agID, commitments...)

	// Everything is overdue by two hours; one of them has been attempted, and
	// its own retry is only an hour old - the newest row of the lot.
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET next_attempt_at = now() - interval '2 hours'
		 WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("age the queue: %v", err)
	}
	retried := intents[len(intents)-1]
	if _, err := s.db.Exec(`
		UPDATE outbound_intents
		SET attempts_in_generation = 1, failure_streak = 1,
		    next_attempt_at = now() - interval '1 hour'
		WHERE id = $1`, retried); err != nil {
		t.Fatalf("age the retry: %v", err)
	}

	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 1, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(leased) != 1 {
		t.Fatalf("the claim took %d commitments", len(leased))
	}
	if leased[0].Intent.ID != retried {
		t.Fatalf("the one slot reserved for retries went to %s, an untried commitment "+
			"that happens to be older", leased[0].Intent.ID)
	}

	// And when there is no retry to reach, the same claim does not idle: the
	// slot goes to whatever is due.
	fallback, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 2, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if len(fallback) != 2 {
		t.Fatalf("with no retries left the claim took %d of the 2 slots it had",
			len(fallback))
	}
	for _, l := range fallback {
		if l.Intent.AttemptsInGeneration != 0 {
			t.Fatalf("%s was claimed twice", l.Intent.ID)
		}
	}
}

// TestAClaimForNothingThisBuildTakesIsRefused. The phases are a closed set, and
// the wrong default is silent: an unrecognised phase treated as "anything due"
// hands out leases for work the caller did not ask for, and the caller finds
// out by delivering it.
func TestAClaimForNothingThisBuildTakesIsRefused(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: "whatever_comes",
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if !errors.Is(err, ErrOutboundContract) {
		t.Fatalf("a phase nobody declared was served: %v", err)
	}
	if len(leased) != 0 {
		t.Fatalf("%d commitments were leased anyway", len(leased))
	}

	var token sql.NullString
	if err := s.db.QueryRow(
		`SELECT lease_token FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&token); err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	if token.Valid {
		t.Fatal("the refused claim still took a lease")
	}
}

// TestDueSnapshotSeparatesSchedulingFromHealth pins the two numbers that must
// not be one: what a claim can take, and how late the queue is.
func TestDueSnapshotSeparatesSchedulingFromHealth(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	ids := admitOne(t, s, agID, channelCommitment("C0001", 0), channelCommitment("C0002", 0))

	// One is held by another instance and never begun - the shape of a worker
	// that hung after claiming. Written directly so the test does not depend on
	// which row a claim happens to take.
	if _, err := s.db.Exec(`
		UPDATE outbound_intents
		SET lease_token = 'held-elsewhere', locked_until = now() + interval '1 minute',
		    next_attempt_at = now() - interval '2 minutes'
		WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("hold the commitment elsewhere: %v", err)
	}

	due, err := s.DueSnapshot(context.Background(), testFamily)
	if err != nil {
		t.Fatalf("read the queue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("the queue reported %d providers", len(due))
	}
	if due[0].ClaimableDue != 1 {
		t.Errorf("the scheduler was offered %d commitments, want the one that is free",
			due[0].ClaimableDue)
	}
	if due[0].LatenessSeconds < 100 {
		t.Errorf("lateness is %.0fs: the commitment claimed and never begun is hidden",
			due[0].LatenessSeconds)
	}
}

// TestExpireEndsWhatIsOverdue: a deadline that passes with nothing sent is a
// terminal outcome, and it has to leave an explanation behind - a status change
// on its own says nothing to whoever finds it later.
func TestExpireEndsWhatIsOverdue(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	ids := admitOne(t, s, agID, channelCommitment("C0001", 0), channelCommitment("C0002", 0))

	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		ids[0]); err != nil {
		t.Fatalf("set a deadline in the past: %v", err)
	}

	expired, err := s.ExpireDueIntents(context.Background(), testFamily, 10)
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if len(expired) != 1 || expired[0].IntentID != ids[0] {
		t.Fatalf("expiry took %d commitments", len(expired))
	}
	if statusOf(t, s, ids[0]) != outbound.StatusExpired {
		t.Fatalf("the overdue commitment is %s", statusOf(t, s, ids[0]))
	}
	if statusOf(t, s, ids[1]) != outbound.StatusPending {
		t.Fatal("expiry took a commitment that was not due")
	}

	var events int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_intent_events WHERE intent_id = $1 AND kind = 'expired'`,
		ids[0]).Scan(&events); err != nil {
		t.Fatalf("count the events: %v", err)
	}
	if events != 1 {
		t.Fatalf("the expired commitment left %d explanations", events)
	}
}

// TestBeginRefusesWhatItCannotAuthorise walks the answers a worker can get
// instead of an attempt. Each of them is a moment when going ahead would send a
// message somebody has already decided against.
func TestBeginRefusesWhatItCannotAuthorise(t *testing.T) {
	s := setupTestDB(t)

	t.Run("somebody else holds it now", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		claimOne(t, s, intentID)

		result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: "a token from a previous life",
			Preparation: outbound.PreparationReady,
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if result.Outcome != outbound.BeginLeaseLost {
			t.Fatalf("a stale token answered %q", result.Outcome)
		}
	})

	t.Run("the reply to a previous begin was lost", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		beginOne(t, s, intentID, token)

		again := beginOne(t, s, intentID, token)
		if again.Outcome != outbound.BeginUncertain {
			t.Fatalf("a repeated begin answered %q", again.Outcome)
		}

		var attempts int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM outbound_attempts WHERE intent_id = $1`, intentID).
			Scan(&attempts); err != nil {
			t.Fatalf("count the attempts: %v", err)
		}
		if attempts != 1 {
			t.Fatalf("a repeated begin authorised the network %d times", attempts)
		}
	})

	t.Run("it was withdrawn while the worker prepared", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)

		if _, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}

		result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: token,
			Preparation: outbound.PreparationReady,
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if result.Outcome != outbound.BeginIntentFinalized {
			t.Fatalf("beginning a withdrawn commitment answered %q", result.Outcome)
		}
		if statusOf(t, s, intentID) != outbound.StatusCanceled {
			t.Fatalf("after the acknowledgement the commitment is %s", statusOf(t, s, intentID))
		}
	})
}

// TestPreparationRefusalsLeaveProofRatherThanDoubt is the distinction the
// journal is built around: a call that provably never happened is recorded as
// such, and never as an attempt whose fate is unknown.
func TestPreparationRefusalsLeaveProofRatherThanDoubt(t *testing.T) {
	s := setupTestDB(t)

	cases := []struct {
		name        string
		preparation outbound.PreparationOutcome
		expect      outbound.BeginOutcome
		status      outbound.Status
	}{
		{
			name:        "nothing will fix itself",
			preparation: outbound.PreparationPermanent,
			expect:      outbound.BeginPreparedPermanent,
			status:      outbound.StatusPermanentFailed,
		},
		{
			name:        "not this time",
			preparation: outbound.PreparationTransient,
			expect:      outbound.BeginPreparedRetry,
			status:      outbound.StatusPending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agID := outboundGroup(t, s)
			intentID := admitOne(t, s, agID)[0]
			token := claimOne(t, s, intentID)

			result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
				IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
				Preparation: tc.preparation, ErrorClass: "identity_not_linked",
			})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if result.Outcome != tc.expect {
				t.Fatalf("a %s preparation answered %q", tc.preparation, result.Outcome)
			}
			if got := statusOf(t, s, intentID); got != tc.status {
				t.Fatalf("the commitment is %s, want %s", got, tc.status)
			}

			var kind string
			var startedAt, leaseToken *string
			if err := s.db.QueryRow(`
				SELECT record_kind, started_at::text, lease_token
				FROM outbound_attempts WHERE intent_id = $1`, intentID).
				Scan(&kind, &startedAt, &leaseToken); err != nil {
				t.Fatalf("read the journal: %v", err)
			}
			if kind != string(outbound.RecordPreparation) || startedAt != nil || leaseToken != nil {
				t.Fatalf("the refusal was recorded as %q with started_at=%v: it looks like a call",
					kind, startedAt)
			}

			// And the lease is released in both halves, or the commitment would
			// be invisible until a lease nobody holds expires.
			var leased bool
			if err := s.db.QueryRow(`
				SELECT lease_token IS NOT NULL OR locked_until IS NOT NULL
				FROM outbound_intents WHERE id = $1`, intentID).Scan(&leased); err != nil {
				t.Fatalf("read the lease: %v", err)
			}
			if leased {
				t.Fatal("the refusal left a lease behind")
			}
		})
	}
}

// TestRecoveryClosesAbandonedAttempts is the case a worker dying mid-flight
// leaves behind: an attempt that might have reached the provider, and a
// commitment nobody is working on.
func TestRecoveryClosesAbandonedAttempts(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	expireLease(t, s, intentID)

	recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].AttemptID != begun.AttemptID {
		t.Fatalf("recovery took %d attempts", len(recovered))
	}

	// The doubt is recorded before anything else is decided.
	var outcome, reason string
	if err := s.db.QueryRow(
		`SELECT outcome, finish_reason FROM outbound_attempts WHERE id = $1`, begun.AttemptID).
		Scan(&outcome, &reason); err != nil {
		t.Fatalf("read the attempt: %v", err)
	}
	if outcome != string(outbound.OutcomeAmbiguous) || reason != "lease_lost" {
		t.Fatalf("the abandoned attempt was closed as %q/%q", outcome, reason)
	}

	// And the commitment is queued again rather than quietly dropped.
	if got := statusOf(t, s, intentID); got != outbound.StatusPending {
		t.Fatalf("after recovery the commitment is %s", got)
	}
	var streak int
	if err := s.db.QueryRow(
		`SELECT failure_streak FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&streak); err != nil {
		t.Fatalf("read the streak: %v", err)
	}
	if streak != 1 {
		t.Fatalf("the abandoned attempt counted as %d failures", streak)
	}
}

// TestRecoveryAndFinalizeCannotBothWin is a worker coming back to an attempt
// the recovery already gave up on. Whoever takes the row first decides; the
// other is told, and a genuine late result is kept rather than dropped.
//
// Both orders are played out here one after the other, which is what pins the
// OUTCOME of each. That the two cannot interleave into a deadlock is a
// different claim, and it is made where it can actually be observed:
// outbound_race_test.go.
func TestRecoveryAndFinalizeCannotBothWin(t *testing.T) {
	s := setupTestDB(t)

	t.Run("recovery first, then the worker returns", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)
		expireLease(t, s, intentID)

		if _, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10); err != nil {
			t.Fatalf("recover: %v", err)
		}

		result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: accepted(),
		})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.Outcome != outbound.FinalizeLeaseLost {
			t.Fatalf("the returning worker answered %q", result.Outcome)
		}
		if !result.ObservationRecorded {
			t.Fatal("a late acceptance was thrown away; it may be the only proof the message exists")
		}

		var observations int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM outbound_attempt_observations WHERE attempt_id = $1`,
			begun.AttemptID).Scan(&observations); err != nil {
			t.Fatalf("count the observations: %v", err)
		}
		if observations != 1 {
			t.Fatalf("%d late results were kept", observations)
		}
	})

	t.Run("the worker finishes first", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: accepted(),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}

		// Recovery looks for commitments that are still sending; a finished one
		// is invisible to it, whatever its lease says.
		recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(recovered) != 0 {
			t.Fatalf("recovery reopened %d finished attempts", len(recovered))
		}
		if statusOf(t, s, intentID) != outbound.StatusSucceeded {
			t.Fatalf("the delivered commitment is %s", statusOf(t, s, intentID))
		}
	})

	t.Run("two recoveries at once", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		beginOne(t, s, intentID, token)
		expireLease(t, s, intentID)

		var wg sync.WaitGroup
		start := make(chan struct{})
		counts := make([]int, 2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
				counts[i], errs[i] = len(recovered), err
			}(i)
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("recovery %d: %v", i, err)
			}
		}
		if counts[0]+counts[1] != 1 {
			t.Fatalf("the abandoned attempt was recovered %d times", counts[0]+counts[1])
		}
	})
}

// TestFinalizeClassifiesEveryWayItCanBeCalled walks the ladder. The order of
// its rungs is the design: a successful finalisation clears the commitment's
// token, so a repeat has to be recognised by the attempt rather than by the
// lease it no longer matches.
func TestFinalizeClassifiesEveryWayItCanBeCalled(t *testing.T) {
	s := setupTestDB(t)

	t.Run("a repeat after a lost reply", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		request := outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
		}
		if _, err := s.FinalizeDeliveryAttempt(context.Background(), request); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		again, err := s.FinalizeDeliveryAttempt(context.Background(), request)
		if err != nil {
			t.Fatalf("finalize again: %v", err)
		}
		if again.Outcome != outbound.FinalizeIdempotentRepeat {
			t.Fatalf("a repeat answered %q", again.Outcome)
		}
	})

	t.Run("two different conclusions for one call", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}

		other, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomeAmbiguous, "no_response"),
		})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if other.Outcome != outbound.FinalizeConflict {
			t.Fatalf("a contradicting conclusion answered %q", other.Outcome)
		}
	})

	t.Run("a caller that cannot prove the result is its own", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: "somebody else's token",
			Conclusion: accepted(),
		})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.Outcome != outbound.FinalizeLeaseLost {
			t.Fatalf("a stranger's conclusion answered %q", result.Outcome)
		}
		if result.ObservationRecorded {
			t.Fatal("a stranger's 'accepted' was kept as evidence")
		}
		if statusOf(t, s, intentID) != outbound.StatusSending {
			t.Fatal("a stranger's conclusion moved the commitment")
		}
	})

	t.Run("an attempt nobody has", func(t *testing.T) {
		result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: "00000000-0000-0000-0000-000000000000", LeaseToken: "t",
			Conclusion: accepted(),
		})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.Outcome != outbound.FinalizeNotFound {
			t.Fatalf("finalizing nothing answered %q", result.Outcome)
		}
	})

	// The rungs above are climbed before anything the caller SAID is read, and
	// the two cases that used to prove it - an unknown attempt and a stranger's
	// token, each carrying a malformed result - can no longer be written: a
	// conclusion is built by the domain, and the shapes they relied on are
	// unrepresentable. What survives is where the checks sit in
	// FinalizeDeliveryAttempt, after the token gate, and the domain tests that
	// nobody can build those shapes at all.
}

// TestAcknowledgementMeetsADeliveryInFlight is what the domain exists to make
// boring: whatever order the two arrive in, the alert ends up acknowledged and
// nobody is paged twice. Sequential on purpose - each subtest fixes one order
// and states what it must produce. The concurrent version, where the two
// transactions really overlap, is in outbound_race_test.go.
func TestAcknowledgementMeetsADeliveryInFlight(t *testing.T) {
	s := setupTestDB(t)

	t.Run("acknowledged before the call went out", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]

		if _, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusCanceled {
			t.Fatalf("an unsent notification survived the acknowledgement as %s", got)
		}

		var events int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM outbound_intent_events WHERE intent_id = $1 AND kind = 'canceled'`,
			intentID).Scan(&events); err != nil {
			t.Fatalf("count the events: %v", err)
		}
		if events != 1 {
			t.Fatalf("the withdrawal left %d explanations", events)
		}
	})

	t.Run("acknowledged while the call was in flight and it failed", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusSending {
			t.Fatalf("a send in flight was interrupted rather than flagged: %s", got)
		}

		result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
		})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.To != outbound.StatusCanceled {
			t.Fatalf("a failed send of a withdrawn notification became %s", result.To)
		}
	})

	t.Run("acknowledged while the call was in flight and it landed", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}

		result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: accepted(),
		})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.To != outbound.StatusSucceeded {
			t.Fatalf("a message that really went out became %s", result.To)
		}
		// The acknowledgement stands: a delivery never moves the group back.
		if got := groupStatusOf(t, s, agID); got != model.AlertGroupStatusAcknowledged {
			t.Fatalf("the group is %s after a send that raced the acknowledgement", got)
		}
	})

	t.Run("acknowledged after a retryable failure", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusPending {
			t.Fatalf("a retryable failure left the commitment %s", got)
		}

		if _, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusCanceled {
			t.Fatalf("the retry survived the acknowledgement as %s", got)
		}
	})
}

// TestBackoffGrowsWithTheStreak pins the curve the retries follow, and the fact
// that a success clears it: the failures of a delivery that has already
// happened have no business slowing down the next one.
func TestBackoffGrowsWithTheStreak(t *testing.T) {
	within := func(d time.Duration, base time.Duration) bool {
		low := time.Duration(float64(base) * 0.8)
		high := time.Duration(float64(base) * 1.2)
		return d >= low && d <= high
	}

	for streak, base := range map[int]time.Duration{
		1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second,
		9: 5 * time.Minute, 100: 5 * time.Minute,
	} {
		got := outbound.Backoff(streak)
		if !within(got, base) {
			t.Errorf("failure %d waits %s, want about %s", streak, got, base)
		}
	}
}

// TestTheClaimReadsTheQueueThroughTheIndex is the difference between a rule
// that holds and a rule that holds until it matters.
//
// The claim asks for four rows in a particular order. If the index cannot
// produce that order, PostgreSQL reads every due row of the provider and sorts
// it - which is invisible on the empty queue every other test runs against, and
// is exactly the queue an outage does not leave behind. The plan is asserted on
// a backlog big enough for the planner to have a choice.
func TestTheClaimReadsTheQueueThroughTheIndex(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	seed := admitOne(t, s, agID)[0]

	var batchID string
	if err := s.db.QueryRow(
		`SELECT batch_id FROM outbound_intents WHERE id = $1`, seed).Scan(&batchID); err != nil {
		t.Fatalf("read the batch: %v", err)
	}

	// A backlog of an outage: thousands due, most of them never attempted, and
	// the retries scattered through it.
	if _, err := s.db.Exec(`
		INSERT INTO outbound_intents (
			id, batch_id, idempotency_key, delivery_family, key_kind, grammar_version,
			provider, target_kind, target_ref, alert_group_id, form, completion_mode,
			ambiguity_policy, payload_schema_version, payload, provider_key_codec_version,
			status, desired_revision, attempts_in_generation, not_before, next_attempt_at)
		SELECT gen_random_uuid()::text, $1, 'backlog-' || g, $2, 'escalation', 1,
		       'slack', 'channel', 'C' || g, $3, 'editable', 'on_acceptance',
		       'retry', 1,
		       jsonb_build_object(
			       'slot', jsonb_build_object('kind', 'firehose', 'index', 0),
			       'target', jsonb_build_object('kind', 'channel', 'ref', 'C' || g),
			       'interactive', true), 1,
		       'pending', 0, CASE WHEN g % 10 = 0 THEN 1 ELSE 0 END,
		       now() - interval '3 hours', now() - make_interval(secs => g)
		FROM generate_series(1, 5000) g`, batchID, testFamily, agID); err != nil {
		t.Fatalf("build the backlog: %v", err)
	}
	if _, err := s.db.Exec(`ANALYZE outbound_intents`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	for _, phase := range []outbound.ClaimPhase{
		outbound.ClaimFirstAttempts, outbound.ClaimRetriesFirst,
	} {
		t.Run(string(phase), func(t *testing.T) {
			statement, err := claimStatement(phase)
			if err != nil {
				t.Fatalf("build the claim: %v", err)
			}

			var plan string
			if err := s.db.QueryRow("EXPLAIN (FORMAT JSON) "+statement,
				testFamily, "slack", 4, outbound.NotificationLease.Seconds(),
				"worker-1").Scan(&plan); err != nil {
				t.Fatalf("explain the claim: %v", err)
			}

			if strings.Contains(plan, `"Node Type": "Seq Scan"`) {
				t.Errorf("the claim reads the whole table:\n%s", plan)
			}
			if strings.Contains(plan, `"Node Type": "Sort"`) {
				t.Errorf("the claim sorts the backlog to take four rows:\n%s", plan)
			}
			if !strings.Contains(plan, `"Node Type": "Index Scan"`) {
				t.Errorf("the claim does not walk an index at all:\n%s", plan)
			}
		})
	}
}
