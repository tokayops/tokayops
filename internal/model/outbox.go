package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// OutboxEventStatus represents the processing status of an outbox event.
type OutboxEventStatus string

const (
	OutboxEventStatusPending    OutboxEventStatus = "pending"
	OutboxEventStatusProcessing OutboxEventStatus = "processing"
	OutboxEventStatusCompleted  OutboxEventStatus = "completed"
	OutboxEventStatusFailed     OutboxEventStatus = "failed"
	// OutboxEventStatusFannedOut: the event has been turned into delivery
	// commitments, one per subscriber that was enabled and in scope, and nothing
	// reads it as work any more. The statuses above it are the old delivery
	// worker's and remain on rows written before the cutover.
	OutboxEventStatusFannedOut OutboxEventStatus = "fanned_out"
)

// OutboxEventType represents a lifecycle event type.
type OutboxEventType string

const (
	OutboxEventFiring       OutboxEventType = "alert_group.firing"
	OutboxEventAcknowledged OutboxEventType = "alert_group.acknowledged"
	OutboxEventResolved     OutboxEventType = "alert_group.resolved"
)

// OutboxEvent represents a lifecycle event in the transactional outbox.
type OutboxEvent struct {
	ID            string            `json:"id"`
	EventType     OutboxEventType   `json:"event_type"`
	AlertGroupID  string            `json:"alert_group_id"`
	TeamID        string            `json:"team_id"`
	Actor         string            `json:"actor"`
	Payload       json.RawMessage   `json:"payload"`
	Status        OutboxEventStatus `json:"status"`
	Attempts      int               `json:"attempts"`
	NextAttemptAt *time.Time        `json:"next_attempt_at,omitempty"`
	LockedUntil   *time.Time        `json:"locked_until,omitempty"`
	LockedBy      *string           `json:"locked_by,omitempty"`
	LastError     *string           `json:"last_error,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	SentAt        *time.Time        `json:"sent_at,omitempty"`
}

// WebhookEventPayload is the canonical webhook delivery body.
// Used as the durable snapshot in event_outbox.payload.
type WebhookEventPayload struct {
	Event      string                   `json:"event"`
	Timestamp  string                   `json:"timestamp"`
	AlertGroup WebhookAlertGroupPayload `json:"alert_group"`
	Actor      WebhookActorPayload      `json:"actor"`
}

type WebhookAlertGroupPayload struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Severity   string `json:"severity"`
	TeamID     string `json:"team_id"`
	TeamName   string `json:"team_name"`
	AlertCount int    `json:"alert_count"`
	CreatedAt  string `json:"created_at"`
	URL        string `json:"url,omitempty"`
}

type WebhookActorPayload struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// BuildWebhookEventPayload constructs a typed webhook payload matching the documented contract.
func BuildWebhookEventPayload(
	eventType OutboxEventType,
	ag *AlertGroup,
	teamName string,
	actorName string,
	actorEmail string,
	timestamp time.Time,
) (json.RawMessage, error) {
	status, err := eventStatusFromType(eventType)
	if err != nil {
		return nil, err
	}
	p := WebhookEventPayload{
		Event:     string(eventType),
		Timestamp: timestamp.UTC().Format(time.RFC3339),
		AlertGroup: WebhookAlertGroupPayload{
			ID:         ag.ID,
			Title:      ag.Title,
			Status:     status,
			Severity:   ag.Severity,
			TeamID:     ag.TeamID,
			TeamName:   teamName,
			AlertCount: len(ag.Alerts),
			CreatedAt:  ag.CreatedAt.UTC().Format(time.RFC3339),
		},
		Actor: WebhookActorPayload{
			Name:  actorName,
			Email: actorEmail,
		},
	}
	return json.Marshal(p)
}

func eventStatusFromType(t OutboxEventType) (string, error) {
	switch t {
	case OutboxEventFiring:
		return "firing", nil
	case OutboxEventAcknowledged:
		return "acknowledged", nil
	case OutboxEventResolved:
		return "resolved", nil
	default:
		return "", fmt.Errorf("unknown outbox event type: %s", t)
	}
}

// OutboxDeliveryStatus represents the delivery status for a specific integration.
type OutboxDeliveryStatus string

const (
	OutboxDeliveryPending OutboxDeliveryStatus = "pending"
	OutboxDeliveryRetry   OutboxDeliveryStatus = "retry"
	OutboxDeliverySent    OutboxDeliveryStatus = "sent"
	OutboxDeliveryFailed  OutboxDeliveryStatus = "failed"
)

// DeliveryAttempt records a single HTTP attempt for audit (append-only).
type DeliveryAttempt struct {
	ID                string    `json:"id"`
	DeliveryID        string    `json:"delivery_id"`
	Attempt           int       `json:"attempt"`
	HTTPStatus        *int      `json:"http_status,omitempty"`
	Error             *string   `json:"error,omitempty"`
	ResponseBodyTrunc *string   `json:"response_body_trunc,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// OutboxDelivery represents a fan-out delivery attempt for an event to an integration.
type OutboxDelivery struct {
	ID                string               `json:"id"`
	EventID           string               `json:"event_id"`
	IntegrationID     string               `json:"integration_id"`
	Status            OutboxDeliveryStatus `json:"status"`
	Attempts          int                  `json:"attempts"`
	NextAttemptAt     *time.Time           `json:"next_attempt_at,omitempty"`
	LastHTTPStatus    *int                 `json:"last_http_status,omitempty"`
	LastError         *string              `json:"last_error,omitempty"`
	RequestPayload    *string              `json:"request_payload,omitempty"`
	ResponseBodyTrunc *string              `json:"response_body_trunc,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	SentAt            *time.Time           `json:"sent_at,omitempty"`
}
