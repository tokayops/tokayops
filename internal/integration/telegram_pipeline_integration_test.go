//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	telegramprovider "github.com/tokayops/tokayops/internal/outbound/providers/telegram"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher"
	"github.com/tokayops/tokayops/internal/engine"
	"github.com/tokayops/tokayops/internal/ingester"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

const tgWebhookSecret = "e2e-telegram-secret"

// fakeBot is an httptest stand-in for the Telegram Bot API. It records calls per
// method and the last sendMessage body so the pipeline test can assert what the
// provider actually sent (card chat + inline keyboard).
type fakeBot struct {
	mu           sync.Mutex
	calls        map[string]int
	lastSendBody map[string]interface{}
}

func newFakeBot() *fakeBot { return &fakeBot{calls: map[string]int{}} }

func (f *fakeBot) start(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		f.calls[method]++
		if method == "sendMessage" {
			var b map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.lastSendBody = b
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage":
			// The whole Message, as the Bot API answers it: the chat is half of
			// what says WHERE the message is, and a delivery that cannot say
			// that is not an acceptance anybody can act on later. The id comes
			// back as a NUMBER even when the request named a @username, which
			// is what a receipt has to be able to read.
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true,
				"result": map[string]interface{}{
					"message_id": 4242,
					"chat":       map[string]interface{}{"id": -1001234567890},
				},
			})
		case "getMe":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"username": "e2e_bot"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": true})
		}
	}))
}

func (f *fakeBot) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeBot) sendBody() map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastSendBody
}

type tgPipelineEnv struct {
	S      *store.Store
	Eng    *engine.Engine
	Disp   *dispatcher.Dispatcher
	Echo   *echo.Echo
	Bot    *fakeBot
	Worker *outbound.Worker
}

// setupTelegramPipeline wires the real pipeline (ingester → engine → dispatcher)
// with a REAL TelegramProvider pointed at a fake Bot API, plus the API layer so
// the inbound /telegram/webhook is reachable on the same echo. Mirrors
// pipeline_integration_test.go's harness, swapping MockProvider for telegram.
func setupTelegramPipeline(t *testing.T) *tgPipelineEnv {
	// Integration config is encrypted at rest — provide a deterministic test key.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv(config.EncryptionKeyEnv, hex.EncodeToString(key))

	s := testutil.SetupDB(t)
	bot := newFakeBot()
	server := bot.start(t)
	t.Cleanup(server.Close)

	// Telegram integration row → feeds the provider token (GetTelegramToken) and
	// the webhook secret (GetTelegramSecretToken) through the cache.
	tgCfg, _ := json.Marshal(model.TelegramConfig{BotToken: "123:e2e", SecretToken: tgWebhookSecret})
	if err := s.CreateIntegration(&model.Integration{
		Type: model.IntegrationTypeTelegram, Name: "tg-e2e", Enabled: true, Config: tgCfg,
	}); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}
	cache := store.NewIntegrationCache()
	if err := cache.LoadAll(s); err != nil {
		t.Fatalf("cache.LoadAll: %v", err)
	}

	// Policy: a single telegram CHANNEL step; team routes critical → it.
	if err := s.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: "tg_policy", Name: "TG Policy",
		Steps: []*model.EscalationStep{{
			ID: "tg_step_1", PolicyID: "tg_policy", StepIndex: 0,
			Provider: "telegram", TargetKind: "channel", TargetType: "channel", TargetID: "@e2echan",
			TimeoutSeconds: 10, MaxAttempts: 1,
		}},
	}); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}
	if err := s.CreateTeam(&model.Team{
		ID: "tgteam", Name: "TG Team",
		SeverityRoutes: map[string]string{"critical": "tg_policy"},
	}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	// Empty firehose config → no firehose step prepended (firehose is Slack-only).
	// The self URL is not empty though: the buttons need somewhere to send
	// people back to, and a plan freezes that link when it admits the
	// escalation rather than building it at send time.
	cfg := &config.Config{
		ConfigVersion: 3,
		Global:        config.GlobalConfig{SelfURL: "https://tokay.e2e"},
	}

	ing := ingester.NewIngester(s, cfg, &testSecretValidator{})
	eng := engine.NewEngine(s, schedulerender.New(s.ScheduleReadRepository()), &testSettings{}, cfg)
	disp, err := dispatcher.NewDispatcher(s, cfg)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	// One shared provider instance for the ack-update loop (dispatcher) and
	// answerCallback (API). The escalation SEND no longer goes through it: the
	// engine admits commitments and the outbound worker below delivers them
	// through the channel handler, against the same fake Bot API.
	tg := telegramprovider.NewProvider(cache, "https://tokay.e2e", telegramprovider.WithBaseURL(server.URL))
	disp.RegisterProvider("telegram", tg)
	disp.RegisterProviderCapabilities(dispatcher.ProviderCapabilities{
		Name: "telegram", IntegrationType: model.IntegrationTypeTelegram, SupportedTargetKinds: []string{"dm", "channel"},
	})

	apiService := api.NewAPI(s, nil, nil, cache, "https://tokay.e2e", api.NewProviderCapsAdapter(disp.Providers()))
	apiService.SetTelegram(tg)

	e := echo.New()
	ing.RegisterRoutes(e)        // /webhook/alertmanager (ingest)
	apiService.RegisterRoutes(e) // /telegram/webhook (+ everything else)

	worker := outbound.NewWorker(s, "telegram-integration",
		map[string]outbound.Channel{
			"telegram": telegramprovider.NewHandler(cache,
				func(context.Context, string, string) (string, error) {
					return "", providers.ErrNotLinked
				},
				telegramprovider.WithHandlerBaseURL(server.URL)),
		})

	return &tgPipelineEnv{S: s, Eng: eng, Disp: disp, Worker: worker, Echo: e, Bot: bot}
}

func postTelegramWebhook(t *testing.T, e *echo.Echo, secret, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func waitForFake(t *testing.T, count func() int, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if count() >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s (count %d >= %d)", what, count(), want)
}

// TestTelegramPipeline_SendCallbackAck drives the full feature end-to-end against
// a fake Bot API: alert → policy → dispatcher → telegram card send (with Ack/Resolve
// keyboard), then an inbound callback_query → ack → answerCallbackQuery, and the
// ack-update loop → editMessageText.
func TestTelegramPipeline_SendCallbackAck(t *testing.T) {
	env := setupTelegramPipeline(t)

	// On-call user, linked to telegram, with ack permission (global admin).
	user := testutil.SeedUser(t, env.S, "tg-oncall@e2e.test")
	user.Role = model.UserRoleAdmin
	if err := env.S.UpdateUser(user); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if err := env.S.BindExternalIdentity(&model.ExternalIdentity{UserID: user.ID, Provider: "telegram", ExternalID: "777"}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}

	// 1. Ingest critical alert → engine → dispatcher → telegram channel send.
	payload := `{
		"groupKey": "tg_e2e_1", "status": "firing",
		"commonLabels": {"team": "tgteam", "severity": "critical", "alertname": "TGAlert"},
		"alerts": [{"fingerprint": "fp-tg-1", "status": "firing", "labels": {"alertname": "TGAlert"}}]
	}`
	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)
	go runOutboundWorker(ctx, env.Worker)

	waitForDeliveries(t, env.S, "tg_e2e_1", 1)

	// Assert: exactly one sendMessage carrying the card chat + Ack/Resolve keyboard.
	if got := env.Bot.count("sendMessage"); got != 1 {
		t.Fatalf("sendMessage calls = %d, want 1", got)
	}
	ag, err := env.S.GetActiveAlertGroupByAlertKey("tg_e2e_1")
	if err != nil {
		t.Fatalf("GetActiveAlertGroup: %v", err)
	}
	body := env.Bot.sendBody()
	if body["chat_id"] != "@e2echan" {
		t.Errorf("sendMessage chat_id = %v, want @e2echan", body["chat_id"])
	}
	rm, _ := json.Marshal(body["reply_markup"])
	if !strings.Contains(string(rm), "ack:"+ag.ID) || !strings.Contains(string(rm), "res:"+ag.ID) {
		t.Errorf("keyboard missing ack/res callback_data: %s", rm)
	}

	// Assert: a durable commitment that says where the message went. It is
	// editable because the card is a channel card - the alert can bring it to a
	// later revision - and it holds the coordinates that make that possible.
	var form, receipt string
	if err := env.S.GetDB().QueryRow(`
		SELECT i.form, COALESCE(i.receipt::text, '')
		FROM outbound_intents i WHERE i.alert_group_id = $1`, ag.ID).
		Scan(&form, &receipt); err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	if form != "editable" {
		t.Errorf("the channel card is %q", form)
	}
	if !strings.Contains(receipt, "4242") {
		t.Errorf("the commitment does not say where the message is: %q", receipt)
	}

	// 2. Inbound Ack button → answerCallbackQuery + AG acknowledged.
	cbBody := fmt.Sprintf(`{"callback_query":{"id":"cb1","from":{"id":777,"first_name":"Oncall"},"data":"ack:%s"}}`, ag.ID)
	if rec := postTelegramWebhook(t, env.Echo, tgWebhookSecret, cbBody); rec.Code != http.StatusOK {
		t.Fatalf("webhook callback: got %d: %s", rec.Code, rec.Body.String())
	}
	waitForFake(t, func() int { return env.Bot.count("answerCallbackQuery") }, 1, "answerCallbackQuery")

	acked, _ := env.S.GetAlertGroupByID(ag.ID)
	if acked.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("AG status = %s, want acknowledged", acked.Status)
	}

	// And the card is owed a new revision: the acknowledgement moved what its
	// messages have to show, so the commitment is back in the queue aimed at
	// it. Whether the worker has got there yet is a matter of timing; that it
	// was aimed is not.
	var intentStatus string
	var desired, applied int64
	if err := env.S.GetDB().QueryRow(`
		SELECT status, desired_revision, COALESCE(applied_revision, -1)
		FROM outbound_intents WHERE alert_group_id = $1`, ag.ID).
		Scan(&intentStatus, &desired, &applied); err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	switch intentStatus {
	case "pending", "sending":
		if desired <= applied {
			t.Errorf("the commitment is %q with nothing left to apply (%d applied of %d)",
				intentStatus, applied, desired)
		}
	case "idle", "succeeded", "canceled":
		if applied != desired {
			t.Errorf("the commitment settled as %q at revision %d, and %d is desired",
				intentStatus, applied, desired)
		}
	default:
		t.Errorf("the commitment is %q after the alert was acknowledged", intentStatus)
	}
}

// TestTelegramPipeline_StartLinkingAndSecretGuard covers the inbound /start deep-link
// binding through the full router, plus the secret-token rejection.
func TestTelegramPipeline_StartLinkingAndSecretGuard(t *testing.T) {
	env := setupTelegramPipeline(t)
	user := testutil.SeedUser(t, env.S, "tg-link@e2e.test")

	if err := env.S.IssueLinkToken(user.ID, "telegram", "", "deeplink-tok", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}
	startBody := `{"message":{"chat":{"id":888},"from":{"id":888,"first_name":"Linker","username":"linker"},"text":"/start deeplink-tok"}}`
	if rec := postTelegramWebhook(t, env.Echo, tgWebhookSecret, startBody); rec.Code != http.StatusOK {
		t.Fatalf("/start: got %d", rec.Code)
	}
	gotUser, err := env.S.GetUserByExternalID("telegram", "888")
	if err != nil || gotUser.ID != user.ID {
		t.Errorf("identity not linked via /start: %v / %+v", err, gotUser)
	}

	// Wrong secret is rejected by the middleware.
	if rec := postTelegramWebhook(t, env.Echo, "wrong-secret", `{}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret: got %d, want 401", rec.Code)
	}
}

// deliverCardForLinkedUser ingests a critical alert, runs the pipeline to deliver
// the telegram channel card, links an admin user's telegram id, and starts the
// dispatcher loop (so ack/resolution update jobs execute). Returns the AG.
func deliverCardForLinkedUser(t *testing.T, env *tgPipelineEnv, ctx context.Context, dedup, email, externalID string) *model.AlertGroup {
	t.Helper()
	user := testutil.SeedUser(t, env.S, email)
	user.Role = model.UserRoleAdmin
	if err := env.S.UpdateUser(user); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if err := env.S.BindExternalIdentity(&model.ExternalIdentity{UserID: user.ID, Provider: "telegram", ExternalID: externalID}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}
	payload := fmt.Sprintf(`{"groupKey":%q,"status":"firing","commonLabels":{"team":"tgteam","severity":"critical","alertname":"TGAlert"},"alerts":[{"fingerprint":"fp-%s","status":"firing","labels":{"alertname":"TGAlert"}}]}`, dedup, dedup)
	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())
	go runDispatcherLoop(ctx, env.Disp)
	go runOutboundWorker(ctx, env.Worker)
	waitForDeliveries(t, env.S, dedup, 1)
	ag, err := env.S.GetActiveAlertGroupByAlertKey(dedup)
	if err != nil {
		t.Fatalf("GetActiveAlertGroup: %v", err)
	}
	return ag
}

// TestTelegramPipeline_ResolveCallback covers the res: button branch end-to-end:
// callback → RBAC(resolve) → agService.Resolve → answerCallbackQuery, and the
// resolution loop → TelegramProvider.Resolve → editMessageText.
func TestTelegramPipeline_ResolveCallback(t *testing.T) {
	env := setupTelegramPipeline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ag := deliverCardForLinkedUser(t, env, ctx, "tg_e2e_resolve", "tg-resolve@e2e.test", "778")

	cbBody := fmt.Sprintf(`{"callback_query":{"id":"cbr","from":{"id":778,"first_name":"Oncall"},"data":"res:%s"}}`, ag.ID)
	if rec := postTelegramWebhook(t, env.Echo, tgWebhookSecret, cbBody); rec.Code != http.StatusOK {
		t.Fatalf("resolve callback: got %d: %s", rec.Code, rec.Body.String())
	}
	waitForFake(t, func() int { return env.Bot.count("answerCallbackQuery") }, 1, "answerCallbackQuery")

	resolved, _ := env.S.GetAlertGroupByID(ag.ID)
	if resolved.Status != model.AlertGroupStatusResolved {
		t.Errorf("AG status = %s, want resolved", resolved.Status)
	}

	// Two things follow, and they are different things. Nothing that had not
	// gone out is owed any more - nobody is paged about an alert somebody has
	// already closed. And the card that DID go out is owed its last revision:
	// it is still out there saying the alert is firing.
	var unsent int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_intents
		WHERE alert_group_id = $1 AND status IN ('pending', 'sending')
		  AND NOT receipt_recorded`, ag.ID).Scan(&unsent); err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	if unsent != 0 {
		t.Errorf("%d unsent notifications are still owed for a resolved alert", unsent)
	}

	var final bool
	if err := env.S.GetDB().QueryRow(
		`SELECT final FROM outbound_group_snapshots WHERE alert_group_id = $1`, ag.ID).
		Scan(&final); err != nil {
		t.Fatalf("read the state the card is brought to: %v", err)
	}
	if !final {
		t.Error("the resolution did not settle what the card has to show")
	}
}

// TestTelegramPipeline_LinkEndpointDeepLink exercises the full link loop against
// the REAL provider: authenticated POST /me/telegram/link → BotUsername via getMe
// (fake) + high-entropy token → t.me link → /start <token> → identity bound.
func TestTelegramPipeline_LinkEndpointDeepLink(t *testing.T) {
	env := setupTelegramPipeline(t)
	user := testutil.SeedUser(t, env.S, "tg-linkflow@e2e.test")

	token, err := auth.GenerateToken(user.ID)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/me/telegram/link", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.AddCookie(&http.Cookie{Name: api.AuthCookieName, Value: token})
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("link endpoint: got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode link response: %v", err)
	}
	const prefix = "https://t.me/e2e_bot?start=" // fake getMe returns username "e2e_bot"
	if !strings.HasPrefix(resp["link"], prefix) {
		t.Fatalf("link = %q, want prefix %q", resp["link"], prefix)
	}
	startToken := strings.TrimPrefix(resp["link"], prefix)
	if startToken == "" {
		t.Fatal("empty start token in link")
	}

	// Complete the link by simulating /start with the issued token.
	startBody := fmt.Sprintf(`{"message":{"chat":{"id":889},"from":{"id":889,"first_name":"Flow"},"text":"/start %s"}}`, startToken)
	if rec := postTelegramWebhook(t, env.Echo, tgWebhookSecret, startBody); rec.Code != http.StatusOK {
		t.Fatalf("/start: got %d", rec.Code)
	}
	gotUser, err := env.S.GetUserByExternalID("telegram", "889")
	if err != nil || gotUser.ID != user.ID {
		t.Errorf("link flow did not bind identity: %v / %+v", err, gotUser)
	}
}
