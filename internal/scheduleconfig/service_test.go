package scheduleconfig_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
)

const (
	groupAlice = "6c0a1e2c-1111-4a3b-8c4d-000000000001"
	groupBob   = "6c0a1e2c-1111-4a3b-8c4d-000000000002"
)

func validConfig() rotation.ScheduleConfiguration {
	monday := 1
	weekly := rotation.RotationPolicy{
		SchemaVersion: rotation.PolicySchemaVersion,
		Cadence:       model.RotationWeekly,
		HandoffTime:   "11:00",
		HandoffDay:    &monday,
	}
	return rotation.ScheduleConfiguration{
		Timezone:         "Europe/Amsterdam",
		SlackUsergroupID: "S123",
		L1: rotation.LayerConfiguration{
			Enabled: true,
			Policy:  weekly,
			Groups: []rotation.RotationGroup{
				{ID: groupAlice, Members: []string{"alice"}},
				{ID: groupBob, Members: []string{"bob"}},
			},
		},
		L2: rotation.LayerConfiguration{
			Enabled: false,
			Policy:  weekly,
		},
		L2EscalationTimeoutMins: 5,
	}
}

// newService wires a Service to a fake repository with a pinned clock and
// deterministic IDs.
func newService(t *testing.T, now time.Time) (*scheduleconfig.Service, *fakes.ScheduleConfigRepo) {
	t.Helper()
	repo := fakes.NewScheduleConfigRepo()
	n := 0
	svc := scheduleconfig.NewService(repo,
		scheduleconfig.WithClock(func() time.Time { return now }),
		scheduleconfig.WithIDSource(func() string {
			n++
			return fmt.Sprintf("id-%d", n)
		}),
	)
	return svc, repo
}

func TestCreateScheduleWritesRootAndFirstRevisionTogether(t *testing.T) {
	// Deliberately sub-microsecond: it must not survive into persistence.
	now := time.Date(2026, 5, 4, 8, 30, 0, 123456789, time.UTC)
	svc, repo := newService(t, now)

	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig())
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	if rev.Version != 1 {
		t.Fatalf("revision version = %d, want 1", rev.Version)
	}
	if rev.EffectiveTo != nil {
		t.Fatalf("initial revision must be open-ended, got effective_to %v", *rev.EffectiveTo)
	}
	if want := scheduleconfig.NormalizeTimestamp(now); !rev.EffectiveFrom.Equal(want) {
		t.Fatalf("effective_from = %v, want normalized %v", rev.EffectiveFrom, want)
	}
	if rev.ChangeSummary == nil {
		t.Fatal("initial revision carries no change summary")
	}
	if rev.Snapshot.L1.PhaseAnchorSlotStart == nil || rev.Snapshot.L1.StartPosition == nil {
		t.Fatal("active L1 layer must carry a phase pair")
	}
	if *rev.Snapshot.L1.StartPosition != 0 {
		t.Fatalf("initial start position = %d, want 0", *rev.Snapshot.L1.StartPosition)
	}

	root, ok := repo.RootByTeam("devops")
	if !ok {
		t.Fatal("schedule root was not written")
	}
	if root.ConfigVersion != 1 {
		t.Fatalf("config version = %d, want 1", root.ConfigVersion)
	}
	if root.HistoryCompleteFrom == nil || !root.HistoryCompleteFrom.Equal(rev.EffectiveFrom) {
		t.Fatalf("history_complete_from = %v, want %v", root.HistoryCompleteFrom, rev.EffectiveFrom)
	}
	if got := repo.Revisions(root.ID); len(got) != 1 {
		t.Fatalf("got %d revisions, want exactly 1", len(got))
	}
}

func TestCreateScheduleSecondCreateForSameTeamConflicts(t *testing.T) {
	svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))

	if _, err := svc.CreateSchedule(context.Background(), "devops", validConfig()); err != nil {
		t.Fatalf("first CreateSchedule: %v", err)
	}
	_, err := svc.CreateSchedule(context.Background(), "devops", validConfig())
	if !errors.Is(err, scheduleconfig.ErrScheduleExists) {
		t.Fatalf("second CreateSchedule error = %v, want ErrScheduleExists", err)
	}
	if got := repo.RootCount(); got != 1 {
		t.Fatalf("got %d schedule roots, want 1", got)
	}
}

// A failure at any step must leave nothing behind: the create flow has no
// partial outcome in which a schedule exists without its first revision.
func TestCreateScheduleRollsBackEveryStep(t *testing.T) {
	boom := errors.New("injected failure")

	for _, step := range []string{"WithinTx", "CreateInitialSchedule", "Commit"} {
		t.Run(step, func(t *testing.T) {
			svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))
			repo.FailOn[step] = boom

			rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig())
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want injected failure", err)
			}
			if rev != nil {
				t.Fatalf("revision returned despite failure: %+v", rev)
			}
			if got := repo.RootCount(); got != 0 {
				t.Fatalf("got %d schedule roots after rollback, want 0", got)
			}
			if _, ok := repo.RootByTeam("devops"); ok {
				t.Fatal("team still owns a schedule after rollback")
			}
		})
	}
}

func TestCreateScheduleRejectsInvalidInputBeforeWriting(t *testing.T) {
	badTZ := validConfig()
	badTZ.Timezone = "Mars/Olympus"

	dupGroups := validConfig()
	dupGroups.L1.Groups[1].ID = groupAlice

	tests := []struct {
		name   string
		teamID string
		config rotation.ScheduleConfiguration
	}{
		{name: "empty team id", teamID: "", config: validConfig()},
		{name: "unknown timezone", teamID: "devops", config: badTZ},
		{name: "duplicate group ids", teamID: "devops", config: dupGroups},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))
			if _, err := svc.CreateSchedule(context.Background(), tc.teamID, tc.config); err == nil {
				t.Fatal("expected an error")
			}
			if got := repo.RootCount(); got != 0 {
				t.Fatalf("got %d schedule roots, want 0", got)
			}
			for _, call := range repo.Calls {
				if call == "CreateInitialSchedule" {
					t.Fatal("invalid input reached persistence")
				}
			}
		})
	}
}

// The current override projection must pick the latest revision per override
// first and drop tombstones only afterwards; the reverse order resurrects the
// revision that preceded a delete.
func TestFakeCurrentOverridesDoNotResurrectDeleted(t *testing.T) {
	svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))
	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig())
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	scheduleID := rev.ScheduleID

	from := time.Date(2026, 5, 5, 9, 0, 0, 0, time.UTC)
	err = repo.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		for i, deleted := range []bool{false, false, true} {
			rev := &scheduleconfig.OverrideRevision{
				RevisionID: fmt.Sprintf("ovr-rev-%d", i+1),
				OverrideID: "ovr-1",
				ScheduleID: scheduleID,
				Revision:   int64(i + 1),
				Layer:      scheduleconfig.LayerL1,
				UserID:     "alice",
				ValidFrom:  from,
				ValidTo:    from.Add(time.Hour),
				Deleted:    deleted,
			}
			if err := tx.InsertOverrideRevision(context.Background(), rev); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert override revisions: %v", err)
	}

	var current []scheduleconfig.OverrideRevision
	err = repo.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		var err error
		current, err = tx.GetCurrentOverrides(context.Background(), scheduleID)
		return err
	})
	if err != nil {
		t.Fatalf("GetCurrentOverrides: %v", err)
	}
	if len(current) != 0 {
		t.Fatalf("got %d current overrides after delete, want 0", len(current))
	}
	if got := len(repo.OverrideRevisions(scheduleID)); got != 3 {
		t.Fatalf("history holds %d revisions, want 3 (append-only)", got)
	}
}
