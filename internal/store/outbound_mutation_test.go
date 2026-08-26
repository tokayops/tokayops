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

// What a change to a message leaves behind, in the database rather than in the
// domain that decided it.
//
// The rules these prove are the ones with no second line of defence: coordinates
// written onto the wrong row, proof of a loss recorded by something that made no
// loss, an answer thrown away. Each of them is invisible at the moment it goes
// wrong and expensive later, which is why they are asserted here and not only
// where they are computed.

// changeableCard is a card that has been posted: a commitment with coordinates,
// a name for them, and a revision it has already applied.
func changeableCard(t *testing.T, s *Store, agID string) string {
	t.Helper()
	intentID := admitOne(t, s, agID, channelCommitment("C0001", 0))[0]

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
	}); err != nil {
		t.Fatalf("post the card: %v", err)
	}
	return intentID
}

// aim raises what the group's messages have to show, which is what puts a
// settled card back in the queue.
//
// Through a merge rather than an acknowledgement, and the difference matters
// further down: an operator may not create a NEW card for an alert somebody has
// already acknowledged, so a fixture that acknowledged would be testing that
// rule instead of the one it is about.
func aim(t *testing.T, s *Store, agID string) int64 {
	t.Helper()
	recordAlerts(t, s, agID, []model.Alert{{
		Fingerprint: "fp-2", Status: model.AlertStatusFiring,
		StartsAt: time.Unix(1700000600, 0),
		Labels:   map[string]string{"alertname": "DiskSlow"},
	}})
	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredMerge, Actor: "ingester",
	})
	if err != nil || result.Outcome != outbound.DesiredApplied {
		t.Fatalf("raise the desired state: %s (%v)", result.Outcome, err)
	}
	return result.Revision
}

// due makes a commitment claimable now, without waiting out the backoff its
// last failure earned.
func due(t *testing.T, s *Store, intentID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`,
		intentID); err != nil {
		t.Fatalf("make %s due: %v", intentID, err)
	}
}

func intentReceipt(t *testing.T, s *Store, intentID string) (string, string) {
	t.Helper()
	var receipt, ref string
	if err := s.db.QueryRow(
		`SELECT COALESCE(receipt::text, ''), COALESCE(receipt_ref, '')
		 FROM outbound_intents WHERE id = $1`, intentID).Scan(&receipt, &ref); err != nil {
		t.Fatalf("read the commitment's coordinates: %v", err)
	}
	return receipt, ref
}

func attemptReceipt(t *testing.T, s *Store, attemptID string) (string, bool) {
	t.Helper()
	var receipt string
	var recorded bool
	if err := s.db.QueryRow(
		`SELECT COALESCE(receipt::text, ''), receipt_recorded
		 FROM outbound_attempts WHERE id = $1`, attemptID).Scan(&receipt, &recorded); err != nil {
		t.Fatalf("read the attempt's receipt: %v", err)
	}
	return receipt, recorded
}

// TestAChangeAcceptedWithNothingToSayAppliesTheRevision is Telegram's answer to
// an edit that would leave the message as it is.
//
// It is an error code, and it means the outside already shows what we are
// asking for - which is the revision applied. Treating it as a failure would
// retry forever against a provider that will keep saying the same thing.
func TestAChangeAcceptedWithNothingToSayAppliesTheRevision(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	before, beforeRef := intentReceipt(t, s, intentID)
	revision := aim(t, s, agID)

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if begun.AttemptKind != outbound.AttemptMutation {
		t.Fatalf("the call was planned as %s", begun.AttemptKind)
	}

	// Accepted, with nothing handed back - which is what an edit that changed
	// nothing answers.
	result, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: mutationAccepted(t, begun.ReceiptRef, outbound.Receipt{}),
	})
	if err != nil {
		t.Fatalf("finalize the change: %v", err)
	}
	if result.To != outbound.StatusIdle {
		t.Fatalf("the card settled as %s", result.To)
	}

	var applied int64
	if err := s.db.QueryRow(
		`SELECT applied_revision FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&applied); err != nil {
		t.Fatalf("read the applied revision: %v", err)
	}
	if applied != revision {
		t.Fatalf("the card is at revision %d, and %d was applied", applied, revision)
	}

	// The coordinates are untouched: the change did not make that message.
	if after, afterRef := intentReceipt(t, s, intentID); after != before || afterRef != beforeRef {
		t.Fatalf("the change rewrote the commitment's coordinates: %s/%s -> %s/%s",
			before, beforeRef, after, afterRef)
	}
	// And the attempt says honestly that it got nothing back.
	if receipt, recorded := attemptReceipt(t, s, begun.AttemptID); recorded || receipt != "" {
		t.Fatalf("the attempt claims coordinates it never received: %q", receipt)
	}
}

// TestAChangeKeepsWhatTheProviderAnsweredWithoutMovingTheCard covers the two
// answers that DO carry coordinates: the ordinary one that repeats them back,
// and the one that names a different message.
//
// Both are kept on the attempt, because both are what the provider said.
// Neither reaches the commitment: a change was aimed at coordinates rather than
// having produced them, and following an answer about another message would
// move somebody's card.
func TestAChangeKeepsWhatTheProviderAnsweredWithoutMovingTheCard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answered string
		outcome  outbound.Outcome
		settles  outbound.Status
	}{
		{
			name: "the same message", answered: "",
			outcome: outbound.OutcomeAccepted, settles: outbound.StatusIdle,
		},
		{
			name: "a different message", answered: "C_OTHER/999.999",
			// A channel that has lost track of what it was asked to do. The
			// domain calls it doubt rather than following it.
			outcome: outbound.OutcomeAmbiguous, settles: outbound.StatusPending,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")

			agID := desiredGroup(t, s, "Disk filling up")
			intentID := changeableCard(t, s, agID)
			before, beforeRef := intentReceipt(t, s, intentID)
			aim(t, s, agID)

			token := claimOne(t, s, intentID)
			begun := beginOne(t, s, intentID, token)

			answered := begun.ReceiptRef
			if tc.answered != "" {
				answered = tc.answered
			}
			handed := receiptOf(answered, `{"channel_id":"C_ANSWERED","timestamp":"999.999"}`)

			conclusion := mutationAccepted(t, begun.ReceiptRef, handed)
			if tc.outcome == outbound.OutcomeAmbiguous {
				conclusion = mutationRetargeted(t, answered, handed)
			}
			if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
				AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: conclusion,
			}); err != nil {
				t.Fatalf("finalize the change: %v", err)
			}

			if got := statusOf(t, s, intentID); got != tc.settles {
				t.Fatalf("the card is %s, want %s", got, tc.settles)
			}
			// What the provider said is on the attempt, whichever answer it was.
			if receipt, recorded := attemptReceipt(t, s, begun.AttemptID); !recorded ||
				!strings.Contains(receipt, "C_ANSWERED") {
				t.Fatalf("the answer was not kept: %q (recorded=%v)", receipt, recorded)
			}
			// And the card is where it was.
			if after, afterRef := intentReceipt(t, s, intentID); after != before ||
				afterRef != beforeRef {
				t.Fatalf("the answer moved the card: %s/%s -> %s/%s",
					before, beforeRef, after, afterRef)
			}
		})
	}
}

// TestProofThatAMessageIsGoneIsRecordedAndReadBack. It exists for one moment -
// the attempt - and is what an operator's permission to make a second message
// rests on weeks later.
func TestProofThatAMessageIsGoneIsRecordedAndReadBack(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: messageGone(t, begun.ReceiptRef),
	}); err != nil {
		t.Fatalf("finalize the change: %v", err)
	}

	// On the row, and in the journal a person reads. Without the second, the
	// same fact would be visible or not depending on whether recovery happened
	// to close the attempt first.
	var stored string
	if err := s.db.QueryRow(
		`SELECT COALESCE(provider_result_detail, '') FROM outbound_attempts WHERE id = $1`,
		begun.AttemptID).Scan(&stored); err != nil {
		t.Fatalf("read the attempt: %v", err)
	}
	if stored != string(keys.DetailDefinitelyAbsent) {
		t.Fatalf("the attempt records %q", stored)
	}

	journal, err := s.IntentJournal(context.Background(), intentID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	var seen bool
	for _, record := range journal.Attempts {
		if record.ResultDetail == string(keys.DetailDefinitelyAbsent) {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the journal does not say what became of the message")
	}

	// And it is what lets an operator make a second one.
	result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
		Actor: "nina", Reason: "the card is gone",
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.Outcome != outbound.ResolveResolved {
		t.Fatalf("the decision came back %s", result.Outcome)
	}
}

// TestAChangeThatFailedForItsOwnReasonsDoesNotLicenseASecondCard is the other
// side of the same door, and the one an operator meets far more often.
//
// A change can fail permanently without the message being gone - the token lost
// its rights, the channel stopped accepting edits. The card is still there. A
// second one would sit beside it, and nothing in the product would ever remove
// the first, so the decision is refused however firmly the person asking says
// they accept the consequences: the proof is a fact in the journal and there is
// no way to assert it from outside.
func TestAChangeThatFailedForItsOwnReasonsDoesNotLicenseASecondCard(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "cant_update_message"),
	}); err != nil {
		t.Fatalf("finalize the change: %v", err)
	}
	// Permanent, and silent about the message: the column stays empty.
	var detail string
	if err := s.db.QueryRow(
		`SELECT COALESCE(provider_result_detail, '') FROM outbound_attempts WHERE id = $1`,
		begun.AttemptID).Scan(&detail); err != nil {
		t.Fatalf("read the attempt: %v", err)
	}
	if detail != "" {
		t.Fatalf("a failure that proved nothing recorded %q", detail)
	}

	result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
		AcceptedDuplicateRisk: true, Actor: "nina", Reason: "just make a new one",
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.Outcome != outbound.ResolveInvalidDecision {
		t.Fatalf("a second card was allowed beside one that still exists: %s",
			result.Outcome)
	}
}

// TestOnlyAChangeCanProveItInTheDatabaseToo. The domain refuses the pair; this
// is the same rule where the row is written, so that neither half depends on
// the other having held.
func TestOnlyAChangeCanProveItInTheDatabaseToo(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := admitOne(t, s, agID, channelCommitment("C0001", 0))[0]
	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)

	// A create claiming the message is gone would licence a duplicate of what
	// it had just made.
	//
	// It names no object, deliberately: a create that named one without
	// recording it is refused by a different rule, and this test is about the
	// claim rather than about the shape.
	absent := keys.DetailDefinitelyAbsent
	_, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: conclusion(outbound.ConclusionInput{
			Outcome: outbound.OutcomePermanentRejection, Class: "message_not_found",
			Status: "message_not_found", Detail: &absent,
			Summary: "the message is not there any more",
		}),
	})
	if err == nil {
		t.Fatal("a create was allowed to prove the message it made is gone")
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusSending {
		t.Fatalf("the refused result moved the commitment to %s", got)
	}
}

// TestProofFromALateAnswerSurvivesTheAttemptsAfterIt is the interleaving the
// narrow reading of the journal would miss.
//
// Recovery closes an attempt as doubtful; the answer arrives afterwards and is
// kept beside it. The next attempt fails for an unrelated reason and says
// nothing about the message. Reading only the last attempt would find no proof
// and refuse a second message that is genuinely needed.
func TestProofFromALateAnswerSurvivesTheAttemptsAfterIt(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)

	// A change whose worker vanished, closed by recovery as doubtful.
	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	expireLease(t, s, intentID)
	if _, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// Its answer arrives after that, and is kept as an observation.
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: messageGone(t, begun.ReceiptRef),
	}); err != nil {
		t.Fatalf("the late answer: %v", err)
	}
	var observed string
	if err := s.db.QueryRow(`
		SELECT COALESCE(o.provider_result_detail, '')
		FROM outbound_attempt_observations o WHERE o.attempt_id = $1`, begun.AttemptID).
		Scan(&observed); err != nil {
		t.Fatalf("read the observation: %v", err)
	}
	if observed != string(keys.DetailDefinitelyAbsent) {
		t.Fatalf("the late answer was kept as %q", observed)
	}

	// A later attempt fails for a reason that says nothing about the message.
	due(t, s, intentID)
	later := claimOne(t, s, intentID)
	next := beginOne(t, s, intentID, later)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: next.AttemptID, LeaseToken: later,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "invalid_auth"),
	}); err != nil {
		t.Fatalf("finalize the later attempt: %v", err)
	}

	// The proof still stands: it is a fact about the generation, and nothing in
	// this build can put the message back.
	//
	// The risk is accepted explicitly, and it has to be: this generation holds
	// an attempt nobody knows the fate of, so a second message might turn out
	// to be a duplicate. That is a separate rule from the proof, and this test
	// is about the proof - which is why the decision is refused without the
	// flag and allowed with it.
	refused, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
		Actor: "nina", Reason: "the card is gone",
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if refused.Outcome != outbound.ResolveInvalidDecision {
		t.Fatalf("a duplicate was allowed without anybody accepting the risk: %s",
			refused.Outcome)
	}

	result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
		AcceptedDuplicateRisk: true, Actor: "nina", Reason: "the card is gone",
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if result.Outcome != outbound.ResolveResolved {
		t.Fatalf("proof from a late answer was not enough: %s", result.Outcome)
	}
}

// The conclusions a change produces, built through the domain so a test cannot
// express a result production could not.

// mutationAccepted is a change the provider took: the object it was applied to
// is named, and whatever came back - often nothing - is what it says it is.
func mutationAccepted(t *testing.T, effectRef string, handed outbound.Receipt) outbound.Conclusion {
	t.Helper()
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomeAccepted, Status: "ok",
		ReceiptRef: effectRef, Receipt: handed, Summary: "the message was updated",
	})
}

// mutationRetargeted is a channel answering about a message it was not asked
// about: doubt, named by what actually came back, with the answer kept.
func mutationRetargeted(t *testing.T, answeredRef string, handed outbound.Receipt) outbound.Conclusion {
	t.Helper()
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomeAmbiguous, Class: "mutation_retargeted", Status: "ok",
		ReceiptRef: answeredRef, Receipt: handed,
		Summary: "the provider answered about another message",
	})
}

// messageGone is the one answer that proves anything about the object.
func messageGone(t *testing.T, effectRef string) outbound.Conclusion {
	t.Helper()
	absent := keys.DetailDefinitelyAbsent
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomePermanentRejection, Class: "message_not_found",
		Status: "message_not_found", ReceiptRef: effectRef, Detail: &absent,
		Summary: "the message is not there any more",
	})
}
