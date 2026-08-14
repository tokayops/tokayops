package ingester

import (
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

func TestIngester_PartialUpdate_KeepsStatusAndFlagsSlackUpdate(t *testing.T) {
	// Setup Dependencies
	s := store.NewMockStore()

	// Create an existing alert group that is already triggered
	existingID := "existing-group-id"
	existingAG := &model.AlertGroup{
		ID:       existingID,
		DedupKey: "g1",
		Status:   model.AlertGroupStatusTriggered,
		Alerts: []model.Alert{
			{Fingerprint: "f1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "Alert1"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.CreateAlertGroup(existingAG)

	// Config
	cfg := &config.Config{
		Global: config.GlobalConfig{},
	}

	validator := &mockSecretValidator{secrets: map[string]bool{"test-secret": true}}
	ing := NewIngester(s, cfg, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// SEND UPDATE: New alert "f2" firing in same group "g1"
	payload := `{"status":"firing","groupKey":"g1","alerts":[
		{"status":"firing","labels":{"alertname":"Alert1"},"fingerprint":"f1"},
		{"status":"firing","labels":{"alertname":"Alert2"},"fingerprint":"f2"}
	]}`

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=test-secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rec.Code)
	}

	// Verify State in Store
	updatedAG, err := s.GetActiveAlertGroup("g1")
	if err != nil {
		t.Fatalf("Failed to get AG: %v", err)
	}

	// Status should stay triggered (ingester does NOT regress status)
	if updatedAG.Status != model.AlertGroupStatusTriggered {
		t.Errorf("Expected status to stay triggered, got %s", updatedAG.Status)
	}

	// SlackUpdatePending should be flagged for dispatcher to pick up
	if !updatedAG.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be true after merge")
	}

	// Should have 2 alerts now
	if len(updatedAG.Alerts) != 2 {
		t.Errorf("Expected 2 alerts, got %d", len(updatedAG.Alerts))
	}
}
