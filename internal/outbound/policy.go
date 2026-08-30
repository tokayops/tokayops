package outbound

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// The notification family's execution policy: how long a lease lives, how long
// one attempt may take, and how fast a failing commitment is allowed to try
// again.
//
// These numbers bound frequency, never lifetime. A commitment whose delivery is
// retryable lives until it succeeds, is withdrawn, or is decided on by a
// person; what a policy gets to say is how often it may knock, which is the
// difference between a promise and a counter that gives up at eight.

// FamilyNotification is the execution partition paging runs in, and
// FamilyHandoff the one shift changes run in. Claims are taken per family so a
// backlog of one kind of delivery cannot delay another - a hundred schedules
// turning over on the same hour boundary must not stand between an alert and
// the person on call.
const (
	FamilyNotification = "notification"
	FamilyHandoff      = "handoff"
)

const (
	// NotificationLease is how long a claim holds a commitment. Comfortably
	// longer than one attempt so a worker that is merely slow does not have its
	// work taken from underneath it, and short enough that a worker that died
	// is picked up while the page still matters.
	NotificationLease = 90 * time.Second

	// NotificationAttemptDeadline bounds one attempt end to end - the call and
	// whatever enrichment follows it. Strictly inside the lease: an attempt
	// still running after its lease expired is an attempt somebody else has
	// already been told to redo.
	NotificationAttemptDeadline = 25 * time.Second

	// NotificationPoolSize is how many attempts one instance runs at once, and
	// therefore how many leases it may hold: a lease is only taken when a slot
	// is free.
	NotificationPoolSize = 8

	// NotificationClaimInterval is how often the queue is looked at.
	NotificationClaimInterval = time.Second

	// NotificationPrepareDeadline bounds resolving an address, credentials and
	// configuration. That is local work measured in milliseconds; the deadline
	// is here so a dependency that hangs costs one slot for five seconds
	// rather than for the length of a lease.
	NotificationPrepareDeadline = 5 * time.Second

	// NotificationRecordDeadline bounds the two database calls that open and
	// close an attempt. Longer than OutboundLockTimeout, so a contended row is
	// refused by the rule that knows why rather than by a cancelled context,
	// and far shorter than the lease.
	NotificationRecordDeadline = 10 * time.Second

	// NotificationShutdownDeadline is how long a stopping worker waits for the
	// attempts it holds.
	//
	// Spelled as the sum of what one commitment can take rather than as a
	// number, because a number drifts: written as forty seconds it was already
	// shorter than the chain it was meant to cover, and a worker that walked
	// away at forty would abandon a call whose answer had just arrived - the
	// one outcome nobody can recover from afterwards. The slack is for the
	// scheduling between the steps.
	NotificationShutdownDeadline = NotificationPrepareDeadline +
		NotificationRecordDeadline + NotificationAttemptDeadline +
		NotificationRecordDeadline + 10*time.Second
)

// Policy is one family's execution policy, as one value.
//
// A family is a set of numbers that only make sense together: an attempt that
// may run longer than its own lease is an attempt somebody else has already
// been told to redo, and a worker that stops waiting before one commitment's
// chain can finish walks away from a call whose answer has arrived. Passed as
// separate arguments, those relations are things a caller can get wrong; passed
// as a value nobody outside this file can build, they are checked once, here.
type Policy struct {
	// Family is the claim partition this policy governs. Set by PolicyOf, so a
	// policy cannot describe a family it was not taken for.
	Family string

	// Lease is how long a claim holds a commitment.
	Lease time.Duration

	// AttemptDeadline bounds one attempt end to end - the call and whatever
	// enrichment follows it. Strictly inside the lease.
	AttemptDeadline time.Duration

	// PoolSize is how many attempts one instance runs at once, and therefore
	// how many leases it may hold: a lease is only taken when a slot is free.
	PoolSize int

	// ClaimInterval is how often the queue is looked at.
	ClaimInterval time.Duration

	// PrepareDeadline bounds resolving an address, credentials and
	// configuration.
	PrepareDeadline time.Duration

	// RecordDeadline bounds the two database calls that open and close an
	// attempt. Longer than OutboundLockTimeout, so a contended row is refused
	// by the rule that knows why rather than by a cancelled context.
	RecordDeadline time.Duration

	// ShutdownDeadline is how long a stopping worker waits for the attempts it
	// holds. Spelled as the sum of what one commitment can take rather than as
	// a number, because a number drifts.
	ShutdownDeadline time.Duration
}

// PolicyOf is the closed set of families this build executes.
//
// Closed and by name: a worker that took its numbers from a caller could be
// started for a partition nothing else knows about, and its claims would then
// be invisible to every metric and every alert written against the families
// that do exist. An unknown name is a wiring mistake, and it is refused where
// it is made rather than discovered as a queue nobody drains.
func PolicyOf(family string) (Policy, error) {
	switch family {
	case FamilyNotification:
		return Policy{
			Family:           FamilyNotification,
			Lease:            NotificationLease,
			AttemptDeadline:  NotificationAttemptDeadline,
			PoolSize:         NotificationPoolSize,
			ClaimInterval:    NotificationClaimInterval,
			PrepareDeadline:  NotificationPrepareDeadline,
			RecordDeadline:   NotificationRecordDeadline,
			ShutdownDeadline: NotificationShutdownDeadline,
		}, nil
	case FamilyHandoff:
		return Policy{
			Family:           FamilyHandoff,
			Lease:            HandoffLease,
			AttemptDeadline:  HandoffAttemptDeadline,
			PoolSize:         HandoffPoolSize,
			ClaimInterval:    HandoffClaimInterval,
			PrepareDeadline:  HandoffPrepareDeadline,
			RecordDeadline:   HandoffRecordDeadline,
			ShutdownDeadline: HandoffShutdownDeadline,
		}, nil
	default:
		return Policy{}, fmt.Errorf("outbound: %q is not a family this build executes", family)
	}
}

// The handover family's numbers.
//
// Smaller than paging, deliberately. A second worker is a second pool of calls
// to the same providers, so the upper bound on concurrent requests from one
// instance is the two pools added together - and an announcement about coming
// on duty is not urgent in the way a page is. What it must not do is take slots
// away from one.
//
// The claim interval is the other half of that: five seconds rather than one,
// because a hundred announcements arriving together are a burst to work through
// rather than an event to react to.
const (
	// HandoffLease is longer than paging's for the same reason paging's is
	// longer than its attempt: comfortably outside one attempt, and short
	// enough that a worker that died is picked up while the shift it announces
	// is still running.
	HandoffLease = 90 * time.Second

	// HandoffAttemptDeadline is paging's. One message to one person through
	// the same provider takes the same time whichever family asked for it.
	HandoffAttemptDeadline = 25 * time.Second

	// HandoffPoolSize is two. The profile is a hundred announcements due at
	// once, and the deadline they have to meet is minutes rather than seconds,
	// so the pool is sized to drain that without ever being the reason a page
	// waits for a Slack connection.
	HandoffPoolSize = 2

	// HandoffClaimInterval is how often the queue is looked at.
	HandoffClaimInterval = 5 * time.Second

	HandoffPrepareDeadline = NotificationPrepareDeadline
	HandoffRecordDeadline  = NotificationRecordDeadline

	// HandoffShutdownDeadline is the same sum as paging's, over this family's
	// own numbers.
	HandoffShutdownDeadline = HandoffPrepareDeadline +
		HandoffRecordDeadline + HandoffAttemptDeadline +
		HandoffRecordDeadline + 10*time.Second
)

// OutboundLockTimeout bounds how long a point mutation waits for a row somebody
// else is holding.
//
// Not part of a family's policy, and deliberately: it is set once per store,
// on every statement that store runs, so a per-family value would be a number
// the tests could change and the database would never see. What has to be true
// of it is the same for every family, and it is asserted as such - a mutation
// that waited longer than the lease it protects would be applying a decision
// that has already been reassigned to somebody else.
const OutboundLockTimeout = 3 * time.Second

const (
	backoffBase = 2 * time.Second
	backoffCap  = 5 * time.Minute

	// backoffJitter spreads retries that failed together - a provider outage
	// makes every commitment fail at the same instant, and without jitter they
	// would come back in the same instant too.
	backoffJitter = 0.20

	// backoffStreakCap saturates the exponent BEFORE it is raised. A commitment
	// lives until it is delivered, so a hundredth failure is a reachable state,
	// and 2^100 nanoseconds is not a duration.
	backoffStreakCap = 24
)

// Backoff is how long a commitment waits after its failure number `streak`.
//
//	1 -> 2s    4 -> 16s   7 -> 128s
//	2 -> 4s    5 -> 32s   8 -> 256s
//	3 -> 8s    6 -> 64s   9 and beyond -> 5m
//
// The streak counts consecutive failures of the CURRENT effect and is cleared
// by any success. The failures of an effect that has already happened have no
// business slowing down the next one.
func Backoff(streak int) time.Duration {
	if streak < 1 {
		streak = 1
	}
	if streak > backoffStreakCap {
		streak = backoffStreakCap
	}

	raw := float64(backoffBase) * math.Pow(2, float64(streak-1))
	if raw > float64(backoffCap) {
		raw = float64(backoffCap)
	}

	spread := raw * backoffJitter * (2*randomFraction() - 1)
	delay := time.Duration(raw + spread)
	if delay < 0 {
		delay = time.Duration(raw)
	}
	return delay
}

func randomFraction() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A backoff without jitter is worse than one with, and refusing to
		// schedule a retry because the random source blinked would be worse
		// than both.
		return 0.5
	}
	return float64(binary.LittleEndian.Uint64(b[:])>>11) / (1 << 53)
}
