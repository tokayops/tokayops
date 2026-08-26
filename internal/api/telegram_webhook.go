package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rbac"
	"github.com/tokayops/tokayops/internal/store"
)

// telegramMeta is the metadata attached to timeline events originating from Telegram.
var telegramMeta = map[string]string{"source": "telegram"}

// telegramUpdate is the slice of the Bot API update payload we consume: a /start
// message (linking) or a callback_query (Ack/Resolve button). Telegram numeric
// ids are int64; we store them as decimal strings in external_identities.
type telegramUpdate struct {
	Message *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		From *struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		From struct {
			ID        int64  `json:"id"`
			FirstName string `json:"first_name"`
			Username  string `json:"username"`
		} `json:"from"`
		Data string `json:"data"`
	} `json:"callback_query"`
}

// TelegramSecretMiddleware verifies the X-Telegram-Bot-Api-Secret-Token header
// (set when we call setWebhook) against the configured secret via a constant-time
// compare. Unlike Slack this is NOT an HMAC — Telegram echoes the secret verbatim.
// Returns 503 when no secret is configured (mirrors SlackSignatureMiddleware).
func (a *API) TelegramSecretMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		var secret string
		if a.integrationCache != nil {
			secret = a.integrationCache.GetTelegramSecretToken()
		}
		if secret == "" {
			c.Logger().Error("telegram/webhook: secret token not configured")
			return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "telegram secret token not configured"})
		}
		got := c.Request().Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "invalid telegram secret token"})
		}

		// Cap + restore the body for the handler (Telegram updates are small).
		const maxBodySize = 1 << 20
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodySize+1))
		if err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "failed to read body"})
		}
		if len(body) > maxBodySize {
			return c.JSON(http.StatusRequestEntityTooLarge, ErrorResponse{Error: "request body too large"})
		}
		c.Request().Body = io.NopCloser(bytes.NewReader(body))
		return next(c)
	}
}

// HandleTelegramWebhook processes Bot API updates: `/start <token>` links the
// sender's Telegram account, and a callback_query acks/resolves an alert group.
// It always returns 200 (Telegram retries non-200) and answers the callback so
// the button stops spinning. The card itself is not touched here: it belongs to
// the delivery domain, which brings it to the acknowledged revision.
func (a *API) HandleTelegramWebhook(c echo.Context) error {
	var upd telegramUpdate
	if err := json.NewDecoder(c.Request().Body).Decode(&upd); err != nil {
		return c.NoContent(http.StatusOK) // ignore malformed updates
	}
	ctx := c.Request().Context()

	switch {
	case upd.Message != nil && strings.HasPrefix(upd.Message.Text, "/start"):
		a.handleTelegramStart(ctx, &upd)
	case upd.CallbackQuery != nil:
		a.handleTelegramCallback(ctx, &upd)
	}
	return c.NoContent(http.StatusOK)
}

// handleTelegramStart consumes `/start <token>` and binds the sender's identity.
func (a *API) handleTelegramStart(ctx context.Context, upd *telegramUpdate) {
	if upd.Message.From == nil {
		return
	}
	chatID := strconv.FormatInt(upd.Message.Chat.ID, 10)
	parts := strings.Fields(upd.Message.Text)
	if len(parts) < 2 {
		a.sendTelegram(ctx, chatID, "Open Tokay → Profile → Connect Telegram to get your personal link.")
		return
	}
	token := parts[1]
	from := upd.Message.From
	externalID := strconv.FormatInt(from.ID, 10)
	displayName := from.FirstName
	if from.Username != "" {
		displayName = "@" + from.Username
	}

	_, err := a.store.ConsumeLinkToken("telegram", token, externalID, chatID, displayName)
	switch {
	case err == nil:
		metrics.TelegramUserLinkedTotal.Inc()
		a.sendTelegram(ctx, chatID, "✅ Your Telegram account is now linked to Tokay.")
	case errors.Is(err, store.ErrLinkTokenInvalid), errors.Is(err, store.ErrLinkTokenExpired):
		a.sendTelegram(ctx, chatID, "This link is invalid or expired. Generate a new one from your Tokay profile.")
	case errors.Is(err, store.ErrExternalIdentityAlreadyLinked):
		a.sendTelegram(ctx, chatID, "This Telegram account is already linked to another Tokay user.")
	default:
		log.Printf("telegram/webhook: ConsumeLinkToken: %v", err)
		a.sendTelegram(ctx, chatID, "Something went wrong linking your account. Please try again.")
	}
}

// handleTelegramCallback handles an Ack/Resolve button — mirror of HandleSlackInteractive.
func (a *API) handleTelegramCallback(ctx context.Context, upd *telegramUpdate) {
	cb := upd.CallbackQuery

	var action rbac.Action
	var label, verb, agID string
	switch {
	case strings.HasPrefix(cb.Data, model.TelegramCallbackAckPrefix):
		action, label, verb = rbac.ActionAlertAck, "ack", "acknowledge"
		agID = strings.TrimPrefix(cb.Data, model.TelegramCallbackAckPrefix)
	case strings.HasPrefix(cb.Data, model.TelegramCallbackResolvePrefix):
		action, label, verb = rbac.ActionAlertResolve, "resolve", "resolve"
		agID = strings.TrimPrefix(cb.Data, model.TelegramCallbackResolvePrefix)
	default:
		a.answerTelegram(ctx, cb.ID, "")
		return
	}

	externalID := strconv.FormatInt(cb.From.ID, 10)
	user, err := a.store.GetUserByExternalID("telegram", externalID)
	if err != nil || user == nil {
		metrics.TelegramUnlinkedUserTotal.Inc()
		metrics.TelegramInteractionTotal.WithLabelValues(label, "unlinked").Inc()
		a.answerTelegram(ctx, cb.ID, "Your Telegram account isn't linked to Tokay. Open Tokay → Profile → Connect Telegram.")
		return
	}

	ag, err := a.store.GetAlertGroupByID(agID)
	if err != nil || ag == nil {
		metrics.TelegramInteractionTotal.WithLabelValues(label, "error").Inc()
		a.answerTelegram(ctx, cb.ID, "Alert group not found.")
		return
	}

	allowed, err := a.rbac.HasPermission(user.ID, action, rbac.TeamScope(ag.TeamID))
	if err != nil {
		metrics.TelegramInteractionTotal.WithLabelValues(label, "error").Inc()
		a.answerTelegram(ctx, cb.ID, "Something went wrong. Please try again.")
		return
	}
	if !allowed {
		metrics.TelegramInteractionTotal.WithLabelValues(label, "unauthorized").Inc()
		a.answerTelegram(ctx, cb.ID, fmt.Sprintf("You don't have permission to %s alerts for this team.", verb))
		return
	}

	actor := alertgroup.Actor{Name: actorName(user), Email: user.Email}
	var result *alertgroup.TransitionResult
	if action == rbac.ActionAlertAck {
		result, err = a.agService.Ack(agID, actor, telegramMeta)
	} else {
		result, err = a.agService.Resolve(agID, actor, telegramMeta)
	}
	if err != nil {
		metrics.TelegramInteractionTotal.WithLabelValues(label, "error").Inc()
		log.Printf("telegram/webhook: %s %s: %v", label, agID, err)
		a.answerTelegram(ctx, cb.ID, "Something went wrong. Please try again.")
		return
	}

	switch result.Outcome {
	case alertgroup.OutcomeApplied:
		metrics.TelegramInteractionTotal.WithLabelValues(label, "success").Inc()
		a.answerTelegram(ctx, cb.ID, fmt.Sprintf("Alert group %sd by %s.", verb, actor.Name))
	case alertgroup.OutcomeAlreadyDone:
		metrics.TelegramInteractionTotal.WithLabelValues(label, "already_done").Inc()
		a.answerTelegram(ctx, cb.ID, "Already acknowledged or resolved.")
	default:
		metrics.TelegramInteractionTotal.WithLabelValues(label, "error").Inc()
		a.answerTelegram(ctx, cb.ID, "Alert group not found.")
	}
}

// answerTelegram answers a callback query (best-effort; the toast is cosmetic).
func (a *API) answerTelegram(ctx context.Context, callbackQueryID, text string) {
	if a.telegram == nil {
		return
	}
	if err := a.telegram.AnswerCallback(ctx, callbackQueryID, text); err != nil {
		log.Printf("telegram/webhook: answerCallbackQuery: %v", err)
	}
}

// sendTelegram sends a plain-text message (best-effort; /start confirmations).
func (a *API) sendTelegram(ctx context.Context, chatID, text string) {
	if a.telegram == nil {
		return
	}
	if err := a.telegram.SendText(ctx, chatID, text); err != nil {
		log.Printf("telegram/webhook: SendText: %v", err)
	}
}
