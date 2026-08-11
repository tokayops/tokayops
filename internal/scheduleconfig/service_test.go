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
	repo.SetTeamMembers("devops", "alice", "bob", "carol", "dave")
	// The actor exists without being on the team: an admin editing someone
	// else's schedule is the normal case.
	repo.AddUsers("actor")
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

	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
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

	if _, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil); err != nil {
		t.Fatalf("first CreateSchedule: %v", err)
	}
	_, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
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

			rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
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
			if _, err := svc.CreateSchedule(context.Background(), tc.teamID, tc.config, "actor", nil); err == nil {
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

// The fake and the PostgreSQL repository share one preparation step, so a
// value one stores is never one the other rejects. An ID source that runs dry
// is the cheapest way to prove it: the fake used to keep a revision with an
// empty ID that the database has always refused.
func TestCreateScheduleRejectsWhatTheDatabaseWouldReject(t *testing.T) {
	repo := fakes.NewScheduleConfigRepo()
	repo.SetTeamMembers("devops", "alice", "bob")
	repo.AddUsers("actor")
	n := 0
	svc := scheduleconfig.NewService(repo, scheduleconfig.WithIDSource(func() string {
		n++
		if n == 1 {
			return "schedule-1"
		}
		return ""
	}))

	_, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("error = %v, want ErrInvariantViolation", err)
	}
	if got := repo.RootCount(); got != 0 {
		t.Fatalf("got %d schedule roots, want 0", got)
	}
	if got := repo.Revisions("schedule-1"); len(got) != 0 {
		t.Fatalf("got %d revisions, want 0", len(got))
	}
}

// The revision the caller holds after a write must be what a read returns.
// Storage canonicalizes a snapshot on the way in, so a writer that skipped
// that step would hand back nil groups and a non-UTC anchor where persistence
// has [] and UTC. The store-side twin of this is TestCreateScheduleStoresCanonicalSnapshot.
func TestCreateScheduleReturnsCanonicalSnapshot(t *testing.T) {
	svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))

	// The configuration leaves L2 disabled with no groups, which is exactly
	// the nil slice persistence turns into [].
	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	if rev.Snapshot.L2.Groups == nil {
		t.Fatal("returned snapshot still carries a nil group slice")
	}
	if loc := rev.Snapshot.L1.PhaseAnchorSlotStart.Location(); loc != time.UTC {
		t.Fatalf("returned anchor location = %v, want UTC", loc)
	}

	stored := repo.Revisions(rev.ScheduleID)
	if len(stored) != 1 {
		t.Fatalf("got %d revisions, want 1", len(stored))
	}
	returned, err := rotation.EncodeSnapshot(rev.Snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot(returned): %v", err)
	}
	kept, err := rotation.EncodeSnapshot(stored[0].Snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot(stored): %v", err)
	}
	if string(returned) != string(kept) {
		t.Fatalf("returned and stored snapshots differ:\n%s\n%s", returned, kept)
	}
}

// A database hands back data, not aliases. The fake must do the same, or a
// test that mutates a configuration after saving it would silently "change
// history" and pass against behaviour PostgreSQL would never produce.
func TestFakeStoresSnapshotsByValue(t *testing.T) {
	svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))

	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// Mutate everything reachable through the returned revision.
	rev.Snapshot.L1.Groups[0].Members[0] = "mallory"
	rev.Snapshot.L1.Groups = append(rev.Snapshot.L1.Groups, rotation.RotationGroup{ID: "extra"})
	*rev.Snapshot.L1.StartPosition = 99
	*rev.Snapshot.L1.PhaseAnchorSlotStart = time.Unix(0, 0).UTC()
	*rev.Snapshot.L1.Policy.HandoffDay = 6
	rev.ChangeSummary.L1PhaseAction = "tampered"

	stored := repo.Revisions(rev.ScheduleID)
	if len(stored) != 1 {
		t.Fatalf("got %d revisions, want 1", len(stored))
	}
	got := stored[0].Snapshot
	if len(got.L1.Groups) != 2 {
		t.Fatalf("stored group count changed to %d", len(got.L1.Groups))
	}
	if got.L1.Groups[0].Members[0] != "alice" {
		t.Fatalf("stored member changed to %q", got.L1.Groups[0].Members[0])
	}
	if *got.L1.StartPosition != 0 {
		t.Fatalf("stored start position changed to %d", *got.L1.StartPosition)
	}
	if got.L1.PhaseAnchorSlotStart.Equal(time.Unix(0, 0).UTC()) {
		t.Fatal("stored phase anchor was mutated through the caller's pointer")
	}
	if *got.L1.Policy.HandoffDay != 1 {
		t.Fatalf("stored handoff day changed to %d", *got.L1.Policy.HandoffDay)
	}
	if stored[0].ChangeSummary.L1PhaseAction == "tampered" {
		t.Fatal("stored change summary was mutated through the caller's pointer")
	}

	// Mutating what a diagnostic accessor returns must not reach back either.
	stored[0].Snapshot.L1.Groups[0].Members[0] = "mallory"
	if again := repo.Revisions(rev.ScheduleID); again[0].Snapshot.L1.Groups[0].Members[0] != "alice" {
		t.Fatal("the accessor handed out an alias of the stored revision")
	}
}

func TestFakeRollbackRestoresDeepState(t *testing.T) {
	svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))
	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	boom := errors.New("injected failure")
	err = repo.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		if err := tx.CloseRevision(context.Background(), rev.ScheduleID, rev.ID,
			rev.EffectiveFrom.Add(time.Hour)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	if got := repo.Revisions(rev.ScheduleID); got[0].EffectiveTo != nil {
		t.Fatalf("revision stayed closed at %v after rollback", *got[0].EffectiveTo)
	}
}

// The current override projection must pick the latest revision per override
// first and drop tombstones only afterwards; the reverse order resurrects the
// revision that preceded a delete.
func TestFakeCurrentOverridesDoNotResurrectDeleted(t *testing.T) {
	svc, repo := newService(t, time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC))
	rev, err := svc.CreateSchedule(context.Background(), "devops", validConfig(), "actor", nil)
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
		current, err = tx.GetOverrideProjectionInRange(context.Background(), scheduleID, nil, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("GetOverrideProjectionInRange: %v", err)
	}
	if len(current) != 0 {
		t.Fatalf("got %d current overrides after delete, want 0", len(current))
	}
	if got := len(repo.OverrideRevisions(scheduleID)); got != 3 {
		t.Fatalf("history holds %d revisions, want 3 (append-only)", got)
	}
}

// A rolled-back transaction must put back everything it snapshotted, and the
// snapshot is easy to under-copy: two maps were added to the fake state for
// DeleteTeam and clone() did not know about them, so any rollback wiped every
// team the test had registered. Nothing failed, because the tests that delete
// teams do not roll back and the tests that roll back do not use teams - which
// is exactly the kind of hole a fake grows quietly.
func TestFakeRollbackRestoresTeamState(t *testing.T) {
	repo := fakes.NewScheduleConfigRepo()
	repo.AddTeams("devops")
	repo.AddTeamIntegration("billing")

	boom := errors.New("boom")
	err := repo.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		if err := tx.DeleteTeam(context.Background(), "devops"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("WithinTx = %v, want the injected failure", err)
	}

	// Both maps have to survive: the one the transaction touched, and the one
	// it never looked at.
	if err := repo.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.LockTeam(context.Background(), "devops")
	}); err != nil {
		t.Errorf("the rolled-back delete stayed applied: %v", err)
	}
	if err := repo.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.DeleteTeam(context.Background(), "billing")
	}); !errors.Is(err, scheduleconfig.ErrTeamHasIntegrations) {
		t.Errorf("the untouched integration state was lost on rollback: %v", err)
	}
}
