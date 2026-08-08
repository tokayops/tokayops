//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
	"github.com/labstack/echo/v4"
)

type APIIntegrationEnv struct {
	S    *store.Store
	API  *api.API
	Echo *echo.Echo
}

func setupAPITest(t *testing.T) *APIIntegrationEnv {
	s := testutil.SetupDB(t)

	// We pass nil for OIDC provider, Slack messenger, and IntegrationCache for now
	a := api.NewAPI(s, nil, nil, nil, "", nil)
	wireScheduleServices(a, s)
	e := echo.New()
	a.RegisterRoutes(e)

	return &APIIntegrationEnv{
		S:    s,
		API:  a,
		Echo: e,
	}
}

// wireScheduleServices gives the API the same schedule and erasure services
// main builds. They are separate setters because the revision model is
// deliberately not part of store.StoreInterface, so NewAPI cannot reach them.
func wireScheduleServices(a *api.API, s *store.Store) {
	a.SetScheduleConfigService(scheduleconfig.NewService(s.ScheduleConfigRepository()))
	a.SetScheduleReadRepository(s.ScheduleReadRepository())
	a.SetScheduleRenderer(schedulerender.New(s.ScheduleReadRepository()))
	a.SetUserEraser(erasure.NewService(s.ErasureRepository()))
}

func createAuthenticatedRequest(t *testing.T, method, path string, body []byte, userID string) *http.Request {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}

	token, err := auth.GenerateToken(userID)
	if err != nil {
		t.Fatalf("Failed to generate token: %v", err)
	}

	req.AddCookie(&http.Cookie{
		Name:     api.AuthCookieName,
		Value:    token,
		Expires:  time.Now().Add(24 * time.Hour),
		HttpOnly: true,
	})
	return req
}

func TestAPI_Users_CRUD(t *testing.T) {
	env := setupAPITest(t)

	// 1. Setup Admin User (for auth)
	admin := testutil.SeedUser(t, env.S, "admin@example.com")

	// 2. Create User
	reqBody := `{"id": "testusr", "email": "test@example.com", "name": "Test User", "password": "Password123!"}`
	req := createAuthenticatedRequest(t, http.MethodPost, "/api/v1/users", []byte(reqBody), admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("Expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify DB
	u, err := env.S.GetUserByID("testusr")
	if err != nil {
		t.Errorf("User not found in DB: %v", err)
	}
	if u.Email != "test@example.com" {
		t.Errorf("Expected email test@example.com, got %s", u.Email)
	}
}

func TestAPI_Users_UpdateRole(t *testing.T) {
	env := setupAPITest(t)

	// 1. Setup Admin and User
	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	admin.Role = model.UserRoleAdmin
	env.S.UpdateUser(admin)

	user := testutil.SeedUser(t, env.S, "user@example.com")

	// 2. Promote User to Admin
	reqBody := `{"role": "admin"}`
	req := createAuthenticatedRequest(t, http.MethodPut, "/api/v1/users/"+user.ID, []byte(reqBody), admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Verify DB
	updatedUser, err := env.S.GetUserByID(user.ID)
	if err != nil {
		t.Fatalf("Failed to get user: %v", err)
	}
	if updatedUser.Role != model.UserRoleAdmin {
		t.Errorf("Expected role 'admin', got '%s'", updatedUser.Role)
	}
}

func TestAPI_AlertGroups_Lifecycle(t *testing.T) {
	env := setupAPITest(t)
	// 1. Setup Team and User
	team := testutil.SeedTeam(t, env.S, "test-team")
	user := testutil.SeedUser(t, env.S, "op@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, user.ID, model.TeamMemberRoleAdmin)

	// Seed Alert Group
	ag := &model.AlertGroup{
		ID:               "ag-api-1",
		DedupKey:         "dedup-api-1",
		Status:           model.AlertGroupStatusTriggered,
		Title:            "API Test Alert",
		TeamID:           team.ID,
		TeamNameSnapshot: team.Name,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		Alerts:           []model.Alert{{Fingerprint: "fp1", Status: "firing"}},
	}
	if err := env.S.CreateAlertGroup(ag); err != nil {
		t.Fatalf("Failed to seed AG: %v", err)
	}

	// 1. Get Alert Group
	req := createAuthenticatedRequest(t, http.MethodGet, "/api/v1/alert-groups/ag-api-1", nil, user.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	var gotAG model.AlertGroup
	json.Unmarshal(rec.Body.Bytes(), &gotAG)
	if gotAG.ID != "ag-api-1" {
		t.Errorf("Expected ID ag-api-1, got %s", gotAG.ID)
	}

	// 2. Ack Alert Group
	req = createAuthenticatedRequest(t, http.MethodPatch, "/api/v1/alert-groups/ag-api-1/ack", nil, user.ID)
	rec = httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK (Ack), got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify DB State
	updated, err := env.S.GetActiveAlertGroup(ag.DedupKey)
	if err != nil {
		t.Fatalf("Failed to fetch AG: %v", err)
	}
	if updated.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status Acknowledged, got %s", updated.Status)
	}

	// Verify Timeline
	events, _ := env.S.GetTimelineEvents(ag.ID)
	found := false
	for _, e := range events {
		if e.Type == model.TimelineEventAcknowledged {
			found = true
			break
		}
	}
	if !found {
		t.Error("Timeline event Acknowledged missing")
	}

	// 3. Resolve Alert Group
	req = createAuthenticatedRequest(t, http.MethodPatch, "/api/v1/alert-groups/ag-api-1/resolve", nil, user.ID)
	rec = httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK (Resolve), got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify DB (Direct query as GetActive filters resolved)
	// Verify DB (Direct query as GetActive filters resolved)
	var status string
	env.S.GetDB().QueryRow("SELECT status FROM alert_groups WHERE id=$1", "ag-api-1").Scan(&status)
	if status != string(model.AlertGroupStatusResolved) {
		t.Errorf("Expected status Resolved, got %s", status)
	}
}

func TestAPI_RBAC_Advanced(t *testing.T) {
	env := setupAPITest(t)

	// Setup:
	// 1. Global Admin
	globalAdmin := testutil.SeedUser(t, env.S, "globaladmin@example.com")
	globalAdmin.Role = model.UserRoleAdmin
	env.S.UpdateUser(globalAdmin)

	// 2. Team A with Admin A
	teamA := testutil.SeedTeam(t, env.S, "team-a")
	adminA := testutil.SeedUser(t, env.S, "admin-a@example.com")
	testutil.SeedTeamMember(t, env.S, teamA.ID, adminA.ID, model.TeamMemberRoleAdmin)

	// 3. Team B
	teamB := testutil.SeedTeam(t, env.S, "team-b")

	// 4. Regular User
	userReg := testutil.SeedUser(t, env.S, "regular@example.com")

	t.Run("Global Admin Impersonation (Cross-Team Access)", func(t *testing.T) {
		// Global Admin tries to add a member to Team A (where they are NOT a member)
		body := `{"user_id": "` + userReg.ID + `", "role": "team_member"}`
		req := createAuthenticatedRequest(t, http.MethodPost, "/api/v1/teams/"+teamA.ID+"/members", []byte(body), globalAdmin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Global Admin should be able to manage any team. Got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Cross-Team Isolation", func(t *testing.T) {
		// Admin of Team A tries to add a member to Team B
		body := `{"user_id": "` + userReg.ID + `", "role": "team_member"}`
		req := createAuthenticatedRequest(t, http.MethodPost, "/api/v1/teams/"+teamB.ID+"/members", []byte(body), adminA.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Team Admin A should NOT manage Team B. Got %d", rec.Code)
		}
	})

	t.Run("Team Role Persistence (Upsert)", func(t *testing.T) {
		// 1. Verify userReg is currently team_member in Team A (added by Global Admin above)
		members, _ := env.S.GetTeamMembers(teamA.ID)
		var initialRole model.TeamMemberRole
		for _, m := range members {
			if m.ID == userReg.ID {
				initialRole = m.TeamRole
			}
		}
		if initialRole != model.TeamMemberRoleMember {
			t.Errorf("Expected initial role 'team_member', got '%s'", initialRole)
		}

		// 2. Promote userReg to team_admin (using Admin A)
		body := `{"user_id": "` + userReg.ID + `", "role": "team_admin"}`
		req := createAuthenticatedRequest(t, http.MethodPost, "/api/v1/teams/"+teamA.ID+"/members", []byte(body), adminA.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("Failed to promote member: %d", rec.Code)
		}

		// 3. Verify Persistence in DB
		members, _ = env.S.GetTeamMembers(teamA.ID)
		var newRole model.TeamMemberRole
		for _, m := range members {
			if m.ID == userReg.ID {
				newRole = m.TeamRole
			}
		}
		if newRole != model.TeamMemberRoleAdmin {
			t.Errorf("Expected persisted role 'team_admin', got '%s'", newRole)
		}
	})

	t.Run("Last Admin Protection", func(t *testing.T) {
		// Try to demote Global Admin (who is the only admin) via API
		body := `{"role": "user"}`
		req := createAuthenticatedRequest(t, http.MethodPut, "/api/v1/users/"+globalAdmin.ID, []byte(body), globalAdmin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)

		if rec.Code != http.StatusConflict {
			t.Errorf("Should not allow demoting last admin. Got %d: %s", rec.Code, rec.Body.String())
		}
	})
}
