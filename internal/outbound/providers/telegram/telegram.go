package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// ErrNoToken is returned when the Telegram bot token is not configured.
var ErrNoToken = fmt.Errorf("telegram integration not configured (no token)")

// ErrNoReceipt is a change to a message whose stored coordinates cannot be
// read. The store refuses to ask for one without them, so this is a damaged
// row rather than a message that went missing.
var ErrNoReceipt = fmt.Errorf("telegram: the message to change has no readable coordinates")

const (
	// telegramDefaultBaseURL is the public Bot API host; overridable in tests via WithBaseURL.
	telegramDefaultBaseURL = "https://api.telegram.org"
	// telegramHTTPTimeout bounds a single Bot API call, so a stuck call fails
	// rather than holding its goroutine for good.
	//
	// It does NOT bound the step: an executor may make several calls, and their
	// timeouts add up past the 60s job lease - which an earlier version of this
	// comment claimed it prevented. Whether a step can outlive its lease is a
	// question for the worker, not for this constant.
	telegramHTTPTimeout = 30 * time.Second
	// telegramMaxMessageLen is the Bot API hard limit for a message text.
	telegramMaxMessageLen = 4096
)

// TokenSource provides the send-time Telegram settings (mirror of
// SlackTokenSource): the bot token and whether Ack/Resolve buttons are enabled.
// The webhook secret token is intentionally NOT here - it is a webhook-verification
// concern read off the concrete IntegrationCache, not a send-time concern.
type TokenSource interface {
	GetTelegramToken() string
	GetTelegramInteractive() bool
}

// Provider implements dispatcher.Provider against the Telegram Bot API
// using a raw net/http client (no third-party SDK) so the timeout and the test
// base URL are fully under our control.
type Provider struct {
	tokenSource TokenSource
	selfURL     string               // TokayOps base URL for deep links
	teamLookup  providers.TeamLookup // nil means "assume onboarded", see teamIsOnboarded
	baseURL     string               // Bot API base; default telegramDefaultBaseURL, overridable in tests
	mu          sync.Mutex
	cachedToken string
	client      *http.Client
	// getMe-derived bot username, cached and invalidated on token change.
	cachedUsername      string
	cachedUsernameToken string
}

// Option configures a Provider at construction.
type Option func(*Provider)

// WithBaseURL overrides the Bot API base URL. The primary use is pointing tests
// (including cross-package integration tests) at an httptest.Server.
func WithBaseURL(u string) Option {
	return func(p *Provider) {
		if u != "" {
			p.baseURL = strings.TrimRight(u, "/")
		}
	}
}

// WithTeamLookup wires the check for whether an alert group's team is onboarded.
// Telegram never posts a card for an unonboarded team today (that path is
// firehose, which is Slack-only), so this exists to keep the two channels from
// drifting apart if that ever changes.
func WithTeamLookup(lookup providers.TeamLookup) Option {
	return func(p *Provider) { p.teamLookup = lookup }
}

func NewProvider(tokenSource TokenSource, selfURL string, opts ...Option) *Provider {
	p := &Provider{
		tokenSource: tokenSource,
		selfURL:     selfURL,
		baseURL:     telegramDefaultBaseURL,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Data is what a commitment records about the message it made: the coordinates
// needed to edit it later. Nothing else - Telegram has no thread, and a
// permalink exists only for a public chat and has no reader in this build.
type Data struct {
	ChatID    string `json:"chat_id"`
	MessageID int    `json:"message_id"`
}

func parseData(raw string) (*Data, bool) {
	if raw == "" {
		return nil, false
	}
	var data Data
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
// without a restart). Returns ErrNoToken if no token is configured.
func (t *Provider) getClient() (*http.Client, string, error) {
	if t.tokenSource == nil {
		return nil, "", ErrNoToken
	}
	token := t.tokenSource.GetTelegramToken()
	if token == "" {
		return nil, "", ErrNoToken
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.client == nil || t.cachedToken != token {
		if t.client != nil && t.cachedToken != token {
			log.Printf("Provider: Token changed, recreating client")
		}
		t.cachedToken = token
		t.client = effectClient()
	}
	return t.client, token, nil
}

// effectClient is the client every call that can create a message goes through.
//
// It does not follow redirects, and that is about the journal rather than about
// Telegram. A provider that accepted the POST and answered 3xx would send the
// client somewhere else; if that hop fails to resolve or handshake, the error
// looks exactly like a request that never left - and the retry it earns sends a
// second message. Stopping at the first response keeps the proof honest.
func effectClient() *http.Client {
	return &http.Client{
		Timeout: telegramHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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
func (t *Provider) callBotAPI(ctx context.Context, client *http.Client, token, method string, body map[string]interface{}) (*tgResponse, error) {
	return callBotAPI(ctx, client, t.baseURL, token, method, body)
}

// callBotAPI is the transport itself, shared by the provider and the handler so
// there is one place that knows how a Bot API call is made and how its envelope
// is read.
func callBotAPI(ctx context.Context, client *http.Client, baseURL, token, method string, body map[string]interface{}) (*tgResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: marshal body: %w", method, err)
	}
	url := baseURL + "/bot" + token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("telegram %s: build request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram %s: request failed: %w", method, withoutURL(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("telegram %s: read response: %w", method, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// An answer, and one nobody here recognises: the request reached
		// Telegram and what it did with it is unknown.
		return &tgResponse{
			ErrorCode:   resp.StatusCode,
			Description: "redirected, which this build does not follow",
		}, nil
	}

	var tgr tgResponse
	if err := json.Unmarshal(raw, &tgr); err != nil {
		return nil, fmt.Errorf("telegram %s: decode response (status %d): %w", method, resp.StatusCode, err)
	}
	return &tgr, nil
}

// withoutURL strips the address from a transport error and keeps everything
// else about it.
//
// The bot token is IN that address - the Bot API puts it in the path - and
// net/http puts the address into every error it returns. Those errors end up in
// a delivery's summary, which is a durable row that people read: without this,
// one unreachable Telegram host writes the installation's bot token into the
// journal, where it stays.
//
// The cause is kept and still unwraps, because what the cause IS - a refused
// connection, a failed handshake, a timeout - is what decides whether the
// message might have gone out.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// sendMessage posts a message and returns its message_id. parseMode "" sends
// plain text (no parse_mode); "HTML" enables HTML formatting. replyMarkup nil
// omits the inline keyboard.
func (t *Provider) sendMessage(ctx context.Context, client *http.Client, token, chatID, text, parseMode string, replyMarkup interface{}) (int, error) {
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

// Send is the job engine's one remaining call into this channel: a
// fire-and-forget direct message. The same rule as Slack's, for the same
// reason - a card posted from here would be one nothing can update, because
// nothing here records where it landed.
func (t *Provider) Send(ctx context.Context, req providers.NotificationRequest) (string, error) {
	if req.Target.Kind != "user" {
		return "", fmt.Errorf("telegram: %q is not sent from here any more", req.Target.Kind)
	}
	if req.Message == "" {
		return "", fmt.Errorf("telegram: user send requires a message")
	}
	return "", t.sendDM(ctx, req.Target.ID, req.Message)
}

// sendDM sends a fire-and-forget plain-text DM. Not deliverable until Sprint 3
// (linking): without a linked identity the dm step path returns
// ErrIdentityNotLinked before reaching here. Returns no payload (Editable=false).
func (t *Provider) sendDM(ctx context.Context, chatID, message string) error {
	client, token, err := t.getClient()
	if err != nil {
		return err
	}
	if _, err := t.sendMessage(ctx, client, token, chatID, message, "", nil); err != nil {
		return err
	}
	return nil
}

// KeyboardFor decides the keyboard from the snapshot and from whether this
// delivery was admitted with buttons. Both are frozen: buttons that appear or
// vanish between two attempts of one delivery are two different messages under
// one key.
func KeyboardFor(state keys.SnapshotInput, interactive bool) interface{} {
	if state.GroupURL == nil || *state.GroupURL == "" {
		return nil
	}
	if !interactive {
		return emptyInlineKeyboard()
	}
	// A resolved card carries no buttons, and neither does one whose team
	// TokayOps does not have: the button would answer nobody.
	if state.Status == keys.GroupResolved || state.Status == keys.GroupClosed {
		return emptyInlineKeyboard()
	}
	if !state.TeamOnboarded {
		return emptyInlineKeyboard()
	}
	return ackResolveKeyboard(state)
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
func ackResolveKeyboard(state keys.SnapshotInput) map[string]interface{} {
	var row []map[string]string
	if state.Status != keys.GroupAcknowledged {
		row = append(row, map[string]string{
			"text":          "✅ Acknowledge",
			"callback_data": model.TelegramCallbackAckPrefix + state.AlertGroupID,
		})
	}
	row = append(row, map[string]string{
		"text":          "🟢 Resolve",
		"callback_data": model.TelegramCallbackResolvePrefix + state.AlertGroupID,
	})
	return map[string]interface{}{"inline_keyboard": [][]map[string]string{row}}
}

// AnswerCallback acknowledges a callback_query so the tapped button stops
// spinning; text (optional) shows as a toast. Uses the current cached token.
func (t *Provider) AnswerCallback(ctx context.Context, callbackQueryID, text string) error {
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
func (t *Provider) SendText(ctx context.Context, chatID, text string) error {
	client, token, err := t.getClient()
	if err != nil {
		return err
	}
	_, err = t.sendMessage(ctx, client, token, chatID, text, "", nil)
	return err
}

// BotUsername returns the bot's @username (via getMe), cached per token and
// invalidated on token change. Used to build the t.me/<bot>?start=<token> link.
func (t *Provider) BotUsername(ctx context.Context) (string, error) {
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
func (t *Provider) SetWebhook(ctx context.Context, token, webhookURL, secretToken string) error {
	if token == "" {
		return ErrNoToken
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
func (t *Provider) DeleteWebhook(ctx context.Context, token string) error {
	if token == "" {
		return ErrNoToken
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

// RenderCard draws the card from a snapshot and from nothing else. Telegram has
// no threads, so it is a single self-contained message.
func RenderCard(state keys.SnapshotInput) string {
	return assembleCard(cardBodyLines(state), cardFooter(state), telegramMaxMessageLen)
}

// freeze takes the snapshot for the path that still starts from a live row,
// reading the configuration, the team lookup and the process zone once - here -
// rather than halfway through drawing a card.
func (t *Provider) freeze(ag *model.AlertGroup, isResolved bool) keys.SnapshotInput {
	onboarded := true
	if t.interactive() && !isResolved && ag != nil {
		onboarded = providers.TeamIsOnboarded(t.teamLookup, ag.TeamID)
	}
	return providers.RenderableOf(providers.GroupView{
		Group:         ag,
		IsResolved:    isResolved,
		SelfURL:       t.selfURL,
		TeamOnboarded: onboarded,
		Zone:          providers.ProcessZone(),
	})
}

func (t *Provider) interactive() bool {
	return t.tokenSource != nil && t.tokenSource.GetTelegramInteractive()
}

// cardBodyLines returns the body as a slice of complete, individually-valid HTML
// lines (balanced tags within each line). Truncation drops whole lines, so it can
// never sever a tag or entity.
func cardBodyLines(state keys.SnapshotInput) []string {
	status := providers.ResolveStatus(state)

	titleText := html.EscapeString(status.Title)
	if state.ExternalURL != nil && *state.ExternalURL != "" {
		titleText = fmt.Sprintf(`<a href="%s">%s</a>`,
			html.EscapeString(*state.ExternalURL), html.EscapeString(status.Title))
	}
	lines := []string{"<b>" + titleText + "</b>"}

	if status.Actor != "" {
		lines = append(lines, html.EscapeString(status.Actor))
	}
	lines = append(lines, "Severity: "+html.EscapeString(state.Severity))
	lines = append(lines, "Alerts:")
	lines = append(lines, alertLines(state)...)
	return lines
}

// alertLines renders up to 10 alert bullets (HTML-escaped).
func alertLines(state keys.SnapshotInput) []string {
	const maxAlerts = 10
	if len(state.Alerts) == 0 {
		return []string{"• " + html.EscapeString(state.Title)}
	}
	var out []string
	rendered := 0
	for _, a := range state.Alerts {
		if rendered >= maxAlerts {
			break
		}
		rendered++
		name := html.EscapeString(a.AlertName)
		if a.Status == keys.AlertFiring {
			out = append(out, fmt.Sprintf("• 🔴 %s (Sev: %s)", name, html.EscapeString(a.Severity)))
		} else {
			out = append(out, fmt.Sprintf("• 🟢 %s (Resolved)", name))
		}
		// What is wrong and since when, on a line of its own. Slack says the
		// same thing the same way: a card that told the two channels different
		// stories about one alert would be worse than either.
		out = append(out, "   <i>"+html.EscapeString(alertDetail(a, state.DisplayTimezone))+"</i>")
	}
	if remaining := len(state.Alerts) - maxAlerts; remaining > 0 {
		out = append(out, fmt.Sprintf("… and %d more alerts", remaining))
	}
	return out
}

// alertDetail is the second line of an alert: its description when it has one,
// and the moment it started, which it always has.
func alertDetail(a keys.AlertSnapshot, zone string) string {
	since := "since " + providers.AlertStartedAt(a, zone)
	if description := providers.AlertDescription(a); description != "" {
		return description + " · " + since
	}
	return since
}

// cardFooter returns the "Open in Tokay" deep-link footer as one complete HTML line.
func cardFooter(state keys.SnapshotInput) string {
	if state.GroupURL != nil && *state.GroupURL != "" {
		return fmt.Sprintf(`ID: <a href="%s">%s</a>`,
			html.EscapeString(*state.GroupURL), html.EscapeString(state.AlertGroupID))
	}
	return "ID: " + html.EscapeString(state.AlertGroupID)
}

// assembleCard joins body lines + footer within the length limit WITHOUT
// cutting any line. Whole pre-built lines are dropped from the end if needed (each
// is valid HTML on its own), and the footer is always appended intact — so the
// result never contains a dangling tag or partial entity.
func assembleCard(lines []string, footer string, limit int) string {
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
