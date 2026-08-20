package builders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

// OnCallProjection is the slice of the schedule projection the builder needs.
//
// It is an interface declared here rather than *schedulerender.Service because
// the revision model is deliberately absent from MockStore: without
// it, every builder unit test would have to run against PostgreSQL.
type OnCallProjection interface {
	// CurrentTeamOnCallNow answers who is on duty for a team and whether the
	// team has a schedule at all.
	CurrentTeamOnCallNow(ctx context.Context, teamID string) (schedulerender.TeamOnCall, error)

	// CurrentOnCallNow answers for one schedule by ID.
	CurrentOnCallNow(ctx context.Context, scheduleID string) (schedulerender.OnCall, error)
}

// ErrOnCallResolutionUnavailable means the on-call recipients could not be
// resolved: the projection would not answer, or its answer could not be
// hydrated into people.
//
// It is NOT "nobody is on duty". That is an answer, and a step aimed at nobody
// still builds - as a marker the executor reports. This is the absence of an
// answer, and a job built on it would page nobody while looking like a job that
// had. The caller must retry rather than commit one: once an alert group has an
// escalation job, nothing picks it up again, so the page is not late, it is
// gone.
var ErrOnCallResolutionUnavailable = errors.New("on-call recipients could not be resolved")

// EscalationJobBuilder builds escalation jobs from Store-based policies
type EscalationJobBuilder struct {
	Store  store.StoreInterface
	OnCall OnCallProjection
	Config *config.Config // for firehose channel config
}

// scheduleResolution is one schedule's answer as this build saw it, outcome
// included: a read that failed is an answer to remember too, or two steps naming
// one schedule would get a marker and a real recipient out of the same build.
type scheduleResolution struct {
	users []*model.User
	err   error
}

// onCallResolver answers "who is on duty for this step" consistently within one
// Build.
//
// The team's answer is read ONCE, by the caller, and handed in: the engine
// stores that same answer on the alert group, and a job that pages one group
// while the snapshot records another describes a state that never existed. A
// schedule read by ID is remembered here for the same reason one step down - two
// policy steps naming the same schedule are one question, and neither a handoff
// nor a transient failure landing between them may split the answer.
type onCallResolver struct {
	builder *EscalationJobBuilder
	team    schedulerender.TeamOnCallResult

	// teamUsers is the team's answer hydrated into people. Nil means "not
	// worked out yet" - a pointer rather than a value plus a flag, so the
	// "computed" bit cannot drift away from what was computed, and a result
	// that is legitimately empty is still a result.
	teamUsers *scheduleResolution

	byID map[string]scheduleResolution
}

func NewEscalationJobBuilder(s store.StoreInterface, oncall OnCallProjection, cfg *config.Config) *EscalationJobBuilder {
	return &EscalationJobBuilder{Store: s, OnCall: oncall, Config: cfg}
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

// Build assembles the escalation job for one alert group.
//
// teamOnCall is who is on duty for the alert's team, read once by the caller,
// with whether that read succeeded. It is a parameter rather than a read of its
// own because the same answer has to serve every schedule-typed step of this job
// AND the snapshot the caller stores on the alert group: two reads a moment
// apart can straddle a handoff or a save, and then the people paged and the
// people recorded are different people.
func (b *EscalationJobBuilder) Build(ctx context.Context, ag *model.AlertGroup, policyID string,
	teamOnCall schedulerender.TeamOnCallResult) (*model.Job, []*model.JobStage, []*model.JobStep, *model.EscalationPolicySnapshot, error) {

	onCall := &onCallResolver{builder: b, team: teamOnCall}

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
	now := time.Now()

	snapshot := &model.EscalationPolicySnapshot{
		PolicyID: policyID,
	}
	if policy != nil {
		snapshot.Name = policy.Name
	}

	// Identity, type and alert group are not set here: EnsureEscalationJob
	// derives all three from the group it locks, so this builder cannot hand it
	// a job that contradicts itself.
	job := &model.Job{
		ID:           jobID,
		Status:       model.JobStatusPending,
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

				// Which failure this is decides what to do with it, and the
				// renderer already draws that line. Damage to stored data will
				// answer the same way on every retry, so degrading to a marker
				// is the best available outcome: the rest of the policy still
				// runs and the alert still reaches its channel. Anything else -
				// a transaction that would not open, a hydration read that
				// failed - may well answer next tick, and committing a job now
				// spends the only chance this alert had to page its on-call.
				users, resolveErr := onCall.resolveScheduleUsers(ctx, stepCfg.TargetID)
				if resolveErr != nil {
					reason, damaged := schedulerender.FailureReasonOf(resolveErr)
					if !damaged {
						return nil, nil, nil, nil, fmt.Errorf("%w: schedule target %s: %w",
							ErrOnCallResolutionUnavailable, stepCfg.TargetID, resolveErr)
					}
					// The target is named, not blamed: a team whose own
					// schedule is damaged fails here too, and that schedule is
					// not the one this step names.
					log.Printf("EscalationBuilder: on-call resolution for schedule target %s failed due to projection damage (%s), degrading to a marker step: %v",
						stepCfg.TargetID, reason, resolveErr)
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

// resolveScheduleUsers resolves one schedule-typed policy step to on-call users.
//
// The team is asked first and the stored schedule ID is only a fallback, which
// is what keeps a policy step pointing at a schedule that was replaced from
// paging whoever used to be on it.
//
// There is no override branch here any more: the projection overlays overrides
// onto the layer, so L1 already names whoever is actually on duty. Escalating
// "to the override user" separately would be a second, older answer to the same
// question. Only L1 is returned - L2 is not an escalation target today.
func (r *onCallResolver) resolveScheduleUsers(ctx context.Context, fallbackScheduleID string) ([]*model.User, error) {
	// An unreadable team is not a team without a schedule. Falling through to
	// the stored ID here would answer a question that could not be asked, out of
	// a schedule the team may no longer own - and it would answer it from a
	// second transaction, so the job would page people the alert group is not
	// even recording.
	if err := r.team.Err(); err != nil {
		return nil, fmt.Errorf("team on-call state is unreadable: %w", err)
	}

	// A team WITH a schedule answers for itself, even if that schedule is
	// deleted or between shifts: "nobody" is its answer, and falling through to
	// the stored ID would answer with someone else's schedule.
	//
	// The answer is hydrated once. The projection is already fixed for this
	// build, but turning IDs into people is a database read of its own, and a
	// transient failure in the middle of a two-step policy would otherwise leave
	// one step with a marker and the other with real recipients - the same split
	// answer the schedule memo below exists to prevent, one level down.
	if team := r.team.OnCall(); team.ScheduleID != "" {
		if r.teamUsers == nil {
			users, err := r.builder.hydrateOnCall(team.OnCall)
			r.teamUsers = &scheduleResolution{users: users, err: err}
		}
		return r.teamUsers.users, r.teamUsers.err
	}

	if fallbackScheduleID == "" {
		return nil, nil
	}
	if seen, ok := r.byID[fallbackScheduleID]; ok {
		return seen.users, seen.err
	}

	resolution := scheduleResolution{}
	onCall, err := r.builder.OnCall.CurrentOnCallNow(ctx, fallbackScheduleID)
	if err != nil {
		resolution.err = fmt.Errorf("failed to resolve schedule %s: %w", fallbackScheduleID, err)
	} else {
		resolution.users, resolution.err = r.builder.hydrateOnCall(onCall)
	}

	if r.byID == nil {
		r.byID = map[string]scheduleResolution{}
	}
	r.byID[fallbackScheduleID] = resolution
	return resolution.users, resolution.err
}

// hydrateOnCall turns the projected L1 user IDs into user records, in the order
// the projection gave them so step indices stay stable across ticks.
//
// Users the store does not return are dropped rather than escalated to: an
// erased person has no identities left to notify, and a step aimed at them
// would only fail.
func (b *EscalationJobBuilder) hydrateOnCall(onCall schedulerender.OnCall) ([]*model.User, error) {
	if onCall.L1 == nil || len(onCall.L1.UserIDs) == 0 {
		return nil, nil
	}
	fetched, err := b.Store.GetUsersByIDs(onCall.L1.UserIDs)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.User, len(fetched))
	for _, u := range fetched {
		byID[u.ID] = u
	}
	out := make([]*model.User, 0, len(onCall.L1.UserIDs))
	for _, id := range onCall.L1.UserIDs {
		if u, ok := byID[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}
