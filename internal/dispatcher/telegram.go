package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// ErrNoTelegramToken is returned when the Telegram bot token is not configured.
var ErrNoTelegramToken = fmt.Errorf("telegram integration not configured (no token)")

const (
	// telegramDefaultBaseURL is the public Bot API host; overridable in tests via WithBaseURL.
	telegramDefaultBaseURL = "https://api.telegram.org"
	// telegramHTTPTimeout bounds a single Bot API call. Must stay < the 60s job lease
	// so a stuck call fails the step instead of being re-leased and duplicated.
	telegramHTTPTimeout = 30 * time.Second
	// telegramMaxMessageLen is the Bot API hard limit for a message text.
	telegramMaxMessageLen = 4096
)

// TelegramTokenSource provides the send-time Telegram settings (mirror of
// SlackTokenSource): the bot token and whether Ack/Resolve buttons are enabled.
// The webhook secret token is intentionally NOT here - it is a webhook-verification
// concern read off the concrete IntegrationCache, not a send-time concern.
type TelegramTokenSource interface {
	GetTelegramToken() string
	GetTelegramInteractive() bool
}

// TelegramProvider implements dispatcher.Provider against the Telegram Bot API
// using a raw net/http client (no third-party SDK) so the timeout and the test
// base URL are fully under our control.
type TelegramProvider struct {
	tokenSource TelegramTokenSource
	selfURL     string     // TokayOps base URL for deep links
	teamLookup  TeamLookup // nil means "assume onboarded", see teamIsOnboarded
	baseURL     string     // Bot API base; default telegramDefaultBaseURL, overridable in tests
	mu          sync.Mutex
	cachedToken string
	client      *http.Client
	// getMe-derived bot username, cached and invalidated on token change.
	cachedUsername      string
	cachedUsernameToken string
}

var _ Provider = (*TelegramProvider)(nil)

// TelegramOption configures a TelegramProvider at construction.
type TelegramOption func(*TelegramProvider)

// WithBaseURL overrides the Bot API base URL. The primary use is pointing tests
// (including cross-package integration tests) at an httptest.Server.
func WithBaseURL(u string) TelegramOption {
	return func(p *TelegramProvider) {
		if u != "" {
			p.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithTeamLookup wires the check for whether an alert group's team is onboarded.
// Telegram never posts a card for an unonboarded team today (that path is
// firehose, which is Slack-only), so this exists to keep the two channels from
// drifting apart if that ever changes.
func WithTeamLookup(lookup TeamLookup) TelegramOption {
	return func(p *TelegramProvider) { p.teamLookup = lookup }
}

func NewTelegramProvider(tokenSource TelegramTokenSource, selfURL string, opts ...TelegramOption) *TelegramProvider {
	p := &TelegramProvider{
		tokenSource: tokenSource,
		selfURL:     selfURL,
		baseURL:     telegramDefaultBaseURL,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// TelegramData is the opaque provider payload stored in a delivery row: the
// coordinates needed to edit the message later. No timeline ts (Telegram v1 has
// no threads) and no stored permalink (derived on demand in Permalink).
type TelegramData struct {
	ChatID    string `json:"chat_id"`
	MessageID int    `json:"message_id"`
}

func parseTelegramData(raw string) (*TelegramData, bool) {
	if raw == "" {
		return nil, false
	}
	var data TelegramData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, false
	}
	if data.ChatID == "" || data.MessageID == 0 {
		return nil, false
	}
	return &data, true
}

// getClient returns an HTTP client and the current bot token, recreating the
// client if the token changed (so an API config edit applies after LoadAll
// without a restart). Returns ErrNoTelegramToken if no token is configured.
func (t *TelegramProvider) getClient() (*http.Client, string, error) {
	if t.tokenSource == nil {
		return nil, "", ErrNoTelegramToken
	}
	token := t.tokenSource.GetTelegramToken()
	if token == "" {
		return nil, "", ErrNoTelegramToken
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.client == nil || t.cachedToken != token {
		if t.client != nil && t.cachedToken != token {
			log.Printf("TelegramProvider: Token changed, recreating client")
		}
		t.cachedToken = token
		t.client = &http.Client{Timeout: telegramHTTPTimeout}
	}
	return t.client, token, nil
}

// tgResponse is the Bot API envelope. Result is left raw because editMessageText
// may return either a Message object or the bare value true.
type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

// callBotAPI POSTs a JSON body to a Bot API method and decodes the envelope.
// It never logs the URL (which contains the token). No retry (v1 decision) —
// transient failures bubble up to the step-level retry.
func (t *TelegramProvider) callBotAPI(ctx context.Context, client *http.Client, token, method string, body map[string]interface{}) (*tgResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: marshal body: %w", method, err)
	}
	url := t.baseURL + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("telegram %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: request failed: %w", method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram %s: read response: %w", method, err)
	}
	var tgr tgResponse
	if err := json.Unmarshal(raw, &tgr); err != nil {
		return nil, fmt.Errorf("telegram %s: decode response (status %d): %w", method, resp.StatusCode, err)
	}
	return &tgr, nil
}

// sendMessage posts a message and returns its message_id. parseMode "" sends
// plain text (no parse_mode); "HTML" enables HTML formatting. replyMarkup nil
// omits the inline keyboard.
func (t *TelegramProvider) sendMessage(ctx context.Context, client *http.Client, token, chatID, text, parseMode string, replyMarkup interface{}) (int, error) {
	body := map[string]interface{}{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if parseMode != "" {
		body["parse_mode"] = parseMode
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	tgr, err := t.callBotAPI(ctx, client, token, "sendMessage", body)
	if err != nil {
		return 0, err
	}
	if !tgr.OK {
		return 0, fmt.Errorf("telegram sendMessage failed (code %d): %s", tgr.ErrorCode, tgr.Description)
	}
	var r struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(tgr.Result, &r); err != nil {
		return 0, fmt.Errorf("telegram sendMessage: decode result: %w", err)
	}
	return r.MessageID, nil
}

// editMessageText edits an existing message in place. A 400 "message is not
// modified" is treated as success (idempotent re-edit to identical content).
// replyMarkup nil leaves the existing keyboard untouched; a non-nil value
// (including an empty inline_keyboard) replaces it.
func (t *TelegramProvider) editMessageText(ctx context.Context, client *http.Client, token, chatID string, messageID int, text string, replyMarkup interface{}) error {
	body := map[string]interface{}{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if replyMarkup != nil {
		body["reply_markup"] = replyMarkup
	}
	tgr, err := t.callBotAPI(ctx, client, token, "editMessageText", body)
	if err != nil {
		return err
	}
	if !tgr.OK {
		if tgr.ErrorCode == 400 && strings.Contains(tgr.Description, "message is not modified") {
			return nil
		}
		return fmt.Errorf("telegram editMessageText failed (code %d): %s", tgr.ErrorCode, tgr.Description)
	}
	return nil
}

// Send dispatches on the target kind: a "user" target is a fire-and-forget DM
// (returns no payload), a "channel" target posts an editable alert card and
// returns its TelegramData payload. Behaviour keys on Target.Kind — never on
// req.Kind. An unknown kind is rejected rather than silently treated as a card.
func (t *TelegramProvider) Send(ctx context.Context, req NotificationRequest) (string, error) {
	switch req.Target.Kind {
	case "user":
		if req.Message == "" {
			return "", fmt.Errorf("telegram: user send requires a message")
		}
		return "", t.sendDM(ctx, req.Target.ID, req.Message)
	case "channel":
		if req.AlertGroup == nil {
			return "", fmt.Errorf("telegram: channel send requires an alert group")
		}
		return t.sendCard(ctx, req.Target.ID, req.AlertGroup)
	default:
		return "", fmt.Errorf("telegram: unsupported target kind %q", req.Target.Kind)
	}
}

// sendDM sends a fire-and-forget plain-text DM. Not deliverable until Sprint 3
// (linking): without a linked identity the dm step path returns
// ErrIdentityNotLinked before reaching here. Returns no payload (Editable=false).
func (t *TelegramProvider) sendDM(ctx context.Context, chatID, message string) error {
	client, token, err := t.getClient()
	if err != nil {
		return err
	}
	if _, err := t.sendMessage(ctx, client, token, chatID, message, "", nil); err != nil {
		return err
	}
	return nil
}

// sendCard posts an alert-group card to a channel/group and returns the
// TelegramData payload needed to edit it later.
func (t *TelegramProvider) sendCard(ctx context.Context, chatID string, ag *model.AlertGroup) (string, error) {
	if ag == nil {
		return "", fmt.Errorf("telegram: channel send requires an alert group")
	}
	client, token, err := t.getClient()
	if err != nil {
		return "", err
	}

	messageID, err := t.sendMessage(ctx, client, token, chatID, t.renderCard(ag, false), "HTML", t.keyboardFor(ag, false))
	if err != nil {
		return "", err
	}
	// The editable card's payload must carry valid coordinates; without them the
	// delivery row would be unusable for Update/Resolve (recordDelivery rejects
	// an editable delivery with an empty payload).
	if chatID == "" || messageID == 0 {
		return "", fmt.Errorf("telegram: sendMessage returned empty chat/message id (chat=%q id=%d)", chatID, messageID)
	}

	log.Printf("TelegramProvider: Sent message to %s (message_id: %d)", chatID, messageID)

	data := TelegramData{ChatID: chatID, MessageID: messageID}
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}

func (t *TelegramProvider) Update(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) (string, error) {
	if d == nil || d.ProviderPayload == "" {
		return "", nil
	}
	data, ok := parseTelegramData(d.ProviderPayload)
	if !ok {
		return "", fmt.Errorf("telegram: invalid provider payload for delivery %s", d.ID)
	}
	client, token, err := t.getClient()
	if err != nil {
		return "", err
	}
	if err := t.editMessageText(ctx, client, token, data.ChatID, data.MessageID, t.renderCard(ag, false), t.keyboardFor(ag, false)); err != nil {
		return "", err
	}
	// Payload is unchanged (no timeline ts to track).
	return d.ProviderPayload, nil
}

func (t *TelegramProvider) Resolve(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) error {
	if d == nil || d.ProviderPayload == "" {
		return nil
	}
	data, ok := parseTelegramData(d.ProviderPayload)
	if !ok {
		return fmt.Errorf("telegram: invalid provider payload for delivery %s", d.ID)
	}
	client, token, err := t.getClient()
	if err != nil {
		return err
	}
	return t.editMessageText(ctx, client, token, data.ChatID, data.MessageID, t.renderCard(ag, true), t.keyboardFor(ag, true))
}

// Permalink returns a t.me link only for public targets (a chat addressed by
// @username). Private chats/groups have no public permalink → "".
func (t *TelegramProvider) Permalink(d *model.NotificationDelivery) string {
	if d == nil {
		return ""
	}
	data, ok := parseTelegramData(d.ProviderPayload)
	if !ok {
		return ""
	}
	if strings.HasPrefix(data.ChatID, "@") {
		return fmt.Sprintf("https://t.me/%s/%d", strings.TrimPrefix(data.ChatID, "@"), data.MessageID)
	}
	return ""
}

// keyboardFor returns the inline keyboard for a card.
//
// Without a public selfURL the webhook can't be registered, so the Ack/Resolve
// buttons would be dead - we omit the keyboard entirely (the card still sends as
// a plain notification, consistent with the footer degrading to plain text).
// Returning nil here is deliberate: no keyboard was ever sent, so there is
// nothing to take back.
//
// When interactivity is switched off the card HAS to carry an empty keyboard
// rather than nil, because editMessageText leaves the existing keyboard in place
// when reply_markup is absent. Sending nil would strand live buttons on cards
// posted before the switch was flipped.
func (t *TelegramProvider) keyboardFor(ag *model.AlertGroup, isResolved bool) interface{} {
	if t.selfURL == "" {
		return nil
	}
	if t.tokenSource == nil || !t.tokenSource.GetTelegramInteractive() {
		return emptyInlineKeyboard()
	}
	// A resolved card carries no buttons either way, so return early rather
	// than pay for a team lookup that cannot change the answer.
	if isResolved {
		return emptyInlineKeyboard()
	}
	if !teamIsOnboarded(t.teamLookup, ag.TeamID) {
		return emptyInlineKeyboard()
	}
	return ackResolveKeyboard(ag, isResolved)
}

// emptyInlineKeyboard is a keyboard with no buttons, i.e. the payload that
// removes buttons from an already-sent card.
func emptyInlineKeyboard() map[string]interface{} {
	return map[string]interface{}{"inline_keyboard": [][]map[string]string{}}
}

// ackResolveKeyboard builds the inline keyboard for an alert-group card. A
// resolved card gets an empty keyboard (buttons removed); an active card gets
// Resolve, plus Acknowledge when not yet acknowledged. callback_data is
// "<prefix><agID>" — well within Telegram's 64-byte limit.
func ackResolveKeyboard(ag *model.AlertGroup, isResolved bool) map[string]interface{} {
	if isResolved {
		return emptyInlineKeyboard()
	}
	var row []map[string]string
	if ag.Status != model.AlertGroupStatusAcknowledged {
		row = append(row, map[string]string{"text": "✅ Acknowledge", "callback_data": model.TelegramCallbackAckPrefix + ag.ID})
	}
	row = append(row, map[string]string{"text": "🟢 Resolve", "callback_data": model.TelegramCallbackResolvePrefix + ag.ID})
	return map[string]interface{}{"inline_keyboard": [][]map[string]string{row}}
}

// AnswerCallback acknowledges a callback_query so the tapped button stops
// spinning; text (optional) shows as a toast. Uses the current cached token.
func (t *TelegramProvider) AnswerCallback(ctx context.Context, callbackQueryID, text string) error {
	client, token, err := t.getClient()
	if err != nil {
		return err
	}
	body := map[string]interface{}{"callback_query_id": callbackQueryID}
	if text != "" {
		body["text"] = text
	}
	tgr, err := t.callBotAPI(ctx, client, token, "answerCallbackQuery", body)
	if err != nil {
		return err
	}
	if !tgr.OK {
		return fmt.Errorf("telegram answerCallbackQuery failed (code %d): %s", tgr.ErrorCode, tgr.Description)
	}
	return nil
}

// SendText sends a plain-text message (used for the /start link confirmation).
func (t *TelegramProvider) SendText(ctx context.Context, chatID, text string) error {
	client, token, err := t.getClient()
	if err != nil {
		return err
	}
	_, err = t.sendMessage(ctx, client, token, chatID, text, "", nil)
	return err
}

// BotUsername returns the bot's @username (via getMe), cached per token and
// invalidated on token change. Used to build the t.me/<bot>?start=<token> link.
func (t *TelegramProvider) BotUsername(ctx context.Context) (string, error) {
	client, token, err := t.getClient()
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	if t.cachedUsername != "" && t.cachedUsernameToken == token {
		u := t.cachedUsername
		t.mu.Unlock()
		return u, nil
	}
	t.mu.Unlock()

	tgr, err := t.callBotAPI(ctx, client, token, "getMe", map[string]interface{}{})
	if err != nil {
		return "", err
	}
	if !tgr.OK {
		return "", fmt.Errorf("telegram getMe failed (code %d): %s", tgr.ErrorCode, tgr.Description)
	}
	var r struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(tgr.Result, &r); err != nil {
		return "", fmt.Errorf("telegram getMe: decode result: %w", err)
	}
	if r.Username == "" {
		return "", fmt.Errorf("telegram getMe: empty username")
	}
	t.mu.Lock()
	t.cachedUsername, t.cachedUsernameToken = r.Username, token
	t.mu.Unlock()
	return r.Username, nil
}

// SetWebhook registers the webhook for the bot identified by an EXPLICIT token
// (not the cached one), so the integration lifecycle can target the old vs new
// bot precisely across enable/disable/token-rotation.
func (t *TelegramProvider) SetWebhook(ctx context.Context, token, webhookURL, secretToken string) error {
	if token == "" {
		return ErrNoTelegramToken
	}
	body := map[string]interface{}{
		"url":             webhookURL,
		"allowed_updates": []string{"message", "callback_query"},
	}
	if secretToken != "" {
		body["secret_token"] = secretToken
	}
	tgr, err := t.callBotAPI(ctx, &http.Client{Timeout: telegramHTTPTimeout}, token, "setWebhook", body)
	if err != nil {
		return err
	}
	if !tgr.OK {
		return fmt.Errorf("telegram setWebhook failed (code %d): %s", tgr.ErrorCode, tgr.Description)
	}
	return nil
}

// DeleteWebhook removes the webhook for the bot identified by an EXPLICIT token.
func (t *TelegramProvider) DeleteWebhook(ctx context.Context, token string) error {
	if token == "" {
		return ErrNoTelegramToken
	}
	tgr, err := t.callBotAPI(ctx, &http.Client{Timeout: telegramHTTPTimeout}, token, "deleteWebhook", map[string]interface{}{})
	if err != nil {
		return err
	}
	if !tgr.OK {
		return fmt.Errorf("telegram deleteWebhook failed (code %d): %s", tgr.ErrorCode, tgr.Description)
	}
	return nil
}

// renderCard builds the HTML message body. Dynamic values are escaped with
// html.EscapeString (covers & < > ' " — safe for both text and href attributes).
// Telegram has no threads, so the card is a single self-contained message.
func (t *TelegramProvider) renderCard(ag *model.AlertGroup, isResolved bool) string {
	return assembleTelegramCard(t.cardBodyLines(ag, isResolved), t.cardFooter(ag), telegramMaxMessageLen)
}

// cardBodyLines returns the body as a slice of complete, individually-valid HTML
// lines (balanced tags within each line). Truncation drops whole lines, so it can
// never sever a tag or entity.
func (t *TelegramProvider) cardBodyLines(ag *model.AlertGroup, isResolved bool) []string {
	status := resolveStatus(ag, isResolved, countFiring(ag.Alerts))

	titleText := html.EscapeString(status.title)
	if ag.ExternalURL != "" {
		titleText = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(ag.ExternalURL), html.EscapeString(status.title))
	}
	lines := []string{"<b>" + titleText + "</b>"}

	if status.actor != "" {
		lines = append(lines, html.EscapeString(status.actor))
	}
	lines = append(lines, "Severity: "+html.EscapeString(ag.Severity))
	lines = append(lines, "Alerts:")
	lines = append(lines, telegramAlertLines(ag)...)
	return lines
}

// telegramAlertLines renders up to 10 alert bullets (HTML-escaped).
func telegramAlertLines(ag *model.AlertGroup) []string {
	const maxAlerts = 10
	if len(ag.Alerts) == 0 {
		return []string{"• " + html.EscapeString(ag.Title)}
	}
	var out []string
	rendered := 0
	for _, a := range ag.Alerts {
		if rendered >= maxAlerts {
			break
		}
		rendered++
		name := html.EscapeString(a.Labels["alertname"])
		if a.Status == model.AlertStatusFiring {
			out = append(out, fmt.Sprintf("• 🔴 %s (Sev: %s)", name, html.EscapeString(a.Labels["severity"])))
		} else {
			out = append(out, fmt.Sprintf("• 🟢 %s (Resolved)", name))
		}
	}
	if remaining := len(ag.Alerts) - maxAlerts; remaining > 0 {
		out = append(out, fmt.Sprintf("… and %d more alerts", remaining))
	}
	return out
}

// cardFooter returns the "Open in Tokay" deep-link footer as one complete HTML line.
func (t *TelegramProvider) cardFooter(ag *model.AlertGroup) string {
	if t.selfURL != "" {
		url := fmt.Sprintf("%s/#/ops/alert-groups/%s", t.selfURL, ag.ID)
		return fmt.Sprintf(`ID: <a href="%s">%s</a>`, html.EscapeString(url), html.EscapeString(ag.ID))
	}
	return "ID: " + html.EscapeString(ag.ID)
}

// assembleTelegramCard joins body lines + footer within the length limit WITHOUT
// cutting any line. Whole pre-built lines are dropped from the end if needed (each
// is valid HTML on its own), and the footer is always appended intact — so the
// result never contains a dangling tag or partial entity.
func assembleTelegramCard(lines []string, footer string, limit int) string {
	const sep = "\n"
	const trunc = "… (truncated)"
	reserve := len(sep) + len(footer)

	var b strings.Builder
	truncated := false
	for _, ln := range lines {
		cost := len(ln)
		if b.Len() > 0 {
			cost += len(sep)
		}
		if b.Len()+cost+reserve > limit {
			truncated = true
			break
		}
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		b.WriteString(ln)
	}
	if truncated && b.Len()+len(sep)+len(trunc)+reserve <= limit {
		b.WriteString(sep)
		b.WriteString(trunc)
	}
	if b.Len() > 0 {
		b.WriteString(sep)
	}
	b.WriteString(footer)
	return b.String()
}
