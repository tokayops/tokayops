package schedulerender

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
)

// bulkRepo builds a repository holding several schedules, which the single-
// schedule seed() helper cannot: it owns testScheduleID.
type bulkSchedule struct {
	id     string
	teamID string
	at     time.Time
	cfg    rotation.ScheduleConfiguration

	// deletedAt, when set, closes the chain with a deleted-kind revision and
	// marks the root soft-deleted - the two halves of a delete, written
	// together the way the command writes them.
	deletedAt *time.Time
}

func bulkRepo(t testing.TB, schedules ...bulkSchedule) *fakes.ScheduleConfigRepo {
	t.Helper()
	repo := fakes.NewScheduleConfigRepo()
	ctx := context.Background()

	for _, sc := range schedules {
		sc := sc
		snapshot := snapshotFrom(t, nil, sc.cfg, sc.at)
		initial := scheduleconfig.ScheduleRevision{
			ID:            sc.id + "-rev1",
			ScheduleID:    sc.id,
			Version:       1,
			Kind:          scheduleconfig.RevisionActive,
			Snapshot:      snapshot,
			EffectiveFrom: sc.at,
			RecordedAt:    sc.at,
		}
		err := repo.WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
			root := &scheduleconfig.ScheduleRoot{ID: sc.id, TeamID: sc.teamID}
			if err := tx.CreateInitialSchedule(ctx, root, &initial); err != nil {
				return err
			}
			if sc.deletedAt == nil {
				return nil
			}
			if err := tx.CloseRevision(ctx, sc.id, initial.ID, *sc.deletedAt); err != nil {
				return err
			}
			deleted := scheduleconfig.ScheduleRevision{
				ID:            sc.id + "-rev2",
				ScheduleID:    sc.id,
				Version:       2,
				Kind:          scheduleconfig.RevisionDeleted,
				Snapshot:      snapshot,
				EffectiveFrom: *sc.deletedAt,
				RecordedAt:    *sc.deletedAt,
			}
			if err := tx.InsertRevision(ctx, &deleted); err != nil {
				return err
			}
			return tx.SetScheduleDeleted(ctx, sc.id, sc.deletedAt)
		})
		if err != nil {
			t.Fatalf("seed schedule %s: %v", sc.id, err)
		}
	}
	return repo
}

// usergroupConfig is a configuration that also carries the Slack usergroup, so
// a test can prove the projection reads it from the snapshot.
func usergroupConfig(tz, usergroup string, policy rotation.RotationPolicy, groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	cfg := config(tz, policy, groups...)
	cfg.SlackUsergroupID = usergroup
	return cfg
}

func callCount(repo *fakes.ScheduleConfigRepo, method string) int {
	n := 0
	for _, call := range repo.Calls {
		if call == method {
			n++
		}
	}
	return n
}

func scheduleByID(bulk BulkOnCall, id string) (ScheduleOnCall, bool) {
	for _, sc := range bulk.Schedules {
		if sc.ScheduleID == id {
			return sc, true
		}
	}
	return ScheduleOnCall{}, false
}

func failureByID(bulk BulkOnCall, id string) (ProjectionFailure, bool) {
	for _, f := range bulk.Failures {
		if f.ScheduleID == id {
			return f, true
		}
	}
	return ProjectionFailure{}, false
}

// TestBulkOnCallReadsSnapshotFields: the timezone and the usergroup travel with
// the projection because they live in the snapshot that is already loaded. A
// second read of them would be a second source of truth, which is exactly the
// defect this replaces.
func TestBulkOnCallReadsSnapshotFields(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	repo := bulkRepo(t, bulkSchedule{
		id: "sched-1", teamID: "devops", at: start,
		cfg: usergroupConfig("Europe/Berlin", "S12345", dailyPolicy("11:00"), group(groupA, "alice")),
	})

	bulk, err := New(repo).CurrentOnCallForAll(context.Background(), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("CurrentOnCallForAll: %v", err)
	}
	if len(bulk.Schedules) != 1 || len(bulk.Failures) != 0 {
		t.Fatalf("got %d schedules and %d failures, want 1 and 0", len(bulk.Schedules), len(bulk.Failures))
	}
	got := bulk.Schedules[0]
	if got.Timezone != "Europe/Berlin" {
		t.Errorf("timezone = %q, want the snapshot's", got.Timezone)
	}
	if got.SlackUsergroupID != "S12345" {
		t.Errorf("usergroup = %q, want the snapshot's", got.SlackUsergroupID)
	}
	if got.TeamID != "devops" {
		t.Errorf("team = %q, want devops", got.TeamID)
	}
	if got.OnCall.L1 == nil || got.OnCall.L1.UserIDs[0] != "alice" {
		t.Errorf("L1 = %v, want alice", got.OnCall.L1)
	}
}

// TestBulkOnCallReportsDeletedSchedules: a deleted schedule is present with an
// empty duty. Filtering it out here would leave the handoff notifier believing
// the last group is still on call, and a delete/recreate cycle would pass in
// silence.
func TestBulkOnCallReportsDeletedSchedules(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	deletedAt := utc(2026, 5, 3, 11, 0)
	repo := bulkRepo(t,
		bulkSchedule{id: "sched-live", teamID: "devops", at: start,
			cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		bulkSchedule{id: "sched-gone", teamID: "platform", at: start, deletedAt: &deletedAt,
			cfg: usergroupConfig("Asia/Bangkok", "S999", dailyPolicy("11:00"), group(groupB, "bob"))},
	)

	bulk, err := New(repo).CurrentOnCallForAll(context.Background(), deletedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("CurrentOnCallForAll: %v", err)
	}
	if len(bulk.Schedules) != 2 {
		t.Fatalf("got %d schedules, want both", len(bulk.Schedules))
	}
	gone, ok := scheduleByID(bulk, "sched-gone")
	if !ok {
		t.Fatal("the deleted schedule is missing from the projection")
	}
	if gone.DeletedAt == nil {
		t.Error("DeletedAt is nil for a deleted schedule")
	}
	if gone.OnCall.L1 != nil {
		t.Errorf("L1 = %v for a deleted schedule, want nobody", gone.OnCall.L1)
	}
	// The deleted tail carries the last valid snapshot, so a consumer can still
	// tell which usergroup this schedule was about.
	if gone.SlackUsergroupID != "S999" || gone.Timezone != "Asia/Bangkok" {
		t.Errorf("snapshot fields = %q/%q, want them from the deleted tail",
			gone.Timezone, gone.SlackUsergroupID)
	}
}

// TestBulkOnCallLegacyRootIsEmptyNotFailure: a legacy row is reachable while
// legacy creates and the seed exist, so it is "no schedule", not damage.
// Sprint 6B removes the branch together with the last way of producing one.
func TestBulkOnCallLegacyRootIsEmptyNotFailure(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	repo := bulkRepo(t, bulkSchedule{
		id: "sched-1", teamID: "devops", at: start,
		cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
	})
	repo.SeedLegacyRoot("sched-legacy", "legacy-team")

	bulk, err := New(repo).CurrentOnCallForAll(context.Background(), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("CurrentOnCallForAll: %v", err)
	}
	if len(bulk.Failures) != 0 {
		t.Fatalf("failures = %v, want a legacy root to be no failure at all", bulk.Failures)
	}
	legacy, ok := scheduleByID(bulk, "sched-legacy")
	if !ok {
		t.Fatal("the legacy schedule is missing from the projection")
	}
	if legacy.OnCall.L1 != nil || legacy.Timezone != "" || legacy.SlackUsergroupID != "" {
		t.Errorf("legacy projection = %+v, want an empty one", legacy)
	}
}

// TestBulkOnCallBeforeHistoryHorizonIsEmpty: an instant before the schedule's
// history horizon precedes the schedule itself.
func TestBulkOnCallBeforeHistoryHorizonIsEmpty(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	repo := bulkRepo(t, bulkSchedule{
		id: "sched-1", teamID: "devops", at: start,
		cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
	})

	before := len(repo.Calls)
	bulk, err := New(repo).CurrentOnCallForAll(context.Background(), start.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CurrentOnCallForAll: %v", err)
	}
	if len(bulk.Failures) != 0 || len(bulk.Schedules) != 1 {
		t.Fatalf("got %d schedules and %d failures, want 1 and 0", len(bulk.Schedules), len(bulk.Failures))
	}
	if bulk.Schedules[0].OnCall.L1 != nil {
		t.Errorf("L1 = %v before the horizon, want nobody", bulk.Schedules[0].OnCall.L1)
	}
	// The answer comes off the root, which the list query already returned: no
	// revision read is needed and none must happen.
	if got := callCount(repo, "GetEffectiveRevision"); got != 0 {
		t.Errorf("%d revision reads for an instant before the horizon, want 0 (calls: %v)",
			got, repo.Calls[before:])
	}
}

// TestBulkOnCallDamageIsPerSchedule walks the failures a schedule's own data can
// produce. Each is classified by the renderer, each leaves the other schedules
// answered, and none of them fails the call.
func TestBulkOnCallDamageIsPerSchedule(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	at := start.Add(time.Hour)
	horizon := start

	tests := []struct {
		name   string
		setup  func(repo *fakes.ScheduleConfigRepo)
		reason ProjectionFailureReason
	}{
		{
			name: "revision gap",
			setup: func(repo *fakes.ScheduleConfigRepo) {
				// An active root at config_version 1 whose chain is empty: the
				// state a lost revision row leaves behind.
				repo.SeedRoot(scheduleconfig.ScheduleRoot{
					ID: "sched-broken", TeamID: "broken-team",
					ConfigVersion: 1, HistoryCompleteFrom: &horizon,
				})
			},
			reason: FailureRevisionGap,
		},
		{
			name: "history horizon missing",
			setup: func(repo *fakes.ScheduleConfigRepo) {
				repo.SeedRoot(scheduleconfig.ScheduleRoot{
					ID: "sched-broken", TeamID: "broken-team", ConfigVersion: 1,
				})
			},
			reason: FailureHistoryMarkerMissing,
		},
		{
			name: "snapshot decode",
			setup: func(repo *fakes.ScheduleConfigRepo) {
				repo.SeedRoot(scheduleconfig.ScheduleRoot{
					ID: "sched-broken", TeamID: "broken-team",
					ConfigVersion: 1, HistoryCompleteFrom: &horizon,
				})
				repo.FailScheduleRead("sched-broken",
					fmt.Errorf("%w: revision r1: bad json", scheduleconfig.ErrSnapshotDecode))
			},
			reason: FailureSnapshotDecode,
		},
		{
			name: "revision metadata decode",
			setup: func(repo *fakes.ScheduleConfigRepo) {
				repo.SeedRoot(scheduleconfig.ScheduleRoot{
					ID: "sched-broken", TeamID: "broken-team",
					ConfigVersion: 1, HistoryCompleteFrom: &horizon,
				})
				repo.FailScheduleRead("sched-broken",
					fmt.Errorf("%w: revision r1: bad json", scheduleconfig.ErrRevisionMetadataDecode))
			},
			reason: FailureRevisionMetadata,
		},
		{
			name: "rotation math",
			setup: func(repo *fakes.ScheduleConfigRepo) {
				repo.SeedRoot(scheduleconfig.ScheduleRoot{
					ID: "sched-broken", TeamID: "broken-team",
					ConfigVersion: 1, HistoryCompleteFrom: &horizon,
				})
				repo.FailScheduleRead("sched-broken", layerError(
					scheduleconfig.ScheduleRevision{ID: "r1"}, LayerL1,
					errors.New("unknown time zone Mars/Olympus")))
			},
			reason: FailureRotation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := bulkRepo(t, bulkSchedule{
				id: "sched-healthy", teamID: "devops", at: start,
				cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
			})
			tc.setup(repo)

			bulk, err := New(repo).CurrentOnCallForAll(context.Background(), at)
			if err != nil {
				t.Fatalf("CurrentOnCallForAll: %v, want damage reported per schedule", err)
			}
			if _, ok := scheduleByID(bulk, "sched-healthy"); !ok {
				t.Fatal("the healthy schedule was dropped because another one is damaged")
			}
			failure, ok := failureByID(bulk, "sched-broken")
			if !ok {
				t.Fatalf("failures = %v, want the damaged schedule listed", bulk.Failures)
			}
			if failure.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", failure.Reason, tc.reason)
			}
			if failure.TeamID != "broken-team" {
				t.Errorf("team = %q, want it reported so the failure can be attributed", failure.TeamID)
			}
			if failure.Err == nil {
				t.Error("Err is nil; the reason is for branching, the error for reading")
			}
		})
	}
}

// TestBulkOnCallUnknownErrorFailsTheCall: the default is closed. An error the
// renderer does not recognize is infrastructure, and letting it through as a
// per-schedule failure would smear one connection failure across N schedules
// and make the metric lie.
func TestBulkOnCallUnknownErrorFailsTheCall(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	repo := bulkRepo(t, bulkSchedule{
		id: "sched-1", teamID: "devops", at: start,
		cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
	})
	repo.FailScheduleRead("sched-1", errors.New("connection reset by peer"))

	bulk, err := New(repo).CurrentOnCallForAll(context.Background(), start.Add(time.Hour))
	if err == nil {
		t.Fatal("an unrecognized error was absorbed into Failures")
	}
	if len(bulk.Schedules) != 0 || len(bulk.Failures) != 0 {
		t.Errorf("partial result returned alongside a call error: %+v", bulk)
	}
}

// TestBulkOnCallSnapshotFailureIsCallError: nothing was read, so nothing is
// reported - not an empty list that a consumer would mistake for "no schedules".
func TestBulkOnCallSnapshotFailureIsCallError(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	repo := bulkRepo(t, bulkSchedule{
		id: "sched-1", teamID: "devops", at: start,
		cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
	})

	boom := errors.New("could not begin transaction")
	for _, method := range []string{"WithinSnapshot", "ListScheduleRoots"} {
		t.Run(method, func(t *testing.T) {
			repo.FailOn = map[string]error{method: boom}
			defer func() { repo.FailOn = map[string]error{} }()

			bulk, err := New(repo).CurrentOnCallForAll(context.Background(), start.Add(time.Hour))
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want the read failure", err)
			}
			if len(bulk.Schedules) != 0 {
				t.Errorf("schedules = %v, want nothing", bulk.Schedules)
			}
		})
	}
}

// TestBulkOnCallUsesOneSnapshot: one transaction per call, whatever the number
// of schedules. A loop over the single-schedule projection would open one per
// schedule and describe as many different moments.
func TestBulkOnCallUsesOneSnapshot(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	var schedules []bulkSchedule
	for i := 0; i < 5; i++ {
		schedules = append(schedules, bulkSchedule{
			id:     fmt.Sprintf("sched-%02d", i),
			teamID: fmt.Sprintf("team-%02d", i),
			at:     start,
			cfg:    config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
		})
	}
	repo := bulkRepo(t, schedules...)
	repo.Calls = nil

	if _, err := New(repo).CurrentOnCallForAll(context.Background(), start.Add(time.Hour)); err != nil {
		t.Fatalf("CurrentOnCallForAll: %v", err)
	}
	if got := callCount(repo, "WithinSnapshot"); got != 1 {
		t.Errorf("%d snapshots opened for 5 schedules, want 1", got)
	}
}

// TestBulkOnCallQueryCount pins the cost of a tick, because the cost of a tick
// is round trips and nothing else. The worst case is 1 + 2A for A active
// schedules; the general case is 1 + 2A + D + P over deleted and broken ones.
// A third read sneaking into the per-schedule path fails this test rather than
// showing up as a slow tick in production.
func TestBulkOnCallQueryCount(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	at := start.Add(time.Hour)
	horizon := start

	t.Run("100 active schedules", func(t *testing.T) {
		var schedules []bulkSchedule
		for i := 0; i < 100; i++ {
			schedules = append(schedules, bulkSchedule{
				id:     fmt.Sprintf("sched-%03d", i),
				teamID: fmt.Sprintf("team-%03d", i),
				at:     start,
				cfg:    config("UTC", dailyPolicy("11:00"), group(groupA, "alice")),
			})
		}
		repo := bulkRepo(t, schedules...)
		repo.Calls = nil

		bulk, err := New(repo).CurrentOnCallForAll(context.Background(), at)
		if err != nil {
			t.Fatalf("CurrentOnCallForAll: %v", err)
		}
		if len(bulk.Schedules) != 100 {
			t.Fatalf("projected %d schedules, want 100", len(bulk.Schedules))
		}
		if got, want := readCount(repo), 1+2*100; got != want {
			t.Errorf("%d reads for 100 active schedules, want %d (%s)", got, want, callBreakdown(repo))
		}
	})

	t.Run("mixed states", func(t *testing.T) {
		// The instant is a day in, so the two deleted schedules are already
		// deleted at it - a schedule deleted later is still active now and
		// costs an active schedule's two reads.
		mixedAt := start.Add(24 * time.Hour)
		deletedAt := start.Add(12 * time.Hour)
		// 3 active, 2 deleted, 1 before its own horizon, 1 legacy, 1 broken chain.
		schedules := []bulkSchedule{
			{id: "sched-a1", teamID: "t-a1", at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
			{id: "sched-a2", teamID: "t-a2", at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
			{id: "sched-a3", teamID: "t-a3", at: start, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
			{id: "sched-d1", teamID: "t-d1", at: start, deletedAt: &deletedAt, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
			{id: "sched-d2", teamID: "t-d2", at: start, deletedAt: &deletedAt, cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
			{id: "sched-f1", teamID: "t-f1", at: mixedAt.Add(24 * time.Hour), cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"))},
		}
		repo := bulkRepo(t, schedules...)
		repo.SeedLegacyRoot("sched-l1", "t-l1")
		repo.SeedRoot(scheduleconfig.ScheduleRoot{
			ID: "sched-p1", TeamID: "t-p1", ConfigVersion: 1, HistoryCompleteFrom: &horizon,
		})
		repo.Calls = nil

		bulk, err := New(repo).CurrentOnCallForAll(context.Background(), mixedAt)
		if err != nil {
			t.Fatalf("CurrentOnCallForAll: %v", err)
		}
		if len(bulk.Failures) != 1 {
			t.Fatalf("failures = %v, want only the broken chain", bulk.Failures)
		}

		// A = 3 (two reads each), D = 2 (no overrides to read), P = 1 (the
		// refusal costs the revision read), and the schedule that starts later
		// plus the legacy row cost nothing at all.
		if got, want := readCount(repo), 1+2*3+2+1; got != want {
			t.Errorf("%d reads for the mixed fixture, want %d (%s)", got, want, callBreakdown(repo))
		}
	})
}

// TestCurrentOnCallForAllNowUsesTheServiceClock: the runtime calls the Now
// variant so there is exactly one clock. A consumer reaching for time.Now()
// would ignore WithClock and drift from the preview.
func TestCurrentOnCallForAllNowUsesTheServiceClock(t *testing.T) {
	start := utc(2026, 5, 1, 11, 0)
	repo := bulkRepo(t,
		bulkSchedule{id: "sched-1", teamID: "devops", at: start,
			cfg: config("UTC", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob"))},
	)
	// Two days on, the daily rotation has moved to the second group.
	frozen := start.Add(48 * time.Hour).Add(time.Hour)
	svc := New(repo, WithClock(func() time.Time { return frozen }))

	bulk, err := svc.CurrentOnCallForAllNow(context.Background())
	if err != nil {
		t.Fatalf("CurrentOnCallForAllNow: %v", err)
	}
	if len(bulk.Schedules) != 1 || bulk.Schedules[0].OnCall.L1 == nil {
		t.Fatalf("got %+v, want somebody on call", bulk.Schedules)
	}
	l1 := bulk.Schedules[0].OnCall.L1
	if !l1.AssignmentStart.Equal(start.Add(48 * time.Hour)) {
		t.Errorf("assignment start = %v, want the slot containing the frozen clock", l1.AssignmentStart)
	}
	if !bulk.Schedules[0].OnCall.At.Equal(frozen) {
		t.Errorf("projected at %v, want the service clock %v", bulk.Schedules[0].OnCall.At, frozen)
	}
}

// readCount is the number of database reads one call made: every recorded call
// except the transaction boundaries themselves.
func readCount(repo *fakes.ScheduleConfigRepo) int {
	n := 0
	for _, call := range repo.Calls {
		if call == "WithinSnapshot" || call == "WithinTx" || call == "Commit" {
			continue
		}
		n++
	}
	return n
}

func callBreakdown(repo *fakes.ScheduleConfigRepo) string {
	counts := map[string]int{}
	var order []string
	for _, call := range repo.Calls {
		if _, seen := counts[call]; !seen {
			order = append(order, call)
		}
		counts[call]++
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, fmt.Sprintf("%s=%d", name, counts[name]))
	}
	return strings.Join(parts, " ")
}
