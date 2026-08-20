package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

func TestProcessNewAlertGroups(t *testing.T) {
	// Setup Store with MockStore
	s := store.NewMockStore()

	// Create team with policy routing in Store
	team := &model.Team{
		ID:              "devops",
		Name:            "DevOps Team",
		DefaultPolicyID: "default_policy",
		SeverityRoutes: map[string]string{
			"critical": "critical_policy",
		},
	}
	s.CreateTeam(team)

	// Create policy in Store (new pattern - policies from DB)
	policy := &model.EscalationPolicy{
		ID:   "critical_policy",
		Name: "Critical Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "step1",
				PolicyID:       "critical_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "U12345",
				DelaySeconds:   0,
				TimeoutSeconds: 30,
				MaxAttempts:    3,
			},
		},
	}
	s.CreateEscalationPolicy(policy)

	// Setup Config (only for firehose channels now)
	cfg := &config.Config{}

	e := NewEngine(s, &fakeProjection{}, cfg)

	// Seed a NEW alert group
	ag := &model.AlertGroup{
		ID:        "ag-1",
		DedupKey:  "dedup-1",
		Status:    model.AlertGroupStatusNew,
		TeamID:    "devops",
		Severity:  "critical",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("Failed to seed alert group: %v", err)
	}

	// Run Processing (once)
	e.ProcessNewAlertGroups(context.Background())

	// Verify State
	updated, err := s.GetActiveAlertGroup("dedup-1")
	if err != nil {
		t.Errorf("Failed to fetch updated alert group: %v", err)
	}

	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status Processing, got %s", updated.Status)
	}
	if updated.PolicyID != "critical_policy" {
		t.Errorf("Expected policy critical_policy, got %s", updated.PolicyID)
	}
}

func TestResolvePolicy(t *testing.T) {
	s := store.NewMockStore()

	// Create teams in Store
	s.CreateTeam(&model.Team{
		ID:              "devops",
		Name:            "DevOps",
		DefaultPolicyID: "default_policy",
		SeverityRoutes: map[string]string{
			"critical": "critical_policy",
		},
	})
	s.CreateTeam(&model.Team{
		ID:              "triage",
		Name:            "Triage",
		DefaultPolicyID: "triage_policy",
	})

	e := &Engine{store: s}

	tests := []struct {
		name     string
		team     string
		severity string
		want     string
	}{
		{
			name:     "Direct Match",
			team:     "devops",
			severity: "critical",
			want:     "critical_policy",
		},
		{
			name:     "Fallback to Default Route",
			team:     "devops",
			severity: "unknown",
			want:     "default_policy",
		},
		{
			name:     "Triage Default",
			team:     "triage",
			severity: "info",
			want:     "triage_policy",
		},
		{
			name:     "Team Not Found",
			team:     "missing_team",
			severity: "critical",
			want:     "", // No team = no policy
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.resolvePolicy(tt.team, tt.severity); got != tt.want {
				t.Errorf("resolvePolicy() = %v, want %v", got, tt.want)
			}
		})
	}

}

func TestPolicySnapshot_Versioning(t *testing.T) {
	s := store.NewMockStore()
	eng := NewEngine(s, &fakeProjection{}, &config.Config{})

	// 1. Setup Policy V1
	policyID := "mutable_policy"
	policy := &model.EscalationPolicy{
		ID:   policyID,
		Name: "Mutable Policy",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetID: "UserV1", StepIndex: 0, MaxAttempts: 3},
		},
	}
	s.CreateEscalationPolicy(policy)

	// 2. Setup Team using this policy
	teamID := "team_mutable"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})

	// 3. Process AG 1 (should get V1)
	ag1 := &model.AlertGroup{
		ID:       "ag1",
		DedupKey: "dk-ag1",
		Status:   model.AlertGroupStatusNew,
		TeamID:   teamID,
		Severity: "info",
	}
	s.CreateAlertGroup(ag1)

	eng.ProcessNewAlertGroups(context.Background())

	// Verify AG1 snapshot
	updatedAG1, _ := s.GetAlertGroupByID("ag1")
	if updatedAG1.PolicySnapshot == nil {
		t.Fatal("AG1 Snapshot missing")
	}
	if len(updatedAG1.PolicySnapshot.Steps) != 1 || updatedAG1.PolicySnapshot.Steps[0].TargetID != "UserV1" {
		t.Errorf("AG1 snapshot has wrong TargetID: %v", updatedAG1.PolicySnapshot.Steps[0].TargetID)
	}

	// 4. Update Policy (V2)
	policyV2 := &model.EscalationPolicy{
		ID:   policyID,
		Name: "Mutable Policy V2",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetID: "UserV2", StepIndex: 0, MaxAttempts: 3},
		},
	}
	s.CreateEscalationPolicy(policyV2)

	// 5. Process AG 2 (should get V2)
	ag2 := &model.AlertGroup{
		ID:       "ag2",
		DedupKey: "dk-ag2",
		Status:   model.AlertGroupStatusNew,
		TeamID:   teamID,
		Severity: "info",
	}
	s.CreateAlertGroup(ag2)

	eng.ProcessNewAlertGroups(context.Background())

	updatedAG2, _ := s.GetAlertGroupByID("ag2")
	if updatedAG2.PolicySnapshot == nil {
		t.Fatal("AG2 Snapshot missing")
	}
	if len(updatedAG2.PolicySnapshot.Steps) != 1 || updatedAG2.PolicySnapshot.Steps[0].TargetID != "UserV2" {
		t.Errorf("AG2 snapshot expected UserV2, got %s", updatedAG2.PolicySnapshot.Steps[0].TargetID)
	}

	// 6. Verify AG1 Snapshot UNCHANGED
	updatedAG1_Check, _ := s.GetAlertGroupByID("ag1")
	if updatedAG1_Check.PolicySnapshot.Steps[0].TargetID != "UserV1" {
		t.Errorf("AG1 snapshot changed to %s! Should stay UserV1", updatedAG1_Check.PolicySnapshot.Steps[0].TargetID)
	}
}

func TestEngine_BuildFailure_AGStaysNew(t *testing.T) {
	s := store.NewMockStore()

	// Create team that routes to a policy with an invalid step (empty TargetID)
	teamID := "team_bad_policy"
	policyID := "bad_policy"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Bad Policy",
		Steps: []*model.EscalationStep{
			{
				Provider:    "slack",
				TargetKind:  "dm",
				TargetType:  "user",
				TargetID:    "", // Empty target → Build() returns error
				StepIndex:   0,
				MaxAttempts: 3,
			},
		},
	})

	cfg := &config.Config{}
	eng := NewEngine(s, &fakeProjection{}, cfg)

	ag := &model.AlertGroup{
		ID:       "ag-build-fail",
		DedupKey: "dedup-build-fail",
		Status:   model.AlertGroupStatusNew,
		TeamID:   teamID,
		Severity: "info",
	}
	s.CreateAlertGroup(ag)

	eng.ProcessNewAlertGroups(context.Background())

	// AG should stay "new" because Build() failed — not marked as "processing"
	updated, err := s.GetAlertGroupByID("ag-build-fail")
	if err != nil {
		t.Fatalf("Failed to fetch alert group: %v", err)
	}
	if updated.Status != model.AlertGroupStatusNew {
		t.Errorf("Expected AG status to stay 'new' after Build failure, got '%s'", updated.Status)
	}
}

func TestEngine_FirehoseCreation(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{
		Global: config.GlobalConfig{
			FirehoseCriticalChannel: "C_FIRE",
		},
	}
	eng := NewEngine(s, &fakeProjection{}, cfg)

	// Create AG (Critical) - no policy, firehose only
	ag := &model.AlertGroup{ID: "ag_fire", Severity: "critical", DedupKey: "dk_fire", Status: model.AlertGroupStatusNew}
	s.CreateAlertGroup(ag)

	eng.ProcessNewAlertGroups(context.Background())

	// Firehose is now step 0 in the unified escalation job
	job, err := s.FindJobByIdentity(jobdedup.Escalation("ag_fire"))
	if err != nil {
		t.Fatalf("Escalation job not found: %v", err)
	}
	if job == nil {
		t.Fatal("Escalation job is nil")
	}
	if job.Type != "escalation" {
		t.Errorf("Expected job type escalation, got %s", job.Type)
	}

	// Step 0 should be firehose
	fetchedSteps := s.GetJobStepsByJobID(job.ID)
	step := fetchedSteps[0]
	if step.StepType != "firehose" {
		t.Errorf("Expected step type firehose, got %s", step.StepType)
	}

	var data model.EscalationStepData
	if err := json.Unmarshal(step.Data, &data); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if data.TargetID != "C_FIRE" {
		t.Errorf("Expected target C_FIRE, got %s", data.TargetID)
	}
	if !data.IsFirehose {
		t.Error("IsFirehose should be true")
	}
}

func TestEngine_ReconcileStaleProcessing(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}

	// Create team and policy
	teamID := "team-reconcile"
	policyID := "reconcile_policy"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Reconcile Policy",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "U999", StepIndex: 0, MaxAttempts: 3},
		},
	})

	// Simulate crash scenario: AG is in "processing" with stale updated_at, no job exists
	ag := &model.AlertGroup{
		ID:        "ag-orphan",
		DedupKey:  "dk-orphan",
		Status:    model.AlertGroupStatusProcessing,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now().Add(-60 * time.Second), // Stale: 60s ago
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("Failed to create AG: %v", err)
	}

	eng := NewEngine(s, &fakeProjection{}, cfg)
	eng.ProcessNewAlertGroups(context.Background())

	// Verify: AG should still be "processing" (re-processed by engine)
	updated, err := s.GetAlertGroupByID("ag-orphan")
	if err != nil {
		t.Fatalf("Failed to get AG: %v", err)
	}
	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status processing, got %s", updated.Status)
	}

	// Verify: a job should now exist for this AG
	job, err := s.FindJobByIdentity(jobdedup.Escalation("ag-orphan"))
	if err != nil {
		t.Fatalf("Job lookup failed: %v", err)
	}
	if job == nil {
		t.Fatal("Expected job to be created for orphaned AG, got nil")
	}
	if job.Type != "escalation" {
		t.Errorf("Expected job type escalation, got %s", job.Type)
	}
}

// TestEngine_ScheduleRecreation_OnCallConsistency verifies that after a schedule
// is recreated (new UUID), the on-call snapshot and the job step targets resolve
// to the same user (from the current schedule, not the stale one).
func TestEngine_ScheduleRecreation_OnCallConsistency(t *testing.T) {
	s := store.NewMockStore()

	// Users
	userOld := &model.User{ID: "user-old", Name: "John"}
	userNew := &model.User{ID: "user-new", Name: "Denis"}
	s.CreateUser(userOld)
	s.CreateUser(userNew)

	// Team + policy referencing old schedule
	teamID := "team-stale"
	policyID := "pol-stale"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		Name:            "Stale Team",
		DefaultPolicyID: policyID,
	})

	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Stale Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old", MaxAttempts: 3},
		},
	})

	// === SIMULATE SCHEDULE RECREATION ===
	// The orphaned old schedule is still readable by the ID the policy names and
	// still holds user-old; the team now belongs to a new schedule with user-new.
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			teamID: teamSchedule("sched-new", onDuty("g-new", userNew.ID)),
		},
		schedules: map[string]schedulerender.OnCall{
			"sched-old": onDuty("g-old", userOld.ID),
			"sched-new": onDuty("g-new", userNew.ID),
		},
	}

	// Create alert group
	ag := &model.AlertGroup{
		ID:        "ag-stale-engine",
		DedupKey:  "dk-stale-engine",
		Status:    model.AlertGroupStatusNew,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	cfg := &config.Config{}
	eng := NewEngine(s, proj, cfg)
	eng.ProcessNewAlertGroups(context.Background())

	// 1. Verify on-call snapshot shows user-new (Denis)
	updated, err := s.GetAlertGroupByID("ag-stale-engine")
	if err != nil {
		t.Fatalf("Failed to get AG: %v", err)
	}
	snapshotUserID := ""
	if updated.OnCallSnapshot != nil && len(updated.OnCallSnapshot.L1Users) > 0 {
		snapshotUserID = updated.OnCallSnapshot.L1Users[0].ID
	}
	if snapshotUserID != userNew.ID {
		t.Errorf("OnCallSnapshot should show '%s' (Denis), got '%s'", userNew.ID, snapshotUserID)
	}

	// 2. Verify job step targets the same user
	job, err := s.FindJobByIdentity(jobdedup.Escalation("ag-stale-engine"))
	if err != nil || job == nil {
		t.Fatalf("Job not found: %v", err)
	}
	fetchedSteps := s.GetJobStepsByJobID(job.ID)
	var dmStep *model.JobStep
	for _, step := range fetchedSteps {
		if step.StepType == "dm" {
			dmStep = step
			break
		}
	}
	if dmStep == nil {
		t.Fatal("Expected a dm step in the job")
	}

	var stepData model.EscalationStepData
	json.Unmarshal(dmStep.Data, &stepData)

	// Critical consistency check: snapshot and job step must agree
	if stepData.TargetID != snapshotUserID {
		t.Errorf("REGRESSION: Job step targets '%s' but OnCallSnapshot shows '%s' — stale schedule bug!",
			stepData.TargetID, snapshotUserID)
	}
	if stepData.TargetID != userNew.ID {
		t.Errorf("REGRESSION: Job step should target '%s' (Denis), got '%s'", userNew.ID, stepData.TargetID)
	}
}

func TestEngine_StaleProcessing_WithSucceededJob_NotReconciled(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}

	teamID := "team-succeeded-noop"
	policyID := "policy-succeeded"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Succeeded Policy",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "U111", StepIndex: 0, MaxAttempts: 3},
		},
	})

	// AG in processing with stale updated_at
	ag := &model.AlertGroup{
		ID:        "ag-succeeded-noop",
		DedupKey:  "dk-succeeded-noop",
		Status:    model.AlertGroupStatusProcessing,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now().Add(-60 * time.Second),
	}
	s.CreateAlertGroup(ag)

	// Simulate: escalation job already ran and succeeded
	eng := NewEngine(s, &fakeProjection{}, cfg)
	escBuilder := builders.NewEscalationJobBuilder(s, &fakeProjection{}, cfg)
	job, stages, steps, snapshot, _ := escBuilder.Build(context.Background(), ag, policyID, schedulerender.TeamOnCallRead(schedulerender.TeamOnCall{}, nil))
	// Create job directly (bypassing engine) and mark as succeeded
	if err := s.SeedEscalationJob(ag.ID, job, stages, steps); err != nil {
		t.Fatalf("SeedEscalationJob: %v", err)
	}
	s.MarkJobSucceeded(jobdedup.Escalation(ag.ID))
	// Save snapshot so we can verify it's not overwritten
	s.UpdateAlertGroupPolicy(ag.ID, snapshot.PolicyID, snapshot)

	beforeRun := time.Now()
	eng.ProcessNewAlertGroups(context.Background())

	// AG should NOT be picked up — job exists (succeeded), not a true orphan
	updated, _ := s.GetAlertGroupByID("ag-succeeded-noop")
	if updated.UpdatedAt.After(beforeRun) {
		t.Error("Stale processing AG with succeeded job should NOT be re-processed by engine")
	}
}

func TestEnsureEscalationJob_SkipsAckedAG(t *testing.T) {
	s := store.NewMockStore()

	// Create team + policy
	teamID := "team-ack-skip"
	policyID := "policy-ack-skip"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Ack Skip Policy",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "U111", StepIndex: 0, MaxAttempts: 3},
		},
	})

	// Create AG already acknowledged
	ag := &model.AlertGroup{
		ID:        "ag-acked",
		DedupKey:  "dk-acked",
		Status:    model.AlertGroupStatusAcknowledged,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	// Build job for this AG (as engine would)
	cfg := &config.Config{}
	eng := NewEngine(s, &fakeProjection{}, cfg)
	escBuilder := builders.NewEscalationJobBuilder(s, &fakeProjection{}, cfg)
	job, stages, steps, snapshot, err := escBuilder.Build(context.Background(), ag, policyID, schedulerender.TeamOnCallRead(schedulerender.TeamOnCall{}, nil))
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if job == nil {
		t.Fatal("Expected job to be built")
	}
	_ = eng // engine not used directly here

	// Call EnsureEscalationJob — should return (false, nil)
	created, err := s.EnsureEscalationJob(ag.ID, job, stages, steps, snapshot)
	if err != nil {
		t.Fatalf("EnsureEscalationJob error: %v", err)
	}
	if created {
		t.Error("Expected created=false for acknowledged AG")
	}

	// Verify AG status unchanged
	updated, _ := s.GetAlertGroupByID("ag-acked")
	if updated.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status to stay 'acknowledged', got '%s'", updated.Status)
	}
}

func TestEnsureEscalationJob_DedupSkipsSnapshotOverwrite(t *testing.T) {
	s := store.NewMockStore()

	teamID := "team-dedup-snap"
	policyID := "policy-dedup"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Dedup Policy",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "UserV1", StepIndex: 0, MaxAttempts: 3},
		},
	})

	// Create AG
	ag := &model.AlertGroup{
		ID:        "ag-dedup",
		DedupKey:  "dk-dedup",
		Status:    model.AlertGroupStatusNew,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	cfg := &config.Config{}
	eng := NewEngine(s, &fakeProjection{}, cfg)

	// First call — should create job with V1 snapshot
	eng.ProcessNewAlertGroups(context.Background())

	updatedAG, _ := s.GetAlertGroupByID("ag-dedup")
	if updatedAG.PolicySnapshot == nil {
		t.Fatal("Expected V1 snapshot to be saved")
	}
	if updatedAG.PolicySnapshot.Steps[0].TargetID != "UserV1" {
		t.Fatalf("Expected UserV1, got %s", updatedAG.PolicySnapshot.Steps[0].TargetID)
	}

	// Update policy to V2
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Dedup Policy V2",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "UserV2", StepIndex: 0, MaxAttempts: 3},
		},
	})

	// Force AG back to "new" to re-trigger processing
	s.UpdateAlertGroupStatus("ag-dedup", model.AlertGroupStatusNew)

	// Second call — job already exists (dedup), snapshot should NOT be overwritten
	eng.ProcessNewAlertGroups(context.Background())

	updatedAG2, _ := s.GetAlertGroupByID("ag-dedup")
	if updatedAG2.PolicySnapshot == nil {
		t.Fatal("Snapshot should still exist")
	}
	if updatedAG2.PolicySnapshot.Steps[0].TargetID != "UserV1" {
		t.Errorf("Snapshot should stay UserV1, got %s", updatedAG2.PolicySnapshot.Steps[0].TargetID)
	}
}

func TestEnsureEscalationJob_SkipsSucceededJob(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}

	teamID := "team-succeeded-skip"
	policyID := "policy-succeeded-skip"
	s.CreateTeam(&model.Team{
		ID:              teamID,
		DefaultPolicyID: policyID,
	})
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Succeeded Skip Policy",
		Steps: []*model.EscalationStep{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "U222", StepIndex: 0, MaxAttempts: 3},
		},
	})

	// Create AG in "new"
	ag := &model.AlertGroup{
		ID:        "ag-succeeded-skip",
		DedupKey:  "dk-succeeded-skip",
		Status:    model.AlertGroupStatusNew,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	eng := NewEngine(s, &fakeProjection{}, cfg)

	// First run — creates escalation job
	eng.ProcessNewAlertGroups(context.Background())

	// Verify job was created
	job, err := s.FindJobByIdentity(jobdedup.Escalation("ag-succeeded-skip"))
	if err != nil {
		t.Fatalf("Job not found: %v", err)
	}
	if job == nil {
		t.Fatal("Expected job to be created")
	}

	// Mark job as succeeded
	s.MarkJobSucceeded(jobdedup.Escalation("ag-succeeded-skip"))

	// Force AG back to new to re-trigger processing
	s.UpdateAlertGroupStatus("ag-succeeded-skip", model.AlertGroupStatusNew)

	// Second run — should NOT create a new job (DB invariant: 1 escalation per AG)
	eng.ProcessNewAlertGroups(context.Background())

	// Verify AG was picked up but dedup prevented a new job
	updated, _ := s.GetAlertGroupByID("ag-succeeded-skip")
	// AG transitions to processing because EnsureEscalationJob updates status before dedup check
	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected AG status 'processing' (transitioned before dedup), got '%s'", updated.Status)
	}
}

func TestEngine_JobNil_StaleProcessing_TouchesUpdatedAt(t *testing.T) {
	s := store.NewMockStore()

	// Team with no policy — will produce job == nil
	teamID := "team-no-policy"
	s.CreateTeam(&model.Team{
		ID:   teamID,
		Name: "No Policy Team",
	})

	// AG in stale processing (no job)
	staleTime := time.Now().Add(-60 * time.Second)
	ag := &model.AlertGroup{
		ID:        "ag-stale-touch",
		DedupKey:  "dk-stale-touch",
		Status:    model.AlertGroupStatusProcessing,
		TeamID:    teamID,
		Severity:  "info",
		CreatedAt: time.Now(),
		UpdatedAt: staleTime,
	}
	s.CreateAlertGroup(ag)

	cfg := &config.Config{}
	eng := NewEngine(s, &fakeProjection{}, cfg)

	// First tick — should pick up stale AG and touch updated_at
	eng.ProcessNewAlertGroups(context.Background())

	updated, _ := s.GetAlertGroupByID("ag-stale-touch")
	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status to stay 'processing', got '%s'", updated.Status)
	}
	if !updated.UpdatedAt.After(staleTime) {
		t.Error("Expected updated_at to be refreshed (touched)")
	}

	// Second tick — AG should NOT be picked up again (updated_at is fresh)
	beforeSecondTick := time.Now()
	time.Sleep(10 * time.Millisecond) // ensure time difference
	eng.ProcessNewAlertGroups(context.Background())

	updated2, _ := s.GetAlertGroupByID("ag-stale-touch")
	if updated2.UpdatedAt.After(beforeSecondTick) {
		t.Error("AG should not have been re-processed on second tick (updated_at is fresh)")
	}
}

// TestEngine_OnCallSnapshot_OverrideCarriesSource: the override information is
// not lost now that the projection answers instead of a legacy override row.
// L1Users names the stand-in, and Source says that is why they are on it.
func TestEngine_OnCallSnapshot_OverrideCarriesSource(t *testing.T) {
	s := store.NewMockStore()
	standIn := &model.User{ID: "user-standin", Name: "Carol"}
	s.CreateUser(standIn)

	teamID := "team-override"
	s.CreateTeam(&model.Team{ID: teamID, Name: "Override Team"})
	ag := &model.AlertGroup{
		ID: "ag-override", DedupKey: "dk-override", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	proj := &fakeProjection{teams: map[string]schedulerender.TeamOnCall{
		teamID: teamSchedule("sched-1", onDutyByOverride("ovr-1", standIn.ID)),
	}}
	NewEngine(s, proj, &config.Config{}).ProcessNewAlertGroups(context.Background())

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	snap := updated.OnCallSnapshot
	if snap == nil {
		t.Fatal("no on-call snapshot was stored")
	}
	if len(snap.L1Users) != 1 || snap.L1Users[0].ID != standIn.ID {
		t.Fatalf("L1Users = %+v, want the stand-in", snap.L1Users)
	}
	if snap.Source != schedulerender.SourceOverride {
		t.Errorf("source = %q, want %q", snap.Source, schedulerender.SourceOverride)
	}
	if snap.L1Since == nil || !snap.L1Since.Equal(projectionBase) {
		t.Errorf("L1Since = %v, want the assignment start", snap.L1Since)
	}
	if snap.L1Until == nil || !snap.L1Until.Equal(projectionBase.Add(24*time.Hour)) {
		t.Errorf("L1Until = %v, want the assignment end", snap.L1Until)
	}
}

// TestEngine_OnCallSnapshot_NoSchedule_IsEmptyNotAnError: "nobody was on call" is
// a fact worth recording on the alert group, and it is not a failure.
func TestEngine_OnCallSnapshot_NoSchedule_IsEmptyNotAnError(t *testing.T) {
	s := store.NewMockStore()
	teamID := "team-scheduleless"
	s.CreateTeam(&model.Team{ID: teamID, Name: "No Schedule"})
	ag := &model.AlertGroup{
		ID: "ag-no-sched", DedupKey: "dk-no-sched", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	NewEngine(s, &fakeProjection{}, &config.Config{}).ProcessNewAlertGroups(context.Background())

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if updated.OnCallSnapshot == nil {
		t.Fatal("no snapshot stored; an empty one states that nobody was on call")
	}
	if len(updated.OnCallSnapshot.L1Users) != 0 {
		t.Errorf("L1Users = %+v, want nobody", updated.OnCallSnapshot.L1Users)
	}
	if updated.OnCallSnapshot.Source != "" {
		t.Errorf("source = %q, want it empty when nobody is on duty", updated.OnCallSnapshot.Source)
	}
}

// TestEngine_OnCallSnapshot_DeletedSchedule_IsEmpty: a deleted schedule answers
// for its team, and its answer is nobody.
func TestEngine_OnCallSnapshot_DeletedSchedule_IsEmpty(t *testing.T) {
	s := store.NewMockStore()
	teamID := "team-deleted-sched"
	s.CreateTeam(&model.Team{ID: teamID, Name: "Deleted Schedule"})
	ag := &model.AlertGroup{
		ID: "ag-deleted-sched", DedupKey: "dk-deleted-sched", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	deletedAt := projectionBase
	proj := &fakeProjection{teams: map[string]schedulerender.TeamOnCall{
		teamID: {ScheduleID: "sched-1", DeletedAt: &deletedAt, OnCall: schedulerender.OnCall{At: projectionBase}},
	}}
	NewEngine(s, proj, &config.Config{}).ProcessNewAlertGroups(context.Background())

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if updated.OnCallSnapshot == nil || len(updated.OnCallSnapshot.L1Users) != 0 {
		t.Fatalf("snapshot = %+v, want an empty one", updated.OnCallSnapshot)
	}
}

// TestEngine_OnCallSnapshot_L2IsRecorded: the snapshot keeps its shape, L2
// included, so its existing readers are unaffected.
func TestEngine_OnCallSnapshot_L2IsRecorded(t *testing.T) {
	s := store.NewMockStore()
	primary := &model.User{ID: "user-l1", Name: "Alice"}
	backup := &model.User{ID: "user-l2", Name: "Bob"}
	s.CreateUser(primary)
	s.CreateUser(backup)

	teamID := "team-l2"
	s.CreateTeam(&model.Team{ID: teamID, Name: "Two Layers"})
	ag := &model.AlertGroup{
		ID: "ag-l2", DedupKey: "dk-l2", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	withL2 := onDuty("g-a", primary.ID)
	withL2.L2 = layer(backup.ID, schedulerender.SourceRotation, backup.ID)
	proj := &fakeProjection{teams: map[string]schedulerender.TeamOnCall{
		teamID: teamSchedule("sched-1", withL2),
	}}
	NewEngine(s, proj, &config.Config{}).ProcessNewAlertGroups(context.Background())

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if updated.OnCallSnapshot == nil || updated.OnCallSnapshot.L2User == nil {
		t.Fatalf("snapshot = %+v, want the L2 user recorded", updated.OnCallSnapshot)
	}
	if updated.OnCallSnapshot.L2User.ID != backup.ID {
		t.Errorf("L2User = %s, want %s", updated.OnCallSnapshot.L2User.ID, backup.ID)
	}
}

// TestEngine_OnCallReadOncePerAlertGroup: the job and the snapshot are two
// halves of one statement about who was on call. Reading on-call twice lets a
// handoff land between them, and then the alert group records a group the job
// never paged - so the engine reads once and hands the same answer to both.
func TestEngine_OnCallReadOncePerAlertGroup(t *testing.T) {
	s := store.NewMockStore()
	outgoing := &model.User{ID: "user-outgoing", Name: "Alice"}
	incoming := &model.User{ID: "user-incoming", Name: "Bob"}
	s.CreateUser(outgoing)
	s.CreateUser(incoming)

	teamID := "team-handoff-race"
	policyID := "pol-handoff-race"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Schedule Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-1", MaxAttempts: 3},
			// A second step naming the SAME schedule: two steps of one job are
			// one question, and they must not be answered differently either.
			{ID: "s2", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-1", MaxAttempts: 3},
		},
	})
	s.CreateTeam(&model.Team{ID: teamID, Name: "Handoff Race", DefaultPolicyID: policyID})

	ag := &model.AlertGroup{
		ID: "ag-handoff-race", DedupKey: "dk-handoff-race", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	// The shift changes hands the instant after the first read.
	after := teamSchedule("sched-1", onDuty("g-incoming", incoming.ID))
	proj := &countingProjection{
		first: teamSchedule("sched-1", onDuty("g-outgoing", outgoing.ID)),
		then:  &after,
	}
	NewEngine(s, proj, &config.Config{}).ProcessNewAlertGroups(context.Background())

	if proj.calls != 1 {
		t.Errorf("projection read %d times for one alert group, want 1", proj.calls)
	}

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if updated.OnCallSnapshot == nil || len(updated.OnCallSnapshot.L1Users) != 1 {
		t.Fatalf("snapshot = %+v, want one user on call", updated.OnCallSnapshot)
	}
	snapshotUser := updated.OnCallSnapshot.L1Users[0].ID

	job, err := s.FindJobByIdentity(jobdedup.Escalation(ag.ID))
	if err != nil || job == nil {
		t.Fatalf("job not found: %v", err)
	}
	var targets []string
	for _, step := range s.GetJobStepsByJobID(job.ID) {
		if step.StepType != "dm" {
			continue
		}
		var data model.EscalationStepData
		if err := json.Unmarshal(step.Data, &data); err != nil {
			t.Fatalf("unmarshal step data: %v", err)
		}
		targets = append(targets, data.TargetID)
	}
	if len(targets) != 2 {
		t.Fatalf("job has %d dm steps, want one per policy step: %v", len(targets), targets)
	}
	for _, target := range targets {
		if target != snapshotUser {
			t.Errorf("job step pages %q while the snapshot records %q", target, snapshotUser)
		}
	}
}

// TestEngine_OnCallReadFailure_DefersEverything: a tick that could not read the
// projection records nothing, pages nobody, and commits nothing.
//
// Writing an empty snapshot would state that nobody was on call, which is a
// claim about the schedule rather than about the database that just refused to
// answer. And the failure has to reach the builder AS a failure: handed on as a
// zero value it reads as "this team has no schedule", which sends the builder to
// the schedule ID stored on the policy step - here a schedule the team no longer
// owns, which would answer, and page the wrong people.
//
// The job is not committed either, and the alert group is left new. A job built
// on an unknown roster pages nobody, and an alert group that owns one is never
// picked up again - so committing it would decide, permanently, that this alert
// reaches no one. TestEngine_OnCallReadRecovers_PagesOnCall is the other side of
// this: left alone, the next tick delivers.
func TestEngine_OnCallReadFailure_DefersEverything(t *testing.T) {
	s := store.NewMockStore()
	stale := &model.User{ID: "user-stale", Name: "John"}
	s.CreateUser(stale)

	teamID := "team-unreadable"
	policyID := "pol-unreadable"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Unreadable Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})
	s.CreateTeam(&model.Team{ID: teamID, Name: "Unreadable", DefaultPolicyID: policyID})
	ag := &model.AlertGroup{
		ID: "ag-unreadable", DedupKey: "dk-unreadable", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	// The team read fails; the stale schedule would answer perfectly well.
	proj := &countingProjection{
		err:  errors.New("could not begin transaction"),
		byID: map[string]schedulerender.OnCall{"sched-old": onDuty("g-old", stale.ID)},
	}
	deferralsBefore := counterValue(t, metrics.EngineEscalationBuildDeferralsTotal)
	NewEngine(s, proj, &config.Config{}).ProcessNewAlertGroups(context.Background())

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if updated.OnCallSnapshot != nil {
		t.Errorf("snapshot = %+v after an unreadable projection, want none", updated.OnCallSnapshot)
	}
	if updated.Status != model.AlertGroupStatusNew {
		t.Errorf("status = %q after an unreadable projection, want %q so the next tick picks it up",
			updated.Status, model.AlertGroupStatusNew)
	}
	if proj.scheduleCalls != 0 {
		t.Errorf("the fallback schedule was read %d times after a failed team read, want 0", proj.scheduleCalls)
	}

	job, err := s.FindJobByIdentity(jobdedup.Escalation(ag.ID))
	if err == nil && job != nil {
		t.Errorf("job %s was committed on an unknown roster - nothing would ever rebuild it", job.ID)
	}

	if got := counterValue(t, metrics.EngineEscalationBuildDeferralsTotal) - deferralsBefore; got != 1 {
		t.Errorf("deferrals counted = %v, want 1", got)
	}
}

// TestEngine_OnCallReadRecovers_PagesOnCall is the regression for the defect
// itself: a read that failed once must cost the alert a tick, not its page.
//
// Before the fix, the first tick committed a job whose schedule step named
// nobody. That job made the alert group ineligible for pickup, so the second
// tick - with the projection answering again - never ran, and the person on duty
// was simply never told. Here the first tick defers, and the second delivers.
func TestEngine_OnCallReadRecovers_PagesOnCall(t *testing.T) {
	s := store.NewMockStore()
	onDutyUser := &model.User{ID: "user-on-duty", Name: "Alice"}
	s.CreateUser(onDutyUser)

	teamID := "team-recovers"
	policyID := "pol-recovers"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Recovering Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-current"},
		},
	})
	s.CreateTeam(&model.Team{ID: teamID, Name: "Recovers", DefaultPolicyID: policyID})
	ag := &model.AlertGroup{
		ID: "ag-recovers", DedupKey: "dk-recovers", TeamID: teamID, Severity: "info",
		Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	// The first read fails, every later one answers.
	proj := &countingProjection{
		err:          errors.New("could not begin transaction"),
		errUntilCall: 1,
		first:        teamSchedule("sched-current", onDuty("g-a", onDutyUser.ID)),
	}
	engine := NewEngine(s, proj, &config.Config{})

	deferralsBefore := counterValue(t, metrics.EngineEscalationBuildDeferralsTotal)
	engine.ProcessNewAlertGroups(context.Background())

	if job, _ := s.FindJobByIdentity(jobdedup.Escalation(ag.ID)); job != nil {
		t.Fatalf("first tick committed job %s although the roster was unknown", job.ID)
	}
	deferralsAfterFirst := counterValue(t, metrics.EngineEscalationBuildDeferralsTotal)
	if got := deferralsAfterFirst - deferralsBefore; got != 1 {
		t.Fatalf("deferrals counted on the first tick = %v, want 1", got)
	}

	engine.ProcessNewAlertGroups(context.Background())

	job, err := s.FindJobByIdentity(jobdedup.Escalation(ag.ID))
	if err != nil || job == nil {
		t.Fatalf("second tick built no job although the projection answered: %v", err)
	}
	targets := dmTargetsOf(t, s.GetJobStepsByJobID(job.ID))
	if len(targets) != 1 || targets[0] != onDutyUser.ID {
		t.Errorf("second tick pages %v, want the on-call user %q", targets, onDutyUser.ID)
	}

	updated, err := s.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if updated.OnCallSnapshot == nil || len(updated.OnCallSnapshot.L1Users) != 1 ||
		updated.OnCallSnapshot.L1Users[0].ID != onDutyUser.ID {
		t.Errorf("snapshot = %+v, want the same on-call user the job pages", updated.OnCallSnapshot)
	}

	if got := counterValue(t, metrics.EngineEscalationBuildDeferralsTotal) - deferralsAfterFirst; got != 0 {
		t.Errorf("deferrals counted on the successful tick = %v, want 0", got)
	}
}

// TestEngine_DeferredTick_NamesTheBatchOnceAndNothingPerGroup pins both halves
// of what the tick may write while nothing can be built.
//
// The line naming the batch has to come BEFORE any group is touched: everything
// else is written after the work it describes, so without it a group that hangs
// or panics leaves nothing to narrow the search to. And it has to be the only
// line, whatever the batch size - a deferred group returns every second, so a
// line per group is a line per group per second for as long as the trouble
// lasts. Both properties were broken once and are cheap to break again.
//
// The batch order is whatever the store returned, so the expected first group
// is read out of the line rather than assumed: MockStore walks a map, and
// assuming one made this test fail 16 times in 200 runs.
func TestEngine_DeferredTick_NamesTheBatchOnceAndNothingPerGroup(t *testing.T) {
	s := store.NewMockStore()
	policyID := "pol-batch"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Batch",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-x"},
		},
	})
	s.CreateTeam(&model.Team{ID: "team-batch", Name: "Batch", DefaultPolicyID: policyID})
	for _, id := range []string{"ag-first", "ag-second"} {
		s.CreateAlertGroup(&model.AlertGroup{
			ID: id, DedupKey: "dk-" + id, TeamID: "team-batch", Severity: "info",
			Status: model.AlertGroupStatusNew, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
	}

	var buf bytes.Buffer
	restore := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(restore)

	proj := &countingProjection{err: errors.New("could not begin transaction")}
	NewEngine(s, proj, &config.Config{}).ProcessNewAlertGroups(context.Background())

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("a tick that built nothing wrote %d lines, want 2 (the batch and the tally):\n%s",
			len(lines), buf.String())
	}
	for _, id := range []string{"ag-first", "ag-second"} {
		if !strings.Contains(lines[0], id) {
			t.Errorf("the batch line does not name %s, so a hang on it could not be narrowed down: %q", id, lines[0])
		}
	}

	_, listed, ok := strings.Cut(lines[0], "group(s): ")
	if !ok {
		t.Fatalf("the batch line does not list any group: %q", lines[0])
	}
	firstInBatch := strings.Fields(listed)[0]
	if !strings.Contains(lines[1], firstInBatch) {
		t.Errorf("the tally names a different group than the batch begins with (%s): %q", firstInBatch, lines[1])
	}
}

// TestAlertGroupIDs_CapsTheList: the breadcrumb is bounded, and the boundary is
// where a bound goes wrong. Beyond the cap the ids are not merely unlisted, they
// are unavailable - which is one half of why this line narrows a hang down
// rather than naming it.
func TestAlertGroupIDs_CapsTheList(t *testing.T) {
	batch := func(n int) []*model.AlertGroup {
		out := make([]*model.AlertGroup, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, &model.AlertGroup{ID: fmt.Sprintf("ag-%02d", i)})
		}
		return out
	}

	ten := alertGroupIDs(batch(10))
	if strings.Contains(ten, "more") {
		t.Errorf("a batch of exactly 10 was truncated: %q", ten)
	}
	if !strings.Contains(ten, "ag-09") {
		t.Errorf("a batch of exactly 10 lost its last id: %q", ten)
	}

	eleven := alertGroupIDs(batch(11))
	if !strings.Contains(eleven, "ag-09") || strings.Contains(eleven, "ag-10") {
		t.Errorf("a batch of 11 should list ids 00..09 and no more, got %q", eleven)
	}
	if !strings.Contains(eleven, "(and 1 more)") {
		t.Errorf("a batch of 11 does not say how many it left out: %q", eleven)
	}
}

// dmTargetsOf lists what the dm steps of a built job would page.
func dmTargetsOf(t *testing.T, steps []*model.JobStep) []string {
	t.Helper()
	var out []string
	for _, step := range steps {
		if step.StepType != "dm" {
			continue
		}
		var data model.EscalationStepData
		if err := json.Unmarshal(step.Data, &data); err != nil {
			t.Fatalf("unmarshal step data: %v", err)
		}
		out = append(out, data.TargetID)
	}
	return out
}

// counterValue reads a counter without prometheus/testutil, which would promote
// prometheus/common from an indirect dependency for one assertion. Counters are
// process-wide, so callers compare a delta rather than an absolute.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
