package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rbac"
	"github.com/tokayops/tokayops/internal/slackcard"
)

// Re-export Slack action IDs from model for use in tests and this package.
const (
	SlackActionAckAlertGroup     = model.SlackActionAckAlertGroup
	SlackActionResolveAlertGroup = model.SlackActionResolveAlertGroup
)

// slackMeta is the metadata attached to timeline events originating from Slack.
var slackMeta = map[string]string{"source": "slack"}

// SlackSignatureMiddleware validates Slack request signatures using HMAC-SHA256.
// It checks X-Slack-Request-Timestamp and X-Slack-Signature headers against the
// signing secret from integrationCache. Body is restored for downstream handlers.
func (a *API) SlackSignatureMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		var secret string
		if a.integrationCache != nil {
			secret = a.integrationCache.GetSlackSigningSecret()
		}
		if secret == "" {
			c.Logger().Error("slack/interactive: signing secret not configured")
			return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "slack signing secret not configured"})
		}

		// 1. Read and validate timestamp header
		ts := c.Request().Header.Get("X-Slack-Request-Timestamp")
		if ts == "" {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "missing timestamp"})
		}
		tsInt, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid timestamp"})
		}
		if math.Abs(float64(time.Now().Unix()-tsInt)) > 5*60 {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "timestamp too old"})
		}

		// 2. Read raw body (capped at 1 MB to prevent memory abuse) and restore it for the handler
		const maxBodySize = 1 << 20 // 1 MB — Slack payloads are typically <100 KB
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodySize+1))
		if err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "failed to read body"})
		}
		if len(body) > maxBodySize {
			return c.JSON(http.StatusRequestEntityTooLarge, ErrorResponse{Error: "request body too large"})
		}
		c.Request().Body = io.NopCloser(bytes.NewReader(body))

		// 3. Compute HMAC-SHA256: "v0:{ts}:{body}"
		sigBasestring := fmt.Sprintf("v0:%s:%s", ts, string(body))
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(sigBasestring))
		expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

		// 4. Constant-time comparison with X-Slack-Signature
		sig := c.Request().Header.Get("X-Slack-Signature")
		if !hmac.Equal([]byte(expected), []byte(sig)) {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid slack signature"})
		}

		return next(c)
	}
}

// resolveSlackUser maps a Slack user ID to an TokayOps user.
//
// Resolution order:
//  1. Direct match: store.GetUserByExternalID (O(1) DB lookup via external_identities)
//  2. Email match:  Slack users.info → email → store.GetUserByEmail → auto-link (one-time network call)
//  3. nil → caller sends OTP fallback ephemeral
func (a *API) resolveSlackUser(ctx context.Context, slackUserID string) *model.User {
	// 1. Direct match via external_identities
	user, err := a.store.GetUserByExternalID("slack", slackUserID)
	if err == nil && user != nil {
		return user
	}

	// 2. Email match via Slack API (only if slack client is available)
	if a.slack != nil {
		linkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		email, err := a.slack.GetEmailBySlackID(linkCtx, slackUserID)
		if err == nil && email != "" {
			user, err := a.store.GetUserByEmail(email)
			if err == nil && user != nil {
				// BindExternalIdentityIfAbsent returns (false, ErrExternalIdentityAlreadyLinked)
				// if the Slack id is bound to a *different* user — we silently fall through to OTP
				// rather than overwriting.
				changed, _ := a.store.BindExternalIdentityIfAbsent(user.ID, "slack", slackUserID, "")
				if changed {
					metrics.SlackUserLinkedTotal.WithLabelValues("email_match").Inc()
					return user
				}
			}
		}
	}

	return nil
}

// HandleSlackInteractive handles Slack interactive payloads (button clicks).
// Authentication: Slack signature verification (US-4.2), NOT AuthMiddleware.
//
// Sync path (within 3 s Slack budget):
//
//	parse payload → resolve user → RBAC → atomic ack/resolve → return HTTP 200
//
// Async (goroutine): ephemeral feedback via response_url.
// Async (background loop): Slack message appearance update (already handled by ackUpdateProcessingLoop).
func (a *API) HandleSlackInteractive(c echo.Context) error {
	// 1. Parse: form value "payload" → InteractionCallback
	var callback slack.InteractionCallback
	payload := c.FormValue("payload")
	if payload == "" {
		return c.NoContent(http.StatusOK) // gracefully ignore
	}
	if err := json.Unmarshal([]byte(payload), &callback); err != nil {
		c.Logger().Warnf("slack/interactive: invalid JSON payload: %v", err)
		return c.NoContent(http.StatusOK) // gracefully ignore malformed payloads
	}

	// 2. Validate: must be block_actions with at least one action
	if callback.Type != slack.InteractionTypeBlockActions {
		return c.NoContent(http.StatusOK)
	}
	if len(callback.ActionCallback.BlockActions) == 0 {
		return c.NoContent(http.StatusOK)
	}

	// 3. Extract first action
	act := callback.ActionCallback.BlockActions[0]
	actionID := act.ActionID
	alertGroupID := act.Value
	slackUserID := callback.User.ID
	responseURL := callback.ResponseURL

	// Map action_id to RBAC action
	var rbacAction rbac.Action
	switch actionID {
	case SlackActionAckAlertGroup:
		rbacAction = rbac.ActionAlertAck
	case SlackActionResolveAlertGroup:
		rbacAction = rbac.ActionAlertResolve
	default:
		// Unknown action — ignore gracefully
		return c.NoContent(http.StatusOK)
	}

	// 4. Resolve TokayOps user from Slack user ID
	user := a.resolveSlackUser(c.Request().Context(), slackUserID)
	if user == nil {
		metrics.SlackUnlinkedUserTotal.Inc()
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "unlinked").Inc()
		go a.respondEphemeral(responseURL,
			fmt.Sprintf("Your Slack account is not linked to TokayOps. "+
				"To link: open TokayOps → Profile → Link Slack Account, "+
				"and enter your Slack User ID: `%s`", slackUserID))
		return c.NoContent(http.StatusOK)
	}

	// 5. Fetch alert group
	ag, err := a.store.GetAlertGroupByID(alertGroupID)
	if err != nil {
		if err == sql.ErrNoRows {
			go a.respondEphemeral(responseURL, "Alert group not found.")
			return c.NoContent(http.StatusOK)
		}
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "error").Inc()
		c.Logger().Errorf("slack/interactive: GetAlertGroupByID: %v", err)
		go a.respondEphemeral(responseURL, "Something went wrong. Please try again.")
		return c.NoContent(http.StatusOK)
	}

	// 6. RBAC check (best-effort team name for denied message)
	allowed, err := a.rbac.HasPermission(user.ID, rbacAction, rbac.TeamScope(ag.TeamID))
	if err != nil {
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "error").Inc()
		c.Logger().Errorf("slack/interactive: RBAC error: %v", err)
		go a.respondEphemeral(responseURL, "Something went wrong. Please try again.")
		return c.NoContent(http.StatusOK)
	}
	if !allowed {
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "unauthorized").Inc()
		verb := "acknowledge"
		if actionID == SlackActionResolveAlertGroup {
			verb = "resolve"
		}
		teamLabel := ag.TeamNameSnapshot
		if teamLabel == "" {
			teamLabel = ag.TeamID
		}
		go a.respondEphemeral(responseURL,
			fmt.Sprintf("You don't have permission to %s alerts for team %s.", verb, teamLabel))
		return c.NoContent(http.StatusOK)
	}

	// 7. Execute transition via service
	actor := alertgroup.Actor{Name: actorName(user), Email: user.Email}
	var result *alertgroup.TransitionResult

	switch actionID {
	case SlackActionAckAlertGroup:
		result, err = a.agService.Ack(alertGroupID, actor, slackMeta)
	case SlackActionResolveAlertGroup:
		result, err = a.agService.Resolve(alertGroupID, actor, slackMeta)
	}
	if err != nil {
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "error").Inc()
		c.Logger().Errorf("slack/interactive: %s: %v", slackActionLabel(actionID), err)
		go a.respondEphemeral(responseURL, "Something went wrong. Please try again.")
		return c.NoContent(http.StatusOK)
	}

	isResolve := actionID == SlackActionResolveAlertGroup
	switch result.Outcome {
	case alertgroup.OutcomeApplied:
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "success").Inc()
		if isResolve {
			go a.replaceOrEphemeral(responseURL, alertGroupID, result.AlertGroup, true,
				fmt.Sprintf("Alert group resolved by %s.", actor.Name))
		} else {
			go a.replaceOrEphemeral(responseURL, alertGroupID, result.AlertGroup, false,
				fmt.Sprintf("Alert group acknowledged by %s.", actor.Name))
		}
	case alertgroup.OutcomeAlreadyDone:
		metrics.SlackInteractionTotal.WithLabelValues(slackActionLabel(actionID), "already_done").Inc()
		if isResolve {
			go a.replaceOrEphemeral(responseURL, alertGroupID, nil, true,
				"Alert group is already resolved.")
		} else {
			go a.replaceOrEphemeral(responseURL, alertGroupID, nil, false,
				"Alert group is already acknowledged or resolved.")
		}
	}

	// 11. Return 200 empty body — background loop handles other deliveries + timeline
	return c.NoContent(http.StatusOK)
}

// actorName returns a display name for a user: Name → Email → ID.
func actorName(u *model.User) string {
	if u.Name != "" {
		return u.Name
	}
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

// slackActionLabel maps an action ID to a short metric label.
func slackActionLabel(actionID string) string {
	switch actionID {
	case SlackActionAckAlertGroup:
		return "ack"
	case SlackActionResolveAlertGroup:
		return "resolve"
	default:
		return actionID
	}
}

// postResponseURL sends an ephemeral message to Slack's response_url.
// Fire-and-forget from a goroutine; errors are logged but not surfaced.
func postResponseURL(responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]interface{}{
		"response_type":    "ephemeral",
		"replace_original": false,
		"text":             text,
	})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(responseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return // fire-and-forget
	}
	resp.Body.Close()
}

// postResponseURLReplace replaces the original Slack message via response_url.
// Sends top-level text + blocks (for unfurl preview) and the colored attachment.
// Returns an error so the caller can fall back to ephemeral feedback.
func postResponseURLReplace(responseURL string, card slackcard.Card) error {
	if responseURL == "" {
		return fmt.Errorf("empty response URL")
	}
	body, err := json.Marshal(map[string]interface{}{
		"replace_original": true,
		"text":             card.Text,
		"blocks":           card.Blocks,
		"attachments":      []slack.Attachment{card.Attachment},
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(responseURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("slack response_url returned %d", resp.StatusCode)
	}
	return nil
}

// replaceOrEphemeral tries to replace the Slack card via response_url.
// If cardRenderer is nil or replace fails, falls back to ephemeral text.
// When ag is nil (idempotent/already_done path), re-fetches from DB once
// for both card rendering and fallback text generation.
func (a *API) replaceOrEphemeral(responseURL, alertGroupID string, ag *model.AlertGroup, isResolved bool, fallbackText string) {
	if a.cardRenderer == nil || a.replaceOriginal == nil {
		if ag == nil {
			fallbackText = a.alreadyDoneFallback(alertGroupID, fallbackText)
		}
		a.respondEphemeral(responseURL, fallbackText)
		return
	}
	renderAG := ag
	if renderAG == nil {
		current, err := a.store.GetAlertGroupByID(alertGroupID)
		if err != nil {
			a.respondEphemeral(responseURL, fallbackText)
			return
		}
		renderAG = current
		fallbackText = fallbackFromAG(current)
		isResolved = renderAG.Status == model.AlertGroupStatusResolved ||
			renderAG.Status == model.AlertGroupStatusClosed
	}
	card := a.cardRenderer.RenderCard(renderAG, isResolved)
	if err := a.replaceOriginal(responseURL, card); err != nil {
		a.respondEphemeral(responseURL, fallbackText)
	}
}

// alreadyDoneFallback re-fetches the AG once and builds a specific fallback message.
// Used only when cardRenderer is nil (no card replacement attempt).
func (a *API) alreadyDoneFallback(alertGroupID, defaultText string) string {
	current, err := a.store.GetAlertGroupByID(alertGroupID)
	if err != nil {
		return defaultText
	}
	return fallbackFromAG(current)
}

// fallbackFromAG builds a human-readable ephemeral message from an already-fetched alert group.
func fallbackFromAG(ag *model.AlertGroup) string {
	switch ag.Status {
	case model.AlertGroupStatusClosed:
		return "Alert group is already closed."
	case model.AlertGroupStatusResolved:
		return "Alert group is already resolved."
	case model.AlertGroupStatusAcknowledged:
		if ag.AcknowledgedBy != "" {
			return fmt.Sprintf("Alert group was already acknowledged by %s.", ag.AcknowledgedBy)
		}
		return "Alert group is already acknowledged."
	default:
		return "Alert group is already acknowledged or resolved."
	}
}
