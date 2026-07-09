package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// ========================================
// API Token Management Endpoints
// ========================================

// CreateAPITokenRequest represents a request to create a new API token.
type CreateAPITokenRequest struct {
	Name      string `json:"name"`                 // Required: descriptive name for the token
	ExpiresIn *int   `json:"expires_in,omitempty"` // Optional: expiration in days (nil = no expiration)
}

// CreateAPITokenResponse returns the created token (plain text shown once).
type CreateAPITokenResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Token     string     `json:"token"` // Plain token - shown ONCE
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// APITokenListItem represents a token in the list (without hash or plain token).
type APITokenListItem struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// APITokenListResponse represents a list of user's tokens.
type APITokenListResponse struct {
	Tokens []*APITokenListItem `json:"tokens"`
	Total  int                 `json:"total"`
}

// CreateAPIToken godoc
// @Summary Create a new API token
// @Description Generate a new API token for authentication. The token is shown only once.
// @Tags api-tokens
// @Accept json
// @Produce json
// @Param token body CreateAPITokenRequest true "Token configuration"
// @Success 201 {object} CreateAPITokenResponse
// @Failure 400 {object} ErrorResponse
// @Failure 401 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/tokens [post]
func (a *API) CreateAPIToken(c echo.Context) error {
	// Require session auth - don't allow Bearer tokens to create more tokens
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}

	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}

	var req CreateAPITokenRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "name is required"})
	}

	// Generate random token: tok_<32 random hex chars>
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to generate token"})
	}
	plainToken := "tok_" + hex.EncodeToString(tokenBytes)

	// Hash the token for storage
	hash := sha256.Sum256([]byte(plainToken))
	hashStr := hex.EncodeToString(hash[:])

	// Calculate expiration
	var expiresAt *time.Time
	if req.ExpiresIn != nil && *req.ExpiresIn > 0 {
		exp := time.Now().Add(time.Duration(*req.ExpiresIn) * 24 * time.Hour)
		expiresAt = &exp
	}

	// Create token record
	token := &model.APIToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      req.Name,
		TokenHash: hashStr,
		ExpiresAt: expiresAt,
		CreatedAt: time.Now(),
	}

	if err := a.store.CreateAPIToken(token); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusCreated, CreateAPITokenResponse{
		ID:        token.ID,
		Name:      token.Name,
		Token:     plainToken, // Show only once!
		ExpiresAt: expiresAt,
		CreatedAt: token.CreatedAt,
	})
}

// ListAPITokens godoc
// @Summary List user's API tokens
// @Description Get all API tokens for the current user
// @Tags api-tokens
// @Produce json
// @Success 200 {object} APITokenListResponse
// @Failure 401 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/tokens [get]
func (a *API) ListAPITokens(c echo.Context) error {
	// Require session auth - token list is sensitive
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}

	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}

	tokens, err := a.store.GetUserAPITokens(userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Convert to list items (without hash)
	var items []*APITokenListItem
	for _, t := range tokens {
		items = append(items, &APITokenListItem{
			ID:         t.ID,
			Name:       t.Name,
			ExpiresAt:  t.ExpiresAt,
			LastUsedAt: t.LastUsedAt,
			CreatedAt:  t.CreatedAt,
		})
	}

	if items == nil {
		items = []*APITokenListItem{}
	}

	return c.JSON(http.StatusOK, APITokenListResponse{
		Tokens: items,
		Total:  len(items),
	})
}

// DeleteAPIToken godoc
// @Summary Delete (revoke) an API token
// @Description Delete an API token by ID. The token will no longer work.
// @Tags api-tokens
// @Param id path string true "Token ID"
// @Success 204 "No Content"
// @Failure 401 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/tokens/{id} [delete]
func (a *API) DeleteAPIToken(c echo.Context) error {
	// Require session auth - don't allow Bearer tokens to revoke tokens
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}

	tokenID := c.Param("id")

	// RBAC Middleware (ScopeFromResource check) ensures the token exists
	// and belongs to the current user (ScopeUserSelfOrAdmin rule).

	if err := a.store.DeleteAPIToken(tokenID); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}
