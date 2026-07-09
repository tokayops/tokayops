package builders

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/google/uuid"
)

type ResolutionJobBuilder struct {
	Config *config.Config
	Store  store.StoreInterface
}

func NewResolutionJobBuilder(cfg *config.Config, s store.StoreInterface) *ResolutionJobBuilder {
	return &ResolutionJobBuilder{Config: cfg, Store: s}
}

func (b *ResolutionJobBuilder) Build(ag *model.AlertGroup) (*model.Job, []*model.JobStage, []*model.JobStep, error) {
	if b.Store == nil {
		return nil, nil, nil, fmt.Errorf("resolution builder requires store")
	}

	// Get ALL deliveries for this alert group
	deliveries, err := b.Store.ListDeliveries(ag.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to list deliveries: %w", err)
	}

	// Filter only updatable deliveries (can be resolved)
	var updatableDeliveries []*model.NotificationDelivery
	for _, d := range deliveries {
		if d.SupportsUpdate {
			updatableDeliveries = append(updatableDeliveries, d)
		}
	}

	// Nothing to resolve
	if len(updatableDeliveries) == 0 {
		return nil, nil, nil, nil
	}

	// Job Setup
	jobID := uuid.New().String()
	dedupKey := "resolve_" + ag.ID
	now := time.Now()

	job := &model.Job{
		ID:          jobID,
		Type:        "resolution",
		Status:      model.JobStatusPending,
		DedupKey:    &dedupKey,
		CurrentStage: 0,
		Payload:     json.RawMessage("{}"),
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	var steps []*model.JobStep

	// Create a resolve step for EACH updatable delivery
	for i, delivery := range updatableDeliveries {
		stepData := model.ResolutionStepData{
			AlertGroupID: ag.ID,
			DeliveryID:   delivery.ID,
			ProviderName: delivery.Provider,
			IsFirehose:   delivery.IsFirehose,
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
			StepType:       "resolve",
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
