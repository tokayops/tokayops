package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// TestUpdateTeam_RoutingConfig_ShouldUpdate verifies that routing fields
// (DefaultPolicyID, SeverityRoutes) can be updated via API.
func TestUpdateTeam_RoutingConfig_ShouldUpdate(t *testing.T) {
	e := echo.New()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)

	// Setup: Admin user
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	s.CreateUser(admin)

	// Setup: Team
	team := &model.Team{ID: "team-1", Name: "Team 1", CreatedAt: time.Now()}
	s.CreateTeam(team)

	// Setup: Policies (Required for validation)
	s.CreateEscalationPolicy(&model.EscalationPolicy{ID: "policy-new", Name: "New Policy", TeamID: &team.ID})
	s.CreateEscalationPolicy(&model.EscalationPolicy{ID: "policy-crit", Name: "Crit Policy", TeamID: &team.ID})

	// Request: Update default_policy_id
	// Note: We define a custom struct here to mimic what the CLIENT sends,
	// to prove the server struct ignores it if fields are missing.
	type UpdateTeamRequestClient struct {
		Name            string            `json:"name"`
		DefaultPolicyID string            `json:"default_policy_id"`
		SeverityRoutes  map[string]string `json:"severity_routes"`
	}

	reqBody := UpdateTeamRequestClient{
		Name:            "Team 1 Updated",
		DefaultPolicyID: "policy-new",
		SeverityRoutes:  map[string]string{"critical": "policy-crit"},
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/team-1", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("team-1")
	c.Set("user_id", admin.ID)

	// Execute
	if err := api.UpdateTeam(c); err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", rec.Code)
	}

	// Verify persistence in store
	updatedTeam, _ := s.GetTeamByID("team-1")
	if updatedTeam.DefaultPolicyID != "policy-new" {
		t.Errorf("Regression! DefaultPolicyID not updated. Expected 'policy-new', got '%s'", updatedTeam.DefaultPolicyID)
	}
	if val, ok := updatedTeam.SeverityRoutes["critical"]; !ok || val != "policy-crit" {
		t.Errorf("Regression! SeverityRoutes not updated. Expected 'policy-crit', got '%v'", updatedTeam.SeverityRoutes)
	}
}

// TestUpdateTeam_UseDefaultClearsSeverityRoutes locks the contract teams.js relies
// on: sending an EMPTY severity_routes map clears previously-saved routes (so all
// severities fall back to the Default Policy). Before the UI fix, the client sent
// null and the stale route persisted, shadowing the default.
func TestUpdateTeam_UseDefaultClearsSeverityRoutes(t *testing.T) {
	e := echo.New()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)

	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	s.CreateUser(admin)
	s.CreateEscalationPolicy(&model.EscalationPolicy{ID: "policy-default", Name: "Default Policy"})
	s.CreateTeam(&model.Team{
		ID:             "team-1",
		Name:           "Team 1",
		SeverityRoutes: map[string]string{"critical": "policy-crit"}, // pre-existing route to clear
		CreatedAt:      time.Now(),
	})

	// Mimic the UI "all Use default" save: empty severity_routes map + a default policy.
	body := []byte(`{"name":"Team 1","default_policy_id":"policy-default","severity_routes":{}}`)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/team-1", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("team-1")
	c.Set("user_id", admin.ID)

	if err := api.UpdateTeam(c); err != nil {
		t.Fatalf("Handler error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	updated, _ := s.GetTeamByID("team-1")
	if len(updated.SeverityRoutes) != 0 {
		t.Errorf("severity routes should be cleared, got %v", updated.SeverityRoutes)
	}
	if updated.DefaultPolicyID != "policy-default" {
		t.Errorf("default policy = %q, want policy-default", updated.DefaultPolicyID)
	}
}
