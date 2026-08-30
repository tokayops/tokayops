package outbound

import "testing"

// The deadlines are not independent numbers. Each one bounds a step of the same
// piece of work, and the relations between them are what keep a commitment from
// being abandoned mid-flight - which is why they are asserted here rather than
// left to whoever edits one of them next.
//
// Every family, not only the one that came first. A second family is a second
// set of the same numbers, and the way it goes wrong is by being written as a
// copy with one value changed.
func TestTheDeadlinesFitInsideEachOther(t *testing.T) {
	for _, family := range []string{FamilyNotification, FamilyHandoff} {
		t.Run(family, func(t *testing.T) {
			p, err := PolicyOf(family)
			if err != nil {
				t.Fatalf("no policy for %s: %v", family, err)
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
	for _, family := range []string{"", "webhook", "Notification"} {
		if _, err := PolicyOf(family); err == nil {
			t.Errorf("%q was given a policy", family)
		}
	}
}
