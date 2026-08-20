package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
)

// createJobWithDedupTx inserts a job and its steps within the given transaction.
// Returns (jobID, created, error):
//   - (id, true, nil)  — job created
//   - (id, false, nil) — dedup conflict, returned existing job ID
//   - ("", false, err) — DB error
func (s *Store) createJobWithDedupTx(tx *sql.Tx, job *model.Job, stages []*model.JobStage, steps []*model.JobStep) (string, bool, error) {
	payloadBytes, err := json.Marshal(job.Payload)
	if err != nil {
		return "", false, fmt.Errorf("failed to marshal payload: %w", err)
	}
	if job.Payload == nil {
		payloadBytes = []byte("{}")
	}

	var jobID string
	query := `
		INSERT INTO jobs (id, type, status, payload, dedup_key, alert_group_id, current_stage, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (dedup_key) WHERE status IN ('pending', 'running') DO NOTHING
		RETURNING id`

	err = tx.QueryRow(query,
		job.ID, job.Type, job.Status, string(payloadBytes), job.DedupKey, job.AlertGroupID, job.CurrentStage,
		job.CreatedAt, job.UpdatedAt).Scan(&jobID)

	if err == sql.ErrNoRows {
		if job.DedupKey == nil {
			return "", false, fmt.Errorf("duplicate job check failed: dedup_key is nil")
		}
		err = tx.QueryRow(`SELECT id FROM jobs WHERE dedup_key = $1 AND status IN ('pending', 'running')`,
			job.DedupKey).Scan(&jobID)
		if err != nil {
			return "", false, fmt.Errorf("failed to fetch existing job: %w", err)
		}
		return jobID, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to insert job: %w", err)
	}

	// Insert stages
	for _, stage := range stages {
		_, err = tx.Exec(`
			INSERT INTO job_stages (id, job_id, stage_index, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			stage.ID, jobID, stage.StageIndex, stage.Status, stage.CreatedAt, stage.UpdatedAt)
		if err != nil {
			return "", false, fmt.Errorf("failed to insert stage %d: %w", stage.StageIndex, err)
		}
	}

	// Insert steps
	stmt, err := tx.Prepare(`
		INSERT INTO job_steps (
			id, job_id, stage_id, step_index, step_type, status, data, next_run_at,
			locked_by, locked_until,
			timeout_seconds, max_attempts, continue_on_failure, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`)
	if err != nil {
		return "", false, err
	}
	defer stmt.Close()

	for _, step := range steps {
		dataBytes, err := json.Marshal(step.Data)
		if err != nil {
			return "", false, fmt.Errorf("failed to marshal step data: %w", err)
		}
		if step.Data == nil {
			dataBytes = []byte("{}")
		}

		_, err = stmt.Exec(
			step.ID, jobID, step.StageID, step.StepIndex, step.StepType, step.Status, string(dataBytes),
			step.NextRunAt, step.LockedBy, step.LockedUntil,
			step.TimeoutSeconds, step.MaxAttempts, step.ContinueOnFailure,
			step.CreatedAt, step.UpdatedAt,
		)
		if err != nil {
			return "", false, fmt.Errorf("failed to insert step %d: %w", step.StepIndex, err)
		}
	}

	return jobID, true, nil
}

// CreateJobWithDedup creates a job and its steps atomically.
// If a job with the same dedup_key exists and is active, it returns that job's
// ID (idempotent) with created false.
func (s *Store) CreateJobWithDedup(job *model.Job, stages []*model.JobStage, steps []*model.JobStep) (string, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	jobID, created, err := s.createJobWithDedupTx(tx, job, stages, steps)
	if err != nil {
		return "", false, err
	}
	return jobID, created, tx.Commit()
}

// EnsureEscalationJob atomically transitions an AG from new/processing → processing
// and creates the escalation job + snapshot in a single transaction.
// Returns:
//   - (true, nil)  — job created, snapshot saved, AG → processing
//   - (false, nil) — dedup (job exists) OR AG already acked/resolved/triggered (user wins)
//   - (_, error)   — DB error, engine should retry
func (s *Store) EnsureEscalationJob(agID string, job *model.Job, stages []*model.JobStage, steps []*model.JobStep, snapshot *model.EscalationPolicySnapshot) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 1. Lock the AG row and read current status
	var status string
	err = tx.QueryRow(`SELECT status FROM alert_groups WHERE id = $1 FOR UPDATE`, agID).Scan(&status)
	if err != nil {
		return false, fmt.Errorf("failed to lock alert group %s: %w", agID, err)
	}

	// 2. Only new or processing are eligible
	agStatus := model.AlertGroupStatus(status)
	if agStatus != model.AlertGroupStatusNew && agStatus != model.AlertGroupStatusProcessing {
		// User already acked/resolved/triggered — they win
		return false, tx.Commit()
	}

	// 3. Transition to processing (or touch updated_at if already processing)
	now := time.Now()
	_, err = tx.Exec(`UPDATE alert_groups SET status = $1, updated_at = $2 WHERE id = $3`,
		model.AlertGroupStatusProcessing, now, agID)
	if err != nil {
		return false, fmt.Errorf("failed to update AG status: %w", err)
	}

	// 4. Validate: alert_group_id must match
	if job.AlertGroupID == nil || *job.AlertGroupID != agID {
		return false, fmt.Errorf("job.AlertGroupID must match agID")
	}

	// 5. Create escalation job — dedup by alert_group_id via unique index
	payloadBytes, err := json.Marshal(job.Payload)
	if err != nil {
		return false, fmt.Errorf("failed to marshal payload: %w", err)
	}
	if job.Payload == nil {
		payloadBytes = []byte("{}")
	}

	var jobID string
	err = tx.QueryRow(`
		INSERT INTO jobs (id, type, status, payload, dedup_key, alert_group_id, current_stage, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (alert_group_id) WHERE type = 'escalation' AND alert_group_id IS NOT NULL
		DO NOTHING
		RETURNING id`,
		job.ID, job.Type, job.Status, string(payloadBytes), job.DedupKey, job.AlertGroupID,
		job.CurrentStage, job.CreatedAt, job.UpdatedAt,
	).Scan(&jobID)

	if err == sql.ErrNoRows {
		// Escalation job already exists for this AG (any status) — don't create duplicate
		return false, tx.Commit()
	}
	if err != nil {
		return false, fmt.Errorf("failed to insert escalation job: %w", err)
	}

	// 6. Insert stages
	for _, stage := range stages {
		_, err = tx.Exec(`
			INSERT INTO job_stages (id, job_id, stage_index, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			stage.ID, jobID, stage.StageIndex, stage.Status, stage.CreatedAt, stage.UpdatedAt)
		if err != nil {
			return false, fmt.Errorf("failed to insert stage %d: %w", stage.StageIndex, err)
		}
	}

	// 7. Insert steps
	stmt, err := tx.Prepare(`
		INSERT INTO job_steps (
			id, job_id, stage_id, step_index, step_type, status, data, next_run_at,
			locked_by, locked_until,
			timeout_seconds, max_attempts, continue_on_failure, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()

	for _, step := range steps {
		dataBytes, err := json.Marshal(step.Data)
		if err != nil {
			return false, fmt.Errorf("failed to marshal step data: %w", err)
		}
		if step.Data == nil {
			dataBytes = []byte("{}")
		}

		_, err = stmt.Exec(
			step.ID, jobID, step.StageID, step.StepIndex, step.StepType, step.Status, string(dataBytes),
			step.NextRunAt, step.LockedBy, step.LockedUntil,
			step.TimeoutSeconds, step.MaxAttempts, step.ContinueOnFailure,
			step.CreatedAt, step.UpdatedAt,
		)
		if err != nil {
			return false, fmt.Errorf("failed to insert step %d: %w", step.StepIndex, err)
		}
	}

	// 8. Job created — save policy snapshot
	var snapshotVal sql.NullString
	if snapshot != nil {
		data, _ := json.Marshal(snapshot)
		snapshotVal = sql.NullString{String: string(data), Valid: true}
	}
	policyID := ""
	if snapshot != nil {
		policyID = snapshot.PolicyID
	}
	_, err = tx.Exec(`UPDATE alert_groups SET policy_id = $1, policy_snapshot = $2 WHERE id = $3`,
		policyID, snapshotVal, agID)
	if err != nil {
		return false, fmt.Errorf("failed to save policy snapshot: %w", err)
	}

	return true, tx.Commit()
}

// GetJobByID fetches a job by ID
func (s *Store) GetJobByID(id string) (*model.Job, error) {
	job := &model.Job{}
	var payloadStr string
	var statusStr string

	err := s.db.QueryRow(`
		SELECT id, type, status, payload, dedup_key, alert_group_id, current_stage, error, created_at, updated_at, finished_at, canceled_at
		FROM jobs WHERE id = $1`, id).Scan(
		&job.ID, &job.Type, &statusStr, &payloadStr, &job.DedupKey, &job.AlertGroupID, &job.CurrentStage,
		&job.Error, &job.CreatedAt, &job.UpdatedAt, &job.FinishedAt, &job.CanceledAt,
	)
	if err != nil {
		return nil, err
	}
	job.Status = model.JobStatus(statusStr)
	job.Payload = json.RawMessage(payloadStr)
	return job, nil
}

// GetJobByDedupKey fetches the latest job by dedup key
func (s *Store) GetJobByDedupKey(dedupKey string) (*model.Job, error) {
	job := &model.Job{}
	var payloadStr string
	var statusStr string

	err := s.db.QueryRow(`
		SELECT id, type, status, payload, dedup_key, alert_group_id, current_stage, error, created_at, updated_at, finished_at, canceled_at
		FROM jobs WHERE dedup_key = $1 ORDER BY created_at DESC LIMIT 1`, dedupKey).Scan(
		&job.ID, &job.Type, &statusStr, &payloadStr, &job.DedupKey, &job.AlertGroupID, &job.CurrentStage,
		&job.Error, &job.CreatedAt, &job.UpdatedAt, &job.FinishedAt, &job.CanceledAt,
	)
	if err != nil {
		return nil, err
	}
	job.Status = model.JobStatus(statusStr)
	job.Payload = json.RawMessage(payloadStr)
	return job, nil
}

// GetJobStepByID fetches a specific step by its unique ID
func (s *Store) GetJobStepByID(stepID string) (*model.JobStep, error) {
	step := &model.JobStep{}
	var dataStr, statusStr string
	err := s.db.QueryRow(`
		SELECT id, job_id, stage_id, step_index, step_type, status, data, next_run_at,
			   timeout_seconds, max_attempts, continue_on_failure, created_at, updated_at
		FROM job_steps WHERE id = $1`, stepID).Scan(
		&step.ID, &step.JobID, &step.StageID, &step.StepIndex, &step.StepType, &statusStr, &dataStr,
		&step.NextRunAt, &step.TimeoutSeconds, &step.MaxAttempts, &step.ContinueOnFailure,
		&step.CreatedAt, &step.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	step.Status = model.JobStepStatus(statusStr)
	step.Data = json.RawMessage(dataStr)
	return step, nil
}

// ClaimNextJobSteps locks and returns pending steps ready for execution. The
// lease token is generated in-DB (locked_by = gen_random_uuid) and returned on
// each step as LockedBy; callers don't supply one.
func (s *Store) ClaimNextJobSteps(limit int, duration time.Duration) ([]*model.JobStep, error) {
	query := `
		WITH claimed_steps AS (
			SELECT js.id
			FROM job_steps js
			JOIN job_stages jst ON js.stage_id = jst.id AND jst.status = 'active'
			JOIN jobs j ON jst.job_id = j.id AND j.status IN ('pending', 'running')
			WHERE (js.next_run_at IS NULL OR js.next_run_at <= NOW())
			  AND (
			      (js.status IN ('pending', 'retry') AND (js.locked_until IS NULL OR js.locked_until < NOW()))
			      OR
			      (js.status = 'running' AND (js.locked_until < NOW() OR js.locked_until IS NULL))
			  )
			ORDER BY js.next_run_at ASC NULLS FIRST
			LIMIT $1
			FOR UPDATE OF js SKIP LOCKED
		)
		UPDATE job_steps
		SET status = 'running',
			locked_by = gen_random_uuid()::text,
			locked_until = NOW() + $2 * interval '1 second',
			updated_at = NOW()
		WHERE id IN (SELECT id FROM claimed_steps)
		RETURNING id, job_id, stage_id, step_index, step_type, status, data, next_run_at,
				  locked_until, locked_by, attempt_count, timeout_seconds, max_attempts,
				  continue_on_failure, created_at, updated_at;
	`
	rows, err := s.db.Query(query, limit, duration.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []*model.JobStep
	for rows.Next() {
		step := &model.JobStep{}
		var dataStr string
		var statusStr string
		err := rows.Scan(
			&step.ID, &step.JobID, &step.StageID, &step.StepIndex, &step.StepType, &statusStr, &dataStr,
			&step.NextRunAt, &step.LockedUntil, &step.LockedBy, &step.AttemptCount,
			&step.TimeoutSeconds, &step.MaxAttempts, &step.ContinueOnFailure,
			&step.CreatedAt, &step.UpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		step.Status = model.JobStepStatus(statusStr)
		step.Data = json.RawMessage(dataStr)
		steps = append(steps, step)
	}

	// Also ensure parent jobs are marked as 'running' if they were 'pending'
	if len(steps) > 0 {
		jobIDs := make([]string, len(steps))
		for i, s := range steps {
			jobIDs[i] = s.JobID
		}
		// Batch update job status (best effort)
		_, _ = s.db.Exec(`UPDATE jobs SET status = 'running', updated_at = NOW() WHERE id = ANY($1) AND status = 'pending'`,
			pq.Array(jobIDs))
	}

	return steps, nil
}

// UpdateJobStepIfOwned updates a step only if the lease token matches (CAS).
// Returns (true, nil) if updated, (false, nil) if lease lost.
func (s *Store) UpdateJobStepIfOwned(step *model.JobStep, leaseToken string) (bool, error) {
	resultBytes, _ := json.Marshal(step.Result)
	if step.Result == nil {
		resultBytes = []byte("null")
	}

	res, err := s.db.Exec(`
		UPDATE job_steps
		SET status = $1, result = $2, error = $3, locked_until = $4,
		    next_run_at = $5, attempt_count = $6, updated_at = NOW()
		WHERE id = $7 AND locked_by = $8`,
		step.Status, string(resultBytes), step.Error, step.LockedUntil,
		step.NextRunAt, step.AttemptCount, step.ID, leaseToken,
	)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// FinishStepAndAdvance atomically finalizes a step and advances the job.
// Single TX, lock order: job → stage → step (matches CancelEscalationJobByAlertGroupID).
func (s *Store) FinishStepAndAdvance(
	stepID string,
	leaseToken string,
	outcome model.JobStepStatus,
	result string,
	stepError string,
) (model.AdvanceResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 0. Load step metadata (stage_id, job_id, continue_on_failure) — no locks
	var stageID, jobID string
	var continueOnFailure bool
	err = tx.QueryRow(`
		SELECT js.stage_id, jst.job_id, js.continue_on_failure
		FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		WHERE js.id = $1`, stepID).Scan(&stageID, &jobID, &continueOnFailure)
	if err != nil {
		return 0, fmt.Errorf("step %s not found: %w", stepID, err)
	}

	// 1. FOR UPDATE on job row — first lock
	var jobStatus string
	err = tx.QueryRow(`SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&jobStatus)
	if err != nil {
		return 0, fmt.Errorf("job %s not found: %w", jobID, err)
	}
	if jobStatus != string(model.JobStatusPending) && jobStatus != string(model.JobStatusRunning) {
		// Best-effort: cancel our step so it doesn't stay as 'running'
		tx.Exec(`UPDATE job_steps SET status = 'canceled', locked_until = NULL, updated_at = NOW()
		          WHERE id = $1 AND locked_by = $2`, stepID, leaseToken)
		tx.Commit()
		return model.AdvanceJobAlreadyTerminal, nil
	}

	// 2. FOR UPDATE on stage row — second lock
	var stageStatus string
	var stageIndex int
	err = tx.QueryRow(`SELECT status, stage_index FROM job_stages WHERE id = $1 FOR UPDATE`,
		stageID).Scan(&stageStatus, &stageIndex)
	if err != nil {
		return 0, fmt.Errorf("stage %s not found: %w", stageID, err)
	}
	if stageStatus == string(model.JobStageStatusSucceeded) ||
		stageStatus == string(model.JobStageStatusFailed) ||
		stageStatus == string(model.JobStageStatusCanceled) {
		return model.AdvanceAlreadyAdvanced, tx.Commit()
	}

	// 3. CAS: finalize step only if lease is ours — third lock
	var resultVal *string
	if result != "" {
		quoted := fmt.Sprintf("%q", result)
		resultVal = &quoted
	}
	res, err := tx.Exec(`
		UPDATE job_steps
		SET status = $1, result = $2, error = $3, locked_until = NULL, updated_at = NOW()
		WHERE id = $4 AND locked_by = $5 AND status = 'running'`,
		outcome, resultVal, nilIfEmpty(stepError), stepID, leaseToken)
	if err != nil {
		return 0, fmt.Errorf("failed to finalize step: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return model.AdvanceLeaseLost, tx.Commit()
	}

	// 4. Hard-fail: step failed without ContinueOnFailure → fail job immediately
	if outcome == model.JobStepStatusFailed && !continueOnFailure {
		tx.Exec(`UPDATE job_stages SET status = 'failed', updated_at = NOW() WHERE id = $1`, stageID)
		tx.Exec(`UPDATE jobs SET status = 'failed', error = $1, finished_at = NOW(), updated_at = NOW()
		          WHERE id = $2`, nilIfEmpty(stepError), jobID)
		return model.AdvanceJobFinished, tx.Commit()
	}

	// 5. Pending siblings in this stage?
	var pendingCount int
	err = tx.QueryRow(`
		SELECT COUNT(*) FROM job_steps
		WHERE stage_id = $1 AND status NOT IN ('succeeded', 'failed', 'canceled')`,
		stageID).Scan(&pendingCount)
	if err != nil {
		return 0, err
	}
	if pendingCount > 0 {
		return model.AdvanceWaitingSiblings, tx.Commit()
	}

	// 6. Check hard-fail siblings (another sibling failed + !continue_on_failure)
	var hasHardFail bool
	err = tx.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM job_steps
			WHERE stage_id = $1 AND status = 'failed' AND continue_on_failure = false
		)`, stageID).Scan(&hasHardFail)
	if err != nil {
		return 0, err
	}
	if hasHardFail {
		tx.Exec(`UPDATE job_stages SET status = 'failed', updated_at = NOW() WHERE id = $1`, stageID)
		tx.Exec(`UPDATE jobs SET status = 'failed', error = 'step failed', finished_at = NOW(), updated_at = NOW()
		          WHERE id = $1`, jobID)
		return model.AdvanceJobFinished, tx.Commit()
	}

	// 7. Stage completed — determine status
	var hasAnyFailed bool
	tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM job_steps WHERE stage_id = $1 AND status = 'failed')`,
		stageID).Scan(&hasAnyFailed)
	stageResult := model.JobStageStatusSucceeded
	if hasAnyFailed {
		stageResult = model.JobStageStatusFailed
	}
	tx.Exec(`UPDATE job_stages SET status = $1, updated_at = NOW() WHERE id = $2`, stageResult, stageID)

	// 8. Next blocked stage?
	var nextStageID string
	err = tx.QueryRow(`
		SELECT id FROM job_stages
		WHERE job_id = $1 AND stage_index = $2 AND status = 'blocked'`,
		jobID, stageIndex+1).Scan(&nextStageID)

	if err == sql.ErrNoRows {
		// Last stage — finish job
		var jobHasFailed bool
		tx.QueryRow(`
			SELECT EXISTS (SELECT 1 FROM job_steps WHERE stage_id IN
				(SELECT id FROM job_stages WHERE job_id = $1) AND status = 'failed')`,
			jobID).Scan(&jobHasFailed)
		finalStatus := model.JobStatusSucceeded
		if jobHasFailed {
			finalStatus = model.JobStatusFailed
		}
		tx.Exec(`UPDATE jobs SET status = $1, finished_at = NOW(), updated_at = NOW() WHERE id = $2`,
			finalStatus, jobID)
		return model.AdvanceJobFinished, tx.Commit()
	}
	if err != nil {
		return 0, fmt.Errorf("failed to find next stage: %w", err)
	}

	// 9. Unlock next stage and its steps (with per-step delay)
	tx.Exec(`UPDATE job_stages SET status = 'active', updated_at = NOW() WHERE id = $1`, nextStageID)

	nextStepRows, err := tx.Query(`SELECT id, data FROM job_steps WHERE stage_id = $1 AND status = 'blocked'`, nextStageID)
	if err != nil {
		return 0, err
	}
	defer nextStepRows.Close()

	type pendingStep struct {
		id   string
		data string
	}
	var pendingSteps []pendingStep
	for nextStepRows.Next() {
		var ps pendingStep
		if err := nextStepRows.Scan(&ps.id, &ps.data); err != nil {
			return 0, err
		}
		pendingSteps = append(pendingSteps, ps)
	}
	nextStepRows.Close()

	for _, ps := range pendingSteps {
		var stepData model.LegacyStepData
		_ = json.Unmarshal([]byte(ps.data), &stepData)
		tx.Exec(`
			UPDATE job_steps
			SET status = 'pending', next_run_at = NOW() + $1 * interval '1 second', updated_at = NOW()
			WHERE id = $2 AND status = 'blocked'`,
			stepData.DelaySeconds, ps.id)
	}

	tx.Exec(`UPDATE jobs SET current_stage = $1, updated_at = NOW() WHERE id = $2`,
		stageIndex+1, jobID)

	return model.AdvanceUnlockedNextStage, tx.Commit()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CancelEscalationJobByAlertGroupID cancels the escalation job of one alert
// group - the job, its active stages and its pending steps - in one transaction.
//
// The body lives in cancelEscalationJobByAlertGroupIDTx, which the atomic alert
// group transitions call from inside their own transaction. Before Epic 11 the
// two were separate near-copies in separate files, alike in meaning and
// drifting in form; there is one cancellation, so there is one implementation.
func (s *Store) CancelEscalationJobByAlertGroupID(alertGroupID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := cancelEscalationJobByAlertGroupIDTx(tx, alertGroupID); err != nil {
		return err
	}
	return tx.Commit()
}

// ExtendStepLease extends the lock on a running step
func (s *Store) ExtendStepLease(stepID string, leaseToken string, duration time.Duration) error {
	res, err := s.db.Exec(`
		UPDATE job_steps 
		SET locked_until = NOW() + $1 * interval '1 second'
		WHERE id = $2 AND locked_by = $3 AND status = 'running'`,
		duration.Seconds(), stepID, leaseToken)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("step %s not found or lost lock", stepID)
	}
	return nil
}

// GetJobsByIDs fetches multiple jobs
func (s *Store) GetJobsByIDs(ids []string) (map[string]*model.Job, error) {
	rows, err := s.db.Query(`
		SELECT id, type, status, payload, dedup_key, alert_group_id, current_stage, error, created_at
		FROM jobs WHERE id = ANY($1)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]*model.Job)
	for rows.Next() {
		job := &model.Job{}
		var payloadStr, statusStr string
		err := rows.Scan(
			&job.ID, &job.Type, &statusStr, &payloadStr, &job.DedupKey, &job.AlertGroupID, &job.CurrentStage,
			&job.Error, &job.CreatedAt,
		)
		if err != nil {
			return nil, err
		}
		job.Status = model.JobStatus(statusStr)
		job.Payload = json.RawMessage(payloadStr)
		result[job.ID] = job
	}
	return result, nil
}

// FailJob marks a job as failed with an error message
func (s *Store) FailJob(jobID string, errorMsg string) error {
	_, err := s.db.Exec(`UPDATE jobs SET status = 'failed', error = $1, finished_at = NOW(), updated_at = NOW() WHERE id = $2`, errorMsg, jobID)
	return err
}
