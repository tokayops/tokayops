package builders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

// TestEscalationJobBuilder_MessagePropagation verifies that the Message field
// from EscalationStep is successfully propagated to the JobStep Data.
func TestEscalationJobBuilder_MessagePropagation(t *testing.T) {
	s := store.NewMockStore()
	proj := &fakeProjection{}
	builder := NewEscalationJobBuilder(s, proj, nil)

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
	job, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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
	proj := &fakeProjection{}
	builder := NewEscalationJobBuilder(s, proj, nil)

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

	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": teamSchedule(schedID, onDuty("g-a", user1.ID)),
		},
	}

	policyID := "pol-tg-fanout"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID:   policyID,
		Name: "Telegram Fan-out",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "telegram", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
			{ID: "s2", Provider: "telegram", TargetKind: "channel", TargetType: "channel", TargetID: "-100123"},
		},
	})

	ag := &model.AlertGroup{ID: "ag-tg-fanout", DedupKey: "dk-tg-fanout", TeamID: "team-1", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	// Empty cfg → no firehose stage prepended.
	builder := NewEscalationJobBuilder(s, proj, &config.Config{})
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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
	proj := &fakeProjection{}
	builder := NewEscalationJobBuilder(s, proj, nil)

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
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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

	// Schedule with L1=user1 and L2=user2. L2 is projected but must not be
	// escalated to: it is not an escalation target today.
	schedID := "sched-fanout"
	l1AndL2 := onDuty("g-a", user1.ID)
	l1AndL2.L2 = &schedulerender.LayerOnCall{
		GroupID: user2.ID, UserIDs: []string{user2.ID},
		Source:          schedulerender.SourceRotation,
		GridSlotStart:   projectionBase,
		GridSlotEnd:     projectionBase.Add(24 * time.Hour),
		AssignmentStart: projectionBase,
		AssignmentEnd:   projectionBase.Add(24 * time.Hour),
	}
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": teamSchedule(schedID, l1AndL2),
		},
	}

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

	ag := &model.AlertGroup{ID: "ag-fanout", DedupKey: "dk-fanout", TeamID: "team-1", Severity: "warning", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, proj, cfg)
	_, stages, steps, snapshot, err := buildFor(t, builder, proj, ag, policyID)
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
	// The override is already overlaid onto L1 by the projection - there is no
	// second, older answer for the builder to prefer.
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": teamSchedule(schedID, onDutyByOverride("ov-1", user2.ID)),
		},
	}

	policyID := "pol-ov"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Override Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
		},
	})

	ag := &model.AlertGroup{ID: "ag-ov", DedupKey: "dk-ov", TeamID: "team-1", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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

	// Policy referencing old schedule
	policyID := "pol-stale"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Stale Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})

	// === SIMULATE SCHEDULE RECREATION ===
	// The old schedule is orphaned but still readable by ID and still has
	// user-old on it; team-1 now belongs to a new schedule with user-new.
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": teamSchedule("sched-new", onDuty("g-new", userNew.ID)),
		},
		schedules: map[string]schedulerender.OnCall{
			"sched-old": onDuty("g-old", userOld.ID),
			"sched-new": onDuty("g-new", userNew.ID),
		},
	}

	// AlertGroup belongs to team-1
	ag := &model.AlertGroup{
		ID: "ag-stale", DedupKey: "dk-stale", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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

	// Policy referencing old schedule
	policyID := "pol-deleted"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Deleted Schedule Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})

	// === DELETE old schedule, create new one ===
	// The deleted schedule projects nobody; team-1's current schedule answers.
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": teamSchedule("sched-new", onDuty("g-new", userNew.ID)),
		},
		schedules: map[string]schedulerender.OnCall{
			"sched-old": nobodyOnDuty(),
			"sched-new": onDuty("g-new", userNew.ID),
		},
	}

	ag := &model.AlertGroup{
		ID: "ag-deleted", DedupKey: "dk-deleted", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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
	// The schedule exists and has nobody on duty at this instant.
	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": teamSchedule(schedID, nobodyOnDuty()),
		},
	}

	policyID := "pol-empty"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Empty Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: schedID},
		},
	})

	ag := &model.AlertGroup{ID: "ag-empty", DedupKey: "dk-empty", TeamID: "team-1", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	builder := NewEscalationJobBuilder(s, proj, nil)
	_, stages, steps, _, err := buildFor(t, builder, proj, ag, policyID)
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

// TestEscalationJobBuilder_UnknownScheduleTarget_MarkerStep: a policy step
// naming a schedule that does not exist, or belongs to another team, keeps its
// old behaviour - a marker step the executor fails - rather than failing the
// build. An alert must still reach the rest of the policy.
func TestEscalationJobBuilder_UnknownScheduleTarget_MarkerStep(t *testing.T) {
	s := store.NewMockStore()

	policyID := "pol-unknown-sched"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Unknown Schedule Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-nobody-knows"},
			{ID: "s2", Provider: "slack", TargetKind: "channel", TargetType: "channel", TargetID: "C_ALERTS"},
		},
	})

	ag := &model.AlertGroup{
		ID: "ag-unknown-sched", DedupKey: "dk-unknown-sched", TeamID: "team-without-schedule",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// Neither the team nor the named schedule is known to the projection.
	proj := &fakeProjection{}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
	if err != nil {
		t.Fatalf("Build failed instead of degrading to a marker step: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("Expected 2 steps (marker + channel), got %d", len(steps))
	}

	var marker model.EscalationStepData
	json.Unmarshal(steps[0].Data, &marker)
	if marker.TargetID != "" {
		t.Errorf("Expected an empty marker target, got %s", marker.TargetID)
	}
	if !steps[0].ContinueOnFailure {
		t.Error("The marker step must not block the rest of the policy")
	}
	if steps[1].StepType != "channel" {
		t.Errorf("Step 1 should still be the channel step, got %s", steps[1].StepType)
	}
}

// TestEscalationJobBuilder_TeamScheduleDeleted_NoFallThrough: a team WITH a
// schedule answers for itself even when that schedule is deleted. Falling
// through to the stored ID would page the group of a schedule the team no longer
// owns - which is the same defect the stale-ID test guards from the other side.
func TestEscalationJobBuilder_TeamScheduleDeleted_NoFallThrough(t *testing.T) {
	s := store.NewMockStore()
	userOld := &model.User{ID: "user-old", Name: "John"}
	s.CreateUser(userOld)

	policyID := "pol-team-deleted"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Deleted Team Schedule Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})

	ag := &model.AlertGroup{
		ID: "ag-team-deleted", DedupKey: "dk-team-deleted", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	proj := &fakeProjection{
		teams: map[string]schedulerender.TeamOnCall{
			"team-1": deletedTeamSchedule("sched-current"),
		},
		// Still readable by ID, and still holding somebody.
		schedules: map[string]schedulerender.OnCall{
			"sched-old": onDuty("g-old", userOld.ID),
		},
	}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}

	var data model.EscalationStepData
	json.Unmarshal(steps[0].Data, &data)
	if data.TargetID != "" {
		t.Errorf("Expected nobody escalated to, got %s - the stored schedule ID was used", data.TargetID)
	}
}

// TestEscalationJobBuilder_ResolutionUnavailable_CommitsNothing: a read that
// would not answer defers the whole build instead of degrading to a marker, and
// the deferral is all-or-nothing, firehose included.
//
// A marker is what "nobody is on duty" looks like, and committing one here would
// say that about a schedule nobody could read - for good, since an alert group
// with an escalation job is never picked up again. The engine leaves the group
// new and asks again next tick.
//
// The firehose stage is assembled before the policy steps, so returning what was
// built so far is the tempting shortcut - and it is the defect, not a partial
// success: the alert group would then own an escalation job, nothing would pick
// it up again, and the missing stage would be the one that pages the on-call.
// Delivering the channel a few seconds later is the cheaper half of that trade.
func TestEscalationJobBuilder_ResolutionUnavailable_CommitsNothing(t *testing.T) {
	s := store.NewMockStore()

	policyID := "pol-firehose-and-schedule"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Firehose And Schedule",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-1"},
		},
	})
	ag := &model.AlertGroup{
		ID: "ag-firehose-and-schedule", DedupKey: "dk-firehose-and-schedule", TeamID: "team-1",
		Severity: "critical", Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	cfg := &config.Config{}
	cfg.Global.FirehoseCriticalChannel = "C_FIREHOSE"

	proj := &fakeProjection{err: errors.New("could not begin transaction")}
	builder := NewEscalationJobBuilder(s, proj, cfg)
	job, stages, steps, snapshot, err := buildFor(t, builder, proj, ag, policyID)

	if !errors.Is(err, ErrOnCallResolutionUnavailable) {
		t.Fatalf("Build error = %v, want ErrOnCallResolutionUnavailable", err)
	}
	if job != nil {
		t.Errorf("job = %+v, want nothing to commit", job)
	}
	if stages != nil {
		t.Errorf("stages = %d, want nothing to commit", len(stages))
	}
	if steps != nil {
		t.Errorf("steps = %d, want nothing to commit - the firehose stage is built first and must not survive", len(steps))
	}
	if snapshot != nil {
		t.Errorf("snapshot = %+v, want nothing to commit", snapshot)
	}
}

// TestEscalationJobBuilder_DamagedSchedule_MarkerStep: stored data that cannot be
// projected keeps the old behaviour, and that is the other half of the rule.
//
// Retrying answers nothing here: the snapshot will fail to decode next tick too,
// and the tick after that, so deferring would trade one lost page for an alert
// that never reaches anyone at all. The step degrades to the marker the executor
// reports and the rest of the policy runs - the same trade BulkOnCall makes when
// it refuses to let one damaged row fail a whole projection.
func TestEscalationJobBuilder_DamagedSchedule_MarkerStep(t *testing.T) {
	s := store.NewMockStore()

	policyID := "pol-damaged-sched"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Damaged Schedule Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-1"},
			{ID: "s2", Provider: "slack", TargetKind: "channel", TargetType: "channel", TargetID: "C_ALERTS"},
		},
	})
	ag := &model.AlertGroup{
		ID: "ag-damaged-sched", DedupKey: "dk-damaged-sched", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	proj := &fakeProjection{err: fmt.Errorf("revision r7: %w", scheduleconfig.ErrSnapshotDecode)}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
	if err != nil {
		t.Fatalf("Build failed instead of degrading to a marker step: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("Expected 2 steps (marker + channel), got %d", len(steps))
	}
	var marker model.EscalationStepData
	json.Unmarshal(steps[0].Data, &marker)
	if marker.TargetID != "" {
		t.Errorf("Expected an empty marker target, got %s", marker.TargetID)
	}
	if steps[1].StepType != "channel" {
		t.Errorf("Step 1 should still be the channel step, got %s", steps[1].StepType)
	}
}

// countingProjection counts what was read and can fail one read at a time, so a
// test can put a transient failure exactly where it hurts.
type countingProjection struct {
	teams     map[string]schedulerender.TeamOnCall
	teamErr   error
	teamCalls int

	// answers is consulted by call index, so read N can fail while read N+1
	// would have succeeded.
	answers       []scheduleAnswer
	scheduleCalls int
}

type scheduleAnswer struct {
	onCall schedulerender.OnCall
	err    error
}

func (c *countingProjection) CurrentTeamOnCallNow(ctx context.Context, teamID string) (schedulerender.TeamOnCall, error) {
	c.teamCalls++
	if c.teamErr != nil {
		return schedulerender.TeamOnCall{}, c.teamErr
	}
	return c.teams[teamID], nil
}

func (c *countingProjection) CurrentOnCallNow(ctx context.Context, scheduleID string) (schedulerender.OnCall, error) {
	i := c.scheduleCalls
	c.scheduleCalls++
	if i < len(c.answers) {
		return c.answers[i].onCall, c.answers[i].err
	}
	return schedulerender.OnCall{}, nil
}

// twoScheduleStepPolicy is a policy whose two steps name the same schedule -
// one question asked twice.
func twoScheduleStepPolicy(t *testing.T, s *store.MockStore, policyID, scheduleID string) {
	t.Helper()
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Same Schedule Twice",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: scheduleID},
			{ID: "s2", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: scheduleID},
		},
	})
}

// dmTargets lists what the dm steps of a built job would page.
func dmTargets(t *testing.T, steps []*model.JobStep) []string {
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

// TestEscalationJobBuilder_SameScheduleResolvedOnce: two policy steps naming the
// same schedule are one question. Reading twice lets a handoff land between them,
// and one job would then page two different groups for the same target.
func TestEscalationJobBuilder_SameScheduleResolvedOnce(t *testing.T) {
	s := store.NewMockStore()
	outgoing := &model.User{ID: "user-outgoing", Name: "Alice"}
	incoming := &model.User{ID: "user-incoming", Name: "Bob"}
	s.CreateUser(outgoing)
	s.CreateUser(incoming)

	policyID := "pol-same-schedule"
	twoScheduleStepPolicy(t, s, policyID, "sched-1")
	// No team schedule, so both steps take the fallback-by-ID path.
	ag := &model.AlertGroup{
		ID: "ag-same-schedule", DedupKey: "dk-same-schedule", TeamID: "team-without-schedule",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// The shift changes hands the instant after the first read.
	proj := &countingProjection{answers: []scheduleAnswer{
		{onCall: onDuty("g-outgoing", outgoing.ID)},
		{onCall: onDuty("g-incoming", incoming.ID)},
	}}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if proj.scheduleCalls != 1 {
		t.Errorf("schedule read %d times for one job, want 1", proj.scheduleCalls)
	}
	targets := dmTargets(t, steps)
	if len(targets) != 2 {
		t.Fatalf("expected 2 dm steps, got %d", len(targets))
	}
	if targets[0] != targets[1] {
		t.Errorf("one job pages %q and %q for the same schedule", targets[0], targets[1])
	}
}

// TestEscalationJobBuilder_UnreadableTeam_NoFallback: a failed team read is not a
// team without a schedule.
//
// If it were, the builder would go looking for the schedule ID stored on the
// policy step - a second transaction, which may well succeed, and which may name
// a schedule this team no longer owns. The alert would then page last quarter's
// rotation while the alert group, which knows the read failed, records nothing.
//
// The fallback staying untouched is the point here; that the build is deferred
// rather than degraded is the neighbouring rule, asserted so the two cannot drift.
func TestEscalationJobBuilder_UnreadableTeam_NoFallback(t *testing.T) {
	s := store.NewMockStore()
	stale := &model.User{ID: "user-stale", Name: "John"}
	s.CreateUser(stale)

	policyID := "pol-unreadable-team"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Unreadable Team Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-old"},
		},
	})
	ag := &model.AlertGroup{
		ID: "ag-unreadable-team", DedupKey: "dk-unreadable-team", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// The team read fails; the stale schedule would answer perfectly well.
	proj := &countingProjection{
		teamErr: errors.New("could not begin transaction"),
		answers: []scheduleAnswer{{onCall: onDuty("g-old", stale.ID)}},
	}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, _, _, err := buildFor(t, builder, proj, ag, policyID)
	if !errors.Is(err, ErrOnCallResolutionUnavailable) {
		t.Fatalf("Build error = %v, want ErrOnCallResolutionUnavailable", err)
	}

	if proj.scheduleCalls != 0 {
		t.Errorf("the fallback schedule was read %d times after a failed team read, want 0", proj.scheduleCalls)
	}
}

// TestEscalationJobBuilder_DamagedScheduleReadIsRemembered: a read that failed is
// an answer to remember too.
//
// Cache only the successes and the second step re-reads: one build then produces
// a marker for the first step and a real recipient for the second, which is
// precisely the "one schedule, two answers" this memo exists to prevent.
//
// The case is damaged data on purpose. Any other failure defers the whole build
// at the first step, and a memo proves nothing about a loop that stops - here
// the build carries on, so the second step really can ask again.
func TestEscalationJobBuilder_DamagedScheduleReadIsRemembered(t *testing.T) {
	s := store.NewMockStore()
	user := &model.User{ID: "user-1", Name: "Alice"}
	s.CreateUser(user)

	policyID := "pol-damaged-schedule"
	twoScheduleStepPolicy(t, s, policyID, "sched-1")
	ag := &model.AlertGroup{
		ID: "ag-damaged-schedule", DedupKey: "dk-damaged-schedule", TeamID: "team-without-schedule",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// The first read finds damage; the second would have answered.
	proj := &countingProjection{answers: []scheduleAnswer{
		{err: fmt.Errorf("revision r7: %w", scheduleconfig.ErrSnapshotDecode)},
		{onCall: onDuty("g-a", user.ID)},
	}}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
	if err != nil {
		t.Fatalf("Build failed instead of degrading to marker steps: %v", err)
	}

	if proj.scheduleCalls != 1 {
		t.Errorf("schedule read %d times for one job, want 1", proj.scheduleCalls)
	}
	targets := dmTargets(t, steps)
	if len(targets) != 2 {
		t.Fatalf("expected 2 dm steps, got %d: %v", len(targets), targets)
	}
	if targets[0] != targets[1] {
		t.Errorf("one job pages %q and %q for the same damaged schedule", targets[0], targets[1])
	}
	if targets[0] != "" {
		t.Errorf("job pages %q from a schedule that could not be projected, want a marker", targets[0])
	}
}

// flakyUserStore fails the first N GetUsersByIDs calls, so a test can put a
// transient failure between two policy steps that ask the same question.
type flakyUserStore struct {
	store.StoreInterface
	failures int
	calls    int
}

func (f *flakyUserStore) GetUsersByIDs(ids []string) ([]*model.User, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("transient db error")
	}
	return f.StoreInterface.GetUsersByIDs(ids)
}

// TestEscalationJobBuilder_TeamOnCallHydratedOnce: the team's answer is turned
// into people once per job, and a hydration that failed defers the build.
//
// Hydration is a database read of its own, so it fails the way reads fail, and
// its failure means the same thing as a projection that would not answer: the
// recipients are unknown, not absent. Repeating the read per step would have one
// job disagree with itself about who is on call; committing it either way would
// page nobody for good.
func TestEscalationJobBuilder_TeamOnCallHydratedOnce(t *testing.T) {
	mock := store.NewMockStore()
	user := &model.User{ID: "user-1", Name: "Alice"}
	mock.CreateUser(user)

	policyID := "pol-team-hydration"
	twoScheduleStepPolicy(t, mock, policyID, "sched-1")
	ag := &model.AlertGroup{
		ID: "ag-team-hydration", DedupKey: "dk-team-hydration", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	mock.CreateAlertGroup(ag)

	// The team's schedule answers, so both steps take the team path. The first
	// hydration fails; the second would have succeeded.
	s := &flakyUserStore{StoreInterface: mock, failures: 1}
	proj := &countingProjection{teams: map[string]schedulerender.TeamOnCall{
		"team-1": teamSchedule("sched-current", onDuty("g-a", user.ID)),
	}}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, _, _, err := buildFor(t, builder, proj, ag, policyID)
	if !errors.Is(err, ErrOnCallResolutionUnavailable) {
		t.Fatalf("Build error = %v, want ErrOnCallResolutionUnavailable", err)
	}

	if s.calls != 1 {
		t.Errorf("on-call hydrated %d times for one job, want 1", s.calls)
	}
	if proj.scheduleCalls != 0 {
		t.Errorf("the fallback schedule was read %d times although the team has one, want 0", proj.scheduleCalls)
	}
}

// TestEscalationJobBuilder_TeamOnCallHydrationIsShared: the successful case of
// the same rule - two steps, one hydration, the same recipients.
func TestEscalationJobBuilder_TeamOnCallHydrationIsShared(t *testing.T) {
	mock := store.NewMockStore()
	user := &model.User{ID: "user-1", Name: "Alice"}
	mock.CreateUser(user)

	policyID := "pol-team-hydration-ok"
	twoScheduleStepPolicy(t, mock, policyID, "sched-1")
	ag := &model.AlertGroup{
		ID: "ag-team-hydration-ok", DedupKey: "dk-team-hydration-ok", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	mock.CreateAlertGroup(ag)

	s := &flakyUserStore{StoreInterface: mock}
	proj := &countingProjection{teams: map[string]schedulerender.TeamOnCall{
		"team-1": teamSchedule("sched-current", onDuty("g-a", user.ID)),
	}}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := buildFor(t, builder, proj, ag, policyID)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if s.calls != 1 {
		t.Errorf("on-call hydrated %d times for one job, want 1", s.calls)
	}
	targets := dmTargets(t, steps)
	if len(targets) != 2 || targets[0] != user.ID || targets[1] != user.ID {
		t.Errorf("job pages %v, want both steps aimed at %s", targets, user.ID)
	}
}
