package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/dispatcher"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

const AuthCookieName = "access_token"

// LoginRequest represents the login credentials.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Login godoc
// @Summary Login
// @Description Authenticate user and set HTTPOnly cookie
// @Tags auth
// @Accept json
// @Produce json
// @Param credentials body LoginRequest true "Login credentials"
// @Success 200 {object} model.User
// @Failure 401 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/login [post]
func (a *API) Login(c echo.Context) error {
	var req LoginRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request"})
	}

	user, err := a.store.GetUserByEmail(req.Email)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid credentials"}) // generic error
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if !auth.CheckPasswordHash(req.Password, user.PasswordHash) {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid credentials"})
	}

	token, err := auth.GenerateToken(user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to generate token"})
	}

	// Update auth provider to password
	a.store.UpdateUserAuthProvider(user.ID, "password")

	// Set Cookie
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	c.SetCookie(&http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Expires:  time.Now().Add(auth.SessionDuration),
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("APP_ENV") == "production", // Set to true in prod (TLS)
		SameSite: http.SameSiteLaxMode,
	})

	return c.JSON(http.StatusOK, user)
}

// Logout godoc
// @Summary Logout
// @Description Clear authentication cookie
// @Tags auth
// @Success 200 "OK"
// @Router /api/auth/logout [post]
func (a *API) Logout(c echo.Context) error {
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	c.SetCookie(&http.Cookie{
		Name:     AuthCookieName,
		Value:    "",
		Expires:  time.Now().Add(-1 * time.Hour),
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("APP_ENV") == "production",
		SameSite: http.SameSiteLaxMode,
	})
	return c.JSON(http.StatusOK, map[string]string{"message": "logged out"})
}

// Me godoc
// @Summary Get current user
// @Description Get currently authenticated user details
// @Tags auth
// @Produce json
// @Success 200 {object} model.UserWithPermissions
// @Failure 401 {object} ErrorResponse
// @Router /api/auth/me [get]
func (a *API) Me(c echo.Context) error {
	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}

	// The active read: these are session paths, and an erased user has no
	// session left. GetUserByID exists for hydrating history, not for this.
	user, err := a.store.GetActiveUserByID(userID)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
	}
	user.Identities, _ = a.store.ListUserIdentities(userID)

	memberships, err := a.store.GetTeamMembershipsForUser(userID)
	if err != nil {
		// Log error but don't fail, return empty memberships?
		// Or fail? Better to fail safely or return empty.
		// Let's assume empty for robustness, or just fail 500.
		// Given this is auth/me, better to succeed with partial data or fail?
		// Fail is safer to avoid UI thinking no perms.
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to fetch permissions"})
	}

	return c.JSON(http.StatusOK, model.UserWithPermissions{
		User:  *user,
		Teams: memberships,
	})
}

// UpdateMeRequest represents a request to update current user profile.
type UpdateMeRequest struct {
	Name *string `json:"name,omitempty"`
}

// UpdateMe godoc
// @Summary Update current user profile
// @Description Update the current user's name. SSO users cannot change their name.
// @Tags auth
// @Accept json
// @Produce json
// @Param profile body UpdateMeRequest true "Profile updates"
// @Success 200 {object} model.User
// @Failure 400 {object} ErrorResponse
// @Failure 401 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Router /api/auth/me [patch]
func (a *API) UpdateMe(c echo.Context) error {
	// Profile updates require session authentication (no Bearer tokens)
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}

	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}

	// The active read: these are session paths, and an erased user has no
	// session left. GetUserByID exists for hydrating history, not for this.
	user, err := a.store.GetActiveUserByID(userID)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
	}

	var req UpdateMeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// SSO users cannot change their name (it's synced from provider)
	if req.Name != nil && user.AuthProvider == "oidc" {
		return c.JSON(http.StatusForbidden, ErrorResponse{Error: "SSO users cannot change their name"})
	}

	// Update fields
	if req.Name != nil {
		user.Name = *req.Name
	}

	if err := a.store.UpdateUser(user); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, user)
}

// AuthMiddleware validates the JWT cookie or Bearer API token
func (a *API) AuthMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// 1. Check for Bearer token first (API automation)
		authHeader := c.Request().Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			apiToken, err := a.validateAPIToken(token)
			if err == nil {
				c.Set("user_id", apiToken.UserID)
				c.Set("api_token", true)
				return next(c)
			}
			// Distinguish between "not found" (invalid token) and server errors
			if err == sql.ErrNoRows || err.Error() == "token expired" {
				return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid api token"})
			}
			// Server error (DB issues, etc)
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "authentication error"})
		}

		// 2. Fall back to cookie auth (browser)
		cookie, err := c.Cookie(AuthCookieName)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "missing session"})
		}

		claims, err := auth.ValidateToken(cookie.Value)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid session"})
		}

		// Verify the user still exists AND has not been erased. GetUserByID
		// would answer for an erased user - it is the display read that keeps
		// history legible - and that is exactly how an erased person's token
		// kept working: soft delete has to end the session on the next
		// request, not at the next natural expiry.
		if _, err := a.store.GetActiveUserByID(claims.UserID); err != nil {
			if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
				return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
			}
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "authentication error"})
		}

		c.Set("user_id", claims.UserID)
		return next(c)
	}
}

// requireSessionAuth checks that the request uses cookie auth, not Bearer token.
// This is used for sensitive operations like token management where we don't want
// a stolen API token to be able to create more tokens.
func (a *API) requireSessionAuth(c echo.Context) error {
	if isAPIToken, _ := c.Get("api_token").(bool); isAPIToken {
		return c.JSON(http.StatusForbidden, ErrorResponse{Error: "session authentication required"})
	}
	return nil
}

// validateAPIToken validates an API token and returns the token record
func (a *API) validateAPIToken(token string) (*model.APIToken, error) {
	// Hash the token
	hash := sha256.Sum256([]byte(token))
	hashStr := hex.EncodeToString(hash[:])

	// Look up token by hash
	apiToken, err := a.store.GetAPITokenByHash(hashStr)
	if err != nil {
		return nil, err
	}

	// Check expiration
	if apiToken.ExpiresAt != nil && time.Now().After(*apiToken.ExpiresAt) {
		return nil, errors.New("token expired")
	}

	// Update last used (async, ignore errors)
	go a.store.UpdateAPITokenLastUsed(apiToken.ID)

	return apiToken, nil
}

// ========================================
// OIDC Handlers
// ========================================

// OIDCConfigResponse represents the OIDC configuration for the frontend.
type OIDCConfigResponse struct {
	Enabled  bool   `json:"enabled"`
	LoginURL string `json:"login_url,omitempty"`
}

// OIDCConfig godoc
// @Summary Get OIDC configuration
// @Description Returns OIDC status and login URL for frontend
// @Tags auth
// @Produce json
// @Success 200 {object} OIDCConfigResponse
// @Router /api/auth/oidc/config [get]
func (a *API) OIDCConfig(c echo.Context) error {
	if a.oidcProvider == nil || !a.oidcProvider.IsEnabled() {
		return c.JSON(http.StatusOK, OIDCConfigResponse{Enabled: false})
	}

	return c.JSON(http.StatusOK, OIDCConfigResponse{
		Enabled:  true,
		LoginURL: "/api/auth/oidc/redirect",
	})
}

// OIDCRedirect godoc
// @Summary Redirect to OIDC provider
// @Description Initiates OIDC login flow
// @Tags auth
// @Success 302 "Redirect to OIDC provider"
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/oidc/redirect [get]
func (a *API) OIDCRedirect(c echo.Context) error {
	if a.oidcProvider == nil || !a.oidcProvider.IsEnabled() {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "OIDC not configured"})
	}

	// Generate random state
	state := generateRandomState()

	// Store state in cookie for CSRF protection
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	c.SetCookie(&http.Cookie{
		Name:     "oidc_state",
		Value:    state,
		MaxAge:   300, // 5 minutes
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("APP_ENV") == "production",
		SameSite: http.SameSiteLaxMode,
	})

	authURL := a.oidcProvider.GetAuthURL(state)
	return c.Redirect(http.StatusFound, authURL)
}

// OIDCCallback godoc
// @Summary Handle OIDC callback
// @Description Completes OIDC login flow, creates user if needed
// @Tags auth
// @Param code query string true "Authorization code"
// @Param state query string true "State parameter"
// @Success 302 "Redirect to home"
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/oidc/callback [get]
func (a *API) OIDCCallback(c echo.Context) error {
	if a.oidcProvider == nil || !a.oidcProvider.IsEnabled() {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "OIDC not configured"})
	}

	// Verify state (CSRF protection)
	stateCookie, err := c.Cookie("oidc_state")
	if err != nil || stateCookie.Value != c.QueryParam("state") {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid state"})
	}

	// Clear state cookie
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	c.SetCookie(&http.Cookie{
		Name:     "oidc_state",
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("APP_ENV") == "production",
		SameSite: http.SameSiteLaxMode,
	})

	// Exchange code for token
	code := c.QueryParam("code")
	if code == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "missing authorization code"})
	}

	userInfo, err := a.oidcProvider.ExchangeCode(c.Request().Context(), code)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to exchange code: " + err.Error()})
	}

	// Validate domain
	if !a.oidcProvider.ValidateDomain(userInfo.Email) {
		return c.JSON(http.StatusForbidden, ErrorResponse{Error: "email domain not allowed"})
	}

	// Find or create user
	user, err := a.store.GetUserByEmail(userInfo.Email)
	if err != nil {
		if err == sql.ErrNoRows {
			// Create new user
			user = &model.User{
				ID:           uuid.New().String(),
				Email:        userInfo.Email,
				Name:         userInfo.Name,
				AuthProvider: "oidc",
				CreatedAt:    time.Now(),
			}
			if user.Name == "" {
				// Use email prefix as name if not provided
				parts := strings.Split(userInfo.Email, "@")
				user.Name = parts[0]
			}
			if err := a.store.CreateUser(user); err != nil {
				return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to create user: " + err.Error()})
			}
		} else {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
	} else {
		// Update auth provider and sync name from OIDC for existing user
		a.store.UpdateUserAuthProvider(user.ID, "oidc")

		// Sync name from OIDC if available and different
		if userInfo.Name != "" && userInfo.Name != user.Name {
			user.Name = userInfo.Name
			a.store.UpdateUser(user)
		}
	}

	// Background Slack auto-link for SSO users (only when Slack integration is configured).
	// tryLinkSlackUser does a cheap DB pre-check before hitting Slack's users.lookupByEmail,
	// so already-linked users incur no API call.
	if a.slack != nil && a.integrationCache != nil && a.integrationCache.GetSlackToken() != "" {
		go a.tryLinkSlackUser(user.ID, userInfo.Email)
	}

	// Generate JWT token
	token, err := auth.GenerateToken(user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to generate token"})
	}

	// Set auth cookie
	// nosemgrep: go.lang.security.audit.net.cookie-missing-secure.cookie-missing-secure
	c.SetCookie(&http.Cookie{
		Name:     AuthCookieName,
		Value:    token,
		Expires:  time.Now().Add(auth.SessionDuration),
		Path:     "/",
		HttpOnly: true,
		Secure:   os.Getenv("APP_ENV") == "production",
		SameSite: http.SameSiteLaxMode,
	})

	// Redirect to home page
	return c.Redirect(http.StatusFound, "/")
}

// tryLinkSlackUser attempts to auto-link a user's Slack account by looking up
// their email in the Slack workspace. Runs in a background goroutine during SSO login.
//
// Cheap pre-check first: skip the Slack API call entirely for already-linked users.
// (Sprint 3 dropped the in-process user.SlackUserID guard; without this we'd hit
// users.lookupByEmail on every OIDC login — rate limit / perf regression.)
func (a *API) tryLinkSlackUser(userID, email string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ident, err := a.store.GetExternalIdentity(userID, "slack"); err == nil && ident != nil {
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("tryLinkSlackUser: identity lookup failed for user %s: %v", userID, err)
		return
	}

	slackUserID, err := a.slack.GetSlackUserIDByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, dispatcher.ErrSlackUserNotFound) {
			log.Printf("tryLinkSlackUser: slack lookup failed for user %s: %v", userID, err)
		}
		return
	}

	changed, err := a.store.BindExternalIdentityIfAbsent(userID, "slack", slackUserID, "")
	if err != nil {
		// ErrExternalIdentityAlreadyLinked means a *different* user owns that Slack id;
		// fall through without surfacing — user can still link manually via OTP.
		if !errors.Is(err, store.ErrExternalIdentityAlreadyLinked) {
			log.Printf("tryLinkSlackUser: failed to save slack link for user %s: %v", userID, err)
		}
		return
	}

	if changed {
		log.Printf("tryLinkSlackUser: linked user %s to Slack %s", userID, slackUserID)
		metrics.SlackUserLinkedTotal.WithLabelValues("sso_email").Inc()
	}
}

func generateRandomState() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
