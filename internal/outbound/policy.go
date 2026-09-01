package outbound

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
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
	// FamilyWebhook is the partition outgoing webhooks run in. Its own because
	// the slot scheduler is fair by count and paging pays in time: a call to a
	// subscriber that neither answers nor refuses holds a slot for the whole of
	// its timeout, two orders of magnitude longer than a direct message.
	FamilyWebhook = string(keys.FamilyWebhook)
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

	// ConfigReadBudget bounds the channel's own read of its configuration
	// INSIDE an attempt, where one is needed: the webhook channel reads the
	// subscriber's secret and timeout from the database before it posts. Zero
	// for the families whose channels read nothing there. It is a separate
	// context inside the attempt, not a share of it, so a slow read costs the
	// read and never the subscriber's promised timeout.
	ConfigReadBudget time.Duration

	// MaxSubscriberTimeout is the longest HTTP timeout a subscriber may be
	// given, and the third term the attempt deadline has to exceed. Zero for
	// the families that have no subscribers.
	MaxSubscriberTimeout time.Duration

	// Expiry is how long a commitment of this family stays owed, measured from
	// admission, where the family fixes that rather than the producer. Zero
	// means the family does not: an escalation is withdrawn by an
	// acknowledgement, a handover carries a deadline of its own shape.
	Expiry time.Duration

	// BackoffCap is where this family's retry curve stops growing. The curve
	// is one; the ceiling is the family's, because what it costs to knock on a
	// dead subscriber's door every five minutes for a day is not what it costs
	// to knock on Slack's.
	BackoffCap time.Duration
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
			BackoffCap:       backoffCap,
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
			BackoffCap:       backoffCap,
		}, nil
	case FamilyWebhook:
		return Policy{
			Family:               FamilyWebhook,
			Lease:                WebhookLease,
			AttemptDeadline:      WebhookAttemptDeadline,
			PoolSize:             WebhookPoolSize,
			ClaimInterval:        WebhookClaimInterval,
			PrepareDeadline:      WebhookPrepareDeadline,
			RecordDeadline:       WebhookRecordDeadline,
			ShutdownDeadline:     WebhookShutdownDeadline,
			ConfigReadBudget:     WebhookConfigReadBudget,
			MaxSubscriberTimeout: WebhookMaxSubscriberTimeout,
			Expiry:               WebhookExpiry,
			BackoffCap:           WebhookBackoffCap,
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

// The webhook family's numbers.
//
// Sized for the expensive case and not for the healthy one. Healthy webhook
// traffic is trivial; what costs is a subscriber that neither answers nor
// refuses - a black hole - holding a slot for the whole of its timeout. The
// profile these numbers are computed against is in the gates (D4): up to 140
// live commitments to dead addresses per instance, a burst of 80 due at once,
// a slot held no longer than 31 seconds.
const (
	// WebhookLease is the old outbox worker's lease, kept: the chain of one
	// commitment is 5 + 10 + 35 + 10 = 60 seconds and fits with room.
	WebhookLease = 120 * time.Second

	// WebhookMaxSubscriberTimeout is the longest a subscriber may be given to
	// answer. The API refuses more; a value saved before the limit is clamped
	// by the channel at every read before the call.
	WebhookMaxSubscriberTimeout = 30 * time.Second

	// WebhookConfigReadBudget bounds the channel's read of the subscriber's
	// configuration inside the attempt, as its own context.
	WebhookConfigReadBudget = 2 * time.Second

	// WebhookAttemptDeadline is a SUM, and has to be: the read, the longest
	// timeout a subscriber may have, and slack for the scheduling between them.
	// Written as a number it would quietly promise the subscriber thirty
	// seconds and deliver fewer whenever the read was slow, and a receiver
	// answering on its 29th second would be cut off and recorded as doubt.
	WebhookAttemptDeadline = WebhookConfigReadBudget + WebhookMaxSubscriberTimeout + 3*time.Second

	// WebhookPoolSize is eight. Slots are shared by provider and this family
	// has one, so all eight can go to one dead subscriber; that is accepted,
	// and it is why the number is sized against the black-hole profile rather
	// than against healthy traffic.
	WebhookPoolSize = 8

	// WebhookClaimInterval is the old outbox worker's. An event to a subscriber
	// is not urgent the way a page is.
	WebhookClaimInterval = 2 * time.Second

	WebhookPrepareDeadline = NotificationPrepareDeadline
	WebhookRecordDeadline  = NotificationRecordDeadline

	// WebhookShutdownDeadline is the same sum as the others, over this family's
	// numbers.
	WebhookShutdownDeadline = WebhookPrepareDeadline +
		WebhookRecordDeadline + WebhookAttemptDeadline +
		WebhookRecordDeadline + 10*time.Second

	// WebhookExpiry is how long an event stays owed to a subscriber. A day is a
	// product decision: an alert event a day late tells the receiving system
	// about an incident that is over, the subscriber has the REST API to
	// recover from, and a day covers an ordinary maintenance window. It enters
	// the fingerprint of every webhook commitment, so it is fixed here and not
	// tuned later.
	WebhookExpiry = 24 * time.Hour

	// WebhookBackoffCap is where the retry curve stops for this family. Thirty
	// minutes rather than the five of the others: a subscriber that is dead for
	// a day would otherwise be knocked on some 270 times, holding a slot for
	// its whole timeout each time. Not in the fingerprint; tunable.
	WebhookBackoffCap = 30 * time.Minute
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

// BackoffFor is how long a commitment of a family waits after its failure
// number `streak`.
//
//	1 -> 2s    4 -> 16s   7 -> 128s
//	2 -> 4s    5 -> 32s   8 -> 256s
//	3 -> 8s    6 -> 64s   9 and beyond -> the family's ceiling
//
// The curve is one for every family; the ceiling is the family's (D4). At five
// minutes it is reached from the ninth failure; at thirty, the curve keeps
// doubling through 512 and 1024 seconds and the ceiling is reached from the
// eleventh. A ceiling therefore moves the point of saturation too, which is
// why the number of attempts a dead subscriber costs is taken by running this
// function and not by a formula.
//
// The streak counts consecutive failures of the CURRENT effect and is cleared
// by any success. The failures of an effect that has already happened have no
// business slowing down the next one.
//
// A family this build does not know is refused rather than given the default
// curve: the row that named it is a row nothing here understands.
func BackoffFor(family string, streak int) (time.Duration, error) {
	policy, err := PolicyOf(family)
	if err != nil {
		return 0, err
	}
	return backoffWith(policy.BackoffCap, streak, randomFraction()), nil
}

// backoffWith is the curve with the random half taken as an argument, so the
// two edges of the jitter can be pinned in a test rather than sampled.
func backoffWith(cap time.Duration, streak int, fraction float64) time.Duration {
	if streak < 1 {
		streak = 1
	}
	if streak > backoffStreakCap {
		streak = backoffStreakCap
	}

	raw := float64(backoffBase) * math.Pow(2, float64(streak-1))
	if raw > float64(cap) {
		raw = float64(cap)
	}

	spread := raw * backoffJitter * (2*fraction - 1)
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
