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
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/store"
)

// MockProvider adapts the Provider interface to the func-field shapes used
// throughout these tests. A "user" target routes to SendDMFunc
// (fire-and-forget), a "channel" target routes to SendFunc.
type MockProvider struct {
	SendFunc   func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error)
	SendDMFunc func(ctx context.Context, userID, message string) error
}

var _ Provider = (*MockProvider)(nil)

func (m *MockProvider) Send(ctx context.Context, req providers.NotificationRequest) (string, error) {
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

// testJobIdentity gives a fixture job an identity. Tests about the step
// machinery do not care which family a job belongs to; an alert update is
// borrowed because it is while_active, so a fixture never has to reason about
// a claim outliving the job it was written for.
func testJobIdentity(jobID string) *jobdedup.Spec {
	return jobdedup.AlertUpdate("test:" + jobID)
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
	s.CreateAlertGroup(&model.AlertGroup{ID: "ag1", AlertKey: "dk1"})

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

	realStore.CreateAlertGroup(&model.AlertGroup{ID: "ag1", AlertKey: "dk1"})

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
