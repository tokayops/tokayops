package outbound

import (
	"context"
	"fmt"
	"time"
)

// A channel whose acceptance only means "queued".
//
// For Slack, Telegram and a webhook, the provider taking the request IS the
// delivery, and an attempt's answer settles the commitment. A phone call is
// different: the provider accepts the request into its own queue, and only
// later - by a callback, or by being asked - says whether the call was put
// through. Between the two the commitment waits, and what it is waiting for is
// the fate of one or more external objects its attempts made.
//
// The domain folds what the provider says; the channel only translates its
// dialect into the closed set below. The partial order, the sequence numbers and
// the verdict over a generation are decided here once, for every such channel
// there will be - a channel that could decide them could turn a call nobody
// placed into a success.

// EffectState is what is known about one external object an attempt made.
type EffectState string

const (
	// EffectInProgress: the object exists and has not finished - queued at the
	// provider, ringing, in progress.
	EffectInProgress EffectState = "in_progress"
	// EffectHappened: the effect took place - for a call, it was put through
	// and ended, whether or not anybody answered.
	EffectHappened EffectState = "happened"
	// EffectNotPlaced: the provider says it could not make the effect happen -
	// the number was unreachable, the carrier refused.
	EffectNotPlaced EffectState = "not_placed"
	// EffectWithdrawn: this system took the object back before it happened.
	EffectWithdrawn EffectState = "withdrawn"
)

// EffectStates is the closed set, for the doors that read one back.
func EffectStates() []EffectState {
	return []EffectState{EffectInProgress, EffectHappened, EffectNotPlaced, EffectWithdrawn}
}

// Terminal reports whether nothing more can happen to the object.
func (s EffectState) Terminal() bool {
	switch s {
	case EffectHappened, EffectNotPlaced, EffectWithdrawn:
		return true
	default:
		return false
	}
}

// Known reports whether s is one of the closed set.
func (s EffectState) Known() bool {
	for _, known := range EffectStates() {
		if s == known {
			return true
		}
	}
	return false
}

// ProviderEvent is one thing a provider said about one external object, by
// callback or in answer to a poll.
type ProviderEvent struct {
	Provider string
	// AccountScope is the provider account the event belongs to. An event id
	// is unique only within an account.
	AccountScope string
	// EventID is the provider's identity of this event, unique per account. A
	// repeat of it is the same event.
	EventID string
	// AttemptID is the attempt the object was made by, carried by this system
	// to the provider and back: it is written before the network is touched, so
	// an event that overtakes the answer to the request still finds its owner.
	AttemptID string
	// ExternalRef is the provider's name for the object.
	ExternalRef string
	// Sequence orders the events of one object, where the provider numbers
	// them. Nil when it does not - an answer to a poll.
	Sequence *int
	// ProviderStatus is the provider's own word for where the object stands.
	ProviderStatus string
	OccurredAt     time.Time
	// Summary is a short account for the journal. Never an address.
	Summary string
}

// EffectRef names one external object to ask about. The channel finds the
// account to ask with through the attempt, whose binding it made.
type EffectRef struct {
	IntentID    string
	AttemptID   string
	ExternalRef string
}

// EffectTranslator is the half of a ReceiptChannel that reads an event: what
// the store needs to apply one, and nothing that touches the network.
type EffectTranslator interface {
	EffectStateOf(event ProviderEvent) (EffectState, string, bool)
}

// ApplyOutcome is what applying one stored event came to.
type ApplyOutcome string

const (
	// ApplyApplied: the event was folded in, and the commitment moved if it
	// had to.
	ApplyApplied ApplyOutcome = "applied"
	// ApplyIgnored: the event is kept and changed nothing - its commitment had
	// already ended, or its status is one this build does not know.
	ApplyIgnored ApplyOutcome = "ignored"
	// ApplyUnmatched: nothing here made the object it is about, yet. It stays
	// in the inbox and is tried again.
	ApplyUnmatched ApplyOutcome = "unmatched"
	// ApplyAlreadyDone: the event had been applied before.
	ApplyAlreadyDone ApplyOutcome = "already_done"
)

// ApplyResult is the answer to applying one event.
type ApplyResult struct {
	Outcome  ApplyOutcome
	IntentID string
	// To and Row are set when the commitment moved.
	To  Status
	Row string
}

// AwaitingReceipt is a commitment whose wait came round: the objects to ask
// about, the ones whose end has not been heard.
type AwaitingReceipt struct {
	IntentID string
	Provider string
	Effects  []EffectRef
}

// ReceiptChannel is a channel whose acceptance only means "queued".
//
// Two more questions than any channel answers, and still only questions about
// the provider's own dialect.
type ReceiptChannel interface {
	Channel

	// EffectStateOf says which state the event puts its object in, in the
	// domain's words, and the provider's code for the journal. False for a
	// status this build has never seen: the object stays where it was and the
	// breach is counted, rather than a guess deciding whether somebody was
	// called. Pure: no I/O, no clock.
	EffectStateOf(event ProviderEvent) (EffectState, string, bool)

	// Poll asks the provider where one object stands, and answers with the
	// event that says so. An error is a failure to ask, not an answer.
	Poll(ctx context.Context, effect EffectRef) (ProviderEvent, error)
}

// Effect is one external object as the domain folds it.
type Effect struct {
	AttemptID    string
	Generation   int
	ExternalRef  string
	State        EffectState
	LastSequence *int
}

// FoldEffect is what the object becomes when an event puts it in next.
//
// A terminal state absorbs: an object that ended does not start again, and a
// late "ringing" after "completed" is history, not news. Between two
// non-terminal states the provider's numbering decides, where it numbers -
// events are separate requests and arrive in any order. A terminal state wins
// over any non-terminal one whatever its number: it can only be later.
func FoldEffect(current Effect, next EffectState, sequence *int) (Effect, bool) {
	if current.State.Terminal() {
		return current, false
	}
	if !next.Terminal() && current.LastSequence != nil && sequence != nil &&
		*sequence <= *current.LastSequence {
		return current, false
	}
	folded := current
	folded.State = next
	if sequence != nil && (current.LastSequence == nil || *sequence > *current.LastSequence) {
		value := *sequence
		folded.LastSequence = &value
	}
	return folded, folded.State != current.State ||
		!equalSequence(folded.LastSequence, current.LastSequence)
}

func equalSequence(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Verdict is what the effects of a commitment add up to.
type Verdict string

const (
	// VerdictHappened: one of the objects took place.
	VerdictHappened Verdict = "happened"
	// VerdictNotPlaced: every object of the current generation definitely did
	// not take place, and no attempt of it left doubt.
	VerdictNotPlaced Verdict = "not_placed"
	// VerdictUnknown: the wait is over and nothing settled it.
	VerdictUnknown Verdict = "unknown"
	// VerdictStillGoing: something may still happen.
	VerdictStillGoing Verdict = "still_going"
)

// Verdicts is the closed set.
func Verdicts() []Verdict {
	return []Verdict{VerdictHappened, VerdictNotPlaced, VerdictUnknown, VerdictStillGoing}
}

// GenerationFacts is everything the verdict depends on.
type GenerationFacts struct {
	// Effects are every object the commitment's attempts made, of every
	// generation.
	Effects []Effect
	// Generation is the current one.
	Generation int
	// AttemptOpen says an attempt of the current generation is in flight.
	AttemptOpen bool
	// Unaccounted is how many attempts of the current generation may have made
	// an object nobody has heard of: they ended in doubt, and no event has
	// named what they made.
	Unaccounted int
	// DeadlinePassed says the wait for the provider's word is over.
	DeadlinePassed bool
}

// GenerationVerdict is a table read in order: the first row that holds
// decides.
//
//  1. an object of ANY generation happened - the obligation is discharged; a
//     call that did take place, late, is not made again
//  2. an attempt is still open - its answer is not in yet
//  3. every object of this generation did not take place, and no attempt of it
//     left doubt - not placed
//  4. the wait is over - unknown, and the domain assumes the call happened
//  5. otherwise something is still going
//
// Row 4 comes before row 5 on purpose. The last thing heard of an object is
// often "in progress", and when the callback that would have ended it is lost
// and every poll fails, that stale word would keep the commitment waiting
// forever - in exactly the case the deadline exists for.
func GenerationVerdict(f GenerationFacts) (Verdict, error) {
	current := 0
	for _, e := range f.Effects {
		if !e.State.Known() {
			return "", fmt.Errorf("outbound: effect of attempt %s is in %q, which this build does not know",
				e.AttemptID, e.State)
		}
		if e.State == EffectHappened {
			return VerdictHappened, nil
		}
		if e.Generation == f.Generation {
			current++
		}
	}
	if f.AttemptOpen {
		return VerdictStillGoing, nil
	}
	if f.Unaccounted == 0 && current > 0 {
		settled := true
		for _, e := range f.Effects {
			if e.Generation == f.Generation && !(e.State == EffectNotPlaced || e.State == EffectWithdrawn) {
				settled = false
				break
			}
		}
		if settled {
			return VerdictNotPlaced, nil
		}
	}
	if f.DeadlinePassed {
		return VerdictUnknown, nil
	}
	return VerdictStillGoing, nil
}
