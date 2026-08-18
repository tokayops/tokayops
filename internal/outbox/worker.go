package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

const (
	claimBatch    = 10
	claimInterval = 2 * time.Second
	leaseDuration = 120 * time.Second
)

// Worker is the outbox delivery worker. It claims outbox events, fans them out
// to subscriber generic_webhook integrations, delivers via HTTP, and retries.
type Worker struct {
	store    store.StoreInterface
	sender   WebhookSender
	workerID string
}

// New creates a new outbox delivery worker.
func New(st store.StoreInterface, sender WebhookSender) *Worker {
	return &Worker{
		store:    st,
		sender:   sender,
		workerID: uuid.New().String(),
	}
}

// Run starts the single claim loop goroutine. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("Outbox worker (%s) started", w.workerID)
	ticker := time.NewTicker(claimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("Outbox worker (%s) stopped", w.workerID)
			return
		case <-ticker.C:
			w.claimLoop(ctx)
		}
	}
}

func (w *Worker) claimLoop(ctx context.Context) {
	events, err := w.store.ClaimOutboxEvents(w.workerID, claimBatch, leaseDuration)
	if err != nil {
		log.Printf("outbox: claim error: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	metrics.OutboxEventsClaimedTotal.Add(float64(len(events)))

	for _, event := range events {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("outbox: panic processing event %s: %v", event.ID, r)
					errMsg := fmt.Sprintf("panic: %v", r)
					w.releaseEvent(event, fmt.Errorf("%s", errMsg))
				}
			}()
			w.processEvent(ctx, event)
		}()
	}
}

func (w *Worker) processEvent(ctx context.Context, event *model.OutboxEvent) {
	// Get all generic_webhook integrations
	integrations, err := w.store.GetIntegrationsByType(model.IntegrationTypeGenericWebhook)
	if err != nil {
		log.Printf("outbox: event %s: failed to get integrations: %v", event.ID, err)
		w.releaseEvent(event, err)
		return
	}

	// Filter: enabled + scope match
	var subscribers []*model.Integration
	for _, integ := range integrations {
		if !integ.Enabled {
			continue
		}
		if integ.Scope == nil {
			continue
		}
		switch *integ.Scope {
		case model.WebhookScopeGlobal:
			subscribers = append(subscribers, integ)
		case model.WebhookScopeTeam:
			if integ.TeamID != nil && *integ.TeamID == event.TeamID {
				subscribers = append(subscribers, integ)
			}
		}
	}

	// No subscribers → complete immediately
	if len(subscribers) == 0 {
		now := time.Now()
		event.Status = model.OutboxEventStatusCompleted
		event.SentAt = &now
		event.LockedUntil = nil
		event.LockedBy = nil
		event.NextAttemptAt = nil
		event.LastError = nil
		ok, err := w.store.UpdateOutboxEventIfOwned(event, w.workerID)
		if err != nil {
			log.Printf("outbox: event %s: failed to complete (no subscribers): %v", event.ID, err)
			return
		}
		if !ok {
			log.Printf("outbox: event %s: ownership lost, skipping no-subscribers completion", event.ID)
			return
		}
		metrics.OutboxEventsCompletedTotal.WithLabelValues("no_subscribers").Inc()
		return
	}

	// Fan out: create delivery per subscriber, attempt pending ones
	var fanoutErr bool
	for _, integ := range subscribers {
		delivery, err := w.ensureDelivery(event, integ)
		if err != nil {
			log.Printf("outbox: event %s integ %s: ensure delivery error: %v", event.ID, integ.ID, err)
			fanoutErr = true
			continue
		}

		// Skip terminal
		if delivery.Status == model.OutboxDeliverySent || delivery.Status == model.OutboxDeliveryFailed {
			continue
		}

		// Skip if retry not yet due
		if delivery.Status == model.OutboxDeliveryRetry && delivery.NextAttemptAt != nil && delivery.NextAttemptAt.After(time.Now()) {
			continue
		}

		if !w.attemptDelivery(ctx, event, delivery, integ) {
			return // lease lost, stop processing this event
		}
	}

	if fanoutErr {
		w.releaseEvent(event, errors.New("fan-out: one or more delivery creations failed"))
		return
	}

	w.finalizeEventIfDone(event)
}

func (w *Worker) ensureDelivery(event *model.OutboxEvent, integ *model.Integration) (*model.OutboxDelivery, error) {
	delivery := &model.OutboxDelivery{
		EventID:       event.ID,
		IntegrationID: integ.ID,
		Status:        model.OutboxDeliveryPending,
	}
	err := w.store.CreateOutboxDelivery(delivery)
	if err != nil {
		// Duplicate: fetch existing
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			existing, fetchErr := w.store.GetOutboxDelivery(event.ID, integ.ID)
			if fetchErr != nil {
				return nil, fetchErr
			}
			return existing, nil
		}
		return nil, err
	}
	return delivery, nil
}

// attemptDelivery delivers to a single integration. Returns false if lease was lost (caller should stop).
func (w *Worker) attemptDelivery(ctx context.Context, event *model.OutboxEvent, delivery *model.OutboxDelivery, integ *model.Integration) bool {
	// Unmarshal config
	var cfg model.GenericWebhookConfig
	if err := json.Unmarshal(integ.Config, &cfg); err != nil {
		errMsg := fmt.Sprintf("unmarshal config: %v", err)
		delivery.Status = model.OutboxDeliveryFailed
		delivery.LastError = &errMsg
		if err := w.store.UpdateOutboxDelivery(delivery); err != nil {
			log.Printf("outbox: delivery %s: update error: %v", delivery.ID, err)
		}
		return true
	}

	// Timeout
	timeout := 30 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	// Extend lease before sending — must cover at least timeout + safety margin
	const safetyMargin = 15 * time.Second
	leaseNeeded := timeout + safetyMargin
	if leaseNeeded < leaseDuration {
		leaseNeeded = leaseDuration
	}
	deadline := time.Now().Add(leaseNeeded)
	ok, err := w.store.ExtendOutboxEventLease(event.ID, w.workerID, deadline)
	if err != nil {
		log.Printf("outbox: event %s: extend lease error: %v", event.ID, err)
		return false
	}
	if !ok {
		log.Printf("outbox: event %s: lease lost, skipping delivery", event.ID)
		return false
	}

	// Build headers
	headers := BuildHeaders(event.ID, event.EventType, event.Payload, cfg.Secret, cfg.CustomHeaders)

	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Send
	result := w.sender.Send(sendCtx, cfg.URL, event.Payload, headers)

	// Record attempt (append-only audit)
	attempt := &model.DeliveryAttempt{
		DeliveryID: delivery.ID,
		Attempt:    delivery.Attempts,
	}
	if result.Error != nil {
		errStr := result.Error.Error()
		attempt.Error = &errStr
	}
	if result.HTTPStatus != 0 {
		attempt.HTTPStatus = &result.HTTPStatus
	}
	if result.ResponseBody != "" {
		attempt.ResponseBodyTrunc = &result.ResponseBody
	}
	if err := w.store.CreateDeliveryAttempt(attempt); err != nil {
		log.Printf("outbox: delivery %s: create attempt error: %v", delivery.ID, err)
	}

	// Store request payload on first attempt
	if delivery.Attempts == 0 {
		payload := string(event.Payload)
		delivery.RequestPayload = &payload
	}

	delivery.Attempts++

	// Reset stale metadata from previous attempts before populating new values
	resetDeliveryAttemptSummary(delivery)

	if result.ResponseBody != "" {
		delivery.ResponseBodyTrunc = &result.ResponseBody
	}

	if result.Error != nil {
		// Network/timeout/SSRF error → retry or fail
		errStr := result.Error.Error()
		delivery.LastError = &errStr
		if delivery.Attempts >= DefaultMaxAttempts {
			delivery.Status = model.OutboxDeliveryFailed
			delivery.NextAttemptAt = nil
			metrics.OutboxDeliveryAttemptsTotal.WithLabelValues("failed").Inc()
		} else {
			delivery.Status = model.OutboxDeliveryRetry
			next := time.Now().Add(computeBackoff(delivery.Attempts - 1))
			delivery.NextAttemptAt = &next
			metrics.OutboxDeliveryAttemptsTotal.WithLabelValues("retry").Inc()
		}
	} else if result.HTTPStatus >= 200 && result.HTTPStatus < 300 {
		// Success
		now := time.Now()
		delivery.Status = model.OutboxDeliverySent
		delivery.SentAt = &now
		delivery.LastHTTPStatus = &result.HTTPStatus
		delivery.LastError = nil
		delivery.NextAttemptAt = nil
		metrics.OutboxDeliveryAttemptsTotal.WithLabelValues("sent").Inc()
	} else if result.HTTPStatus >= 400 && result.HTTPStatus < 500 {
		// Client error → permanent failure
		delivery.Status = model.OutboxDeliveryFailed
		delivery.LastHTTPStatus = &result.HTTPStatus
		errStr := fmt.Sprintf("HTTP %d", result.HTTPStatus)
		delivery.LastError = &errStr
		delivery.NextAttemptAt = nil
		metrics.OutboxDeliveryAttemptsTotal.WithLabelValues("failed").Inc()
	} else {
		// 5xx or unexpected → retry or fail
		delivery.LastHTTPStatus = &result.HTTPStatus
		errStr := fmt.Sprintf("HTTP %d", result.HTTPStatus)
		delivery.LastError = &errStr
		if delivery.Attempts >= DefaultMaxAttempts {
			delivery.Status = model.OutboxDeliveryFailed
			delivery.NextAttemptAt = nil
			metrics.OutboxDeliveryAttemptsTotal.WithLabelValues("failed").Inc()
		} else {
			delivery.Status = model.OutboxDeliveryRetry
			next := time.Now().Add(computeBackoff(delivery.Attempts - 1))
			delivery.NextAttemptAt = &next
			metrics.OutboxDeliveryAttemptsTotal.WithLabelValues("retry").Inc()
		}
	}

	if err := w.store.UpdateOutboxDelivery(delivery); err != nil {
		log.Printf("outbox: delivery %s: update error: %v", delivery.ID, err)
	}
	return true
}

// resetDeliveryAttemptSummary clears stale per-attempt metadata so fields from
// a previous attempt (e.g. HTTP status after a 5xx) don't survive into the next.
func resetDeliveryAttemptSummary(d *model.OutboxDelivery) {
	d.LastHTTPStatus = nil
	d.LastError = nil
	d.NextAttemptAt = nil
	d.ResponseBodyTrunc = nil
}

func (w *Worker) finalizeEventIfDone(event *model.OutboxEvent) {
	deliveries, err := w.store.GetDeliveriesByEventID(event.ID)
	if err != nil {
		log.Printf("outbox: event %s: get deliveries error: %v", event.ID, err)
		return
	}

	allTerminal := true
	var minNextAttempt *time.Time
	for _, d := range deliveries {
		if d.Status != model.OutboxDeliverySent && d.Status != model.OutboxDeliveryFailed {
			allTerminal = false
			if d.NextAttemptAt != nil && (minNextAttempt == nil || d.NextAttemptAt.Before(*minNextAttempt)) {
				minNextAttempt = d.NextAttemptAt
			}
		}
	}

	if allTerminal {
		now := time.Now()
		event.Status = model.OutboxEventStatusCompleted
		event.SentAt = &now
		event.LockedUntil = nil
		event.LockedBy = nil
		event.NextAttemptAt = nil
		event.LastError = nil
		ok, err := w.store.UpdateOutboxEventIfOwned(event, w.workerID)
		if err != nil {
			log.Printf("outbox: event %s: complete error: %v", event.ID, err)
			return
		}
		if !ok {
			log.Printf("outbox: event %s: ownership lost, skipping completion", event.ID)
			return
		}
		metrics.OutboxEventsCompletedTotal.WithLabelValues("delivered").Inc()
	} else {
		// Keep processing, set next_attempt_at to earliest retryable delivery
		event.NextAttemptAt = minNextAttempt
		event.LockedUntil = nil
		event.LockedBy = nil
		ok, err := w.store.UpdateOutboxEventIfOwned(event, w.workerID)
		if err != nil {
			log.Printf("outbox: event %s: release error: %v", event.ID, err)
			return
		}
		if !ok {
			log.Printf("outbox: event %s: ownership lost, skipping release", event.ID)
			return
		}
	}
}

func (w *Worker) releaseEvent(event *model.OutboxEvent, releaseErr error) {
	event.Attempts++
	errMsg := releaseErr.Error()
	event.LastError = &errMsg
	event.LockedUntil = nil
	event.LockedBy = nil

	if event.Attempts >= DefaultMaxAttempts {
		event.Status = model.OutboxEventStatusFailed
	} else {
		next := time.Now().Add(computeBackoff(event.Attempts - 1))
		event.NextAttemptAt = &next
	}

	ok, err := w.store.UpdateOutboxEventIfOwned(event, w.workerID)
	if err != nil {
		log.Printf("outbox: event %s: release error: %v", event.ID, err)
		return
	}
	if !ok {
		log.Printf("outbox: event %s: ownership lost, skipping release", event.ID)
	}
}
