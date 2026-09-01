package outbound

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Classification is where a wrong answer becomes either a duplicate page or a
// lost one, so the two rules the domain keeps for itself are the two where the
// cost is asymmetric: what a silent transport means, and what an answer nobody
// recognises means.

// wilfulHandler is a channel that answers however the test tells it to,
// including in ways the contract forbids. Every one of these is a mistake a new
// channel could plausibly make, and the point of the fold is that none of them
// can turn into a delivered page or a discarded one.
type wilfulHandler struct {
	outcome Outcome
	class   string
	known   bool
	detail  *keys.ProviderResultDetail
	asked   int
}

func (h *wilfulHandler) ExecuteAttempt(context.Context, Call) (Result, error) {
	return Result{}, nil
}

func (h *wilfulHandler) ClassifyResponse(Result) (Classification, bool) {
	h.asked++
	return Classification{Outcome: h.outcome, Class: h.class, Detail: h.detail}, h.known
}

// TestSilenceIsNotTheHandlersToClassify. A request that never left can be
// retried for free. A request that may have gone out cannot: calling that a
// failure asserts an absence nobody established, and the retry becomes a second
// message with nothing in the record to explain it.
//
// Neither depends on the provider, so neither is asked of it - the whole reason
// this fold exists is that a channel could get them wrong and nobody reviewing
// that channel would see a page go missing.
func TestSilenceIsNotTheHandlersToClassify(t *testing.T) {
	cases := []struct {
		evidence Evidence
		want     Outcome
	}{
		{DefinitelyNotSent, OutcomeRetryableRejection},
		{PossiblySent, OutcomeAmbiguous},
	}

	for _, tc := range cases {
		t.Run(string(tc.evidence), func(t *testing.T) {
			// The handler tries to say the opposite of the rule.
			handler := &wilfulHandler{outcome: OutcomePermanentRejection, known: true}
			if tc.want == OutcomePermanentRejection {
				handler.outcome = OutcomeAccepted
			}

			concluded, breach := Conclude(handler, createCall(), Result{Evidence: tc.evidence}, nil)
			if concluded.Outcome() != tc.want {
				t.Fatalf("%s was concluded as %q, want %q",
					tc.evidence, concluded.Outcome(), tc.want)
			}
			if handler.asked != 0 {
				t.Fatal("the provider was asked what its own silence meant")
			}
			if breach != BreachNone {
				t.Fatalf("a well behaved silence was reported as %q", breach)
			}
		})
	}
}

// TestAnAnswerNobodyRecognisesIsDoubt is the default rule, and it outranks
// every line of the capability matrix: a rejection may only be declared where
// the provider's documentation proves the effect did not happen.
func TestAnAnswerNobodyRecognisesIsDoubt(t *testing.T) {
	for _, status := range []string{"", "teapot", "some_new_code_slack_added"} {
		// A handler that does not recognise a status says so rather than
		// guessing, and what it half-said is discarded.
		handler := &wilfulHandler{outcome: OutcomePermanentRejection, class: "guessed"}
		concluded, breach := Conclude(handler, createCall(), Result{Evidence: ProviderResponse, Status: status}, nil)

		if concluded.Outcome() != OutcomeAmbiguous {
			t.Fatalf("status %q was concluded as %q; an unrecognised answer is doubt",
				status, concluded.Outcome())
		}
		class := concluded.Completion().ErrorClass
		if class == nil || *class == "guessed" {
			t.Fatalf("status %q kept the guess the handler did not stand behind", status)
		}
		if breach != BreachNone {
			t.Fatalf("a handler admitting it does not know is not a breach: %q", breach)
		}
	}
}

// TestAHandlerCannotInventAnOutcome. The outcomes are a closed set and a
// withdrawal is not a provider's to declare - somebody changing their mind is
// not something Slack can observe.
func TestAHandlerCannotInventAnOutcome(t *testing.T) {
	for _, outcome := range []Outcome{"delivered_probably", OutcomeCanceled, ""} {
		handler := &wilfulHandler{outcome: outcome, class: "whatever", known: true}
		concluded, breach := Conclude(handler, createCall(), Result{Evidence: ProviderResponse, Status: "ok"}, nil)

		if concluded.Outcome() != OutcomeAmbiguous {
			t.Fatalf("a handler answering %q produced %q", outcome, concluded.Outcome())
		}
		if breach != BreachUnknownOutcome {
			t.Fatalf("a handler answering %q was reported as %q", outcome, breach)
		}
	}
}

// TestAHandlerThatWillNotSayWhatItProved gets the safe answer and is reported.
// A result with no evidence has told us nothing about the message, and nothing
// is doubt.
func TestAHandlerThatWillNotSayWhatItProved(t *testing.T) {
	handler := &wilfulHandler{outcome: OutcomeAccepted, known: true}
	concluded, breach := Conclude(handler, createCall(), Result{Status: "ok"}, nil)

	if concluded.Outcome() != OutcomeAmbiguous {
		t.Fatalf("a result with no evidence was concluded as %q", concluded.Outcome())
	}
	if breach != BreachUnknownEvidence {
		t.Fatalf("it was reported as %q", breach)
	}
	if handler.asked != 0 {
		t.Fatal("a result with no evidence was still handed to the provider")
	}
}

// TestAnAcceptanceHasToSayWhatItMade. "Yes, and I will not tell you where" is
// not a delivery: nothing can find, update or resolve that message afterwards.
// It is recorded as doubt, and the provider's breach is reported rather than
// absorbed.
func TestAnAcceptanceHasToSayWhatItMade(t *testing.T) {
	accepting := &wilfulHandler{outcome: OutcomeAccepted, known: true}

	bare, breach := Conclude(accepting, createCall(), Result{Evidence: ProviderResponse, Status: "ok"}, nil)
	if breach != BreachAcceptanceWithoutReceipt {
		t.Fatalf("an acceptance with no receipt was reported as %q", breach)
	}
	if bare.Outcome() != OutcomeAmbiguous {
		t.Fatalf("it was recorded as %q", bare.Outcome())
	}
	if bare.Completion().ErrorClass == nil || *bare.Completion().ErrorClass == "" {
		t.Fatal("the breach was recorded without saying what it was")
	}

	receipt, err := NewReceipt("C0001/1700000000.000100",
		json.RawMessage(`{"channel":"C0001","ts":"1700000000.000100"}`))
	if err != nil {
		t.Fatalf("build a receipt: %v", err)
	}
	whole, breach := Conclude(accepting, createCall(), Result{
		Evidence: ProviderResponse, Status: "ok", Receipt: receipt,
	}, nil)
	if breach != BreachNone {
		t.Fatalf("a complete acceptance was reported as %q", breach)
	}
	if whole.Outcome() != OutcomeAccepted {
		t.Fatalf("a complete acceptance became %q", whole.Outcome())
	}
	ref := whole.Completion().ReceiptRef
	if ref == nil || *ref != "C0001/1700000000.000100" {
		t.Fatalf("the coordinates were lost: %+v", ref)
	}
	if len(whole.Receipt()) == 0 {
		t.Fatal("the acceptance travels without the object it made")
	}

	// And the conclusion never names a revision. That belongs to the attempt,
	// and a handler that could set it could file its answer against a message
	// it never sent.
	if whole.Completion().AppliedRevision != nil {
		t.Fatal("a provider's answer named the revision it applied")
	}
}

// TestAReceiptIsWholeOrNothing. Half a receipt is worse than none: a reference
// with nothing stored leaves the commitment looking unsent, and the next
// revision would create a SECOND message beside the one that exists.
func TestAReceiptIsWholeOrNothing(t *testing.T) {
	cases := map[string]struct {
		ref string
		raw json.RawMessage
	}{
		"nothing to call it by":      {"", json.RawMessage(`{"channel":"C0001"}`)},
		"nothing recorded":           {"C0001/1700000000.000100", nil},
		"an empty body":              {"C0001/1700000000.000100", json.RawMessage("")},
		"something that is not JSON": {"C0001/1700000000.000100", json.RawMessage("{oops")},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			receipt, err := NewReceipt(tc.ref, tc.raw)
			if err == nil {
				t.Fatalf("half a receipt was accepted: %+v", receipt)
			}
			if receipt.Recorded() {
				t.Fatal("the refused receipt still says it recorded something")
			}
		})
	}

	// And what a handler cannot build, it cannot smuggle past the fold: an
	// acceptance carrying the zero value is doubt.
	concluded, breach := Conclude(&wilfulHandler{outcome: OutcomeAccepted, known: true}, createCall(), Result{Evidence: ProviderResponse, Status: "ok", Receipt: Receipt{}}, nil)
	if concluded.Outcome() != OutcomeAmbiguous || breach != BreachAcceptanceWithoutReceipt {
		t.Fatalf("an acceptance with an empty receipt concluded %q (%q)",
			concluded.Outcome(), breach)
	}
}

// TestAConclusionCarriesWhatTheProviderSaid keeps the journal answerable: the
// status and the reason are what somebody reads months later, and a conclusion
// that dropped them would leave "ambiguous" with no story.
func TestAConclusionCarriesWhatTheProviderSaid(t *testing.T) {
	handler := &wilfulHandler{
		outcome: OutcomeRetryableRejection, class: "rate_limited", known: true,
	}
	concluded, _ := Conclude(handler, createCall(), Result{
		Evidence: ProviderResponse, Status: "ratelimited",
	}, nil)
	completion := concluded.Completion()

	if completion.ProviderStatus == nil || *completion.ProviderStatus != "ratelimited" {
		t.Fatalf("the provider's own code was dropped: %+v", completion.ProviderStatus)
	}
	if completion.ErrorClass == nil || *completion.ErrorClass != "rate_limited" {
		t.Fatalf("the reason was dropped: %+v", completion.ErrorClass)
	}
	if _, err := completion.Fingerprint(keys.CurrentCompletionFingerprintVersion()); err != nil {
		t.Fatalf("the conclusion cannot be fingerprinted: %v", err)
	}
}

// TestAConclusionCannotNameARevision. Which revision a message carried is the
// attempt's own record, and the store fills it in from there. Nothing a caller
// can build says otherwise - this is the test that keeps that true, because the
// store's refusal of one that did is now a boundary check nobody can reach.
func TestAConclusionCannotNameARevision(t *testing.T) {
	inputs := []ConclusionInput{
		{Outcome: OutcomeRetryableRejection, Class: "rate_limited"},
		{Outcome: OutcomeAmbiguous, Class: "no_response"},
		{Outcome: OutcomePermanentRejection, Class: "channel_not_found", Status: "x"},
		{Outcome: OutcomeAccepted, Receipt: mustReceipt("C1/1", `{"channel":"C1"}`)},
	}

	for _, in := range inputs {
		concluded, err := NewConclusion(in)
		if err != nil {
			t.Fatalf("build a conclusion: %v", err)
		}
		if concluded.Completion().AppliedRevision != nil {
			t.Fatalf("a conclusion of %q named a revision", in.Outcome)
		}
		// And the two halves of a receipt always agree, which is the other
		// state the store would have had to refuse.
		named := concluded.Completion().ReceiptRef != nil
		if stored := len(concluded.Receipt()) > 0; named != stored {
			t.Fatalf("a conclusion of %q names an object it did not record, "+
				"or records one it will not name", in.Outcome)
		}
	}
}

// TestAnOutcomeNobodyDeclaredIsRefused: the constructor is the boundary for
// callers that are not a provider handler at all - a test, an operator tool,
// whatever comes next.
func TestAnOutcomeNobodyDeclaredIsRefused(t *testing.T) {
	for _, outcome := range []Outcome{"", "delivered_probably", OutcomeCanceled} {
		if _, err := NewConclusion(ConclusionInput{Outcome: outcome}); err == nil {
			t.Fatalf("%q was accepted as something an attempt concluded", outcome)
		}
	}
	if _, err := NewConclusion(ConclusionInput{Outcome: OutcomeAccepted}); err == nil {
		t.Fatal("an acceptance with no receipt was accepted")
	}
}

// TestAReceiptDoesNotChangeUnderTheJournal. The bytes come from a buffer the
// handler decoded from, and the message they name is the one thing nothing else
// in the system can reconstruct.
func TestAReceiptDoesNotChangeUnderTheJournal(t *testing.T) {
	raw := []byte(`{"channel":"C0001","ts":"1700000000.000100"}`)
	receipt, err := NewReceipt("C0001/1700000000.000100", raw)
	if err != nil {
		t.Fatalf("build a receipt: %v", err)
	}

	kept := string(receipt.Raw())
	for i := range raw {
		raw[i] = 'x'
	}
	if got := string(receipt.Raw()); got != kept {
		t.Fatalf("the receipt changed with the buffer it came from: %s", got)
	}

	// And what comes out cannot be edited into what is held either.
	out := receipt.Raw()
	for i := range out {
		out[i] = 'y'
	}
	if got := string(receipt.Raw()); got != kept {
		t.Fatalf("the receipt changed under whoever read it: %s", got)
	}
}

// TestAConclusionCannotBeEditedFromOutside. The completion is a struct of
// pointers, so handing one out hands out its insides too - and a caller who
// emptied the receipt reference would leave the store fingerprinting a message
// under a name it was never accepted as, beside a receipt that still holds the
// real one.
func TestAConclusionCannotBeEditedFromOutside(t *testing.T) {
	concluded, err := NewConclusion(ConclusionInput{
		Outcome: OutcomeAccepted,
		Class:   "none",
		Status:  "ok",
		Receipt: mustReceipt("C0001/1700000000.000100", `{"channel":"C0001"}`),
		Summary: "posted",
	})
	if err != nil {
		t.Fatalf("build a conclusion: %v", err)
	}

	handed := concluded.Completion()
	*handed.ReceiptRef = ""
	*handed.ErrorClass = "edited"
	*handed.ProviderStatus = "edited"

	after := concluded.Completion()
	if after.ReceiptRef == nil || *after.ReceiptRef != "C0001/1700000000.000100" {
		t.Fatalf("the conclusion now names %v", after.ReceiptRef)
	}
	if *after.ErrorClass != "none" || *after.ProviderStatus != "ok" {
		t.Fatalf("the conclusion was edited through what it handed out: %+v", after)
	}

	// Two readers do not share insides either.
	first, second := concluded.Completion(), concluded.Completion()
	if first.ReceiptRef == second.ReceiptRef {
		t.Fatal("two readers were given the same pointer")
	}
}

// TestASummaryIsTruncatedWhereverItComesFrom. The limit is the journal's, not
// one caller's, so it lives in the one door into a conclusion.
func TestASummaryIsTruncatedWhereverItComesFrom(t *testing.T) {
	long := strings.Repeat("é", SummaryLimit*2)

	direct, err := NewConclusion(ConclusionInput{
		Outcome: OutcomeAmbiguous, Class: "no_response", Summary: long,
	})
	if err != nil {
		t.Fatalf("build a conclusion: %v", err)
	}
	if runes := []rune(direct.Summary()); len(runes) > SummaryLimit+3 {
		t.Fatalf("a summary of %d runes was kept whole", len(runes))
	}

	folded, _ := Conclude(&wilfulHandler{}, createCall(), Result{
		Evidence: PossiblySent, Summary: long,
	}, nil)
	if runes := []rune(folded.Summary()); len(runes) > SummaryLimit+3 {
		t.Fatalf("the fold kept %d runes", len(runes))
	}
}

// createCall is what most of these are about: making a message, with nothing
// out there yet.
func createCall() Call {
	return Call{AttemptKind: AttemptCreate, Operation: OperationSend, KeyKind: keys.KindEscalation}
}

// mutationCall is a change to a message that exists, named by the channel that
// made it.
func mutationCall(ref string) Call {
	return Call{
		AttemptKind: AttemptMutation, Operation: OperationUpdate, KeyKind: keys.KindEscalation,
		Receipt:    []byte(`{"channel_id":"C0001","timestamp":"100.200"}`),
		ReceiptRef: ref,
	}
}

// TestWhatAnAcceptanceHasToProveDependsOnWhatWasAsked is the four cases of the
// receipt contract, in one place.
//
// Making a message and changing one are different claims. A create that will
// not say what it made leaves a commitment that looks unsent, and the next
// revision makes a second message beside the one that exists. A change has
// nothing to name - the object was named before the call - and half the
// providers say nothing at all when there was nothing to alter.
func TestWhatAnAcceptanceHasToProveDependsOnWhatWasAsked(t *testing.T) {
	accepting := &wilfulHandler{outcome: OutcomeAccepted, known: true}
	made := mustReceipt("C0001/100.200", `{"channel_id":"C0001","timestamp":"100.200"}`)
	elsewhere := mustReceipt("C0002/999.999", `{"channel_id":"C0002","timestamp":"999.999"}`)

	cases := []struct {
		name    string
		call    Call
		got     Receipt
		outcome Outcome
		breach  Breach
		ref     string
		stored  bool
	}{
		{
			name: "a create that names what it made", call: createCall(),
			got: made, outcome: OutcomeAccepted, breach: BreachNone,
			ref: "C0001/100.200", stored: true,
		},
		{
			name: "a create that will not say", call: createCall(),
			outcome: OutcomeAmbiguous, breach: BreachAcceptanceWithoutReceipt,
		},
		{
			name: "a change accepted with nothing to say",
			call: mutationCall("C0001/100.200"),
			// Telegram answers exactly this to a change that would leave the
			// message as it is, and what it means is that the revision is
			// applied.
			outcome: OutcomeAccepted, breach: BreachNone, ref: "C0001/100.200",
		},
		{
			name: "a change that repeats the coordinates back",
			call: mutationCall("C0001/100.200"), got: made,
			outcome: OutcomeAccepted, breach: BreachNone,
			ref: "C0001/100.200", stored: true,
		},
		{
			name: "a change that answers about another message",
			call: mutationCall("C0001/100.200"), got: elsewhere,
			// Following it would move this card to somebody else's message.
			// The evidence is kept, and the fingerprint names what came back so
			// that two different wrong answers are two different results.
			outcome: OutcomeAmbiguous, breach: BreachMutationRetargeted,
			ref: "C0002/999.999", stored: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			concluded, breach := Conclude(accepting, tc.call,
				Result{Evidence: ProviderResponse, Status: "ok", Receipt: tc.got}, nil)

			if concluded.Outcome() != tc.outcome {
				t.Errorf("concluded %q, want %q", concluded.Outcome(), tc.outcome)
			}
			if breach != tc.breach {
				t.Errorf("breach %q, want %q", breach, tc.breach)
			}
			ref := concluded.Completion().ReceiptRefOrEmpty()
			if ref != tc.ref {
				t.Errorf("the conclusion names %q, want %q", ref, tc.ref)
			}
			if stored := len(concluded.Receipt()) > 0; stored != tc.stored {
				t.Errorf("the answer was %skept", map[bool]string{true: "", false: "not "}[stored])
			}
		})
	}
}

// TestOnlyAChangeMayProveAMessageIsGone is the whole outcome x detail x kind
// space, and almost all of it is refused.
//
// The word belongs to reconciliation, which this build never performs. The one
// thing an ordinary answer can prove is that the object is not there, and it is
// the only ground on which an operator may create a second message - so a
// channel able to state it about anything else could licence a duplicate.
func TestOnlyAChangeMayProveAMessageIsGone(t *testing.T) {
	details := []*keys.ProviderResultDetail{
		nil,
		detailOf(keys.DetailDefinitelyAbsent),
		detailOf(keys.DetailAcceptanceProven),
		detailOf(keys.DetailDeliveryProven),
		detailOf(keys.DetailInconclusive),
	}
	outcomes := []Outcome{
		OutcomeAccepted, OutcomeRetryableRejection,
		OutcomePermanentRejection, OutcomeAmbiguous,
	}
	kinds := []AttemptKind{AttemptCreate, AttemptMutation}

	for _, kind := range kinds {
		for _, outcome := range outcomes {
			for _, detail := range details {
				name := string(kind) + "/" + string(outcome) + "/" + detailName(detail)
				t.Run(name, func(t *testing.T) {
					call := createCall()
					if kind == AttemptMutation {
						call = mutationCall("C0001/100.200")
					}
					handler := &wilfulHandler{outcome: outcome, known: true, detail: detail}

					// An acceptance needs something to name, and that is a
					// different rule; give it one so this test is only about
					// the detail.
					got := mustReceipt("C0001/100.200", `{"channel_id":"C0001","timestamp":"100.200"}`)
					concluded, breach := Conclude(handler, call,
						Result{Evidence: ProviderResponse, Status: "s", Receipt: got}, nil)

					allowed := detail == nil ||
						(*detail == keys.DetailDefinitelyAbsent &&
							kind == AttemptMutation &&
							outcome == OutcomePermanentRejection)

					if allowed {
						if breach != BreachNone {
							t.Fatalf("a legal pair was refused: %q", breach)
						}
						if got := concluded.Completion().ProviderResultDetail; !sameDetail(got, detail) {
							t.Fatalf("the conclusion carries %v, want %v", got, detail)
						}
						return
					}

					if breach != BreachDetailNotAllowed {
						t.Fatalf("the pair was accepted with breach %q", breach)
					}
					if concluded.Outcome() != OutcomeAmbiguous {
						t.Fatalf("a refused pair concluded %q", concluded.Outcome())
					}
					// And nothing false is written down: the detail is the one
					// durable ground for making a second message.
					if concluded.Completion().ProviderResultDetail != nil {
						t.Fatal("a detail nobody may state was recorded anyway")
					}
				})
			}
		}
	}
}

func detailOf(d keys.ProviderResultDetail) *keys.ProviderResultDetail { return &d }

func detailName(d *keys.ProviderResultDetail) string {
	if d == nil {
		return "none"
	}
	return string(*d)
}

func sameDetail(got, want *keys.ProviderResultDetail) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

// TestWhetherAnAcceptanceNamesAnObjectIsTheKindsToSay is the first of the three
// places the rule lives, and the one that quietly changes the outcome rather
// than refusing: a webhook acceptance with no receipt is a success, an
// escalation's or a handover's is a breach recorded as doubt, and a kind this
// build cannot judge is doubt of its own kind. Through Conclude, because a test
// that called the constructor would never reach this place.
func TestWhetherAnAcceptanceNamesAnObjectIsTheKindsToSay(t *testing.T) {
	accepting := &wilfulHandler{outcome: OutcomeAccepted, known: true}
	made := mustReceipt("C0001/100.200", `{"channel_id":"C0001","timestamp":"100.200"}`)

	cases := []struct {
		name    string
		kind    keys.Kind
		got     Receipt
		outcome Outcome
		breach  Breach
		ref     string
	}{
		{name: "a webhook POST accepted, nothing to name", kind: keys.KindWebhookEvent,
			outcome: OutcomeAccepted, breach: BreachNone},
		{name: "a webhook replay accepted, nothing to name", kind: keys.KindWebhookReplay,
			outcome: OutcomeAccepted, breach: BreachNone},
		{name: "a webhook that named something anyway keeps it", kind: keys.KindWebhookEvent,
			got: made, outcome: OutcomeAccepted, breach: BreachNone, ref: "C0001/100.200"},
		{name: "an escalation accepted without coordinates", kind: keys.KindEscalation,
			outcome: OutcomeAmbiguous, breach: BreachAcceptanceWithoutReceipt},
		{name: "a re-admitted escalation accepted without coordinates", kind: keys.KindEscalationReplay,
			outcome: OutcomeAmbiguous, breach: BreachAcceptanceWithoutReceipt},
		{name: "a handover accepted without coordinates", kind: keys.KindHandoff,
			outcome: OutcomeAmbiguous, breach: BreachAcceptanceWithoutReceipt},
		{name: "a kind this build cannot judge", kind: keys.Kind("something_newer"),
			outcome: OutcomeAmbiguous, breach: BreachUnknownKind},
		{name: "no kind at all", kind: "",
			outcome: OutcomeAmbiguous, breach: BreachUnknownKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := createCall()
			call.KeyKind = tc.kind
			concluded, breach := Conclude(accepting, call,
				Result{Evidence: ProviderResponse, Status: "200", Receipt: tc.got}, nil)
			if concluded.Outcome() != tc.outcome {
				t.Errorf("concluded %q, want %q", concluded.Outcome(), tc.outcome)
			}
			if breach != tc.breach {
				t.Errorf("breach %q, want %q", breach, tc.breach)
			}
			if ref := concluded.Completion().ReceiptRefOrEmpty(); ref != tc.ref {
				t.Errorf("the conclusion names %q, want %q", ref, tc.ref)
			}
		})
	}

	// The second place, the constructor, answers the same way - and only asks
	// the kind on the one path where it matters.
	if _, err := NewConclusion(ConclusionInput{Outcome: OutcomeAccepted, KeyKind: keys.KindWebhookEvent}); err != nil {
		t.Fatalf("a webhook acceptance with nothing to name was refused: %v", err)
	}
	if _, err := NewConclusion(ConclusionInput{Outcome: OutcomeAccepted, KeyKind: keys.KindEscalation}); err == nil {
		t.Fatal("an escalation acceptance with nothing to name was built")
	}
	if _, err := NewConclusion(ConclusionInput{Outcome: OutcomeAccepted}); err == nil {
		t.Fatal("an acceptance with nothing to name and no kind was built")
	}
	if _, err := NewConclusion(ConclusionInput{Outcome: OutcomeAccepted, ReceiptRef: "C0001/1"}); err != nil {
		t.Fatalf("an acceptance that names its object needs no kind, got: %v", err)
	}
}
