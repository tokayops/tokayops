package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a channel has to be able to do, and nothing more.
//
// Two methods, because there are exactly two questions the domain cannot answer
// for a provider: make the call, and say what its own answer means in its own
// dialect. Everything else - what a silence means, what an unrecognised answer
// means, whether an acceptance is worth believing - is decided here, once, for
// every channel there will ever be. A handler that could decide those could
// turn a page nobody received into a success, and no reviewer of a new channel
// would notice.
//
// Reconcile and ReduceProviderEvent are deliberately absent. Nothing in this
// build can perform either, and an interface method with no implementation is a
// promise that does not exist.

// Evidence is what the transport can prove about a request, and it is the whole
// reason a handler returns a struct rather than an error.
//
// The three are separated by whether an answer came back, not by anybody's
// conclusion about the message: a classification derived from the text of an
// error is a guess, and the guess that goes wrong costs either a duplicate page
// or a lost one.
type Evidence string

const (
	// DefinitelyNotSent: no answer, and the request provably never left - the
	// connection was refused, the name did not resolve, the handshake failed,
	// or the context was cancelled before the request was written.
	DefinitelyNotSent Evidence = "definitely_not_sent"

	// PossiblySent: no answer, and the request may have gone out whole. A
	// timeout after the write, a connection dropped mid-flight, an unknown
	// local failure once the bytes were on the wire.
	PossiblySent Evidence = "possibly_sent"

	// ProviderResponse: the provider answered. What the answer means is the
	// provider's dialect, and only the handler speaks it.
	ProviderResponse Evidence = "provider_response"
)

// Receipt is where a message ended up: the name the object is known by, and the
// whole of what the provider said about it.
//
// One value with no halves, because the halves are both dangerous. A reference
// with nothing stored leaves a commitment that looks unsent to the next
// revision, which would create a SECOND message beside the one that exists. A
// stored blob with no reference cannot be fingerprinted, so a repeated result
// reads as a contradiction.
type Receipt struct {
	ref string
	raw json.RawMessage
}

// NewReceipt is the only way to make one, and it refuses anything that is not a
// whole answer.
//
// The bytes are copied in and copied out. A value that shared a slice with the
// buffer a handler decoded from would change under the journal the first time
// somebody reused that buffer - and the message it names is the one thing
// nothing else in the system can reconstruct.
func NewReceipt(ref string, raw json.RawMessage) (Receipt, error) {
	if ref == "" {
		return Receipt{}, fmt.Errorf("outbound: a receipt with nothing to call the message by")
	}
	if len(raw) == 0 {
		return Receipt{}, fmt.Errorf("outbound: a receipt for %q with nothing recorded", ref)
	}
	if !json.Valid(raw) {
		return Receipt{}, fmt.Errorf("outbound: the receipt for %q is not valid JSON", ref)
	}
	return Receipt{ref: ref, raw: bytes.Clone(raw)}, nil
}

// Ref is the stable name the message is known by, and what the conclusion is
// fingerprinted over.
func (r Receipt) Ref() string { return r.ref }

// Raw is what gets stored, and the domain never looks inside it.
func (r Receipt) Raw() json.RawMessage { return bytes.Clone(r.raw) }

// Recorded says the provider told us where the message is.
func (r Receipt) Recorded() bool { return r.ref != "" && len(r.raw) > 0 }

// Result is what one call produced.
type Result struct {
	Evidence Evidence

	// Status is the provider's own code for what happened, when it answered.
	Status string

	// Receipt is set when the provider returned coordinates. Absent otherwise,
	// including for an acceptance - which is then not believed.
	Receipt Receipt

	// Summary is a short account for the journal.
	Summary string
}

// Handler makes one call for one provider.
type Handler interface {
	// ExecuteAttempt performs the single external effect of an attempt. The
	// context carries the attempt's deadline and bounds the call and whatever
	// enrichment follows it - never the recording of what happened.
	ExecuteAttempt(ctx context.Context, call Call) (Result, error)

	// ClassifyResponse says what the provider's OWN answer means, and is asked
	// nothing else: it is never called for a request that failed before an
	// answer arrived, because what those mean does not vary by provider.
	//
	// The bool is the honest half of the contract. A status this build has
	// never seen is not a failure to be guessed at - returning false hands it
	// to the rule that knows what to do with an unknown, and a handler that
	// guessed instead would be declaring an absence its documentation does not
	// prove.
	ClassifyResponse(res Result) (outcome Outcome, class string, known bool)
}

// Call is everything a handler needs to make one call, and deliberately not the
// commitment it belongs to.
//
// The state to render is the snapshot the admission froze, never the live alert
// group: a retry has to send what was accepted, and two instances have to send
// the same thing.
type Call struct {
	IntentID  string
	AttemptID string
	Provider  string

	AttemptKind AttemptKind
	Operation   Operation

	// Endpoint and ProviderKey are the effect's own identity, bound when the
	// generation opened. The handler sends to this address; it does not
	// resolve one of its own.
	Endpoint    string
	ProviderKey string

	Revision             int64
	State                keys.RenderSnapshot
	Payload              json.RawMessage
	PayloadSchemaVersion int
}

// Breach names a way a handler broke its contract. None of them stops a
// delivery - the safe answer is recorded either way - but every one of them
// means a channel is wrong, and a system that swallowed them would keep
// delivering slightly incorrect answers forever.
type Breach string

const (
	BreachNone Breach = ""

	// BreachUnknownEvidence: the handler did not say what it could prove.
	BreachUnknownEvidence Breach = "unknown_evidence"

	// BreachUnknownOutcome: the handler classified an answer as something this
	// domain does not have, or as a withdrawal, which is not a provider's to
	// declare.
	BreachUnknownOutcome Breach = "unknown_outcome"

	// BreachAcceptanceWithoutReceipt: the provider said yes and would not say
	// what it made.
	BreachAcceptanceWithoutReceipt Breach = "acceptance_without_receipt"
)

// Conclusion is what one attempt concluded: the answer the protocol
// fingerprints and the object the provider made, as ONE value.
//
// They were two arguments once, and the pair could be assembled wrong - an
// acceptance naming a message beside a receipt of nothing. The store took it,
// the commitment settled as done with no receipt stored, and the next revision
// of the alert, finding none, would have created a second message beside the
// one that already existed. The invariant only holds if the halves cannot be
// carried separately.
type Conclusion struct {
	completion keys.Completion
	receipt    Receipt
	summary    string
}

// ConclusionInput is what a conclusion is made of.
type ConclusionInput struct {
	Outcome Outcome
	Class   string
	Status  string
	Receipt Receipt
	Summary string
}

// NewConclusion states what an attempt concluded, and refuses the states the
// domain has no meaning for: an outcome nobody declared, and an acceptance with
// nothing to show for it.
func NewConclusion(in ConclusionInput) (Conclusion, error) {
	switch in.Outcome {
	case OutcomeAccepted, OutcomeRetryableRejection, OutcomePermanentRejection,
		OutcomeAmbiguous:
	default:
		return Conclusion{}, fmt.Errorf("outbound: %q is not an outcome an attempt has",
			in.Outcome)
	}
	if in.Outcome == OutcomeAccepted && !in.Receipt.Recorded() {
		return Conclusion{}, fmt.Errorf(
			"outbound: an acceptance with no receipt is a message nothing can find again")
	}

	completion := keys.Completion{Outcome: in.Outcome}
	if in.Class != "" {
		class := in.Class
		completion.ErrorClass = &class
	}
	if in.Status != "" {
		status := in.Status
		completion.ProviderStatus = &status
	}
	if in.Receipt.Recorded() {
		ref := in.Receipt.Ref()
		completion.ReceiptRef = &ref
	}
	return Conclusion{
		completion: completion,
		receipt:    in.Receipt,
		// Truncated here rather than by the caller: this is the only door into
		// a conclusion, and a limit that lives at one call site is a limit the
		// next caller does not have.
		summary: truncate(in.Summary),
	}, nil
}

// Completion is what the protocol fingerprints.
//
// A copy, down to the pointers. The struct is full of them, and handing out the
// originals would let a caller empty the receipt reference of a conclusion the
// store is about to fingerprint - leaving a message recorded under a name that
// is no longer the one it was accepted as.
func (c Conclusion) Completion() keys.Completion { return c.completion.Clone() }

// Receipt is what gets stored about the object, and it is present exactly when
// the completion names one.
func (c Conclusion) Receipt() json.RawMessage { return c.receipt.Raw() }

// Summary is the short account for the journal.
func (c Conclusion) Summary() string { return c.summary }

// Outcome is what the attempt concluded.
func (c Conclusion) Outcome() Outcome { return c.completion.Outcome }

// Conclude is the only path from what a handler saw to what the store records,
// and the handler is asked exactly one question along the way.
//
// It exists as one function because the alternative was tried: with the handler
// classifying everything, a channel could declare a request that may have gone
// out to be a clean failure, and the retry would be a second page with nothing
// in the journal to explain it. The rules that cost a page when they are wrong
// do not belong in the part of the system that a new channel adds.
func Conclude(h Handler, res Result, err error) (Conclusion, Breach) {
	outcome, class, breach := classify(h, res)

	// An acceptance has to say what it made. "Yes, and I will not tell you
	// where" leaves a message nothing can find, update or resolve - and leaves
	// the commitment looking unsent, so the next revision would make a second
	// one beside it.
	receipt := res.Receipt
	if outcome == OutcomeAccepted && !receipt.Recorded() {
		outcome, class, breach = OutcomeAmbiguous,
			string(BreachAcceptanceWithoutReceipt), BreachAcceptanceWithoutReceipt
	}

	concluded, buildErr := NewConclusion(ConclusionInput{
		Outcome: outcome, Class: class, Status: res.Status,
		Receipt: receipt, Summary: Summarise(res.Summary, err),
	})
	if buildErr != nil {
		// Unreachable through the fold above, which already refuses everything
		// the constructor does - and if a rule is ever added to one and not the
		// other, doubt is the answer that costs least.
		concluded, _ = NewConclusion(ConclusionInput{
			Outcome: OutcomeAmbiguous, Class: string(BreachUnknownOutcome),
			Status: res.Status, Summary: Summarise(res.Summary, err),
		})
		return concluded, BreachUnknownOutcome
	}
	return concluded, breach
}

// classify folds one result into an outcome. The two silent cases never reach
// the handler; the third is the only one it is asked about.
func classify(h Handler, res Result) (Outcome, string, Breach) {
	switch res.Evidence {
	case DefinitelyNotSent:
		// Proof of absence: nothing happened, so trying again costs nothing.
		return OutcomeRetryableRejection, "transport_refused", BreachNone

	case PossiblySent:
		// The expensive honest answer. Calling this retryable would assert an
		// absence nobody established, and the retry would then be a duplicate
		// nobody could explain.
		return OutcomeAmbiguous, "no_response", BreachNone

	case ProviderResponse:
		outcome, class, known := h.ClassifyResponse(res)
		if !known {
			outcome, class := unknownStatus(res.Status)
			return outcome, class, BreachNone
		}
		switch outcome {
		case OutcomeAccepted, OutcomeRetryableRejection, OutcomePermanentRejection,
			OutcomeAmbiguous:
			return outcome, class, BreachNone
		default:
			// Including a withdrawal: a provider does not get to say that
			// somebody changed their mind.
			return OutcomeAmbiguous, string(BreachUnknownOutcome), BreachUnknownOutcome
		}

	default:
		// A handler that will not say what it could prove has told us nothing
		// about the message, and nothing is doubt.
		return OutcomeAmbiguous, string(BreachUnknownEvidence), BreachUnknownEvidence
	}
}

// unknownStatus is the default rule of the capability matrix, and it outranks
// every line in it: a rejection may only be declared where the provider's own
// documentation proves the effect did not happen. Everything else that comes
// back from a request that went out is doubt.
//
// Getting this backwards is how a page is lost: "unknown code, probably a
// failure, retry later" quietly discards a message that was delivered, or fails
// a commitment that was never attempted.
func unknownStatus(status string) (Outcome, string) {
	if status == "" {
		return OutcomeAmbiguous, "unknown_response"
	}
	return OutcomeAmbiguous, "unknown_status"
}

// SummaryLimit is how much of an account of a call is kept (NFR-7). Enough to
// recognise what happened, not enough to make the journal a log sink.
const SummaryLimit = 500

// Summarise is what the journal keeps about a call: the handler's own account,
// or the error if it did not give one.
func Summarise(summary string, err error) string {
	if summary == "" && err != nil {
		return err.Error()
	}
	return summary
}

func truncate(summary string) string {
	runes := []rune(summary)
	if len(runes) <= SummaryLimit {
		return summary
	}
	return string(runes[:SummaryLimit]) + "..."
}
