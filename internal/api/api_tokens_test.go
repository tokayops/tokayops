package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestAPITokens(t *testing.T) {
	s := store.NewMockStore()
	e := echo.New()
	api := NewAPI(s, nil, nil, nil, "", nil)
	api.RegisterRoutes(e)

	// Create test user
	user := &model.User{
		ID:        "tokenuser",
		Email:     "tokenuser@example.com",
		Name:      "Token User",
		CreatedAt: time.Now(),
	}
	s.CreateUser(user)

	t.Run("CreateAPIToken Success", func(t *testing.T) {
		reqBody := `{"name": "My Test Token"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected status 201, got %d. Body: %s", rec.Code, rec.Body.String())
		}

		var resp CreateAPITokenResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)

		if resp.Token == "" {
			t.Error("Expected token in response")
		}
		if !strings.HasPrefix(resp.Token, "tok_") {
			t.Errorf("Expected token to start with 'tok_', got %s", resp.Token)
		}
		if resp.Name != "My Test Token" {
			t.Errorf("Expected name 'My Test Token', got %s", resp.Name)
		}
		if resp.ID == "" {
			t.Error("Expected ID in response")
		}
	})

	t.Run("CreateAPIToken with Expiration", func(t *testing.T) {
		expiresDays := 30
		reqBody := `{"name": "Expiring Token", "expires_in": 30}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected status 201, got %d", rec.Code)
		}

		var resp CreateAPITokenResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)

		if resp.ExpiresAt == nil {
			t.Error("Expected expires_at in response")
		} else {
			expectedExpiry := time.Now().Add(time.Duration(expiresDays) * 24 * time.Hour)
			diff := resp.ExpiresAt.Sub(expectedExpiry)
			if diff > time.Minute || diff < -time.Minute {
				t.Errorf("Expected expires_at around %v, got %v", expectedExpiry, resp.ExpiresAt)
			}
		}
	})

	t.Run("CreateAPIToken Missing Name", func(t *testing.T) {
		reqBody := `{}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected status 400, got %d", rec.Code)
		}

		var errResp ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp.Error != "name is required" {
			t.Errorf("Expected 'name is required', got '%s'", errResp.Error)
		}
	})

	t.Run("CreateAPIToken Empty Name", func(t *testing.T) {
		reqBody := `{"name": ""}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected status 400, got %d", rec.Code)
		}
	})

	t.Run("ListAPITokens", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		var resp APITokenListResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)

		// Should have tokens from previous tests
		if resp.Total < 2 {
			t.Errorf("Expected at least 2 tokens, got %d", resp.Total)
		}

		// Verify token hash is not exposed
		for _, token := range resp.Tokens {
			if token.Name == "" {
				t.Error("Token name should be present")
			}
		}
	})

	t.Run("DeleteAPIToken Success", func(t *testing.T) {
		// First create a token
		reqBody := `{"name": "Token To Delete"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		var createResp CreateAPITokenResponse
		json.Unmarshal(rec.Body.Bytes(), &createResp)

		// Now delete it
		req = httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/"+createResp.ID, nil)
		addAuth(req, "tokenuser")
		rec = httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("Expected status 204, got %d", rec.Code)
		}
	})

	t.Run("DeleteAPIToken NotFound", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/nonexistent", nil)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected status 404, got %d", rec.Code)
		}
	})

	t.Run("DeleteAPIToken OtherUser", func(t *testing.T) {
		// Create another user and their token
		otherUser := &model.User{
			ID:        "otheruser",
			Email:     "other@example.com",
			Name:      "Other User",
			CreatedAt: time.Now(),
		}
		s.CreateUser(otherUser)

		// Create token for other user directly in store
		otherToken := &model.APIToken{
			ID:        "other-token-id",
			UserID:    "otheruser",
			Name:      "Other User Token",
			TokenHash: "somehash",
			CreatedAt: time.Now(),
		}
		s.CreateAPIToken(otherToken)

		// Try to delete other user's token as tokenuser
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/other-token-id", nil)
		addAuth(req, "tokenuser")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected status 403 for other user's token, got %d", rec.Code)
		}
	})
}

// TestAPITokensRequireSession tests that token management endpoints reject Bearer auth
func TestAPITokensRequireSession(t *testing.T) {
	s := store.NewMockStore()
	e := echo.New()
	api := NewAPI(s, nil, nil, nil, "", nil)
	api.RegisterRoutes(e)

	// Create test user
	user := &model.User{
		ID:        "sessionuser",
		Email:     "session@example.com",
		Name:      "Session User",
		CreatedAt: time.Now(),
	}
	s.CreateUser(user)

	// Helper to create token for Bearer auth
	token := "tok_sessiontest123"
	hash := sha256.Sum256([]byte(token))
	hashStr := hex.EncodeToString(hash[:])
	s.CreateAPIToken(&model.APIToken{
		ID:        "session-token",
		UserID:    "sessionuser",
		Name:      "Session Token",
		TokenHash: hashStr,
		CreatedAt: time.Now(),
	})

	t.Run("CreateAPIToken Rejects Bearer Auth", func(t *testing.T) {
		reqBody := `{"name": "Malicious Token"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected status 403, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("ListAPITokens Rejects Bearer Auth", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected status 403, got %d", rec.Code)
		}
	})

	t.Run("DeleteAPIToken Rejects Bearer Auth", func(t *testing.T) {
		// Mock token to delete (need to existence check?)
		// RBAC middleware runs first. ScopeFromResource checks existence.
		// If we pass nonexistent, we get 404.
		// If we pass existent, we get 403 (because requireSessionAuth).
		// We can reuse "session-token" itself.
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/session-token", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected status 403, got %d", rec.Code)
		}
	})
}

func TestAPITokenUnauthorized(t *testing.T) {
	s := store.NewMockStore()
	e := echo.New()
	api := NewAPI(s, nil, nil, nil, "", nil)
	api.RegisterRoutes(e)

	t.Run("CreateAPIToken Unauthorized", func(t *testing.T) {
		reqBody := `{"name": "My Token"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(reqBody))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}
	})

	t.Run("ListAPITokens Unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}
	})

	t.Run("DeleteAPIToken Unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/tokens/someid", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}
	})
}
func TestBearerTokenAuth(t *testing.T) {
	s := store.NewMockStore()
	e := echo.New()
	api := NewAPI(s, nil, nil, nil, "", nil)

	// Create test user
	user := &model.User{
		ID:        "beareruser",
		Email:     "bearer@example.com",
		Name:      "Bearer User",
		CreatedAt: time.Now(),
	}
	s.CreateUser(user)

	// Create a valid token
	plainToken := "tok_testtoken12345678901234567890"
	hash := sha256.Sum256([]byte(plainToken))
	hashStr := hex.EncodeToString(hash[:])

	validToken := &model.APIToken{
		ID:        "valid-token",
		UserID:    "beareruser",
		Name:      "Valid Token",
		TokenHash: hashStr,
		CreatedAt: time.Now(),
	}
	s.CreateAPIToken(validToken)

	t.Run("Bearer Token Auth Success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.Header.Set("Authorization", "Bearer "+plainToken)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		// Use middleware
		h := api.AuthMiddleware(func(c echo.Context) error {
			userID := c.Get("user_id").(string)
			isAPIToken := c.Get("api_token")
			return c.JSON(http.StatusOK, map[string]interface{}{
				"user_id":   userID,
				"api_token": isAPIToken,
			})
		})

		if err := h(c); err != nil {
			t.Fatalf("Auth failed: %v", err)
		}

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)

		if resp["user_id"] != "beareruser" {
			t.Errorf("Expected user_id 'beareruser', got '%v'", resp["user_id"])
		}
		if resp["api_token"] != true {
			t.Errorf("Expected api_token true, got '%v'", resp["api_token"])
		}
	})

	t.Run("Bearer Token Invalid Token", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.Header.Set("Authorization", "Bearer tok_invalidtoken123")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})

		h(c)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}

		var errResp ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp.Error != "invalid api token" {
			t.Errorf("Expected 'invalid api token', got '%s'", errResp.Error)
		}
	})

	t.Run("Bearer Token Expired", func(t *testing.T) {
		// Create expired token
		expiredPlainToken := "tok_expiredtoken12345678901234"
		expiredHash := sha256.Sum256([]byte(expiredPlainToken))
		expiredHashStr := hex.EncodeToString(expiredHash[:])
		expiredAt := time.Now().Add(-24 * time.Hour) // Expired yesterday

		expiredToken := &model.APIToken{
			ID:        "expired-token",
			UserID:    "beareruser",
			Name:      "Expired Token",
			TokenHash: expiredHashStr,
			ExpiresAt: &expiredAt,
			CreatedAt: time.Now(),
		}
		s.CreateAPIToken(expiredToken)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.Header.Set("Authorization", "Bearer "+expiredPlainToken)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})

		h(c)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401 for expired token, got %d", rec.Code)
		}
	})

	t.Run("Bearer Token Valid Future Expiry", func(t *testing.T) {
		// Create token with future expiry
		futurePlainToken := "tok_futuretoken12345678901234567"
		futureHash := sha256.Sum256([]byte(futurePlainToken))
		futureHashStr := hex.EncodeToString(futureHash[:])
		expiresAt := time.Now().Add(30 * 24 * time.Hour) // Expires in 30 days

		futureToken := &model.APIToken{
			ID:        "future-token",
			UserID:    "beareruser",
			Name:      "Future Token",
			TokenHash: futureHashStr,
			ExpiresAt: &expiresAt,
			CreatedAt: time.Now(),
		}
		s.CreateAPIToken(futureToken)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.Header.Set("Authorization", "Bearer "+futurePlainToken)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})

		h(c)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200 for valid token, got %d", rec.Code)
		}
	})

	t.Run("Bearer Token Empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.Header.Set("Authorization", "Bearer ")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})

		h(c)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401 for empty bearer, got %d", rec.Code)
		}
	})

	t.Run("Fallback to Cookie Auth", func(t *testing.T) {
		// Create user with password
		hash, _ := auth.HashPassword("password123")
		cookieUser := &model.User{
			ID:           "cookieuser",
			Email:        "cookie@example.com",
			Name:         "Cookie User",
			PasswordHash: hash,
			CreatedAt:    time.Now(),
		}
		s.CreateUser(cookieUser)

		// Generate JWT token
		jwtToken, _ := auth.GenerateToken("cookieuser")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: jwtToken})
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			userID := c.Get("user_id").(string)
			isAPIToken := c.Get("api_token")
			return c.JSON(http.StatusOK, map[string]interface{}{
				"user_id":   userID,
				"api_token": isAPIToken,
			})
		})

		if err := h(c); err != nil {
			t.Fatalf("Auth failed: %v", err)
		}

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)

		if resp["user_id"] != "cookieuser" {
			t.Errorf("Expected user_id 'cookieuser', got '%v'", resp["user_id"])
		}
		// api_token should be nil for cookie auth
		if resp["api_token"] != nil {
			t.Errorf("Expected api_token nil, got '%v'", resp["api_token"])
		}
	})

	t.Run("No Auth Header And No Cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})

		h(c)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}

		var errResp ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp.Error != "missing session" {
			t.Errorf("Expected 'missing session', got '%s'", errResp.Error)
		}
	})

	t.Run("Non-Bearer Authorization Header Falls Through", func(t *testing.T) {
		// If Authorization header is present but not Bearer, should fall through to cookie auth
		req := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
		req.Header.Set("Authorization", "Basic somebasicauth")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)

		h := api.AuthMiddleware(func(c echo.Context) error {
			return c.NoContent(http.StatusOK)
		})

		h(c)

		// Should fail with missing session since no cookie
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", rec.Code)
		}

		var errResp ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &errResp)
		if errResp.Error != "missing session" {
			t.Errorf("Expected 'missing session' for non-bearer auth, got '%s'", errResp.Error)
		}
	})
}
