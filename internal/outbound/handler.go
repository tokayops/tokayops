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
	ClassifyResponse(res Result) (Classification, bool)
}

// Classification is what a provider's own answer means, in the domain's words.
type Classification struct {
	Outcome Outcome

	// Class is the provider's own code, kept for the journal and the metric. A
	// closed vocabulary per provider, never a message.
	Class string

	// Detail is the typed thing the answer PROVES about the object, where it
	// proves anything. Almost always absent: an ordinary rejection says the
	// request failed, not what became of the message.
	//
	// The only one a channel may state here is that the object is gone -
	// "message_not_found" and its equivalents - because that is the fact an
	// operator needs before being allowed to create a second one. What the
	// domain does with it is Conclude's business, and the combinations it will
	// accept are closed there.
	Detail *keys.ProviderResultDetail
}

// Call is everything a handler needs to make one call, and deliberately not the
// commitment it belongs to.
//
// The state to render is a stored snapshot, never the live alert group: two
// instances have to send the same thing, and a retry has to send what its
// commitment is for. WHICH stored snapshot depends on what the commitment is. A
// one-shot message renders the state its admission froze, so every attempt of
// it carries the bytes that were accepted. A card renders the state the alert
// is in now, because bringing it up to date is the whole reason it can be
// changed at all.
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

	// Receipt is where the message this call changes already is - the whole of
	// what the provider said when it made one. Present for a mutation and empty
	// for a create, which is the difference between changing something and
	// making it.
	//
	// It is this commitment's own, never a neighbour's: the bytes of one
	// message may not depend on which of its siblings has been sent.
	Receipt json.RawMessage

	// ReceiptRef is what that message is called - the channel's own name for
	// it, settled when it was made. The domain compares this rather than
	// reading the coordinates, which are the provider's shape and nobody
	// else's.
	ReceiptRef string

	// KeyKind and Family say what this commitment IS, read from its row rather
	// than inferred. A handler picks its decoder and its renderer from them:
	// two kinds can both be at payload schema 1 and mean different things, and
	// "not an escalation, so try the other one" is a guess that becomes wrong
	// the day a third kind exists. Reading the emptiness of Content instead
	// would be the same guess wearing a different hat.
	KeyKind keys.Kind
	Family  string

	// Content is the state this call renders, in whichever of the two forms
	// this commitment has. A handler that needs a snapshot asks for one and is
	// told when there is none.
	Content AttemptContent

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

	// BreachMutationRetargeted: the provider answered a change to one message
	// with the coordinates of another. Slack's own documentation says the
	// channel in an update identifies the message and does not move it, so this
	// is a channel that has lost track of what it was asked to do - and
	// following it would take a card to somebody else's message.
	BreachMutationRetargeted Breach = "mutation_retargeted"

	// BreachDetailNotAllowed: the channel stated something about the object
	// that its answer does not prove, or that is not a channel's to state.
	BreachDetailNotAllowed Breach = "detail_not_allowed"

	// BreachUnknownKind: the call carried a kind of claim this build cannot
	// judge acceptances for. Not a channel's fault, and not a delivery
	// problem either - a row from another version of this system - but the
	// answer is doubt for the same reason every other unknown is.
	BreachUnknownKind Breach = "unknown_kind"
)

// Conclusion is what one attempt concluded: the answer the protocol
// fingerprints and whatever coordinates came back with it, as ONE value.
//
// They were two arguments once, and the pair could be assembled wrong - an
// acceptance naming a message beside a receipt of nothing. The store took it,
// the commitment settled as done with no receipt stored, and the next revision
// of the alert, finding none, would have created a second message beside the
// one that already existed. The invariant only holds if the halves cannot be
// carried separately.
//
// What the two halves mean depends on what the attempt was. A create names the
// object it made and carries its coordinates. A change names an object that
// already exists, through the effect receipt it was handed, and may carry no
// coordinates at all - half the providers answer a change that altered nothing
// with nothing.
type Conclusion struct {
	completion keys.Completion
	receipt    Receipt
	summary    string
}

// EffectReceipt is where the object this attempt acted on already was.
//
// It is not the same thing as the receipt above, and the difference is the
// whole reason a mutation can be concluded at all. The receipt above is what
// the provider returned FROM THIS CALL - absent when a change was accepted with
// nothing to say. This is the object the change was applied TO, known before
// the call was made, and it is what the completion is fingerprinted against so
// that a repeat of the same change compares equal to itself.
type EffectReceipt struct {
	ref string
}

// EffectReceiptOf names the object a mutation acts on.
func EffectReceiptOf(ref string) EffectReceipt { return EffectReceipt{ref: ref} }

// Ref is the stable name of that object.
func (e EffectReceipt) Ref() string { return e.ref }

// ConclusionInput is what a conclusion is made of.
type ConclusionInput struct {
	Outcome Outcome
	Class   string
	Status  string

	// KeyKind is what sort of claim the attempt was for. It decides one thing
	// here: whether an acceptance that names no object is a contradiction (a
	// message has coordinates) or the normal shape of success (a POST has
	// none). Read only on that path, so a conclusion that names its object
	// needs no kind to be valid.
	KeyKind keys.Kind

	// Receipt is what THIS call produced, and it is what gets stored about the
	// attempt. A change to an existing message usually produces nothing.
	Receipt Receipt

	// ReceiptRef is the name the completion is fingerprinted under: the object
	// that was made, or - for a change - the object that was changed. Without
	// it a repeated finalisation of a change would compare against nothing and
	// read as a contradiction rather than as the same result twice.
	ReceiptRef string

	// Detail is the typed thing the answer proved about the object, where it
	// proved anything. Almost always absent.
	Detail *keys.ProviderResultDetail

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
	// A caller that recorded coordinates and did not say what to call them
	// means the obvious thing. Conclude always says it outright, because for a
	// change the two differ; everywhere else they are the same object.
	if in.ReceiptRef == "" && in.Receipt.Recorded() {
		in.ReceiptRef = in.Receipt.Ref()
	}
	if in.Outcome == OutcomeAccepted && in.ReceiptRef == "" {
		names, err := AcceptanceNamesObject(in.KeyKind)
		if err != nil {
			return Conclusion{}, err
		}
		if names {
			return Conclusion{}, fmt.Errorf(
				"outbound: an acceptance naming no message is one nothing can find again")
		}
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
	if in.ReceiptRef != "" {
		ref := in.ReceiptRef
		completion.ReceiptRef = &ref
	}
	if in.Detail != nil {
		detail := *in.Detail
		completion.ProviderResultDetail = &detail
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

// Receipt is the coordinates the provider returned FROM THIS CALL, for the
// attempt's own row. They are optional: a change accepted with nothing to alter
// returns none, and the object it was applied to is named by the completion
// instead.
//
// The coordinates OF THE COMMITMENT are a different thing, decided by the store
// from the kind of the attempt - a create records them, a change never
// rewrites them.
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
//
// It takes the call as well as the answer, because what an acceptance has to
// prove depends on what was asked. Making a message and changing one are not
// the same claim.
func Conclude(h Handler, call Call, res Result, err error) (Conclusion, Breach) {
	outcome, class, detail, breach := classify(h, call, res)

	effect := EffectReceiptOf(call.ReceiptRef)

	receipt := res.Receipt
	if outcome == OutcomeAccepted {
		switch call.AttemptKind {
		case AttemptMutation:
			switch {
			case effect.Ref() == "":
				// A change to an object nobody named. The store refuses to make
				// the call at all, so this is a handler inventing one.
				outcome, class, breach = OutcomeAmbiguous,
					string(BreachAcceptanceWithoutReceipt), BreachAcceptanceWithoutReceipt
			case receipt.Recorded() && receipt.Ref() != effect.Ref():
				// It answered about a different message. Believing it would
				// point the card at somebody else's.
				outcome, class, breach = OutcomeAmbiguous,
					string(BreachMutationRetargeted), BreachMutationRetargeted
			}
		default:
			// Whether an acceptance has to say what it made is the KIND's to
			// answer, and it is asked here first because this is the first of
			// the three places the rule lives - and the one that does not refuse
			// but quietly changes the outcome. For a message, "yes, and I will
			// not tell you where" leaves one nothing can find, update or
			// resolve, and the commitment looking unsent, so the next revision
			// would make a second beside it. For a POST there is nothing to
			// name, and demanding it would turn every delivery into doubt.
			names, err := AcceptanceNamesObject(call.KeyKind)
			switch {
			case err != nil:
				outcome, class, breach = OutcomeAmbiguous,
					string(BreachUnknownKind), BreachUnknownKind
			case names && !receipt.Recorded():
				outcome, class, breach = OutcomeAmbiguous,
					string(BreachAcceptanceWithoutReceipt), BreachAcceptanceWithoutReceipt
			}
		}
	}

	// The name the completion is fingerprinted under. For a change it is the
	// object that was changed, whether or not the provider repeated it back: a
	// repeat of the same change has to compare equal to itself, and half the
	// providers say nothing at all when there was nothing to alter.
	ref := ""
	switch {
	case breach == BreachMutationRetargeted && receipt.Recorded():
		// What actually came back, so two different wrong answers are two
		// different results rather than one.
		ref = receipt.Ref()
	case call.AttemptKind == AttemptMutation:
		ref = effect.Ref()
	case receipt.Recorded():
		ref = receipt.Ref()
	}

	// What the provider returned is kept whole, including when it is the
	// evidence of a channel answering about the wrong message. Where it must
	// NOT go is onto the commitment: a change did not produce those
	// coordinates, it was aimed at them. Keeping the two apart is the store's
	// job, and it has the attempt kind to do it with.
	concluded, buildErr := NewConclusion(ConclusionInput{
		Outcome: outcome, Class: class, Status: res.Status, KeyKind: call.KeyKind,
		Receipt: receipt, ReceiptRef: ref, Detail: detail,
		Summary: Summarise(res.Summary, err),
	})
	if buildErr != nil {
		// Unreachable through the fold above, which already refuses everything
		// the constructor does - and if a rule is ever added to one and not the
		// other, doubt is the answer that costs least.
		concluded, _ = NewConclusion(ConclusionInput{
			Outcome: OutcomeAmbiguous, Class: string(BreachUnknownOutcome),
			Status: res.Status, KeyKind: call.KeyKind, Summary: Summarise(res.Summary, err),
		})
		return concluded, BreachUnknownOutcome
	}
	return concluded, breach
}

// allowedDetail says whether a channel may state this about its own answer.
//
// Closed, and narrow on purpose. The vocabulary belongs to reconciliation - a
// question this build never asks - and the one member of it an ordinary answer
// can prove is that the object is gone. Everything else would be a channel
// declaring a fact about the world from a status code, which is exactly what
// the split between ClassifyResponse and this function exists to prevent.
//
// The pair matters as much as the value: "the message is not there" is only
// meaningful beside a permanent rejection of a change. Said about an acceptance
// it contradicts itself, and said about a create it names a loss that cannot
// have happened, since nothing was made.
func allowedDetail(kind AttemptKind, outcome Outcome, detail *keys.ProviderResultDetail) bool {
	if detail == nil {
		return true
	}
	return *detail == keys.DetailDefinitelyAbsent &&
		kind == AttemptMutation && outcome == OutcomePermanentRejection
}

// classify folds one result into an outcome. The two silent cases never reach
// the handler; the third is the only one it is asked about.
func classify(h Handler, call Call, res Result) (Outcome, string, *keys.ProviderResultDetail, Breach) {
	switch res.Evidence {
	case DefinitelyNotSent:
		// Proof of absence: nothing happened, so trying again costs nothing.
		return OutcomeRetryableRejection, "transport_refused", nil, BreachNone

	case PossiblySent:
		// The expensive honest answer. Calling this retryable would assert an
		// absence nobody established, and the retry would then be a duplicate
		// nobody could explain.
		return OutcomeAmbiguous, "no_response", nil, BreachNone

	case ProviderResponse:
		answer, known := h.ClassifyResponse(res)
		if !known {
			outcome, class := unknownStatus(res.Status)
			return outcome, class, nil, BreachNone
		}
		switch answer.Outcome {
		case OutcomeAccepted, OutcomeRetryableRejection, OutcomePermanentRejection,
			OutcomeAmbiguous:
		default:
			// Including a withdrawal: a provider does not get to say that
			// somebody changed their mind.
			return OutcomeAmbiguous, string(BreachUnknownOutcome), nil, BreachUnknownOutcome
		}
		if !allowedDetail(call.AttemptKind, answer.Outcome, answer.Detail) {
			// The detail is dropped rather than recorded: it is the only
			// durable proof an operator is allowed to create a second message
			// on, and one written where it does not belong would be read as
			// exactly that.
			return OutcomeAmbiguous, string(BreachDetailNotAllowed), nil, BreachDetailNotAllowed
		}
		return answer.Outcome, answer.Class, answer.Detail, BreachNone

	default:
		// A handler that will not say what it could prove has told us nothing
		// about the message, and nothing is doubt.
		return OutcomeAmbiguous, string(BreachUnknownEvidence), nil, BreachUnknownEvidence
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
	// The limit is the whole summary, ellipsis included: what the journal
	// holds is never longer than the limit says.
	return string(runes[:SummaryLimit-len(ellipsis)]) + ellipsis
}

const ellipsis = "..."
