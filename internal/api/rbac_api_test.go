package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// setupRBACEnv creates a standard environment for RBAC testing:
// - Teams: team-a, team-b
// - Users:
//   - admin (global admin)
//   - alice: team_admin of team-a
//   - bob: team_member of team-a
//   - charlie: member of team-b (outsider to team-a)
//   - guest: no team memberships
func setupRBACEnv(t *testing.T) (*API, *store.MockStore, *echo.Echo) {
	a, s, e := setupTestAPI(t)

	// Create Teams
	s.CreateTeam(&model.Team{ID: "team-a", Name: "Team A"})
	s.CreateTeam(&model.Team{ID: "team-b", Name: "Team B"})

	// Create Users
	users := []struct {
		ID   string
		Role model.UserRole
	}{
		{"admin", model.UserRoleAdmin},
		{"alice", model.UserRoleUser},
		{"bob", model.UserRoleUser},
		{"charlie", model.UserRoleUser},
		{"guest", model.UserRoleUser},
	}
	for _, u := range users {
		s.CreateUser(&model.User{ID: u.ID, Email: u.ID + "@test.com", Role: u.Role})
	}

	// Assign Team Roles
	s.AddTeamMember("team-a", "alice", model.TeamMemberRoleAdmin)
	s.AddTeamMember("team-a", "bob", model.TeamMemberRoleMember)
	s.AddTeamMember("team-b", "charlie", model.TeamMemberRoleMember)

	return a, s, e
}

func TestRBAC_TeamManagement(t *testing.T) {
	_, _, e := setupRBACEnv(t)

	tests := []struct {
		name       string
		method     string
		path       string
		user       string
		wantStatus int
	}{
		// UPDATE Team
		{"Admin can update team", http.MethodPut, "/api/v1/teams/team-a", "admin", http.StatusOK},
		{"Team Admin can update own team", http.MethodPut, "/api/v1/teams/team-a", "alice", http.StatusOK},
		{"Team Member CANNOT update team", http.MethodPut, "/api/v1/teams/team-a", "bob", http.StatusForbidden},
		{"Outsider CANNOT update team", http.MethodPut, "/api/v1/teams/team-a", "charlie", http.StatusForbidden},

		// DELETE Team
		{"Admin can delete team", http.MethodDelete, "/api/v1/teams/team-a", "admin", http.StatusOK}, // returns 200 or 204? MockDelete returns nil. Handler usually 204.
		{"Team Admin CANNOT delete team", http.MethodDelete, "/api/v1/teams/team-a", "alice", http.StatusForbidden},
		{"Team Member CANNOT delete team", http.MethodDelete, "/api/v1/teams/team-a", "bob", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.method == http.MethodPut {
				body := `{"name": "Updated Name"}`
				req = httptest.NewRequest(tt.method, tt.path, strings.NewReader(body))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			} else {
				req = httptest.NewRequest(tt.method, tt.path, nil)
			}
			addAuth(req, tt.user)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			// Special handling: DELETE might return 204 No Content
			if rec.Code != tt.wantStatus {
				if tt.wantStatus == http.StatusOK && rec.Code == http.StatusNoContent {
					// Accept 204 if we expected 200 for simplicity, or fix expectation
				} else {
					t.Errorf("path %s user %s: expected status %d, got %d. Body: %s", tt.path, tt.user, tt.wantStatus, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestRBAC_TeamMembers(t *testing.T) {
	_, s, e := setupRBACEnv(t)

	// Pre-create a target user to add/remove
	s.CreateUser(&model.User{ID: "target-user", Email: "target@test.com"})

	t.Run("Add Member", func(t *testing.T) {
		tests := []struct {
			user       string
			wantStatus int
		}{
			{"admin", http.StatusCreated},
			{"alice", http.StatusCreated},     // Team Admin
			{"bob", http.StatusForbidden},     // Team Member
			{"charlie", http.StatusForbidden}, // Outsider
		}

		for _, tt := range tests {
			t.Run(tt.user, func(t *testing.T) {
				// Reset team membership for target user before each try (clearing it manually or just ignoring duplications if idempotent)
				// Since mock allows overwrites, we just try to add.
				// However, if we want to test "Add", we should ensure strictly.
				// For 403 cases, it doesn't matter. For 201 cases, we accept.

				body := `{"user_id": "target-user", "role": "team_member"}`
				req := httptest.NewRequest(http.MethodPost, "/api/v1/teams/team-a/members", strings.NewReader(body))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				addAuth(req, tt.user)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)

				if rec.Code != tt.wantStatus {
					t.Errorf("user %s expected %d, got %d", tt.user, tt.wantStatus, rec.Code)
				}
			})
		}
	})

	t.Run("Remove Member", func(t *testing.T) {
		// Ensure target-user is in team-a
		s.AddTeamMember("team-a", "target-user", model.TeamMemberRoleMember)

		tests := []struct {
			user       string
			wantStatus int
		}{
			{"admin", http.StatusNoContent},
			{"alice", http.StatusNoContent},   // Team Admin
			{"bob", http.StatusForbidden},     // Team Member
			{"charlie", http.StatusForbidden}, // Outsider
		}

		for _, tt := range tests {
			t.Run(tt.user, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodDelete, "/api/v1/teams/team-a/members/target-user", nil)
				addAuth(req, tt.user)
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)

				if rec.Code != tt.wantStatus {
					t.Errorf("user %s expected %d, got %d", tt.user, tt.wantStatus, rec.Code)
				}
			})
		}
	})
}

func TestRBAC_UserManagement(t *testing.T) {
	_, _, e := setupRBACEnv(t)

	t.Run("Create User", func(t *testing.T) {
		body := `{"email": "newuser@test.com", "name": "New User"}`

		// Admin -> 201
		req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "admin")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("Admin should be able to create user, got %d", rec.Code)
		}

		// Non-Admin -> 403
		req = httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Non-Admin NOT create user, got %d", rec.Code)
		}
	})

	t.Run("Delete User", func(t *testing.T) {
		// Admin -> 204
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/users/guest", nil)
		addAuth(req, "admin")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		// 204 or 200 depending on handler
		if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
			t.Errorf("Admin should be able to delete user, got %d", rec.Code)
		}

		// Non-Admin -> 403
		req = httptest.NewRequest(http.MethodDelete, "/api/v1/users/charlie", nil)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Non-Admin NOT delete user, got %d", rec.Code)
		}
	})

	t.Run("Password Change", func(t *testing.T) {
		body := `{"password": "NewPassword123!"}`

		// Admin -> 200 (can change anyone)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/users/bob/password", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "admin")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Admin should change password, got %d", rec.Code)
		}

		// Self -> 200
		req = httptest.NewRequest(http.MethodPut, "/api/v1/users/alice/password", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("User should change own password, got %d", rec.Code)
		}

		// Other -> 403
		req = httptest.NewRequest(http.MethodPut, "/api/v1/users/admin/password", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("User CANNOT change others password, got %d", rec.Code)
		}
	})
}

func TestRBAC_ScheduleManagement(t *testing.T) {
	_, s, e := setupRBACEnv(t)
	_ = s

	// Editing a schedule is authorized by TestScheduleConfigRBACMatrix, which
	// exercises the revision endpoints this replaced. What is left here is
	// deletion, whose scope resolves through the schedule rather than the team
	// in the path.

	t.Run("Delete Schedule", func(t *testing.T) {
		// A revision-model schedule, created through the API by the team admin.
		created := createTeamSchedule(t, e, "team-a", "alice", "alice")

		// Team Member -> 403
		req := httptest.NewRequest(http.MethodDelete,
			fmt.Sprintf("/api/v1/teams/team-a/schedule?expected_version=%d", created.Version), nil)
		addAuth(req, "bob")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Team Member delete schedule: want 403, got %d", rec.Code)
		}

		// Non-member -> 403
		req = httptest.NewRequest(http.MethodDelete,
			fmt.Sprintf("/api/v1/teams/team-a/schedule?expected_version=%d", created.Version), nil)
		addAuth(req, "charlie")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Non-member delete schedule: want 403, got %d", rec.Code)
		}

		// Team Admin -> 204
		req = httptest.NewRequest(http.MethodDelete,
			fmt.Sprintf("/api/v1/teams/team-a/schedule?expected_version=%d", created.Version), nil)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("Team Admin delete schedule: want 204, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// createTeamSchedule creates a revision-model schedule for a team, with the
// named members as the single rotation group.
func createTeamSchedule(t *testing.T, e *echo.Echo, teamID, actor string, members ...string) PutScheduleConfigResponse {
	t.Helper()
	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/"+teamID+"/schedule/config",
		configRequest(0, members), actor)
	if rec.Code != http.StatusOK {
		t.Fatalf("create schedule for %s: want 200, got %d: %s", teamID, rec.Code, rec.Body.String())
	}
	var out PutScheduleConfigResponse
	decodeJSON(t, rec, &out)
	return out
}

func TestRBAC_ScheduleOverrides(t *testing.T) {
	_, s, e := setupRBACEnv(t)
	defer s.Close()
	createTeamSchedule(t, e, "team-a", "alice", "alice")

	scheduleID := func() string {
		rec := doJSON(t, e, http.MethodGet, "/api/v1/teams/team-a/schedule/config", nil, "alice")
		var cfg ScheduleConfigResponse
		decodeJSON(t, rec, &cfg)
		return cfg.ScheduleID
	}()

	override := ScheduleOverrideRequest{
		UserID:    "alice",
		ValidFrom: time.Date(2099, 1, 1, 10, 0, 0, 0, time.UTC),
		ValidTo:   time.Date(2099, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	var created ScheduleOverrideDTO
	t.Run("Create Override", func(t *testing.T) {
		// Team Member -> 201
		rec := doJSON(t, e, http.MethodPost, "/api/v1/teams/team-a/schedule/overrides", override, "bob")
		if rec.Code != http.StatusCreated {
			t.Fatalf("Team Member create override: want 201, got %d: %s", rec.Code, rec.Body.String())
		}
		decodeJSON(t, rec, &created)

		// Non-Member -> 403
		rec = doJSON(t, e, http.MethodPost, "/api/v1/teams/team-a/schedule/overrides", override, "charlie")
		if rec.Code != http.StatusForbidden {
			t.Errorf("Non-Member create override: want 403, got %d", rec.Code)
		}
	})

	t.Run("Update Override", func(t *testing.T) {
		path := fmt.Sprintf("/api/v1/schedules/%s/overrides/%s", scheduleID, created.OverrideID)
		edit := override
		edit.ExpectedRevision = created.Revision
		edit.ValidTo = override.ValidTo.Add(time.Hour)

		// Team Member -> 200
		rec := doJSON(t, e, http.MethodPut, path, edit, "bob")
		if rec.Code != http.StatusOK {
			t.Fatalf("Team Member update override: want 200, got %d: %s", rec.Code, rec.Body.String())
		}

		// Non-Member -> 403
		edit.ExpectedRevision = created.Revision + 1
		rec = doJSON(t, e, http.MethodPut, path, edit, "charlie")
		if rec.Code != http.StatusForbidden {
			t.Errorf("Non-Member update override: want 403, got %d", rec.Code)
		}
	})
}

func TestRBAC_Visibility(t *testing.T) {
	_, _, e := setupRBACEnv(t)
	// team-a already created by setupRBACEnv

	t.Run("Public Internal Access", func(t *testing.T) {
		// Verify "charlie" (outsider) can see public internal info
		endpoints := []string{
			"/api/v1/teams/team-a",
			"/api/v1/teams",
			"/api/v1/alert-groups", // assuming some exist
		}

		for _, ep := range endpoints {
			req := httptest.NewRequest(http.MethodGet, ep, nil)
			addAuth(req, "charlie")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("Endpoint %s should be public internal, got %d", ep, rec.Code)
			}
		}
	})
}

func TestRBAC_Legacy(t *testing.T) {
	_, _, e := setupRBACEnv(t)

	t.Run("Incidents Endpoint Aliases", func(t *testing.T) {
		// Should act same as alert-groups (public internal)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil)
		addAuth(req, "charlie")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Legacy /incidents should be visible, got %d", rec.Code)
		}
	})
}

func TestRBAC_TokenAndPolicy(t *testing.T) {
	_, s, e := setupRBACEnv(t)

	// Seed Tokens
	// Alice has a token
	s.CreateAPIToken(&model.APIToken{
		ID:        "token-alice",
		UserID:    "alice",
		Name:      "Alice Token",
		TokenHash: "hash1",
	})
	// Bob has a token
	s.CreateAPIToken(&model.APIToken{
		ID:        "token-bob",
		UserID:    "bob",
		Name:      "Bob Token",
		TokenHash: "hash2",
	})

	t.Run("Token Management", func(t *testing.T) {
		// List (Access check only - logic filters data)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Alice should list tokens, got %d", rec.Code)
		}

		// Create
		body := `{"name": "New Token"}`
		req = httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("Alice should create token, got %d", rec.Code)
		}

		// Delete Own -> 204
		req = httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/token-alice", nil)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		// Usually 204
		if rec.Code != http.StatusNoContent {
			t.Errorf("Alice should delete own token, got %d", rec.Code)
		}

		// Delete Other -> 403
		req = httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/token-bob", nil)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Alice CANNOT delete Bob's token, got %d", rec.Code)
		}
	})

	// Seed Policies
	// Global Policy
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: "policy-global", Name: "Global Policy", Description: "Global", Steps: []*model.EscalationStep{},
	})
	// Team Policy
	teamID := "team-a"
	s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: "policy-team", Name: "Team Policy", Description: "Team A", TeamID: &teamID, Steps: []*model.EscalationStep{},
	})

	t.Run("Policy Management", func(t *testing.T) {
		// Global Policy View -> All
		req := httptest.NewRequest(http.MethodGet, "/api/v1/policies/policy-global", nil)
		addAuth(req, "charlie") // Outsider
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Outsider should view global policy, got %d", rec.Code)
		}

		// Global Policy Update -> Admin Only
		// steps required by handler
		validBody := `{"name": "Updated Global", "steps": [{"provider": "slack", "target_kind": "dm", "target_type": "user", "target_id": "admin", "delay_seconds": 300}]}`

		req = httptest.NewRequest(http.MethodPut, "/api/v1/policies/policy-global", strings.NewReader(validBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "admin")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Admin should update global policy, got %d. Body: %s", rec.Code, rec.Body.String())
		}
		// User -> 403
		req = httptest.NewRequest(http.MethodPut, "/api/v1/policies/policy-global", strings.NewReader(validBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("User CANNOT update global policy, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		// Team Policy View
		// Member -> 200
		req = httptest.NewRequest(http.MethodGet, "/api/v1/policies/policy-team", nil)
		addAuth(req, "bob")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Team Member should view team policy, got %d", rec.Code)
		}
		// Outsider -> 403
		req = httptest.NewRequest(http.MethodGet, "/api/v1/policies/policy-team", nil)
		addAuth(req, "charlie")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Outsider CANNOT view team policy, got %d", rec.Code)
		}

		// Team Policy Update
		// Team Admin -> 200
		req = httptest.NewRequest(http.MethodPut, "/api/v1/policies/policy-team", strings.NewReader(validBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "alice")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Team Admin should update team policy, got %d", rec.Code)
		}
		// Team Member -> 403
		req = httptest.NewRequest(http.MethodPut, "/api/v1/policies/policy-team", strings.NewReader(validBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "bob")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Team Member CANNOT update team policy, got %d", rec.Code)
		}

		// Create Policy with Non-Existent Team (Should Fail Validation)
		// Admin tries to create for "ghost-team" -> Permission OK (Global Admin), but Validation Fails
		body := `{"name": "Bad Team Policy", "team_id": "ghost-team", "steps": [{"provider": "slack", "target_kind": "dm", "target_type": "user", "target_id": "admin", "delay_seconds": 300}]}`
		req = httptest.NewRequest(http.MethodPost, "/api/v1/policies", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "admin")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("Admin creating policy for non-existent team should fail validation (400), got %d. Body: %s", rec.Code, rec.Body.String())
		}
	})
}
