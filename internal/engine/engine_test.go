package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/model"
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

	e := NewEngine(s, cfg)

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
	e.ProcessNewAlertGroups()

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

	e := &Engine{store: s, cfg: &config.Config{}}

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
	eng := NewEngine(s, &config.Config{})

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

	eng.ProcessNewAlertGroups()

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

	eng.ProcessNewAlertGroups()

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
	eng := NewEngine(s, cfg)

	ag := &model.AlertGroup{
		ID:       "ag-build-fail",
		DedupKey: "dedup-build-fail",
		Status:   model.AlertGroupStatusNew,
		TeamID:   teamID,
		Severity: "info",
	}
	s.CreateAlertGroup(ag)

	eng.ProcessNewAlertGroups()

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
	eng := NewEngine(s, cfg)

	// Create AG (Critical) - no policy, firehose only
	ag := &model.AlertGroup{ID: "ag_fire", Severity: "critical", DedupKey: "dk_fire", Status: model.AlertGroupStatusNew}
	s.CreateAlertGroup(ag)

	eng.ProcessNewAlertGroups()

	// Firehose is now step 0 in the unified escalation job (dedup key = ag.DedupKey)
	job, err := s.GetJobByDedupKey("dk_fire")
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

	eng := NewEngine(s, cfg)
	eng.ProcessNewAlertGroups()

	// Verify: AG should still be "processing" (re-processed by engine)
	updated, err := s.GetAlertGroupByID("ag-orphan")
	if err != nil {
		t.Fatalf("Failed to get AG: %v", err)
	}
	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status processing, got %s", updated.Status)
	}

	// Verify: a job should now exist for this AG
	job, err := s.GetJobByDedupKey("dk-orphan")
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

	// Old schedule with user-old on-call
	s.CreateSchedule(&model.Schedule{
		ID: "sched-old", TeamID: teamID,
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups("sched-old", [][]string{{userOld.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: "sched-old", Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{userOld.ID}},
	})

	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Stale Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old", MaxAttempts: 3},
		},
	})

	// === SIMULATE SCHEDULE RECREATION ===
	s.CreateSchedule(&model.Schedule{
		ID: "sched-old", TeamID: "",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.CreateSchedule(&model.Schedule{
		ID: "sched-new", TeamID: teamID,
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups("sched-new", [][]string{{userNew.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: "sched-new", Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{userNew.ID}},
	})

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
	eng := NewEngine(s, cfg)
	eng.ProcessNewAlertGroups()

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
	job, err := s.GetJobByDedupKey("dk-stale-engine")
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
	eng := NewEngine(s, cfg)
	escBuilder := builders.NewEscalationJobBuilder(s, cfg)
	job, stages, steps, snapshot, _ := escBuilder.Build(ag, policyID)
	// Create job directly (bypassing engine) and mark as succeeded
	s.CreateJobWithDedup(job, stages, steps)
	s.MarkJobSucceeded(ag.DedupKey)
	// Save snapshot so we can verify it's not overwritten
	s.UpdateAlertGroupPolicy(ag.ID, snapshot.PolicyID, snapshot)

	beforeRun := time.Now()
	eng.ProcessNewAlertGroups()

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
	eng := NewEngine(s, cfg)
	escBuilder := builders.NewEscalationJobBuilder(s, cfg)
	job, stages, steps, snapshot, err := escBuilder.Build(ag, policyID)
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
	eng := NewEngine(s, cfg)

	// First call — should create job with V1 snapshot
	eng.ProcessNewAlertGroups()

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
	eng.ProcessNewAlertGroups()

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

	eng := NewEngine(s, cfg)

	// First run — creates escalation job
	eng.ProcessNewAlertGroups()

	// Verify job was created
	job, err := s.GetJobByDedupKey("dk-succeeded-skip")
	if err != nil {
		t.Fatalf("Job not found: %v", err)
	}
	if job == nil {
		t.Fatal("Expected job to be created")
	}

	// Mark job as succeeded
	s.MarkJobSucceeded("dk-succeeded-skip")

	// Force AG back to new to re-trigger processing
	s.UpdateAlertGroupStatus("ag-succeeded-skip", model.AlertGroupStatusNew)

	// Second run — should NOT create a new job (DB invariant: 1 escalation per AG)
	eng.ProcessNewAlertGroups()

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
	eng := NewEngine(s, cfg)

	// First tick — should pick up stale AG and touch updated_at
	eng.ProcessNewAlertGroups()

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
	eng.ProcessNewAlertGroups()

	updated2, _ := s.GetAlertGroupByID("ag-stale-touch")
	if updated2.UpdatedAt.After(beforeSecondTick) {
		t.Error("AG should not have been re-processed on second tick (updated_at is fresh)")
	}
}
