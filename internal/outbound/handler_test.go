package outbound

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Classification is where a wrong answer becomes either a duplicate page or a
// lost one, so the two rules the domain keeps for itself are the two where the
// cost is asymmetric: what a silent transport means, and what an answer nobody
// recognises means.

// TestSilenceIsClassifiedByWhatItProves. A request that never left can be
// retried for free. A request that may have gone out cannot: calling that a
// failure asserts an absence nobody established, and the retry becomes a second
// message with nothing in the record to explain it.
func TestSilenceIsClassifiedByWhatItProves(t *testing.T) {
	refused, class, decided := ClassifyTransport(Result{Evidence: DefinitelyNotSent})
	if !decided || refused != OutcomeRetryableRejection {
		t.Fatalf("a request that never left is %q (%q)", refused, class)
	}

	unknown, class, decided := ClassifyTransport(Result{Evidence: PossiblySent})
	if !decided || unknown != OutcomeAmbiguous {
		t.Fatalf("a request that may have arrived is %q (%q)", unknown, class)
	}

	if _, _, decided := ClassifyTransport(Result{Evidence: ProviderResponse}); decided {
		t.Fatal("the domain answered for a provider's own reply")
	}
}

// TestAnAnswerNobodyRecognisesIsDoubt is the default rule, and it outranks
// every line of the capability matrix: a rejection may only be declared where
// the provider's documentation proves the effect did not happen.
func TestAnAnswerNobodyRecognisesIsDoubt(t *testing.T) {
	for _, status := range []string{"", "teapot", "some_new_code_slack_added"} {
		outcome, class := UnknownStatus(status)
		if outcome != OutcomeAmbiguous {
			t.Fatalf("status %q was classified %q (%q); an unrecognised answer is doubt",
				status, outcome, class)
		}
		if class == "" {
			t.Fatalf("status %q was classified without a reason", status)
		}
	}
}

// TestAnAcceptanceHasToSayWhatItMade. "Yes, and I will not tell you where" is
// not a delivery: nothing can find, update or resolve that message afterwards.
// It is recorded as doubt, and the provider's breach is reported rather than
// absorbed.
func TestAnAcceptanceHasToSayWhatItMade(t *testing.T) {
	completion, broken := Conclude(Result{
		Evidence: ProviderResponse, Status: "ok",
	}, OutcomeAccepted, "")
	if !broken {
		t.Fatal("an acceptance with no coordinates was taken at its word")
	}
	if completion.Outcome != OutcomeAmbiguous {
		t.Fatalf("it was recorded as %q", completion.Outcome)
	}
	if completion.ErrorClass == nil || *completion.ErrorClass == "" {
		t.Fatal("the breach was recorded without saying what it was")
	}

	whole, broken := Conclude(Result{
		Evidence:   ProviderResponse,
		Status:     "ok",
		ReceiptRef: "C0001/1700000000.000100",
		Receipt:    json.RawMessage(`{"channel":"C0001"}`),
	}, OutcomeAccepted, "")
	if broken {
		t.Fatal("a complete acceptance was reported as a breach")
	}
	if whole.Outcome != OutcomeAccepted {
		t.Fatalf("a complete acceptance became %q", whole.Outcome)
	}
	if whole.ReceiptRef == nil || *whole.ReceiptRef != "C0001/1700000000.000100" {
		t.Fatalf("the coordinates were lost: %+v", whole.ReceiptRef)
	}

	// And the conclusion never names a revision. That belongs to the attempt,
	// and a handler that could set it could file its answer against a message
	// it never sent.
	if whole.AppliedRevision != nil {
		t.Fatal("a provider's answer named the revision it applied")
	}
}

// TestAConclusionCarriesWhatTheProviderSaid keeps the journal answerable: the
// status and the reason are what somebody reads months later, and a conclusion
// that dropped them would leave "ambiguous" with no story.
func TestAConclusionCarriesWhatTheProviderSaid(t *testing.T) {
	completion, _ := Conclude(Result{
		Evidence: ProviderResponse, Status: "ratelimited",
	}, OutcomeRetryableRejection, "rate_limited")

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
