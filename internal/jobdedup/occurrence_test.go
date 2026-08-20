package jobdedup

import (
	"strings"
	"testing"
	"time"
)

func sampleOccurrence() Occurrence {
	return Occurrence{
		Kind:            "handoff",
		ScheduleID:      "sched-1",
		Source:          "rotation",
		GroupID:         "g-a",
		UserIDs:         []string{"u-bob", "u-alice"},
		AssignmentStart: time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		RevisionID:      "rev-7",
	}
}

// TestHandoffOccurrenceGoldenVector pins the encoding to a value.
//
// This is not a test of the digest algorithm. It is the only thing standing
// between an innocent-looking edit - a field reordered, a separator changed, the
// time formatted instead of counted - and a silent change of identity for a
// family whose claims are held forever: every occurrence already announced
// would be announced again under its new spelling, and every one already
// written would sit in the table claiming nothing.
//
// If this test fails and the change was deliberate, the change is a new
// namespace, not a new expected value here.
func TestHandoffOccurrenceGoldenVector(t *testing.T) {
	// Computed from the grammar written in HandoffOccurrence's documentation,
	// independently of the code that implements it, over the occurrence below.
	const want = "sched-1:1264ee81aac654f094a55dd4ad085897d4c7d0304d168f078edb1e1089b9d5c5"

	if got := HandoffOccurrence(sampleOccurrence()).Key(); got != want {
		t.Errorf("key = %q, want %q\nthe encoding changed; if that was intended, it needs a new namespace", got, want)
	}
}

// TestHandoffOccurrenceIsTheSpecItsPolicyDeclares: the constructor is the only
// way to make one, so what it produces is what the schema will see.
func TestHandoffOccurrenceCarriesItsPolicy(t *testing.T) {
	spec := HandoffOccurrence(sampleOccurrence())

	if spec.Namespace() != NamespaceHandoffOccurrence {
		t.Errorf("namespace = %q, want %q", spec.Namespace(), NamespaceHandoffOccurrence)
	}
	if spec.Scope() != ScopeForever {
		t.Errorf("scope = %q, want forever - an occurrence is announced once", spec.Scope())
	}
	if spec.JobType() != "handoff_notify" {
		t.Errorf("job type = %q, want handoff_notify", spec.JobType())
	}
	if !strings.HasPrefix(spec.Key(), "sched-1:") {
		t.Errorf("key = %q, want it to name the schedule it belongs to", spec.Key())
	}
}

// TestHandoffOccurrenceOrderOfUsersDoesNotMatter: two instances project the same
// rows and must arrive at the same identity, whatever order the roster comes
// back in.
func TestHandoffOccurrenceOrderOfUsersDoesNotMatter(t *testing.T) {
	one := sampleOccurrence()
	one.UserIDs = []string{"u-alice", "u-bob", "u-carol"}

	other := sampleOccurrence()
	other.UserIDs = []string{"u-carol", "u-alice", "u-bob"}

	if a, b := HandoffOccurrence(one).Key(), HandoffOccurrence(other).Key(); a != b {
		t.Errorf("the same roster keyed as %q and %q", a, b)
	}
}

// TestHandoffOccurrenceDoesNotSortItsInput: the slice handed in is the
// composition the notifier keeps in its cache across ticks. Sorting it in place
// would reorder what is cached through the array both share - and the cache is
// what decides whether the next tick sees a transition at all.
func TestHandoffOccurrenceDoesNotSortItsInput(t *testing.T) {
	users := []string{"u-carol", "u-alice", "u-bob"}
	occurrence := sampleOccurrence()
	occurrence.UserIDs = users

	HandoffOccurrence(occurrence)

	if strings.Join(users, ",") != "u-carol,u-alice,u-bob" {
		t.Errorf("the caller's slice came back as %v; the constructor sorted it in place", users)
	}
}

// TestHandoffOccurrenceFieldsDoNotBleed: length before value is what keeps one
// field from spilling into the next. Without it these pairs would be the same
// bytes and one occurrence would swallow the other - forever.
func TestHandoffOccurrenceFieldsDoNotBleed(t *testing.T) {
	tests := []struct {
		name string
		a, b func(Occurrence) Occurrence
	}{
		{
			name: "a boundary moved between two neighbouring fields",
			a: func(o Occurrence) Occurrence {
				o.Source, o.GroupID = "a", "bc"
				return o
			},
			b: func(o Occurrence) Occurrence {
				o.Source, o.GroupID = "ab", "c"
				return o
			},
		},
		{
			name: "one member spelled with a separator, or two members",
			a: func(o Occurrence) Occurrence {
				o.UserIDs = []string{"x,y"}
				return o
			},
			b: func(o Occurrence) Occurrence {
				o.UserIDs = []string{"x", "y"}
				return o
			},
		},
		{
			name: "an empty field is not the absence of one",
			a: func(o Occurrence) Occurrence {
				o.GroupID, o.RevisionID = "", "g-a"
				return o
			},
			b: func(o Occurrence) Occurrence {
				o.GroupID, o.RevisionID = "g-a", ""
				return o
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := HandoffOccurrence(tc.a(sampleOccurrence())).Key()
			b := HandoffOccurrence(tc.b(sampleOccurrence())).Key()
			if a == b {
				t.Errorf("two different occurrences share the key %q", a)
			}
		})
	}
}

// TestHandoffOccurrenceSeparatesEveryPart: each part of the occurrence is part
// of its identity, so changing any one of them alone produces another key.
func TestHandoffOccurrenceSeparatesEveryPart(t *testing.T) {
	base := HandoffOccurrence(sampleOccurrence()).Key()

	changes := map[string]func(Occurrence) Occurrence{
		"kind":             func(o Occurrence) Occurrence { o.Kind = "added_to_active_shift"; return o },
		"schedule":         func(o Occurrence) Occurrence { o.ScheduleID = "sched-2"; return o },
		"source":           func(o Occurrence) Occurrence { o.Source = "override"; return o },
		"group":            func(o Occurrence) Occurrence { o.GroupID = "g-b"; return o },
		"roster":           func(o Occurrence) Occurrence { o.UserIDs = []string{"u-alice"}; return o },
		"revision":         func(o Occurrence) Occurrence { o.RevisionID = "rev-8"; return o },
		"assignment start": func(o Occurrence) Occurrence { o.AssignmentStart = o.AssignmentStart.Add(time.Nanosecond); return o },
	}

	for part, change := range changes {
		t.Run(part, func(t *testing.T) {
			if got := HandoffOccurrence(change(sampleOccurrence())).Key(); got == base {
				t.Errorf("changing the %s left the key at %q", part, got)
			}
		})
	}
}

// TestHandoffOccurrenceKeyIsBounded: the row this key lands in is never
// deleted and lives in a b-tree index, which stops at about 2704 bytes per
// entry. A roster spelled out in full would pass that on a large team and the
// insert would start failing - which means handover notifications would stop
// being created at all, for that schedule, silently.
func TestHandoffOccurrenceKeyIsBounded(t *testing.T) {
	small := sampleOccurrence()
	small.UserIDs = []string{"u-alice"}

	large := sampleOccurrence()
	large.UserIDs = make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		large.UserIDs = append(large.UserIDs, "1e1a1b6c-7c8f-4e35-9a1e-000000000000")
	}

	shortKey := HandoffOccurrence(small).Key()
	longKey := HandoffOccurrence(large).Key()

	if len(shortKey) != len(longKey) {
		t.Errorf("key length follows the roster: %d for one member, %d for 500", len(shortKey), len(longKey))
	}
	if len(longKey) > 128 {
		t.Errorf("key is %d bytes, which is not a bounded key", len(longKey))
	}
}
