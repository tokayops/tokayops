package outbound

import (
	"testing"
	"time"
)

// The deadlines are not independent numbers. Each one bounds a step of the same
// piece of work, and the relations between them are what keep a commitment from
// being abandoned mid-flight - which is why they are asserted here rather than
// left to whoever edits one of them next.
//
// Every family, not only the one that came first. A second family is a second
// set of the same numbers, and the way it goes wrong is by being written as a
// copy with one value changed.
func TestTheDeadlinesFitInsideEachOther(t *testing.T) {
	for _, family := range []string{FamilyNotification, FamilyHandoff, FamilyWebhook} {
		t.Run(family, func(t *testing.T) {
			p, err := PolicyOf(family)
			if err != nil {
				t.Fatalf("no policy for %s: %v", family, err)
			}

			// The third inequality, and the reason the attempt deadline is a
			// sum: inside one attempt a channel may read its configuration and
			// then give the subscriber the longest timeout it may have, and
			// both have to fit with room. For the families with neither term
			// this is AttemptDeadline > 0, which is what it should be.
			if p.AttemptDeadline <= p.ConfigReadBudget+p.MaxSubscriberTimeout {
				t.Errorf("an attempt may take %s, but reading the configuration (%s) and "+
					"the subscriber's timeout (%s) already fill it, so a slow read is taken "+
					"out of the subscriber's promised time",
					p.AttemptDeadline, p.ConfigReadBudget, p.MaxSubscriberTimeout)
			}
			if p.BackoffCap <= 0 {
				t.Errorf("a backoff ceiling of %s is a curve that never stops", p.BackoffCap)
			}

			// The whole chain, against the lease. Not the attempt alone: the
			// lease starts when the commitment is CLAIMED, and everything from
			// resolving an address to writing the result down happens under
			// it. A chain that outgrew the lease would let somebody else
			// reclaim the commitment and start again while this worker is
			// still recording the answer it already has.
			//
			// This is the invariant, and the shutdown one below cannot stand in
			// for it: the shutdown deadline is defined as this same sum, so
			// growing any step grows both sides of that comparison and it stays
			// true while this one breaks.
			chain := p.PrepareDeadline + p.RecordDeadline + p.AttemptDeadline + p.RecordDeadline
			if chain >= p.Lease {
				t.Errorf("one commitment may take %s under a lease of %s, so it can "+
					"still be in flight when somebody else is told to redo it",
					chain, p.Lease)
			}
			// And the shutdown deadline against the same sum. Written as a
			// number rather than as this sum it was once already shorter than
			// the chain it was meant to cover, and a worker that walked away
			// early abandons a call whose answer has just arrived.
			if p.ShutdownDeadline < chain {
				t.Errorf("a stopping worker waits %s for work that may take %s; the "+
					"difference is calls whose answers are thrown away",
					p.ShutdownDeadline, chain)
			}
			if p.AttemptDeadline >= p.Lease {
				t.Errorf("an attempt may run %s under a lease of %s, so it can still be "+
					"running when somebody else is told to redo it",
					p.AttemptDeadline, p.Lease)
			}
			// The lock timeout is one store-wide number rather than a policy
			// field, so it is compared against every family here: what must be
			// true of it is true of all of them at once.
			if OutboundLockTimeout >= p.Lease {
				t.Errorf("a mutation may wait %s for a row under a lease of %s, and would "+
					"then apply a decision that has been reassigned",
					OutboundLockTimeout, p.Lease)
			}
			if p.RecordDeadline <= OutboundLockTimeout {
				t.Errorf("recording gives up after %s while the store waits %s for a "+
					"contended row, so the refusal would come from the context rather "+
					"than the rule", p.RecordDeadline, OutboundLockTimeout)
			}
			if p.PoolSize < 1 {
				t.Errorf("a pool of %d holds no leases, so this family is a queue "+
					"nobody drains", p.PoolSize)
			}
			if p.ClaimInterval <= 0 {
				t.Errorf("a claim interval of %s is a worker that never looks",
					p.ClaimInterval)
			}
			if p.Family != family {
				t.Errorf("the policy for %s calls itself %s", family, p.Family)
			}
		})
	}
}

// TestAFamilyNobodyExecutesHasNoPolicy. The set is closed on purpose: a worker
// started for a partition nothing else knows about would drain claims that no
// metric counts and no alert watches.
func TestAFamilyNobodyExecutesHasNoPolicy(t *testing.T) {
	for _, family := range []string{"", "carrier_pigeon", "Notification"} {
		if _, err := PolicyOf(family); err == nil {
			t.Errorf("%q was given a policy", family)
		}
	}
}

// TestTheBackoffCeilingIsTheFamilys. One curve, three families, and the ceiling
// is the only thing that differs: paging and handovers stop at five minutes
// from the ninth failure exactly as before, webhooks keep doubling through 512
// and 1024 seconds and stop at thirty minutes from the eleventh. Checked at the
// edges of the jitter rather than sampled, and against every family by name so
// that a family-specific change cannot quietly become a general one.
func TestTheBackoffCeilingIsTheFamilys(t *testing.T) {
	cases := []struct {
		family string
		streak int
		base   time.Duration
	}{
		{FamilyNotification, 1, 2 * time.Second},
		{FamilyNotification, 8, 256 * time.Second},
		{FamilyNotification, 9, 5 * time.Minute},
		{FamilyNotification, 100, 5 * time.Minute},
		{FamilyHandoff, 9, 5 * time.Minute},
		{FamilyHandoff, 100, 5 * time.Minute},
		{FamilyWebhook, 8, 256 * time.Second},
		{FamilyWebhook, 9, 512 * time.Second},
		{FamilyWebhook, 10, 1024 * time.Second},
		{FamilyWebhook, 11, 30 * time.Minute},
		{FamilyWebhook, 100, 30 * time.Minute},
	}
	for _, tc := range cases {
		p, err := PolicyOf(tc.family)
		if err != nil {
			t.Fatalf("%s: %v", tc.family, err)
		}
		low := backoffWith(p.BackoffCap, tc.streak, 0)
		high := backoffWith(p.BackoffCap, tc.streak, 1)
		wantLow := time.Duration(float64(tc.base) * 0.8)
		wantHigh := time.Duration(float64(tc.base) * 1.2)
		if low != wantLow || high != wantHigh {
			t.Errorf("%s failure %d: jitter edges %s..%s, want %s..%s",
				tc.family, tc.streak, low, high, wantLow, wantHigh)
		}
		sampled, err := BackoffFor(tc.family, tc.streak)
		if err != nil || sampled < wantLow || sampled > wantHigh {
			t.Errorf("%s failure %d: sampled %s outside %s..%s (%v)",
				tc.family, tc.streak, sampled, wantLow, wantHigh, err)
		}
	}
	if _, err := BackoffFor("carrier_pigeon", 1); err == nil {
		t.Fatal("a family nobody executes was given a backoff")
	}
}

// attemptsInADay runs the schedule a dead subscriber produces: every attempt
// holds a slot for `hold`, then the commitment waits its backoff, until a day is
// over. The jitter is pinned to one edge so the answer is a bound, not a sample.
func attemptsInADay(cap, hold time.Duration, fraction float64) int {
	const day = 24 * time.Hour
	var elapsed time.Duration
	attempts := 0
	for {
		elapsed += hold
		attempts++
		if elapsed > day {
			return attempts - 1
		}
		elapsed += backoffWith(cap, attempts, fraction)
		if elapsed > day {
			return attempts
		}
	}
}

// TestADeadSubscriberCostsThisManyAttemptsADay pins the arithmetic the webhook
// family's profile is built on (D4). The numbers come from running the curve,
// not from a formula: a ceiling moves the point where the curve saturates, and
// two editions of the plan got the sum of the ramp wrong by assuming it did not.
// Both edges of the jitter, because the short edge is the load and the long
// edge is the floor.
func TestADeadSubscriberCostsThisManyAttemptsADay(t *testing.T) {
	const hold = 31 * time.Second
	webhook, _ := PolicyOf(FamilyWebhook)
	paging, _ := PolicyOf(FamilyNotification)
	for _, tc := range []struct {
		name     string
		cap      time.Duration
		fraction float64
		want     int
	}{
		{"webhook, jitter -20%", webhook.BackoffCap, 0, 68},
		{"webhook, jitter +20%", webhook.BackoffCap, 1, 49},
		{"a five-minute ceiling, jitter -20%", paging.BackoffCap, 0, 325},
		{"a five-minute ceiling, jitter +20%", paging.BackoffCap, 1, 227},
	} {
		if got := attemptsInADay(tc.cap, hold, tc.fraction); got != tc.want {
			t.Errorf("%s: %d attempts in a day, the profile says %d", tc.name, got, tc.want)
		}
	}
}
