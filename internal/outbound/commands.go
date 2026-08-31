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

// DesiredReason is why the desired state of an alert group moved. Closed,
// because it is what the history says happened, what the metric counts, and
// what decides whether the revision is the last one.
type DesiredReason string

const (
	DesiredAck     DesiredReason = "ack"
	DesiredResolve DesiredReason = "resolve"
	DesiredMerge   DesiredReason = "merge"
)

// Known reports whether a reason is one this build states.
func (r DesiredReason) Known() bool {
	switch r {
	case DesiredAck, DesiredResolve, DesiredMerge:
		return true
	default:
		return false
	}
}

// Final says this revision is the last one a message will ever be brought to.
//
// It is derived rather than passed in, and that is the point: as a field beside
// the reason it could disagree with it, and both ways of disagreeing produce a
// message that lies. "Acknowledged and final" freezes a card mid-incident, so
// nothing that happens afterwards ever reaches it. "Resolved and not final"
// leaves the commitment waiting for a revision that will never come, and the
// alert's own resolution never lands on the card.
func (r DesiredReason) Final() bool { return r == DesiredResolve }

// DesiredOutcome is what happened to a proposal to move the desired state.
type DesiredOutcome string

const (
	// DesiredApplied: a new revision exists and the commitments are aimed at it.
	DesiredApplied DesiredOutcome = "applied"

	// DesiredUnchanged: what the message would say has not moved, so nothing
	// was written. Alertmanager repeats the same payload, and a revision raised
	// for every repeat would be a polling loop with a real edit on each tick.
	DesiredUnchanged DesiredOutcome = "unchanged"

	// DesiredNoSnapshot: the group has not been admitted yet, so there is no
	// state to supersede. Its revision 0 will be frozen by the admission, from
	// a group that already includes this change.
	DesiredNoSnapshot DesiredOutcome = "no_snapshot"

	// DesiredStaleAfterFinal: the alert's desired state is settled. A late
	// payload cannot raise a revision over the one that resolved it - nothing
	// would ever apply it.
	DesiredStaleAfterFinal DesiredOutcome = "stale_after_final"
)

// DesiredStateRequest raises the desired state of an alert group.
//
// It carries no revision and no snapshot, and that is the contract rather than
// an omission: both are computed inside the transition's own transaction, from
// the rows as they became. Built before it, a snapshot describes the state the
// change was applied TO - the alert still reading as firing, nobody named as
// having acknowledged or resolved it, and none of the alerts a merge just
// recorded. Every one of those reaches the message.
type DesiredStateRequest struct {
	AlertGroupID string

	// Reason is what happened to the alert, and the command checks the group
	// against it: an acknowledgement whose group is not acknowledged means the
	// caller has not made the transition it is claiming, and the snapshot would
	// freeze a state nobody is in.
	//
	// It also decides finality (see DesiredReason.Final), which is why there is
	// no separate flag to contradict it.
	Reason DesiredReason

	Actor string
}

// DesiredStateResult is what the proposal did.
type DesiredStateResult struct {
	Outcome DesiredOutcome

	// Revision is the revision now stored - the new one when applied, the
	// existing one otherwise. Zero only when there is no snapshot at all.
	Revision int64

	// Touched is how many commitments were aimed at the new revision.
	Touched int
}

// Batch is what a producer hands the store: one admission, whatever it is
// about.
//
// The parts every admission has - the identities the grammar derived, and who
// is asking - sit here; everything that belongs to ONE sort of claim sits in
// the context. Written as one struct with optional fields instead, the
// escalation half would be a set of nil-able values that a handover has to
// leave empty and nothing would refuse an escalation that left them empty too.
type Batch struct {
	// Admission carries the batch key, the fingerprint, the snapshot if this
	// kind has one, and every commitment with its key and payload. It is
	// produced in one pass by the grammar so the parts cannot disagree.
	Admission keys.Admission

	// Context is what this admission is about, in one of the two closed forms.
	Context BatchContext

	Actor string
}

// ContextForm says which sort of claim a batch is.
type ContextForm string

const (
	// ContextEscalation: an alert group is being escalated. The admission is
	// about a state of that group, and the group itself is written to.
	ContextEscalation ContextForm = "escalation"

	// ContextHandoff: a shift change is being announced. There is no alert
	// group, and nothing in the alert domain is touched.
	ContextHandoff ContextForm = "handoff"
)

// BatchContext is the closed pair. Built through the two constructors, so a
// context cannot be half-filled: an escalation without its policy, or a
// handover carrying one.
type BatchContext struct {
	form       ContextForm
	escalation EscalationContext
}

// EscalatingAlertGroup is the context of an escalation.
func EscalatingAlertGroup(about EscalationContext) BatchContext {
	return BatchContext{form: ContextEscalation, escalation: about}
}

// AnnouncingShiftChange is the context of a handover announcement. It carries
// nothing: everything a handover is about is already in its admission.
func AnnouncingShiftChange() BatchContext {
	return BatchContext{form: ContextHandoff}
}

// Form says which of the two this is.
func (c BatchContext) Form() ContextForm { return c.form }

// Escalation is the alert-group half, and false when this is not one. A caller
// that reads it without asking would get a zero policy and a zero version, and
// the version is compared against the group's own.
func (c BatchContext) Escalation() (EscalationContext, bool) {
	if c.form != ContextEscalation {
		return EscalationContext{}, false
	}
	return c.escalation, true
}

// EscalationContext is everything about an escalation that is not in its
// admission: what plan it was built from, what the producer saw, and what it
// could not promise.
type EscalationContext struct {
	// PolicyID and PolicySnapshot are the escalation policy this admission was
	// built against, recorded on the group by the winner of the admission and
	// by nobody else.
	PolicyID       string
	PolicySnapshot json.RawMessage

	// SourceVersion is the version of the alert group the snapshot was frozen
	// from, as the producer observed it.
	//
	// The snapshot is the state EVERY message of this escalation is rendered
	// from, for as long as the escalation lives. Between reading the group and
	// admitting the plan, an alert can join it or a user can resolve it - and
	// the plan would then page about a state the alert is no longer in, with no
	// revision that ever corrects it.
	//
	// So it is checked again under the lock that decides the admission. A
	// version that has moved is not an error and not a conflict: it is a plan
	// built a moment too early, refused whole, with the group left for the next
	// tick to plan again from what is now there.
	//
	// Zero is a version, not "unset": a group nothing has changed is at zero,
	// and it is compared like any other.
	SourceVersion int64

	// OnCallSnapshot is who was on duty when this alert arrived, recorded on
	// the group by the winner and by nobody else.
	//
	// It is here, and not written by the producer after the fact, because it is
	// a claim about the SAME moment the commitments were built from. Written
	// outside this unit of work it would be the loser's answer overwriting the
	// winner's - a group displaying one set of people while another set is
	// being paged.
	//
	// Empty means nothing is recorded. That is not the same as an empty
	// snapshot: "nobody was on call" is a fact about the schedule, and a
	// producer that could not read the people has no business asserting it.
	OnCallSnapshot json.RawMessage

	// Unpromised names the plan steps that produced no commitment, and why.
	// A commitment that must fail is an alert about a failure the system knew
	// about in advance, so the history is where these are recorded - and the
	// reason travels with them, because "nobody was on call" and "nothing here
	// can deliver to that provider" send a reader to two different places.
	Unpromised []UnpromisedStep
}

// UnpromisedStep is one step of a plan that promised nothing.
type UnpromisedStep struct {
	// Step names it as the policy does: its index and what it was aiming at.
	Step   string
	Reason UnpromisedReason
	// Detail is what the reason does not say on its own - which provider,
	// which schedule.
	Detail string
}

// UnpromisedReason is why a step promised nothing. Closed, because each of
// these sends whoever reads the history somewhere different: to the schedule,
// to the policy, or to the deployment.
type UnpromisedReason string

const (
	// ReasonNobodyOnCall: the schedule answered, and the answer was nobody.
	ReasonNobodyOnCall UnpromisedReason = "nobody_on_call"

	// ReasonNoTarget: the step names no recipient at all.
	ReasonNoTarget UnpromisedReason = "no_target"

	// ReasonNoChannel: the step names a provider this build cannot deliver
	// through. Not a fact about the alert - a fact about what is deployed.
	ReasonNoChannel UnpromisedReason = "no_channel"

	// ReasonDuplicate: the plan asked for the same message twice - the same
	// recipient, in the same step of the policy. One promise is what that
	// means, and the second is recorded rather than dropped in silence.
	ReasonDuplicate UnpromisedReason = "duplicate"
)

// SubmitOutcome is what happened to an admission.
type SubmitOutcome string

const (
	// SubmitCreated: this producer's set was accepted.
	SubmitCreated SubmitOutcome = "created"

	// SubmitSourceChanged: the alert moved while the plan was being built, so
	// the snapshot describes a state that is already behind. Nothing was
	// written and the group was not claimed - the next tick plans it again.
	SubmitSourceChanged SubmitOutcome = "source_changed"

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

	// SubmitRecipientErased: the plan promises a message to somebody who has
	// been erased. Nothing is admitted: a commitment aimed at them would need
	// an address, and erasure is a standing prohibition on producing one - not
	// a sweep that happened once. The next tick plans the group again without
	// them.
	SubmitRecipientErased SubmitOutcome = "recipient_erased"
)

// AdmissionLabel is what happened to one admission, as a metric label.
//
// Every producer labels with this rather than with the outcome alone, because
// the outcome alone hides the one answer an operator most wants separated:
// "created" is also what comes back when the domain accepted an admission that
// promised nothing at all. A page nobody receives and a page on its way are the
// same string until this is applied.
func AdmissionLabel(outcome SubmitOutcome, commitments int) string {
	if outcome == SubmitCreated && commitments == 0 {
		return "no_targets"
	}
	return string(outcome)
}

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

	// ClaimRetriesFirst takes the commitments that have already been attempted
	// before the ones that have not, and falls back to untried work rather
	// than idling when there are no retries.
	//
	// The order is the guarantee. Sorted by due time alone, a hundred fresh
	// pages two hours late sit ahead of one retry that is an hour late, and
	// under a steady stream of new work that retry is never sent at all - the
	// promise to keep trying quietly stops being one.
	ClaimRetriesFirst ClaimPhase = "retries_first"
)

// ClaimRequest is one instance asking for work: which partition, which
// provider, which half of its queue, and how much of it.
//
// WorkerID is audit only - who took the row, for a human reading the journal.
// What actually proves ownership is the token the claim mints, and nothing
// anywhere compares names.
type ClaimRequest struct {
	Family   string
	Provider string
	Phase    ClaimPhase
	Limit    int
	Lease    time.Duration
	WorkerID string
}

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

	// ErrorClass and Summary describe a preparation that failed, and are what
	// the journal keeps instead of an attempt.
	ErrorClass string
	Summary    string
}

// What the attempt IS - create or change, which operation, which revision - is
// not in the request and cannot be. All three follow from what the commitment
// already has: an object that exists is changed rather than created, and the
// revision is the one in the state the key was computed over. A worker that
// could name them could send the content of one revision under the identity of
// another, and the provider would deduplicate the two as one message.
//
// The store decides them under the lock and tells the worker in the result.

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

	// AttemptKind and Operation say what this call has to be. They are decided
	// from the commitment's own state, not asked for.
	AttemptKind AttemptKind
	Operation   Operation

	// BoundEndpoint and ProviderKey are the ones the effect is bound to, which
	// may not be the ones proposed: within one generation the address and the
	// key are what they were when it opened. The worker sends to these.
	BoundEndpoint string
	ProviderKey   string

	// Receipt is where the external object is, for the calls that change one.
	// Empty for a create: there is nothing out there yet.
	//
	// It is the commitment's OWN receipt, read under the same lock as
	// everything else here. A handler never reads a neighbouring one - the text
	// of one message must not depend on which of its siblings has been sent.
	Receipt json.RawMessage

	// ReceiptRef is what the channel calls that object. It travels with the
	// coordinates so that the rules about changing one can say WHICH object
	// without reading a provider's own field names.
	ReceiptRef string

	// Content is what this attempt is made from, in one of the two forms a
	// commitment can have: a frozen snapshot with a revision, or the
	// commitment's own payload with none. The worker never names either: it
	// renders what it is given and reports what the provider said.
	Content AttemptContent
	Payload json.RawMessage
	// PayloadSchemaVersion says which shape the payload is in, so a handler
	// reads it rather than assuming the current one.
	PayloadSchemaVersion int

	// CompletionFingerprintVersion is stamped here, not when the attempt is
	// finished: an attempt can outlive a deployment, and the encoder that
	// closes it has to be the one that opened it.
	CompletionFingerprintVersion int

	// FirstAttemptLatency is how long this commitment waited between being
	// admitted and its first call starting, in seconds. Set only when both are
	// true: this is the FIRST attempt, and the commitment was due immediately.
	//
	// It comes from here rather than from the worker's own clock because both
	// ends of the interval are database timestamps, and a worker subtracting
	// its own time from one of them would be reporting clock drift as latency.
	// Nil is "this measurement does not apply", which is the common case: a
	// retry, or a policy step that was scheduled for later and waited exactly
	// as long as it was told to.
	FirstAttemptLatency *float64

	Intent Intent
}

// FinalizeRequest closes one attempt with what is known about it.
type FinalizeRequest struct {
	AttemptID  string
	LeaseToken string

	// Conclusion is what the attempt concluded: the answer its fingerprint is
	// taken over, the coordinates of whatever the provider made, and the
	// account for the journal - as one value.
	//
	// One value because they are one fact. Carried separately, an acceptance
	// could arrive beside an empty receipt: the commitment settles as done with
	// nothing stored, and the next revision of the alert, finding no receipt,
	// makes a second message beside the one that already exists.
	//
	// The coordinates are kept on the attempt as well as on the commitment,
	// because a later generation clears the commitment's copy and the address
	// of a message that really was sent must not disappear with it.
	Conclusion Conclusion
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

	// ResolveRecipientErased: the person this commitment was for has been
	// erased. Nothing here may be revived - a new attempt would need an
	// address, and putting one back is exactly what the erasure forbade. The
	// only decision left is to withdraw it.
	ResolveRecipientErased ResolveOutcome = "recipient_erased"
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

	// ResultDetail is what the answer PROVED about the object, where it proved
	// anything - which is almost never. It is on the record and on the
	// observation beside it, because whether a late answer arrived before or
	// after recovery closed the attempt is a race, and a fact that appeared in
	// one and not the other would be visible or invisible by luck.
	ResultDetail string
	// Receipt, ReceiptRecorded and ReceiptRedactedAt are the three states an
	// external object can be in, and the journal has to show all three: a
	// receipt that never existed and one whose coordinates were removed by an
	// erasure both read as nil, and they mean opposite things about whether a
	// message went out.
	//
	// The commitment's own HasReceipt cannot answer this. After a new
	// generation it describes the CURRENT effect, and the question a journal
	// answers is about the attempt in front of you - which may be the one that
	// delivered to somebody who has since been erased.
	Receipt           json.RawMessage
	ReceiptRecorded   bool
	ReceiptRedactedAt *time.Time

	Summary      string
	FinishReason string

	CompletionFingerprintVersion int
}

// Observation is a result that arrived for an attempt somebody else had already
// closed - usually a worker returning after recovery gave up on it.
//
// It carries everything the attempt's own conclusion would have carried. A
// message that may have been delivered is reconstructed from this and nothing
// else: a summary of it would leave whoever is deciding what to do about the
// alert guessing at exactly the part that matters.
type Observation struct {
	AttemptID  string
	Kind       string
	ObservedAt time.Time

	Outcome              Outcome
	ErrorClass           string
	ProviderStatus       string
	ProviderResultDetail string
	AppliedRevision      *int64

	// The same three states as an attempt's, for the same reason: a late
	// result is often the only proof a message exists, and after an erasure it
	// is proof without coordinates rather than no proof at all.
	Receipt           json.RawMessage
	ReceiptRecorded   bool
	ReceiptRedactedAt *time.Time

	Summary string

	CompletionFingerprintVersion int
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
