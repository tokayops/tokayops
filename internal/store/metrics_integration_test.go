package store

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/google/uuid"
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
			ID: id, DedupKey: "dk-" + id, Status: status,
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

	snap, err := s.GetMetricsSnapshot()
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

func TestGetMetricsSnapshot_TeamsWithoutOnCall(t *testing.T) {
	s := setupTestDB(t)

	now := time.Now()

	// Team A: has schedule + L1 epoch with users → has on-call
	if err := s.CreateTeam(&model.Team{ID: "team-a", Name: "Team A", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	schedA := &model.Schedule{
		ID: uuid.New().String(), TeamID: "team-a", Timezone: "UTC",
		L1RotationType: "daily", L1HandoffTime: "09:00",
		CreatedAt: now,
	}
	if err := s.CreateSchedule(schedA); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRotationEpoch(&model.RotationEpoch{
		ID: uuid.New().String(), ScheduleID: schedA.ID, Layer: "l1",
		Groups: [][]string{{"user1"}, {"user2"}}, StartTime: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Team B: has schedule + L1 epoch with EMPTY users → no on-call
	if err := s.CreateTeam(&model.Team{ID: "team-b", Name: "Team B", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	schedB := &model.Schedule{
		ID: uuid.New().String(), TeamID: "team-b", Timezone: "UTC",
		L1RotationType: "daily", L1HandoffTime: "09:00",
		CreatedAt: now,
	}
	if err := s.CreateSchedule(schedB); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRotationEpoch(&model.RotationEpoch{
		ID: uuid.New().String(), ScheduleID: schedB.ID, Layer: "l1",
		Groups: [][]string{}, StartTime: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Team C: no schedule at all → no on-call
	if err := s.CreateTeam(&model.Team{ID: "team-c", Name: "Team C", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("GetMetricsSnapshot: %v", err)
	}

	// Team B (empty rotation) + Team C (no schedule) = 2 without on-call
	if snap.TeamsWithoutOnCall != 2 {
		t.Errorf("expected 2 teams without on-call, got %d", snap.TeamsWithoutOnCall)
	}
}

func TestGetMetricsSnapshot_TeamsWithPermanentOnCall(t *testing.T) {
	s := setupTestDB(t)

	now := time.Now()

	// Team with single-user L1 rotation → permanent on-call
	if err := s.CreateTeam(&model.Team{ID: "team-perm", Name: "Team Perm", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sched := &model.Schedule{
		ID: uuid.New().String(), TeamID: "team-perm", Timezone: "UTC",
		L1RotationType: "daily", L1HandoffTime: "09:00",
		CreatedAt: now,
	}
	if err := s.CreateSchedule(sched); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRotationEpoch(&model.RotationEpoch{
		ID: uuid.New().String(), ScheduleID: sched.ID, Layer: "l1",
		Groups: [][]string{{"lonely-user"}}, StartTime: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Team with 2-user rotation → not permanent
	if err := s.CreateTeam(&model.Team{ID: "team-good", Name: "Team Good", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	sched2 := &model.Schedule{
		ID: uuid.New().String(), TeamID: "team-good", Timezone: "UTC",
		L1RotationType: "daily", L1HandoffTime: "09:00",
		CreatedAt: now,
	}
	if err := s.CreateSchedule(sched2); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRotationEpoch(&model.RotationEpoch{
		ID: uuid.New().String(), ScheduleID: sched2.ID, Layer: "l1",
		Groups: [][]string{{"user1"}, {"user2"}}, StartTime: now, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("GetMetricsSnapshot: %v", err)
	}

	if snap.TeamsWithPermanentOnCall != 1 {
		t.Errorf("expected 1 team with permanent on-call, got %d", snap.TeamsWithPermanentOnCall)
	}
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

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("GetMetricsSnapshot: %v", err)
	}

	if snap.TeamsWithoutPolicy != 1 {
		t.Errorf("expected 1 team without policy, got %d", snap.TeamsWithoutPolicy)
	}
}
