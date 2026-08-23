package outbound

import (
	"context"
	"encoding/json"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a channel has to be able to do, and nothing more.
//
// Two methods, because there are exactly two questions the domain cannot answer
// for a provider: make the call, and say what the answer means in its own
// dialect. Everything else - whether a call may be made at all, which revision
// it carries, what happens to the commitment afterwards - is the domain's, and
// a handler that could decide any of it could deliver a message the system
// never agreed to send.
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

// Result is what one call produced.
type Result struct {
	Evidence Evidence

	// Status is the provider's own code for what happened, when it answered.
	Status string

	// ReceiptRef is the stable name of the object the provider made, in one
	// string: it is what the conclusion is fingerprinted over, so a repeat of
	// the same result has to spell it the same way.
	ReceiptRef string

	// Receipt is the whole of what came back about that object - the shape
	// differs per provider, and the domain never looks inside it.
	Receipt json.RawMessage

	// Summary is a short, truncated account for the journal (NFR-7).
	Summary string
}

// Handler makes one call for one provider.
type Handler interface {
	// ExecuteAttempt performs the single external effect of an attempt. The
	// context carries the attempt's deadline and bounds the call and whatever
	// enrichment follows it - never the recording of what happened.
	ExecuteAttempt(ctx context.Context, call Call) (Result, error)

	// Classify says what the provider's answer means. It is called for every
	// result, including the ones with no answer at all, and ClassifyTransport
	// below is the part of that decision it must not make for itself.
	Classify(res Result, err error) (Outcome, string)
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

// ClassifyTransport answers for everything that is not a provider's answer, and
// a handler must not answer differently.
//
// The two cases it covers are the ones a provider cannot know about - its own
// message never reached it - and they are also the two where a wrong answer is
// most expensive. It returns false only for a real answer, which is where the
// provider's dialect begins.
func ClassifyTransport(res Result) (Outcome, string, bool) {
	switch res.Evidence {
	case DefinitelyNotSent:
		// Proof of absence: nothing happened, so trying again costs nothing.
		return OutcomeRetryableRejection, "transport_refused", true
	case PossiblySent:
		// The expensive honest answer. Calling this retryable would assert an
		// absence nobody established, and a retry would then be a duplicate
		// nobody could explain.
		return OutcomeAmbiguous, "no_response", true
	default:
		return "", "", false
	}
}

// UnknownStatus is the default rule of the capability matrix, and it outranks
// every line in it: a rejection may only be declared where the provider's own
// documentation proves the effect did not happen. Everything else that comes
// back from a request that went out is doubt.
//
// Getting this backwards is how a page is lost: "unknown code, probably a
// failure, retry later" quietly discards a message that was delivered, or
// fails a commitment that was never attempted.
func UnknownStatus(status string) (Outcome, string) {
	if status == "" {
		return OutcomeAmbiguous, "unknown_response"
	}
	return OutcomeAmbiguous, "unknown_status"
}

// Conclude turns a handler's answer into the conclusion the store records, and
// enforces the one thing a provider is not allowed to get wrong.
//
// An acceptance has to come with the coordinates that prove it. "Yes, and I
// will not tell you what I made" is not a delivery: the message cannot be
// found, updated, resolved or shown to anybody afterwards. It is recorded as
// doubt, and the breach is reported so it can be counted - a provider that does
// this is broken, and the system that hid it would be too.
func Conclude(res Result, outcome Outcome, class string) (keys.Completion, bool) {
	completion := keys.Completion{Outcome: outcome}
	if class != "" {
		completion.ErrorClass = &class
	}
	if res.Status != "" {
		status := res.Status
		completion.ProviderStatus = &status
	}
	if res.ReceiptRef != "" {
		ref := res.ReceiptRef
		completion.ReceiptRef = &ref
	}

	if outcome == OutcomeAccepted && res.ReceiptRef == "" {
		completion.Outcome = OutcomeAmbiguous
		broken := "acceptance_without_coordinates"
		completion.ErrorClass = &broken
		return completion, true
	}
	return completion, false
}
