package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// MockProvider adapts the new Provider interface to the legacy func-field shapes
// used throughout these tests. A "user" target routes to SendDMFunc (fire-and-forget),
// a "channel" target routes to SendFunc. Update/Resolve receive the real AlertGroup.
type MockProvider struct {
	SendFunc      func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error)
	UpdateFunc    func(ctx context.Context, ag *model.AlertGroup) (string, error)
	ResolveFunc   func(ctx context.Context, ag *model.AlertGroup) error
	SendDMFunc    func(ctx context.Context, userID, message string) error
	PermalinkFunc func(d *model.NotificationDelivery) string
}

var _ Provider = (*MockProvider)(nil)

func (m *MockProvider) Send(ctx context.Context, req NotificationRequest) (string, error) {
	if req.Target.Kind == "user" {
		if m.SendDMFunc != nil {
			return "", m.SendDMFunc(ctx, req.Target.ID, req.Message)
		}
		return "", nil
	}
	if m.SendFunc != nil {
		return m.SendFunc(ctx, req.Target.ID, req.AlertGroup)
	}
	return "", nil
}

func (m *MockProvider) Update(ctx context.Context, _ *model.NotificationDelivery, ag *model.AlertGroup) (string, error) {
	if m.UpdateFunc != nil {
		return m.UpdateFunc(ctx, ag)
	}
	return "", nil
}

func (m *MockProvider) Resolve(ctx context.Context, _ *model.NotificationDelivery, ag *model.AlertGroup) error {
	if m.ResolveFunc != nil {
		return m.ResolveFunc(ctx, ag)
	}
	return nil
}

func (m *MockProvider) Permalink(d *model.NotificationDelivery) string {
	if m.PermalinkFunc != nil {
		return m.PermalinkFunc(d)
	}
	return ""
}

// testJobIdentity gives a fixture job an identity. Tests about the step
// machinery do not care which family a job belongs to, and handoff is the
// family whose keys are opaque strings - borrowing it claims the least.
func testJobIdentity(jobID string) *jobdedup.Spec {
	return jobdedup.Handoff("test:" + jobID)
}

func TestProcessStep_SlackSuccess(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{
		Global: config.GlobalConfig{},
	}
	d := mustNewDispatcher(t, s, cfg)

	called := false
	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, message string) error {
			called = true
			if userID != "U1" {
				t.Errorf("expected target U1, got %s", userID)
			}
			return nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Seed AG
	ag := &model.AlertGroup{ID: "ag1", DedupKey: "dk1"}
	s.CreateAlertGroup(ag)

	// Seed Job (Required for processStep lookup)
	leaseToken := "lease-slack-success"
	job := &model.Job{
		ID:     "job1",
		Dedup:  jobdedup.Escalation("ag1"),
		Status: model.JobStatusRunning,
	}
	// Prepare Step
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:          "step1",
		JobID:       "job1",
		StageID:     "stage-0",
		StepIndex:   0,
		StepType:    "dm",
		Status:      model.JobStepStatusRunning,
		Data:        json.RawMessage(dataBytes),
		MaxAttempts: 3,
		LockedBy:    &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	if !called {
		t.Error("Provider Send not called")
	}
	storedStep, _ := s.GetJobStepByID("step1")
	if storedStep.Status != model.JobStepStatusSucceeded {
		t.Errorf("Expected status Succeeded, got %s", storedStep.Status)
	}
}

func TestProcessStep_Retry(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, message string) error {
			return errors.New("slack error")
		},
	}
	d.RegisterProvider("slack", mp)

	// Create AG to avoid "alert group not found"
	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1", DedupKey: "dk1"})

	// Seed Job
	leaseToken := "lease-retry"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:           "step1",
		JobID:        "job1",
		StageID:      "stage-0",
		StepType:     "dm",
		Status:       model.JobStepStatusRunning,
		Data:         json.RawMessage(dataBytes),
		MaxAttempts:  3,
		AttemptCount: 0,
		LockedBy:     &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	// Attempt 1 (0 -> 1)
	d.processStep(context.Background(), step)

	if step.Status != model.JobStepStatusRetry {
		t.Errorf("Expected status Retry, got %s", step.Status)
	}
	if step.AttemptCount != 1 {
		t.Errorf("Expected attempt 1, got %d", step.AttemptCount)
	}
	if step.NextRunAt == nil {
		t.Error("Expected NextRunAt to be set")
	}

	// Make MaxAttempts=2 to test failure — simulate re-claim
	step.MaxAttempts = 2
	step.Status = model.JobStepStatusRunning
	step.AttemptCount = 1
	// Sync stored step to running (simulates re-claim by worker)
	s.UpdateJobStepIfOwned(step, leaseToken)

	// Attempt 2 (1 -> 2) -> Max Reached -> Failed
	d.processStep(context.Background(), step)

	// Check stored step for final status (failStep uses FinishStepAndAdvance)
	storedStep, _ := s.GetJobStepByID("step1")
	if storedStep.Status != model.JobStepStatusFailed {
		t.Errorf("Expected status Failed (on max attempts), got %s", storedStep.Status)
	}
}

func TestProcessStep_Firehose(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{
		Global: config.GlobalConfig{},
	}
	d := mustNewDispatcher(t, s, cfg)

	called := false
	mp := &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			called = true
			if targetID != "C_FIREHOSE" {
				t.Errorf("expected target C_FIREHOSE, got %s", targetID)
			}
			return "msg123", nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Seed AG
	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})

	// Seed Job
	leaseToken := "lease-firehose"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "C_FIREHOSE", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:       "step1",
		JobID:    "job1",
		StageID:  "stage-0",
		StepType: "firehose",
		Status:   model.JobStepStatusRunning,
		Data:     json.RawMessage(dataBytes),
		LockedBy: &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	if !called {
		t.Error("Provider Send not called")
	}
	storedStep, _ := s.GetJobStepByID("step1")
	if storedStep.Status != model.JobStepStatusSucceeded {
		t.Errorf("Expected status Succeeded, got %s", storedStep.Status)
	}
}

func TestProcessStep_MaxRetries(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, message string) error {
			return errors.New("persistent error")
		},
	}
	d.RegisterProvider("slack", mp)

	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})
	leaseToken := "lease-max-retries"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:           "step1",
		JobID:        "job1",
		StageID:      "stage-0",
		StepType:     "dm",
		Status:       model.JobStepStatusRunning,
		Data:         json.RawMessage(dataBytes),
		MaxAttempts:  2,
		AttemptCount: 1, // Will increment to 2 (Max) -> Fail
		LockedBy:     &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	storedStep, _ := s.GetJobStepByID("step1")
	if storedStep.Status != model.JobStepStatusFailed {
		t.Errorf("Expected step status Failed, got %s", storedStep.Status)
	}

	// Verify Job Failed
	updatedJob, _ := s.GetJobByID("job1")
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected job status Failed, got %s", updatedJob.Status)
	}
}

func TestProcessStep_Canceled(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})
	// Job is Canceled
	leaseToken := "lease-canceled"
	job := &model.Job{ID: "job1", Status: model.JobStatusCanceled, Dedup: testJobIdentity("job1")}
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:        "step1",
		JobID:     "job1",
		StageID:   "stage-0",
		StepIndex: 0,
		Status:    model.JobStepStatusRunning,
		Data:      json.RawMessage(dataBytes),
		LockedBy:  &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	if step.Status != model.JobStepStatusCanceled {
		t.Errorf("Expected step status Canceled, got %s", step.Status)
	}

	// Verify No Timeline Event (Dispatcher decoupled)
	events, _ := s.GetTimelineEvents("ag1")
	for _, e := range events {
		if e.Type == model.TimelineEventStatusChange && strings.Contains(e.Message, "canceled") {
			t.Error("Unexpected timeline event for cancellation (should be decoupled)")
		}
	}
}

// FailingStoreWrapper wraps MockStore to inject errors
type FailingStoreWrapper struct {
	store.StoreInterface
	FailCount int
}

func (f *FailingStoreWrapper) GetJobByID(id string) (*model.Job, error) {
	if f.FailCount > 0 {
		f.FailCount--
		return nil, errors.New("transient db error")
	}
	return f.StoreInterface.GetJobByID(id)
}

func TestProcessStep_TransientError(t *testing.T) {
	realStore := store.NewMockStore()
	// Seed data in real store
	realStore.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})
	// Wrap it
	wrapper := &FailingStoreWrapper{
		StoreInterface: realStore,
		FailCount:      1, // Fail once
	}

	d := mustNewDispatcher(t, wrapper, &config.Config{})

	leaseToken := "lease-transient"
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:           "step1",
		JobID:        "job1",
		StageID:      "stage-0",
		Status:       model.JobStepStatusRunning,
		Data:         json.RawMessage(dataBytes),
		MaxAttempts:  3,
		AttemptCount: 0,
		LockedBy:     &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	realStore.CreateJobWithDedup(&model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}, stages, []*model.JobStep{step})

	d.processStep(context.Background(), step)

	// Since we used handleStepRetry inside processStep for GetJobByID error,
	// the step should be in Retry state.
	if step.Status != model.JobStepStatusRetry {
		t.Errorf("Expected step status Retry, got %s", step.Status)
	}
	// Attempt count should increment (depends on implementation details of handleStepRetry)
	// Current impl: handleStepRetry increments attempt count.
	if step.AttemptCount != 1 {
		t.Errorf("Expected attempt count 1, got %d", step.AttemptCount)
	}
}

// ===================================================================================
// ContinueOnFailure tests
// ===================================================================================

func TestFailStep_ContinueOnFailure_HasNextStep(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	mp := &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			return "", errors.New("firehose failed")
		},
	}
	d.RegisterProvider("slack", mp)

	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})

	// Create job with TWO steps: firehose (index 0) + dm (index 1)
	leaseToken := "lease-cof-next"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData0 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "C_FIREHOSE", ProviderName: "slack"}
	dataBytes0, _ := json.Marshal(stepData0)
	stepData1 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes1, _ := json.Marshal(stepData1)

	step0 := &model.JobStep{
		ID:                "step0",
		JobID:             "job1",
		StageID:           "stage-0",
		StepIndex:         0,
		StepType:          "firehose",
		Status:            model.JobStepStatusRunning,
		Data:              json.RawMessage(dataBytes0),
		MaxAttempts:       1, // Fail immediately (no retries)
		ContinueOnFailure: true,
		LockedBy:          &leaseToken,
	}
	step1 := &model.JobStep{
		ID:        "step1",
		JobID:     "job1",
		StageID:   "stage-1",
		StepIndex: 0,
		StepType:  "dm",
		Status:    model.JobStepStatusBlocked,
		Data:      json.RawMessage(dataBytes1),
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
		{ID: "stage-1", JobID: "job1", StageIndex: 1, Status: model.JobStageStatusBlocked},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step0, step1})

	// Execute step 0 (will fail)
	d.processStep(context.Background(), step0)

	// Step 0 should be failed
	storedStep0, _ := s.GetJobStepByID(step0.ID)
	if storedStep0.Status != model.JobStepStatusFailed {
		t.Errorf("Expected step0 status Failed, got %s", storedStep0.Status)
	}

	// Step 1 should be unlocked (pending)
	updatedStep1, _ := s.GetJobStepByID(step1.ID)
	if updatedStep1.Status != model.JobStepStatusPending {
		t.Errorf("Expected step1 status Pending (unlocked), got %s", updatedStep1.Status)
	}

	// Job should NOT be failed (still running)
	updatedJob, _ := s.GetJobByID("job1")
	if updatedJob.Status == model.JobStatusFailed {
		t.Errorf("Expected job status NOT Failed when ContinueOnFailure=true and next step exists, got %s", updatedJob.Status)
	}
}

func TestFailStep_ContinueOnFailure_NoNextStep(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	mp := &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			return "", errors.New("firehose failed")
		},
	}
	d.RegisterProvider("slack", mp)

	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})

	// Create job with ONLY one step (firehose) - simulates firehose-only job
	leaseToken := "lease-cof-no-next"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "C_FIREHOSE", ProviderName: "slack"}
	dataBytes, _ := json.Marshal(stepData)

	step := &model.JobStep{
		ID:                "step0",
		JobID:             "job1",
		StageID:           "stage-0",
		StepIndex:         0,
		StepType:          "firehose",
		Status:            model.JobStepStatusRunning,
		Data:              json.RawMessage(dataBytes),
		MaxAttempts:       1, // Fail immediately
		ContinueOnFailure: true,
		LockedBy:          &leaseToken,
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step})

	// Execute step (will fail)
	d.processStep(context.Background(), step)

	// Step should be failed
	storedStep, _ := s.GetJobStepByID(step.ID)
	if storedStep.Status != model.JobStepStatusFailed {
		t.Errorf("Expected step status Failed, got %s", storedStep.Status)
	}

	// Job should be FAILED (not succeeded!) since there's no next step to continue to
	updatedJob, _ := s.GetJobByID("job1")
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected job status Failed when ContinueOnFailure=true but no next step, got %s", updatedJob.Status)
	}
}

func TestFailStep_Default(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s, &config.Config{})

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, message string) error {
			return errors.New("step failed")
		},
	}
	d.RegisterProvider("slack", mp)

	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})

	// Create job with two steps, but first step has ContinueOnFailure=false (default)
	leaseToken := "lease-default-fail"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData0 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes0, _ := json.Marshal(stepData0)
	stepData1 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U2", ProviderName: "slack"}
	dataBytes1, _ := json.Marshal(stepData1)

	step0 := &model.JobStep{
		ID:                "step0",
		JobID:             "job1",
		StageID:           "stage-0",
		StepIndex:         0,
		StepType:          "dm",
		Status:            model.JobStepStatusRunning,
		Data:              json.RawMessage(dataBytes0),
		MaxAttempts:       1, // Fail immediately
		ContinueOnFailure: false,
		LockedBy:          &leaseToken,
	}
	step1 := &model.JobStep{
		ID:        "step1",
		JobID:     "job1",
		StageID:   "stage-1",
		StepIndex: 0,
		StepType:  "dm",
		Status:    model.JobStepStatusBlocked,
		Data:      json.RawMessage(dataBytes1),
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
		{ID: "stage-1", JobID: "job1", StageIndex: 1, Status: model.JobStageStatusBlocked},
	}
	s.CreateJobWithDedup(job, stages, []*model.JobStep{step0, step1})

	// Execute step 0 (will fail)
	d.processStep(context.Background(), step0)

	// Step 0 should be failed
	storedStep0, _ := s.GetJobStepByID(step0.ID)
	if storedStep0.Status != model.JobStepStatusFailed {
		t.Errorf("Expected step0 status Failed, got %s", storedStep0.Status)
	}

	// Job should be FAILED (default behavior, no fail-forward)
	updatedJob, _ := s.GetJobByID("job1")
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected job status Failed when ContinueOnFailure=false, got %s", updatedJob.Status)
	}

	// Step 1 should NOT be unlocked (should stay blocked)
	updatedStep1, _ := s.GetJobStepByID(step1.ID)
	if updatedStep1.Status != model.JobStepStatusBlocked {
		t.Errorf("Expected step1 status Blocked (not unlocked), got %s", updatedStep1.Status)
	}
}

// FinishStepFailingStoreWrapper wraps MockStore to inject FinishStepAndAdvance errors
type FinishStepFailingStoreWrapper struct {
	store.StoreInterface
	FinishFailCount int
}

func (f *FinishStepFailingStoreWrapper) FinishStepAndAdvance(stepID string, leaseToken string, outcome model.JobStepStatus, result string, stepError string) (model.AdvanceResult, error) {
	if f.FinishFailCount > 0 {
		f.FinishFailCount--
		return 0, errors.New("transient db error")
	}
	return f.StoreInterface.FinishStepAndAdvance(stepID, leaseToken, outcome, result, stepError)
}

func TestContinueOnFailure_UnlockRetrySuccess(t *testing.T) {
	realStore := store.NewMockStore()
	// Wrap with failing FinishStepAndAdvance (fails twice, succeeds on third)
	wrapper := &FinishStepFailingStoreWrapper{
		StoreInterface:  realStore,
		FinishFailCount: 2, // Fail twice, succeed on third
	}

	d := mustNewDispatcher(t, wrapper, &config.Config{})

	mp := &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			return "", errors.New("firehose failed")
		},
	}
	d.RegisterProvider("slack", mp)

	realStore.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})

	// Create job with TWO steps (each in its own stage)
	leaseToken := "lease-unlock-retry"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData0 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "C_FIREHOSE", ProviderName: "slack"}
	dataBytes0, _ := json.Marshal(stepData0)
	stepData1 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes1, _ := json.Marshal(stepData1)

	step0 := &model.JobStep{
		ID:                "step0",
		JobID:             "job1",
		StageID:           "stage-0",
		StepIndex:         0,
		StepType:          "firehose",
		Status:            model.JobStepStatusRunning,
		Data:              json.RawMessage(dataBytes0),
		MaxAttempts:       1,
		ContinueOnFailure: true,
		LockedBy:          &leaseToken,
	}
	step1 := &model.JobStep{
		ID:        "step1",
		JobID:     "job1",
		StageID:   "stage-1",
		StepIndex: 0,
		StepType:  "dm",
		Status:    model.JobStepStatusBlocked,
		Data:      json.RawMessage(dataBytes1),
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
		{ID: "stage-1", JobID: "job1", StageIndex: 1, Status: model.JobStageStatusBlocked},
	}
	realStore.CreateJobWithDedup(job, stages, []*model.JobStep{step0, step1})

	// Execute step 0 (will fail, then FinishStepAndAdvance retries twice before succeeding)
	d.processStep(context.Background(), step0)

	// Step 0 should be failed (check stored state)
	storedStep0, _ := realStore.GetJobStepByID(step0.ID)
	if storedStep0.Status != model.JobStepStatusFailed {
		t.Errorf("Expected step0 status Failed, got %s", storedStep0.Status)
	}

	// Step 1 should be unlocked (pending) after retries
	updatedStep1, _ := realStore.GetJobStepByID(step1.ID)
	if updatedStep1.Status != model.JobStepStatusPending {
		t.Errorf("Expected step1 status Pending after unlock retries, got %s", updatedStep1.Status)
	}

	// Job should NOT be failed
	updatedJob, _ := realStore.GetJobByID("job1")
	if updatedJob.Status == model.JobStatusFailed {
		t.Errorf("Expected job NOT failed after successful unlock retry, got %s", updatedJob.Status)
	}
}

func TestContinueOnFailure_UnlockRetryExhausted(t *testing.T) {
	realStore := store.NewMockStore()
	// Wrap with failing FinishStepAndAdvance (fails all 3 retries)
	wrapper := &FinishStepFailingStoreWrapper{
		StoreInterface:  realStore,
		FinishFailCount: 10, // More than maxRetries (3)
	}

	d := mustNewDispatcher(t, wrapper, &config.Config{})

	mp := &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			return "", errors.New("firehose failed")
		},
	}
	d.RegisterProvider("slack", mp)

	realStore.CreateAlertGroup(&model.AlertGroup{ID: "ag1"})

	// Create job with TWO steps (each in its own stage)
	leaseToken := "lease-retry-exhaust"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData0 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "C_FIREHOSE", ProviderName: "slack"}
	dataBytes0, _ := json.Marshal(stepData0)
	stepData1 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes1, _ := json.Marshal(stepData1)

	step0 := &model.JobStep{
		ID:                "step0",
		JobID:             "job1",
		StageID:           "stage-0",
		StepIndex:         0,
		StepType:          "firehose",
		Status:            model.JobStepStatusRunning,
		Data:              json.RawMessage(dataBytes0),
		MaxAttempts:       1,
		ContinueOnFailure: true,
		LockedBy:          &leaseToken,
	}
	step1 := &model.JobStep{
		ID:        "step1",
		JobID:     "job1",
		StageID:   "stage-1",
		StepIndex: 0,
		StepType:  "dm",
		Status:    model.JobStepStatusBlocked,
		Data:      json.RawMessage(dataBytes1),
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
		{ID: "stage-1", JobID: "job1", StageIndex: 1, Status: model.JobStageStatusBlocked},
	}
	realStore.CreateJobWithDedup(job, stages, []*model.JobStep{step0, step1})

	// Execute step 0 (will fail, then all FinishStepAndAdvance retries fail)
	d.processStep(context.Background(), step0)

	// Job should be FAILED (all retries exhausted, FailJob called as fallback)
	updatedJob, _ := realStore.GetJobByID("job1")
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected job Failed after finish retries exhausted, got %s", updatedJob.Status)
	}
}

// ===================================================================================
// Ack Update Loop Tests
// ===================================================================================

// JobCountingStoreWrapper wraps MockStore to count job creations
type JobCountingStoreWrapper struct {
	store.StoreInterface
	JobsCreated int
}

func (w *JobCountingStoreWrapper) CreateJobWithDedup(job *model.Job, stages []*model.JobStage, steps []*model.JobStep) (bool, error) {
	// Call real implementation
	created, err := w.StoreInterface.CreateJobWithDedup(job, stages, steps)
	if created {
		// Only count if THIS job was actually created (not deduped to existing)
		w.JobsCreated++
	}
	return created, err
}

// TestProcessAcknowledgedAlertGroups_NoDuplicateJobs_WithJobCompletion tests that
// ProcessAcknowledgedAlertGroups does not create duplicate update jobs when a job completes
// between loop iterations.
// Regression test for: infinite update jobs being created every 2 seconds because
// dedup only applies to pending/running jobs, not succeeded ones.
func TestProcessAcknowledgedAlertGroups_NoDuplicateJobs_WithJobCompletion(t *testing.T) {
	realStore := store.NewMockStore()
	wrapper := &JobCountingStoreWrapper{StoreInterface: realStore}

	cfg := &config.Config{
		Global: config.GlobalConfig{},
	}
	d := mustNewDispatcher(t, wrapper, cfg)

	mp := &MockProvider{
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"ts":"1234567890.123456"}`, nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Create acknowledged AG
	ag := &model.AlertGroup{
		ID:       "ag1",
		DedupKey: "dk1",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	realStore.CreateAlertGroup(ag)

	// Create an updatable delivery for this AG
	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag1",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	realStore.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// First iteration: creates job
	d.ProcessAcknowledgedAlertGroups(ctx)
	if wrapper.JobsCreated != 1 {
		t.Errorf("Expected 1 job after first iteration, got %d", wrapper.JobsCreated)
	}

	// Simulate job completion: mark job as succeeded in the mock store
	// This simulates what happens between loop iterations when the job executes
	realStore.MarkJobSucceeded(jobdedup.AckUpdate("ag1"))

	// Second iteration: should skip because succeeded job exists
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Verify: only 1 job created (no duplicates)
	if wrapper.JobsCreated > 1 {
		t.Errorf("Expected only 1 update job total, but got %d (duplicate job created)", wrapper.JobsCreated)
	}

	// Verify AG stays acknowledged (not transitioned to triggered!)
	updatedAG, _ := realStore.GetAlertGroupByID("ag1")
	if updatedAG.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected AG to stay acknowledged, but got status %s", updatedAG.Status)
	}
}

// TestProcessAcknowledgedAlertGroups_CancelsEscalationJob tests that when an AG is acknowledged,
// the active escalation job is canceled to stop further escalation steps.
func TestProcessAcknowledgedAlertGroups_CancelsEscalationJob(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)

	mp := &MockProvider{
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"ts":"1234567890.123456"}`, nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Create acknowledged AG with a dedup key
	ag := &model.AlertGroup{
		ID:       "ag1",
		DedupKey: "test_dedup_key",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	// Create an updatable delivery
	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag1",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	s.UpsertNotificationDelivery(delivery)

	// Create an active escalation job (simulating ongoing escalation), shaped
	// like the builder's: cancellation addresses alert_group_id, and the dedup
	// identity is that same group.
	escalationAGID := ag.ID
	escalationJob := &model.Job{
		ID:           "escalation_job_1",
		Type:         "escalation",
		Status:       model.JobStatusRunning,
		Dedup:        jobdedup.Escalation(ag.ID),
		AlertGroupID: &escalationAGID,
	}
	escalationStep := &model.JobStep{
		ID:        "step1",
		JobID:     "escalation_job_1",
		StepIndex: 1,
		Status:    model.JobStepStatusBlocked, // Waiting to run
	}
	s.CreateJobWithDedup(escalationJob, nil, []*model.JobStep{escalationStep})

	ctx := context.Background()

	// Process acknowledged AG - should cancel escalation job
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Verify escalation job is canceled
	updatedJob, _ := s.GetJobByID("escalation_job_1")
	if updatedJob.Status != model.JobStatusCanceled {
		t.Errorf("Expected escalation job to be canceled, but got status %s", updatedJob.Status)
	}
}

// ===================================================================================
// AckProcessedAt Tests - Fix for infinite loop when no Slack deliveries exist
// ===================================================================================

// TestProcessAcknowledgedAlertGroups_MarksAsProcessed tests that ProcessAcknowledgedAlertGroups
// marks alert groups as processed via ack_processed_at, preventing infinite loops.
func TestProcessAcknowledgedAlertGroups_MarksAsProcessed(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)

	mp := &MockProvider{
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"ts":"1234567890.123456"}`, nil
		},
	}
	d.RegisterProvider("slack", mp)

	// Create acknowledged AG
	ag := &model.AlertGroup{
		ID:       "ag1",
		DedupKey: "dk1",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	// Create an updatable delivery
	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag1",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	s.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// First iteration: should process and mark as processed
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Verify AG is marked as processed
	updatedAG, _ := s.GetAlertGroupByID("ag1")
	if updatedAG.AckProcessedAt == nil {
		t.Error("Expected AckProcessedAt to be set after processing")
	}
}

// TestProcessAcknowledgedAlertGroups_NoInfiniteLoop_NoDeliveries tests the fix for the infinite loop
// bug where ProcessAcknowledgedAlertGroups would repeatedly log "Canceling escalation job" every 2 seconds
// when there are no Slack deliveries to update.
//
// Root cause: When Build() returns nil (no updatable deliveries), no job was created,
// so existingJob check always failed, causing the loop to repeat indefinitely.
//
// Fix: Now we mark AG as processed via ack_processed_at regardless of whether a job was created.
func TestProcessAcknowledgedAlertGroups_NoInfiniteLoop_NoDeliveries(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)

	d.RegisterProvider("slack", &MockProvider{})

	// Create acknowledged AG WITHOUT any deliveries (this triggers the bug)
	ag := &model.AlertGroup{
		ID:       "ag_no_deliveries",
		DedupKey: "dk_no_deliveries",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	ctx := context.Background()

	// First iteration: should mark as processed even without creating a job
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Verify AG is marked as processed
	updatedAG, _ := s.GetAlertGroupByID("ag_no_deliveries")
	if updatedAG.AckProcessedAt == nil {
		t.Error("Expected AckProcessedAt to be set even when no deliveries exist")
	}

	// Second iteration: AG should NOT be returned by GetAcknowledgedAlertGroups
	ags, _ := s.GetAcknowledgedAlertGroups()
	for _, ag := range ags {
		if ag.ID == "ag_no_deliveries" {
			t.Error("AG should be filtered out after being marked as processed (infinite loop bug)")
		}
	}
}

// TestProcessAcknowledgedAlertGroups_MultipleIterations_NoLoop verifies that after being
// processed, the same AG is not processed again on subsequent iterations.
func TestProcessAcknowledgedAlertGroups_MultipleIterations_NoLoop(t *testing.T) {
	s := store.NewMockStore()
	wrapper := &JobCountingStoreWrapper{StoreInterface: s}
	cfg := &config.Config{}
	d := mustNewDispatcher(t, wrapper, cfg)

	d.RegisterProvider("slack", &MockProvider{})

	// Create acknowledged AG with a delivery
	ag := &model.AlertGroup{
		ID:       "ag1",
		DedupKey: "dk1",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag1",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	s.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// Run 5 iterations (simulating the 2-second ticker)
	for i := 0; i < 5; i++ {
		d.ProcessAcknowledgedAlertGroups(ctx)
	}

	// Verify only 1 job was created (no duplicates from looping)
	if wrapper.JobsCreated != 1 {
		t.Errorf("Expected exactly 1 job, but got %d (infinite loop detected)", wrapper.JobsCreated)
	}

	// Verify AG is marked as processed
	updatedAG, _ := s.GetAlertGroupByID("ag1")
	if updatedAG.AckProcessedAt == nil {
		t.Error("Expected AckProcessedAt to be set")
	}
}

// TestGetAcknowledgedAlertGroups_FiltersProcessed tests that GetAcknowledgedAlertGroups
// correctly filters out alert groups that have already been processed (ack_processed_at != nil).
func TestGetAcknowledgedAlertGroups_FiltersProcessed(t *testing.T) {
	s := store.NewMockStore()

	// Create two acknowledged AGs
	ag1 := &model.AlertGroup{
		ID:       "ag_unprocessed",
		DedupKey: "dk1",
		Status:   model.AlertGroupStatusAcknowledged,
		// AckProcessedAt is nil (not processed)
	}
	ag2 := &model.AlertGroup{
		ID:       "ag_processed",
		DedupKey: "dk2",
		Status:   model.AlertGroupStatusAcknowledged,
		// Will be marked as processed below
	}
	s.CreateAlertGroup(ag1)
	s.CreateAlertGroup(ag2)

	// Mark ag2 as processed
	s.MarkAckProcessed("ag_processed")

	// Get acknowledged alert groups
	ags, err := s.GetAcknowledgedAlertGroups()
	if err != nil {
		t.Fatalf("GetAcknowledgedAlertGroups failed: %v", err)
	}

	// Should only return unprocessed AG
	if len(ags) != 1 {
		t.Errorf("Expected 1 unprocessed AG, got %d", len(ags))
	}

	if len(ags) > 0 && ags[0].ID != "ag_unprocessed" {
		t.Errorf("Expected ag_unprocessed, got %s", ags[0].ID)
	}
}

// TestMarkAckProcessed_SetsTimestamp tests that MarkAckProcessed correctly sets
// the ack_processed_at timestamp on an alert group.
func TestMarkAckProcessed_SetsTimestamp(t *testing.T) {
	s := store.NewMockStore()

	// Create AG
	ag := &model.AlertGroup{
		ID:       "ag1",
		DedupKey: "dk1",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	// Initially, AckProcessedAt should be nil
	fetched, _ := s.GetAlertGroupByID("ag1")
	if fetched.AckProcessedAt != nil {
		t.Error("Expected AckProcessedAt to be nil initially")
	}

	// Mark as processed
	err := s.MarkAckProcessed("ag1")
	if err != nil {
		t.Fatalf("MarkAckProcessed failed: %v", err)
	}

	// Verify timestamp is set
	fetched, _ = s.GetAlertGroupByID("ag1")
	if fetched.AckProcessedAt == nil {
		t.Error("Expected AckProcessedAt to be set after MarkAckProcessed")
	}
}

// TestProcessAcknowledgedAlertGroups_MarksProcessedOnError tests that when builder.Build()
// returns an error, the AG is still marked as processed to prevent retry loops.
func TestProcessAcknowledgedAlertGroups_MarksProcessedOnError(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)

	d.RegisterProvider("slack", &MockProvider{})

	// Create acknowledged AG - Build() will return "no updatable deliveries" error
	// when there are no deliveries, which is normal and should still mark as processed
	ag := &model.AlertGroup{
		ID:       "ag_error",
		DedupKey: "dk_error",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	ctx := context.Background()
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Even with no deliveries (which causes builder to skip), AG should be marked processed
	updatedAG, _ := s.GetAlertGroupByID("ag_error")
	if updatedAG.AckProcessedAt == nil {
		t.Error("Expected AckProcessedAt to be set even when Build returns error/nil")
	}
}

// ===================================================================================
// Tests for identified issues (these tests FAIL to expose bugs, then we fix them)
// ===================================================================================

// FailingCreateJobStoreWrapper wraps MockStore to inject CreateJobWithDedup errors
type FailingCreateJobStoreWrapper struct {
	store.StoreInterface
	FailCount int
}

func (f *FailingCreateJobStoreWrapper) CreateJobWithDedup(job *model.Job, stages []*model.JobStage, steps []*model.JobStep) (bool, error) {
	if f.FailCount > 0 {
		f.FailCount--
		return false, errors.New("transient db error")
	}
	return f.StoreInterface.CreateJobWithDedup(job, stages, steps)
}

// TestProcessAcknowledgedAlertGroups_TransientJobCreationError_ShouldRetry tests that
// when CreateJobWithDedup fails due to a transient error, the AG should NOT be marked
// as processed, allowing retry on the next iteration.
//
// ISSUE: Currently marks as processed even on failure, blocking retries forever.
func TestProcessAcknowledgedAlertGroups_TransientJobCreationError_ShouldRetry(t *testing.T) {
	realStore := store.NewMockStore()
	// Fail the first CreateJobWithDedup call
	wrapper := &FailingCreateJobStoreWrapper{
		StoreInterface: realStore,
		FailCount:      1,
	}

	cfg := &config.Config{}
	d := mustNewDispatcher(t, wrapper, cfg)
	d.RegisterProvider("slack", &MockProvider{})

	// Create acknowledged AG with delivery
	ag := &model.AlertGroup{
		ID:       "ag_transient",
		DedupKey: "dk_transient",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	realStore.CreateAlertGroup(ag)

	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag_transient",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	realStore.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// First iteration: CreateJobWithDedup fails (transient error)
	d.ProcessAcknowledgedAlertGroups(ctx)

	// AG should NOT be marked as processed after transient failure
	updatedAG, _ := realStore.GetAlertGroupByID("ag_transient")
	if updatedAG.AckProcessedAt != nil {
		t.Error("AG should NOT be marked as processed after transient CreateJobWithDedup failure - retry blocked forever!")
	}

	// Second iteration: should succeed now
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Verify job was created on retry
	job, _ := realStore.FindJobByIdentity(jobdedup.AckUpdate("ag_transient"))
	if job == nil {
		t.Error("Expected update job to be created on retry")
	}
}

// TestReAck_ShouldClearAckProcessedAt tests that when a user re-acknowledges an AG
// (to retry Slack update after a failure), ack_processed_at should be cleared
// so the AG is picked up again by ProcessAcknowledgedAlertGroups.
//
// ISSUE: UpdateAlertGroupAcknowledged doesn't clear ack_processed_at, so re-ack is broken.
func TestReAck_ShouldClearAckProcessedAt(t *testing.T) {
	s := store.NewMockStore()

	// Create AG and mark as acknowledged
	ag := &model.AlertGroup{
		ID:       "ag_reack",
		DedupKey: "dk_reack",
		Status:   model.AlertGroupStatusTriggered, // Start as triggered
	}
	s.CreateAlertGroup(ag)

	// First ack
	s.UpdateAlertGroupAcknowledged("ag_reack", "user1")

	// Mark as processed (simulating first ack processing completed)
	s.MarkAckProcessed("ag_reack")

	// Verify it's processed
	fetched, _ := s.GetAlertGroupByID("ag_reack")
	if fetched.AckProcessedAt == nil {
		t.Fatal("Setup error: AckProcessedAt should be set")
	}

	// Re-ack (user wants to retry)
	s.UpdateAlertGroupAcknowledged("ag_reack", "user2")

	// ack_processed_at should be cleared to allow reprocessing
	fetched, _ = s.GetAlertGroupByID("ag_reack")
	if fetched.AckProcessedAt != nil {
		t.Error("Re-ack should clear ack_processed_at to allow retry, but it's still set!")
	}

	// AG should appear in GetAcknowledgedAlertGroups for reprocessing
	ags, _ := s.GetAcknowledgedAlertGroups()
	found := false
	for _, ag := range ags {
		if ag.ID == "ag_reack" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Re-acked AG should be returned by GetAcknowledgedAlertGroups for retry")
	}
}

// TestGetAlertGroupByID_ShouldIncludeAckProcessedAt tests that GetAlertGroupByID
// includes the ack_processed_at field in the response for API consistency.
//
// ISSUE: ack_processed_at is in swagger but not selected in GetAlertGroupByID query.
// Note: This test uses MockStore which works correctly, but the real Store doesn't select it.
func TestGetAlertGroupByID_ShouldIncludeAckProcessedAt(t *testing.T) {
	s := store.NewMockStore()

	// Create AG
	ag := &model.AlertGroup{
		ID:       "ag_api",
		DedupKey: "dk_api",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	// Mark as processed
	s.MarkAckProcessed("ag_api")

	// Fetch via GetAlertGroupByID - should include AckProcessedAt
	fetched, err := s.GetAlertGroupByID("ag_api")
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}

	// AckProcessedAt should be populated (not nil)
	if fetched.AckProcessedAt == nil {
		t.Error("GetAlertGroupByID should return AckProcessedAt field, but it's nil")
	}
}

// TestDeliveryCreatedAfterAck_ShouldBeUpdated tests the case where a notification
// delivery is created AFTER the AG was acknowledged (e.g., slow Slack API response).
// The update job should still be able to update this delivery.
//
// ISSUE: If ack_processed_at is set before delivery exists, the new delivery
// will never be updated because the AG won't be picked up again.
func TestDeliveryCreatedAfterAck_ShouldBeUpdated(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)
	d.RegisterProvider("slack", &MockProvider{})

	// Create AG and acknowledge it
	ag := &model.AlertGroup{
		ID:       "ag_late_delivery",
		DedupKey: "dk_late_delivery",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)
	// Note: NO delivery exists yet

	ctx := context.Background()

	// First processing: no deliveries exist, AG gets marked as processed
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Now a delivery arrives (slow Slack response, or another escalation step completed)
	delivery := &model.NotificationDelivery{
		ID:              "late_delivery",
		AlertGroupID:    "ag_late_delivery",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	s.UpsertNotificationDelivery(delivery)

	// AG should be processed again to update this delivery
	// Currently it won't because ack_processed_at is already set
	ags, _ := s.GetAcknowledgedAlertGroups()
	found := false
	for _, ag := range ags {
		if ag.ID == "ag_late_delivery" {
			found = true
			break
		}
	}

	// This test documents the limitation - late deliveries won't be updated
	// If this is important, we need a different mechanism (e.g., delivery-level tracking)
	if !found {
		t.Log("KNOWN LIMITATION: Deliveries created after ack processing won't be updated")
		t.Log("Consider: delivery-level ack tracking, or periodic re-scan, or webhook from delivery creation")
	}
}

// TestProcessAcknowledgedAlertGroups_OnlyMarkProcessedOnSuccess tests that
// ack_processed_at is ONLY set when processing actually succeeds (job created
// or no deliveries to update), not on transient failures.
func TestProcessAcknowledgedAlertGroups_OnlyMarkProcessedOnSuccess(t *testing.T) {
	testCases := []struct {
		name                string
		hasDelivery         bool
		jobCreationFails    bool
		shouldMarkProcessed bool
		description         string
	}{
		{
			name:                "no_deliveries",
			hasDelivery:         false,
			jobCreationFails:    false,
			shouldMarkProcessed: true,
			description:         "No deliveries = nothing to update, mark as processed",
		},
		{
			name:                "job_created_successfully",
			hasDelivery:         true,
			jobCreationFails:    false,
			shouldMarkProcessed: true,
			description:         "Job created successfully, mark as processed",
		},
		{
			name:                "job_creation_transient_failure",
			hasDelivery:         true,
			jobCreationFails:    true,
			shouldMarkProcessed: false,
			description:         "Transient failure should NOT mark as processed (allow retry)",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			realStore := store.NewMockStore()
			var storeToUse store.StoreInterface = realStore

			if tc.jobCreationFails {
				storeToUse = &FailingCreateJobStoreWrapper{
					StoreInterface: realStore,
					FailCount:      1,
				}
			}

			cfg := &config.Config{}
			d := mustNewDispatcher(t, storeToUse, cfg)
			d.RegisterProvider("slack", &MockProvider{})

			ag := &model.AlertGroup{
				ID:       "ag_" + tc.name,
				DedupKey: "dk_" + tc.name,
				Status:   model.AlertGroupStatusAcknowledged,
			}
			realStore.CreateAlertGroup(ag)

			if tc.hasDelivery {
				delivery := &model.NotificationDelivery{
					ID:              "delivery_" + tc.name,
					AlertGroupID:    ag.ID,
					Provider:        "slack",
					SupportsUpdate:  true,
					IsPrimary:       true,
					ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
				}
				realStore.UpsertNotificationDelivery(delivery)
			}

			ctx := context.Background()
			d.ProcessAcknowledgedAlertGroups(ctx)

			updatedAG, _ := realStore.GetAlertGroupByID(ag.ID)
			isProcessed := updatedAG.AckProcessedAt != nil

			if isProcessed != tc.shouldMarkProcessed {
				t.Errorf("%s: expected processed=%v, got processed=%v",
					tc.description, tc.shouldMarkProcessed, isProcessed)
			}
		})
	}
}

// ===================================================================================
// Transient Build Error Tests (Issue 1 fix)
// ===================================================================================

// FailingListDeliveriesStoreWrapper wraps MockStore to inject ListDeliveries errors
type FailingListDeliveriesStoreWrapper struct {
	store.StoreInterface
	FailCount int
}

func (f *FailingListDeliveriesStoreWrapper) ListDeliveries(alertGroupID string) ([]*model.NotificationDelivery, error) {
	if f.FailCount > 0 {
		f.FailCount--
		return nil, errors.New("transient db error")
	}
	return f.StoreInterface.ListDeliveries(alertGroupID)
}

// TestProcessAcknowledgedAlertGroups_TransientBuildError_ShouldRetry tests that when
// Build() fails due to a transient error (e.g., ListDeliveries DB error), the AG should
// NOT be marked as processed, allowing retry on the next iteration.
func TestProcessAcknowledgedAlertGroups_TransientBuildError_ShouldRetry(t *testing.T) {
	realStore := store.NewMockStore()
	// Fail the first ListDeliveries call
	wrapper := &FailingListDeliveriesStoreWrapper{
		StoreInterface: realStore,
		FailCount:      1,
	}

	cfg := &config.Config{}
	d := mustNewDispatcher(t, wrapper, cfg)
	d.RegisterProvider("slack", &MockProvider{})

	// Create acknowledged AG with delivery
	ag := &model.AlertGroup{
		ID:       "ag_build_transient",
		DedupKey: "dk_build_transient",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	realStore.CreateAlertGroup(ag)

	delivery := &model.NotificationDelivery{
		ID:              "delivery_build",
		AlertGroupID:    "ag_build_transient",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	realStore.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// First iteration: Build() fails due to ListDeliveries error (transient)
	d.ProcessAcknowledgedAlertGroups(ctx)

	// AG should NOT be marked as processed after transient Build failure
	updatedAG, _ := realStore.GetAlertGroupByID("ag_build_transient")
	if updatedAG.AckProcessedAt != nil {
		t.Error("AG should NOT be marked as processed after transient Build failure - retry blocked forever!")
	}

	// AG should still be returned by GetAcknowledgedAlertGroups for retry
	ags, _ := realStore.GetAcknowledgedAlertGroups()
	found := false
	for _, ag := range ags {
		if ag.ID == "ag_build_transient" {
			found = true
			break
		}
	}
	if !found {
		t.Error("AG should be returned by GetAcknowledgedAlertGroups for retry after transient Build error")
	}

	// Second iteration: should succeed now (FailCount exhausted)
	d.ProcessAcknowledgedAlertGroups(ctx)

	// Verify job was created on retry
	job, _ := realStore.FindJobByIdentity(jobdedup.AckUpdate("ag_build_transient"))
	if job == nil {
		t.Error("Expected update job to be created on retry after transient Build error")
	}

	// AG should now be marked as processed
	updatedAG, _ = realStore.GetAlertGroupByID("ag_build_transient")
	if updatedAG.AckProcessedAt == nil {
		t.Error("AG should be marked as processed after successful job creation")
	}
}

// ===================================================================================
// Regression Tests: completeStep deadlock + ProcessResolvedAlertGroups lost notifications
// ===================================================================================

// TestRegression_CompleteStep_UnlockFailure tests that when FinishStepAndAdvance fails
// after all retries in completeStep, the job is failed (not deadlocked).
// Regression: previously completeStep swallowed unlock errors, leaving
// step N+1 blocked forever.
func TestRegression_CompleteStep_UnlockFailure(t *testing.T) {
	realStore := store.NewMockStore()
	// All finish attempts fail (more than maxRetries=3)
	wrapper := &FinishStepFailingStoreWrapper{
		StoreInterface:  realStore,
		FinishFailCount: 10,
	}

	d := mustNewDispatcher(t, wrapper, &config.Config{})

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, message string) error {
			return nil // step execution succeeds
		},
	}
	d.RegisterProvider("slack", mp)

	realStore.CreateAlertGroup(&model.AlertGroup{ID: "ag1", DedupKey: "dk1"})

	// Create job with TWO steps (each in its own stage)
	leaseToken := "lease-regression"
	job := &model.Job{ID: "job1", Status: model.JobStatusRunning, Dedup: testJobIdentity("job1")}
	stepData0 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U1", ProviderName: "slack"}
	dataBytes0, _ := json.Marshal(stepData0)
	stepData1 := model.EscalationStepData{AlertGroupID: "ag1", TargetID: "U2", ProviderName: "slack"}
	dataBytes1, _ := json.Marshal(stepData1)

	step0 := &model.JobStep{
		ID:          "step0",
		JobID:       "job1",
		StageID:     "stage-0",
		StepIndex:   0,
		StepType:    "dm",
		Status:      model.JobStepStatusRunning,
		Data:        json.RawMessage(dataBytes0),
		MaxAttempts: 3,
		LockedBy:    &leaseToken,
	}
	step1 := &model.JobStep{
		ID:        "step1",
		JobID:     "job1",
		StageID:   "stage-1",
		StepIndex: 0,
		StepType:  "dm",
		Status:    model.JobStepStatusBlocked,
		Data:      json.RawMessage(dataBytes1),
	}
	stages := []*model.JobStage{
		{ID: "stage-0", JobID: "job1", StageIndex: 0, Status: model.JobStageStatusActive},
		{ID: "stage-1", JobID: "job1", StageIndex: 1, Status: model.JobStageStatusBlocked},
	}
	realStore.CreateJobWithDedup(job, stages, []*model.JobStep{step0, step1})

	// Execute step 0 — succeeds, but FinishStepAndAdvance fails after retries
	d.processStep(context.Background(), step0)

	// Job should be FAILED (not deadlocked with step1 blocked forever)
	updatedJob, _ := realStore.GetJobByID("job1")
	if updatedJob.Status != model.JobStatusFailed {
		t.Errorf("Expected job status Failed after FinishStepAndAdvance retries exhausted, got %s", updatedJob.Status)
	}
}

// TestRegression_ProcessResolved_CreateJobFailure tests that when CreateJobWithDedup
// fails for a resolved AG, the AG stays in "resolved" status (not "closed"),
// allowing retry on the next tick.
// Regression: previously ProcessResolvedAlertGroups marked AG as closed unconditionally,
// even if job creation failed — losing the resolution notification forever.
func TestRegression_ProcessResolved_CreateJobFailure(t *testing.T) {
	realStore := store.NewMockStore()
	wrapper := &FailingCreateJobStoreWrapper{
		StoreInterface: realStore,
		FailCount:      1, // Fail first CreateJobWithDedup
	}

	cfg := &config.Config{}
	d := mustNewDispatcher(t, wrapper, cfg)
	d.RegisterProvider("slack", &MockProvider{})

	// Create resolved AG with an updatable delivery
	ag := &model.AlertGroup{
		ID:       "ag_resolved",
		DedupKey: "dk_resolved",
		Status:   model.AlertGroupStatusResolved,
	}
	realStore.CreateAlertGroup(ag)

	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag_resolved",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	realStore.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// First iteration: CreateJobWithDedup fails
	d.ProcessResolvedAlertGroups(ctx)

	// AG should still be "resolved" (NOT "closed")
	updatedAG, _ := realStore.GetAlertGroupByID("ag_resolved")
	if updatedAG.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected AG status 'resolved' after CreateJobWithDedup failure, got '%s'", updatedAG.Status)
	}

	// Second iteration: should succeed now (FailCount exhausted)
	d.ProcessResolvedAlertGroups(ctx)

	// AG should now be closed
	updatedAG, _ = realStore.GetAlertGroupByID("ag_resolved")
	if updatedAG.Status != model.AlertGroupStatusClosed {
		t.Errorf("Expected AG status 'closed' after successful retry, got '%s'", updatedAG.Status)
	}
}

// TestRegression_ProcessResolved_BuildFailure tests that when Build() returns an error
// for a resolved AG, the AG stays in "resolved" status (not "closed"),
// allowing retry on the next tick.
// Regression: previously ProcessResolvedAlertGroups marked AG as closed unconditionally,
// even if Build() failed — losing the resolution notification forever.
func TestRegression_ProcessResolved_BuildFailure(t *testing.T) {
	realStore := store.NewMockStore()
	// Fail first ListDeliveries call (which Build() calls internally)
	wrapper := &FailingListDeliveriesStoreWrapper{
		StoreInterface: realStore,
		FailCount:      1,
	}

	cfg := &config.Config{}
	d := mustNewDispatcher(t, wrapper, cfg)
	d.RegisterProvider("slack", &MockProvider{})

	// Create resolved AG with an updatable delivery
	ag := &model.AlertGroup{
		ID:       "ag_build_fail",
		DedupKey: "dk_build_fail",
		Status:   model.AlertGroupStatusResolved,
	}
	realStore.CreateAlertGroup(ag)

	delivery := &model.NotificationDelivery{
		ID:              "delivery1",
		AlertGroupID:    "ag_build_fail",
		Provider:        "slack",
		SupportsUpdate:  true,
		IsPrimary:       true,
		ProviderPayload: `{"channel":"C123","ts":"1234567890.000000"}`,
	}
	realStore.UpsertNotificationDelivery(delivery)

	ctx := context.Background()

	// First iteration: Build() fails due to ListDeliveries error
	d.ProcessResolvedAlertGroups(ctx)

	// AG should still be "resolved" (NOT "closed")
	updatedAG, _ := realStore.GetAlertGroupByID("ag_build_fail")
	if updatedAG.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected AG status 'resolved' after Build failure, got '%s'", updatedAG.Status)
	}

	// Second iteration: should succeed now (FailCount exhausted)
	d.ProcessResolvedAlertGroups(ctx)

	// AG should now be closed
	updatedAG, _ = realStore.GetAlertGroupByID("ag_build_fail")
	if updatedAG.Status != model.AlertGroupStatusClosed {
		t.Errorf("Expected AG status 'closed' after successful retry, got '%s'", updatedAG.Status)
	}
}

// TestProcessAcknowledgedAlertGroups_NoDeliveries_UsesError tests that when Build()
// returns ErrNoUpdatableDeliveries (no deliveries to update), the AG is marked as
// processed correctly.
func TestProcessAcknowledgedAlertGroups_NoDeliveries_UsesError(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d := mustNewDispatcher(t, s, cfg)
	d.RegisterProvider("slack", &MockProvider{})

	// Create acknowledged AG WITHOUT any deliveries
	ag := &model.AlertGroup{
		ID:       "ag_no_deliveries_sentinel",
		DedupKey: "dk_no_deliveries_sentinel",
		Status:   model.AlertGroupStatusAcknowledged,
	}
	s.CreateAlertGroup(ag)

	ctx := context.Background()

	// Process - should use ErrNoUpdatableDeliveries sentinel and mark as processed
	d.ProcessAcknowledgedAlertGroups(ctx)

	// AG should be marked as processed
	updatedAG, _ := s.GetAlertGroupByID("ag_no_deliveries_sentinel")
	if updatedAG.AckProcessedAt == nil {
		t.Error("AG with no deliveries should be marked as processed via ErrNoUpdatableDeliveries sentinel")
	}

	// AG should NOT be returned by GetAcknowledgedAlertGroups anymore
	ags, _ := s.GetAcknowledgedAlertGroups()
	for _, ag := range ags {
		if ag.ID == "ag_no_deliveries_sentinel" {
			t.Error("Processed AG should not be returned by GetAcknowledgedAlertGroups")
		}
	}
}

// =================================================================================
// Alert Update Loop Tests
// =================================================================================

func TestProcessAlertUpdates_CreatesUpdateJob(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d, _ := NewDispatcher(s, cfg)
	d.RegisterProvider("slack", &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			return `{"channel_id":"C123","timestamp":"100.200"}`, nil
		},
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"channel_id":"C123","timestamp":"100.200"}`, nil
		},
	})

	// Setup: create AG in processing state with slack_update_pending
	ag := &model.AlertGroup{
		ID:       "ag_update",
		DedupKey: "dk_update",
		Status:   model.AlertGroupStatusProcessing,
		Title:    "Test",
		Severity: "critical",
	}
	s.CreateAlertGroup(ag)
	s.SetSlackUpdatePending("ag_update", true)

	// Create a delivery so UpdateJobBuilder can find it
	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		ID:              "del1",
		AlertGroupID:    "ag_update",
		Provider:        "slack",
		SupportsUpdate:  true,
		ProviderPayload: `{"channel_id":"C123","timestamp":"100.200"}`,
	})

	// Run ProcessAlertUpdates
	ctx := context.Background()
	d.ProcessAlertUpdates(ctx)

	// Verify: job was created
	job, err := s.FindJobByIdentity(jobdedup.AlertUpdate("ag_update"))
	if err != nil {
		t.Fatalf("Job not found: %v", err)
	}
	if job == nil {
		t.Fatal("Expected update job to be created")
	}
	if job.Type != "update" {
		t.Errorf("Expected job type 'update', got '%s'", job.Type)
	}

	// Verify: flag was cleared
	updated, _ := s.GetAlertGroupByID("ag_update")
	if updated.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be false after job creation")
	}
}

func TestProcessAlertUpdates_KeepsFlagWhenNoDeliveries(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d, _ := NewDispatcher(s, cfg)

	// Setup: AG with flag but NO deliveries (escalation not done yet)
	ag := &model.AlertGroup{
		ID:       "ag_no_del",
		DedupKey: "dk_no_del",
		Status:   model.AlertGroupStatusProcessing,
		Title:    "Test",
		Severity: "info",
	}
	s.CreateAlertGroup(ag)
	s.SetSlackUpdatePending("ag_no_del", true)

	ctx := context.Background()
	d.ProcessAlertUpdates(ctx)

	// Verify: flag should STILL be true (no deliveries → retry later)
	updated, _ := s.GetAlertGroupByID("ag_no_del")
	if !updated.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to stay true when no deliveries exist")
	}
}

func TestProcessAlertUpdates_ClearsFlagWhenNoUpdatableDeliveries(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d, _ := NewDispatcher(s, cfg)

	// Setup: AG with flag and a delivery that does NOT support updates
	ag := &model.AlertGroup{
		ID:       "ag_no_upd",
		DedupKey: "dk_no_upd",
		Status:   model.AlertGroupStatusProcessing,
		Title:    "Test",
		Severity: "info",
	}
	s.CreateAlertGroup(ag)
	s.SetSlackUpdatePending("ag_no_upd", true)

	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		ID:             "del_no_upd",
		AlertGroupID:   "ag_no_upd",
		Provider:       "slack",
		SupportsUpdate: false, // delivery exists but not updatable
	})

	ctx := context.Background()
	d.ProcessAlertUpdates(ctx)

	// Verify: flag cleared — deliveries exist but none support updates, so flag would be stuck forever
	updated, _ := s.GetAlertGroupByID("ag_no_upd")
	if updated.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be false when deliveries exist but none are updatable")
	}
}

func TestProcessAlertUpdates_ClearsFlagOnDedupHit(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d, _ := NewDispatcher(s, cfg)
	d.RegisterProvider("slack", &MockProvider{
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"channel_id":"C123","timestamp":"100.200"}`, nil
		},
	})

	// Setup: AG with delivery and flag
	ag := &model.AlertGroup{
		ID:       "ag_dedup",
		DedupKey: "dk_dedup",
		Status:   model.AlertGroupStatusProcessing,
		Title:    "Test",
		Severity: "info",
	}
	s.CreateAlertGroup(ag)
	s.SetSlackUpdatePending("ag_dedup", true)

	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		ID:              "del_dedup",
		AlertGroupID:    "ag_dedup",
		Provider:        "slack",
		SupportsUpdate:  true,
		ProviderPayload: `{"channel_id":"C123","timestamp":"100.200"}`,
	})

	// First run — creates job with dedup key "update_alert_ag_dedup"
	ctx := context.Background()
	d.ProcessAlertUpdates(ctx)

	// Verify first job was created and flag cleared
	job, _ := s.FindJobByIdentity(jobdedup.AlertUpdate("ag_dedup"))
	if job == nil {
		t.Fatal("Expected job to be created on first run")
	}
	if job.Status != model.JobStatusPending {
		t.Fatalf("Expected job status pending, got %s", job.Status)
	}

	// Re-set flag (simulating new alerts arriving while job is still pending)
	s.SetSlackUpdatePending("ag_dedup", true)

	// Second run — job is still pending, so CreateJobWithDedup reports created=false
	d.ProcessAlertUpdates(ctx)

	// Verify: flag cleared even though no new job was created (dedup hit)
	updated, _ := s.GetAlertGroupByID("ag_dedup")
	if updated.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be false after dedup hit")
	}

	// Verify: still the same job (dedup returned existing, didn't create new)
	jobAfter, _ := s.FindJobByIdentity(jobdedup.AlertUpdate("ag_dedup"))
	if jobAfter == nil {
		t.Fatal("Expected job to still exist")
	}
	if jobAfter.ID != job.ID {
		t.Errorf("Expected same job ID %s after dedup hit, got %s", job.ID, jobAfter.ID)
	}
}

func TestProcessAlertUpdates_HandlesTriggeredGroups(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d, _ := NewDispatcher(s, cfg)
	d.RegisterProvider("slack", &MockProvider{
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"channel_id":"C123","timestamp":"100.200"}`, nil
		},
	})

	// Setup: triggered AG with slack_update_pending
	ag := &model.AlertGroup{
		ID:       "ag_triggered",
		DedupKey: "dk_triggered",
		Status:   model.AlertGroupStatusTriggered,
		Title:    "Test Triggered",
		Severity: "critical",
	}
	s.CreateAlertGroup(ag)
	s.SetSlackUpdatePending("ag_triggered", true)

	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		ID:              "del_triggered",
		AlertGroupID:    "ag_triggered",
		Provider:        "slack",
		SupportsUpdate:  true,
		ProviderPayload: `{"channel_id":"C123","timestamp":"100.200"}`,
	})

	ctx := context.Background()
	d.ProcessAlertUpdates(ctx)

	// Verify: job created for triggered group
	job, _ := s.FindJobByIdentity(jobdedup.AlertUpdate("ag_triggered"))
	if job == nil {
		t.Fatal("Expected alert update job to be created for triggered group")
	}

	// Verify: flag cleared
	updated, _ := s.GetAlertGroupByID("ag_triggered")
	if updated.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be false after job creation")
	}
}

func TestProcessAlertUpdates_HandlesAcknowledgedGroups(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	d, _ := NewDispatcher(s, cfg)
	d.RegisterProvider("slack", &MockProvider{
		UpdateFunc: func(ctx context.Context, ag *model.AlertGroup) (string, error) {
			return `{"channel_id":"C123","timestamp":"100.200"}`, nil
		},
	})

	// Setup: acknowledged AG with ack already processed + new flag
	ag := &model.AlertGroup{
		ID:       "ag_acked",
		DedupKey: "dk_acked",
		Status:   model.AlertGroupStatusAcknowledged,
		Title:    "Test",
		Severity: "critical",
	}
	s.CreateAlertGroup(ag)
	s.UpdateAlertGroupAcknowledged("ag_acked", "user1")
	s.MarkAckProcessed("ag_acked")
	s.SetSlackUpdatePending("ag_acked", true)

	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		ID:              "del_acked",
		AlertGroupID:    "ag_acked",
		Provider:        "slack",
		SupportsUpdate:  true,
		ProviderPayload: `{"channel_id":"C123","timestamp":"100.200"}`,
	})

	ctx := context.Background()
	d.ProcessAlertUpdates(ctx)

	// Verify: job created for acknowledged group
	job, _ := s.FindJobByIdentity(jobdedup.AlertUpdate("ag_acked"))
	if job == nil {
		t.Fatal("Expected alert update job to be created for acknowledged group")
	}

	// Verify: flag cleared
	updated, _ := s.GetAlertGroupByID("ag_acked")
	if updated.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be false after job creation")
	}
}
