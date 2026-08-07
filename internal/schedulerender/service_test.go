package schedulerender

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
)

// seed writes a revision chain and its overrides into the fake repository.
func seed(t *testing.T, revs []scheduleconfig.ScheduleRevision, overrides []scheduleconfig.OverrideRevision) *fakes.ScheduleConfigRepo {
	t.Helper()
	repo := fakes.NewScheduleConfigRepo()
	ctx := context.Background()

	err := repo.WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		initial := revs[0]
		root := &scheduleconfig.ScheduleRoot{ID: testScheduleID, TeamID: "devops"}
		open := initial
		open.EffectiveTo = nil
		if err := tx.CreateInitialSchedule(ctx, root, &open); err != nil {
			return err
		}
		for i := 1; i < len(revs); i++ {
			at := revs[i].EffectiveFrom
			if err := tx.CloseRevision(ctx, testScheduleID, revs[i-1].ID, at); err != nil {
				return err
			}
			next := revs[i]
			next.EffectiveTo = nil
			if err := tx.InsertRevision(ctx, &next); err != nil {
				return err
			}
		}
		for i := range overrides {
			o := overrides[i]
			if err := tx.InsertOverrideRevision(ctx, &o); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return repo
}

func TestServiceRenderRangeMatchesPureRender(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	edit := utc(2026, 5, 2, 15, 0)
	until := utc(2026, 5, 4, 11, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: edit, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)
	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start),
	}
	repo := seed(t, revs, overrides)

	got, err := New(repo).RenderRange(context.Background(), testScheduleID, start, until, nil)
	if err != nil {
		t.Fatalf("RenderRange: %v", err)
	}

	rootFromFake, _ := repo.Root(testScheduleID)
	want := renderOf(t, Input{
		Root:      rootFromFake,
		Revisions: repo.Revisions(testScheduleID),
		Overrides: overrides,
		From:      start, Until: until,
	})
	if !SameShifts(MergeAdjacent(got.Assignments), MergeAdjacent(want.Assignments)) {
		t.Fatal("loading through the service changed the answer")
	}
	if !got.HistoryComplete {
		t.Fatal("history reported incomplete for a fully recorded range")
	}
}

func TestServiceCurrentOnCall(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})
	overrides := []scheduleconfig.OverrideRevision{
		override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start),
	}
	svc := New(seed(t, revs, overrides))
	ctx := context.Background()

	inside, err := svc.CurrentOnCall(ctx, testScheduleID, utc(2026, 5, 1, 16, 0))
	if err != nil {
		t.Fatalf("CurrentOnCall: %v", err)
	}
	if inside.L1 == nil || inside.L1.UserIDs[0] != "carol" {
		t.Fatalf("L1 = %v, want the override holder", inside.L1)
	}

	after, err := svc.CurrentOnCall(ctx, testScheduleID, utc(2026, 5, 1, 20, 0))
	if err != nil {
		t.Fatalf("CurrentOnCall: %v", err)
	}
	if after.L1 == nil || after.L1.UserIDs[0] != "alice" {
		t.Fatalf("L1 = %v, want the rotation back", after.L1)
	}
	if !after.L1.AssignmentStart.Equal(utc(2026, 5, 1, 18, 0)) {
		t.Fatalf("assignment start = %v, want the moment the override released the rotation",
			after.L1.AssignmentStart)
	}
}

// TestServiceCurrentOnCallWithoutSchedule: a dispatcher asking who to page
// must be told nobody, not handed an error it has to interpret.
func TestServiceCurrentOnCallWithoutSchedule(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})
	svc := New(seed(t, revs, nil))
	ctx := context.Background()

	tests := []struct {
		name       string
		scheduleID string
		at         time.Time
	}{
		{"unknown schedule", "no-such-schedule", start},
		{"before any revision", testScheduleID, start.Add(-time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.CurrentOnCall(ctx, tc.scheduleID, tc.at)
			if err != nil {
				t.Fatalf("CurrentOnCall: %v", err)
			}
			if got.L1 != nil || got.L2 != nil {
				t.Fatalf("got %v on call, want nobody", got)
			}
		})
	}
}

// TestServiceCurrentOnCallDuringDeletedPeriod: nobody is on call, and it is
// read off the revision state rather than inferred from missing data.
func TestServiceCurrentOnCallDuringDeletedPeriod(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	deleted := utc(2026, 5, 3, 11, 0)

	revs := chain(t,
		revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		revisionStep{at: deleted, kind: scheduleconfig.RevisionDeleted},
	)
	svc := New(seed(t, revs, nil))
	ctx := context.Background()

	during, err := svc.CurrentOnCall(ctx, testScheduleID, deleted.Add(time.Hour))
	if err != nil {
		t.Fatalf("CurrentOnCall: %v", err)
	}
	if during.L1 != nil {
		t.Fatalf("L1 = %v during a deleted period, want nobody", during.L1)
	}

	// History before the delete is still answerable.
	before, err := svc.CurrentOnCall(ctx, testScheduleID, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("CurrentOnCall: %v", err)
	}
	if before.L1 == nil || before.L1.UserIDs[0] != "alice" {
		t.Fatalf("L1 = %v before the delete, want the rotation", before.L1)
	}
}

// TestSnapshotIsolatesConcurrentWrites is the reason the read side hands out a
// snapshot rather than individual reads: a Save committing between two reads
// would otherwise let one answer be built from two different states.
func TestSnapshotIsolatesConcurrentWrites(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})
	repo := seed(t, revs, nil)
	ctx := context.Background()

	err := repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		before, err := view.GetOverrideProjectionInRange(ctx, testScheduleID, nil, nil, nil)
		if err != nil {
			return err
		}

		// A concurrent writer commits an override in the middle of the read.
		done := make(chan error, 1)
		go func() {
			done <- repo.WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
				o := override("ovr-late", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), start)
				return tx.InsertOverrideRevision(ctx, &o)
			})
		}()
		if err := <-done; err != nil {
			return err
		}

		after, err := view.GetOverrideProjectionInRange(ctx, testScheduleID, nil, nil, nil)
		if err != nil {
			return err
		}
		if len(before) != len(after) {
			t.Fatalf("the snapshot saw a concurrent write: %d overrides became %d", len(before), len(after))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithinSnapshot: %v", err)
	}

	// The write did land - the snapshot hid it, it did not block it.
	if got := len(repo.OverrideRevisions(testScheduleID)); got != 1 {
		t.Fatalf("stored %d override revisions, want the concurrent write committed", got)
	}
}

// TestAsOfSelectsTheVersionKnownThen replays override state as it was
// recorded, which is what makes a historical answer reproducible.
func TestAsOfSelectsTheVersionKnownThen(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	revs := chain(t, revisionStep{at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))})

	firstRecorded := start
	editRecorded := start.Add(2 * time.Hour)

	first := override("ovr-1", LayerL1, "carol", utc(2026, 5, 1, 14, 0), utc(2026, 5, 1, 18, 0), firstRecorded)
	edited := first
	edited.RevisionID = "ovr-1-r2"
	edited.Revision = 2
	edited.UserID = "dave"
	edited.RecordedAt = editRecorded

	svc := New(seed(t, revs, []scheduleconfig.OverrideRevision{first, edited}))
	ctx := context.Background()
	until := utc(2026, 5, 2, 11, 0)

	asOfBefore := firstRecorded.Add(time.Hour)
	early, err := svc.RenderRange(ctx, testScheduleID, start, until, &asOfBefore)
	if err != nil {
		t.Fatalf("RenderRange: %v", err)
	}
	if got := overrideHolder(early); got != "carol" {
		t.Fatalf("as-of before the edit the override is held by %q, want carol", got)
	}

	current, err := svc.RenderRange(ctx, testScheduleID, start, until, nil)
	if err != nil {
		t.Fatalf("RenderRange: %v", err)
	}
	if got := overrideHolder(current); got != "dave" {
		t.Fatalf("current override is held by %q, want dave", got)
	}
}

func overrideHolder(res Result) string {
	for _, a := range res.Assignments {
		if a.Source == SourceOverride && len(a.UserIDs) > 0 {
			return a.UserIDs[0]
		}
	}
	return ""
}
