package model

import (
	"encoding/json"
	"time"

	"github.com/tokayops/tokayops/internal/jobdedup"
)

// JobStatus represents job lifecycle state
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCanceled  JobStatus = "canceled"
)

// JobStepStatus represents step lifecycle state
type JobStepStatus string

const (
	JobStepStatusPending   JobStepStatus = "pending"
	JobStepStatusRunning   JobStepStatus = "running"
	JobStepStatusSucceeded JobStepStatus = "succeeded"
	JobStepStatusFailed    JobStepStatus = "failed"
	JobStepStatusBlocked   JobStepStatus = "blocked"
	JobStepStatusCanceled  JobStepStatus = "canceled"
	JobStepStatusRetry     JobStepStatus = "retry"
)

// JobStageStatus represents stage lifecycle state
type JobStageStatus string

const (
	JobStageStatusBlocked   JobStageStatus = "blocked"
	JobStageStatusActive    JobStageStatus = "active"
	JobStageStatusSucceeded JobStageStatus = "succeeded"
	JobStageStatusFailed    JobStageStatus = "failed"
	JobStageStatusCanceled  JobStageStatus = "canceled"
)

// JobStage represents a sequential execution group within a job.
// Steps within a stage execute in parallel; stages execute sequentially.
type JobStage struct {
	ID         string         `json:"id" db:"id"`
	JobID      string         `json:"job_id" db:"job_id"`
	StageIndex int            `json:"stage_index" db:"stage_index"`
	Status     JobStageStatus `json:"status" db:"status"`
	CreatedAt  time.Time      `json:"created_at" db:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at" db:"updated_at"`
}

// AdvanceResult represents the outcome of FinishStepAndAdvance
type AdvanceResult int

const (
	AdvanceWaitingSiblings    AdvanceResult = iota // siblings in stage still working
	AdvanceUnlockedNextStage                       // next stage unlocked
	AdvanceJobFinished                             // last stage done or hard-fail, job terminated
	AdvanceJobAlreadyTerminal                      // job already failed/canceled before we ran
	AdvanceAlreadyAdvanced                         // stage already completed by another worker
	AdvanceLeaseLost                               // lease expired, step not updated
)

// Job represents a background execution unit (e.g., escalation workflow)
type Job struct {
	ID           string          `json:"id" db:"id"`
	Type         string          `json:"type" db:"type"`
	Status       JobStatus       `json:"status" db:"status"`
	Payload      json.RawMessage `json:"payload" db:"payload"`
	Dedup        *jobdedup.Spec  `json:"dedup"`
	AlertGroupID *string         `json:"alert_group_id" db:"alert_group_id"`
	CurrentStage int             `json:"current_stage" db:"current_stage"`
	Error        *string         `json:"error" db:"error"`
	CreatedAt    time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at" db:"updated_at"`
	FinishedAt   *time.Time      `json:"finished_at" db:"finished_at"`
	CanceledAt   *time.Time      `json:"canceled_at" db:"canceled_at"`
}

// JobStep represents a single execution step within a job
type JobStep struct {
	ID           string          `json:"id" db:"id"`
	JobID        string          `json:"job_id" db:"job_id"`
	StageID      string          `json:"stage_id" db:"stage_id"`
	StepIndex    int             `json:"step_index" db:"step_index"`
	StepType     string          `json:"step_type" db:"step_type"`
	Status       JobStepStatus   `json:"status" db:"status"`
	Data         json.RawMessage `json:"data" db:"data"`
	Result       json.RawMessage `json:"result,omitempty" db:"result"`
	Error        *string         `json:"error,omitempty" db:"error"`
	NextRunAt    *time.Time      `json:"next_run_at" db:"next_run_at"`
	LockedUntil  *time.Time      `json:"locked_until" db:"locked_until"`
	LockedBy     *string         `json:"locked_by" db:"locked_by"`
	AttemptCount int             `json:"attempt_count" db:"attempt_count"`

	// Config snapshots (denormalized to run without config)
	TimeoutSeconds    *int `json:"timeout_seconds" db:"timeout_seconds"`
	MaxAttempts       int  `json:"max_attempts" db:"max_attempts"`
	ContinueOnFailure bool `json:"continue_on_failure" db:"continue_on_failure"` // If true, unlock next step on failure instead of failing job

	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
}

// EscalationPayload is the payload for 'escalation' job type
type EscalationPayload struct {
	AlertGroupID   string                    `json:"alert_group_id"`
	PolicyID       string                    `json:"policy_id"`
	PolicySnapshot *EscalationPolicySnapshot `json:"policy_snapshot,omitempty"` // Snapshot at job creation time
}

// EscalationPolicySnapshot stores policy data at job creation time
// Used for resolution even if policy is later deleted
type EscalationPolicySnapshot struct {
	PolicyID string                    `json:"policy_id,omitempty"` // Effective policy ID (empty if firehose-only fallback)
	Name     string                    `json:"name"`
	Steps    []*EscalationStepSnapshot `json:"steps"`
}

// EscalationStepSnapshot stores step data at job creation time. The pair
// (Provider, TargetKind) mirrors EscalationStep — see Sprint 4 (Epic 7 L6).
type EscalationStepSnapshot struct {
	Provider   string `json:"provider"`
	TargetKind string `json:"target_kind"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	IsFirehose bool   `json:"is_firehose,omitempty"` // true for firehose step
	StageIndex int    `json:"stage_index"`           // which stage this step belongs to
}

// LegacyStepData is the generic structure for step data (Deprecated: use specific StepData types)
type LegacyStepData struct {
	TargetID     string `json:"target_id"`
	Message      string `json:"message"`
	AlertGroupID string `json:"alert_group_id"`
	DelaySeconds int    `json:"delay_seconds"`
}
