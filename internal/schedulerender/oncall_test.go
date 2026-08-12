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
