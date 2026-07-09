package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

type ResolutionExecutor struct {
	store     store.StoreInterface
	providers ProviderResolver
	cfg       *config.Config
}

func NewResolutionExecutor(s store.StoreInterface, providers ProviderResolver, cfg *config.Config) *ResolutionExecutor {
	return &ResolutionExecutor{
		store:     s,
		providers: providers,
		cfg:       cfg,
	}
}

func (e *ResolutionExecutor) Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error) {
	var stepData model.ResolutionStepData
	if err := json.Unmarshal(step.Data, &stepData); err != nil {
		return "", fmt.Errorf("invalid resolution data: %w", err)
	}

	// 1. Load Alert Group
	ag, err := e.store.GetAlertGroupByID(stepData.AlertGroupID)
	if err != nil {
		return "", fmt.Errorf("failed to load alert group: %w", err)
	}
	if ag == nil {
		return "", fmt.Errorf("alert group not found: %s", stepData.AlertGroupID)
	}

	// 2. Get Provider — set by the builder from the delivery; empty is a bug.
	providerName := stepData.ProviderName
	if providerName == "" {
		return "", fmt.Errorf("resolution step %s: %w", step.ID, ErrMissingProvider)
	}
	provider, err := e.providers.Provider(providerName)
	if err != nil {
		return "", err
	}

	// 3. Load the specific delivery by ID
	var delivery *model.NotificationDelivery
	if stepData.DeliveryID != "" {
		// New path: use DeliveryID directly
		delivery, err = e.store.GetDeliveryByID(stepData.DeliveryID)
		if err != nil {
			log.Printf("ResolutionExecutor: Failed to load delivery %s: %v", stepData.DeliveryID, err)
			return "", err
		}
	} else {
		// Legacy path: fallback to old behavior for backward compatibility
		if stepData.IsFirehose {
			delivery, err = e.store.GetFirehoseDelivery(ag.ID, providerName)
		} else {
			delivery, err = e.store.GetPrimaryDelivery(ag.ID, providerName)
		}
		if err != nil {
			log.Printf("ResolutionExecutor: Failed to load delivery (legacy): %v", err)
			return "", err
		}
	}

	if delivery == nil || delivery.ProviderPayload == "" {
		return "", fmt.Errorf("delivery not found or empty payload for alert group %s", ag.ID)
	}

	// 4. Execute operation based on type. The provider reads its own ref from
	// delivery.ProviderPayload — no AG-level provider blob.
	operation := stepData.Operation
	if operation == "" {
		operation = "resolve" // default for backward compatibility
	}

	log.Printf("Executor: %s %s via %s (delivery=%s, firehose=%v)",
		operation, ag.ID, providerName, delivery.ID, delivery.IsFirehose)

	var execErr error
	var updatedPayload string
	if operation == "update" {
		updatedPayload, execErr = provider.Update(ctx, delivery, ag)
	} else {
		execErr = provider.Resolve(ctx, delivery, ag)
	}

	if execErr != nil {
		return "", execErr
	}

	// 6. Save updated payload (contains TimelineTimestamp for subsequent updates)
	if updatedPayload != "" {
		if err := e.store.UpdateDeliveryPayload(delivery.ID, updatedPayload); err != nil {
			log.Printf("ResolutionExecutor: Failed to save updated payload: %v", err)
			// Non-fatal: continue even if save fails
		}
	}

	return operation + "d", nil // "resolved" or "updated"
}
