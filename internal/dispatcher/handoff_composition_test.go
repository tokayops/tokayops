package dispatcher

import (
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/schedulerender"
)

func comp(source, groupID string, users ...string) *composition {
	c := composition{Source: source, GroupID: groupID, UserIDs: users}
	return &c
}

func rotationComp(groupID string, users ...string) *composition {
	return comp(schedulerender.SourceRotation, groupID, users...)
}

// obs builds an observation with the same defaults the projection fake uses.
func obs(source, groupID string, users ...string) observation {
	return observe(duty(dutySpec{
		scheduleID: "sched-1",
		source:     source,
		groupID:    groupID,
		users:      users,
	}))
}

func rotationObs(groupID string, users ...string) observation {
	return obs(schedulerender.SourceRotation, groupID, users...)
}

// TestClassify is the decision table of S6-D2 and S6-D15 as a test: what
// happened, and who is told.
func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		prev       *composition
		next       observation
		wantKind   string
		wantNotify []string
	}{
		{
			name: "first observation of a schedule notifies nobody",
			prev: nil, next: rotationObs("g-a", "alice"),
			wantKind: "", wantNotify: nil,
		},
		{
			name: "natural handoff tells the incoming group",
			prev: rotationComp("g-a", "alice"), next: rotationObs("g-b", "bob"),
			wantKind: kindHandoff, wantNotify: []string{"bob"},
		},
		{
			name: "handoff tells only those who were not on duty",
			prev: rotationComp("g-a", "alice", "bob"), next: rotationObs("g-b", "bob", "carol"),
			wantKind: kindHandoff, wantNotify: []string{"carol"},
		},
		{
			name: "somebody added to the group on duty",
			prev: rotationComp("g-b", "bob"), next: rotationObs("g-b", "bob", "dave"),
			wantKind: kindAddedToActiveShift, wantNotify: []string{"dave"},
		},
		{
			name: "somebody removed from the group on duty says nothing",
			prev: rotationComp("g-b", "bob", "dave"), next: rotationObs("g-b", "bob"),
			wantKind: "", wantNotify: nil,
		},
		{
			name: "an unchanged composition says nothing",
			prev: rotationComp("g-a", "alice"), next: rotationObs("g-a", "alice"),
			wantKind: "", wantNotify: nil,
		},
		{
			name:     "an override starting is a handoff to the stand-in",
			prev:     rotationComp("g-a", "alice"),
			next:     observe(overrideDuty("sched-1", "ovr-1", "carol")),
			wantKind: kindHandoff, wantNotify: []string{"carol"},
		},
		{
			name:     "an override ending is a handoff back to the rotation",
			prev:     comp(schedulerender.SourceOverride, "ovr-1", "carol"),
			next:     rotationObs("g-a", "alice"),
			wantKind: kindHandoff, wantNotify: []string{"alice"},
		},
		{
			name: "an override on the very person the rotation had is still a handoff",
			prev: rotationComp("g-a", "alice"),
			next: observe(overrideDuty("sched-1", "ovr-1", "alice")),
			// Nobody new is on duty, so nobody is told - but the transition is
			// real, and the caller records the new composition either way.
			wantKind: kindHandoff, wantNotify: nil,
		},
		{
			name: "coming on call after a stretch with nobody on duty notifies",
			prev: &composition{}, next: rotationObs("g-a", "alice"),
			wantKind: kindHandoff, wantNotify: []string{"alice"},
		},
		{
			name: "nobody on duty notifies nobody",
			prev: rotationComp("g-a", "alice"), next: observe(emptyDuty("sched-1")),
			wantKind: "", wantNotify: nil,
		},
		{
			name: "a group shrinking to nobody notifies nobody",
			prev: rotationComp("g-a", "alice"), next: observe(emptyDuty("sched-1")),
			wantKind: "", wantNotify: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, notify := classify(tc.prev, tc.next)
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
			if strings.Join(notify, ",") != strings.Join(tc.wantNotify, ",") {
				t.Errorf("notify = %v, want %v", notify, tc.wantNotify)
			}
		})
	}
}

// TestClassifyIgnoresMovingBoundaries: a reanchor after a policy edit moves the
// slot without changing who is on duty, and a schedule with a single group
// crosses a boundary every shift with the same people on it. Putting an instant
// in the composition would make both of those look like a handoff.
func TestClassifyIgnoresMovingBoundaries(t *testing.T) {
	prev := rotationComp("g-a", "alice")

	shifted := observe(duty(dutySpec{
		scheduleID: "sched-1",
		source:     schedulerender.SourceRotation,
		groupID:    "g-a",
		users:      []string{"alice"},
		slotStart:  dutyBase.Add(-2 * time.Hour),
		start:      dutyBase.Add(-2 * time.Hour),
		end:        dutyBase.Add(22 * time.Hour),
	}))
	if kind, notify := classify(prev, shifted); kind != "" || notify != nil {
		t.Fatalf("reanchor produced %q for %v, want silence", kind, notify)
	}

	// Three consecutive boundaries of a one-group schedule: the composition
	// never changes, so nothing is ever sent.
	for i := 1; i <= 3; i++ {
		slot := dutyBase.Add(time.Duration(i) * 24 * time.Hour)
		next := observe(duty(dutySpec{
			scheduleID: "sched-1",
			source:     schedulerender.SourceRotation,
			groupID:    "g-a",
			users:      []string{"alice"},
			slotStart:  slot,
		}))
		if kind, notify := classify(prev, next); kind != "" || notify != nil {
			t.Fatalf("boundary %d produced %q for %v, want silence", i, kind, notify)
		}
	}
}

// TestOccurrenceKeyDistinguishesArrivals: the dedup key identifies an event, not
// a state. A composition recurs, and if the key were the state alone, the second
// - entirely legitimate - notification would be swallowed by the unique index
// while the first job was still pending.
func TestOccurrenceKeyDistinguishesArrivals(t *testing.T) {
	slot := func(i int) time.Time { return dutyBase.Add(time.Duration(i) * 24 * time.Hour) }
	arrival := func(groupID string, day int, users ...string) observation {
		return observe(duty(dutySpec{
			scheduleID: "sched-1",
			source:     schedulerender.SourceRotation,
			groupID:    groupID,
			users:      users,
			slotStart:  slot(day),
		}))
	}

	t.Run("a rotation returning to the same group", func(t *testing.T) {
		// A -> B -> C -> A: the last arrival repeats the first composition.
		first := occurrenceKey(kindHandoff, "sched-1", arrival("g-a", 0, "alice"))
		again := occurrenceKey(kindHandoff, "sched-1", arrival("g-a", 3, "alice"))
		if first == again {
			t.Fatalf("both arrivals of group A produced %q; the second would be deduped away", first)
		}
	})

	t.Run("an edit returning to the same members", func(t *testing.T) {
		// [B] -> [B,D] -> [B] -> [B,D]: the same composition arrives twice, at
		// two different assignment boundaries.
		firstAdd := observe(duty(dutySpec{
			scheduleID: "sched-1", source: schedulerender.SourceRotation, groupID: "g-b",
			users: []string{"bob", "dave"}, slotStart: slot(0), start: slot(0).Add(2 * time.Hour),
		}))
		secondAdd := observe(duty(dutySpec{
			scheduleID: "sched-1", source: schedulerender.SourceRotation, groupID: "g-b",
			users: []string{"bob", "dave"}, slotStart: slot(0), start: slot(0).Add(6 * time.Hour),
		}))
		if occurrenceKey(kindAddedToActiveShift, "sched-1", firstAdd) ==
			occurrenceKey(kindAddedToActiveShift, "sched-1", secondAdd) {
			t.Fatal("two edits producing the same members share a dedup key")
		}
	})

	t.Run("the kind is part of the key", func(t *testing.T) {
		// Without it, a lingering added_to_active_shift job would suppress the
		// handoff that follows with the same composition and boundary.
		next := arrival("g-b", 1, "bob", "dave")
		if occurrenceKey(kindHandoff, "sched-1", next) == occurrenceKey(kindAddedToActiveShift, "sched-1", next) {
			t.Fatal("the two kinds share a dedup key")
		}
	})

	t.Run("the same event from two instances is one key", func(t *testing.T) {
		// Two processes observing the same transition must agree, or the dedup
		// key cannot stop the second job.
		a := occurrenceKey(kindHandoff, "sched-1", arrival("g-b", 1, "bob", "dave"))
		b := occurrenceKey(kindHandoff, "sched-1", arrival("g-b", 1, "dave", "bob"))
		if a != b {
			t.Fatalf("the same event keyed as %q and %q", a, b)
		}
	})

	t.Run("a group and an override on the same person differ", func(t *testing.T) {
		// The user set alone is ambiguous: [alice] as a group and an override on
		// alice are different reasons for her to be on duty.
		group := observe(duty(dutySpec{
			scheduleID: "sched-1", source: schedulerender.SourceRotation,
			groupID: "g-a", users: []string{"alice"}, slotStart: slot(0),
		}))
		override := observe(duty(dutySpec{
			scheduleID: "sched-1", source: schedulerender.SourceOverride,
			groupID: "ovr-1", users: []string{"alice"}, slotStart: slot(0),
		}))
		if occurrenceKey(kindHandoff, "sched-1", group) == occurrenceKey(kindHandoff, "sched-1", override) {
			t.Fatal("a rotation group and an override share a dedup key")
		}
	})
}

// TestObserveSortsUsers: one composition must have one spelling, or two
// instances would key the same event differently.
func TestObserveSortsUsers(t *testing.T) {
	got := observe(rotationDuty("sched-1", "g-a", "carol", "alice", "bob"))
	if strings.Join(got.Composition.UserIDs, ",") != "alice,bob,carol" {
		t.Fatalf("user IDs = %v, want them sorted", got.Composition.UserIDs)
	}
}

// TestObserveEmptyForNobodyOnCall: a schedule with nobody on duty yields the
// empty composition, which is a state to record rather than a row to skip.
func TestObserveEmptyForNobodyOnCall(t *testing.T) {
	if got := observe(emptyDuty("sched-1")); !got.Composition.empty() {
		t.Fatalf("composition = %+v, want the empty one", got.Composition)
	}
}
