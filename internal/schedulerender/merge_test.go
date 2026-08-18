package schedulerender

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
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
		if s.End.Sub(s.Start) != 7*24*time.Hour {
			t.Fatalf("shift %d lasts %v, want a week", i, s.End.Sub(s.Start))
		}
	}
	if shifts[0].GroupID == shifts[1].GroupID {
		t.Fatal("consecutive weekly shifts served by the same group")
	}
}

// A group serving several slots in a row is one continuous shift, spanning
// them all. How many slots are underneath is not part of the answer: a shift
// is a run of duty, and the grid that produced it is a separate question with
// a separate type.
func TestMergeJoinsConsecutiveSlotsIntoOneRun(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	// A single group serves every slot, so the whole range is one shift.
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	until := utc(2026, 5, 4, 11, 0)
	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: until})
	shifts := MergeAdjacent(assignmentsOf(res, LayerL1))

	if len(shifts) != 1 {
		t.Fatalf("got %d shifts, want 1 continuous run", len(shifts))
	}
	if !shifts[0].Start.Equal(start) || !shifts[0].End.Equal(until) {
		t.Fatalf("run = %v..%v, want the whole range", shifts[0].Start, shifts[0].End)
	}
}

// TestMetadataOnlySaveDoesNotSplitAShift is the property paired with the
// rotation-core carry invariant: a save that provably cannot move the
// rotation must not be visible in the rendered shifts either.
//
// It is a property rather than one example because the invariant has to hold
// across the whole space the phase pair is carried over - both cadences, both
// group counts, DST and non-DST zones - and for a save landing anywhere in a
// shift, including exactly on a handoff boundary, where a naive renderer
// would double-advance.
//
// Shifts are compared with sameShifts, which ignores provenance: the save
// creates a revision by definition, so requiring the revision IDs to match
// would make the property unsatisfiable rather than strict.
func TestMetadataOnlySaveDoesNotSplitAShift(t *testing.T) {
	base := utc(2026, 5, 4, 11, 0) // Monday, on the handoff boundary

	shapes := []struct {
		name   string
		zone   string
		policy rotation.RotationPolicy
		groups []rotation.RotationGroup
		until  time.Time
	}{
		{
			name: "weekly two groups", zone: "UTC", policy: weeklyPolicy("11:00", 1),
			groups: []rotation.RotationGroup{group(groupA, "alice"), group(groupB, "bob")},
			until:  base.AddDate(0, 0, 28),
		},
		{
			name: "daily three groups", zone: "UTC", policy: dailyPolicy("11:00"),
			groups: []rotation.RotationGroup{group(groupA, "alice"), group(groupB, "bob"), group(groupC, "carol")},
			until:  base.AddDate(0, 0, 10),
		},
		{
			// Spans the 2026-10-25 fall-back transition.
			name: "daily across DST", zone: "Europe/Berlin", policy: dailyPolicy("11:00"),
			groups: []rotation.RotationGroup{group(groupA, "alice"), group(groupB, "bob")},
			until:  time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC),
		},
		{
			name: "single group never handing over", zone: "Europe/Berlin", policy: weeklyPolicy("09:00", 3),
			groups: []rotation.RotationGroup{group(groupA, "alice", "amy")},
			until:  base.AddDate(0, 0, 21),
		},
	}

	// Save instants relative to the range start, including one exactly on a
	// handoff boundary and one a microsecond after it.
	offsets := []struct {
		name string
		d    time.Duration
	}{
		{"just after the range starts", scheduleconfig.TimestampResolution},
		{"mid shift", 37 * time.Hour},
		{"exactly on a handoff", 24 * time.Hour},
		{"one tick after a handoff", 24*time.Hour + scheduleconfig.TimestampResolution},
		{"late in the range", 8 * 24 * time.Hour},
	}

	for _, shape := range shapes {
		for _, offset := range offsets {
			t.Run(shape.name+"/"+offset.name, func(t *testing.T) {
				start := base
				if shape.zone == "Europe/Berlin" && shape.policy.HandoffDay != nil {
					start = utc(2026, 5, 6, 7, 0) // Wednesday 09:00 Berlin
				}
				save := start.Add(offset.d)
				if !save.Before(shape.until) {
					t.Skipf("save %v falls outside the range", save)
				}

				before := config(shape.zone, shape.policy, shape.groups...)
				before.SlackUsergroupID = "S-OLD"
				after := before
				after.SlackUsergroupID = "S-NEW"

				unchanged := renderOf(t, Input{
					Root:      root(start),
					Revisions: chain(t, revisionStep{at: start, cfg: before}),
					From:      start, Until: shape.until,
				})
				saved := renderOf(t, Input{
					Root: root(start),
					Revisions: chain(t,
						revisionStep{at: start, cfg: before},
						revisionStep{at: save, cfg: after}),
					From: start, Until: shape.until,
				})

				wantShifts := MergeAdjacent(assignmentsOf(unchanged, LayerL1))
				gotShifts := MergeAdjacent(assignmentsOf(saved, LayerL1))
				if len(wantShifts) == 0 {
					t.Fatal("nothing rendered; the case proves nothing")
				}
				if !sameShifts(gotShifts, wantShifts) {
					t.Fatalf("a metadata-only save changed the rendered shifts:\n got %+v\nwant %+v",
						gotShifts, wantShifts)
				}
				// Neither result may report damage.
				if len(unchanged.Warnings) != 0 || len(saved.Warnings) != 0 {
					t.Fatalf("warnings appeared: %v / %v", warningCodes(unchanged), warningCodes(saved))
				}

				// The save IS a real boundary underneath: both revisions
				// contribute atomic assignments. Only the presentation is
				// unchanged, not the data.
				//
				// How many assignments that costs is not asserted: a save
				// landing exactly on a handoff splits nothing, because the
				// revision boundary and the slot boundary coincide.
				contributors := map[string]bool{}
				for _, a := range assignmentsOf(saved, LayerL1) {
					contributors[a.ScheduleRevisionID] = true
				}
				if len(contributors) != 2 {
					t.Fatalf("assignments come from %d revisions, want both sides of the save", len(contributors))
				}
			})
		}
	}
}

// TestMetadataOnlySaveMidSlotSplitsAtomicAssignments is the mechanical claim
// the property above deliberately does not make: a save inside a slot does
// cut the atomic assignment in two, and merging is what puts it back
// together. Without the split there would be nothing for the property to
// prove.
func TestMetadataOnlySaveMidSlotSplitsAtomicAssignments(t *testing.T) {
	start := utc(2026, 5, 4, 11, 0)
	save := utc(2026, 5, 5, 9, 30) // inside the second daily slot
	until := utc(2026, 5, 8, 11, 0)

	before := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))
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

	if got, want := len(assignmentsOf(saved, LayerL1)), len(assignmentsOf(unchanged, LayerL1))+1; got != want {
		t.Fatalf("got %d atomic assignments, want %d - the save should split the slot it fell in", got, want)
	}

	shifts := MergeAdjacent(assignmentsOf(saved, LayerL1))
	if !sameShifts(shifts, MergeAdjacent(assignmentsOf(unchanged, LayerL1))) {
		t.Fatal("the split survived the merge")
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
