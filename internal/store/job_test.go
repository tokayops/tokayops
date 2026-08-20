package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/testutil"
)

func TestUpdateJobStepIfOwned_Persistence(t *testing.T) {
	s := testutil.SetupDB(t)

	// Create Job & Step with stage
	jobID := uuid.New().String()
	key := "dedup_persistence"
	job := &model.Job{
		ID:        jobID,
		Type:      "test",
		Status:    model.JobStatusRunning,
		Dedup:     jobdedup.AlertUpdate(key),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	leaseToken := uuid.New().String()
	stageID := uuid.New().String()

	stage := &model.JobStage{
		ID:         stageID,
		JobID:      jobID,
		StageIndex: 0,
		Status:     model.JobStageStatusActive,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	step := &model.JobStep{
		ID:           uuid.New().String(),
		JobID:        jobID,
		StageID:      stageID,
		StepIndex:    0,
		StepType:     "test",
		Status:       model.JobStepStatusRunning,
		AttemptCount: 1,
		MaxAttempts:  3,
		LockedBy:     &leaseToken,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}

	_, err := s.CreateJobWithDedup(job, []*model.JobStage{stage}, []*model.JobStep{step})
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Update Step with new AttemptCount
	step.AttemptCount = 2
	step.Status = model.JobStepStatusRetry
	now := time.Now()
	step.NextRunAt = &now

	owned, err := s.UpdateJobStepIfOwned(step, leaseToken)
	if err != nil {
		t.Fatalf("Failed to update step: %v", err)
	}
	if !owned {
		t.Fatal("Expected owned=true from UpdateJobStepIfOwned")
	}

	// Read back to verify persistence
	var storedAttempt int
	err = s.GetDB().QueryRow("SELECT attempt_count FROM job_steps WHERE id = $1", step.ID).Scan(&storedAttempt)
	if err != nil {
		t.Fatalf("Failed to query step: %v", err)
	}

	if storedAttempt != 2 {
		t.Errorf("Expected attempt_count 2, got %d", storedAttempt)
	}
}

func TestFinishStepAndAdvance_Basic(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_advance"
	job := &model.Job{
		ID:        jobID,
		Dedup:     jobdedup.AlertUpdate(key),
		Status:    model.JobStatusRunning,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	stageID0 := uuid.New().String()
	stageID1 := uuid.New().String()

	stage0 := &model.JobStage{
		ID:         stageID0,
		JobID:      jobID,
		StageIndex: 0,
		Status:     model.JobStageStatusActive,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	stage1 := &model.JobStage{
		ID:         stageID1,
		JobID:      jobID,
		StageIndex: 1,
		Status:     model.JobStageStatusBlocked,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	leaseToken0 := uuid.New().String()
	step0 := &model.JobStep{
		ID:        uuid.New().String(),
		JobID:     jobID,
		StageID:   stageID0,
		StepIndex: 0,
		Status:    model.JobStepStatusRunning,
		LockedBy:  &leaseToken0,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	delaySec := 5
	step1Data := model.LegacyStepData{DelaySeconds: delaySec}
	dataBytes, _ := json.Marshal(step1Data)

	step1 := &model.JobStep{
		ID:        uuid.New().String(),
		JobID:     jobID,
		StageID:   stageID1,
		StepIndex: 0,
		Status:    model.JobStepStatusBlocked,
		Data:      json.RawMessage(dataBytes),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	_, err := s.CreateJobWithDedup(job, []*model.JobStage{stage0, stage1}, []*model.JobStep{step0, step1})
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Finish step0 -> should advance to stage1 (unlock step1)
	res, err := s.FinishStepAndAdvance(step0.ID, leaseToken0, model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("FinishStepAndAdvance failed: %v", err)
	}
	if res != model.AdvanceUnlockedNextStage {
		t.Errorf("Expected AdvanceUnlockedNextStage, got %d", res)
	}

	// Verify Step 1 is Pending and has NextRunAt with delay
	var status string
	var nextRunAt *time.Time
	err = s.GetDB().QueryRow("SELECT status, next_run_at FROM job_steps WHERE id = $1", step1.ID).Scan(&status, &nextRunAt)
	if err != nil {
		t.Fatalf("Failed to query step 1: %v", err)
	}

	if status != string(model.JobStepStatusPending) {
		t.Errorf("Expected step 1 status Pending, got %s", status)
	}

	if nextRunAt == nil {
		t.Fatal("Expected NextRunAt to be set, got nil")
	}
	expectedTime := time.Now().Add(time.Duration(delaySec) * time.Second)
	diff := expectedTime.Sub(*nextRunAt)
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Errorf("Expected NextRunAt around %v, got %v (diff %v)", expectedTime, nextRunAt, diff)
	}

	// ---------------------------------------------------------
	// Now set step1 to running with a lease, then finish it
	// ---------------------------------------------------------
	leaseToken1 := uuid.New().String()
	_, err = s.GetDB().Exec(
		"UPDATE job_steps SET status = 'running', locked_by = $1 WHERE id = $2",
		leaseToken1, step1.ID,
	)
	if err != nil {
		t.Fatalf("Failed to set step1 to running: %v", err)
	}

	res, err = s.FinishStepAndAdvance(step1.ID, leaseToken1, model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("FinishStepAndAdvance (final) failed: %v", err)
	}
	if res != model.AdvanceJobFinished {
		t.Errorf("Expected AdvanceJobFinished, got %d", res)
	}

	// Verify job succeeded
	var jobStatus string
	err = s.GetDB().QueryRow("SELECT status FROM jobs WHERE id = $1", jobID).Scan(&jobStatus)
	if err != nil {
		t.Fatalf("Failed to query job: %v", err)
	}

	if jobStatus != string(model.JobStatusSucceeded) {
		t.Errorf("Expected job succeeded, got %s", jobStatus)
	}
}

func TestFinishStepAndAdvance_LeaseLost(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_lease_lost"
	stageID := uuid.New().String()
	stepID := uuid.New().String()
	realToken := uuid.New().String()

	job := &model.Job{ID: jobID, Type: "test", Status: model.JobStatusRunning, Dedup: jobdedup.AlertUpdate(key), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	stage := &model.JobStage{ID: stageID, JobID: jobID, StageIndex: 0, Status: model.JobStageStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	step := &model.JobStep{ID: stepID, JobID: jobID, StageID: stageID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusRunning, LockedBy: &realToken, MaxAttempts: 3, CreatedAt: time.Now(), UpdatedAt: time.Now()}

	if _, err := s.CreateJobWithDedup(job, []*model.JobStage{stage}, []*model.JobStep{step}); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	// Try to finish with wrong token
	res, err := s.FinishStepAndAdvance(stepID, "wrong-token", model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("FinishStepAndAdvance: %v", err)
	}
	if res != model.AdvanceLeaseLost {
		t.Errorf("Expected AdvanceLeaseLost, got %d", res)
	}

	// Step should still be running
	var status string
	s.GetDB().QueryRow("SELECT status FROM job_steps WHERE id = $1", stepID).Scan(&status)
	if status != string(model.JobStepStatusRunning) {
		t.Errorf("Step should still be running, got %s", status)
	}
}

func TestFinishStepAndAdvance_HardFail(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_hard_fail"
	stage0ID := uuid.New().String()
	stage1ID := uuid.New().String()
	step0ID := uuid.New().String()
	step1ID := uuid.New().String()
	token := uuid.New().String()

	job := &model.Job{ID: jobID, Type: "test", Status: model.JobStatusRunning, Dedup: jobdedup.AlertUpdate(key), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	stage0 := &model.JobStage{ID: stage0ID, JobID: jobID, StageIndex: 0, Status: model.JobStageStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	stage1 := &model.JobStage{ID: stage1ID, JobID: jobID, StageIndex: 1, Status: model.JobStageStatusBlocked, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	step0 := &model.JobStep{ID: step0ID, JobID: jobID, StageID: stage0ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusRunning, LockedBy: &token, MaxAttempts: 3, ContinueOnFailure: false, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	step1 := &model.JobStep{ID: step1ID, JobID: jobID, StageID: stage1ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusBlocked, MaxAttempts: 3, CreatedAt: time.Now(), UpdatedAt: time.Now()}

	if _, err := s.CreateJobWithDedup(job, []*model.JobStage{stage0, stage1}, []*model.JobStep{step0, step1}); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	res, err := s.FinishStepAndAdvance(step0ID, token, model.JobStepStatusFailed, "", "boom")
	if err != nil {
		t.Fatalf("FinishStepAndAdvance: %v", err)
	}
	if res != model.AdvanceJobFinished {
		t.Errorf("Expected AdvanceJobFinished, got %d", res)
	}

	// Job should be failed
	var jobStatus string
	s.GetDB().QueryRow("SELECT status FROM jobs WHERE id = $1", jobID).Scan(&jobStatus)
	if jobStatus != string(model.JobStatusFailed) {
		t.Errorf("Expected job failed, got %s", jobStatus)
	}

	// Stage 0 should be failed
	var stage0Status string
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage0ID).Scan(&stage0Status)
	if stage0Status != string(model.JobStageStatusFailed) {
		t.Errorf("Expected stage0 failed, got %s", stage0Status)
	}

	// Stage 1 should still be blocked (NOT unlocked)
	var stage1Status string
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage1ID).Scan(&stage1Status)
	if stage1Status != string(model.JobStageStatusBlocked) {
		t.Errorf("Expected stage1 blocked, got %s", stage1Status)
	}
}

func TestFinishStepAndAdvance_JobAlreadyTerminal(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_terminal"
	stageID := uuid.New().String()
	stepID := uuid.New().String()
	token := uuid.New().String()

	// Job starts as canceled
	job := &model.Job{ID: jobID, Type: "test", Status: model.JobStatusCanceled, Dedup: jobdedup.AlertUpdate(key), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	stage := &model.JobStage{ID: stageID, JobID: jobID, StageIndex: 0, Status: model.JobStageStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	step := &model.JobStep{ID: stepID, JobID: jobID, StageID: stageID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusRunning, LockedBy: &token, MaxAttempts: 3, CreatedAt: time.Now(), UpdatedAt: time.Now()}

	if _, err := s.CreateJobWithDedup(job, []*model.JobStage{stage}, []*model.JobStep{step}); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	res, err := s.FinishStepAndAdvance(stepID, token, model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("FinishStepAndAdvance: %v", err)
	}
	if res != model.AdvanceJobAlreadyTerminal {
		t.Errorf("Expected AdvanceJobAlreadyTerminal, got %d", res)
	}

	// Step should be canceled (best-effort cleanup)
	var status string
	s.GetDB().QueryRow("SELECT status FROM job_steps WHERE id = $1", stepID).Scan(&status)
	if status != string(model.JobStepStatusCanceled) {
		t.Errorf("Expected step canceled, got %s", status)
	}
}

func TestFinishStepAndAdvance_ContinueOnFailure_LastStage(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_cof_last"
	stageID := uuid.New().String()
	stepID := uuid.New().String()
	token := uuid.New().String()

	job := &model.Job{ID: jobID, Type: "test", Status: model.JobStatusRunning, Dedup: jobdedup.AlertUpdate(key), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	stage := &model.JobStage{ID: stageID, JobID: jobID, StageIndex: 0, Status: model.JobStageStatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	step := &model.JobStep{ID: stepID, JobID: jobID, StageID: stageID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusRunning, LockedBy: &token, MaxAttempts: 3, ContinueOnFailure: true, CreatedAt: time.Now(), UpdatedAt: time.Now()}

	if _, err := s.CreateJobWithDedup(job, []*model.JobStage{stage}, []*model.JobStep{step}); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	res, err := s.FinishStepAndAdvance(stepID, token, model.JobStepStatusFailed, "", "soft fail")
	if err != nil {
		t.Fatalf("FinishStepAndAdvance: %v", err)
	}
	if res != model.AdvanceJobFinished {
		t.Errorf("Expected AdvanceJobFinished, got %d", res)
	}

	// Job should be failed (step failed, even with ContinueOnFailure)
	var jobStatus string
	s.GetDB().QueryRow("SELECT status FROM jobs WHERE id = $1", jobID).Scan(&jobStatus)
	if jobStatus != string(model.JobStatusFailed) {
		t.Errorf("Expected job failed, got %s", jobStatus)
	}

	// Stage should be failed
	var stageStatus string
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stageID).Scan(&stageStatus)
	if stageStatus != string(model.JobStageStatusFailed) {
		t.Errorf("Expected stage failed, got %s", stageStatus)
	}
}

func TestClaimNextJobSteps_SkipsBlockedStages(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_claim_blocked"
	stage0ID := uuid.New().String()
	stage1ID := uuid.New().String()
	step0ID := uuid.New().String()
	step1ID := uuid.New().String()
	now := time.Now()

	// Due a minute ago, not now: the claim compares next_run_at against the
	// DATABASE clock, and a fixture stamped from this process is a coin flip on
	// a database whose clock runs a few milliseconds behind - which is every
	// containerised one. The test is about blocked stages, not about the
	// instant a step becomes due.
	due := now.Add(-time.Minute)

	job := &model.Job{ID: jobID, Type: "test", Status: model.JobStatusRunning, Dedup: jobdedup.AlertUpdate(key), CreatedAt: now, UpdatedAt: now}
	stage0 := &model.JobStage{ID: stage0ID, JobID: jobID, StageIndex: 0, Status: model.JobStageStatusActive, CreatedAt: now, UpdatedAt: now}
	stage1 := &model.JobStage{ID: stage1ID, JobID: jobID, StageIndex: 1, Status: model.JobStageStatusBlocked, CreatedAt: now, UpdatedAt: now}
	step0 := &model.JobStep{ID: step0ID, JobID: jobID, StageID: stage0ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusPending, NextRunAt: &due, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}
	step1 := &model.JobStep{ID: step1ID, JobID: jobID, StageID: stage1ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusPending, NextRunAt: &due, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now}

	if _, err := s.CreateJobWithDedup(job, []*model.JobStage{stage0, stage1}, []*model.JobStep{step0, step1}); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	claimed, err := s.ClaimNextJobSteps(10, 30*time.Second)
	if err != nil {
		t.Fatalf("ClaimNextJobSteps: %v", err)
	}

	// Only step0 (active stage) should be claimed
	if len(claimed) != 1 {
		t.Fatalf("Expected 1 claimed step, got %d", len(claimed))
	}
	if claimed[0].ID != step0ID {
		t.Errorf("Expected step0 claimed, got %s", claimed[0].ID)
	}

	// step1 should still be pending with no locked_by
	var s1Status string
	var s1LockedBy *string
	s.GetDB().QueryRow("SELECT status, locked_by FROM job_steps WHERE id = $1", step1ID).Scan(&s1Status, &s1LockedBy)
	if s1Status != string(model.JobStepStatusPending) {
		t.Errorf("Step1 should still be pending, got %s", s1Status)
	}
	if s1LockedBy != nil {
		t.Errorf("Step1 should not be locked, got %v", *s1LockedBy)
	}
}

func TestFinishStepAndAdvance_StageTransitions(t *testing.T) {
	s := testutil.SetupDB(t)

	jobID := uuid.New().String()
	key := "dedup_stage_trans"
	stage0ID := uuid.New().String()
	stage1ID := uuid.New().String()
	stage2ID := uuid.New().String()
	step0ID := uuid.New().String()
	step1ID := uuid.New().String()
	step2ID := uuid.New().String()
	token0 := uuid.New().String()
	now := time.Now()

	job := &model.Job{ID: jobID, Type: "test", Status: model.JobStatusRunning, Dedup: jobdedup.AlertUpdate(key), CreatedAt: now, UpdatedAt: now}
	stages := []*model.JobStage{
		{ID: stage0ID, JobID: jobID, StageIndex: 0, Status: model.JobStageStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: stage1ID, JobID: jobID, StageIndex: 1, Status: model.JobStageStatusBlocked, CreatedAt: now, UpdatedAt: now},
		{ID: stage2ID, JobID: jobID, StageIndex: 2, Status: model.JobStageStatusBlocked, CreatedAt: now, UpdatedAt: now},
	}
	steps := []*model.JobStep{
		{ID: step0ID, JobID: jobID, StageID: stage0ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusRunning, LockedBy: &token0, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: step1ID, JobID: jobID, StageID: stage1ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusBlocked, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
		{ID: step2ID, JobID: jobID, StageID: stage2ID, StepIndex: 0, StepType: "test", Status: model.JobStepStatusBlocked, MaxAttempts: 3, CreatedAt: now, UpdatedAt: now},
	}

	if _, err := s.CreateJobWithDedup(job, stages, steps); err != nil {
		t.Fatalf("CreateJobWithDedup: %v", err)
	}

	// Finish step0 → stage0=succeeded, stage1=active
	res, err := s.FinishStepAndAdvance(step0ID, token0, model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("Finish step0: %v", err)
	}
	if res != model.AdvanceUnlockedNextStage {
		t.Errorf("Step0: expected UnlockedNextStage, got %d", res)
	}

	var st0, st1, st2 string
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage0ID).Scan(&st0)
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage1ID).Scan(&st1)
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage2ID).Scan(&st2)
	if st0 != "succeeded" {
		t.Errorf("Stage0 expected succeeded, got %s", st0)
	}
	if st1 != "active" {
		t.Errorf("Stage1 expected active, got %s", st1)
	}
	if st2 != "blocked" {
		t.Errorf("Stage2 expected blocked, got %s", st2)
	}

	// Claim step1 and finish → stage1=succeeded, stage2=active
	claimed, _ := s.ClaimNextJobSteps(1, 30*time.Second)
	if len(claimed) != 1 || claimed[0].ID != step1ID {
		t.Fatalf("Expected to claim step1, got %v", claimed)
	}
	token1 := *claimed[0].LockedBy

	res, err = s.FinishStepAndAdvance(step1ID, token1, model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("Finish step1: %v", err)
	}
	if res != model.AdvanceUnlockedNextStage {
		t.Errorf("Step1: expected UnlockedNextStage, got %d", res)
	}

	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage1ID).Scan(&st1)
	s.GetDB().QueryRow("SELECT status FROM job_stages WHERE id = $1", stage2ID).Scan(&st2)
	if st1 != "succeeded" {
		t.Errorf("Stage1 expected succeeded, got %s", st1)
	}
	if st2 != "active" {
		t.Errorf("Stage2 expected active, got %s", st2)
	}

	// Claim step2 and finish → job=succeeded
	claimed, _ = s.ClaimNextJobSteps(1, 30*time.Second)
	if len(claimed) != 1 || claimed[0].ID != step2ID {
		t.Fatalf("Expected to claim step2, got %v", claimed)
	}
	token2 := *claimed[0].LockedBy

	res, err = s.FinishStepAndAdvance(step2ID, token2, model.JobStepStatusSucceeded, "ok", "")
	if err != nil {
		t.Fatalf("Finish step2: %v", err)
	}
	if res != model.AdvanceJobFinished {
		t.Errorf("Step2: expected JobFinished, got %d", res)
	}

	var jobStatus string
	s.GetDB().QueryRow("SELECT status FROM jobs WHERE id = $1", jobID).Scan(&jobStatus)
	if jobStatus != "succeeded" {
		t.Errorf("Job expected succeeded, got %s", jobStatus)
	}
}
