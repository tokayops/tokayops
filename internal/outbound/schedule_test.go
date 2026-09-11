package outbound

import (
	"fmt"
	"testing"
)

// The scheduler is where fairness either exists or does not, and every failure
// it can have is invisible in production until the day it matters: a provider
// starved by a busier one, a pool running at half capacity, an old retry that
// is never sent because new work keeps arriving. All of them are arithmetic, so
// all of them are testable here.

func totalOf(shares map[string]int) int {
	sum := 0
	for _, n := range shares {
		sum += n
	}
	return sum
}

// TestSlotsGoWhereTheWorkIs is the property the previous scheme broke: a slot
// does not idle while somebody is asking for it, and nobody is held to an
// equal share they cannot use.
func TestSlotsGoWhereTheWorkIs(t *testing.T) {
	cases := []struct {
		name   string
		free   int
		demand map[string]int
		want   map[string]int
	}{{
		name:   "one provider asking for more than the pool",
		free:   8,
		demand: map[string]int{"slack": 100},
		want:   map[string]int{"slack": 8},
	}, {
		name:   "a small demand next to a large one leaves nothing idle",
		free:   8,
		demand: map[string]int{"slack": 1, "telegram": 100},
		want:   map[string]int{"slack": 1, "telegram": 7},
	}, {
		name:   "equal demand splits evenly",
		free:   8,
		demand: map[string]int{"slack": 10, "telegram": 10},
		want:   map[string]int{"slack": 4, "telegram": 4},
	}, {
		name:   "everybody gets what they asked for when the pool is bigger",
		free:   8,
		demand: map[string]int{"slack": 2, "telegram": 3},
		want:   map[string]int{"slack": 2, "telegram": 3},
	}, {
		name:   "a provider asking for nothing is not a candidate",
		free:   4,
		demand: map[string]int{"slack": 0, "telegram": 9},
		want:   map[string]int{"telegram": 4},
	}, {
		name:   "nothing due",
		free:   8,
		demand: map[string]int{},
		want:   map[string]int{},
	}, {
		name:   "no free slots",
		free:   0,
		demand: map[string]int{"slack": 5},
		want:   map[string]int{},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shareSlots(tc.free, tc.demand, 0)
			for provider, want := range tc.want {
				if got[provider] != want {
					t.Errorf("%s got %d slots, want %d (%v)", provider, got[provider], want, got)
				}
			}
			for provider, n := range got {
				if _, expected := tc.want[provider]; !expected && n != 0 {
					t.Errorf("%s got %d slots and was not meant to be served", provider, n)
				}
				if n > tc.demand[provider] {
					t.Errorf("%s got %d slots for %d work", provider, n, tc.demand[provider])
				}
			}
			if total := totalOf(got); total > tc.free {
				t.Errorf("handed out %d of %d slots", total, tc.free)
			}
		})
	}
}

// TestTheLastSlotRotates is starvation at the smallest scale. With one slot and
// two providers asking, somebody loses - and if it is the same one every tick,
// a provider is never served at all while the pool is tight.
func TestTheLastSlotRotates(t *testing.T) {
	demand := map[string]int{"slack": 5, "telegram": 5}
	served := map[string]int{}

	for tick := uint64(0); tick < 6; tick++ {
		shares := shareSlots(1, demand, tick)
		if total := totalOf(shares); total != 1 {
			t.Fatalf("tick %d handed out %d slots", tick, total)
		}
		for provider, n := range shares {
			served[provider] += n
		}
	}

	for _, provider := range []string{"slack", "telegram"} {
		if served[provider] == 0 {
			t.Fatalf("%s was never served in six ticks of a tight pool", provider)
		}
	}
}

// TestWaterFillingTerminates: the loop hands slots back and shares them again,
// and a rule that hands back what it just gave would spin forever.
func TestWaterFillingTerminates(t *testing.T) {
	for free := 1; free <= 32; free++ {
		demand := map[string]int{}
		for i := 0; i < 7; i++ {
			demand[fmt.Sprintf("p%d", i)] = i // one provider asking for nothing
		}
		shares := shareSlots(free, demand, uint64(free))

		total := totalOf(shares)
		want := 0
		for _, n := range demand {
			want += n
		}
		if want > free {
			want = free
		}
		if total != want {
			t.Fatalf("free=%d handed out %d slots, want %d", free, total, want)
		}
	}
}

// TestBothQueuesGetServed. Three to one in favour of work nobody has tried,
// because that is the page somebody is waiting for - but never all of it. A
// commitment that has failed once must still be sent, whatever the rate of new
// work, or the promise to keep trying is not one.
func TestBothQueuesGetServed(t *testing.T) {
	cases := []struct {
		name                      string
		limit, fresh, retries     int
		tick                      uint64
		wantFirst, wantAnythingIn int
	}{
		{"three to one", 8, 10, 10, 0, 6, 2},
		{"the older queue is never squeezed out", 4, 10, 10, 0, 3, 1},
		{"nothing new means all of it to the retries", 4, 0, 10, 0, 0, 4},
		{"no retries means all of it to the new work", 4, 10, 0, 0, 4, 0},
		{"more slots than new work leaves the rest to the retries", 8, 1, 10, 0, 1, 7},
		{"one slot goes to the new work on an even tick", 1, 5, 5, 0, 1, 0},
		{"and to the older queue on the next", 1, 5, 5, 1, 0, 1},
		{"two slots, one each", 2, 5, 5, 0, 1, 1},
		{"nothing to give", 0, 5, 5, 0, 0, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, any := splitPhases(tc.limit, tc.fresh, tc.retries, tc.tick)
			if first != tc.wantFirst || any != tc.wantAnythingIn {
				t.Fatalf("split %d slots as %d/%d, want %d/%d",
					tc.limit, first, any, tc.wantFirst, tc.wantAnythingIn)
			}
			if first+any != tc.limit {
				t.Fatalf("%d of %d slots were left unused", tc.limit-first-any, tc.limit)
			}
			if first > tc.fresh {
				t.Fatalf("asked for %d first attempts when %d exist", first, tc.fresh)
			}
		})
	}
}

// TestAnOldRetryIsAlwaysReached is the liveness claim stated as a property: at
// every pool size, with both queues busy, the older one gets at least one slot.
func TestAnOldRetryIsAlwaysReached(t *testing.T) {
	for limit := 1; limit <= 16; limit++ {
		reached := 0
		for tick := uint64(0); tick < 2; tick++ {
			if _, any := splitPhases(limit, 100, 100, tick); any > 0 {
				reached++
			}
		}
		if reached == 0 {
			t.Fatalf("with %d slots the older queue got nothing in two ticks", limit)
		}
	}
}
