package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

func addAuth(req *http.Request, userID string) {
	if userID == "" {
		userID = "denis"
	}
	token, _ := auth.GenerateToken(userID)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
}

// scheduleTestEnv exposes the doubles behind the revision model so a test can
// seed revisions and override the clock. Membership and users live in the mock
// store; only the revision model lives in the fake.
type scheduleTestEnv struct {
	Config  *fakes.ScheduleConfigRepo
	Erasure *testErasureRepo

	// now is read through a pointer so a test can move the clock after the
	// services are built.
	now *time.Time
}

// SetNow moves the clock every schedule command and the preview read.
func (env *scheduleTestEnv) SetNow(at time.Time) { *env.now = at }

func setupTestAPI(t *testing.T) (*API, *store.MockStore, *echo.Echo) {
	api, s, e, _ := setupScheduleAPI(t)
	return api, s, e
}

// setupScheduleAPI wires the API with the real schedule and erasure services
// over the test doubles. Every API test goes through here: the handlers refuse
// to run unwired, and a suite where half the endpoints answer 503 would prove
// nothing about them.
func setupScheduleAPI(t *testing.T) (*API, *store.MockStore, *echo.Echo, *scheduleTestEnv) {
	t.Helper()
	s := store.NewMockStore()

	api := NewAPI(s, nil, nil, nil, "", nil)

	now := time.Now().UTC()
	env := &scheduleTestEnv{
		Config:  fakes.NewScheduleConfigRepo(),
		Erasure: newTestErasureRepo(s),
		now:     &now,
	}
	clock := func() time.Time { return *env.now }
	repo := &testScheduleRepo{ScheduleConfigRepo: env.Config, store: s}

	api.SetScheduleConfigService(scheduleconfig.NewService(repo,
		scheduleconfig.WithClock(clock)))
	api.SetScheduleReadRepository(repo)
	api.SetScheduleRenderer(schedulerender.New(repo, schedulerender.WithClock(clock)))
	api.SetUserEraser(erasure.NewService(env.Erasure, erasure.WithClock(clock)))

	e := echo.New()
	api.RegisterRoutes(e)

	return api, s, e, env
}

func createTestAlertGroup(t *testing.T, s store.StoreInterface, id, alertKey string, status model.AlertGroupStatus) *model.AlertGroup {
	ag := &model.AlertGroup{
		ID:               id,
		AlertKey:         alertKey,
		Status:           status,
		Title:            "Test Alert Group",
		TeamID:           "devops",
		TeamNameSnapshot: "DevOps",
		Severity:         "critical",
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("Failed to create test alert group: %v", err)
	}
	return ag
}

func TestListAlertGroups(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	t.Run("Empty List", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var resp AlertGroupListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if len(resp.AlertGroups) != 0 {
			t.Errorf("Expected 0 alert groups, got %d", len(resp.AlertGroups))
		}
	})

	// Create test alert groups
	createTestAlertGroup(t, s, "ag-1", "dedup-1", model.AlertGroupStatusNew)
	createTestAlertGroup(t, s, "ag-2", "dedup-2", model.AlertGroupStatusProcessing)

	t.Run("List All", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var resp AlertGroupListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if len(resp.AlertGroups) != 2 {
			t.Errorf("Expected 2 alert groups, got %d", len(resp.AlertGroups))
		}
	})

	t.Run("Filter By Status", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?status=new", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var resp AlertGroupListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if len(resp.AlertGroups) != 1 {
			t.Errorf("Expected 1 alert group with status 'new', got %d", len(resp.AlertGroups))
		}
	})

	t.Run("Pagination", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?limit=1&offset=0", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var resp AlertGroupListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if len(resp.AlertGroups) != 1 {
			t.Errorf("Expected 1 alert group with limit=1, got %d", len(resp.AlertGroups))
		}
	})

	t.Run("Legacy Incidents Endpoint", func(t *testing.T) {
		// Test that /incidents still works as an alias
		req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200 for legacy /incidents, got %d", rec.Code)
		}
	})
}

func TestGetAlertGroup(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-get-1", "dedup-get-1", model.AlertGroupStatusNew)

	t.Run("Found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups/ag-get-1", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var ag model.AlertGroup
		if err := json.Unmarshal(rec.Body.Bytes(), &ag); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if ag.ID != "ag-get-1" {
			t.Errorf("Expected ID 'ag-get-1', got '%s'", ag.ID)
		}
	})

	t.Run("Not Found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups/nonexistent", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})
}

func TestAckAlertGroup(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-ack-1", "dedup-ack-1", model.AlertGroupStatusTriggered)

	t.Run("Acknowledge", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-1/ack", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		var ag model.AlertGroup
		if err := json.Unmarshal(rec.Body.Bytes(), &ag); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if ag.Status != model.AlertGroupStatusAcknowledged {
			t.Errorf("Expected status 'acknowledged', got '%s'", ag.Status)
		}

		// Verify in DB
		dbAg, _ := s.GetAlertGroupByID("ag-ack-1")
		if dbAg.Status != model.AlertGroupStatusAcknowledged {
			t.Errorf("DB status should be 'acknowledged', got '%s'", dbAg.Status)
		}
	})

	t.Run("Idempotent - already acked returns 200", func(t *testing.T) {
		// ag-ack-1 was acked above; second ack should be idempotent 200
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-1/ack", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200 on idempotent ack, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		// Verify only 1 ack timeline event (no duplicate)
		events, _ := s.GetTimelineEvents("ag-ack-1")
		ackCount := 0
		for _, ev := range events {
			if ev.Type == model.TimelineEventAcknowledged {
				ackCount++
			}
		}
		if ackCount != 1 {
			t.Errorf("Expected exactly 1 ack timeline event, got %d", ackCount)
		}
	})

	t.Run("Not Found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/nonexistent/ack", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})
}

func TestResolveAlertGroup(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-res-1", "dedup-res-1", model.AlertGroupStatusTriggered)

	t.Run("Resolve", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-res-1/resolve", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		var ag model.AlertGroup
		if err := json.Unmarshal(rec.Body.Bytes(), &ag); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if ag.Status != model.AlertGroupStatusResolved {
			t.Errorf("Expected status 'resolved', got '%s'", ag.Status)
		}
		if ag.ResolvedBy != "Denis" {
			t.Errorf("Expected resolved_by 'Denis', got %q", ag.ResolvedBy)
		}

		// Verify in DB
		dbAg, _ := s.GetAlertGroupByID("ag-res-1")
		if dbAg.Status != model.AlertGroupStatusResolved {
			t.Errorf("DB status should be 'resolved', got '%s'", dbAg.Status)
		}
		if dbAg.ResolvedBy != "Denis" {
			t.Errorf("DB resolved_by should be 'Denis', got %q", dbAg.ResolvedBy)
		}
	})

	t.Run("Not Found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/nonexistent/resolve", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("Idempotent - already resolved does not re-observe MTTR", func(t *testing.T) {
		// ag-res-1 was resolved above; get MTTR histogram count before second resolve
		before := mttrHistogramCount(t, "devops", "critical", "none")

		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-res-1/resolve", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200 on idempotent resolve, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		after := mttrHistogramCount(t, "devops", "critical", "none")
		if after != before {
			t.Errorf("MTTR histogram should not increment on repeated resolve, got %d -> %d", before, after)
		}

		// Verify resolved_at was not overwritten (DB should retain original timestamp)
		dbAg, _ := s.GetAlertGroupByID("ag-res-1")
		if dbAg.ResolvedAt == nil {
			t.Error("resolved_at should still be set")
		}
	})

	t.Run("Idempotent - closed alert group returns OK without status change", func(t *testing.T) {
		createTestAlertGroup(t, s, "ag-closed-1", "dedup-closed-1", model.AlertGroupStatusClosed)

		before := mttrHistogramCount(t, "devops", "critical", "none")

		req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-closed-1/resolve", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200 for closed AG, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		var ag model.AlertGroup
		if err := json.Unmarshal(rec.Body.Bytes(), &ag); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if ag.Status != model.AlertGroupStatusClosed {
			t.Errorf("Expected status to remain 'closed', got '%s'", ag.Status)
		}

		after := mttrHistogramCount(t, "devops", "critical", "none")
		if after != before {
			t.Errorf("MTTR histogram should not increment for closed AG, got %d -> %d", before, after)
		}

		// Verify DB status was not changed
		dbAg, _ := s.GetAlertGroupByID("ag-closed-1")
		if dbAg.Status != model.AlertGroupStatusClosed {
			t.Errorf("DB status should remain 'closed', got '%s'", dbAg.Status)
		}
	})
}

func TestAckAlertGroup_FromProcessing(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-ack-proc", "dedup-ack-proc", model.AlertGroupStatusProcessing)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-proc/ack", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	var ag model.AlertGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &ag); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status 'acknowledged', got '%s'", ag.Status)
	}

	dbAg, _ := s.GetAlertGroupByID("ag-ack-proc")
	if dbAg.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("DB status should be 'acknowledged', got '%s'", dbAg.Status)
	}
}

func TestResolveAlertGroup_FromProcessing(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-res-proc", "dedup-res-proc", model.AlertGroupStatusProcessing)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-res-proc/resolve", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	var ag model.AlertGroup
	if err := json.Unmarshal(rec.Body.Bytes(), &ag); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if ag.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", ag.Status)
	}

	dbAg, _ := s.GetAlertGroupByID("ag-res-proc")
	if dbAg.Status != model.AlertGroupStatusResolved {
		t.Errorf("DB status should be 'resolved', got '%s'", dbAg.Status)
	}
}

func TestAckAlertGroup_OutboxEvent(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-ack-outbox", "dedup-ack-outbox", model.AlertGroupStatusTriggered)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-outbox/ack", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	// Verify outbox event was created
	events, err := s.GetPendingOutboxEvents(10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents: %v", err)
	}

	var found *model.OutboxEvent
	for _, ev := range events {
		if ev.AlertGroupID == "ag-ack-outbox" && ev.EventType == model.OutboxEventAcknowledged {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatal("Expected outbox event for acknowledged AG")
	}

	// Verify payload
	var payload model.WebhookEventPayload
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}
	if payload.AlertGroup.Status != "acknowledged" {
		t.Errorf("Expected payload status 'acknowledged', got %q", payload.AlertGroup.Status)
	}
	if payload.AlertGroup.TeamName != "DevOps" {
		t.Errorf("Expected team_name 'DevOps', got %q", payload.AlertGroup.TeamName)
	}
	if payload.Actor.Name != "Denis" {
		t.Errorf("Expected actor.name 'Denis', got %q", payload.Actor.Name)
	}
	if payload.Actor.Email != "denis@example.com" {
		t.Errorf("Expected actor.email 'denis@example.com', got %q", payload.Actor.Email)
	}
}

func TestAckAlertGroup_IdempotentNoOutbox(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-ack-idem", "dedup-ack-idem", model.AlertGroupStatusTriggered)

	// First ack
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-idem/ack", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("First ack: expected 200, got %d", rec.Code)
	}

	// Count outbox events
	events1, _ := s.GetPendingOutboxEvents(100)
	ackCount1 := 0
	for _, ev := range events1 {
		if ev.AlertGroupID == "ag-ack-idem" && ev.EventType == model.OutboxEventAcknowledged {
			ackCount1++
		}
	}

	// Second ack (idempotent)
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-idem/ack", nil)
	addAuth(req, "denis")
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Second ack: expected 200, got %d", rec.Code)
	}

	// Count again — should be same
	events2, _ := s.GetPendingOutboxEvents(100)
	ackCount2 := 0
	for _, ev := range events2 {
		if ev.AlertGroupID == "ag-ack-idem" && ev.EventType == model.OutboxEventAcknowledged {
			ackCount2++
		}
	}
	if ackCount2 != ackCount1 {
		t.Errorf("Expected no additional outbox event on idempotent ack, got %d -> %d", ackCount1, ackCount2)
	}
}

func TestResolveAlertGroup_OutboxEvent(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	createTestAlertGroup(t, s, "ag-res-outbox", "dedup-res-outbox", model.AlertGroupStatusTriggered)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-res-outbox/resolve", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	events, err := s.GetPendingOutboxEvents(10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents: %v", err)
	}

	var found *model.OutboxEvent
	for _, ev := range events {
		if ev.AlertGroupID == "ag-res-outbox" && ev.EventType == model.OutboxEventResolved {
			found = ev
			break
		}
	}
	if found == nil {
		t.Fatal("Expected outbox event for resolved AG")
	}

	var payload model.WebhookEventPayload
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}
	if payload.AlertGroup.Status != "resolved" {
		t.Errorf("Expected payload status 'resolved', got %q", payload.AlertGroup.Status)
	}
	if payload.AlertGroup.TeamName != "DevOps" {
		t.Errorf("Expected team_name 'DevOps', got %q", payload.AlertGroup.TeamName)
	}
	if payload.Actor.Name != "Denis" {
		t.Errorf("Expected actor.name 'Denis', got %q", payload.Actor.Name)
	}
	if payload.Actor.Email != "denis@example.com" {
		t.Errorf("Expected actor.email 'denis@example.com', got %q", payload.Actor.Email)
	}
}

func TestAckAlertGroup_AlreadyAcked_NoTeamLookup(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	// AG already acknowledged — service short-circuits, no team lookup needed
	ag := &model.AlertGroup{
		ID: "ag-ack-noop", AlertKey: "dedup-ack-noop",
		Status: model.AlertGroupStatusAcknowledged, Title: "Test",
		TeamID: "nonexistent-team", TeamNameSnapshot: "Ghost Team", Severity: "critical",
	}
	s.CreateAlertGroup(ag)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-ack-noop/ack", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200 for already-acked AG, got %d. Body: %s", rec.Code, rec.Body.String())
	}
}

func TestResolveAlertGroup_AlreadyClosed_NoTeamLookup(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	// AG already closed — service short-circuits, no team lookup needed
	ag := &model.AlertGroup{
		ID: "ag-res-noop", AlertKey: "dedup-res-noop",
		Status: model.AlertGroupStatusClosed, Title: "Test",
		TeamID: "nonexistent-team", TeamNameSnapshot: "Ghost Team", Severity: "critical",
	}
	s.CreateAlertGroup(ag)

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/alert-groups/ag-res-noop/resolve", nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200 for already-closed AG, got %d. Body: %s", rec.Code, rec.Body.String())
	}
}

// ========================================
// Users API Tests
// ========================================

func TestUsersAPI(t *testing.T) {
	api, s, e := setupTestAPI(t)
	defer s.Close()

	t.Run("ListUsers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var resp UserListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		// Seed users: denis, alex
		if len(resp.Users) != 2 {
			t.Errorf("Expected 2 seed users, got %d", len(resp.Users))
		}
	})

	t.Run("GetUser", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/users/denis", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var user model.User
		if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if user.Email != "denis@example.com" {
			t.Errorf("Expected email denis@example.com, got %s", user.Email)
		}
	})

	t.Run("CreateUser", func(t *testing.T) {
		body := `{"email": "new@test.com", "name": "New User"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
		addAuth(req, "denis")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		var user model.User
		if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if user.ID != "new" {
			t.Errorf("Expected ID 'new' (from email), got %s", user.ID)
		}
	})

	t.Run("CreateUser_Validation", func(t *testing.T) {
		body := `{"email": ""}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
		addAuth(req, "denis")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d", rec.Code)
		}
	})

	t.Run("UpdateUser ignores slack_user_id (linking is via /me/slack)", func(t *testing.T) {
		// Sprint 3: admin user CRUD no longer accepts slack_user_id; it's silently dropped
		// and the user response carries identities populated from external_identities (empty here).
		body := `{"name": "Renamed", "slack_user_id": "U999999"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/users/new", strings.NewReader(body))
		addAuth(req, "denis")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		var user model.User
		if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if len(user.Identities) != 0 {
			t.Errorf("Expected no linked identities, got %+v", user.Identities)
		}
	})

	t.Run("UpdateUserPassword", func(t *testing.T) {
		// Set new password
		body := `{"password": "NewPassword123!"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/users/new/password", strings.NewReader(body))
		addAuth(req, "denis")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		// Verify by checking login
		checkReqBody := `{"email":"new@test.com", "password":"NewPassword123!"}`
		checkReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(checkReqBody))
		checkReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		checkRec := httptest.NewRecorder()
		checkC := e.NewContext(checkReq, checkRec)

		if err := api.Login(checkC); err != nil {
			t.Fatalf("Login check failed: %v", err)
		}
		if checkRec.Code != http.StatusOK {
			t.Errorf("Expected 200 login with new password, got %d", checkRec.Code)
		}
	})

	t.Run("DeleteUser", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/new", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("Expected 204, got %d", rec.Code)
		}

		// Verify deleted
		req = httptest.NewRequest(http.MethodGet, "/api/v1/users/new", nil)
		addAuth(req, "denis")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404 after delete, got %d", rec.Code)
		}
	})

	t.Run("DeleteUser_NotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/nonexistent", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})
}

func TestTeamMembersAPI(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	t.Run("GetTeamMembers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams/devops/members", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}

		var resp TeamMemberListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		// denis and alex are in devops
		if len(resp.Users) != 2 {
			t.Errorf("Expected 2 team members, got %d", len(resp.Users))
		}
	})

	t.Run("AddTeamMember", func(t *testing.T) {
		// First create a new user
		newUser := &model.User{ID: "newmember", Email: "member@test.com", Name: "Member"}
		s.CreateUser(newUser)

		body := `{"user_id": "newmember", "role": "team_member"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/teams/devops/members", strings.NewReader(body))
		addAuth(req, "denis")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		// Verify added
		members, _ := s.GetTeamMembers("devops")
		found := false
		for _, m := range members {
			if m.ID == "newmember" {
				found = true
				break
			}
		}
		if !found {
			t.Error("New member not found in team")
		}
	})

	t.Run("AddTeamMember_UserNotFound", func(t *testing.T) {
		body := `{"user_id": "nonexistent"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/teams/devops/members", strings.NewReader(body))
		addAuth(req, "denis")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("RemoveTeamMember", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/teams/devops/members/newmember", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("Expected 204, got %d", rec.Code)
		}

		// Verify removed
		members, _ := s.GetTeamMembers("devops")
		for _, m := range members {
			if m.ID == "newmember" {
				t.Error("Member should be removed from team")
			}
		}
	})
}

func TestDeletedUserCannotAccess(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	// Create a user and generate a valid token for them
	user := &model.User{ID: "doomed", Email: "doomed@test.com", Name: "Doomed User"}
	s.CreateUser(user)
	token, err := auth.GenerateToken("doomed")
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}

	// Delete the user
	s.DeleteUser("doomed")

	// Try to access an authenticated endpoint with the deleted user's token
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 for deleted user, got %d. Body: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteTeamSchedule(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	t.Run("Success", func(t *testing.T) {
		created := createSchedule(t, e, []string{"denis"})

		req := httptest.NewRequest(http.MethodDelete,
			fmt.Sprintf("/api/v1/teams/devops/schedule?expected_version=%d", created.Version), nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("Expected 204, got %d: %s", rec.Code, rec.Body.String())
		}

		// The configuration endpoint still answers, with deleted_at set: the
		// editor needs the last valid configuration to prefill a recreate.
		req = httptest.NewRequest(http.MethodGet, "/api/v1/teams/devops/schedule/config", nil)
		addAuth(req, "denis")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200 after delete, got %d", rec.Code)
		}
		var cfg ScheduleConfigResponse
		decodeJSON(t, rec, &cfg)
		if cfg.DeletedAt == nil {
			t.Fatal("deleted schedule must report deleted_at")
		}
	})

	t.Run("AlreadyDeleted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete,
			"/api/v1/teams/devops/schedule?expected_version=2", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Fatalf("Expected 409 for an already deleted schedule, got %d: %s",
				rec.Code, rec.Body.String())
		}
	})

	t.Run("NoSchedule", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete,
			"/api/v1/teams/triage/schedule?expected_version=1", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("Expected 404 for a team with no schedule, got %d", rec.Code)
		}
	})

	t.Run("MissingExpectedVersion", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/teams/devops/schedule", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("Expected 400 without expected_version, got %d", rec.Code)
		}
	})
}

// mttrHistogramCount returns the current sample count of the MTTR histogram for the given labels.
func mttrHistogramCount(t *testing.T, team, severity, oncallUser string) uint64 {
	t.Helper()
	observer, err := metrics.AlertGroupResolutionDuration.GetMetricWithLabelValues(team, severity, oncallUser)
	if err != nil {
		t.Fatalf("failed to get MTTR metric: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write MTTR metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestPaginationMeta(t *testing.T) {
	_, s, e := setupTestAPI(t)
	defer s.Close()

	// Create 3 alert groups
	for i := 1; i <= 3; i++ {
		ag := &model.AlertGroup{
			ID:       fmt.Sprintf("pg-%d", i),
			AlertKey: fmt.Sprintf("dedup-pg-%d", i),
			Status:   model.AlertGroupStatusNew,
			Title:    fmt.Sprintf("Alert %d", i),
			TeamID:   "devops",
			Severity: "critical",
		}
		if err := s.CreateAlertGroup(ag); err != nil {
			t.Fatalf("Failed to create alert group: %v", err)
		}
	}

	t.Run("PageClamp", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?page=999&limit=2&view=summary", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		var resp AlertGroupSummaryListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if resp.Page != 2 {
			t.Errorf("Expected page=2, got %d", resp.Page)
		}
		if resp.TotalPages != 2 {
			t.Errorf("Expected total_pages=2, got %d", resp.TotalPages)
		}
		if len(resp.AlertGroups) != 1 {
			t.Errorf("Expected 1 alert group on last page, got %d", len(resp.AlertGroups))
		}
		if resp.HasNext {
			t.Error("Expected has_next=false")
		}
		if !resp.HasPrev {
			t.Error("Expected has_prev=true")
		}
	})

	t.Run("EmptyList", func(t *testing.T) {
		// Use a filter that matches nothing
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?statuses=resolved&view=summary", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		var resp AlertGroupSummaryListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		if resp.Page != 1 {
			t.Errorf("Expected page=1, got %d", resp.Page)
		}
		if resp.TotalPages != 1 {
			t.Errorf("Expected total_pages=1, got %d", resp.TotalPages)
		}
		if resp.HasNext || resp.HasPrev {
			t.Error("Expected has_next=false, has_prev=false")
		}
		if resp.Total != 0 {
			t.Errorf("Expected total=0, got %d", resp.Total)
		}
	})

	t.Run("LimitGuard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?limit=9999&view=summary", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		var resp AlertGroupSummaryListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}
		// With 3 items and limit clamped to 200, everything fits on page 1
		if resp.TotalPages != 1 {
			t.Errorf("Expected total_pages=1 (limit clamped to 200), got %d", resp.TotalPages)
		}
	})

	t.Run("DeterministicPaging", func(t *testing.T) {
		// All 3 have same severity=critical; sort by severity should use tie-breaker
		req1 := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?view=summary&sort=severity&sort_dir=desc&limit=2&page=1", nil)
		addAuth(req1, "denis")
		rec1 := httptest.NewRecorder()
		e.ServeHTTP(rec1, req1)

		req2 := httptest.NewRequest(http.MethodGet, "/api/v1/alert-groups?view=summary&sort=severity&sort_dir=desc&limit=2&page=2", nil)
		addAuth(req2, "denis")
		rec2 := httptest.NewRecorder()
		e.ServeHTTP(rec2, req2)

		var resp1, resp2 AlertGroupSummaryListResponse
		json.Unmarshal(rec1.Body.Bytes(), &resp1)
		json.Unmarshal(rec2.Body.Bytes(), &resp2)

		page1IDs := map[string]bool{}
		for _, ag := range resp1.AlertGroups {
			page1IDs[ag.ID] = true
		}
		for _, ag := range resp2.AlertGroups {
			if page1IDs[ag.ID] {
				t.Errorf("ID %s appears on both page 1 and page 2", ag.ID)
			}
		}
	})
}
