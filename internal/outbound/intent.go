// Package outbound is the delivery domain: what it means to owe somebody a
// message, how that debt is discharged, and what is known about it at every
// point in between.
//
// The vocabulary here is deliberately narrow. A commitment (an intent) is one
// promise to one recipient. An attempt is one network call made under a lease.
// What the provider says about that call is a transport outcome - accepted,
// rejected, or unknown - and what it means for the commitment is a separate
// question the domain answers, because "the API took it" and "the person was
// told" are not the same fact.
package outbound

import (
	"encoding/json"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The closed sets a commitment is described by are declared once, in the
// grammar package, and named here.
//
// They are not repeated: every one of these values ends up inside a key, a
// fingerprint or a payload, so a second declaration would be a second spelling
// of the same fact - and the day the two drifted apart, one half of the system
// would deduplicate by a policy the other half was not executing.
type (
	Outcome         = keys.AttemptOutcome
	AmbiguityPolicy = keys.AmbiguityPolicy
	CompletionMode  = keys.CompletionMode
	Operation       = keys.Operation
	TargetKind      = keys.TargetKind
	Target          = keys.Target
	Slot            = keys.Slot
)

const (
	OutcomeAccepted           = keys.OutcomeAccepted
	OutcomeRetryableRejection = keys.OutcomeRetryableRejection
	OutcomePermanentRejection = keys.OutcomePermanentRejection
	OutcomeAmbiguous          = keys.OutcomeAmbiguous
	OutcomeCanceled           = keys.OutcomeCanceled

	PolicyRetry              = keys.PolicyRetry
	PolicyReconcileThenRetry = keys.PolicyReconcileThenRetry
	PolicyManualReview       = keys.PolicyManualReview
	PolicyAssumeAccepted     = keys.PolicyAssumeAccepted

	CompletionOnAcceptance      = keys.CompletionOnAcceptance
	CompletionOnProviderReceipt = keys.CompletionOnProviderReceipt

	OperationSend    = keys.OperationSend
	OperationUpdate  = keys.OperationUpdate
	OperationResolve = keys.OperationResolve
	OperationDeliver = keys.OperationDeliver
)

// Status is where a commitment stands.
//
// Terminal states are terminal: nothing but an operator's decision leaves them.
// That is the whole point of having them - a delivery that quietly resurrects
// is a delivery nobody can reason about.
type Status string

const (
	// StatusPending is claimable: a first attempt, a retry, or an editable
	// commitment whose desired state has moved ahead of what was applied.
	StatusPending Status = "pending"

	// StatusSending means a started attempt exists. It does not mean the
	// network has been called - only that it might have been, which is why an
	// abandoned one can only ever be resolved as ambiguous.
	StatusSending Status = "sending"

	// StatusIdle is an editable commitment that has applied everything asked of
	// it so far. Not terminal: the next revision puts it back in the queue.
	StatusIdle Status = "idle"

	// StatusManualReview is waiting for a person. Nothing automatic leaves it.
	StatusManualReview Status = "manual_review"

	StatusSucceeded       Status = "succeeded"
	StatusPermanentFailed Status = "permanent_failed"
	StatusExpired         Status = "expired"

	// StatusCanceled is a commitment the domain withdrew. It is not proof that
	// nothing went out: a send that was already in flight may well have landed.
	StatusCanceled Status = "canceled"
)

// Terminal reports whether a status is one only an operator can leave.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusPermanentFailed, StatusExpired, StatusCanceled:
		return true
	default:
		return false
	}
}

// Form is whether the external effect can be brought to a later revision.
type Form string

const (
	// FormOneShot is sent once and is done - a direct message, a webhook
	// delivery.
	FormOneShot Form = "one_shot"

	// FormEditable is a message that will be updated in place as the alert
	// changes: one external object, many revisions.
	FormEditable Form = "editable"
)

// AttemptKind is what an attempt was trying to do to the external world.
type AttemptKind string

const (
	// AttemptCreate makes the external object.
	AttemptCreate AttemptKind = "create"
	// AttemptMutation changes one that exists.
	AttemptMutation AttemptKind = "mutation"
	// AttemptReconcile asks the provider what happened. Not reachable yet.
	AttemptReconcile AttemptKind = "reconcile"
)

// Proof is how a success is known. It changes nothing about what happens to the
// commitment and everything about what the history says happened.
type Proof string

const (
	// ProofAccepted: the provider took the message.
	ProofAccepted Proof = "accepted"
	// ProofDelivered: the provider later confirmed delivery. Not reachable yet.
	ProofDelivered Proof = "delivered"
	// ProofAssumed: nobody confirmed anything and somebody decided to call it
	// delivered. The risk is recorded with it.
	ProofAssumed Proof = "assumed"
)

// RecordKind separates the two kinds of journal entry.
type RecordKind string

const (
	// RecordAttempt is a network call: written before it, closed by exactly one
	// compare-and-set afterwards.
	RecordAttempt RecordKind = "attempt"

	// RecordPreparation is the proven refusal of a send that never reached the
	// provider - an unlinked identity, a missing integration, a payload schema
	// this build cannot render.
	RecordPreparation RecordKind = "preparation"
)

// PreparationOutcome is what happened before the network was touched.
type PreparationOutcome string

const (
	// PreparationReady means the attempt can be made.
	PreparationReady PreparationOutcome = "ready"

	// PreparationPermanent is a refusal that will repeat until somebody changes
	// something: no integration, no linked identity, an unsupported payload.
	PreparationPermanent PreparationOutcome = "permanent"

	// PreparationTransient is a refusal that may not repeat - a cache that was
	// not warm, a lookup that failed. The database is still reachable, so the
	// commitment goes back into the queue with its lease released.
	PreparationTransient PreparationOutcome = "transient"
)

// Intent is a commitment as the domain reasons about it. It carries no
// database concerns: the store maps rows onto it and back.
type Intent struct {
	ID           string
	AlertGroupID string
	// Family is the execution partition this commitment runs in. It travels
	// with the commitment because everything that ends one - a finalisation, an
	// expiry, an operator, an alert being acknowledged - has to be able to say
	// which queue just got shorter without being told separately.
	Family string
	// KeyKind is what sort of claim this commitment came out of. It decides
	// which shape its payload is in and which of the two content forms an
	// attempt renders - and it is read from the row rather than inferred,
	// because two kinds can both be at payload schema 1 and mean different
	// things.
	KeyKind  keys.Kind
	Provider string
	// TargetKind and TargetRef are the recipient as THIS system names them - a
	// channel by its provider id, a person by their user id here. Turning the
	// second into an address the provider knows is preparation's job, and it is
	// redone on every attempt: an identity relinked between two attempts of one
	// effect must not move the message, which is why the address the effect is
	// bound to wins over whatever preparation resolves.
	TargetKind      TargetKind
	TargetRef       string
	Form            Form
	CompletionMode  CompletionMode
	AmbiguityPolicy AmbiguityPolicy
	Status          Status

	GenerationNo         int
	AttemptsInGeneration int
	FailureStreak        int

	// GenerationBound says the current external effect has an address and a
	// provider key of its own. It is a fact rather than a count: a refusal that
	// never reached the provider also adds a journal record, and deriving
	// "already bound" from the number of records would skip the binding of the
	// first call that actually happens.
	GenerationBound bool

	// PayloadSchemaVersion and ProviderKeyCodecVersion travel with the
	// commitment for its whole life. A handler reads the first to know which
	// shape it was given; the second is what a provider key is spelled with,
	// and it may not change between generations or the same revision would
	// reach the provider under two different keys.
	PayloadSchemaVersion    int
	ProviderKeyCodecVersion int

	// Payload is what the channel was asked to send, frozen at admission. It
	// travels with the commitment rather than only with an attempt because
	// preparation has to be able to refuse one it cannot read: decided after
	// the attempt is open, an unreadable payload is a network attempt that
	// never happened, recorded as one, retried forever.
	Payload json.RawMessage

	// PayloadDigest is what the payload digested to when the commitment was
	// admitted. Stored beside the bytes rather than derived from them on every
	// read, because the two are only useful when they disagree: a payload
	// changed after the promise was made still canonicalises perfectly, and
	// without something to compare it against it would simply be delivered.
	PayloadDigest []byte

	DesiredRevision      int64
	AppliedRevision      *int64
	FinalRevisionApplied bool

	// HasReceipt is the FACT that an external object exists, which is not the
	// same as having its coordinates. Erasure removes the coordinates and
	// leaves the fact: a message that was sent stays sent, and a state machine
	// that read the coordinates instead would decide it never happened.
	HasReceipt bool

	// Receipt and ReceiptRef are the coordinates of that object and the name
	// the channel gives it. Both travel with the commitment so a channel can
	// check, BEFORE anything opens an attempt, that it can still find what it
	// is being asked to change: coordinates it cannot read are a refusal, and a
	// refusal discovered after the attempt exists is a retry loop over a row
	// nobody is going to fix by trying again.
	Receipt    json.RawMessage
	ReceiptRef string

	// RecipientErased is the durable prohibition. The person this commitment
	// was for has been erased, so nothing may put their address back: no
	// retry, no new generation, no coordinates written by a call that was
	// already in flight.
	RecipientErased bool

	CancellationRequested bool
	AcceptedDuplicateRisk bool

	NotBefore     time.Time
	NextAttemptAt time.Time
	ExpiresAt     *time.Time

	// CreatedAt is the admission; UpdatedAt moves with every transition, so
	// for a commitment that has ended it is the moment it ended. The journal
	// orders by the first and retention measures from the second.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// GroupBound reports whether this commitment belongs to an alert group, which
// is what decides whether a transition has to take the group's lock first.
func (i Intent) GroupBound() bool { return i.AlertGroupID != "" }
