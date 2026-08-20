package builders

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// ErrNoUpdatableDeliveries indicates no deliveries need updating (not a failure)
var ErrNoUpdatableDeliveries = errors.New("no updatable deliveries")

type UpdateJobBuilder struct {
	Config *config.Config
	Store  store.StoreInterface
}

func NewUpdateJobBuilder(cfg *config.Config, s store.StoreInterface) (*UpdateJobBuilder, error) {
	if s == nil {
		return nil, errors.New("update builder requires store")
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &UpdateJobBuilder{Config: cfg, Store: s}, nil
}

// Build builds the update that follows an acknowledgement.
func (b *UpdateJobBuilder) Build(ag *model.AlertGroup) (*model.Job, []*model.JobStage, []*model.JobStep, error) {
	return b.build(ag, jobdedup.AckUpdate(ag.ID))
}

// BuildAlertUpdate builds the update that follows a new alert in the group.
//
// Two families share the job type "update", and which one this is used to be a
// string a caller passed in. It is a dedup spec now: the family is chosen by
// naming it, not by spelling a prefix.
func (b *UpdateJobBuilder) BuildAlertUpdate(ag *model.AlertGroup) (*model.Job, []*model.JobStage, []*model.JobStep, error) {
	return b.build(ag, jobdedup.AlertUpdate(ag.ID))
}

func (b *UpdateJobBuilder) build(ag *model.AlertGroup, spec *jobdedup.Spec) (*model.Job, []*model.JobStage, []*model.JobStep, error) {
	if b.Store == nil {
		return nil, nil, nil, fmt.Errorf("update builder requires store")
	}

	// Get ALL deliveries for this alert group
	deliveries, err := b.Store.ListDeliveries(ag.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list deliveries: %w", err)
	}

	// Filter only updatable deliveries
	var updatableDeliveries []*model.NotificationDelivery
	for _, d := range deliveries {
		if d.SupportsUpdate {
			updatableDeliveries = append(updatableDeliveries, d)
		}
	}

	// Nothing to update
	if len(updatableDeliveries) == 0 {
		return nil, nil, nil, ErrNoUpdatableDeliveries
	}

	// Job Setup
	jobID := uuid.New().String()
	now := time.Now()

	job := &model.Job{
		ID:           jobID,
		Type:         "update",
		Status:       model.JobStatusPending,
		Dedup:        spec,
		CurrentStage: 0,
		Payload:      json.RawMessage("{}"),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	var steps []*model.JobStep

	// Create an update step for EACH updatable delivery
	for i, delivery := range updatableDeliveries {
		stepData := model.ResolutionStepData{
			AlertGroupID: ag.ID,
			DeliveryID:   delivery.ID,
			ProviderName: delivery.Provider,
			IsFirehose:   delivery.IsFirehose,
			Operation:    "update",
		}
		dataBytes, _ := json.Marshal(stepData)
		timeout := 60

		status := model.JobStepStatusPending
		var nextRunAt *time.Time
		if i == 0 {
			nextRunAt = &now
		} else {
			status = model.JobStepStatusBlocked
		}

		steps = append(steps, &model.JobStep{
			ID:             uuid.New().String(),
			JobID:          jobID,
			StepIndex:      i,
			StepType:       "update", // update operation (uses resolve executor)
			Status:         status,
			Data:           json.RawMessage(dataBytes),
			NextRunAt:      nextRunAt,
			TimeoutSeconds: &timeout,
			MaxAttempts:    3,
			CreatedAt:      now,
			UpdatedAt:      now,
		})
	}

	stages := WrapStepsInStages(jobID, steps, now)
	return job, stages, steps, nil
}
