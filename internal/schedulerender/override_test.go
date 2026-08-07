package schedulerender

import (
	"math/rand"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// TestOverrideSplitsTheSlot is the basic overlay: the rotation resumes after
// the override ends, inside the same grid slot.
func TestOverrideSplitsTheSlot(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})
	ovr := override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start)

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs, Overrides: []scheduleconfig.OverrideRevision{ovr},
		From: start, Until: utc(2026, 5, 2, 11, 0),
	})

	l1 := assignmentsOf(res, LayerL1)
	if len(l1) != 3 {
		t.Fatalf("got %d assignments, want rotation/override/rotation", len(l1))
	}
	want := []struct {
		source     string
		start, end time.Time
	}{
		{SourceRotation, start, utc(2026, 5, 1, 14, 0)},
		{SourceOverride, utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0)},
		{SourceRotation, utc(2026, 5, 1, 18, 0), utc(2026, 5, 2, 11, 0)},
	}
	for i, w := range want {
		if l1[i].Source != w.source || !l1[i].AssignmentStart.Equal(w.start) || !l1[i].AssignmentEnd.Equal(w.end) {
			t.Fatalf("assignment %d = %s %v..%v, want %s %v..%v",
				i, l1[i].Source, l1[i].AssignmentStart, l1[i].AssignmentEnd, w.source, w.start, w.end)
		}
		// Every piece still reports the slot it belongs to.
		if !l1[i].GridSlotStart.Equal(start) {
			t.Fatalf("assignment %d grid slot start = %v, want %v", i, l1[i].GridSlotStart, start)
		}
	}
}

// TestOverrideSurvivesLayerBeingDisabled is the semantics of an active
// override outliving the edit that switched its layer off. Skipping an
// inactive layer before applying overrides - the obvious reading of the
// algorithm - would make the override silently disappear.
func TestOverrideSurvivesLayerBeingDisabled(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	disable := utc(2026, 5, 1, 15, 0)

	enabled := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	disabled := enabled
	disabled.L1 = rotation.LayerConfiguration{Enabled: false, Policy: dailyPolicy("11:00")}

	revs := chain(t,
		revisionStep{at: start, cfg: enabled},
		revisionStep{at: disable, cfg: disabled},
	)
	ovr := override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start)

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs, Overrides: []scheduleconfig.OverrideRevision{ovr},
		From: start, Until: utc(2026, 5, 2, 11, 0),
	})

	// The override must still cover its whole validity, across the boundary.
	covered := coverage(assignmentsOf(res, LayerL1), SourceOverride)
	if !covered.Start.Equal(utc(2026, 5, 1, 14, 0)) || !covered.End.Equal(utc(2026, 5, 1, 18, 0)) {
		t.Fatalf("override covers %v..%v, want its full validity 14:00..18:00", covered.Start, covered.End)
	}
	// After the override ends there is no rotation to fall back to.
	for _, a := range assignmentsOf(res, LayerL1) {
		if a.Source == SourceRotation && !a.AssignmentEnd.After(disable) {
			continue
		}
		if a.Source == SourceRotation && a.AssignmentStart.After(disable) {
			t.Fatalf("rotation assignment at %v on a disabled layer", a.AssignmentStart)
		}
	}
}

// TestOverrideSurvivesGroupsBeingEmptied is the same rule reached the other
// way: the layer stays enabled but loses every group.
func TestOverrideSurvivesGroupsBeingEmptied(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	clear := utc(2026, 5, 1, 15, 0)

	staffed := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	emptied := staffed
	emptied.L1 = rotation.LayerConfiguration{Enabled: true, Policy: dailyPolicy("11:00")}

	revs := chain(t,
		revisionStep{at: start, cfg: staffed},
		revisionStep{at: clear, cfg: emptied},
	)
	ovr := override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start)

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs, Overrides: []scheduleconfig.OverrideRevision{ovr},
		From: start, Until: utc(2026, 5, 2, 11, 0),
	})

	covered := coverage(assignmentsOf(res, LayerL1), SourceOverride)
	if !covered.Start.Equal(utc(2026, 5, 1, 14, 0)) || !covered.End.Equal(utc(2026, 5, 1, 18, 0)) {
		t.Fatalf("override covers %v..%v, want 14:00..18:00", covered.Start, covered.End)
	}
}

// TestOverrideOnDisabledL2 covers a layer that never had a rotation in the
// queried range at all.
func TestOverrideOnDisabledL2(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})
	ovr := override("ovr-l2", LayerL2, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start)

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs, Overrides: []scheduleconfig.OverrideRevision{ovr},
		From: start, Until: utc(2026, 5, 2, 11, 0),
	})

	l2 := assignmentsOf(res, LayerL2)
	if len(l2) != 1 {
		t.Fatalf("got %d L2 assignments, want the override alone", len(l2))
	}
	if l2[0].Source != SourceOverride || l2[0].UserIDs[0] != "carol" {
		t.Fatalf("L2 assignment = %s %v, want the override", l2[0].Source, l2[0].UserIDs)
	}
	// It still reports the grid slot it fell in, taken from the disabled
	// layer's own policy.
	if l2[0].GridSlotStart.IsZero() || !l2[0].GridSlotEnd.After(l2[0].GridSlotStart) {
		t.Fatalf("L2 override has no grid slot: %v..%v", l2[0].GridSlotStart, l2[0].GridSlotEnd)
	}
	// An L1 override must not leak into L2 and vice versa.
	for _, a := range assignmentsOf(res, LayerL1) {
		if a.Source == SourceOverride {
			t.Fatal("an L2 override was applied to L1")
		}
	}
}

// TestOverrideCrossesRevisionBoundary: the override keeps its identity but is
// reported per revision, because each side belongs to a different one.
func TestOverrideCrossesRevisionBoundary(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 1, 16, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: edit, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)
	ovr := override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start)

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs, Overrides: []scheduleconfig.OverrideRevision{ovr},
		From: start, Until: utc(2026, 5, 2, 11, 0),
	})

	var pieces []Assignment
	for _, a := range assignmentsOf(res, LayerL1) {
		if a.Source == SourceOverride {
			pieces = append(pieces, a)
		}
	}
	if len(pieces) != 2 {
		t.Fatalf("got %d override pieces, want one per revision", len(pieces))
	}
	if pieces[0].ScheduleRevisionID == pieces[1].ScheduleRevisionID {
		t.Fatal("both override pieces claim the same revision")
	}
	if !pieces[0].AssignmentEnd.Equal(edit) || !pieces[1].AssignmentStart.Equal(edit) {
		t.Fatalf("override pieces do not meet at the revision boundary: %v / %v",
			pieces[0].AssignmentEnd, pieces[1].AssignmentStart)
	}
	// Merged for display they are one continuous override again.
	shifts := MergeAdjacent(pieces)
	if len(shifts) != 1 {
		t.Fatalf("merged into %d shifts, want 1", len(shifts))
	}
	if len(shifts[0].RevisionIDs) != 2 {
		t.Fatalf("merged shift records %v, want both revisions", shifts[0].RevisionIDs)
	}
}

// TestOverlappingOverridesResolveDeterministically pins the rule: the later
// recorded override wins, and the reader is told the two collided.
func TestOverlappingOverridesResolveDeterministically(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	first := override("ovr-first", LayerL1, "carol", utc(2026, 5, 1, 13, 0), utc(2026, 5, 1, 17, 0), start)
	second := override("ovr-second", LayerL1, "dave", utc(2026, 5, 1, 15, 0), utc(2026, 5, 1, 19, 0), start.Add(time.Hour))

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs, Overrides: []scheduleconfig.OverrideRevision{first, second},
		From: start, Until: utc(2026, 5, 2, 11, 0),
	})

	at := utc(2026, 5, 1, 16, 0)
	found := assignmentAt(assignmentsOf(res, LayerL1), at)
	if found == nil || found.OverrideID != "ovr-second" {
		t.Fatalf("at %v the winner is %v, want the later recorded override", at, found)
	}
	if !hasWarning(res, WarnOverrideOverlap) {
		t.Fatalf("warnings = %v, want override_overlap", warningCodes(res))
	}
}

// TestOverlapResolutionIsPermutationInvariant: the answer must not depend on
// the order the projection happened to return the overrides in.
func TestOverlapResolutionIsPermutationInvariant(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	sets := [][]scheduleconfig.OverrideRevision{
		{
			override("ovr-a", LayerL1, "carol", utc(2026, 5, 1, 13, 0), utc(2026, 5, 1, 17, 0), start),
			override("ovr-b", LayerL1, "dave", utc(2026, 5, 1, 15, 0), utc(2026, 5, 1, 19, 0), start.Add(time.Hour)),
		},
		{
			override("ovr-a", LayerL1, "carol", utc(2026, 5, 1, 13, 0), utc(2026, 5, 1, 20, 0), start),
			override("ovr-b", LayerL1, "dave", utc(2026, 5, 1, 15, 0), utc(2026, 5, 1, 19, 0), start.Add(time.Hour)),
			// Same recorded_at as ovr-b: the tie must break on the ID, not on
			// the position in the slice.
			override("ovr-c", LayerL1, "erin", utc(2026, 5, 1, 16, 0), utc(2026, 5, 1, 18, 0), start.Add(time.Hour)),
		},
	}

	rng := rand.New(rand.NewSource(1))
	for setIdx, set := range sets {
		want := renderOf(t, Input{
			Root: root(start), Revisions: revs, Overrides: set,
			From: start, Until: utc(2026, 5, 2, 11, 0),
		})
		wantShifts := MergeAdjacent(want.Assignments)

		for attempt := 0; attempt < 20; attempt++ {
			shuffled := append([]scheduleconfig.OverrideRevision(nil), set...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

			got := renderOf(t, Input{
				Root: root(start), Revisions: revs, Overrides: shuffled,
				From: start, Until: utc(2026, 5, 2, 11, 0),
			})
			if !SameShifts(MergeAdjacent(got.Assignments), wantShifts) {
				t.Fatalf("set %d attempt %d: a different input order produced a different answer", setIdx, attempt)
			}
		}
	}
}

// coverage returns the span from the first to the last assignment of a source,
// which is enough to check an override was not truncated.
func coverage(assignments []Assignment, source string) interval {
	var out interval
	for _, a := range assignments {
		if a.Source != source {
			continue
		}
		if out.Start.IsZero() || a.AssignmentStart.Before(out.Start) {
			out.Start = a.AssignmentStart
		}
		if a.AssignmentEnd.After(out.End) {
			out.End = a.AssignmentEnd
		}
	}
	return out
}
