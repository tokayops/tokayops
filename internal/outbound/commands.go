package outbound

import (
	"encoding/json"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The commands the domain offers, and what each of them can answer.
//
// Every result is a closed set rather than a boolean or an error string: the
// caller of an admission has to be able to tell "somebody already accepted this
// exact work" from "somebody accepted different work under the same claim", and
// a bool cannot say that. The same goes for every other operation here - the
// outcomes are the vocabulary the workflows above are written in.

// EscalationAdmission is what a producer hands the store: the identities the
// grammar derived, plus the two things about the alert group that are settled
// at the same moment.
type EscalationAdmission struct {
	// Admission carries the batch key, the fingerprint, the snapshot and every
	// commitment with its key and payload. It is produced in one pass by the
	// grammar so the parts cannot disagree.
	Admission keys.Admission

	// PolicyID and PolicySnapshot are the escalation policy this admission was
	// built against, recorded on the group by the winner of the admission and
	// by nobody else.
	PolicyID       string
	PolicySnapshot json.RawMessage

	// StepsWithoutRecipients names the plan steps that resolved to nobody.
	// They produce no commitment - a commitment that must fail is an alert
	// about a failure the system knew about in advance - so the history is
	// where they are recorded.
	StepsWithoutRecipients []string

	Actor string
}

// SubmitOutcome is what happened to an admission.
type SubmitOutcome string

const (
	// SubmitCreated: this producer's set was accepted.
	SubmitCreated SubmitOutcome = "created"

	// SubmitExisting: the same set was already accepted, by this producer or
	// another. An idempotent repeat, and the normal answer to a retry after a
	// lost reply.
	SubmitExisting SubmitOutcome = "existing"

	// SubmitConflict: a DIFFERENT set was already accepted under this claim.
	// The first one stands; this one is refused rather than merged, because
	// merging two audiences is how somebody gets paged who was never chosen.
	SubmitConflict SubmitOutcome = "conflict"

	// SubmitGroupNotAdmitted: the alert group moved on - acknowledged, resolved
	// - before this admission got there. The user won.
	SubmitGroupNotAdmitted SubmitOutcome = "group_not_admitted"
)

// SubmitResult is the answer to an admission, with what was accepted.
type SubmitResult struct {
	Outcome SubmitOutcome
	BatchID string
	// IntentIDs are the commitments this claim holds, in key order, whether
	// they were created now or found already there.
	IntentIDs []string
}

// Expired is one commitment whose deadline passed before anything was sent.
type Expired struct {
	IntentID     string
	AlertGroupID string
}

// Recovered is one commitment whose worker died with an attempt open.
type Recovered struct {
	IntentID  string
	AttemptID string
	To        Status
	Row       string
}

// ProviderDue is the demand of one provider, as the scheduler and the metric
// need it - which is not the same number.
type ProviderDue struct {
	Provider string
	// ClaimableDue and ClaimableFresh count what a claim could actually take:
	// the deadline is alive and no other instance holds a lease. Handing a
	// share of the pool to work that cannot be taken is how a worker ends up
	// idle with a queue in front of it.
	ClaimableDue   int
	ClaimableFresh int
	// LatenessSeconds is measured over ALL due work, leased or not, and is
	// computed by the database in the same statement. A row claimed and never
	// begun is exactly what a health signal must not hide.
	LatenessSeconds float64
}

// ClaimPhase is which half of a provider's queue a claim is asking for.
type ClaimPhase string

const (
	// ClaimFirstAttempts takes commitments nobody has tried yet, so a freshly
	// admitted page does not queue behind a pile of old retries.
	ClaimFirstAttempts ClaimPhase = "first_attempts"

	// ClaimAny takes whatever is due. Retries are guaranteed a share of every
	// claim, or a steady stream of new work would starve them for good.
	ClaimAny ClaimPhase = "any"
)

// Leased is a commitment this worker now holds, with the token that proves it.
type Leased struct {
	Intent      Intent
	LeaseToken  string
	LockedUntil time.Time
}

// BeginAttemptRequest is a worker asking to make one call, with everything it
// resolved before asking.
//
// Preparation happens outside the transaction and outside the domain: an
// address is looked up, credentials are read, the provider's configuration is
// checked. What arrives here is the ANSWER, because the transaction that opens
// an attempt must not be waiting on anything but the database.
type BeginAttemptRequest struct {
	IntentID   string
	LeaseToken string
	WorkerID   string

	Preparation PreparationOutcome

	// BoundEndpoint is the provider's own address for this recipient, as the
	// worker resolved it now. It is a PROPOSAL: if the effect is already bound
	// to an address, that one wins and comes back in the result. A retry after
	// doubt must not go to a number somebody changed in between, and the worker
	// is not the one who gets to decide that.
	BoundEndpoint string
	AttemptKind   AttemptKind
	Operation     Operation
	// Revision is the revision this attempt applies. It is checked against the
	// state the commitment is actually about rather than trusted.
	Revision int64

	// ErrorClass and Summary describe a preparation that failed, and are what
	// the journal keeps instead of an attempt.
	ErrorClass string
	Summary    string
}

// BeginOutcome is what happened when a worker asked to start.
type BeginOutcome string

const (
	// BeginStarted: the attempt exists and the network may be called.
	BeginStarted BeginOutcome = "started"

	// BeginPreparedPermanent: nothing was sent and nothing will be until
	// somebody changes a configuration. The refusal is recorded as proof that
	// no call was made.
	BeginPreparedPermanent BeginOutcome = "prepared_permanent"

	// BeginPreparedRetry: nothing was sent this time.
	BeginPreparedRetry BeginOutcome = "prepared_retry"

	// BeginLeaseLost: somebody else holds this commitment now.
	BeginLeaseLost BeginOutcome = "lease_lost"

	// BeginIntentFinalized: it was withdrawn or finished while the worker was
	// preparing.
	BeginIntentFinalized BeginOutcome = "intent_finalized"

	// BeginUncertain: an attempt of this worker's own lease already exists, so
	// the reply to a previous begin was lost. The network is NOT authorised a
	// second time: after a restart nobody can prove whether the provider was
	// called, and recovery closing it as ambiguous is the conservative answer.
	BeginUncertain BeginOutcome = "uncertain_begin"

	BeginNotFound BeginOutcome = "not_found"
)

// BeginAttemptResult is what the worker needs to make the call: the attempt it
// is making, and the content to render, which comes from the domain rather than
// from the live alert group.
type BeginAttemptResult struct {
	Outcome   BeginOutcome
	AttemptID string
	AttemptNo int

	GenerationNo int
	// BoundEndpoint and ProviderKey are the ones the effect is bound to, which
	// may not be the ones proposed: within one generation the address and the
	// key are what they were when it opened. The worker sends to these.
	BoundEndpoint string
	ProviderKey   string

	AppliedRevision int64
	Snapshot        keys.RenderSnapshot
	Payload         json.RawMessage
	// PayloadSchemaVersion says which shape the payload is in, so a handler
	// reads it rather than assuming the current one.
	PayloadSchemaVersion int

	// CompletionFingerprintVersion is stamped here, not when the attempt is
	// finished: an attempt can outlive a deployment, and the encoder that
	// closes it has to be the one that opened it.
	CompletionFingerprintVersion int

	Intent Intent
}

// FinalizeRequest closes one attempt with what is known about it.
type FinalizeRequest struct {
	AttemptID  string
	LeaseToken string

	// Completion is the conclusion in the form its fingerprint is taken over.
	// It is what tells a repeat of this call from a different one.
	Completion keys.Completion

	// Receipt is the provider's coordinates, stored on the attempt and on the
	// commitment. Kept on the attempt as well because a later generation clears
	// the commitment's copy, and the address of a message that really was sent
	// must not disappear with it.
	Receipt json.RawMessage

	Summary string
}

// FinalizeOutcome is the answer to closing an attempt.
type FinalizeOutcome string

const (
	// FinalizeFinalized: this call's result is now the commitment's.
	FinalizeFinalized FinalizeOutcome = "finalized"

	// FinalizeIdempotentRepeat: the same conclusion was already recorded. The
	// normal answer to a lost commit reply.
	FinalizeIdempotentRepeat FinalizeOutcome = "idempotent_repeat"

	// FinalizeConflict: a DIFFERENT conclusion was already recorded for this
	// attempt. One of the two is wrong, and overwriting would hide which.
	FinalizeConflict FinalizeOutcome = "conflict"

	// FinalizeLeaseLost: somebody else closed this attempt - usually recovery,
	// after the worker was presumed dead. A genuine late result is kept as an
	// observation rather than thrown away.
	FinalizeLeaseLost FinalizeOutcome = "lease_lost"

	FinalizeNotFound FinalizeOutcome = "not_found"
)

// FinalizeResult says what the commitment did as a result.
type FinalizeResult struct {
	Outcome FinalizeOutcome
	To      Status
	Row     string

	// ObservationRecorded is set when a late result was kept for an attempt
	// somebody else had already closed.
	ObservationRecorded bool
}

// ResolveAmbiguityRequest is a person deciding what a stuck commitment does.
type ResolveAmbiguityRequest struct {
	IntentID string
	Decision Decision
	Actor    string
	Reason   string

	// AcceptedDuplicateRisk is the operator saying, on the record, that a
	// second message may exist. Required for a new effect after an attempt
	// whose fate is unknown.
	AcceptedDuplicateRisk bool

	// ResourceLossConfirmed is the operator saying the previous external object
	// is definitely gone, which is the other way a new effect is allowed.
	ResourceLossConfirmed bool

	// NewExpiresAt is required to revive something that expired: without a new
	// deadline the first claim would expire it again.
	NewExpiresAt *time.Time
}

// ResolveOutcome is the answer to an operator's decision.
type ResolveOutcome string

const (
	ResolveResolved ResolveOutcome = "resolved"

	// ResolveAlreadyResolved: somebody - another operator, or an
	// acknowledgement - got there first. The current state comes back with it.
	ResolveAlreadyResolved ResolveOutcome = "already_resolved"

	// ResolveInvalidDecision: the decision does not apply to this commitment.
	ResolveInvalidDecision ResolveOutcome = "invalid_decision"

	// ResolveBusinessClosed: the alert this commitment belongs to is over.
	// Reviving a page for a closed incident is not a delivery anybody wants.
	ResolveBusinessClosed ResolveOutcome = "business_closed"

	ResolveNotFound ResolveOutcome = "not_found"
)

// ResolveAmbiguityResult is where the commitment ended up.
type ResolveAmbiguityResult struct {
	Outcome ResolveOutcome
	Status  Status
	Row     string
}

// StatusCount is how many commitments of a family are in one status, for the
// health signals that watch the queue rather than any single delivery.
type StatusCount struct {
	Status Status
	Count  int
}

// AttemptRecord is one line of the journal: a network call, or the proof that
// one was refused before it could be made.
type AttemptRecord struct {
	ID           string
	AttemptNo    int
	RecordKind   RecordKind
	GenerationNo int
	AttemptKind  AttemptKind
	Operation    Operation

	Provider      string
	BoundEndpoint string
	ProviderKey   string

	AppliedRevision *int64
	StartedAt       *time.Time
	FinishedAt      *time.Time
	Outcome         Outcome
	ErrorClass      string
	ProviderStatus  string
	Receipt         json.RawMessage
	Summary         string
	FinishReason    string

	CompletionFingerprintVersion int
}

// Observation is a result that arrived for an attempt somebody else had already
// closed - usually a worker returning after recovery gave up on it.
type Observation struct {
	AttemptID string
	Kind      string
	Outcome   Outcome
	Receipt   json.RawMessage
	Summary   string
}

// IntentEvent is one thing that happened to a commitment without a network call
// being involved.
type IntentEvent struct {
	Seq        int
	Kind       string
	Reason     string
	Actor      string
	FromStatus string
	ToStatus   string
}

// Journal is everything the system knows about one commitment: what it is, what
// was attempted, what came back late, and what people did to it.
//
// It exists as one read because that is the question anybody actually asks - of
// a delivery that did not arrive, of an audit, of a support request - and
// answering it from three separate calls invites answering it from two.
type Journal struct {
	Intent       Intent
	Attempts     []AttemptRecord
	Observations []Observation
	Events       []IntentEvent
}
