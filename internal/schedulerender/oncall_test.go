package schedulerender

import (
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// onCallAt runs the pure projection for one revision and instant.
func onCallAt(t *testing.T, rev scheduleconfig.ScheduleRevision, at time.Time,
	overrides []scheduleconfig.OverrideRevision) OnCall {
	t.Helper()
	slots, err := onCallSlots(rev, at)
	if err != nil {
		t.Fatalf("onCallSlots at %v: %v", at, err)
	}
	out, err := projectOnCall(rev, at, slots, overrides)
	if err != nil {
		t.Fatalf("projectOnCall at %v: %v", at, err)
	}
	return out
}

// TestOnCallBoundariesAroundOverride is the reason the projection shares its
// overlay with the renderer instead of reimplementing it.
//
// A cheap version that only looks for the next override ahead gets 20:00
// wrong: it reports the shift as having started at the 11:00 handoff, when in
// fact the rotation only resumed at 18:00, where the override released it.
func TestOnCallBoundariesAroundOverride(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})
	rev := revs[0]

	ovrStart := utc(2026, 5, 1, 14, 0)
	ovrEnd := utc(2026, 5, 1, 18, 0)
	nextHandoff := utc(2026, 5, 2, 11, 0)
	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "carol", ovrStart, ovrEnd, start),
	}

	tests := []struct {
		name       string
		at         time.Time
		source     string
		user       string
		start, end time.Time
	}{
		{"before the override", utc(2026, 5, 1, 13, 0), SourceRotation, "alice", start, ovrStart},
		{"exactly at its start", ovrStart, SourceOverride, "carol", ovrStart, ovrEnd},
		{"inside it", utc(2026, 5, 1, 16, 0), SourceOverride, "carol", ovrStart, ovrEnd},
		{"exactly at its end", ovrEnd, SourceRotation, "alice", ovrEnd, nextHandoff},
		{"after it", utc(2026, 5, 1, 20, 0), SourceRotation, "alice", ovrEnd, nextHandoff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := onCallAt(t, rev, tc.at, overrides)
			if got.L1 == nil {
				t.Fatal("nobody on call")
			}
			if got.L1.Source != tc.source {
				t.Fatalf("source = %s, want %s", got.L1.Source, tc.source)
			}
			if len(got.L1.UserIDs) != 1 || got.L1.UserIDs[0] != tc.user {
				t.Fatalf("users = %v, want [%s]", got.L1.UserIDs, tc.user)
			}
			if !got.L1.AssignmentStart.Equal(tc.start) || !got.L1.AssignmentEnd.Equal(tc.end) {
				t.Fatalf("assignment = %v..%v, want %v..%v",
					got.L1.AssignmentStart, got.L1.AssignmentEnd, tc.start, tc.end)
			}
			// The grid slot is the same handoff interval throughout: it is the
			// rotation's shape, not the override's.
			wantSlotStart, wantSlotEnd := start, nextHandoff
			if !got.L1.GridSlotStart.Equal(wantSlotStart) || !got.L1.GridSlotEnd.Equal(wantSlotEnd) {
				t.Fatalf("grid slot = %v..%v, want %v..%v",
					got.L1.GridSlotStart, got.L1.GridSlotEnd, wantSlotStart, wantSlotEnd)
			}
		})
	}
}

// TestOnCallReportsCompositionChangeHonestly is the [B] -> [B,D] case from
// the original bug: the grid still says the shift began at the handoff, but
// the composition only took effect when it was saved.
func TestOnCallReportsCompositionChangeHonestly(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 1, 15, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupB, "bob"))},
		revisionStep{at: edit, cfg: config("UTC", dailyPolicy("11:00"), group(groupB, "bob", "dave"))},
	)

	got := onCallAt(t, revs[1], utc(2026, 5, 1, 16, 0), nil)
	if got.L1 == nil {
		t.Fatal("nobody on call")
	}
	if !got.L1.GridSlotStart.Equal(start) {
		t.Fatalf("grid slot start = %v, want the handoff %v", got.L1.GridSlotStart, start)
	}
	if !got.L1.AssignmentStart.Equal(edit) {
		t.Fatalf("assignment start = %v, want the edit %v", got.L1.AssignmentStart, edit)
	}
	if len(got.L1.UserIDs) != 2 {
		t.Fatalf("users = %v, want the new pair", got.L1.UserIDs)
	}
	if got.L1.GroupID != groupB {
		t.Fatalf("group = %s, want the stable identity %s", got.L1.GroupID, groupB)
	}
}

// TestOnCallOnDisabledLayerWithOverride: the layer carries no rotation, but
// somebody is still on duty.
func TestOnCallOnDisabledLayerWithOverride(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	cfg := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	cfg.L1 = rotation.LayerConfiguration{Enabled: false, Policy: dailyPolicy("11:00")}
	// A schedule needs some rotation somewhere for the snapshot to be
	// meaningful; L2 carries it here.
	cfg.L2 = rotation.LayerConfiguration{
		Enabled: true, Policy: dailyPolicy("11:00"),
		Groups: []rotation.RotationGroup{group("carol", "carol")},
	}
	revs := chain(t, revisionStep{at: start, cfg: cfg})

	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "erin", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start),
	}

	inside := onCallAt(t, revs[0], utc(2026, 5, 1, 16, 0), overrides)
	if inside.L1 == nil || inside.L1.UserIDs[0] != "erin" {
		t.Fatalf("L1 = %v, want the override holder", inside.L1)
	}
	if inside.L2 == nil || inside.L2.UserIDs[0] != "carol" {
		t.Fatalf("L2 = %v, want the rotation", inside.L2)
	}

	// Outside the override the disabled layer has nobody, and that is an
	// answer, not a fallback to the last known group.
	outside := onCallAt(t, revs[0], utc(2026, 5, 1, 20, 0), overrides)
	if outside.L1 != nil {
		t.Fatalf("L1 = %v on a disabled layer outside the override, want nobody", outside.L1)
	}
}

// TestOnCallRefusesOverlappingOverrides: a collision is no less wrong for
// being seen through the current view - and the current view is the one that
// decides who gets woken, so answering with either of the two is worse here
// than anywhere else.
func TestOnCallRefusesOverlappingOverrides(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-a", LayerL1, "carol", utc(2026, 5, 1, 13, 0), utc(2026, 5, 1, 17, 0), start),
		override("ovr-b", LayerL1, "dave", utc(2026, 5, 1, 15, 0), utc(2026, 5, 1, 19, 0), start.Add(time.Hour)),
	}

	slots, err := onCallSlots(revs[0], utc(2026, 5, 1, 16, 0))
	if err != nil {
		t.Fatalf("onCallSlots: %v", err)
	}
	_, err = projectOnCall(revs[0], utc(2026, 5, 1, 16, 0), slots, overrides)
	if !errors.Is(err, scheduleconfig.ErrOverrideCollision) {
		t.Fatalf("error = %v, want ErrOverrideCollision", err)
	}
}

// TestOnCallClipsToRevisionEnd: a historical query inside a closed revision
// must not report a shift running past the revision that ended it.
func TestOnCallClipsToRevisionEnd(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 1, 15, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: edit, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)

	got := onCallAt(t, revs[0], utc(2026, 5, 1, 13, 0), nil)
	if got.L1 == nil {
		t.Fatal("nobody on call")
	}
	if !got.L1.AssignmentEnd.Equal(edit) {
		t.Fatalf("assignment end = %v, want the revision end %v", got.L1.AssignmentEnd, edit)
	}
	if !got.L1.GridSlotEnd.Equal(utc(2026, 5, 2, 11, 0)) {
		t.Fatalf("grid slot end = %v, want the raw handoff", got.L1.GridSlotEnd)
	}
}

// TestOnCallOverrideSpanningTwoSlotsReportsItsOwnEnd: the grid slot decides
// which rotation group is on duty; it must not decide when an override ends.
//
// A daily rotation hands off at 11:00, so an override from 08:00 to 15:00
// crosses the boundary. renderSlot answers per slot and clips to it, and the
// historical renderer never notices because MergeAdjacent joins the pieces back
// together. This projection renders one slot and merges nothing, so before the
// fix it told the caller the stand-in ended at 11:00 - the moment the person
// being stood in for would otherwise have handed over.
func TestOnCallOverrideSpanningTwoSlotsReportsItsOwnEnd(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"),
		group(groupA, "alice"), group(groupB, "bob"))})
	rev := revs[0]

	ovrStart := utc(2026, 5, 2, 8, 0)
	ovrEnd := utc(2026, 5, 2, 15, 0)
	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "carol", ovrStart, ovrEnd, start),
	}

	for _, at := range []time.Time{
		utc(2026, 5, 2, 9, 0),  // in the slot the override starts in
		utc(2026, 5, 2, 13, 0), // in the slot it ends in
	} {
		got := onCallAt(t, rev, at, overrides)
		if got.L1 == nil || got.L1.Source != SourceOverride {
			t.Fatalf("at %v: want the override in force, got %+v", at, got.L1)
		}
		if !got.L1.AssignmentStart.Equal(ovrStart) || !got.L1.AssignmentEnd.Equal(ovrEnd) {
			t.Errorf("at %v: assignment = %v..%v, want the override's own %v..%v",
				at, got.L1.AssignmentStart, got.L1.AssignmentEnd, ovrStart, ovrEnd)
		}
		// The grid pair still reports the slot, and the two must not collapse
		// into one another: that is why the DTO carries both.
		if got.L1.GridSlotEnd.Equal(ovrEnd) {
			t.Errorf("at %v: the grid pair took the override's boundaries", at)
		}
	}

	// And rotation-sourced assignments still end where the slot does: for them
	// the slot IS the shift, and widening those would be the opposite defect.
	after := onCallAt(t, rev, utc(2026, 5, 2, 16, 0), overrides)
	if after.L1 == nil || after.L1.Source != SourceRotation {
		t.Fatalf("after the override the rotation must resume: %+v", after.L1)
	}
	if !after.L1.AssignmentStart.Equal(ovrEnd) || !after.L1.AssignmentEnd.Equal(utc(2026, 5, 3, 11, 0)) {
		t.Errorf("rotation assignment = %v..%v, want %v..%v",
			after.L1.AssignmentStart, after.L1.AssignmentEnd, ovrEnd, utc(2026, 5, 3, 11, 0))
	}
}

// TestHistoricalAssignmentsStayDisjointAcrossSlots is the other half of the
// same fix, and it guards the tempting shortcut: widening renderSlot itself.
//
// renderSlot is shared with the historical renderer, which calls it once per
// slot. If it returned an override beyond the slot it was asked about, the
// renderer would emit the same stretch twice - once from each slot - and the
// raw assignments would overlap. Merging happens after, so the damage would be
// invisible in the merged output and very visible in anything reading raw.
func TestHistoricalAssignmentsStayDisjointAcrossSlots(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"),
		group(groupA, "alice"), group(groupB, "bob"))})

	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "carol", utc(2026, 5, 2, 8, 0), utc(2026, 5, 2, 15, 0), start),
	}
	res, err := Render(Input{
		Root: root(start), Revisions: revs, Overrides: overrides,
		From: start, Until: utc(2026, 5, 4, 11, 0),
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var l1 []Assignment
	for _, a := range res.Assignments {
		if a.Layer == LayerL1 {
			l1 = append(l1, a)
		}
	}
	for i := 1; i < len(l1); i++ {
		if l1[i].AssignmentStart.Before(l1[i-1].AssignmentEnd) {
			t.Fatalf("raw assignments overlap: %v..%v then %v..%v",
				l1[i-1].AssignmentStart, l1[i-1].AssignmentEnd,
				l1[i].AssignmentStart, l1[i].AssignmentEnd)
		}
	}
}
