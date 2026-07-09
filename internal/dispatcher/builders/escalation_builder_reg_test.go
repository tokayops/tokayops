package builders

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// TestEscalationJobBuilder_MessagePropagation verifies that the Message field
// from EscalationStep is successfully propagated to the JobStep Data.
func TestEscalationJobBuilder_MessagePropagation(t *testing.T) {
	s := store.NewMockStore()
	builder := NewEscalationJobBuilder(s, nil)

	// Setup Policy with a message
	policyID := "pol-msg-1"
	policy := &model.EscalationPolicy{
		ID:   policyID,
		Name: "Message Policy",
		Steps: []*model.EscalationStep{
			{
				ID:           "step-1",
				Provider:     "slack",
				TargetKind:   "dm",
				TargetType:   "user",
				TargetID:     "user-1",
				Message:      "Wake up, Neo!", // Critical Custom Message
				DelaySeconds: 60,
			},
		},
	}
	s.CreateEscalationPolicy(policy)

	// Setup AlertGroup
	ag := &model.AlertGroup{
		ID:       "ag-1",
		DedupKey: "dedup-1",
		Status:   model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// Build Job
	job, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}

	// Verify Message in Step Data
	var data model.EscalationStepData
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("Failed to unmarshal step data: %v", err)
	}

	if data.Message != "Wake up, Neo!" {
		t.Errorf("Regression! Message not propagated. Expected 'Wake up, Neo!', got '%s'", data.Message)
	}

	// Also verify Job Payload snapshot (optional, but good practice)
	var payload model.EscalationPayload
	json.Unmarshal(job.Payload, &payload)
	// Currently snapshot might not include message either, checking if we need to update snapshot too.
	// But primarily Step Data is what Executor uses.
}

// TestEscalationJobBuilder_TelegramStepPropagation verifies telegram policy steps
// propagate Provider/TargetKind generically — Epic 8 Sprint 2 expects ZERO builder
// changes, so this is a regression guard for the capability-driven path.
func TestEscalationJobBuilder_TelegramStepPropagation(t *testing.T) {
	s := store.NewMockStore()
	builder := NewEscalationJobBuilder(s, nil)

	policyID := "pol-tg-1"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Telegram Policy",
		Steps: []*model.EscalationStep{
			{ID: "step-1", Provider: "telegram", TargetKind: "dm", TargetType: "user", TargetID: "user-1", Message: "ping"},
		},
	})

	ag := &model.AlertGroup{ID: "ag-tg", DedupKey: "dk-tg", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}
	if steps[0].StepType != "dm" {
		t.Errorf("StepType should be dm (from TargetKind), got %s", steps[0].StepType)
	}
	var data model.EscalationStepData
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("unmarshal step data: %v", err)
	}
	if data.ProviderName != "telegram" {
		t.Errorf("ProviderName should be telegram, got %s", data.ProviderName)
	}
}

// TestEscalationJobBuilder_TelegramScheduleFanOut verifies a telegram dm→schedule
// step fans out to per-user telegram steps and a telegram channel step is carried
// through — the same generic path used for slack.
func TestEscalationJobBuilder_TelegramScheduleFanOut(t *testing.T) {
	s := store.NewMockStore()

	user1 := &model.User{ID: "user-1", Name: "Alice"}
	s.CreateUser(user1)

	schedID := "sched-tg-fanout"
	s.CreateSchedule(&model.Schedule{
		ID:              schedID,
		TeamID:          "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups(schedID, [][]string{{user1.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: schedID, Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{user1.ID}},
	})

	policyID := "pol-tg-fanout"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Telegram Fan-out",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "telegram", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
			{ID: "s2", Provider: "telegram", TargetKind: "channel", TargetType: "channel", TargetID: "-100123"},
		},
	})

	ag := &model.AlertGroup{ID: "ag-tg-fanout", DedupKey: "dk-tg-fanout", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	// Empty cfg → no firehose stage prepended.
	builder := NewEscalationJobBuilder(s, &config.Config{})
	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("Expected 2 steps (dm fan-out to 1 user + channel), got %d", len(steps))
	}

	if steps[0].StepType != "dm" {
		t.Errorf("Step 0 should be dm, got %s", steps[0].StepType)
	}
	var dm model.EscalationStepData
	json.Unmarshal(steps[0].Data, &dm)
	if dm.ProviderName != "telegram" || dm.TargetID != "user-1" {
		t.Errorf("dm step should be telegram→user-1, got provider=%s target=%s", dm.ProviderName, dm.TargetID)
	}

	if steps[1].StepType != "channel" {
		t.Errorf("Step 1 should be channel, got %s", steps[1].StepType)
	}
	var ch model.EscalationStepData
	json.Unmarshal(steps[1].Data, &ch)
	if ch.ProviderName != "telegram" {
		t.Errorf("channel step should be telegram, got %s", ch.ProviderName)
	}
}

// TestEscalationJobBuilder_ContinueOnFailurePropagation verifies that the ContinueOnFailure field
// from EscalationStep is successfully propagated to the JobStep.
func TestEscalationJobBuilder_ContinueOnFailurePropagation(t *testing.T) {
	s := store.NewMockStore()
	builder := NewEscalationJobBuilder(s, nil)

	// Setup Policy with ContinueOnFailure=true
	policyID := "pol-cof-1"
	policy := &model.EscalationPolicy{
		ID:   policyID,
		Name: "ContinueOnFailure Policy",
		Steps: []*model.EscalationStep{
			{
				ID:                "step-1",
				Provider:          "slack",
				TargetKind:        "dm",
				TargetType:        "user",
				TargetID:          "user-1",
				ContinueOnFailure: true, // Critical flag
			},
			{
				ID:                "step-2",
				Provider:          "slack",
				TargetKind:        "dm",
				TargetType:        "user",
				TargetID:          "user-2",
				ContinueOnFailure: false, // Default
			},
		},
	}
	s.CreateEscalationPolicy(policy)

	// Setup AlertGroup
	ag := &model.AlertGroup{
		ID:       "ag-cof-1",
		DedupKey: "dedup-cof-1",
		Status:   model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// Build Job
	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if len(steps) != 2 {
		t.Fatalf("Expected 2 steps, got %d", len(steps))
	}

	// Verify ContinueOnFailure on first step
	if !steps[0].ContinueOnFailure {
		t.Errorf("Regression! ContinueOnFailure not propagated for step 0. Expected true, got false")
	}

	// Verify ContinueOnFailure on second step (default false)
	if steps[1].ContinueOnFailure {
		t.Errorf("Step 1 should have ContinueOnFailure=false, got true")
	}
}

// TestEscalationJobBuilder_ScheduleFanOut validates that schedule targets
// create parallel steps within a single stage (fan-out).
func TestEscalationJobBuilder_ScheduleFanOut(t *testing.T) {
	s := store.NewMockStore()

	// Setup users
	user1 := &model.User{ID: "user-1", Name: "Alice"}
	user2 := &model.User{ID: "user-2", Name: "Bob"}
	s.CreateUser(user1)
	s.CreateUser(user2)

	// Setup schedule with L1=user1, L2=user2
	schedID := "sched-fanout"
	s.CreateSchedule(&model.Schedule{
		ID:              schedID,
		TeamID:          "team-1",
		L2Enabled:       true,
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups(schedID, [][]string{{user1.ID}})
	s.SetScheduleUsers(schedID, "l2", []string{user2.ID})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: schedID, Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{user1.ID}},
	})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: schedID, Layer: "l2",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{user2.ID}},
	})

	// Policy: firehose → schedule(fan-out) → channel
	cfg := &config.Config{}
	cfg.Global.FirehoseWarningChannel = "C_FIREHOSE"
	policyID := "pol-fanout"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Fan-out Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
			{ID: "s2", Provider: "slack", TargetKind: "channel", TargetType: "channel", TargetID: "C_ALERTS"},
		},
	})

	ag := &model.AlertGroup{ID: "ag-fanout", DedupKey: "dk-fanout", Severity: "warning", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, cfg)
	_, stages, steps, snapshot, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// 3 stages: firehose, schedule fan-out (L1 only, no L2 additive), channel
	if len(stages) != 3 {
		t.Fatalf("Expected 3 stages, got %d", len(stages))
	}

	// 3 steps: 1 firehose + 1 DM (L1 only, L2 not added) + 1 channel
	if len(steps) != 3 {
		t.Fatalf("Expected 3 steps (L2 should not be additive), got %d", len(steps))
	}

	// Stage 0: firehose (active)
	if stages[0].Status != model.JobStageStatusActive {
		t.Errorf("Stage 0 should be active, got %s", stages[0].Status)
	}
	if steps[0].StepType != "firehose" {
		t.Errorf("Step 0 should be firehose, got %s", steps[0].StepType)
	}

	// Stage 1: 1 DM step for L1 user only (blocked)
	if stages[1].Status != model.JobStageStatusBlocked {
		t.Errorf("Stage 1 should be blocked, got %s", stages[1].Status)
	}
	if steps[1].StageID != stages[1].ID {
		t.Error("Step 1 should belong to stage 1")
	}
	if steps[1].StepType != "dm" {
		t.Errorf("Step 1 should be dm, got %s", steps[1].StepType)
	}
	var dmData model.EscalationStepData
	json.Unmarshal(steps[1].Data, &dmData)
	if dmData.TargetID != "user-1" {
		t.Errorf("DM step should target L1 user user-1, got %s", dmData.TargetID)
	}

	// Stage 2: channel (blocked)
	if steps[2].StepType != "channel" {
		t.Errorf("Step 2 should be channel, got %s", steps[2].StepType)
	}

	// Snapshot: 3 entries with correct StageIndex
	if len(snapshot.Steps) != 3 {
		t.Fatalf("Expected 3 snapshot steps, got %d", len(snapshot.Steps))
	}
	if snapshot.Steps[0].StageIndex != 0 {
		t.Errorf("Snapshot step 0 StageIndex should be 0, got %d", snapshot.Steps[0].StageIndex)
	}
	if snapshot.Steps[1].StageIndex != 1 {
		t.Errorf("Snapshot step 1 StageIndex should be 1, got %d", snapshot.Steps[1].StageIndex)
	}
	if snapshot.Steps[2].StageIndex != 2 {
		t.Errorf("Snapshot step 2 StageIndex should be 2, got %d", snapshot.Steps[2].StageIndex)
	}
}

// TestEscalationJobBuilder_ScheduleOverride validates that overrides take
// priority over L1 rotation when resolving schedule targets.
func TestEscalationJobBuilder_ScheduleOverride(t *testing.T) {
	s := store.NewMockStore()

	user1 := &model.User{ID: "user-1", Name: "Regular"}
	user2 := &model.User{ID: "user-2", Name: "Override"}
	s.CreateUser(user1)
	s.CreateUser(user2)

	schedID := "sched-override"
	s.CreateSchedule(&model.Schedule{
		ID: schedID, TeamID: "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups(schedID, [][]string{{user1.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: schedID, Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{user1.ID}},
	})
	// Override: user2 is on-call instead
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID: "ov-1", ScheduleID: schedID, UserID: user2.ID,
		StartTime: time.Now().Add(-1 * time.Hour),
		EndTime:   time.Now().Add(1 * time.Hour),
	})

	policyID := "pol-ov"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Override Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
		},
	})

	ag := &model.AlertGroup{ID: "ag-ov", DedupKey: "dk-ov", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, nil)
	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}

	var data model.EscalationStepData
	json.Unmarshal(steps[0].Data, &data)
	if data.TargetID != user2.ID {
		t.Errorf("Expected override user %s, got %s", user2.ID, data.TargetID)
	}
}

// TestEscalationJobBuilder_StaleScheduleID_ResolvesCurrentUser verifies that
// when a policy step references a stale schedule UUID (old schedule still exists
// but is orphaned), the builder resolves on-call from the team's current schedule.
func TestEscalationJobBuilder_StaleScheduleID_ResolvesCurrentUser(t *testing.T) {
	s := store.NewMockStore()

	// Users
	userOld := &model.User{ID: "user-old", Name: "John"}
	userNew := &model.User{ID: "user-new", Name: "Denis"}
	s.CreateUser(userOld)
	s.CreateUser(userNew)

	// Old schedule for team-1 with user-old on-call
	s.CreateSchedule(&model.Schedule{
		ID: "sched-old", TeamID: "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups("sched-old", [][]string{{userOld.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: "sched-old", Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{userOld.ID}},
	})

	// Policy referencing old schedule
	policyID := "pol-stale"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Stale Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})

	// === SIMULATE SCHEDULE RECREATION ===
	// Orphan old schedule (clear TeamID, epochs remain)
	s.CreateSchedule(&model.Schedule{
		ID: "sched-old", TeamID: "",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	// Create new schedule for team-1
	s.CreateSchedule(&model.Schedule{
		ID: "sched-new", TeamID: "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups("sched-new", [][]string{{userNew.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: "sched-new", Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{userNew.ID}},
	})

	// AlertGroup belongs to team-1
	ag := &model.AlertGroup{
		ID: "ag-stale", DedupKey: "dk-stale", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, nil)
	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}

	var data model.EscalationStepData
	json.Unmarshal(steps[0].Data, &data)
	if data.TargetID != userNew.ID {
		t.Errorf("REGRESSION: Builder used stale schedule! Expected target '%s' (Denis), got '%s'", userNew.ID, data.TargetID)
	}
}

// TestEscalationJobBuilder_DeletedSchedule_ResolvesCurrentUser verifies that
// when a policy step references a deleted schedule, the builder falls back
// to the team's current schedule.
func TestEscalationJobBuilder_DeletedSchedule_ResolvesCurrentUser(t *testing.T) {
	s := store.NewMockStore()

	userOld := &model.User{ID: "user-old", Name: "John"}
	userNew := &model.User{ID: "user-new", Name: "Denis"}
	s.CreateUser(userOld)
	s.CreateUser(userNew)

	// Old schedule
	s.CreateSchedule(&model.Schedule{
		ID: "sched-old", TeamID: "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups("sched-old", [][]string{{userOld.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: "sched-old", Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{userOld.ID}},
	})

	// Policy referencing old schedule
	policyID := "pol-deleted"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Deleted Schedule Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})

	// === DELETE old schedule, create new one ===
	s.DeleteSchedule("sched-old")
	s.CreateSchedule(&model.Schedule{
		ID: "sched-new", TeamID: "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	s.SetScheduleGroups("sched-new", [][]string{{userNew.ID}})
	s.CreateRotationEpoch(&model.RotationEpoch{
		ScheduleID: "sched-new", Layer: "l1",
		StartTime: time.Now().Add(-2 * time.Hour),
		Groups:    [][]string{{userNew.ID}},
	})

	ag := &model.AlertGroup{
		ID: "ag-deleted", DedupKey: "dk-deleted", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, nil)
	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}

	var data model.EscalationStepData
	json.Unmarshal(steps[0].Data, &data)
	if data.TargetID != userNew.ID {
		t.Errorf("REGRESSION: Expected target '%s' (Denis), got '%s' (empty=nobody notified)", userNew.ID, data.TargetID)
	}
}

// TestEscalationJobBuilder_ScheduleEmpty validates graceful handling when
// no users are on-call for a schedule.
func TestEscalationJobBuilder_ScheduleEmpty(t *testing.T) {
	s := store.NewMockStore()

	schedID := "sched-empty"
	s.CreateSchedule(&model.Schedule{
		ID: schedID, TeamID: "team-1",
		L1RotationStart: time.Now().Add(-24 * time.Hour),
	})
	// No users, no epochs

	policyID := "pol-empty"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Empty Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
		},
	})

	ag := &model.AlertGroup{ID: "ag-empty", DedupKey: "dk-empty", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, nil)
	_, stages, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	// Should create 1 stage with 1 marker step (ContinueOnFailure=true)
	if len(stages) != 1 {
		t.Fatalf("Expected 1 stage, got %d", len(stages))
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}
	if !steps[0].ContinueOnFailure {
		t.Error("Empty schedule step should have ContinueOnFailure=true")
	}

	var data model.EscalationStepData
	json.Unmarshal(steps[0].Data, &data)
	if data.TargetID != "" {
		t.Errorf("Empty schedule step should have empty TargetID, got %s", data.TargetID)
	}
}
