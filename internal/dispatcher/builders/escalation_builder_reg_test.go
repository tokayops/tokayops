package builders

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

// TestEscalationJobBuilder_MessagePropagation verifies that the Message field
// from EscalationStep is successfully propagated to the JobStep Data.
func TestEscalationJobBuilder_MessagePropagation(t *testing.T) {
	s := store.NewMockStore()
	builder := NewEscalationJobBuilder(s, &fakeProjection{}, nil)

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
	builder := NewEscalationJobBuilder(s, &fakeProjection{}, nil)

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
	builder := NewEscalationJobBuilder(s, &fakeProjection{}, nil)

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
	builder := NewEscalationJobBuilder(s, &fakeProjection{}, nil)
	_, _, steps, _, err := builder.Build(ag, policyID)
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
	_, _, steps, _, err := builder.Build(ag, policyID)
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

// TestEscalationJobBuilder_ProjectionFailure_MarkerStep: a projection that could
// not be read is not a reason to fail the whole escalation. The step degrades to
// the marker the executor reports, and the rest of the policy still runs.
func TestEscalationJobBuilder_ProjectionFailure_MarkerStep(t *testing.T) {
	s := store.NewMockStore()

	policyID := "pol-proj-fail"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: policyID, Name: "Projection Failure Policy",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "dm", TargetType: "schedule", TargetID: "sched-1"},
		},
	})
	ag := &model.AlertGroup{
		ID: "ag-proj-fail", DedupKey: "dk-proj-fail", TeamID: "team-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	proj := &fakeProjection{err: errors.New("could not begin transaction")}
	builder := NewEscalationJobBuilder(s, proj, nil)
	_, _, steps, _, err := builder.Build(ag, policyID)
	if err != nil {
		t.Fatalf("Build failed on an unreadable projection: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("Expected 1 marker step, got %d", len(steps))
	}
	var data model.EscalationStepData
	json.Unmarshal(steps[0].Data, &data)
	if data.TargetID != "" {
		t.Errorf("Expected an empty marker target, got %s", data.TargetID)
	}
}
