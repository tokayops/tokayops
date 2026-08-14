package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

// telegramLinkTTL is how long a deep-link token stays valid.
const telegramLinkTTL = 15 * time.Minute

// telegramLinkRetries bounds the (provider, token_hash) collision retry. A
// 32-byte random token makes a collision astronomically unlikely; we mirror the
// Slack bounded loop only for safety.
const telegramLinkRetries = 3

// generateTelegramToken returns a high-entropy URL-safe bearer token (32 bytes →
// 43 base64url chars, within Telegram's 64-char start= limit). Unlike a 6-digit
// OTP this is unguessable, so ConsumeLinkToken needs no attempt-counting.
func generateTelegramToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RequestTelegramLink godoc
// @Summary Request a Telegram deep link
// @Description Issue a one-time deep-link token and return a t.me/<bot>?start=<token> URL. The user opens it and presses Start to link their Telegram account.
// @Tags auth
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 409 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/telegram/link [post]
func (a *API) RequestTelegramLink(c echo.Context) error {
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}
	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}
	if _, err := a.store.GetActiveUserByID(userID); err != nil {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
	}
	if a.telegram == nil {
		return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "telegram integration not configured"})
	}
	// Linking completes via the /start webhook, which requires a public TOKAY_SELF_URL.
	// Fail fast with a clear message instead of issuing a deep link that can never
	// link (the user would press Start and "Refresh" forever).
	if a.selfURL == "" {
		return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "telegram linking unavailable: server TOKAY_SELF_URL is not configured (interactivity disabled) — contact an admin"})
	}

	// Already linked? Avoid issuing a second link / overwriting silently.
	existing, err := a.store.GetExternalIdentity(userID, "telegram")
	if err != nil && err != sql.ErrNoRows {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to check existing binding"})
	}
	if err == nil && existing != nil {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "your account is already connected to telegram"})
	}

	botUsername, err := a.telegram.BotUsername(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "telegram bot unavailable: " + err.Error()})
	}

	// Issue a high-entropy token with an empty external_id — the Telegram user id
	// is unknown until /start, where ConsumeLinkToken fills it in.
	var token string
	expiresAt := time.Now().Add(telegramLinkTTL)
	for attempt := 0; attempt < telegramLinkRetries; attempt++ {
		token, err = generateTelegramToken()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to generate token"})
		}
		err = a.store.IssueLinkToken(userID, "telegram", "", token, expiresAt)
		if err == nil {
			break
		}
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to issue link token: " + err.Error()})
	}

	link := fmt.Sprintf("https://t.me/%s?start=%s", botUsername, token)
	return c.JSON(http.StatusOK, map[string]string{"link": link})
}

// UnbindTelegram godoc
// @Summary Unbind Telegram account
// @Description Remove the Telegram identity binding from the current user
// @Tags auth
// @Success 204
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/me/telegram [delete]
func (a *API) UnbindTelegram(c echo.Context) error {
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}
	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}
	if err := a.store.UnbindExternalIdentity(userID, "telegram"); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}
