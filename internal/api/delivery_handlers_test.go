package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func setupDeliveryTestEnv(t *testing.T) (*store.MockStore, *echo.Echo, string, string, string) {
	t.Helper()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	// Create a generic_webhook integration
	scope := model.WebhookScopeGlobal
	integ := &model.Integration{
		Type:      model.IntegrationTypeGenericWebhook,
		Direction: model.IntegrationDirectionOutbound,
		Name:      "Test Webhook",
		Enabled:   true,
		Scope:     &scope,
		Config:    json.RawMessage(`{"url":"https://example.com/hook","secret":"s3cret"}`),
	}
	if err := s.CreateIntegration(integ); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	// Create an outbox event
	event := &model.OutboxEvent{
		ID:           "evt-1",
		EventType:    model.OutboxEventFiring,
		AlertGroupID: "ag-1",
		TeamID:       "devops",
		Payload:      json.RawMessage(`{"event":"alert_group.firing"}`),
		Status:       model.OutboxEventStatusCompleted,
	}
	if err := s.CreateOutboxEvent(event); err != nil {
		t.Fatalf("CreateOutboxEvent: %v", err)
	}

	// Create a delivery
	now := time.Now()
	httpOK := 200
	delivery := &model.OutboxDelivery{
		ID:             "del-1",
		EventID:        "evt-1",
		IntegrationID:  integ.ID,
		Status:         model.OutboxDeliverySent,
		Attempts:       1,
		LastHTTPStatus: &httpOK,
		SentAt:         &now,
	}
	if err := s.CreateOutboxDelivery(delivery); err != nil {
		t.Fatalf("CreateOutboxDelivery: %v", err)
	}

	// Create a delivery attempt
	if err := s.CreateDeliveryAttempt(&model.DeliveryAttempt{
		DeliveryID: "del-1",
		Attempt:    0,
		HTTPStatus: &httpOK,
	}); err != nil {
		t.Fatalf("CreateDeliveryAttempt: %v", err)
	}

	return s, e, integ.ID, delivery.ID, event.ID
}

// --- ListIntegrationDeliveries ---

func TestListIntegrationDeliveries(t *testing.T) {
	_, e, integID, _, _ := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+integID+"/deliveries", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var resp DeliveryListResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Deliveries) != 1 {
		t.Errorf("deliveries count = %d, want 1", len(resp.Deliveries))
	}
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
	if resp.Page != 1 {
		t.Errorf("page = %d, want 1", resp.Page)
	}
}

func TestListIntegrationDeliveries_Empty(t *testing.T) {
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	scope := model.WebhookScopeGlobal
	integ := &model.Integration{
		Type:    model.IntegrationTypeGenericWebhook,
		Name:    "Empty WH",
		Enabled: true,
		Scope:   &scope,
		Config:  json.RawMessage(`{"url":"https://example.com","secret":"s"}`),
	}
	s.CreateIntegration(integ)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+integ.ID+"/deliveries", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp DeliveryListResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Deliveries) != 0 {
		t.Errorf("deliveries count = %d, want 0", len(resp.Deliveries))
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestListIntegrationDeliveries_Pagination(t *testing.T) {
	s, e, integID, _, _ := setupDeliveryTestEnv(t)

	// Add 2 more deliveries (total 3) - need distinct event IDs for unique(event_id, integration_id)
	s.CreateOutboxEvent(&model.OutboxEvent{
		ID: "evt-2", EventType: model.OutboxEventFiring, AlertGroupID: "ag-2", TeamID: "devops",
		Payload: json.RawMessage(`{}`), Status: model.OutboxEventStatusCompleted,
	})
	s.CreateOutboxEvent(&model.OutboxEvent{
		ID: "evt-3", EventType: model.OutboxEventFiring, AlertGroupID: "ag-3", TeamID: "devops",
		Payload: json.RawMessage(`{}`), Status: model.OutboxEventStatusCompleted,
	})
	s.CreateOutboxDelivery(&model.OutboxDelivery{
		ID: "del-2", EventID: "evt-2", IntegrationID: integID, Status: model.OutboxDeliveryFailed,
	})
	s.CreateOutboxDelivery(&model.OutboxDelivery{
		ID: "del-3", EventID: "evt-3", IntegrationID: integID, Status: model.OutboxDeliveryRetry,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+integID+"/deliveries?page=1&limit=2", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp DeliveryListResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 3 {
		t.Errorf("total = %d, want 3", resp.Total)
	}
	if !resp.HasNext {
		t.Error("has_next should be true")
	}
	if resp.HasPrev {
		t.Error("has_prev should be false")
	}
}

func TestListIntegrationDeliveries_NotFound(t *testing.T) {
	_, e, _, _, _ := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/nonexistent/deliveries", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- GetDeliveryDetail ---

func TestGetDeliveryDetail(t *testing.T) {
	_, e, integID, delID, _ := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+integID+"/deliveries/"+delID, nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var resp DeliveryDetailResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Delivery == nil {
		t.Fatal("delivery should not be nil")
	}
	if resp.Delivery.ID != delID {
		t.Errorf("delivery.ID = %s, want %s", resp.Delivery.ID, delID)
	}
	if len(resp.Attempts) != 1 {
		t.Errorf("attempts count = %d, want 1", len(resp.Attempts))
	}
}

func TestGetDeliveryDetail_NotFound(t *testing.T) {
	_, e, integID, _, _ := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+integID+"/deliveries/nonexistent", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestGetDeliveryDetail_IDOR(t *testing.T) {
	s, e, _, delID, _ := setupDeliveryTestEnv(t)

	// Create a second integration
	scope := model.WebhookScopeGlobal
	other := &model.Integration{
		Type: model.IntegrationTypeGenericWebhook, Name: "Other",
		Enabled: true, Scope: &scope,
		Config: json.RawMessage(`{"url":"https://other.com","secret":"s"}`),
	}
	s.CreateIntegration(other)

	// Try to access del-1 (belongs to integ-1) via other integration
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+other.ID+"/deliveries/"+delID, nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (IDOR protection)", rec.Code)
	}
}

// --- ReplayDelivery ---

func TestReplayDelivery_Sent(t *testing.T) {
	s, e, integID, delID, _ := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	var resp ReplayDeliveryResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK {
		t.Error("ok should be true")
	}

	// Delivery should be reset to pending
	delivery, _ := s.GetOutboxDeliveryByID(delID)
	if delivery.Status != model.OutboxDeliveryPending {
		t.Errorf("delivery status = %s, want pending", delivery.Status)
	}
	if delivery.Attempts != 0 {
		t.Errorf("delivery attempts = %d, want 0", delivery.Attempts)
	}
	if delivery.SentAt != nil {
		t.Error("delivery sent_at should be nil")
	}
	if delivery.LastHTTPStatus != nil {
		t.Error("delivery last_http_status should be nil")
	}
}

func TestReplayDelivery_Failed(t *testing.T) {
	s, e, integID, delID, _ := setupDeliveryTestEnv(t)

	// Set delivery to failed
	delivery, _ := s.GetOutboxDeliveryByID(delID)
	delivery.Status = model.OutboxDeliveryFailed
	errMsg := "HTTP 500"
	delivery.LastError = &errMsg
	s.UpdateOutboxDelivery(delivery)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	updated, _ := s.GetOutboxDeliveryByID(delID)
	if updated.Status != model.OutboxDeliveryPending {
		t.Errorf("delivery status = %s, want pending", updated.Status)
	}
}

func TestReplayDelivery_Pending(t *testing.T) {
	s, e, integID, delID, _ := setupDeliveryTestEnv(t)

	// Set delivery to pending (in progress)
	delivery, _ := s.GetOutboxDeliveryByID(delID)
	delivery.Status = model.OutboxDeliveryPending
	s.UpdateOutboxDelivery(delivery)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestReplayDelivery_ReopensEvent(t *testing.T) {
	s, e, integID, delID, eventID := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Parent event should be re-opened
	event, _ := s.GetOutboxEventByID(eventID)
	if event.Status != model.OutboxEventStatusProcessing {
		t.Errorf("event status = %s, want processing", event.Status)
	}
	if event.SentAt != nil {
		t.Error("event sent_at should be nil after replay")
	}
}

func TestReplayDelivery_EventAlreadyProcessing(t *testing.T) {
	s, e, integID, delID, eventID := setupDeliveryTestEnv(t)

	// Set event to processing (not terminal)
	event, _ := s.GetOutboxEventByID(eventID)
	event.Status = model.OutboxEventStatusProcessing
	s.UpdateOutboxEvent(event)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Event should still be processing (not changed)
	updated, _ := s.GetOutboxEventByID(eventID)
	if updated.Status != model.OutboxEventStatusProcessing {
		t.Errorf("event status = %s, want processing (unchanged)", updated.Status)
	}
}

func TestReplayDelivery_IDOR(t *testing.T) {
	s, e, _, delID, _ := setupDeliveryTestEnv(t)

	scope := model.WebhookScopeGlobal
	other := &model.Integration{
		Type: model.IntegrationTypeGenericWebhook, Name: "Other",
		Enabled: true, Scope: &scope,
		Config: json.RawMessage(`{"url":"https://other.com","secret":"s"}`),
	}
	s.CreateIntegration(other)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+other.ID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (IDOR protection)", rec.Code)
	}
}

func TestReplayDelivery_NotFound(t *testing.T) {
	_, e, integID, _, _ := setupDeliveryTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/nonexistent/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestReplayDelivery_StoreError(t *testing.T) {
	s, e, integID, delID, _ := setupDeliveryTestEnv(t)

	// Inject error into the atomic replay method
	s.ReplayOutboxDeliveryError = fmt.Errorf("tx commit failed")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integID+"/deliveries/"+delID+"/replay", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}

	// Delivery should NOT be reset (error was injected)
	delivery, _ := s.GetOutboxDeliveryByID(delID)
	if delivery.Status != model.OutboxDeliverySent {
		t.Errorf("delivery status = %s, want sent (unchanged)", delivery.Status)
	}
}
