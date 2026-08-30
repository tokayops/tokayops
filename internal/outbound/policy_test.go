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

			chain := p.PrepareDeadline + p.RecordDeadline + p.AttemptDeadline + p.RecordDeadline
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
			if p.LockTimeout >= p.Lease {
				t.Errorf("a mutation may wait %s for a row under a lease of %s, and would "+
					"then apply a decision that has been reassigned",
					p.LockTimeout, p.Lease)
			}
			if p.RecordDeadline <= p.LockTimeout {
				t.Errorf("recording gives up after %s while the store waits %s for a "+
					"contended row, so the refusal would come from the context rather "+
					"than the rule", p.RecordDeadline, p.LockTimeout)
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
