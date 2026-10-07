package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The call channel through the store: where it is bound, what nobody to reach
// does, what a refusal of one integration does, and how soon a call is tried
// again.

var (
	viaRussia = outbound.BoundContext{IntegrationID: "tw-ru", AccountScope: "AC-ru", FromNumber: "+79990000001"}
	viaWorld  = outbound.BoundContext{IntegrationID: "tw-world", AccountScope: "AC-world", FromNumber: "+15005550002"}
)

func beginCall(t *testing.T, s *Store, intentID, token string, via outbound.BoundContext) outbound.BeginAttemptResult {
	t.Helper()
	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "+79161234567", BoundContext: via,
	})
	if err != nil || result.Outcome != outbound.BeginStarted {
		t.Fatalf("begin: %+v, %v", result, err)
	}
	return result
}

func attemptBoundTo(t *testing.T, s *Store, attemptID string) outbound.BoundContext {
	t.Helper()
	bound, found, err := s.AttemptBinding(context.Background(), attemptID)
	if err != nil || !found {
		t.Fatalf("binding of %s: %v, %v", attemptID, found, err)
	}
	return bound
}

func dueIn(t *testing.T, s *Store, intentID string) time.Duration {
	t.Helper()
	var seconds float64
	if err := s.db.QueryRow(`SELECT EXTRACT(EPOCH FROM next_attempt_at - now()) FROM outbound_intents WHERE id = $1`,
		intentID).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	return time.Duration(seconds * float64(time.Second))
}

// The integration, account and sender number are the generation's: a repeat of
// a request that may have placed the call goes the same way, whatever the
// channel would choose now.
func TestACallIsBoundToItsIntegrationForTheGeneration(t *testing.T) {
	s := setupTestDB(t)
	_, intentID := admitCall(t, s)

	first := beginCall(t, s, intentID, claimCall(t, s, intentID), viaRussia)
	finalize(t, s, first.AttemptID, mustToken(t, s, intentID), concluded(outbound.OutcomeAmbiguous, "timeout"))
	exec(t, s, `UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID)

	repeat := beginCall(t, s, intentID, claimCall(t, s, intentID), viaWorld)
	if got := attemptBoundTo(t, s, repeat.AttemptID); got != viaRussia {
		t.Fatalf("the repeat went through %+v, not the generation's %+v", got, viaRussia)
	}
	if got := repeat.BoundContext; got != viaRussia {
		t.Fatalf("the channel was told %+v", got)
	}
}

// mustToken is the lease token a commitment holds now.
func mustToken(t *testing.T, s *Store, intentID string) string {
	t.Helper()
	var token string
	if err := s.db.QueryRow(`SELECT lease_token FROM outbound_intents WHERE id = $1`, intentID).Scan(&token); err != nil {
		t.Fatal(err)
	}
	return token
}

// Nobody to reach ends the call without a failure, and the escalation behind it
// goes on even where the step said to stop on failure.
func TestNobodyToCallDoesNotStopTheEscalation(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	call := dmCommitment("U0001")
	call.Provider, call.CompletionMode, call.StopOnFailure = keys.ProviderPhone, keys.CompletionOnProviderReceipt, true
	next := dmCommitment("U0002")
	next.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: 2}
	next.Timing = keys.TimingSpec{Kind: keys.TimingRelativeToAdmission, Offset: time.Hour}
	ids := admitOne(t, s, agID, call, next)
	callID, nextID := ids[0], ids[1]
	if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND provider = 'phone'`, callID); n == 0 {
		callID, nextID = nextID, callID
	}

	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: callID, LeaseToken: claimCall(t, s, callID), WorkerID: "worker-1",
		Preparation: outbound.PreparationNoContact, ErrorClass: "no_number", Summary: "U0001 has no phone number",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if result.Outcome == outbound.BeginStarted {
		t.Fatal("a call with nobody to reach was started")
	}
	if got := statusOf(t, s, callID); got != outbound.StatusNoContact {
		t.Fatalf("the call is %s, want no_contact", got)
	}
	if got := statusOf(t, s, nextID); got != outbound.StatusPending {
		t.Fatalf("the next step is %s: nobody to call stopped the escalation", got)
	}
}

// One integration refused, and nothing of the generation is in doubt: the next
// integration, in a new generation, and the refusing one is remembered.
func TestARefusedCallMovesToTheNextIntegration(t *testing.T) {
	s := setupTestDB(t)
	_, intentID := admitCall(t, s)
	token := claimCall(t, s, intentID)
	begun := beginCall(t, s, intentID, token, viaRussia)

	result := finalize(t, s, begun.AttemptID, token, concluded(outbound.OutcomePermanentRejection, "twilio_21211"))
	if result.To != outbound.StatusPending || result.Row != "T10r" {
		t.Fatalf("a refused call: %s (%s)", result.To, result.Row)
	}
	if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND generation_no = 1`, intentID); n != 1 {
		t.Fatal("no new generation")
	}
	refused, err := s.RefusedIntegrations(context.Background(), intentID)
	if err != nil || len(refused) != 1 || refused[0] != "tw-ru" {
		t.Fatalf("refused = %v, %v", refused, err)
	}
}

// The first call ended in doubt and the repeat was refused. The refusal proves
// only that the repeat placed nothing; the first call may be ringing. The
// commitment waits, through the same integration, until that is known.
func TestARefusedRepeatWaitsForTheDoubtfulFirstCall(t *testing.T) {
	s := setupTestDB(t)
	_, intentID := admitCall(t, s)
	first := beginCall(t, s, intentID, claimCall(t, s, intentID), viaRussia)
	finalize(t, s, first.AttemptID, mustToken(t, s, intentID), concluded(outbound.OutcomeAmbiguous, "timeout"))
	exec(t, s, `UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID)

	token := claimCall(t, s, intentID)
	repeat := beginCall(t, s, intentID, token, viaRussia)
	result := finalize(t, s, repeat.AttemptID, token, concluded(outbound.OutcomePermanentRejection, "twilio_21211"))
	if result.To != outbound.StatusAwaitingReceipt || result.Row != "T10w" {
		t.Fatalf("a refused repeat after a doubtful call: %s (%s)", result.To, result.Row)
	}
	if n := countOf(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND generation_no = 0
		AND NOT receipt_recorded`, intentID); n != 1 {
		t.Fatal("the wait moved to another generation, or invented a receipt")
	}

	// Then the first call is heard of: it was never placed. Only now does the
	// commitment move to the next integration.
	moved := hear(t, s, callEvent(first.AttemptID, "CA1", "failed", seqOf(4)))
	if moved.To != outbound.StatusPending || moved.Row != "T36" {
		t.Fatalf("after the first call was not placed: %+v", moved)
	}
}

// A wait with nothing to ask about - the doubtful call never named itself -
// ends at its deadline, like any other.
func TestAWaitWithNoKnownCallEndsAtTheDeadline(t *testing.T) {
	s := setupTestDB(t)
	_, intentID := admitCall(t, s)
	first := beginCall(t, s, intentID, claimCall(t, s, intentID), viaRussia)
	finalize(t, s, first.AttemptID, mustToken(t, s, intentID), concluded(outbound.OutcomeAmbiguous, "timeout"))
	exec(t, s, `UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID)
	token := claimCall(t, s, intentID)
	repeat := beginCall(t, s, intentID, token, viaRussia)
	finalize(t, s, repeat.AttemptID, token, concluded(outbound.OutcomePermanentRejection, "twilio_21211"))

	exec(t, s, `UPDATE outbound_intents SET receipt_deadline = now() - interval '1 second' WHERE id = $1`, intentID)
	result, err := s.ReviewReceiptWait(context.Background(), intentID)
	if err != nil || result.To != outbound.StatusSucceeded || result.Row != "T37/S1" {
		t.Fatalf("a wait with nothing known, past its deadline: %+v, %v", result, err)
	}
}

// A call is not tried again sooner than a minute - after a doubtful request, a
// worker that died, or a channel that was not ready - and a direct message
// keeps the family's curve.
func TestACallIsNotRepeatedWithinAMinute(t *testing.T) {
	s := setupTestDB(t)
	floor := outbound.PhoneRetryFloor - time.Second

	t.Run("after a doubtful request", func(t *testing.T) {
		_, intentID := admitCall(t, s)
		begun := beginCall(t, s, intentID, claimCall(t, s, intentID), viaRussia)
		finalize(t, s, begun.AttemptID, mustToken(t, s, intentID), concluded(outbound.OutcomeAmbiguous, "timeout"))
		if due := dueIn(t, s, intentID); due < floor {
			t.Fatalf("repeated in %s", due)
		}
	})
	t.Run("after a worker died", func(t *testing.T) {
		_, intentID := admitCall(t, s)
		beginCall(t, s, intentID, claimCall(t, s, intentID), viaRussia)
		expireLease(t, s, intentID)
		if _, err := s.RecoverStaleAttempts(context.Background(), outbound.FamilyNotification, 10); err != nil {
			t.Fatal(err)
		}
		if due := dueIn(t, s, intentID); due < floor {
			t.Fatalf("repeated in %s", due)
		}
	})
	t.Run("after a channel that was not ready", func(t *testing.T) {
		_, intentID := admitCall(t, s)
		if _, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: claimCall(t, s, intentID), WorkerID: "worker-1",
			Preparation: outbound.PreparationTransient, ErrorClass: "contact_lookup_failed",
		}); err != nil {
			t.Fatal(err)
		}
		if due := dueIn(t, s, intentID); due < floor {
			t.Fatalf("repeated in %s", due)
		}
	})
	t.Run("a direct message keeps its curve", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0003"))[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)
		finalize(t, s, begun.AttemptID, token, concluded(outbound.OutcomeAmbiguous, "timeout"))
		if due := dueIn(t, s, intentID); due >= floor {
			t.Fatalf("a direct message waits %s", due)
		}
	})
}

// The binding of an attempt is read back as the domain stores it.
func TestAnAttemptKeepsWhatItWasBoundTo(t *testing.T) {
	s := setupTestDB(t)
	_, intentID := admitCall(t, s)
	begun := beginCall(t, s, intentID, claimCall(t, s, intentID), viaRussia)
	var raw []byte
	if err := s.db.QueryRow(`SELECT bound_context FROM outbound_attempts WHERE id = $1`, begun.AttemptID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored outbound.BoundContext
	if err := json.Unmarshal(raw, &stored); err != nil || stored != viaRussia {
		t.Fatalf("stored %s", raw)
	}
	if _, found, _ := s.AttemptBinding(context.Background(), "no-such-attempt"); found {
		t.Fatal("an attempt nobody made has a binding")
	}
}

// The completion mode is the provider's: a call waits for the provider's word
// and is one-shot, and nothing else waits. The other way round is a commitment
// that can never be completed, refused at admission.
func TestTheAdmissionHoldsEachProviderToItsCompletion(t *testing.T) {
	s := setupTestDB(t)
	for _, tc := range []struct {
		name   string
		adjust func(*keys.EscalationCommitment)
		ok     bool
	}{
		{"a call waiting for the provider's word", func(c *keys.EscalationCommitment) {
			c.Provider, c.CompletionMode = keys.ProviderPhone, keys.CompletionOnProviderReceipt
		}, true},
		{"a call settled by its acceptance", func(c *keys.EscalationCommitment) {
			c.Provider = keys.ProviderPhone
		}, false},
		{"a call that can be edited", func(c *keys.EscalationCommitment) {
			c.Provider, c.CompletionMode, c.Editable = keys.ProviderPhone, keys.CompletionOnProviderReceipt, true
		}, false},
		{"a direct message waiting for a word Slack never sends", func(c *keys.EscalationCommitment) {
			c.CompletionMode = keys.CompletionOnProviderReceipt
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dmCommitment("U0001")
			tc.adjust(&c)
			_, err := s.SubmitBatch(context.Background(), outboundAdmission(t, outboundGroup(t, s), "x", c))
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, outbound.ErrNotAdmissible) {
				t.Fatalf("admitted, or refused for another reason: %v", err)
			}
		})
	}
}
