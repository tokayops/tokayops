package outbound

import (
	"crypto/rand"
	"encoding/binary"
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

	// NotificationShutdownDeadline is how long a stopping worker waits for the
	// attempts it holds. Longer than one attempt because the alternative is
	// abandoning a call whose result is then unknowable - the expensive
	// outcome, not the tidy one.
	NotificationShutdownDeadline = 40 * time.Second

	// NotificationPoolSize is how many attempts one instance runs at once, and
	// therefore how many leases it may hold: a lease is only taken when a slot
	// is free.
	NotificationPoolSize = 8

	// NotificationClaimInterval is how often the queue is looked at.
	NotificationClaimInterval = time.Second

	// NotificationLockTimeout bounds how long a point mutation waits for a row
	// somebody else is holding. Well inside the lease, because a mutation that
	// waited longer than the lease it is protecting would be applying a
	// decision that has already been reassigned.
	NotificationLockTimeout = 3 * time.Second
)

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
