package schedulerender

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// TestMergeProducesNaturalShifts: consecutive slots served by the same group
// become one shift, and the shift says how many slots are underneath.
func TestMergeProducesNaturalShifts(t *testing.T) {
	start := utc(2026, 5, 4, 11, 0) // Monday
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", weeklyPolicy("11:00", 1), group(groupA, "alice"), group(groupB, "bob"))})

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 25, 11, 0)})
	shifts := MergeAdjacent(assignmentsOf(res, LayerL1))

	if len(shifts) != 3 {
		t.Fatalf("got %d shifts over three weeks, want 3", len(shifts))
	}
	for i, s := range shifts {
		if s.SlotCount != 1 {
			t.Fatalf("shift %d covers %d slots, want 1", i, s.SlotCount)
		}
		if s.End.Sub(s.Start) != 7*24*time.Hour {
			t.Fatalf("shift %d lasts %v, want a week", i, s.End.Sub(s.Start))
		}
	}
	if shifts[0].GroupID == shifts[1].GroupID {
		t.Fatal("consecutive weekly shifts served by the same group")
	}
}

// TestMergeCountsDistinctSlots: a group serving two slots in a row merges
// into one shift spanning two slots, and SlotCount says two.
func TestMergeCountsDistinctSlots(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	// A single group serves every slot, so the whole range is one shift.
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 4, 11, 0)})
	shifts := MergeAdjacent(assignmentsOf(res, LayerL1))

	if len(shifts) != 1 {
		t.Fatalf("got %d shifts, want 1 continuous run", len(shifts))
	}
	if shifts[0].SlotCount != 3 {
		t.Fatalf("SlotCount = %d, want 3 daily slots", shifts[0].SlotCount)
	}
	if !shifts[0].GridSlotStart.Equal(start) || !shifts[0].GridSlotEnd.Equal(utc(2026, 5, 4, 11, 0)) {
		t.Fatalf("grid span = %v..%v, want first and last slot boundary",
			shifts[0].GridSlotStart, shifts[0].GridSlotEnd)
	}
}

// TestMetadataOnlySaveDoesNotSplitAShift is the property paired with the
// rotation-core carry invariant: a save that provably cannot move the
// rotation must not be visible in the rendered shifts either.
//
// It is compared with SameShifts, which ignores provenance: the save creates
// a revision by definition, so requiring the revision IDs to match would make
// the property unsatisfiable rather than strict.
func TestMetadataOnlySaveDoesNotSplitAShift(t *testing.T) {
	start := utc(2026, 5, 4, 11, 0)
	save := utc(2026, 5, 6, 9, 30)
	until := utc(2026, 5, 18, 11, 0)

	before := config("UTC", weeklyPolicy("11:00", 1), group(groupA, "alice"), group(groupB, "bob"))
	before.SlackUsergroupID = "S-OLD"
	after := before
	after.SlackUsergroupID = "S-NEW"

	unchanged := renderOf(t, Input{
		Root:      root(start),
		Revisions: chain(t, revisionStep{at: start, cfg: before}),
		From:      start, Until: until,
	})
	saved := renderOf(t, Input{
		Root: root(start),
		Revisions: chain(t,
			revisionStep{at: start, cfg: before},
			revisionStep{at: save, cfg: after}),
		From: start, Until: until,
	})

	wantShifts := MergeAdjacent(assignmentsOf(unchanged, LayerL1))
	gotShifts := MergeAdjacent(assignmentsOf(saved, LayerL1))
	if !SameShifts(gotShifts, wantShifts) {
		t.Fatalf("a metadata-only save changed the rendered shifts:\n got %+v\nwant %+v", gotShifts, wantShifts)
	}

	// The atomic assignments do differ - the save is a real revision
	// boundary - and both revisions are recorded as provenance.
	if len(assignmentsOf(saved, LayerL1)) <= len(assignmentsOf(unchanged, LayerL1)) {
		t.Fatal("the save left no trace at all in the atomic assignments")
	}
	var spanning int
	for _, s := range gotShifts {
		if len(s.RevisionIDs) > 1 {
			spanning++
		}
	}
	if spanning != 1 {
		t.Fatalf("%d shifts record both revisions, want exactly the one the save fell in", spanning)
	}
}

// TestMergeKeepsOverridesApart: two different overrides back to back are two
// shifts even when the same person holds both.
func TestMergeKeepsOverridesApart(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	first := override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 13, 0), utc(2026, 5, 1, 15, 0), start)
	second := override("ovr-2", LayerL1, "carol", utc(2026, 5, 1, 15, 0), utc(2026, 5, 1, 17, 0), start)

	res := renderOf(t, Input{
		Root: root(start), Revisions: revs,
		Overrides: []scheduleconfig.OverrideRevision{first, second},
		From:      start, Until: utc(2026, 5, 2, 11, 0),
	})

	var overrideShifts int
	for _, s := range MergeAdjacent(assignmentsOf(res, LayerL1)) {
		if s.Source == SourceOverride {
			overrideShifts++
		}
	}
	if overrideShifts != 2 {
		t.Fatalf("got %d override shifts, want the two overrides kept apart", overrideShifts)
	}
}

func TestMergeDoesNotMutateInput(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice", "amy"))})
	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 3, 11, 0)})

	assignments := assignmentsOf(res, LayerL1)
	original := append([]string(nil), assignments[0].UserIDs...)

	shifts := MergeAdjacent(assignments)
	shifts[0].UserIDs[0] = "tampered"

	if !equalIDs(assignments[0].UserIDs, original) {
		t.Fatal("a merged shift aliases the assignment it came from")
	}
}
