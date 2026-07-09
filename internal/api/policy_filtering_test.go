package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// TestListPolicies_Filtering validates that non-admin users only see
// global policies and policies for teams they belong to.
func TestListPolicies_Filtering(t *testing.T) {
	e := echo.New()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)

	// Setup: Admin
	admin := &model.User{ID: "admin", Role: model.UserRoleAdmin}
	s.CreateUser(admin)

	// Setup: User A (Team 1)
	userA := &model.User{ID: "user-a", Role: model.UserRoleUser}
	s.CreateUser(userA)

	// Setup: Team 1
	team1 := &model.Team{ID: "team-1", Name: "Team 1"}
	s.CreateTeam(team1)
	s.AddTeamMember("team-1", "user-a", model.TeamMemberRoleMember)

	// Setup: Team 2
	team2 := &model.Team{ID: "team-2", Name: "Team 2"}
	s.CreateTeam(team2)
	// User A is NOT in Team 2

	// Setup: Policies
	pGlobal := &model.EscalationPolicy{ID: "p-global", Name: "Global", TeamID: nil}
	pTeam1 := &model.EscalationPolicy{ID: "p-team-1", Name: "Team 1 Policy", TeamID: &team1.ID}
	pTeam2 := &model.EscalationPolicy{ID: "p-team-2", Name: "Team 2 Policy", TeamID: &team2.ID}
	s.CreateEscalationPolicy(pGlobal)
	s.CreateEscalationPolicy(pTeam1)
	s.CreateEscalationPolicy(pTeam2)

	// Execute as User A
	req := httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set("user_id", userA.ID)

	if err := api.ListPolicies(c); err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", rec.Code)
	}

	var policies []*model.EscalationPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &policies); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	// EXPECTATION: User A should see pGlobal and pTeam1. Should NOT see pTeam2.
	// CURRENT BUG: Sees all 3.

	seen := make(map[string]bool)
	for _, p := range policies {
		seen[p.ID] = true
	}

	if !seen["p-global"] {
		t.Error("Regression! Global policy missing")
	}
	if !seen["p-team-1"] {
		t.Error("Regression! Team 1 policy missing")
	}
	if seen["p-team-2"] {
		t.Error("Regression! User A sees Team 2 policy (should be hidden)")
	}
}
