package schedulerender

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// TestPolicyAppliesPerRevision is the core historical property: each side of a
// revision boundary is rendered with the grid that was in force there, not
// with today's.
func TestPolicyAppliesPerRevision(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 3, 15, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
		revisionStep{at: edit, cfg: config("UTC", dailyPolicy("18:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)

	res := renderOf(t, Input{
		Root:      root(start),
		Revisions: revs,
		From:      start,
		Until:     utc(2026, 5, 5, 11, 0),
	})

	l1 := assignmentsOf(res, LayerL1)
	if len(l1) == 0 {
		t.Fatal("no L1 assignments")
	}

	// Before the edit the boundaries follow the 11:00 grid; after it, 18:00.
	for _, a := range l1 {
		if a.AssignmentEnd.After(edit) && a.AssignmentStart.Before(edit) {
			t.Fatalf("assignment %v..%v straddles the revision boundary", a.AssignmentStart, a.AssignmentEnd)
		}
	}
	if got := l1[0]; !got.GridSlotStart.Equal(start) {
		t.Fatalf("first grid slot starts at %v, want %v", got.GridSlotStart, start)
	}

	var afterEdit *Assignment
	for i := range l1 {
		if l1[i].AssignmentStart.Equal(edit) {
			afterEdit = &l1[i]
			break
		}
	}
	if afterEdit == nil {
		t.Fatal("no assignment starts at the edit")
	}
	// The new 18:00 grid slot containing the edit began at 18:00 the previous
	// day - before the revision that introduced it. Reporting the raw grid
	// boundary is the point of keeping the two pairs apart.
	wantGridStart := utc(2026, 5, 2, 18, 0)
	if !afterEdit.GridSlotStart.Equal(wantGridStart) {
		t.Fatalf("grid slot start = %v, want %v", afterEdit.GridSlotStart, wantGridStart)
	}
	if !afterEdit.AssignmentStart.Equal(edit) {
		t.Fatalf("assignment start = %v, want the edit %v", afterEdit.AssignmentStart, edit)
	}
	if afterEdit.GridSlotStart.After(afterEdit.AssignmentStart) {
		t.Fatal("grid slot start must be able to precede the assignment start")
	}
}

// TestHistoryBeforeEditIsUnchanged renders the same past range before and
// after a later edit exists.
func TestHistoryBeforeEditIsUnchanged(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 10, 15, 0)
	from, until := start, utc(2026, 5, 5, 11, 0)

	before := renderOf(t, Input{
		Root: root(start),
		Revisions: chain(t,
			revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))}),
		From: from, Until: until,
	})

	// The edit is outside the queried range, so the range query would not even
	// return the second revision; render with both to prove it changes nothing.
	after := renderOf(t, Input{
		Root: root(start),
		Revisions: chain(t,
			revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
			revisionStep{at: edit, cfg: config("UTC", dailyPolicy("18:00"), group(groupA, "alice"), group(groupB, "bob"))}),
		From: from, Until: until,
	})

	if !sameShifts(MergeAdjacent(before.Assignments), MergeAdjacent(after.Assignments)) {
		t.Fatal("a later edit changed the rendered past")
	}
}

// TestMembershipChangeDoesNotReachIntoThePast covers the original bug: adding
// a member to the active group must not retroactively add them to finished
// shifts.
func TestMembershipChangeDoesNotReachIntoThePast(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 2, 15, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
		revisionStep{at: edit, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob", "dave"))},
	)

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 3, 11, 0)})

	for _, a := range assignmentsOf(res, LayerL1) {
		if !a.AssignmentEnd.After(edit) {
			for _, u := range a.UserIDs {
				if u == "dave" {
					t.Fatalf("dave appears in a segment ending %v, before the edit %v", a.AssignmentEnd, edit)
				}
			}
		}
	}

	// And the segment that starts at the edit does carry the new composition.
	l1 := assignmentsOf(res, LayerL1)
	atEdit := assignmentAt(l1, edit)
	if atEdit == nil {
		t.Fatal("no assignment covers the edit")
	}
	if !atEdit.AssignmentStart.Equal(edit) {
		t.Fatalf("assignment covering the edit starts at %v, want %v", atEdit.AssignmentStart, edit)
	}
	if len(atEdit.UserIDs) != 2 {
		t.Fatalf("assignment at the edit has users %v, want the new pair", atEdit.UserIDs)
	}
}

// TestRangeCrossingCadences renders across a daily -> weekly change.
func TestRangeCrossingCadences(t *testing.T) {
	start := utc(2026, 5, 4, 11, 0) // Monday
	edit := utc(2026, 5, 6, 9, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
		revisionStep{at: edit, cfg: config("UTC", weeklyPolicy("11:00", 1), group(groupA, "alice"), group(groupB, "bob"))},
	)

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 20, 11, 0)})
	l1 := assignmentsOf(res, LayerL1)

	var daily, weekly int
	for _, a := range l1 {
		span := a.GridSlotEnd.Sub(a.GridSlotStart)
		switch {
		case span == 24*time.Hour:
			daily++
		case span == 7*24*time.Hour:
			weekly++
		default:
			t.Fatalf("unexpected grid slot span %v", span)
		}
	}
	if daily == 0 || weekly == 0 {
		t.Fatalf("got %d daily and %d weekly slots, want both", daily, weekly)
	}
}

// TestL2HistoryComesFromTheSnapshot proves the renderer reads l2.enabled per
// revision. Reading it from the current schedule row would erase an L2 that
// existed in the past.
func TestL2HistoryComesFromTheSnapshot(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	disable := utc(2026, 5, 3, 11, 0)

	withL2 := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	withL2.L2 = rotation.LayerConfiguration{
		Enabled: true,
		Policy:  dailyPolicy("11:00"),
		Groups:  []rotation.RotationGroup{group("carol", "carol")},
	}
	withoutL2 := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))

	revs := chain(t,
		revisionStep{at: start, cfg: withL2},
		revisionStep{at: disable, cfg: withoutL2},
	)

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 5, 11, 0)})
	l2 := assignmentsOf(res, LayerL2)
	if len(l2) == 0 {
		t.Fatal("the L2 that existed in the past was not rendered")
	}
	for _, a := range l2 {
		if a.AssignmentEnd.After(disable) {
			t.Fatalf("L2 assignment %v..%v outlives the revision that disabled it", a.AssignmentStart, a.AssignmentEnd)
		}
	}
}

func TestHistoryIncompleteBeforeCutover(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	res := renderOf(t, Input{
		Root:      root(start),
		Revisions: revs,
		From:      start.Add(-48 * time.Hour),
		Until:     utc(2026, 5, 3, 11, 0),
	})

	if res.HistoryComplete {
		t.Fatal("history reported complete for a range preceding the first revision")
	}
	if !hasWarning(res, WarnHistoryIncomplete) {
		t.Fatalf("warnings = %v, want history_incomplete", warningCodes(res))
	}
	// Missing history is not a broken chain.
	if hasWarning(res, WarnRevisionGap) {
		t.Fatal("the range before the first revision was reported as a gap")
	}
	for _, a := range res.Assignments {
		if a.AssignmentStart.Before(start) {
			t.Fatalf("assignment at %v predates the first revision", a.AssignmentStart)
		}
	}
}

func TestHistoryCompleteInsideRecordedRange(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 3, 11, 0)})
	if !res.HistoryComplete {
		t.Fatal("history reported incomplete for a fully recorded range")
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warningCodes(res))
	}
}

// TestDeletedPeriodIsHistoricNotAGap is the reason revision kind exists.
func TestDeletedPeriodIsHistoricNotAGap(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	deleted := utc(2026, 5, 3, 11, 0)
	recreated := utc(2026, 5, 5, 11, 0)

	cfg := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	revs := chain(t,
		revisionStep{at: start, cfg: cfg},
		revisionStep{at: deleted, kind: scheduleconfig.RevisionDeleted},
		revisionStep{at: recreated, cfg: cfg},
	)

	res := renderOf(t, Input{Root: root(start), Revisions: revs, From: start, Until: utc(2026, 5, 7, 11, 0)})

	if hasWarning(res, WarnRevisionGap) {
		t.Fatal("a normal delete/recreate cycle was reported as a broken chain")
	}
	if !hasWarning(res, WarnScheduleInactive) {
		t.Fatalf("warnings = %v, want schedule_inactive", warningCodes(res))
	}
	for _, a := range res.Assignments {
		if a.AssignmentStart.Before(recreated) && a.AssignmentEnd.After(deleted) {
			t.Fatalf("assignment %v..%v falls inside the deleted period", a.AssignmentStart, a.AssignmentEnd)
		}
	}
	// Both sides of the deleted period still render.
	if len(res.Assignments) == 0 {
		t.Fatal("nothing rendered around the deleted period")
	}
}

// TestRealGapIsStillDiagnosed is the other half: with deleted periods
// recorded, a hole can only mean a lost revision, and it must still be said.
func TestRealGapIsStillDiagnosed(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	lost := utc(2026, 5, 3, 11, 0)
	resumed := utc(2026, 5, 5, 11, 0)

	cfg := config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))
	revs := chain(t,
		revisionStep{at: start, cfg: cfg},
		revisionStep{at: lost, kind: scheduleconfig.RevisionDeleted},
		revisionStep{at: resumed, cfg: cfg},
	)
	// Excise the middle revision, leaving the hole a lost row would leave.
	damaged := []scheduleconfig.ScheduleRevision{revs[0], revs[2]}

	res := renderOf(t, Input{Root: root(start), Revisions: damaged, From: start, Until: utc(2026, 5, 7, 11, 0)})
	if !hasWarning(res, WarnRevisionGap) {
		t.Fatalf("warnings = %v, want revision_gap", warningCodes(res))
	}
}

func TestRenderRejectsEmptyRange(t *testing.T) {
	at := utc(2026, 5, 1, 11, 0)
	if _, err := Render(Input{Root: root(at), From: at, Until: at}); err == nil {
		t.Fatal("an empty range was accepted")
	}
}

// TestRenderDoesNotMutateInput is the purity contract: the caller's revisions
// and overrides come back untouched, including the slices inside them.
func TestRenderDoesNotMutateInput(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice", "amy"), group(groupB, "bob"))})
	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start),
	}

	firstRevID := revs[0].ID
	members := append([]string(nil), revs[0].Snapshot.L1.Groups[0].Members...)
	overrideOrder := overrides[0].OverrideID

	res := renderOf(t, Input{Root: root(start), Revisions: revs, Overrides: overrides, From: start, Until: utc(2026, 5, 3, 11, 0)})

	if revs[0].ID != firstRevID || overrides[0].OverrideID != overrideOrder {
		t.Fatal("Render reordered its input")
	}
	if !equalIDs(revs[0].Snapshot.L1.Groups[0].Members, members) {
		t.Fatal("Render mutated the snapshot group members")
	}

	// Mutating the result must not reach back into the snapshot either.
	for i := range res.Assignments {
		if len(res.Assignments[i].UserIDs) > 0 {
			res.Assignments[i].UserIDs[0] = "tampered"
		}
	}
	if !equalIDs(revs[0].Snapshot.L1.Groups[0].Members, members) {
		t.Fatal("assignment user IDs alias the snapshot")
	}
}
