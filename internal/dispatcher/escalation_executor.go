package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/google/uuid"
)

type EscalationExecutor struct {
	store     store.StoreInterface
	providers ProviderResolver
	cfg       *config.Config
}

func NewEscalationExecutor(s store.StoreInterface, p ProviderResolver, cfg *config.Config) *EscalationExecutor {
	return &EscalationExecutor{
		store:     s,
		providers: p,
		cfg:       cfg,
	}
}

func (e *EscalationExecutor) Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error) {
	var stepData model.EscalationStepData
	if err := json.Unmarshal(step.Data, &stepData); err != nil {
		return "", fmt.Errorf("invalid escalation data: %w", err)
	}

	// 1. Load Alert Group
	ag, err := e.store.GetAlertGroupByID(stepData.AlertGroupID)
	if err != nil {
		return "", fmt.Errorf("failed to load alert group: %w", err)
	}
	if ag == nil {
		return "", fmt.Errorf("alert group not found: %s", stepData.AlertGroupID)
	}

	// 2. Determine Provider — from the step (set by the builder), not the step type.
	//    Every builder sets it; an empty value is a build-invariant violation.
	providerName := stepData.ProviderName
	if providerName == "" {
		return "", fmt.Errorf("escalation step %s: %w", step.ID, ErrMissingProvider)
	}

	provider, err := e.providers.Provider(providerName)
	if err != nil {
		return "", err
	}

	// 3. Execute
	// providerPayload is the opaque ref persisted to the delivery (empty for
	// fire-and-forget DMs). stepResult is what we return to the job runner.
	var providerPayload string
	var stepResult string
	var execErr error
	var editable bool
	var timelineDetail string              // Extra detail for timeline message
	var timelineMetadata map[string]string // Metadata for timeline event

	switch step.StepType {
	case "firehose":
		// firehose is intentionally Slack-only — it bypasses the
		// (provider, target_kind) taxonomy. Builder hardcodes
		// ProviderName="slack" for this branch; do not generalize without
		// an explicit feature flag.
		editable = true
		providerPayload, execErr = provider.Send(ctx, NotificationRequest{
			Kind:       "firehose",
			Target:     NotificationTarget{Kind: "channel", ID: stepData.TargetID},
			AlertGroup: ag,
			Editable:   true,
		})
		stepResult = providerPayload
		timelineDetail = fmt.Sprintf(" to channel %s", stepData.TargetID)
		timelineMetadata = map[string]string{
			"step_type":  "firehose",
			"channel_id": stepData.TargetID,
		}
	case "dm":
		// Provider-agnostic direct message: resolve the TokayOps user via
		// external_identities for the active provider, then fire-and-forget.
		targetID := stepData.TargetID
		var userName string

		if stepData.TargetType == "user" {
			resolvedID, name, resolveErr := resolveRecipient(e.store, providerName, stepData.TargetID)
			if resolveErr != nil {
				return "", resolveErr
			}
			targetID = resolvedID
			userName = name
		}

		msg := stepData.Message
		if msg == "" {
			msg = fmt.Sprintf("You have a new alert: %s (Severity: %s)", ag.Title, ag.Severity)
		}
		permalink := e.getPrimaryPermalink(provider, ag.ID, providerName)
		if permalink == "" && e.cfg.Global.DmFallbackToFirehose() {
			permalink = e.getFirehosePermalink(provider, ag.ID, providerName)
		}
		if permalink != "" {
			// Permalink wording stays Slack-shaped until another provider
			// supplies its own format (Epic 8).
			msg = fmt.Sprintf("%s\nPrimary message: <%s|Open in Slack>", msg, permalink)
		}
		_, execErr = provider.Send(ctx, NotificationRequest{
			Kind:     "dm",
			Target:   NotificationTarget{Kind: "user", ID: targetID},
			Message:  msg,
			Editable: false,
		})
		stepResult = "dm_sent"
		timelineMetadata = map[string]string{
			"step_type":    "dm",
			"provider":     providerName,
			"recipient_id": targetID,
		}
		if userName != "" {
			timelineDetail = fmt.Sprintf(" to %s (%s)", userName, targetID)
			timelineMetadata["user_name"] = userName
		} else {
			timelineDetail = fmt.Sprintf(" to %s", targetID)
		}
	case "channel":
		editable = true
		providerPayload, execErr = provider.Send(ctx, NotificationRequest{
			Kind:       "channel",
			Target:     NotificationTarget{Kind: "channel", ID: stepData.TargetID},
			AlertGroup: ag,
			Editable:   true,
		})
		stepResult = providerPayload
		timelineDetail = fmt.Sprintf(" to channel %s", stepData.TargetID)
		timelineMetadata = map[string]string{
			"step_type":  "channel",
			"provider":   providerName,
			"channel_id": stepData.TargetID,
		}
	default:
		// Unknown step types must be rejected, not silently sent as a channel card.
		return "", fmt.Errorf("unsupported step type for escalation executor: %s", step.StepType)
	}

	// 4. Handle Timeline & Status Updates
	if execErr != nil {
		metrics.NotificationErrorsTotal.WithLabelValues(step.StepType, "send_failed").Inc()
		e.addTimelineEvent(ag.ID, model.TimelineEventNotificationFailed,
			fmt.Sprintf("Step %d execution failed: %v", step.StepIndex, execErr), "dispatcher", nil)
		return "", execErr
	}
	metrics.NotificationSentTotal.WithLabelValues(step.StepType).Inc()

	if err := e.recordDelivery(ag, step, stepData, providerName, providerPayload, editable); err != nil {
		if editable {
			// The delivery row is the ONLY ref for ack/resolve/update. Without it the
			// posted card is orphaned, so fail the step (it will retry) rather than
			// advance the AG to triggered with no recoverable delivery.
			metrics.NotificationErrorsTotal.WithLabelValues(step.StepType, "record_failed").Inc()
			return "", fmt.Errorf("failed to record editable delivery for step %s: %w", step.ID, err)
		}
		// Fire-and-forget (DM/OTP/handoff): best-effort, there is no ref to lose.
		log.Printf("EscalationExecutor: Failed to record delivery for step %s: %v", step.ID, err)
	}

	e.transitionAfterStep(ag)
	e.addTimelineEvent(ag.ID, model.TimelineEventNotificationSent,
		fmt.Sprintf("Step %d completed via %s%s", step.StepIndex, step.StepType, timelineDetail), "dispatcher", timelineMetadata)

	return stepResult, nil
}

func (e *EscalationExecutor) addTimelineEvent(alertGroupID string, eventType model.TimelineEventType, message, actor string, metadata map[string]string) {
	event := &model.TimelineEvent{
		ID:           uuid.New().String(),
		AlertGroupID: alertGroupID,
		Type:         eventType,
		Message:      message,
		Actor:        actor,
		Metadata:     metadata,
		CreatedAt:    time.Now(),
	}
	if err := e.store.AddTimelineEvent(event); err != nil {
		log.Printf("EscalationExecutor: Failed to add timeline event: %v", err)
	}
}

// transitionAfterStep moves the alert group from processing → triggered once the
// first notification has gone out. Provider message refs are recorded only on the
// delivery row (recordDelivery), never on the alert group.
func (e *EscalationExecutor) transitionAfterStep(ag *model.AlertGroup) {
	// Conditionally transition processing → triggered.
	// Uses CAS semantics to avoid overwriting a concurrent ack/resolve.
	if ag.Status == model.AlertGroupStatusProcessing {
		changed, err := e.store.TransitionAlertGroupStatus(ag.ID,
			model.AlertGroupStatusProcessing, model.AlertGroupStatusTriggered)
		if err != nil {
			log.Printf("EscalationExecutor: Failed to transition status for %s: %v", ag.ID, err)
		} else if !changed {
			log.Printf("EscalationExecutor: Status of %s already changed (concurrent ack/resolve), skipping transition", ag.ID)
		}
	}
}

// recordDelivery persists the notification delivery. providerPayload is the opaque
// ref returned by Provider.Send (empty for fire-and-forget). editable maps directly
// to SupportsUpdate — set by the caller from the send request, not by inspecting the
// provider-specific payload.
func (e *EscalationExecutor) recordDelivery(ag *model.AlertGroup, step *model.JobStep, stepData model.EscalationStepData, providerName, providerPayload string, editable bool) error {
	if ag == nil || step == nil {
		return nil
	}

	// Contract: an editable delivery must carry a payload — it is the ref that
	// Update/Resolve operate on. A SupportsUpdate=true row with an empty payload
	// would be picked by the builders and then crash the resolution executor.
	if editable && providerPayload == "" {
		return fmt.Errorf("editable delivery for step %s has empty provider payload", step.ID)
	}

	deliveryID := step.ID
	if deliveryID == "" {
		deliveryID = uuid.New().String()
	}
	delivery := &model.NotificationDelivery{
		ID:              deliveryID,
		AlertGroupID:    ag.ID,
		Provider:        providerName,
		Kind:            step.StepType,
		TargetType:      stepData.TargetType,
		TargetID:        stepData.TargetID,
		ProviderPayload: providerPayload,
		SupportsUpdate:  editable,
		IsPrimary:       false,
		IsFirehose:      step.StepType == "firehose",
		Attempt:         step.AttemptCount,
	}
	if step.ID != "" {
		delivery.JobStepID = &step.ID
	}

	if err := e.store.UpsertNotificationDelivery(delivery); err != nil {
		return err
	}

	if delivery.SupportsUpdate && !delivery.IsFirehose {
		if _, err := e.store.SetPrimaryDeliveryIfNone(ag.ID, delivery.ID); err != nil {
			return err
		}
	}

	return nil
}

func (e *EscalationExecutor) getPrimaryPermalink(provider Provider, alertGroupID, providerName string) string {
	delivery, err := e.store.GetPrimaryDelivery(alertGroupID, providerName)
	if err != nil || delivery == nil {
		return ""
	}
	return provider.Permalink(delivery)
}

func (e *EscalationExecutor) getFirehosePermalink(provider Provider, alertGroupID, providerName string) string {
	delivery, err := e.store.GetFirehoseDelivery(alertGroupID, providerName)
	if err != nil || delivery == nil {
		return ""
	}
	return provider.Permalink(delivery)
}
