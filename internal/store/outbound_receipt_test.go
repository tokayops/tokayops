package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
)

// Calls whose acceptance only means "queued", through the store: the object an
// attempt made, the provider's events, the poll, and the verdict they add up
// to.
//
// No production producer admits such a commitment yet, so the fixture makes
// one the way a build that knew the family would: a direct message, moved to
// the call family and told to wait for the provider's word.

// callTranslator reads a Twilio-shaped dialect, which is what these tests
// speak. Anything else is a status this build does not know.
type callTranslator struct{}

func (callTranslator) EffectStateOf(e outbound.ProviderEvent) (outbound.EffectState, string, bool) {
	switch e.ProviderStatus {
	case "queued", "initiated", "ringing", "in-progress":
		return outbound.EffectInProgress, e.ProviderStatus, true
	case "completed", "busy", "no-answer":
		return outbound.EffectHappened, e.ProviderStatus, true
	case "failed":
		return outbound.EffectNotPlaced, e.ProviderStatus, true
	case "canceled":
		return outbound.EffectWithdrawn, e.ProviderStatus, true
	default:
		return "", "", false
	}
}

var callTranslators = map[string]outbound.EffectTranslator{"slack": callTranslator{}}

// admitCall is one call to one person, in a group of its own.
func admitCall(t *testing.T, s *Store) (agID, intentID string) {
	t.Helper()
	agID = outboundGroup(t, s)
	intentID = admitOne(t, s, agID, dmCommitment("U0001"))[0]
	relabel(t, s, intentID, "escalation", outbound.FamilyCall)
	exec(t, s, `UPDATE outbound_intents SET completion_mode = 'on_provider_receipt' WHERE id = $1`, intentID)
	return agID, intentID
}

func claimCall(t *testing.T, s *Store, intentID string) string {
	t.Helper()
	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: outbound.FamilyCall, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.CallLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, l := range leased {
		if l.Intent.ID == intentID {
			return l.LeaseToken
		}
	}
	t.Fatalf("the claim did not include %s", intentID)
	return ""
}

// callAccepted is the provider taking the call into its queue: it names the
// call it made and says nothing of how it will go.
func callAccepted(sid string) outbound.Conclusion {
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomeAccepted,
		Status:  "queued",
		Receipt: receiptOf(sid, `{"sid":"`+sid+`"}`),
	})
}

// placeCall runs one attempt of the call to its answer.
func placeCall(t *testing.T, s *Store, intentID string, c outbound.Conclusion) outbound.BeginAttemptResult {
	t.Helper()
	token := claimCall(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if begun.Outcome != outbound.BeginStarted {
		t.Fatalf("begin answered %s", begun.Outcome)
	}
	finalize(t, s, begun.AttemptID, token, c)
	return begun
}

func finalize(t *testing.T, s *Store, attemptID, token string, c outbound.Conclusion) outbound.FinalizeResult {
	t.Helper()
	result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: attemptID, LeaseToken: token, Conclusion: c,
	})
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return result
}

var eventCounter int

func callEvent(attemptID, sid, status string, sequence *int) outbound.ProviderEvent {
	eventCounter++
	return outbound.ProviderEvent{
		Provider: "twilio-test", AccountScope: "AC1",
		EventID:   fmt.Sprintf("%s:%s:%d", sid, status, eventCounter),
		AttemptID: attemptID, ExternalRef: sid, ProviderStatus: status, Sequence: sequence,
	}
}

func hear(t *testing.T, s *Store, event outbound.ProviderEvent) outbound.ApplyResult {
	t.Helper()
	id, err := s.RecordProviderEvent(context.Background(), event)
	if err != nil {
		t.Fatalf("keep the event: %v", err)
	}
	result, err := s.ApplyProviderEvent(context.Background(), id, callTranslators)
	if err != nil {
		t.Fatalf("apply the event: %v", err)
	}
	return result
}

func seqOf(n int) *int { return &n }

func effectStateOf(t *testing.T, s *Store, attemptID string) outbound.EffectState {
	t.Helper()
	var state outbound.EffectState
	if err := s.db.QueryRow(`SELECT state FROM outbound_effects WHERE attempt_id = $1`, attemptID).
		Scan(&state); err != nil {
		t.Fatalf("read the object of %s: %v", attemptID, err)
	}
	return state
}

func countOf(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The provider took the call into its queue: the commitment waits, holding the
// object its attempt made, and the alert is not yet "triggered" - nobody knows
// whether anybody was reached.
func TestAnAcceptedCallWaitsForItsFate(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	agID, intentID := admitCall(t, s)

	begun := placeCall(t, s, intentID, callAccepted("CA1"))

	if got := statusOf(t, s, intentID); got != outbound.StatusAwaitingReceipt {
		t.Fatalf("an accepted call is %s, want awaiting_receipt", got)
	}
	if got := effectStateOf(t, s, begun.AttemptID); got != outbound.EffectInProgress {
		t.Fatalf("the call's object is %s", got)
	}
	if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1
		AND receipt_timeout_at > now() AND receipt_deadline > receipt_timeout_at AND lease_token IS NULL`,
		intentID); n != 1 {
		t.Fatal("the wait was not scheduled, or the lease was kept")
	}
	if got := groupStatusOf(t, s, agID); got == "triggered" {
		t.Fatal("the alert moved on the strength of a call nobody has heard the end of")
	}

	result := hear(t, s, callEvent(begun.AttemptID, "CA1", "completed", seqOf(3)))
	if result.To != outbound.StatusSucceeded || result.Row != "T35/S1" {
		t.Fatalf("the end of the call settled as %s (%s)", result.To, result.Row)
	}
	if got := groupStatusOf(t, s, agID); got != "triggered" {
		t.Fatalf("the alert is %s after the call ended", got)
	}
}

// A callback can overtake the answer to the request that made the call. It
// finds the attempt - written before the network was touched - and the answer
// that comes after it finds the call already over.
func TestACallbackBeforeTheAnswerIsNotLost(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	token := claimCall(t, s, intentID)
	begun := beginOne(t, s, intentID, token)

	early := hear(t, s, callEvent(begun.AttemptID, "CA1", "completed", seqOf(3)))
	if early.Outcome != outbound.ApplyApplied || early.To != "" {
		t.Fatalf("an event during the request: %+v", early)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusSending {
		t.Fatalf("the event moved a commitment whose request is still open: %s", got)
	}

	result := finalize(t, s, begun.AttemptID, token, callAccepted("CA1"))
	if result.To != outbound.StatusSucceeded {
		t.Fatalf("the answer after the end of the call settled as %s (%s)", result.To, result.Row)
	}
}

// Events are separate requests: they repeat and they arrive out of order.
func TestEventsRepeatAndArriveOutOfOrder(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	begun := placeCall(t, s, intentID, callAccepted("CA1"))

	end := callEvent(begun.AttemptID, "CA1", "completed", seqOf(3))
	first := hear(t, s, end)
	if first.To != outbound.StatusSucceeded {
		t.Fatalf("the end of the call: %+v", first)
	}
	if again := hear(t, s, end); again.Outcome != outbound.ApplyAlreadyDone {
		t.Fatalf("the same event twice: %+v", again)
	}
	if late := hear(t, s, callEvent(begun.AttemptID, "CA1", "ringing", seqOf(2))); late.Outcome != outbound.ApplyIgnored {
		t.Fatalf("a ringing after the end: %+v", late)
	}
	if got := effectStateOf(t, s, begun.AttemptID); got != outbound.EffectHappened {
		t.Fatalf("a late event moved an object that had ended: %s", got)
	}
}

// Two calls of one generation: a doubtful attempt and the one after it. The
// first one failing says nothing about the second, which is ringing.
func TestOneCallFailingDoesNotDecideTheOther(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)

	doubtful := placeCall(t, s, intentID, concluded(outbound.OutcomeAmbiguous, "timeout"))
	if got := statusOf(t, s, intentID); got != outbound.StatusPending {
		t.Fatalf("after a doubtful call: %s", got)
	}
	exec(t, s, `UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID)
	second := placeCall(t, s, intentID, callAccepted("CA2"))

	hear(t, s, callEvent(doubtful.AttemptID, "CA1", "failed", seqOf(4)))
	if got := statusOf(t, s, intentID); got != outbound.StatusAwaitingReceipt {
		t.Fatalf("the first call's failure decided the second: %s", got)
	}
	if result := hear(t, s, callEvent(second.AttemptID, "CA2", "failed", seqOf(4))); result.Row != "T36" {
		t.Fatalf("both calls failed: %+v", result)
	}
}

// The provider could not place the call, and the alert still needs it: a new
// generation. Nobody needs it any more: nothing is made again.
func TestACallThatWasNotPlaced(t *testing.T) {
	t.Run("the obligation stands", func(t *testing.T) {
		s := setupTestDB(t)
		asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
		_, intentID := admitCall(t, s)
		begun := placeCall(t, s, intentID, callAccepted("CA1"))

		result := hear(t, s, callEvent(begun.AttemptID, "CA1", "failed", seqOf(4)))
		if result.To != outbound.StatusPending || result.Row != "T36" {
			t.Fatalf("a call not placed: %+v", result)
		}
		if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1
			AND generation_no = 1 AND receipt_deadline IS NULL AND receipt_timeout_at IS NULL`, intentID); n != 1 {
			t.Fatal("the new generation did not start with a fresh wait")
		}
	})

	t.Run("the alert was acknowledged while the call waited", func(t *testing.T) {
		s := setupTestDB(t)
		asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
		agID, intentID := admitCall(t, s)
		begun := placeCall(t, s, intentID, callAccepted("CA1"))

		if _, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusAwaitingReceipt {
			t.Fatalf("the acknowledgement moved a call already placed: %s", got)
		}
		if n := countOf(t, s, `SELECT count(*) FROM outbound_intents
			WHERE id = $1 AND obligation_withdrawn_at IS NOT NULL`, intentID); n != 1 {
			t.Fatal("the acknowledgement left no record on the waiting call")
		}
		if n := countOf(t, s, `SELECT count(*) FROM outbound_intent_events
			WHERE intent_id = $1 AND kind = 'obligation_withdrawn'`, intentID); n != 1 {
			t.Fatal("the journal does not say the obligation was withdrawn")
		}

		result := hear(t, s, callEvent(begun.AttemptID, "CA1", "failed", seqOf(4)))
		if result.To != outbound.StatusCanceled || result.Row != "T36w" {
			t.Fatalf("a call nobody needs that was not placed: %+v", result)
		}
		if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND generation_no = 0`,
			intentID); n != 1 {
			t.Fatal("a new generation was started for an acknowledged alert")
		}
	})

	t.Run("the alert was acknowledged while the call was being placed", func(t *testing.T) {
		s := setupTestDB(t)
		asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
		agID, intentID := admitCall(t, s)
		token := claimCall(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		result := finalize(t, s, begun.AttemptID, token, callAccepted("CA1"))
		if result.To != outbound.StatusAwaitingReceipt || result.Row != "T16r" {
			t.Fatalf("a call accepted after the acknowledgement: %+v", result)
		}
		if n := countOf(t, s, `SELECT count(*) FROM outbound_intents
			WHERE id = $1 AND obligation_withdrawn_at IS NOT NULL AND NOT cancellation_requested`,
			intentID); n != 1 {
			t.Fatal("the withdrawal was not carried over to the waiting call")
		}
		if result := hear(t, s, callEvent(begun.AttemptID, "CA1", "failed", seqOf(4))); result.Row != "T36w" {
			t.Fatalf("then it was not placed: %+v", result)
		}
	})
}

// A request that ended in doubt is due to be repeated. Hearing about the call
// it may have made decides the repeat.
func TestADoubtfulCallHeardFromLater(t *testing.T) {
	t.Run("it took place: no repeat", func(t *testing.T) {
		s := setupTestDB(t)
		asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
		_, intentID := admitCall(t, s)
		doubtful := placeCall(t, s, intentID, concluded(outbound.OutcomeAmbiguous, "timeout"))

		result := hear(t, s, callEvent(doubtful.AttemptID, "CA1", "completed", seqOf(3)))
		if result.To != outbound.StatusSucceeded || result.Row != "T35p" {
			t.Fatalf("a doubtful call that took place: %+v", result)
		}
	})

	t.Run("it is ringing: wait, not a second call", func(t *testing.T) {
		s := setupTestDB(t)
		asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
		_, intentID := admitCall(t, s)
		doubtful := placeCall(t, s, intentID, concluded(outbound.OutcomeAmbiguous, "timeout"))

		result := hear(t, s, callEvent(doubtful.AttemptID, "CA1", "ringing", seqOf(2)))
		if result.To != outbound.StatusAwaitingReceipt || result.Row != "T39" {
			t.Fatalf("a doubtful call that is ringing: %+v", result)
		}
	})

	t.Run("it was not placed: repeated as a new generation", func(t *testing.T) {
		s := setupTestDB(t)
		asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
		_, intentID := admitCall(t, s)
		doubtful := placeCall(t, s, intentID, concluded(outbound.OutcomeAmbiguous, "timeout"))

		result := hear(t, s, callEvent(doubtful.AttemptID, "CA1", "failed", seqOf(4)))
		if result.To != outbound.StatusPending || result.Row != "T36p" {
			t.Fatalf("a doubtful call that did not happen: %+v", result)
		}
		// No doubt is left, so the repeat resolves afresh rather than going
		// back through the integration that just failed under the same key.
		if n := countOf(t, s, `SELECT count(*) FROM outbound_intents
			WHERE id = $1 AND generation_no = 1 AND create_key IS NULL`, intentID); n != 1 {
			t.Fatal("the repeat stayed in the generation that failed")
		}
	})
}

// The callback was lost. The wait comes round, the provider is asked, and the
// answer goes in through the same door as any callback.
func TestALostCallbackIsAskedFor(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	begun := placeCall(t, s, intentID, callAccepted("CA1"))

	ctx := context.Background()
	if due, _ := s.ClaimDueReceipts(ctx, outbound.FamilyCall, 10); len(due) != 0 {
		t.Fatal("a wait was taken before it came round")
	}
	exec(t, s, `UPDATE outbound_intents SET receipt_timeout_at = now() - interval '1 second' WHERE id = $1`, intentID)

	due, err := s.ClaimDueReceipts(ctx, outbound.FamilyCall, 10)
	if err != nil || len(due) != 1 || len(due[0].Effects) != 1 || due[0].Effects[0].ExternalRef != "CA1" {
		t.Fatalf("the wait that came round: %+v, %v", due, err)
	}
	if again, _ := s.ClaimDueReceipts(ctx, outbound.FamilyCall, 10); len(again) != 0 {
		t.Fatal("the same wait was taken twice")
	}

	answer := callEvent(begun.AttemptID, "CA1", "no-answer", nil)
	answer.EventID = "poll:CA1:no-answer"
	hear(t, s, answer)
	if got := statusOf(t, s, intentID); got != outbound.StatusSucceeded {
		t.Fatalf("after the poll's answer: %s", got)
	}
}

// Several instances poll at once; each wait is taken by one of them.
func TestEachWaitIsTakenOnce(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	const calls = 6
	for i := 0; i < calls; i++ {
		_, intentID := admitCall(t, s)
		placeCall(t, s, intentID, callAccepted(fmt.Sprintf("CA%d", i)))
	}
	exec(t, s, `UPDATE outbound_intents SET receipt_timeout_at = now() - interval '1 second'
		WHERE status = 'awaiting_receipt'`)

	var mu sync.Mutex
	taken := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			due, err := s.ClaimDueReceipts(context.Background(), outbound.FamilyCall, calls)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range due {
				taken[d.IntentID]++
			}
		}()
	}
	wg.Wait()
	if len(taken) != calls {
		t.Fatalf("%d of %d waits were taken", len(taken), calls)
	}
	for id, n := range taken {
		if n != 1 {
			t.Fatalf("wait %s was taken %d times", id, n)
		}
	}
}

// The last word was "in progress", the callback that would have ended it was
// lost and every poll failed. The deadline still ends the wait.
func TestTheDeadlineEndsAWaitWhoseLastWordWasInProgress(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	begun := placeCall(t, s, intentID, callAccepted("CA1"))
	hear(t, s, callEvent(begun.AttemptID, "CA1", "ringing", seqOf(2)))

	ctx := context.Background()
	if result, err := s.ReviewReceiptWait(ctx, intentID); err != nil || result.Row != "T38" {
		t.Fatalf("a poll before the deadline with nothing new: %+v, %v", result, err)
	}
	exec(t, s, `UPDATE outbound_intents SET receipt_deadline = now() - interval '1 second' WHERE id = $1`, intentID)
	result, err := s.ReviewReceiptWait(ctx, intentID)
	if err != nil || result.To != outbound.StatusSucceeded || result.Row != "T37/S1" {
		t.Fatalf("after the deadline: %+v, %v", result, err)
	}
	if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND accepted_duplicate_risk`,
		intentID); n != 1 {
		t.Fatal("an assumed call recorded no risk")
	}
}

// A call already placed is not the commitment's deadline's to end: its fate is
// the poll's.
func TestExpiryLeavesAWaitingCallAlone(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	placeCall(t, s, intentID, callAccepted("CA1"))
	exec(t, s, `UPDATE outbound_intents SET expires_at = now() - interval '1 second' WHERE id = $1`, intentID)

	if _, err := s.ExpireDueIntents(context.Background(), outbound.FamilyCall, 10); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusAwaitingReceipt {
		t.Fatalf("a waiting call was expired: %s", got)
	}
}

// A status this build has never seen does not decide anything.
func TestAnUnknownProviderStatusChangesNothing(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	begun := placeCall(t, s, intentID, callAccepted("CA1"))

	if result := hear(t, s, callEvent(begun.AttemptID, "CA1", "answered-by-robot", seqOf(5))); result.Outcome != outbound.ApplyIgnored {
		t.Fatalf("an unknown status: %+v", result)
	}
	if got := effectStateOf(t, s, begun.AttemptID); got != outbound.EffectInProgress {
		t.Fatalf("an unknown status moved the object to %s", got)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusAwaitingReceipt {
		t.Fatalf("an unknown status moved the commitment to %s", got)
	}
}

// An event about an attempt nothing here made waits in the inbox.
func TestAnEventAboutNothingHereIsKeptUnmatched(t *testing.T) {
	s := setupTestDB(t)
	stranger := callEvent("no-such-attempt", "CA9", "completed", seqOf(3))
	if result := hear(t, s, stranger); result.Outcome != outbound.ApplyUnmatched {
		t.Fatalf("an event about nothing: %+v", result)
	}
	if n := countOf(t, s, `SELECT count(*) FROM outbound_provider_events WHERE state = 'unmatched'`); n != 1 {
		t.Fatal("the stranger's event was not kept")
	}
}

// The history of a finished call goes with its commitment, objects and events
// included, and before the attempts they point at.
func TestAFinishedCallIsSweptWithEverythingItMade(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	begun := placeCall(t, s, intentID, callAccepted("CA1"))
	hear(t, s, callEvent(begun.AttemptID, "CA1", "completed", seqOf(3)))
	exec(t, s, `UPDATE outbound_intents SET updated_at = now() - interval '400 days' WHERE id = $1`, intentID)

	result, err := s.SweepDeliveryHistory(context.Background(), time.Now().Add(-200*24*time.Hour), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Deleted.Intents != 1 || result.Deleted.Effects != 1 || result.Deleted.ProviderEvents != 1 {
		t.Fatalf("the sweep removed %+v", result.Deleted)
	}
}

// An event that settles nothing - "ringing", heard while the wait is due -
// leaves the next look where it was. Pushed back by every such event, a call
// whose provider keeps sending progress would never be asked about, and its
// deadline would be the only thing left to end it.
func TestAnEventThatSettlesNothingDoesNotPushTheNextLook(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	begun := placeCall(t, s, intentID, callAccepted("CA1"))
	exec(t, s, `UPDATE outbound_intents SET receipt_timeout_at = now() - interval '1 second' WHERE id = $1`, intentID)

	result := hear(t, s, callEvent(begun.AttemptID, "CA1", "ringing", seqOf(2)))
	if result.To != "" {
		t.Fatalf("a ringing moved the commitment: %+v", result)
	}
	due, err := s.ClaimDueReceipts(context.Background(), outbound.FamilyCall, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("the wait that was due is no longer: %+v, %v", due, err)
	}
}

// The worker died with a request open, and an event about the call came in
// while it held it. The event was applied and is not coming again; recovery
// asks the same question Finalize would have.
func TestRecoveryHearsWhatCameInWhileTheWorkerWasGone(t *testing.T) {
	for _, tc := range []struct {
		status string
		to     outbound.Status
		row    string
	}{
		{"completed", outbound.StatusSucceeded, "T35s"},
		{"ringing", outbound.StatusAwaitingReceipt, "T39"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			s := setupTestDB(t)
			asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
			_, intentID := admitCall(t, s)
			token := claimCall(t, s, intentID)
			begun := beginOne(t, s, intentID, token)
			_ = token

			hear(t, s, callEvent(begun.AttemptID, "CA1", tc.status, seqOf(3)))
			expireLease(t, s, intentID)

			recovered, err := s.RecoverStaleAttempts(context.Background(), outbound.FamilyCall, 10)
			if err != nil || len(recovered) != 1 {
				t.Fatalf("recover: %+v, %v", recovered, err)
			}
			if recovered[0].To != tc.to || recovered[0].Row != tc.row {
				t.Fatalf("recovered to %s (%s), want %s (%s)", recovered[0].To, recovered[0].Row, tc.to, tc.row)
			}
		})
	}
}

// The first call ended in doubt; while the repeat was being placed, the first
// one was heard to have taken place. The repeat is then refused for good: the
// commitment is settled by the first call, not failed by the second.
func TestARefusedRepeatDoesNotUndoACallThatTookPlace(t *testing.T) {
	s := setupTestDB(t)
	asABuildThatKnows(t, s, "escalation", outbound.FamilyCall)
	_, intentID := admitCall(t, s)
	doubtful := placeCall(t, s, intentID, concluded(outbound.OutcomeAmbiguous, "timeout"))
	exec(t, s, `UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID)

	token := claimCall(t, s, intentID)
	repeat := beginOne(t, s, intentID, token)
	hear(t, s, callEvent(doubtful.AttemptID, "CA1", "completed", seqOf(3)))

	result := finalize(t, s, repeat.AttemptID, token, concluded(outbound.OutcomePermanentRejection, "invalid_number"))
	if result.To != outbound.StatusSucceeded || result.Row != "T35s" {
		t.Fatalf("a refused repeat after a call that took place: %s (%s)", result.To, result.Row)
	}
	if n := countOf(t, s, `SELECT count(*) FROM outbound_attempts
		WHERE id = $1 AND outcome = 'permanent_rejection'`, repeat.AttemptID); n != 1 {
		t.Fatal("the repeat's own answer was not kept as it was")
	}
}
