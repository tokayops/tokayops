package dispatcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

// TestOTPFlow / TestOTPFlow_NoRetry removed in Sprint 4 (Epic 7): the
// job-based OTP path (OTPExecutor / OTPJobBuilder / OTPStepData / step type
// "slack_dm_otp") was dead code — the production OTP flow at
// POST /me/slack/request-code sends the Slack DM synchronously inside the API
// handler, and Sprint 3's link-token service replaced the OTP indirection
// entirely. Removing the executor here removes the only producer of these
// tests' input.

func TestResolutionFlow(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)

	resolveCalled := false
	mp := &MockProvider{
		ResolveFunc: func(ctx context.Context, ag *model.AlertGroup) error {
			resolveCalled = true
			if ag.ID != "ag_res" {
				t.Errorf("Expected resolve for ag_res, got %s", ag.ID)
			}
			return nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Seed Resolved AG
	ag := &model.AlertGroup{
		ID:       "ag_res",
		Status:   model.AlertGroupStatusResolved, // Ready for processing
		PolicyID: "p1",
		DedupKey: "dk_res",
		PolicySnapshot: &model.EscalationPolicySnapshot{
			Name: "p1",
			Steps: []*model.EscalationStepSnapshot{
				{Provider: "slack", TargetKind: "channel", TargetID: "C1"},
			},
		},
	}
	s.CreateAlertGroup(ag)
	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		AlertGroupID:    ag.ID,
		Provider:        "slack",
		Kind:            "channel",
		ProviderPayload: "{\"channel_id\":\"C1\",\"timestamp\":\"123.456\"}",
		SupportsUpdate:  true,
		IsPrimary:       true,
		CreatedAt:       time.Now(),
	})

	// Trigger Resolution Processing
	d.ProcessResolvedAlertGroups(context.Background())

	// 1. Verify AG is Closed
	updatedAG, _ := s.GetAlertGroupByID("ag_res")
	if updatedAG.Status != model.AlertGroupStatusClosed {
		t.Errorf("Expected AG to be Closed, got %s", updatedAG.Status)
	}

	// 2. Verify Job Created
	job, err := s.GetJobByDedupKey("resolve_ag_res")
	if err != nil {
		t.Fatalf("Failed to find resolution job: %v", err)
	}

	if resolveCalled {
		t.Error("Resolve called internally? Should be async via Job")
	}

	// 3. Process Step
	// MockStore doesn't automatically populate NextRunAt correctly for pending steps unless we claim them,
	// but processStep takes the step object directly.
	// We need to fetch step 0.
	fetchedSteps := s.GetJobStepsByJobID(job.ID)
	step := fetchedSteps[0]
	// Simulate it being picked up (status running)
	step.Status = model.JobStepStatusRunning

	d.processStep(context.Background(), step)

	// 4. Verify Executor Called
	if !resolveCalled {
		t.Error("Executor did not call Resolve")
	}
}

func TestEscalationFlow_BuilderIntegration(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)

	s.CreateUser(&model.User{ID: "U_ESCALATION", Name: "Escalation User"})
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: "U_ESCALATION", Provider: "slack", ExternalID: "U_ESCALATION"})

	// Create policy in Store (new pattern)
	policy := &model.EscalationPolicy{
		ID:   "p1",
		Name: "Test Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "step1",
				PolicyID:       "p1",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "U_ESCALATION",
				DelaySeconds:   0,
				TimeoutSeconds: 30,
				MaxAttempts:    3,
			},
		},
	}
	s.CreateEscalationPolicy(policy)

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, message string) error {
			if userID != "U_ESCALATION" {
				t.Errorf("expected U_ESCALATION, got %s", userID)
			}
			return nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Create AG
	ag := &model.AlertGroup{ID: "ag1", TeamID: "team1", Severity: "critical", DedupKey: "dk1"}
	s.CreateAlertGroup(ag)

	// Build Job via Builder (now uses Store)
	builder := builders.NewEscalationJobBuilder(s, &fakeTeamOnCall{}, cfg)
	job, stages, steps, _, err := builder.Build(context.Background(), ag, "p1", builders.TeamOnCallRead(schedulerender.TeamOnCall{}, nil))
	if err != nil {
		t.Fatalf("Builder failed: %v", err)
	}

	// Set lease token and running status before seeding store (simulates claim)
	step := steps[0]
	leaseToken := "lease-escalation"
	step.Status = model.JobStepStatusRunning
	step.LockedBy = &leaseToken

	s.CreateJobWithDedup(job, stages, steps)

	d.processStep(context.Background(), step)

	storedStep, _ := s.GetJobStepByID(step.ID)
	if storedStep.Status != model.JobStepStatusSucceeded {
		t.Errorf("Escalation step failed: %s", storedStep.Status)
	}

	// Verify Timeline
	events, _ := s.GetTimelineEvents("ag1")
	found := false
	for _, e := range events {
		if e.Type == model.TimelineEventNotificationSent {
			found = true
			break
		}
	}
	if !found {
		t.Error("Escalation should add timeline event")
	}
}

func TestUnknownExecutor_FailsJob(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	leaseToken := "lease-unknown"
	job := &model.Job{ID: "job_u", Status: model.JobStatusRunning}
	step := &model.JobStep{
		ID:        "step_u",
		JobID:     "job_u",
		StageID:   "stage-0",
		StepIndex: 0,
		StepType:  "unknown_executor_type",
		Status:    model.JobStepStatusRunning,
		LockedBy:  &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job_u", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	// Verify Step Failed (Read from Store)
	updatedStep, err := s.GetJobStepByID(step.ID)
	if err != nil {
		t.Fatalf("Failed to read step from store: %v", err)
	}

	if updatedStep.Status != model.JobStepStatusFailed {
		t.Errorf("Expected persisted step status failed, got %s", updatedStep.Status)
	}

	// Verify Job Failed
	updatedJob, _ := s.GetJobByID(job.ID)
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected Job Failed due to unknown executor, got %s", updatedJob.Status)
	}
}

func TestJobCompletion_Succeeds(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	// Job with 2 steps in 2 stages.
	// Step 0: Done (stage 0 succeeded).
	// Step 1: Running -> Succeeds -> Job Succeeded.

	leaseToken := "lease-completion"
	job := &model.Job{ID: "job_done", Status: model.JobStatusRunning, CurrentStage: 1}
	step0 := &model.JobStep{ID: "s0", JobID: "job_done", StageID: "stage-0", StepIndex: 0, Status: model.JobStepStatusSucceeded}
	step1 := &model.JobStep{
		ID:        "s1",
		JobID:     "job_done",
		StageID:   "stage-1",
		StepIndex: 0,
		StepType:  "noop_step",
		Status:    model.JobStepStatusRunning,
		LockedBy:  &leaseToken,
	}

	// Register NOOP executor
	d.RegisterExecutor("noop_step", &NoopExecutor{})

	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job_done", StageIndex: 0, Status: model.JobStageStatusSucceeded},
		{ID: "stage-1", JobID: "job_done", StageIndex: 1, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step0, step1})

	d.processStep(context.Background(), step1)

	// Verify Step Persistence
	updatedStep, err := s.GetJobStepByID(step1.ID)
	if err != nil {
		t.Fatalf("Failed to read step from store: %v", err)
	}
	if updatedStep.Status != model.JobStepStatusSucceeded {
		t.Errorf("Expected step1 success in store, got %s", updatedStep.Status)
	}

	updatedJob, _ := s.GetJobByID("job_done")
	if updatedJob.Status != model.JobStatusSucceeded {
		t.Errorf("Expected Job Succeeded after last step, got %s", updatedJob.Status)
	}
}

type NoopExecutor struct{}

func (n *NoopExecutor) Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error) {
	return "done", nil
}

func TestJobFailure_MaxRetries(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	leaseToken := "lease-fail"
	job := &model.Job{ID: "job_fail", Status: model.JobStatusRunning}
	step := &model.JobStep{
		ID:           "s_fail",
		JobID:        "job_fail",
		StageID:      "stage-0",
		StepIndex:    0,
		StepType:     "fail_step",
		Status:       model.JobStepStatusRunning,
		MaxAttempts:  2,
		AttemptCount: 1, // Will create failure
		LockedBy:     &leaseToken,
	}

	d.RegisterExecutor("fail_step", &FailExecutor{})
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job_fail", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	storedStep, _ := s.GetJobStepByID("s_fail")
	if storedStep.Status != model.JobStepStatusFailed {
		t.Errorf("Expected step failed, got %s", storedStep.Status)
	}

	updatedJob, _ := s.GetJobByID("job_fail")
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected Job Failed, got %s", updatedJob.Status)
	}
}

type FailExecutor struct{}

func (f *FailExecutor) Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error) {
	return "", errors.New("fail intentionally")
}
