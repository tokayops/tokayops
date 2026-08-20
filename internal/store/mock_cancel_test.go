package store

import (
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// TestMockStore_CancelEscalationCascade: the double cancels what the store
// cancels - the job, its active stages and its pending steps.
//
// Worth its own test because the mock used to hold three different answers to
// this one question: the atomic transitions cancelled the job alone, the public
// method cancelled the job and its stages, and neither touched steps, while the
// real store cancelled all three. A double that under-cancels lets a test pass
// on behaviour production does not have.
func TestMockStore_CancelEscalationCascade(t *testing.T) {
	s := NewMockStore()

	agID := "ag-mock-cancel"
	dedupKey := "key-" + agID // deliberately not the alert group id
	jobID := "job-mock-cancel"

	job := &model.Job{
		ID:           jobID,
		Type:         "escalation",
		Status:       model.JobStatusPending,
		DedupKey:     &dedupKey,
		AlertGroupID: &agID,
	}
	stages := []*model.JobStage{
		{ID: "stage-active", JobID: jobID, Status: model.JobStageStatusActive},
		{ID: "stage-blocked", JobID: jobID, StageIndex: 1, Status: model.JobStageStatusBlocked},
	}
	steps := []*model.JobStep{
		{ID: "step-pending", JobID: jobID, StageID: "stage-active", Status: model.JobStepStatusPending},
		{ID: "step-blocked", JobID: jobID, StageID: "stage-blocked", StepIndex: 1, Status: model.JobStepStatusBlocked},
	}
	if _, _, err := s.CreateJobWithDedup(job, stages, steps); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	if err := s.CancelEscalationJobByAlertGroupID(agID); err != nil {
		t.Fatalf("CancelEscalationJobByAlertGroupID: %v", err)
	}

	got, err := s.GetJobByID(jobID)
	if err != nil || got == nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got.Status != model.JobStatusCanceled {
		t.Errorf("job = %s, want canceled", got.Status)
	}
	// The mock exposes no stage accessor, and adding one to production code for a
	// test would be the wrong trade: the stages are read straight off the double.
	for _, stage := range s.jobStages {
		if stage.JobID != jobID {
			continue
		}
		if stage.Status != model.JobStageStatusCanceled {
			t.Errorf("stage %s = %s, want canceled", stage.ID, stage.Status)
		}
	}
	for _, step := range s.GetJobStepsByJobID(jobID) {
		if step.Status != model.JobStepStatusCanceled {
			t.Errorf("step %s = %s, want canceled", step.ID, step.Status)
		}
	}
}
