package dispatcher

import (
	"context"
	"errors"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestUnknownExecutor_FailsJob(t *testing.T) {
	s := store.NewMockStore()
	d := mustNewDispatcher(t, s)

	leaseToken := "lease-unknown"
	job := &model.Job{ID: "job_u", Status: model.JobStatusRunning, Dedup: testJobIdentity("job_u")}
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
	d := mustNewDispatcher(t, s)

	// Job with 2 steps in 2 stages.
	// Step 0: Done (stage 0 succeeded).
	// Step 1: Running -> Succeeds -> Job Succeeded.

	leaseToken := "lease-completion"
	job := &model.Job{ID: "job_done", Status: model.JobStatusRunning, CurrentStage: 1, Dedup: testJobIdentity("job_done")}
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
	d := mustNewDispatcher(t, s)

	leaseToken := "lease-fail"
	job := &model.Job{ID: "job_fail", Status: model.JobStatusRunning, Dedup: testJobIdentity("job_fail")}
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
