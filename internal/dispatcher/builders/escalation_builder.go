package builders

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduler"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/google/uuid"
)

// EscalationJobBuilder builds escalation jobs from Store-based policies
type EscalationJobBuilder struct {
	Store  store.StoreInterface
	Config *config.Config // for firehose channel config
}

func NewEscalationJobBuilder(s store.StoreInterface, cfg *config.Config) *EscalationJobBuilder {
	return &EscalationJobBuilder{Store: s, Config: cfg}
}

// Sprint 4 (Epic 7 L6): the legacy providerForStepType helper is gone —
// each EscalationStep already carries its own Provider + TargetKind, so the
// builder just propagates them. The runtime JobStep.StepType is set to
// TargetKind ("dm" / "channel"); the executor switches on that.
//
// Firehose stays the lone exception: a runtime sentinel StepType="firehose"
// with ProviderName="slack" hardcoded. Firehose is intentionally Slack-only
// (see escalation_executor.go); parameterizing it is a future feature.
const firehoseProvider = "slack"

func (b *EscalationJobBuilder) Build(ag *model.AlertGroup, policyID string) (*model.Job, []*model.JobStage, []*model.JobStep, *model.EscalationPolicySnapshot, error) {
	// Determine firehose channel based on severity
	firehoseChan := ""
	if b.Config != nil {
		firehoseChan = b.Config.Global.FirehoseWarningChannel
		if ag.Severity == "critical" {
			firehoseChan = b.Config.Global.FirehoseCriticalChannel
		}
	}
	hasFirehose := firehoseChan != ""

	// Get policy from store (optional - may be nil for firehose-only)
	var policy *model.EscalationPolicy
	if policyID != "" {
		var err error
		policy, err = b.Store.GetEscalationPolicyByID(policyID)
		if err != nil {
			log.Printf("EscalationBuilder: Policy %s not found (err: %v), falling back to firehose-only", policyID, err)
			policy = nil
			policyID = ""
		}
	}

	// Early exit if nothing to do
	if !hasFirehose && policy == nil {
		return nil, nil, nil, nil, nil
	}

	jobID := uuid.New().String()
	dedupKey := ag.DedupKey
	now := time.Now()

	snapshot := &model.EscalationPolicySnapshot{
		PolicyID: policyID,
	}
	if policy != nil {
		snapshot.Name = policy.Name
	}

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

	var allStages []*model.JobStage
	var allSteps []*model.JobStep
	var snapshotSteps []*model.EscalationStepSnapshot
	stageIndex := 0

	// Helper to create a stage
	newStage := func() *model.JobStage {
		status := model.JobStageStatusBlocked
		if stageIndex == 0 {
			status = model.JobStageStatusActive
		}
		return &model.JobStage{
			ID:         uuid.New().String(),
			JobID:      jobID,
			StageIndex: stageIndex,
			Status:     status,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
	}

	// Step status: stage 0 steps are pending, others blocked
	stepStatusFor := func(si int) (model.JobStepStatus, *time.Time) {
		if si == 0 {
			return model.JobStepStatusPending, &now
		}
		return model.JobStepStatusBlocked, nil
	}

	// Firehose stage (if configured)
	if hasFirehose {
		stage := newStage()
		status, nextRunAt := stepStatusFor(stageIndex)

		firehoseData := model.EscalationStepData{
			TargetID:     firehoseChan,
			ProviderName: firehoseProvider, // intentional Slack-only sentinel
			AlertGroupID: ag.ID,
			PolicyID:     policyID,
			IsFirehose:   true,
		}
		dataBytes, _ := json.Marshal(firehoseData)
		timeout := 30

		allSteps = append(allSteps, &model.JobStep{
			ID:                uuid.New().String(),
			JobID:             jobID,
			StageID:           stage.ID,
			StepIndex:         0,
			StepType:          "firehose",
			Status:            status,
			Data:              json.RawMessage(dataBytes),
			NextRunAt:         nextRunAt,
			TimeoutSeconds:    &timeout,
			MaxAttempts:       3,
			ContinueOnFailure: true,
			CreatedAt:         now,
			UpdatedAt:         now,
		})
		allStages = append(allStages, stage)
		snapshotSteps = append(snapshotSteps, &model.EscalationStepSnapshot{
			Provider:   firehoseProvider,
			TargetKind: "channel",
			TargetType: "channel",
			TargetID:   firehoseChan,
			IsFirehose: true,
			StageIndex: stageIndex,
		})
		stageIndex++
	}

	// Policy steps
	if policy != nil {
		for _, stepCfg := range policy.Steps {
			if stepCfg.TargetID == "" {
				return nil, nil, nil, nil, fmt.Errorf("policy %s step missing target", policyID)
			}

			timeoutSec := stepCfg.TimeoutSeconds
			if timeoutSec == 0 {
				timeoutSec = 30
			}
			maxAttempts := stepCfg.MaxAttempts
			if maxAttempts <= 0 {
				maxAttempts = 5
			}

			if stepCfg.TargetType == "schedule" {
				// Fan-out: resolve schedule → parallel steps in one stage
				stage := newStage()
				status, nextRunAt := stepStatusFor(stageIndex)

				users, resolveErr := b.resolveScheduleUsers(ag.TeamID, stepCfg.TargetID)
				if resolveErr != nil {
					log.Printf("EscalationBuilder: Failed to resolve schedule %s: %v", stepCfg.TargetID, resolveErr)
					// Create a single step that will fail gracefully
					users = nil
				}

				if len(users) == 0 {
					// No on-call users — create marker step that executor will fail
					stepData := model.EscalationStepData{
						TargetID:     "",
						TargetType:   "user",
						ProviderName: stepCfg.Provider,
						AlertGroupID: ag.ID,
						PolicyID:     policyID,
						DelaySeconds: stepCfg.DelaySeconds,
						Message:      stepCfg.Message,
					}
					dataBytes, _ := json.Marshal(stepData)
					allSteps = append(allSteps, &model.JobStep{
						ID:                uuid.New().String(),
						JobID:             jobID,
						StageID:           stage.ID,
						StepIndex:         0,
						StepType:          stepCfg.TargetKind,
						Status:            status,
						Data:              json.RawMessage(dataBytes),
						NextRunAt:         nextRunAt,
						TimeoutSeconds:    &timeoutSec,
						MaxAttempts:       1,
						ContinueOnFailure: true,
						CreatedAt:         now,
						UpdatedAt:         now,
					})
					snapshotSteps = append(snapshotSteps, &model.EscalationStepSnapshot{
						Provider:   stepCfg.Provider,
						TargetKind: stepCfg.TargetKind,
						TargetType: "user",
						TargetID:   "",
						StageIndex: stageIndex,
					})
				} else {
					for i, user := range users {
						stepData := model.EscalationStepData{
							TargetID:     user.ID,
							TargetType:   "user",
							ProviderName: stepCfg.Provider,
							AlertGroupID: ag.ID,
							PolicyID:     policyID,
							DelaySeconds: stepCfg.DelaySeconds,
							Message:      stepCfg.Message,
						}
						dataBytes, _ := json.Marshal(stepData)
						allSteps = append(allSteps, &model.JobStep{
							ID:                uuid.New().String(),
							JobID:             jobID,
							StageID:           stage.ID,
							StepIndex:         i,
							StepType:          stepCfg.TargetKind,
							Status:            status,
							Data:              json.RawMessage(dataBytes),
							NextRunAt:         nextRunAt,
							TimeoutSeconds:    &timeoutSec,
							MaxAttempts:       maxAttempts,
							ContinueOnFailure: true, // parallel steps must not block siblings
							CreatedAt:         now,
							UpdatedAt:         now,
						})
						snapshotSteps = append(snapshotSteps, &model.EscalationStepSnapshot{
							Provider:   stepCfg.Provider,
							TargetKind: stepCfg.TargetKind,
							TargetType: "user",
							TargetID:   user.ID,
							StageIndex: stageIndex,
						})
					}
				}
				allStages = append(allStages, stage)
			} else {
				// Non-schedule: single step in its own stage
				stage := newStage()
				status, nextRunAt := stepStatusFor(stageIndex)

				stepData := model.EscalationStepData{
					TargetID:     stepCfg.TargetID,
					TargetType:   stepCfg.TargetType,
					ProviderName: stepCfg.Provider,
					AlertGroupID: ag.ID,
					PolicyID:     policyID,
					DelaySeconds: stepCfg.DelaySeconds,
					Message:      stepCfg.Message,
				}
				dataBytes, _ := json.Marshal(stepData)

				allSteps = append(allSteps, &model.JobStep{
					ID:                uuid.New().String(),
					JobID:             jobID,
					StageID:           stage.ID,
					StepIndex:         0,
					StepType:          stepCfg.TargetKind,
					Status:            status,
					Data:              json.RawMessage(dataBytes),
					NextRunAt:         nextRunAt,
					TimeoutSeconds:    &timeoutSec,
					MaxAttempts:       maxAttempts,
					ContinueOnFailure: stepCfg.ContinueOnFailure,
					CreatedAt:         now,
					UpdatedAt:         now,
				})
				allStages = append(allStages, stage)
				snapshotSteps = append(snapshotSteps, &model.EscalationStepSnapshot{
					Provider:   stepCfg.Provider,
					TargetKind: stepCfg.TargetKind,
					TargetType: stepCfg.TargetType,
					TargetID:   stepCfg.TargetID,
					StageIndex: stageIndex,
				})
			}
			stageIndex++
		}
	}

	snapshot.Steps = snapshotSteps

	// Set payload with snapshot
	payload := model.EscalationPayload{
		AlertGroupID:   ag.ID,
		PolicyID:       policyID,
		PolicySnapshot: snapshot,
	}
	payloadBytes, _ := json.Marshal(payload)
	job.Payload = json.RawMessage(payloadBytes)

	return job, allStages, allSteps, snapshot, nil
}

// resolveScheduleUsers resolves the team's current schedule to on-call users.
// It uses GetScheduleByTeamID to find the active schedule, falling back to
// the provided scheduleID if the team has no schedule configured.
// Override replaces the entire L1 group. Only L1 (or override) is returned —
// L2 rotation is currently not exposed as an escalation target.
func (b *EscalationJobBuilder) resolveScheduleUsers(teamID, fallbackScheduleID string) ([]*model.User, error) {
	scheduleID := fallbackScheduleID
	if teamID != "" {
		sched, err := b.Store.GetScheduleByTeamID(teamID)
		switch {
		case err == nil && sched != nil:
			scheduleID = sched.ID
		case errors.Is(err, sql.ErrNoRows):
			// No schedule for this team — fall back to stored UUID
		default:
			return nil, fmt.Errorf("failed to resolve team schedule for %s: %w", teamID, err)
		}
	}

	result, err := scheduler.FetchCurrentOnCall(b.Store, scheduleID)
	if err != nil {
		return nil, err
	}

	if result.Override != nil && result.Override.User != nil {
		return []*model.User{result.Override.User}, nil
	}
	return result.L1Users, nil
}
