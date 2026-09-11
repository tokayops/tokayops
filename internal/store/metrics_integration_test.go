package store

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

func TestGetMetricsSnapshot_ActiveAlertGroups(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-metrics-1"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Metrics Team", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Create 2 firing + 1 resolved AG
	createAG := func(id string, severity string, status model.AlertGroupStatus) {
		ag := &model.AlertGroup{
			ID: id, AlertKey: "dk-" + id, Status: status,
			TeamID: teamID, TeamNameSnapshot: "Metrics Team", Severity: severity,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := s.CreateAlertGroup(ag); err != nil {
			t.Fatalf("CreateAlertGroup %s: %v", id, err)
		}
	}
	createAG("ag-m-1", "critical", model.AlertGroupStatusProcessing)
	createAG("ag-m-2", "warning", model.AlertGroupStatusTriggered)
	createAG("ag-m-3", "critical", model.AlertGroupStatusResolved)

	snap, err := s.GetMetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("GetMetricsSnapshot: %v", err)
	}

	// Should see 2 active (processing + triggered), not the resolved one
	totalActive := 0
	for _, ag := range snap.ActiveAlertGroups {
		totalActive += ag.Count
	}
	if totalActive != 2 {
		t.Errorf("expected 2 active alert groups, got %d", totalActive)
	}
}

// The on-call gauges read the revision in force, so the fixtures are built
// through the command service rather than by writing rows: what is being
// asserted is that the query agrees with what a save actually produces.
//
// Both gauges are covered by one fixture on purpose. They are two questions
// about the same six states, and building the states twice invites the two
// tests to drift into describing different worlds.
func TestGetMetricsSnapshotOnCallGauges(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	svc := newTestScheduleService(s, at)

	// A rotation of two groups: somebody is on duty, and it is not always the
	// same person.
	seedTeam(t, s, "team-rotating", "alice", "bob")
	mustCreateSchedule(t, svc, "team-rotating", revTestConfig())

	// One group of one person: on duty, and always the same person.
	seedTeam(t, s, "team-lonely", "carol")
	mustCreateSchedule(t, svc, "team-lonely", layerConfig(
		rotation.RotationGroup{ID: revGroupAlice, Members: []string{"carol"}}))

	// One group of TWO people. They are on duty together and forever, so the
	// team has on-call - but it is not a single assignee, and this is the case
	// the pre-revision query got wrong: it counted groups, not people.
	seedTeam(t, s, "team-pair", "dave", "erin")
	mustCreateSchedule(t, svc, "team-pair", layerConfig(
		rotation.RotationGroup{ID: revGroupAlice, Members: []string{"dave", "erin"}}))

	// L1 switched off. Its groups survive in the snapshot - only the phase pair
	// is cleared - so a query that looked at groups alone would report this
	// team as covered, and by a single assignee at that.
	seedTeam(t, s, "team-disabled", "frank")
	disabled := layerConfig(rotation.RotationGroup{ID: revGroupAlice, Members: []string{"frank"}})
	disabled.L1.Enabled = false
	mustCreateSchedule(t, svc, "team-disabled", disabled)

	// Deleted: the schedule existed, and does not now.
	seedTeam(t, s, "team-deleted", "grace")
	created := mustCreateSchedule(t, svc, "team-deleted", layerConfig(
		rotation.RotationGroup{ID: revGroupAlice, Members: []string{"grace"}}))
	deleter := newTestScheduleService(s, at.Add(time.Hour))
	if err := deleter.Delete(context.Background(), "team-deleted",
		scheduleconfig.DeleteCommand{ExpectedVersion: created.Version}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// No schedule at all.
	seedTeam(t, s, "team-none")

	snap, err := s.GetMetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("GetMetricsSnapshot: %v", err)
	}

	// Covered: rotating, lonely, pair. Not covered: disabled, deleted, none.
	if snap.TeamsWithoutOnCall != 3 {
		t.Errorf("teams_without_oncall = %d, want 3 (disabled, deleted, no schedule)",
			snap.TeamsWithoutOnCall)
	}
	// Only the one-group-of-one-person schedule. The pair is two people, the
	// disabled one has nobody, the deleted one no longer exists.
	if snap.TeamsWithPermanentOnCall != 1 {
		t.Errorf("teams_with_permanent_oncall = %d, want 1 (only the single assignee)",
			snap.TeamsWithPermanentOnCall)
	}
}

// layerConfig is revTestConfig with L1 replaced by the given groups.
func layerConfig(groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	cfg := revTestConfig()
	cfg.L1.Groups = groups
	return cfg
}

func mustCreateSchedule(t *testing.T, svc *scheduleconfig.Service, teamID string,
	cfg rotation.ScheduleConfiguration) *scheduleconfig.ScheduleRevision {

	t.Helper()
	res, err := createViaSave(context.Background(), svc, teamID, cfg, "", nil)
	if err != nil {
		t.Fatalf("CreateSchedule %s: %v", teamID, err)
	}
	return res
}

func TestGetMetricsSnapshot_TeamsWithoutPolicy(t *testing.T) {
	s := setupTestDB(t)

	now := time.Now()

	// Team with no default_policy_id
	if err := s.CreateTeam(&model.Team{ID: "team-nopol", Name: "No Policy", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	// Team with default_policy_id set
	if err := s.CreateTeam(&model.Team{ID: "team-pol", Name: "Has Policy", DefaultPolicyID: "pol-1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.GetMetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("GetMetricsSnapshot: %v", err)
	}

	if snap.TeamsWithoutPolicy != 1 {
		t.Errorf("expected 1 team without policy, got %d", snap.TeamsWithoutPolicy)
	}
}
