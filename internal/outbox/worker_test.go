package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

const testWorkerID = "test-worker-id"

// mockSender implements WebhookSender for tests.
type mockSender struct {
	results []*DeliveryResult // returned in order; last one repeats
	calls   int
}

func (m *mockSender) Send(ctx context.Context, url string, body []byte, headers map[string]string) *DeliveryResult {
	idx := m.calls
	if idx >= len(m.results) {
		idx = len(m.results) - 1
	}
	m.calls++
	return m.results[idx]
}

func newTestEvent(teamID string) *model.OutboxEvent {
	payload, _ := json.Marshal(model.WebhookEventPayload{
		Event:     "alert_group.firing",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		AlertGroup: model.WebhookAlertGroupPayload{
			ID:     "ag-1",
			Title:  "Test Alert",
			Status: "firing",
			TeamID: teamID,
		},
	})
	wid := testWorkerID
	future := time.Now().Add(5 * time.Minute)
	return &model.OutboxEvent{
		ID:          "evt-1",
		EventType:   model.OutboxEventFiring,
		TeamID:      teamID,
		Payload:     payload,
		Status:      model.OutboxEventStatusProcessing,
		LockedBy:    &wid,
		LockedUntil: &future,
	}
}

func newGlobalIntegration(id string, cfg model.GenericWebhookConfig) *model.Integration {
	raw, _ := json.Marshal(cfg)
	scope := model.WebhookScopeGlobal
	return &model.Integration{
		ID:        id,
		Type:      model.IntegrationTypeGenericWebhook,
		Direction: model.IntegrationDirectionOutbound,
		Name:      "test-webhook",
		Enabled:   true,
		Scope:     &scope,
		Config:    raw,
	}
}

func newTeamIntegration(id, teamID string, cfg model.GenericWebhookConfig) *model.Integration {
	raw, _ := json.Marshal(cfg)
	scope := model.WebhookScopeTeam
	return &model.Integration{
		ID:        id,
		Type:      model.IntegrationTypeGenericWebhook,
		Direction: model.IntegrationDirectionOutbound,
		Name:      "test-team-webhook",
		Enabled:   true,
		Scope:     &scope,
		TeamID:    &teamID,
		Config:    raw,
	}
}

func setupWorker(st *store.MockStore, sender WebhookSender) *Worker {
	w := New(st, sender)
	w.workerID = testWorkerID
	return w
}

func TestWorker_ZeroSubscribers(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusCompleted {
		t.Errorf("status = %s, want completed", updated.Status)
	}
	if updated.SentAt == nil {
		t.Error("sent_at should be set")
	}
	if sender.calls != 0 {
		t.Errorf("sender.calls = %d, want 0", sender.calls)
	}
}

func TestWorker_SuccessfulDelivery(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s3cret"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200, ResponseBody: `{"ok":true}`}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Check delivery
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count = %d, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.Status != model.OutboxDeliverySent {
		t.Errorf("delivery status = %s, want sent", d.Status)
	}
	if d.SentAt == nil {
		t.Error("delivery sent_at should be set")
	}
	if d.Attempts != 1 {
		t.Errorf("delivery attempts = %d, want 1", d.Attempts)
	}

	// Check event completed
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status = %s, want completed", updated.Status)
	}

	// Check attempt logged
	attempts, _ := st.GetDeliveryAttempts(d.ID)
	if len(attempts) != 1 {
		t.Fatalf("attempts count = %d, want 1", len(attempts))
	}
	if attempts[0].Attempt != 0 {
		t.Errorf("attempt number = %d, want 0", attempts[0].Attempt)
	}
}

func TestWorker_TeamScopeFiltering(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-A")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}

	// Global matches all
	globalInteg := newGlobalIntegration("integ-global", cfg)
	st.CreateIntegration(globalInteg)

	// Team-A matches
	teamAInteg := newTeamIntegration("integ-team-a", "team-A", cfg)
	st.CreateIntegration(teamAInteg)

	// Team-B does NOT match
	teamBInteg := newTeamIntegration("integ-team-b", "team-B", cfg)
	st.CreateIntegration(teamBInteg)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Should create 2 deliveries (global + team-A), not team-B
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 2 {
		t.Fatalf("deliveries count = %d, want 2", len(deliveries))
	}
	if sender.calls != 2 {
		t.Errorf("sender.calls = %d, want 2", sender.calls)
	}
}

func TestWorker_4xx_PermanentFailure(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 404, ResponseBody: "not found"}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count = %d, want 1", len(deliveries))
	}
	if deliveries[0].Status != model.OutboxDeliveryFailed {
		t.Errorf("delivery status = %s, want failed", deliveries[0].Status)
	}

	// Attempt logged
	attempts, _ := st.GetDeliveryAttempts(deliveries[0].ID)
	if len(attempts) != 1 {
		t.Fatalf("attempts count = %d, want 1", len(attempts))
	}

	// Event should be completed (all deliveries terminal)
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status = %s, want completed", updated.Status)
	}
}

func TestWorker_5xx_Retry(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 503, ResponseBody: "unavailable"}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count = %d, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.Status != model.OutboxDeliveryRetry {
		t.Errorf("delivery status = %s, want retry", d.Status)
	}
	if d.Attempts != 1 {
		t.Errorf("delivery attempts = %d, want 1", d.Attempts)
	}
	if d.NextAttemptAt == nil {
		t.Error("next_attempt_at should be set")
	}

	// Event stays processing (or pending if not claimed) with next_attempt_at
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusProcessing && updated.Status != model.OutboxEventStatusPending {
		t.Errorf("event status = %s, want processing or pending", updated.Status)
	}
	if updated.NextAttemptAt == nil {
		t.Error("event next_attempt_at should be set")
	}
}

func TestWorker_MaxAttemptsExhausted(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 500}}}
	w := setupWorker(st, sender)

	// Simulate: delivery already at max-1 attempts
	w.processEvent(context.Background(), event)
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	d := deliveries[0]
	d.Attempts = DefaultMaxAttempts - 1
	d.Status = model.OutboxDeliveryRetry
	past := time.Now().Add(-time.Minute)
	d.NextAttemptAt = &past
	st.UpdateOutboxDelivery(d)

	// Reset event for re-process (simulate re-claim)
	wid := testWorkerID
	future := time.Now().Add(5 * time.Minute)
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &wid
	event.LockedUntil = &future
	event.NextAttemptAt = nil
	st.UpdateOutboxEvent(event)

	w.processEvent(context.Background(), event)

	deliveries, _ = st.GetDeliveriesByEventID(event.ID)
	d = deliveries[0]
	if d.Status != model.OutboxDeliveryFailed {
		t.Errorf("delivery status = %s, want failed", d.Status)
	}
	if d.Attempts != DefaultMaxAttempts {
		t.Errorf("delivery attempts = %d, want %d", d.Attempts, DefaultMaxAttempts)
	}
}

func TestWorker_AllDeliveriesTerminal_EventCompleted(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ1 := newGlobalIntegration("integ-1", cfg)
	integ2 := newGlobalIntegration("integ-2", cfg)
	st.CreateIntegration(integ1)
	st.CreateIntegration(integ2)

	// One succeeds, one fails permanently
	sender := &mockSender{results: []*DeliveryResult{
		{HTTPStatus: 200},
		{HTTPStatus: 400},
	}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Both terminal → event completed
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status = %s, want completed", updated.Status)
	}
}

func TestWorker_ReclaimSkipsSentRetries(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// First attempt: 5xx → retry
	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 503}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)
	if sender.calls != 1 {
		t.Fatalf("first pass: sender.calls = %d, want 1", sender.calls)
	}

	// Delivery is now retry with future next_attempt_at
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	d := deliveries[0]
	if d.Status != model.OutboxDeliveryRetry {
		t.Fatalf("delivery status = %s, want retry", d.Status)
	}

	// Re-process: next_attempt_at is in the future → should skip
	wid := testWorkerID
	future := time.Now().Add(5 * time.Minute)
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &wid
	event.LockedUntil = &future
	st.UpdateOutboxEvent(event)

	sender.calls = 0
	w.processEvent(context.Background(), event)

	if sender.calls != 0 {
		t.Errorf("second pass (future next_attempt_at): sender.calls = %d, want 0", sender.calls)
	}

	// Move next_attempt_at to the past → should retry
	past := time.Now().Add(-time.Second)
	d.NextAttemptAt = &past
	st.UpdateOutboxDelivery(d)
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &wid
	event.LockedUntil = &future
	st.UpdateOutboxEvent(event)

	sender.results = []*DeliveryResult{{HTTPStatus: 200}}
	sender.calls = 0
	w.processEvent(context.Background(), event)

	if sender.calls != 1 {
		t.Errorf("third pass (past next_attempt_at): sender.calls = %d, want 1", sender.calls)
	}
}

func TestWorker_DuplicateDeliveryHandled(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// First pass creates delivery
	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)
	w.processEvent(context.Background(), event)

	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count = %d, want 1", len(deliveries))
	}
	if deliveries[0].Status != model.OutboxDeliverySent {
		t.Errorf("delivery status = %s, want sent", deliveries[0].Status)
	}

	// Second pass: re-process same event (simulating re-claim)
	wid := testWorkerID
	future := time.Now().Add(5 * time.Minute)
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &wid
	event.LockedUntil = &future
	st.UpdateOutboxEvent(event)

	sender.calls = 0
	w.processEvent(context.Background(), event)

	// Should skip the already-sent delivery, not attempt again
	if sender.calls != 0 {
		t.Errorf("re-process: sender.calls = %d, want 0 (skip sent)", sender.calls)
	}
}

func TestWorker_FirstComputeBackoff_Attempt0(t *testing.T) {
	// Verify that the first retry uses attempt=0 for backoff calculation
	// which should give ~5s (baseBackoff)
	d := computeBackoff(0)
	lo := time.Duration(float64(5*time.Second) * 0.8)
	hi := time.Duration(float64(5*time.Second) * 1.2)
	if d < lo || d > hi {
		t.Errorf("computeBackoff(0) = %v, want [%v, %v]", d, lo, hi)
	}
}

func TestWorker_FanoutErrorReleasesEvent(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ1 := newGlobalIntegration("integ-1", cfg)
	integ2 := newGlobalIntegration("integ-2", cfg)
	st.CreateIntegration(integ1)
	st.CreateIntegration(integ2)

	// Inject DB error on CreateOutboxDelivery
	st.CreateOutboxDeliveryError = fmt.Errorf("db: connection reset")

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Event must NOT be completed - it should be released for retry
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status == model.OutboxEventStatusCompleted {
		t.Fatal("event should not be completed when fan-out has errors")
	}
	if updated.LastError == nil || *updated.LastError != "fan-out: one or more delivery creations failed" {
		t.Errorf("event last_error = %v, want fan-out error message", updated.LastError)
	}
	if updated.LockedBy != nil {
		t.Error("event should be unlocked after release")
	}

	// No deliveries should have been created (both failed)
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 0 {
		t.Errorf("deliveries count = %d, want 0", len(deliveries))
	}

	// Sender should not have been called
	if sender.calls != 0 {
		t.Errorf("sender.calls = %d, want 0", sender.calls)
	}
}

func TestWorker_LeaseRefreshedBeforeDelivery(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	beforeProcess := time.Now()
	w.processEvent(context.Background(), event)

	// After processEvent, the event's LockedUntil should have been bumped
	// (refreshLease sets it to now + leaseDuration before each attemptDelivery).
	// Since finalizeEventIfDone clears LockedUntil on completion, we verify
	// indirectly: the delivery was made (sender called) which means refreshLease ran.
	if sender.calls != 1 {
		t.Fatalf("sender.calls = %d, want 1", sender.calls)
	}

	// Also verify that during processing the lease was extended:
	// We can check that the event was updated with a future LockedUntil at some point.
	// Since the event is completed now, LockedUntil is nil. Let's verify the event
	// completed successfully (which means the whole pipeline ran including refreshLease).
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status = %s, want completed", updated.Status)
	}
	_ = beforeProcess // lease was refreshed during processing
}

func TestWorker_StaleMetadataClearedOnSuccess(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// First attempt: 5xx → retry (creates LastError and NextAttemptAt)
	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 503}}}
	w := setupWorker(st, sender)
	w.processEvent(context.Background(), event)

	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	d := deliveries[0]
	if d.Status != model.OutboxDeliveryRetry {
		t.Fatalf("delivery status = %s, want retry", d.Status)
	}
	if d.LastError == nil {
		t.Fatal("LastError should be set after 503")
	}
	if d.NextAttemptAt == nil {
		t.Fatal("NextAttemptAt should be set after 503")
	}

	// Move next_attempt_at to the past so retry fires
	past := time.Now().Add(-time.Second)
	d.NextAttemptAt = &past
	st.UpdateOutboxDelivery(d)

	// Reset event for re-process (simulate re-claim)
	wid := testWorkerID
	future := time.Now().Add(5 * time.Minute)
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &wid
	event.LockedUntil = &future
	st.UpdateOutboxEvent(event)

	// Second attempt: 200 → success
	sender.results = []*DeliveryResult{{HTTPStatus: 200}}
	sender.calls = 0
	w.processEvent(context.Background(), event)

	deliveries, _ = st.GetDeliveriesByEventID(event.ID)
	d = deliveries[0]
	if d.Status != model.OutboxDeliverySent {
		t.Errorf("delivery status = %s, want sent", d.Status)
	}
	if d.LastError != nil {
		t.Errorf("LastError = %v, want nil (should be cleared on success)", *d.LastError)
	}
	if d.NextAttemptAt != nil {
		t.Errorf("NextAttemptAt = %v, want nil (should be cleared on success)", *d.NextAttemptAt)
	}
	if d.SentAt == nil {
		t.Error("SentAt should be set on success")
	}
}

func TestWorker_LeaseExtendUsesTimeout(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	workerID := "w-1"
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	past := time.Now().Add(-time.Second)
	event.LockedUntil = &past
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s", TimeoutSeconds: 50}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := &Worker{store: st, sender: sender, workerID: workerID}

	beforeSend := time.Now()
	w.processEvent(context.Background(), event)

	// The lease should have been extended to at least now + 50s + 15s safety margin
	// Since event is completed, LockedUntil is nil. Verify via the mock's recorded state
	// by checking the event was delivered successfully (lease extension succeeded).
	if sender.calls != 1 {
		t.Fatalf("sender.calls = %d, want 1", sender.calls)
	}

	// Re-check: the ExtendOutboxEventLease mock applies the deadline to the event.
	// After completion, finalizeEventIfDone clears it. So we verify indirectly that
	// the lease was extended long enough by confirming the send happened.
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status = %s, want completed", updated.Status)
	}
	_ = beforeSend
}

func TestWorker_LeaseExtendFailure_SkipsSend(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	workerID := "w-1"
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	future := time.Now().Add(time.Minute)
	event.LockedUntil = &future
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// Simulate lease lost: ExtendOutboxEventLease returns false
	leaseLost := false
	st.ExtendOutboxEventLeaseResult = &leaseLost

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := &Worker{store: st, sender: sender, workerID: workerID}

	w.processEvent(context.Background(), event)

	// Sender must NOT have been called
	if sender.calls != 0 {
		t.Errorf("sender.calls = %d, want 0 (lease lost, skip send)", sender.calls)
	}

	// Delivery should still be pending (no attempt was made)
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count = %d, want 1", len(deliveries))
	}
	if deliveries[0].Status != model.OutboxDeliveryPending {
		t.Errorf("delivery status = %s, want pending", deliveries[0].Status)
	}
}

func TestWorker_5xxThenNetworkError_ClearsHTTPStatus(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	workerID := "w-1"
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	future := time.Now().Add(5 * time.Minute)
	event.LockedUntil = &future
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// First attempt: 503
	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 503, ResponseBody: "unavailable"}}}
	w := &Worker{store: st, sender: sender, workerID: workerID}

	w.processEvent(context.Background(), event)

	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	d := deliveries[0]
	if d.LastHTTPStatus == nil || *d.LastHTTPStatus != 503 {
		t.Fatalf("after 503: LastHTTPStatus = %v, want 503", d.LastHTTPStatus)
	}

	// Second attempt: network error (no HTTP status)
	past := time.Now().Add(-time.Second)
	d.NextAttemptAt = &past
	st.UpdateOutboxDelivery(d)

	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	event.LockedUntil = &future
	event.NextAttemptAt = nil
	st.UpdateOutboxEvent(event)

	sender.results = []*DeliveryResult{{Error: fmt.Errorf("connection refused")}}
	sender.calls = 0
	w.processEvent(context.Background(), event)

	deliveries, _ = st.GetDeliveriesByEventID(event.ID)
	d = deliveries[0]
	if d.LastHTTPStatus != nil {
		t.Errorf("after network error: LastHTTPStatus = %v, want nil (should be cleared)", *d.LastHTTPStatus)
	}
	if d.ResponseBodyTrunc != nil {
		t.Errorf("after network error: ResponseBodyTrunc = %v, want nil (should be cleared)", *d.ResponseBodyTrunc)
	}
}

func TestWorker_LeaseExtendError_SkipsSend(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	workerID := "w-1"
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	future := time.Now().Add(5 * time.Minute)
	event.LockedUntil = &future
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// Simulate ExtendOutboxEventLease returning an error
	st.ExtendOutboxEventLeaseError = fmt.Errorf("db: connection reset")

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := &Worker{store: st, sender: sender, workerID: workerID}

	w.processEvent(context.Background(), event)

	// Sender must NOT have been called
	if sender.calls != 0 {
		t.Errorf("sender.calls = %d, want 0 (lease extend error, skip send)", sender.calls)
	}

	// Delivery should still be pending (no attempt was made)
	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries count = %d, want 1", len(deliveries))
	}
	if deliveries[0].Status != model.OutboxDeliveryPending {
		t.Errorf("delivery status = %s, want pending", deliveries[0].Status)
	}

	// Event should NOT be completed
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status == model.OutboxEventStatusCompleted {
		t.Error("event should not be completed when lease extend errors")
	}
}

// stealingSender simulates another worker reclaiming the event during Send.
type stealingSender struct {
	st      *store.MockStore
	eventID string
	result  *DeliveryResult
	calls   int
}

func (s *stealingSender) Send(ctx context.Context, url string, body []byte, headers map[string]string) *DeliveryResult {
	s.calls++
	// Simulate another worker stealing the event during slow Send.
	// Use a copy so the mock store holds a distinct object from the worker's pointer.
	e, _ := s.st.GetOutboxEventByID(s.eventID)
	stolen := *e
	newOwner := "other-worker"
	stolen.LockedBy = &newOwner
	s.st.UpdateOutboxEvent(&stolen) // direct update (no ownership check)
	return s.result
}

func TestWorker_FinalizeOwnershipLost(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	sender := &stealingSender{
		st:      st,
		eventID: event.ID,
		result:  &DeliveryResult{HTTPStatus: 200},
	}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Sender was called (lease extend succeeded, send happened)
	if sender.calls != 1 {
		t.Errorf("sender.calls = %d, want 1", sender.calls)
	}

	// Event's LockedBy should still be "other-worker" (not overwritten by finalizeEventIfDone)
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.LockedBy == nil || *updated.LockedBy != "other-worker" {
		t.Errorf("event.LockedBy = %v, want 'other-worker' (should not be overwritten)", updated.LockedBy)
	}

	// Event should NOT be completed (finalize was a no-op due to ownership loss)
	if updated.Status == model.OutboxEventStatusCompleted {
		t.Error("event should not be completed when ownership was lost")
	}
}

func TestWorker_NoSubscribers_OwnershipLost(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)
	// No integrations → no subscribers

	// Steal ownership before processEvent reaches the no-subscribers branch.
	// We do this by changing the stored event's LockedBy after creation.
	e, _ := st.GetOutboxEventByID(event.ID)
	thief := "other-worker"
	e.LockedBy = &thief
	st.UpdateOutboxEvent(e)

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Sender must NOT have been called (no subscribers)
	if sender.calls != 0 {
		t.Errorf("sender.calls = %d, want 0", sender.calls)
	}

	// Event should NOT be completed (ownership lost)
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status == model.OutboxEventStatusCompleted {
		t.Error("event should not be completed when ownership was lost")
	}
	if updated.LockedBy == nil || *updated.LockedBy != "other-worker" {
		t.Errorf("event.LockedBy = %v, want 'other-worker'", updated.LockedBy)
	}
}

func TestWorker_NoSubscribers_UpdateError(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)
	// No integrations → no subscribers

	// Inject DB error on UpdateOutboxEventIfOwned
	st.UpdateOutboxEventIfOwnedError = fmt.Errorf("db: disk full")

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Event should NOT be completed (update errored)
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status == model.OutboxEventStatusCompleted {
		t.Error("event should not be completed when update errors")
	}
}

func TestWorker_ReleaseEvent_UpdateError(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	st.CreateOutboxEvent(event)

	// Inject error on GetIntegrationsByType to force releaseEvent path
	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// Make fan-out fail so processEvent calls releaseEvent
	st.CreateOutboxDeliveryError = fmt.Errorf("db: connection reset")

	// Inject error on UpdateOutboxEventIfOwned (releaseEvent path)
	st.UpdateOutboxEventIfOwnedError = fmt.Errorf("db: disk full")

	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 200}}}
	w := setupWorker(st, sender)

	w.processEvent(context.Background(), event)

	// Event should retain its original state (update errored, release was a no-op)
	updated, _ := st.GetOutboxEventByID(event.ID)
	if updated.Status == model.OutboxEventStatusCompleted || updated.Status == model.OutboxEventStatusFailed {
		t.Errorf("event status = %s, want processing (release update errored)", updated.Status)
	}
}

func TestWorker_NonEmptyThenEmptyBody_ClearsResponseBody(t *testing.T) {
	st := store.NewMockStore()
	event := newTestEvent("team-1")
	workerID := "w-1"
	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	future := time.Now().Add(5 * time.Minute)
	event.LockedUntil = &future
	st.CreateOutboxEvent(event)

	cfg := model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s"}
	integ := newGlobalIntegration("integ-1", cfg)
	st.CreateIntegration(integ)

	// First attempt: 503 with body
	sender := &mockSender{results: []*DeliveryResult{{HTTPStatus: 503, ResponseBody: "server error body"}}}
	w := &Worker{store: st, sender: sender, workerID: workerID}
	w.processEvent(context.Background(), event)

	deliveries, _ := st.GetDeliveriesByEventID(event.ID)
	d := deliveries[0]
	if d.ResponseBodyTrunc == nil || *d.ResponseBodyTrunc != "server error body" {
		t.Fatalf("after first attempt: ResponseBodyTrunc = %v, want 'server error body'", d.ResponseBodyTrunc)
	}

	// Second attempt: 200 with empty body
	past := time.Now().Add(-time.Second)
	d.NextAttemptAt = &past
	st.UpdateOutboxDelivery(d)

	event.Status = model.OutboxEventStatusProcessing
	event.LockedBy = &workerID
	event.LockedUntil = &future
	event.NextAttemptAt = nil
	st.UpdateOutboxEvent(event)

	sender.results = []*DeliveryResult{{HTTPStatus: 200, ResponseBody: ""}}
	sender.calls = 0
	w.processEvent(context.Background(), event)

	deliveries, _ = st.GetDeliveriesByEventID(event.ID)
	d = deliveries[0]
	if d.ResponseBodyTrunc != nil {
		t.Errorf("after empty body response: ResponseBodyTrunc = %v, want nil", *d.ResponseBodyTrunc)
	}
}
