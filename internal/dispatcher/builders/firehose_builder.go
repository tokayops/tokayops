package builders

import (
	"encoding/json"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/google/uuid"
)

// FirehoseJobBuilder creates standalone firehose notification jobs.
// This runs independently of escalation policies, ensuring firehose
// notifications are sent even when no policy is configured for a team.
//
// Firehose is intentionally Slack-only (Sprint 4 / Epic 7 L7): the dual-send
// to global L2 Slack channels is a Slack-specific operational pattern, not a
// channel concept that generalizes. Parameterizing it is a future feature,
// not a missing generalization to fix.
type FirehoseJobBuilder struct {
	Config *config.Config
}

func NewFirehoseJobBuilder(cfg *config.Config) *FirehoseJobBuilder {
	return &FirehoseJobBuilder{Config: cfg}
}

// Build creates a firehose job for the given alert group.
// Returns nil, nil, nil if firehose is not configured (no channel set).
func (b *FirehoseJobBuilder) Build(ag *model.AlertGroup) (*model.Job, []*model.JobStage, []*model.JobStep, error) {
	// Determine firehose channel based on severity
	firehoseChan := b.Config.Global.FirehoseWarningChannel
	if ag.Severity == "critical" {
		firehoseChan = b.Config.Global.FirehoseCriticalChannel
	}

	// No firehose configured - return nil (not an error)
	if firehoseChan == "" {
		return nil, nil, nil, nil
	}

	jobID := uuid.New().String()
	now := time.Now()

	// Create job with firehose-specific dedup key
	dedupKey := "firehose_" + ag.DedupKey
	agID := ag.ID
	job := &model.Job{
		ID:           jobID,
		Type:         "escalation",
		Status:       model.JobStatusPending,
		DedupKey:     &dedupKey,
		AlertGroupID: &agID,
		CurrentStage: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// Create payload
	payload := model.EscalationPayload{
		AlertGroupID: ag.ID,
		PolicyID:     "", // Firehose has no policy
	}
	payloadBytes, _ := json.Marshal(payload)
	job.Payload = json.RawMessage(payloadBytes)

	// Create single firehose step. ProviderName is explicit Slack here
	// (matching escalation_builder.go's firehoseProvider constant); the
	// executor requires an explicit provider and never infers one.
	stepData := model.EscalationStepData{
		TargetID:     firehoseChan,
		ProviderName: "slack",
		AlertGroupID: ag.ID,
		IsFirehose:   true,
		PolicyID:     "",
	}
	dataBytes, _ := json.Marshal(stepData)
	timeout := 30

	step := &model.JobStep{
		ID:             uuid.New().String(),
		JobID:          jobID,
		StepIndex:      0,
		StepType:       "firehose",
		Status:         model.JobStepStatusPending,
		Data:           json.RawMessage(dataBytes),
		NextRunAt:      &now,
		TimeoutSeconds: &timeout,
		MaxAttempts:    3,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	steps := []*model.JobStep{step}
	stages := WrapStepsInStages(jobID, steps, now)
	return job, stages, steps, nil
}
