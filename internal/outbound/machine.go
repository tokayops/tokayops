package outbound

import (
	"errors"
	"fmt"
)

// The state machine of a commitment, in one place.
//
// Every point mutation the store makes - a prepared attempt, a finished one, a
// lease recovered after a worker died, an operator's decision - asks this
// function what happens, and then writes exactly what it answers. The reason it
// is a function rather than a set of SQL branches is that the table has to
// exist once: the same rules are checked by a test that walks the entire input
// space, and rules living in five UPDATE statements cannot be walked at all.
//
// Three families of transition are deliberately NOT here, because they are
// set-based by nature and the database performs them over many rows at once:
// expiry of everything that is due, cancellation of everything a group owes
// when it is acknowledged, and the raising of a group's desired state over
// every editable commitment it has. Those are SQL predicates, and each is
// covered by an integration test of its own - the honest cost of the split.
//
// The third is the widest, so its rows are written out here rather than left to
// be read out of one UPDATE. Raising the desired state (D1, T21-T24) does this,
// by the status the commitment is in:
//
//	idle          -> pending, claimable now: it had caught up, and the state
//	                 moved out from under it (T21)
//	pending       -> pending, and its next attempt stays where it was: a
//	                 commitment already on a backoff is coming back anyway, and
//	                 pulling it forward would outrun the wait a provider asked
//	                 for (T22)
//	sending       -> sending: the attempt in flight finishes against the
//	                 revision it started with, and settles into S4 - straight
//	                 back into the queue, because what is desired has moved
//	                 (T23)
//	manual_review -> manual_review: the revision is recorded and a person still
//	                 decides. A commitment revived later aims at the current
//	                 state rather than the one it stopped at (T24)
//
// Terminal commitments are not touched - nothing may come back from one - and
// neither are one-shot ones, which have a single revision by definition.

// ErrInvalidTransition is a statement the machine does not have. It is a
// contract violation, not a runtime condition: the caller asked for something
// the domain cannot do, and doing something adjacent instead would be worse
// than stopping.
var ErrInvalidTransition = errors.New("outbound: invalid transition")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransition, fmt.Sprintf(format, args...))
}

// Trigger is what is asking.
type Trigger string

const (
	// TriggerPreparation is the answer to "can this attempt be made at all",
	// arrived at before any network call.
	TriggerPreparation Trigger = "preparation"

	// TriggerFinishAttempt carries the transport outcome of a call that was
	// made under a live lease.
	TriggerFinishAttempt Trigger = "finish_attempt"

	// TriggerRecoverStale is a lease that expired with an attempt still open:
	// somebody died mid-flight, and what happened externally is unknown.
	TriggerRecoverStale Trigger = "recover_stale"

	// TriggerOperator is a person deciding what a stuck commitment should do.
	TriggerOperator Trigger = "operator"
)

// Decision is what an operator chose.
type Decision string

const (
	DecisionAssumeAccepted         Decision = "assume_accepted"
	DecisionCancel                 Decision = "cancel"
	DecisionRetryCurrentGeneration Decision = "retry_current_generation"
	DecisionRetryNewGeneration     Decision = "retry_new_generation"
)

// TimelineKind is what the alert group's history is told. The wording of a
// success matters: "sent", "delivered" and "assumed delivered" are three
// different claims, and only the first two are claims about the world.
type TimelineKind string

const (
	TimelineNone             TimelineKind = ""
	TimelineSent             TimelineKind = "sent"
	TimelineDelivered        TimelineKind = "delivered"
	TimelineAssumedAccepted  TimelineKind = "assumed_accepted"
	TimelineFailed           TimelineKind = "failed"
	TimelineExpired          TimelineKind = "expired"
	TimelineCanceled         TimelineKind = "canceled"
	TimelineSentAlongsideAck TimelineKind = "sent_alongside_cancel"
	TimelineLeaseLost        TimelineKind = "lease_lost"
)

// Input is everything a transition can depend on.
//
// It is one struct rather than a pile of arguments because the test that proves
// the guards are exhaustive walks this space: every field here is a dimension
// of that walk, and a dimension the caller could pass out of band would be a
// dimension nobody checks.
type Input struct {
	Intent  Intent
	Trigger Trigger

	// Preparation is the answer for TriggerPreparation.
	Preparation PreparationOutcome

	// Outcome is the transport result for TriggerFinishAttempt.
	Outcome Outcome

	// AttemptRevision is the revision the finished attempt was applying, and it
	// becomes the applied revision on success.
	//
	// Nil when the commitment has no revisions at all - one drawn from its own
	// payload rather than from a state that is revised. Not zero: a zero is a
	// revision, and it would compare as "behind revision 1" and put such a
	// commitment back in the queue forever.
	AttemptRevision *int64

	// AttemptIsFinal says the attempt applied the last revision this commitment
	// will ever have - the resolve of an alert, after which the card is done.
	AttemptIsFinal bool

	// Expired says the commitment's own deadline has passed, as of now.
	Expired bool

	// Decision, and the facts its guards need, for TriggerOperator.
	Decision              Decision
	LastAttemptKind       AttemptKind
	AmbiguousInGeneration bool
	AcceptedDuplicateRisk bool
	ResourceLossConfirmed bool
	NewExpiryProvided     bool
}

// Effects are what the store must write, stated as facts rather than as SQL so
// the same answer can be applied by any of the transactions that ask.
type Effects struct {
	OpenGeneration bool
	NewGeneration  bool

	ClearLease          bool
	ClearCurrentAttempt bool
	ConsumeCancellation bool

	ScheduleRetry bool // next attempt after the family's backoff
	ScheduleNow   bool // next attempt immediately

	ResetFailureStreak bool
	BumpFailureStreak  bool

	// Proof is set when the commitment is being settled as applied. It decides
	// what the history is told and whether a risk has to be recorded, and
	// nothing else: an assumed success has exactly the same effects on the rest
	// of the system as a real one, which is what makes it dangerous and why it
	// is written down.
	Proof         Proof
	ApplyRevision bool
	StoreReceipt  bool
	TriggerGroup  bool // the group's first successful send: processing -> triggered

	RecordDuplicateRisk bool
	RaiseFailureSignal  bool

	Timeline TimelineKind
}

// Transition is the answer: where the commitment goes and what has to be
// written with it. Row names the line of the specification this is, so a log or
// a failing test says which rule fired rather than only what happened.
type Transition struct {
	To      Status
	Effects Effects
	Row     string
}

// Decide answers what one commitment does next.
func Decide(in Input) (Transition, error) {
	switch in.Trigger {
	case TriggerPreparation:
		return decidePreparation(in)
	case TriggerFinishAttempt:
		return decideFinish(in)
	case TriggerRecoverStale:
		return decideRecover(in)
	case TriggerOperator:
		return decideOperator(in)
	default:
		return Transition{}, invalidf("unknown trigger %q", in.Trigger)
	}
}

// decidePreparation covers the three ways an attempt can begin: it starts, or
// it is refused for good, or it is refused for now.
//
// The refusals never produce a network attempt record. A started attempt means
// the network might have been called, and one written for a call that provably
// did not happen would have to be resolved as ambiguous later - inventing doubt
// where there was proof.
func decidePreparation(in Input) (Transition, error) {
	if in.Intent.Status != StatusPending {
		return Transition{}, invalidf("preparation from %s", in.Intent.Status)
	}

	switch in.Preparation {
	case PreparationReady:
		return Transition{
			To: StatusSending,
			Effects: Effects{
				// Bound by the FACT, not by the count of journal records: a
				// preparation that never reached the provider adds a record
				// too, and counting those would skip the binding of the first
				// call that really happens.
				OpenGeneration: !in.Intent.GenerationBound,
			},
			Row: "T4",
		}, nil

	case PreparationPermanent:
		return Transition{
			To: StatusPermanentFailed,
			Effects: Effects{
				ClearLease:         true,
				BumpFailureStreak:  true,
				RaiseFailureSignal: true,
				Timeline:           TimelineFailed,
			},
			Row: "T4a",
		}, nil

	case PreparationTransient:
		return Transition{
			To: StatusPending,
			Effects: Effects{
				ClearLease:        true,
				BumpFailureStreak: true,
				ScheduleRetry:     true,
			},
			Row: "T4b",
		}, nil

	default:
		return Transition{}, invalidf("unknown preparation outcome %q", in.Preparation)
	}
}

// decideFinish covers a call that was made and answered under a live lease.
//
// The cancellation flag is read first, and that order is the rule: a withdrawal
// beats any outcome that did not produce an external effect, and beats none
// that did.
func decideFinish(in Input) (Transition, error) {
	if in.Intent.Status != StatusSending {
		return Transition{}, invalidf("finishing an attempt from %s", in.Intent.Status)
	}

	if in.Intent.CancellationRequested {
		if in.Outcome == OutcomeAccepted {
			settled, err := settleApplied(in, ProofAccepted, "T16")
			if err != nil {
				return Transition{}, err
			}
			settled.Effects.ConsumeCancellation = true
			settled.Effects.Timeline = TimelineSentAlongsideAck
			return settled, nil
		}
		if err := knownOutcome(in.Outcome); err != nil {
			return Transition{}, err
		}
		return Transition{
			To: StatusCanceled,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				ConsumeCancellation: true,
				Timeline:            TimelineCanceled,
			},
			Row: "T15",
		}, nil
	}

	switch in.Outcome {
	case OutcomeAccepted:
		return reduceProviderAcceptance(in, "T8")

	case OutcomeRetryableRejection:
		return Transition{
			To: StatusPending,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
				ScheduleRetry:       true,
			},
			Row: "T9",
		}, nil

	case OutcomePermanentRejection:
		return Transition{
			To: StatusPermanentFailed,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
				RaiseFailureSignal:  true,
				Timeline:            TimelineFailed,
			},
			Row: "T10",
		}, nil

	case OutcomeAmbiguous:
		return decideAmbiguous(in)

	case OutcomeCanceled:
		// An attempt closed as canceled without the flag set is a caller
		// inventing a withdrawal nobody asked for.
		return Transition{}, invalidf("an attempt finished as canceled with no cancellation asked for")

	default:
		return Transition{}, invalidf("unknown outcome %q", in.Outcome)
	}
}

// decideAmbiguous is the fork the whole domain exists to make explicit: nobody
// knows whether the message went out, so the policy decides which way to be
// wrong.
func decideAmbiguous(in Input) (Transition, error) {
	switch in.Intent.AmbiguityPolicy {
	case PolicyRetry:
		return Transition{
			To: StatusPending,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
				ScheduleRetry:       true,
			},
			Row: "T11",
		}, nil

	case PolicyReconcileThenRetry:
		// The same move as a plain retry today, and it is only allowed to exist
		// because the next cycle would start with a reconciliation - which no
		// provider here can perform, so nothing may be admitted with it.
		return Transition{
			To: StatusPending,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
				ScheduleRetry:       true,
			},
			Row: "T12",
		}, nil

	case PolicyManualReview:
		return Transition{
			To: StatusManualReview,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
			},
			Row: "T13",
		}, nil

	case PolicyAssumeAccepted:
		if in.Intent.Form != FormOneShot {
			// An editable commitment cannot be assumed delivered automatically:
			// there is nothing to update afterwards, and the card would be
			// stuck at whatever it said when the doubt began.
			return Transition{}, invalidf("assume_accepted is only automatic for one-shot commitments")
		}
		return settleApplied(in, ProofAssumed, "T14")

	default:
		return Transition{}, invalidf("unknown ambiguity policy %q", in.Intent.AmbiguityPolicy)
	}
}

// decideRecover closes a commitment whose worker died with an attempt open.
//
// The attempt itself is recorded as ambiguous by the caller BEFORE this is
// asked, and that order matters: the history of a call whose fate is unknown
// must survive whatever the deadline says next.
func decideRecover(in Input) (Transition, error) {
	if in.Intent.Status != StatusSending {
		return Transition{}, invalidf("recovering from %s", in.Intent.Status)
	}

	if in.Intent.CancellationRequested {
		return Transition{
			To: StatusCanceled,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				ConsumeCancellation: true,
				BumpFailureStreak:   true,
				Timeline:            TimelineCanceled,
			},
			Row: "T7",
		}, nil
	}

	switch in.Intent.AmbiguityPolicy {
	case PolicyManualReview:
		// The deadline does not answer "did it arrive", so an expired
		// commitment still goes to the person rather than to a terminal state
		// that claims to know.
		return Transition{
			To: StatusManualReview,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
				Timeline:            TimelineLeaseLost,
			},
			Row: "T7",
		}, nil

	case PolicyAssumeAccepted:
		if in.Intent.Form != FormOneShot {
			return Transition{}, invalidf("assume_accepted is only automatic for one-shot commitments")
		}
		settled, err := settleApplied(in, ProofAssumed, "T7")
		if err != nil {
			return Transition{}, err
		}
		return settled, nil

	case PolicyRetry, PolicyReconcileThenRetry:
		if in.Expired {
			return Transition{
				To: StatusExpired,
				Effects: Effects{
					ClearLease:          true,
					ClearCurrentAttempt: true,
					BumpFailureStreak:   true,
					Timeline:            TimelineExpired,
				},
				Row: "T7",
			}, nil
		}
		return Transition{
			To: StatusPending,
			Effects: Effects{
				ClearLease:          true,
				ClearCurrentAttempt: true,
				BumpFailureStreak:   true,
				ScheduleRetry:       true,
				Timeline:            TimelineLeaseLost,
			},
			Row: "T7",
		}, nil

	default:
		return Transition{}, invalidf("unknown ambiguity policy %q", in.Intent.AmbiguityPolicy)
	}
}

// reduceProviderAcceptance turns "the provider took it" into what that means
// for this channel.
func reduceProviderAcceptance(in Input, row string) (Transition, error) {
	switch in.Intent.CompletionMode {
	case CompletionOnAcceptance:
		return settleApplied(in, ProofAccepted, row)
	case CompletionOnProviderReceipt:
		// The channel would go on to wait for the provider's own confirmation.
		// Nothing may be admitted in that mode yet, so reaching here means the
		// validation that guarantees it has been bypassed.
		return Transition{}, invalidf("completion mode %s is not reachable in this build",
			in.Intent.CompletionMode)
	default:
		return Transition{}, invalidf("unknown completion mode %q", in.Intent.CompletionMode)
	}
}

// settleApplied is where a success lands, and the four cases are checked in
// order because they are mutually exclusive only in that order.
//
// The effects are the same whatever the proof: an assumed success updates the
// same revision, writes the same receipt and moves the group exactly like a
// real one. Only the history and the recorded risk differ - which is the honest
// way round, because pretending otherwise would make an assumption invisible.
func settleApplied(in Input, proof Proof, row string) (Transition, error) {
	effects := Effects{
		ClearLease:          true,
		ClearCurrentAttempt: true,
		ResetFailureStreak:  true,
		ApplyRevision:       true,
		StoreReceipt:        true,
		TriggerGroup:        true,
		Proof:               proof,
	}

	switch proof {
	case ProofAccepted:
		effects.Timeline = TimelineSent
	case ProofDelivered:
		effects.Timeline = TimelineDelivered
	case ProofAssumed:
		effects.Timeline = TimelineAssumedAccepted
		effects.RecordDuplicateRisk = true
	default:
		return Transition{}, invalidf("unknown proof %q", proof)
	}

	if in.Intent.Form == FormOneShot {
		return Transition{To: StatusSucceeded, Effects: effects, Row: row + "/S1"}, nil
	}

	// Only an editable commitment reaches here, and every one of those is drawn
	// from a state that has revisions - so a missing one is a contradiction
	// rather than a case to handle.
	//
	// Before the final branch, not after it. "This attempt applied the last
	// revision there will be" is a statement ABOUT a revision, and taking it
	// from a caller that could not name one would retire a card on the strength
	// of a claim nothing in the input supports.
	if in.AttemptRevision == nil {
		return Transition{}, invalidf(
			"an editable commitment settled an attempt that applied no revision")
	}

	if in.AttemptIsFinal {
		return Transition{To: StatusSucceeded, Effects: effects, Row: row + "/S2"}, nil
	}

	if *in.AttemptRevision >= in.Intent.DesiredRevision {
		return Transition{To: StatusIdle, Effects: effects, Row: row + "/S3"}, nil
	}

	// The desired state moved while this attempt was in flight, so the
	// commitment goes straight back into the queue rather than waiting to be
	// noticed.
	effects.ScheduleNow = true
	return Transition{To: StatusPending, Effects: effects, Row: row + "/S4"}, nil
}

// decideOperator is the matrix a person acts through. Every row of it is an
// explicit decision with an audit record; none of them is reachable
// automatically.
func decideOperator(in Input) (Transition, error) {
	switch in.Intent.Status {
	case StatusManualReview, StatusPermanentFailed, StatusExpired:
	default:
		return Transition{}, invalidf("no operator decision applies to %s", in.Intent.Status)
	}

	switch in.Decision {
	case DecisionAssumeAccepted:
		return operatorAssume(in)
	case DecisionCancel:
		return operatorCancel(in)
	case DecisionRetryCurrentGeneration:
		return operatorRetryCurrent(in)
	case DecisionRetryNewGeneration:
		return operatorRetryNew(in)
	default:
		return Transition{}, invalidf("unknown decision %q", in.Decision)
	}
}

func operatorAssume(in Input) (Transition, error) {
	if in.Intent.Status != StatusManualReview {
		// From a confirmed refusal or an expiry, "call it delivered" would be a
		// lie rather than a risk: the provider already said no, or the deadline
		// passed with nothing sent.
		return Transition{}, invalidf("assume_accepted from %s would claim something known to be false",
			in.Intent.Status)
	}
	if in.Intent.Form != FormOneShot && !in.Intent.HasReceipt {
		// An editable card with no receipt cannot be assumed delivered: there
		// is no external object to bring to a later revision.
		return Transition{}, invalidf("assume_accepted needs a one-shot commitment or a usable receipt")
	}
	settled, err := settleApplied(in, ProofAssumed, rowFor(in, "T25", "T26"))
	if err != nil {
		return Transition{}, err
	}
	return settled, nil
}

func operatorCancel(in Input) (Transition, error) {
	if in.Intent.Status == StatusExpired {
		// An expiry is already a decided ending. Cancelling it would replace one
		// terminal explanation with another and lose the first.
		return Transition{}, invalidf("cancel from expired: the commitment already ended")
	}
	row := "T27"
	if in.Intent.Status == StatusPermanentFailed {
		row = "T30"
	}
	return Transition{
		To:      StatusCanceled,
		Effects: Effects{Timeline: TimelineCanceled},
		Row:     row,
	}, nil
}

func operatorRetryCurrent(in Input) (Transition, error) {
	if err := throughResolveAmbiguity(in); err != nil {
		return Transition{}, err
	}
	if in.Intent.Status == StatusExpired && !in.NewExpiryProvided {
		// Without a fresh deadline the first claim would expire it again, and
		// the command would be a no-op an operator repeats forever.
		return Transition{}, invalidf("retrying an expired commitment needs a new deadline")
	}
	row := map[Status]string{
		StatusManualReview:    "T28",
		StatusPermanentFailed: "T31",
		StatusExpired:         "T33",
	}[in.Intent.Status]

	return Transition{
		To: StatusPending,
		Effects: Effects{
			ScheduleNow:        true,
			ResetFailureStreak: true,
		},
		Row: row,
	}, nil
}

func operatorRetryNew(in Input) (Transition, error) {
	if err := throughResolveAmbiguity(in); err != nil {
		return Transition{}, err
	}
	if in.LastAttemptKind != AttemptCreate && !in.ResourceLossConfirmed {
		// A new external object may only replace one that was never created or
		// is known to be gone. Otherwise the old one stays behind, unowned.
		return Transition{}, invalidf("a new generation needs a create attempt or a confirmed loss")
	}
	if in.AmbiguousInGeneration && !in.AcceptedDuplicateRisk {
		// Somewhere in this generation a call went out and nobody knows what it
		// did. Making a second object is allowed, but only with the duplicate
		// accepted on the record.
		return Transition{}, invalidf("a new generation after an ambiguous attempt needs the duplicate risk accepted")
	}
	if in.Intent.Status == StatusExpired && !in.NewExpiryProvided {
		return Transition{}, invalidf("retrying an expired commitment needs a new deadline")
	}
	row := map[Status]string{
		StatusManualReview:    "T29",
		StatusPermanentFailed: "T32",
		StatusExpired:         "T34",
	}[in.Intent.Status]

	return Transition{
		To: StatusPending,
		Effects: Effects{
			NewGeneration:       true,
			ScheduleNow:         true,
			ResetFailureStreak:  true,
			RecordDuplicateRisk: in.AcceptedDuplicateRisk,
		},
		Row: row,
	}, nil
}

// throughResolveAmbiguity is the first guard of both operator retries: this
// commitment's kind has to be one whose door to a new effect is this command.
//
// First, before the guards about deadlines and duplicate risk, because a kind
// with another door must be told that and nothing else - an operator refused
// for "no new deadline" would supply one and be refused again for the real
// reason. A webhook commitment is retried by replay, which makes a new
// commitment; reviving this one would be a second live delivery beside it.
func throughResolveAmbiguity(in Input) error {
	door, err := newEffectDoor(in.Intent.KeyKind)
	if err != nil {
		return invalidf("%v", err)
	}
	if door != doorResolveAmbiguity {
		return invalidf("a %s commitment is not retried by an operator; its door to a new effect is %s",
			in.Intent.KeyKind, door)
	}
	return nil
}

// rowFor picks between the one-shot and the editable line of the same decision.
func rowFor(in Input, oneShot, editable string) string {
	if in.Intent.Form == FormOneShot {
		return oneShot
	}
	return editable
}

// knownOutcome refuses a transport outcome nobody defined. It matters most on
// the cancellation path, where the outcome is not otherwise inspected.
func knownOutcome(o Outcome) error {
	switch o {
	case OutcomeAccepted, OutcomeRetryableRejection, OutcomePermanentRejection,
		OutcomeAmbiguous, OutcomeCanceled:
		return nil
	default:
		return invalidf("unknown outcome %q", o)
	}
}
