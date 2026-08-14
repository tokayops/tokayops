package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestAuth(t *testing.T) {
	// Setup Store using MockStore
	s := store.NewMockStore()

	// Create a test user with known password
	password := "secret123"
	hash, _ := auth.HashPassword(password)
	user := &model.User{
		ID:           "testuser",
		Email:        "test@example.com",
		Name:         "Test User",
		PasswordHash: hash,
		CreatedAt:    time.Now(),
	}
	if err := s.CreateUser(user); err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Setup Echo
	e := echo.New()
	api := NewAPI(s, nil, nil, nil, "", nil)

	t.Run("Login Success", func(t *testing.T) {
		reqBody := `{"email":"test@example.com", "password":"secret123"}`
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		if err := api.Login(c); err != nil {
			t.Fatalf("Login failed: %v", err)
		}

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		// Check Cookie
		cookies := rec.Result().Cookies()
		var authCookie *http.Cookie
		for _, cookie := range cookies {
			if cookie.Name == AuthCookieName {
				authCookie = cookie
				break
			}
		}

		if authCookie == nil {
			t.Fatal("Auth cookie not set")
		}
		if authCookie.HttpOnly != true {
			t.Error("Cookie should be HttpOnly")
		}
	})

	t.Run("Login Fail", func(t *testing.T) {
		reqBody := `{"email":"test@example.com", "password":"wrong"}`
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		api.Login(c)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}
	})

	t.Run("Me Protected Success", func(t *testing.T) {
		// Generate valid token
		token, _ := auth.GenerateToken("testuser")

		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Manually wrap with middleware for testing logic or just call handler with context set
		// Testing middleware integration:
		h := api.AuthMiddleware(api.Me)
		if err := h(c); err != nil {
			t.Fatalf("Me request failed: %v", err)
		}

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		var respUser model.User
		json.Unmarshal(rec.Body.Bytes(), &respUser)
		if respUser.ID != "testuser" {
			t.Errorf("Expected user ID testuser, got %s", respUser.ID)
		}
	})

	t.Run("Me Permissions", func(t *testing.T) {
		// Assign user to devops team as admin
		s.AddTeamMember("devops", "testuser", model.TeamMemberRoleAdmin)

		token, _ := auth.GenerateToken("testuser")
		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(api.Me)
		if err := h(c); err != nil {
			t.Fatalf("Me request failed: %v", err)
		}

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		var resp model.UserWithPermissions
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("Failed to parse response: %v", err)
		}

		if resp.ID != "testuser" {
			t.Errorf("Expected user ID testuser, got %s", resp.ID)
		}
		if len(resp.Teams) != 1 {
			t.Errorf("Expected 1 team membership, got %d", len(resp.Teams))
		}
		if role, ok := resp.Teams["devops"]; !ok || role != model.TeamMemberRoleAdmin {
			t.Errorf("Expected devops:team_admin, got %v", resp.Teams["devops"])
		}
	})

	t.Run("Me Protected Fail", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		// No cookie
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(api.Me)
		h(c) // Should fail

	})
}

// TestUpdateUser_RolePromotion verifies that an admin can promote a user.
func TestUpdateUser_RolePromotion(t *testing.T) {
	api, _, e := setupTestAPI(t)

	// Create a target user (initial role: user)
	targetUser := &model.User{
		ID:           "target-user-id",
		Email:        "target@example.com",
		Name:         "Target User",
		Role:         model.UserRoleUser,
		PasswordHash: "hash",
	}
	api.store.(*store.MockStore).CreateUser(targetUser)

	// Update request to promote to admin
	reqBody := `{"role": "admin"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/target-user-id", strings.NewReader(reqBody))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	// Execute handler
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues("target-user-id")

	if assert.NoError(t, api.UpdateUser(c)) {
		assert.Equal(t, http.StatusOK, rec.Code)

		// Verify persistence
		updated, _ := api.store.GetUserByID("target-user-id")
		assert.Equal(t, model.UserRoleAdmin, updated.Role)
	}
}
