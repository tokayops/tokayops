package ingester

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// seedDefaultTeams creates the teams commonly used by ingester tests in the mock store.
func seedDefaultTeams(s *store.MockStore) {
	s.CreateTeam(&model.Team{ID: "devops", Name: "DevOps", CreatedAt: time.Now()})
	s.CreateTeam(&model.Team{ID: "triage", Name: "Triage", CreatedAt: time.Now()})
}

// mockSecretValidator implements WebhookSecretValidator for testing
type mockSecretValidator struct {
	secrets map[string]bool
}

func (m *mockSecretValidator) ValidateWebhookSecret(secret string) bool {
	// If no secrets configured, reject all (matches IntegrationCache behavior)
	if len(m.secrets) == 0 {
		return false
	}
	return m.secrets[secret]
}

func TestGenerateTitle(t *testing.T) {
	ingester := &Ingester{}

	tests := []struct {
		name     string
		payload  AMPayload
		expected string
	}{
		{
			name: "CommonLabel Alertname Present",
			payload: AMPayload{
				Status: "firing",
				CommonLabels: map[string]string{
					"alertname": "TestAlert",
				},
			},
			expected: "TestAlert",
		},
		{
			name: "Alert Label Alertname Present",
			payload: AMPayload{
				Status: "firing",
				Alerts: []model.Alert{
					{
						Labels: map[string]string{
							"alertname": "FallbackAlert",
						},
					},
				},
			},
			expected: "FallbackAlert",
		},
		{
			name: "No Alertname",
			payload: AMPayload{
				Status: "firing",
			},
			expected: "Unknown Alert Group",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ingester.generateTitle(&tt.payload)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestHandleWebhook(t *testing.T) {
	// Setup Dependencies using MockStore
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}

	// Mock webhook secret validator
	mockValidator := &mockSecretValidator{
		secrets: map[string]bool{"secret123": true},
	}

	ing := NewIngester(s, cfg, mockValidator)
	e := echo.New()
	ing.RegisterRoutes(e)

	t.Run("Unauthorized - wrong token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=wrong", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401, got %d", rec.Code)
		}
	})

	t.Run("Unauthorized - no integrations configured", func(t *testing.T) {
		// Create ingester with empty secrets (no integrations)
		emptyValidator := &mockSecretValidator{secrets: map[string]bool{}}
		ingNoSecrets := NewIngester(store.NewMockStore(), cfg, emptyValidator)
		eNoSecrets := echo.New()
		ingNoSecrets.RegisterRoutes(eNoSecrets)

		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=any", nil)
		rec := httptest.NewRecorder()
		eNoSecrets.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected 401 when no integrations configured, got %d", rec.Code)
		}
	})

	t.Run("Authorized Create", func(t *testing.T) {
		payload := `{"status":"firing","groupKey":"g1","alerts":[{"status":"firing","labels":{"alertname":"Test","team":"devops","severity":"critical"},"fingerprint":"f1"}]}`
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != "Created" {
			t.Errorf("Expected 'Created', got '%s'", rec.Body.String())
		}

		// Verify DB
		ag, err := s.GetActiveAlertGroupByAlertKey("g1")
		if err != nil || ag == nil {
			t.Fatal("Alert group not created in store")
		}
		if ag.Status != model.AlertGroupStatusNew {
			t.Errorf("Expected Status 'new', got '%s'", ag.Status)
		}
	})

	t.Run("Authorized Update (Merge)", func(t *testing.T) {
		// Existing alert group g1 (from previous test) is Active.
		// Send payload with resolved alert.
		payload := `{"status":"resolved","groupKey":"g1","alerts":[{"status":"resolved","labels":{"alertname":"Test"},"fingerprint":"f1"}]}`
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}
		if rec.Body.String() != "Resolved" {
			t.Errorf("Expected 'Resolved', got '%s'", rec.Body.String())
		}

		// Verify Resolution in Store
		resolved, _ := s.GetResolvedAlertGroups()
		found := false
		for _, r := range resolved {
			if r.AlertKey == "g1" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Alert group 'g1' not found in Resolved list")
		}
	})
}

func TestSeverityNormalization(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}
	validator := &mockSecretValidator{secrets: map[string]bool{"secret123": true}}
	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	tests := []struct {
		name             string
		inputSeverity    string
		expectedSeverity string
	}{
		{"uppercase CRITICAL", "CRITICAL", "critical"},
		{"mixed case Critical", "Critical", "critical"},
		{"already lowercase", "warning", "warning"},
		{"mixed INFO", "INFO", "info"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupKey := fmt.Sprintf("sev-norm-%d", i)
			payload := fmt.Sprintf(`{"status":"firing","groupKey":"%s","alerts":[{"status":"firing","labels":{"alertname":"Test","severity":"%s"},"fingerprint":"fp-sev-%d"}],"commonLabels":{"severity":"%s"}}`,
				groupKey, tt.inputSeverity, i, tt.inputSeverity)
			req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
			}

			ag, err := s.GetActiveAlertGroupByAlertKey(groupKey)
			if err != nil || ag == nil {
				t.Fatalf("Alert group not found for key %s", groupKey)
			}
			if ag.Severity != tt.expectedSeverity {
				t.Errorf("Expected severity %q, got %q", tt.expectedSeverity, ag.Severity)
			}
		})
	}
}

func TestEmptyDedupKeyRejected(t *testing.T) {
	s := store.NewMockStore()
	cfg := &config.Config{}
	validator := &mockSecretValidator{secrets: map[string]bool{"secret123": true}}
	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// groupKey empty, fingerprint empty → alertKey empty → should be rejected
	payload := `{"status":"firing","groupKey":"","alerts":[{"status":"firing","labels":{"alertname":"Test"},"fingerprint":""}],"commonLabels":{}}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for empty alertKey, got %d. Body: %s", rec.Code, rec.Body.String())
	}
}

func TestMergeIntoGroup_PreservesAcknowledgedStatus(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}
	validator := &mockSecretValidator{secrets: map[string]bool{"secret123": true}}
	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Step 1: Create alert group
	payload1 := `{"status":"firing","groupKey":"ack-test","alerts":[{"status":"firing","labels":{"alertname":"A1","team":"devops"},"fingerprint":"fp1"}]}`
	req1 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload1))
	req1.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("Step 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}

	ag, _ := s.GetActiveAlertGroupByAlertKey("ack-test")
	if ag == nil {
		t.Fatal("Alert group not created")
	}

	// Step 2: Simulate ack → set status to acknowledged, the way an ack
	// actually happens: the engine admits the group, then the transition
	// applies. Acking straight from "new" is refused, as it is in production.
	if err := s.SetAlertGroupStatus(ag.ID, model.AlertGroupStatusProcessing); err != nil {
		t.Fatalf("UpdateAlertGroupStatus: %v", err)
	}
	if changed, err := s.AckAlertGroupAtomic(ag.ID, "user1", nil, nil); err != nil || !changed {
		t.Fatalf("AckAlertGroupAtomic: changed=%v err=%v", changed, err)
	}
	ag, _ = s.GetAlertGroupByID(ag.ID)
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Fatalf("Expected acknowledged, got %s", ag.Status)
	}

	// Step 3: New webhook arrives with additional alert
	payload2 := `{"status":"firing","groupKey":"ack-test","alerts":[{"status":"firing","labels":{"alertname":"A2","team":"devops"},"fingerprint":"fp2"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload2))
	req2.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("Step 3 failed: %d %s", rec2.Code, rec2.Body.String())
	}

	// Verify: status should STILL be acknowledged (not overwritten to processing)
	ag, _ = s.GetAlertGroupByID(ag.ID)
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status to stay 'acknowledged' after merge, got '%s'", ag.Status)
	}
}

func TestMergeIntoGroup_SetsSlackUpdatePending(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}
	validator := &mockSecretValidator{secrets: map[string]bool{"secret123": true}}
	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Step 1: Create alert group
	payload1 := `{"status":"firing","groupKey":"update-test","alerts":[{"status":"firing","labels":{"alertname":"A1","team":"devops"},"fingerprint":"fp1"}]}`
	req1 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload1))
	req1.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("Step 1 failed: %d %s", rec1.Code, rec1.Body.String())
	}

	ag, _ := s.GetActiveAlertGroupByAlertKey("update-test")
	if ag == nil {
		t.Fatal("Alert group not created")
	}

	// Simulate engine processing → status = processing
	s.SetAlertGroupStatus(ag.ID, model.AlertGroupStatusProcessing)

	// Verify flag is NOT set before merge
	ag, _ = s.GetAlertGroupByID(ag.ID)
	if ag.SlackUpdatePending {
		t.Fatal("SlackUpdatePending should be false before merge")
	}

	// Step 2: New webhook → partial merge (not all resolved)
	payload2 := `{"status":"firing","groupKey":"update-test","alerts":[{"status":"firing","labels":{"alertname":"A2","team":"devops"},"fingerprint":"fp2"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload2))
	req2.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("Step 2 failed: %d %s", rec2.Code, rec2.Body.String())
	}

	// Verify: slack_update_pending should be true
	ag, _ = s.GetAlertGroupByID(ag.ID)
	if !ag.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be true after partial merge")
	}
}

// TestAlertRefire verifies that when an alert is resolved and then fires again,
// a timeline event is created for the re-fire.
func TestAlertRefire(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}
	validator := &mockSecretValidator{secrets: map[string]bool{"test-secret": true}}
	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Step 1: Create initial alert (firing)
	payload1 := `{"status":"firing","groupKey":"refire-test","alerts":[{"status":"firing","labels":{"alertname":"HighCPU","team":"devops"},"fingerprint":"fp1"}]}`
	req1 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=test-secret", strings.NewReader(payload1))
	req1.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec1 := httptest.NewRecorder()
	e.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK || rec1.Body.String() != "Created" {
		t.Fatalf("Step 1 failed: expected 200/Created, got %d/%s", rec1.Code, rec1.Body.String())
	}

	ag, _ := s.GetActiveAlertGroupByAlertKey("refire-test")
	if ag == nil {
		t.Fatal("Alert group not created")
	}

	// Step 2: Resolve the alert
	payload2 := `{"status":"resolved","groupKey":"refire-test","alerts":[{"status":"resolved","labels":{"alertname":"HighCPU","team":"devops"},"fingerprint":"fp1"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=test-secret", strings.NewReader(payload2))
	req2.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec2 := httptest.NewRecorder()
	e.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK || rec2.Body.String() != "Resolved" {
		t.Fatalf("Step 2 failed: expected 200/Resolved, got %d/%s", rec2.Code, rec2.Body.String())
	}

	// Verify that resolved alert is stored correctly
	ag, _ = s.GetAlertGroupByID(ag.ID)
	resolvedCount := 0
	for _, a := range ag.Alerts {
		if a.Status == model.AlertStatusResolved {
			resolvedCount++
		}
	}
	if resolvedCount != 1 {
		t.Fatalf("Expected 1 resolved alert, got %d", resolvedCount)
	}

	// Step 3: Re-fire the same alert
	// First we need to re-activate the alert group (since it was resolved)
	ag.Status = model.AlertGroupStatusProcessing
	s.SetAlertGroupStatus(ag.ID, model.AlertGroupStatusProcessing)

	payload3 := `{"status":"firing","groupKey":"refire-test","alerts":[{"status":"firing","labels":{"alertname":"HighCPU","team":"devops"},"fingerprint":"fp1"}]}`
	req3 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=test-secret", strings.NewReader(payload3))
	req3.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec3 := httptest.NewRecorder()
	e.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK || rec3.Body.String() != "Updated" {
		t.Fatalf("Step 3 failed: expected 200/Updated, got %d/%s", rec3.Code, rec3.Body.String())
	}

	// Verify timeline events
	events, err := s.GetTimelineEvents(ag.ID)
	if err != nil {
		t.Fatalf("Failed to get timeline events: %v", err)
	}

	// Expected events:
	// 1. Alert group created
	// 2. Alert added (initial firing)
	// 3. Alert resolved
	// 4. Alert re-fired
	if len(events) < 4 {
		t.Errorf("Expected at least 4 timeline events, got %d", len(events))
		for i, ev := range events {
			t.Logf("Event %d: %s - %s", i, ev.Type, ev.Message)
		}
	}

	// Check for re-fire event
	hasRefireEvent := false
	for _, ev := range events {
		if ev.Type == model.TimelineEventAlertAdded && strings.Contains(ev.Message, "re-fired") {
			hasRefireEvent = true
			break
		}
	}
	if !hasRefireEvent {
		t.Error("Expected 'Alert re-fired' timeline event not found")
		for i, ev := range events {
			t.Logf("Event %d: %s - %s", i, ev.Type, ev.Message)
		}
	}

	// Step 4: Resolve again after re-fire
	payload4 := `{"status":"resolved","groupKey":"refire-test","alerts":[{"status":"resolved","labels":{"alertname":"HighCPU","team":"devops"},"fingerprint":"fp1"}]}`
	req4 := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=test-secret", strings.NewReader(payload4))
	req4.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec4 := httptest.NewRecorder()
	e.ServeHTTP(rec4, req4)

	if rec4.Code != http.StatusOK || rec4.Body.String() != "Resolved" {
		t.Fatalf("Step 4 failed: expected 200/Resolved, got %d/%s", rec4.Code, rec4.Body.String())
	}

	// Verify timeline events after second resolve
	events, err = s.GetTimelineEvents(ag.ID)
	if err != nil {
		t.Fatalf("Failed to get timeline events: %v", err)
	}

	// Expected events:
	// 1. Alert group created
	// 2. Alert added (initial firing)
	// 3. Alert resolved (first resolve)
	// 4. Alert re-fired
	// 5. Alert resolved (second resolve)
	// 6. Alert group resolved
	if len(events) < 6 {
		t.Errorf("Expected at least 6 timeline events, got %d", len(events))
		for i, ev := range events {
			t.Logf("Event %d: %s - %s", i, ev.Type, ev.Message)
		}
	}

	// Count resolved events - should be at least 2 (one for first resolve, one for second)
	resolvedEventCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventAlertResolved {
			resolvedEventCount++
		}
	}
	if resolvedEventCount < 2 {
		t.Errorf("Expected at least 2 'Alert resolved' events, got %d", resolvedEventCount)
		for i, ev := range events {
			t.Logf("Event %d: %s - %s", i, ev.Type, ev.Message)
		}
	}
}

func TestNewGroup_ExcludesResolvedAlerts(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}
	mockValidator := &mockSecretValidator{secrets: map[string]bool{"s": true}}
	ing := NewIngester(s, cfg, mockValidator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Payload with 2 firing + 1 resolved alert
	payload := `{
		"status":"firing",
		"groupKey":"filter-test",
		"commonLabels":{"alertname":"MixedAlerts","team":"devops","severity":"warning"},
		"alerts":[
			{"status":"firing","labels":{"alertname":"A1"},"fingerprint":"fp1"},
			{"status":"resolved","labels":{"alertname":"A2"},"fingerprint":"fp2"},
			{"status":"firing","labels":{"alertname":"A3"},"fingerprint":"fp3"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=s", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "Created" {
		t.Fatalf("Expected 'Created', got %q", rec.Body.String())
	}

	ag, err := s.GetActiveAlertGroupByAlertKey("filter-test")
	if err != nil || ag == nil {
		t.Fatal("Alert group not found")
	}

	// Only firing alerts should be stored
	if len(ag.Alerts) != 2 {
		t.Errorf("Expected 2 firing alerts, got %d", len(ag.Alerts))
		for _, a := range ag.Alerts {
			t.Logf("  alert: %s status=%s", a.Fingerprint, a.Status)
		}
	}
	for _, a := range ag.Alerts {
		if a.Status != model.AlertStatusFiring {
			t.Errorf("Alert %s should be firing, got %s", a.Fingerprint, a.Status)
		}
	}
}

func TestCreatePath_AtomicTimelineAndOutbox(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	cfg := &config.Config{}
	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}

	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	payload := `{
		"status": "firing",
		"groupKey": "atomic-test-group",
		"commonLabels": {"alertname": "TestAtomicAlert", "team": "devops", "severity": "critical"},
		"alerts": [
			{"status": "firing", "labels": {"alertname": "TestAtomicAlert"}, "fingerprint": "fp-atomic-1"},
			{"status": "firing", "labels": {"alertname": "TestAtomicAlert2"}, "fingerprint": "fp-atomic-2"}
		]
	}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d. Body: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "Created" {
		t.Fatalf("Expected 'Created', got '%s'", rec.Body.String())
	}

	// Find the created AG
	ag, err := s.GetActiveAlertGroupByAlertKey("atomic-test-group")
	if err != nil || ag == nil {
		t.Fatal("Alert group not created in store")
	}

	// Verify timeline events: 1 Created + 2 AlertAdded
	events, err := s.GetTimelineEvents(ag.ID)
	if err != nil {
		t.Fatalf("GetTimelineEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("Expected 3 timeline events (1 created + 2 alert_added), got %d", len(events))
	}

	// Verify timeline ordering: Created first, then AlertAdded events
	if events[0].Type != model.TimelineEventCreated {
		t.Errorf("Expected events[0].Type 'created', got '%s'", events[0].Type)
	}
	if events[1].Type != model.TimelineEventAlertAdded {
		t.Errorf("Expected events[1].Type 'alert_added', got '%s'", events[1].Type)
	}
	if events[2].Type != model.TimelineEventAlertAdded {
		t.Errorf("Expected events[2].Type 'alert_added', got '%s'", events[2].Type)
	}

	// Verify µs offsets ensure strict ordering
	if !events[0].CreatedAt.Before(events[1].CreatedAt) {
		t.Error("Expected events[0].CreatedAt < events[1].CreatedAt (µs offset)")
	}
	if !events[1].CreatedAt.Before(events[2].CreatedAt) {
		t.Error("Expected events[1].CreatedAt < events[2].CreatedAt (µs offset)")
	}

	// Verify outbox event
	pending, err := s.GetPendingOutboxEvents(10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents: %v", err)
	}

	var firingEvent *model.OutboxEvent
	for _, ev := range pending {
		if ev.AlertGroupID == ag.ID && ev.EventType == model.OutboxEventFiring {
			firingEvent = ev
			break
		}
	}
	if firingEvent == nil {
		t.Fatal("Expected outbox event with type 'alert_group.firing'")
	}
	if firingEvent.TeamID != "devops" {
		t.Errorf("Expected team_id 'devops', got '%s'", firingEvent.TeamID)
	}
	if firingEvent.Status != model.OutboxEventStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", firingEvent.Status)
	}

	// Verify payload matches typed webhook contract
	var p model.WebhookEventPayload
	if err := json.Unmarshal(firingEvent.Payload, &p); err != nil {
		t.Fatalf("Failed to parse outbox payload: %v", err)
	}
	if p.Event != "alert_group.firing" {
		t.Errorf("Expected payload.event 'alert_group.firing', got %q", p.Event)
	}
	if p.Timestamp == "" {
		t.Error("Expected payload.timestamp to be set")
	}
	if p.AlertGroup.Status != "firing" {
		t.Errorf("Expected payload.alert_group.status 'firing', got %q", p.AlertGroup.Status)
	}
	if p.AlertGroup.ID != ag.ID {
		t.Errorf("Expected payload.alert_group.id '%s', got %q", ag.ID, p.AlertGroup.ID)
	}
	if p.AlertGroup.Title != "TestAtomicAlert" {
		t.Errorf("Expected payload.alert_group.title 'TestAtomicAlert', got %q", p.AlertGroup.Title)
	}
	if p.AlertGroup.Severity != "critical" {
		t.Errorf("Expected payload.alert_group.severity 'critical', got %q", p.AlertGroup.Severity)
	}
	if p.AlertGroup.AlertCount != 2 {
		t.Errorf("Expected payload.alert_group.alert_count 2, got %d", p.AlertGroup.AlertCount)
	}
	if p.AlertGroup.TeamName != "DevOps" {
		t.Errorf("Expected payload.alert_group.team_name 'DevOps', got %q", p.AlertGroup.TeamName)
	}
	if p.AlertGroup.TeamID != "devops" {
		t.Errorf("Expected payload.alert_group.team_id 'devops', got %q", p.AlertGroup.TeamID)
	}
	if p.Actor.Name != "system" {
		t.Errorf("Expected payload.actor.name 'system', got %q", p.Actor.Name)
	}

	// Verify no external_url key and no actor.email key in raw payload
	var rawMap map[string]json.RawMessage
	json.Unmarshal(firingEvent.Payload, &rawMap)
	var agMap map[string]json.RawMessage
	json.Unmarshal(rawMap["alert_group"], &agMap)
	if _, exists := agMap["external_url"]; exists {
		t.Error("Expected no 'external_url' field in payload.alert_group")
	}
	var actorMap map[string]json.RawMessage
	json.Unmarshal(rawMap["actor"], &actorMap)
	if _, exists := actorMap["email"]; exists {
		t.Error("Expected no 'email' key in actor for system-generated firing event")
	}
}

// TestResolveCreatesOutboxEvent verifies that the ingester auto-resolve path
// creates an outbox event with type alert_group.resolved.
func TestResolveCreatesOutboxEvent(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)

	// Create a firing AG
	ag := &model.AlertGroup{
		ID: "ag-outbox-resolve", AlertKey: "outbox-resolve-group",
		Status: model.AlertGroupStatusTriggered, TeamID: "devops", TeamNameSnapshot: "DevOps",
		Severity:  "critical",
		Alerts:    []model.Alert{{Fingerprint: "fp1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "Test"}}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(s, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	payload := `{"status":"resolved","groupKey":"outbox-resolve-group","alerts":[{"status":"resolved","labels":{"alertname":"Test"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "Resolved" {
		t.Errorf("Expected 'Resolved', got '%s'", rec.Body.String())
	}

	// Verify outbox event exists
	events, _ := s.GetPendingOutboxEvents(10)
	var found bool
	for _, ev := range events {
		if ev.AlertGroupID == ag.ID && ev.EventType == model.OutboxEventResolved {
			found = true
		}
	}
	if !found {
		t.Error("Expected outbox event with type alert_group.resolved after auto-resolve")
	}
}

// TestResolveFromNewStatus verifies that the ingester can resolve an AG
// directly from "new" status (before engine picks it up).
func TestResolveFromNewStatus(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)

	ag := &model.AlertGroup{
		ID: "ag-new-resolve", AlertKey: "new-resolve-group",
		Status: model.AlertGroupStatusNew, TeamID: "devops", TeamNameSnapshot: "DevOps",
		Severity:  "warning",
		Alerts:    []model.Alert{{Fingerprint: "fp1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "Test"}}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(ag)

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(s, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	payload := `{"status":"resolved","groupKey":"new-resolve-group","alerts":[{"status":"resolved","labels":{"alertname":"Test"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "Resolved" {
		t.Errorf("Expected 'Resolved', got '%s'", rec.Body.String())
	}

	// Verify status
	stored, _ := s.GetAlertGroupByID("ag-new-resolve")
	if stored.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", stored.Status)
	}
}

// Note: concurrent resolve idempotency (changed=false with alerts convergence)
// is tested in regression_test.go:TestRegression_ConcurrentResolve_AlertsConverge

func TestFilterMergeableAlerts(t *testing.T) {
	existing := map[string]model.AlertStatus{
		"known-firing":   model.AlertStatusFiring,
		"known-resolved": model.AlertStatusResolved,
	}

	tests := []struct {
		name     string
		incoming []model.Alert
		expected []string
	}{
		{
			name:     "unknown resolved alert is dropped",
			incoming: []model.Alert{{Fingerprint: "stranger", Status: model.AlertStatusResolved}},
			expected: nil,
		},
		{
			name:     "unknown firing alert joins the group",
			incoming: []model.Alert{{Fingerprint: "newcomer", Status: model.AlertStatusFiring}},
			expected: []string{"newcomer"},
		},
		{
			name:     "known alert resolving is kept",
			incoming: []model.Alert{{Fingerprint: "known-firing", Status: model.AlertStatusResolved}},
			expected: []string{"known-firing"},
		},
		{
			name:     "known alert re-firing is kept",
			incoming: []model.Alert{{Fingerprint: "known-resolved", Status: model.AlertStatusFiring}},
			expected: []string{"known-resolved"},
		},
		{
			name:     "known alert with unchanged status is kept",
			incoming: []model.Alert{{Fingerprint: "known-firing", Status: model.AlertStatusFiring}},
			expected: []string{"known-firing"},
		},
		{
			name: "mixed payload keeps everything but the unknown resolved alert",
			incoming: []model.Alert{
				{Fingerprint: "known-firing", Status: model.AlertStatusResolved},
				{Fingerprint: "stranger", Status: model.AlertStatusResolved},
				{Fingerprint: "newcomer", Status: model.AlertStatusFiring},
			},
			expected: []string{"known-firing", "newcomer"},
		},
		{
			name:     "empty payload stays empty",
			incoming: nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterMergeableAlerts(tt.incoming, existing)
			if len(got) != len(tt.expected) {
				t.Fatalf("got %d alerts, want %d (%v)", len(got), len(tt.expected), got)
			}
			// Order is preserved, so index comparison is safe.
			for i, fp := range tt.expected {
				if got[i].Fingerprint != fp {
					t.Errorf("alert %d = %q, want %q", i, got[i].Fingerprint, fp)
				}
			}
		})
	}
}
