package api

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/store"
)

// slackOTPTTL is how long an issued Slack OTP code stays valid (the 5-minute
// window this has always had).
const slackOTPTTL = 5 * time.Minute

// otpIssueRetries bounds the retry loop on the rare (provider, token_hash) collision in IssueLinkToken
// - for a 6-digit code there are 1M values, so a fresh draw resolves it.
const otpIssueRetries = 5

// generateSlackOTP returns a fresh 6-digit code.
func generateSlackOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// RequestSlackCodeRequest represents data needed to request OTP
type RequestSlackCodeRequest struct {
	SlackUserID string `json:"slack_user_id"`
}

// RequestSlackCode godoc
// @Summary Request a Slack OTP code
// @Description Send an OTP code to the specified Slack User ID via DM
// @Tags auth
// @Accept json
// @Produce json
// @Param request body RequestSlackCodeRequest true "Request data"
// @Success 200 {object} map[string]string
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/me/slack/request-code [post]
func (a *API) RequestSlackCode(c echo.Context) error {
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

	var req RequestSlackCodeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}
	if req.SlackUserID == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "slack_user_id is required"})
	}

	// 1. Prevent linking a Slack id already bound to a different user.
	existing, err := a.store.GetUserByExternalID("slack", req.SlackUserID)
	if err != nil && err != sql.ErrNoRows {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to check existing binding"})
	}
	if err == nil && existing.ID != userID {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "this slack account is already connected to another user"})
	}

	// 2. Issue a 6-digit link token. Retry on the (rare) (provider, token_hash) collision.
	var code string
	expiresAt := time.Now().Add(slackOTPTTL)
	for attempt := 0; attempt < otpIssueRetries; attempt++ {
		code, err = generateSlackOTP()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to generate code"})
		}
		err = a.store.IssueLinkToken(userID, "slack", req.SlackUserID, code, expiresAt)
		if err == nil {
			break
		}
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to issue link token: " + err.Error()})
	}

	// 3. Deliver via Slack DM.
	if err := a.slack.SendDM(c.Request().Context(), req.SlackUserID, "Your TokayOps One-Time Password is: "+code+". It expires in 5 minutes."); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to send DM: " + err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "OTP sent"})
}

// ConfirmSlackCodeRequest represents data needed to confirm OTP
type ConfirmSlackCodeRequest struct {
	Code string `json:"code"`
}

// ConfirmSlackCode godoc
// @Summary Confirm Slack OTP code
// @Description Verify the OTP code and bind the Slack account
// @Tags auth
// @Accept json
// @Produce json
// @Param request body ConfirmSlackCodeRequest true "Confirm data"
// @Success 200 {object} model.User
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/me/slack/confirm-code [post]
func (a *API) ConfirmSlackCode(c echo.Context) error {
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}

	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}

	user, err := a.store.GetActiveUserByID(userID)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
	}

	var req ConfirmSlackCodeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}
	if req.Code == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "code is required"})
	}

	if _, err := a.store.ConfirmIdentityLink(userID, "slack", req.Code); err != nil {
		switch {
		case errors.Is(err, store.ErrLinkTokenInvalid):
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid code"})
		case errors.Is(err, store.ErrLinkTokenExpired):
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "code expired or too many attempts"})
		case errors.Is(err, store.ErrExternalIdentityAlreadyLinked):
			return c.JSON(http.StatusConflict, ErrorResponse{Error: "this slack account is already connected to another user"})
		default:
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
	}

	// Refresh user with the newly bound identity for the response.
	user.Identities, _ = a.store.ListUserIdentities(userID)
	return c.JSON(http.StatusOK, user)
}

// UnbindSlack godoc
// @Summary Unbind Slack account
// @Description Remove the Slack identity binding from the current user
// @Tags auth
// @Accept json
// @Produce json
// @Success 204
// @Failure 500 {object} ErrorResponse
// @Router /api/auth/me/slack [delete]
func (a *API) UnbindSlack(c echo.Context) error {
	if err := a.requireSessionAuth(c); err != nil {
		return err
	}

	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}

	if err := a.store.UnbindExternalIdentity(userID, "slack"); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}
