package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
)

type HandoffExecutor struct {
	providers ProviderResolver
}

func NewHandoffExecutor(providers ProviderResolver) *HandoffExecutor {
	return &HandoffExecutor{
		providers: providers,
	}
}

func (e *HandoffExecutor) Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error) {
	var stepData model.HandoffStepData
	if err := json.Unmarshal(step.Data, &stepData); err != nil {
		return "", fmt.Errorf("invalid handoff data: %w", err)
	}

	// Sprint 4 (Epic 7 L7): provider comes from the step, not a hardcode.
	// handoff_notifier populates ProviderName per linked identity.
	if stepData.ProviderName == "" {
		return "", fmt.Errorf("handoff step %s: %w", step.ID, ErrMissingProvider)
	}

	provider, err := e.providers.Provider(stepData.ProviderName)
	if err != nil {
		return "", err
	}

	// Handoff is a fire-and-forget DM. No delivery row (no AlertGroup).
	if _, err := provider.Send(ctx, NotificationRequest{
		Kind:     "handoff",
		Target:   NotificationTarget{Kind: "user", ID: stepData.TargetID},
		Message:  stepData.Message,
		Editable: false,
	}); err != nil {
		return "", err
	}

	return fmt.Sprintf("handoff_dm_sent_to_%s", stepData.TargetID), nil
}
