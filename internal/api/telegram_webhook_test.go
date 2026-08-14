package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// fakeTelegramAPI records the calls the API layer makes into the provider,
// including the webhook tokens so lifecycle tests can assert which bot was
// (de)registered across enable/disable/token-rotation.
type fakeTelegramAPI struct {
	answered   []string
	sent       []string
	setCalls   int
	delCalls   int
	setTokens  []string
	setURLs    []string
	setSecrets []string
	delTokens  []string
	username   string
}

func (f *fakeTelegramAPI) AnswerCallback(_ context.Context, _, text string) error {
	f.answered = append(f.answered, text)
	return nil
}
func (f *fakeTelegramAPI) SendText(_ context.Context, _, text string) error {
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeTelegramAPI) BotUsername(_ context.Context) (string, error) {
	if f.username == "" {
		return "tokay_bot", nil
	}
	return f.username, nil
}
func (f *fakeTelegramAPI) SetWebhook(_ context.Context, token, url, secret string) error {
	f.setCalls++
	f.setTokens = append(f.setTokens, token)
	f.setURLs = append(f.setURLs, url)
	f.setSecrets = append(f.setSecrets, secret)
	return nil
}
func (f *fakeTelegramAPI) DeleteWebhook(_ context.Context, token string) error {
	f.delCalls++
	f.delTokens = append(f.delTokens, token)
	return nil
}

func setupTelegramAPI(t *testing.T, secret string) (*API, *store.MockStore, *echo.Echo, *fakeTelegramAPI) {
	t.Helper()
	s := store.NewMockStore()
	cfg, _ := json.Marshal(model.TelegramConfig{BotToken: "123:abc", SecretToken: secret})
	s.CreateIntegration(&model.Integration{
		ID: "int-tg", Type: model.IntegrationTypeTelegram, Name: "tg", Enabled: true, Config: cfg,
	})
	cache := store.NewIntegrationCache()
	if err := cache.LoadAll(s); err != nil {
		t.Fatalf("cache.LoadAll: %v", err)
	}
	api := NewAPI(s, nil, nil, cache, "https://tokay.example", nil)
	ft := &fakeTelegramAPI{}
	api.SetTelegram(ft)
	e := echo.New()
	api.RegisterRoutes(e)
	return api, s, e, ft
}

func telegramWebhookReq(secret, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/telegram/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret)
	}
	return req
}

func TestTelegramWebhook_SecretMiddleware(t *testing.T) {
	const secret = "tg-secret"
	_, _, e, _ := setupTelegramAPI(t, secret)

	t.Run("missing header rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, telegramWebhookReq("", "{}"))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("missing secret: got %d, want 401", rec.Code)
		}
	})
	t.Run("wrong header rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, telegramWebhookReq("nope", "{}"))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong secret: got %d, want 401", rec.Code)
		}
	})
	t.Run("correct header accepted", func(t *testing.T) {
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, telegramWebhookReq(secret, "{}"))
		if rec.Code != http.StatusOK {
			t.Errorf("correct secret: got %d, want 200", rec.Code)
		}
	})
	t.Run("unset secret → 503", func(t *testing.T) {
		s := store.NewMockStore()
		cache := store.NewIntegrationCache()
		_ = cache.LoadAll(s)
		api := NewAPI(s, nil, nil, cache, "", nil)
		api.SetTelegram(&fakeTelegramAPI{})
		ee := echo.New()
		api.RegisterRoutes(ee)
		rec := httptest.NewRecorder()
		ee.ServeHTTP(rec, telegramWebhookReq("anything", "{}"))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("unset secret: got %d, want 503", rec.Code)
		}
	})
}

func TestTelegramWebhook_StartLinks(t *testing.T) {
	const secret = "tg-secret"
	_, s, e, ft := setupTelegramAPI(t, secret)

	denis, err := s.GetUserByEmail("denis@example.com")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := s.IssueLinkToken(denis.ID, "telegram", "", "tok-start", time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}

	body := `{"message":{"chat":{"id":555},"from":{"id":555,"first_name":"Denis","username":"denisp"},"text":"/start tok-start"}}`
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, telegramWebhookReq(secret, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	gotUser, err := s.GetUserByExternalID("telegram", "555")
	if err != nil || gotUser.ID != denis.ID {
		t.Errorf("identity not linked to denis: %v / %+v", err, gotUser)
	}
	if len(ft.sent) == 0 || !strings.Contains(strings.ToLower(ft.sent[0]), "linked") {
		t.Errorf("expected a 'linked' confirmation, got %v", ft.sent)
	}
}

func TestTelegramWebhook_CallbackAcks(t *testing.T) {
	const secret = "tg-secret"
	_, s, e, ft := setupTelegramAPI(t, secret)

	denis, _ := s.GetUserByEmail("denis@example.com")
	if err := s.BindExternalIdentity(&model.ExternalIdentity{UserID: denis.ID, Provider: "telegram", ExternalID: "777"}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}
	agID := "ag-tg-" + fmt.Sprintf("%d", time.Now().UnixNano())
	s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, DedupKey: "dk-" + agID, Status: model.AlertGroupStatusTriggered,
		Title: "Test Alert", TeamID: "devops", TeamNameSnapshot: "DevOps", Severity: "critical",
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
	})

	body := fmt.Sprintf(`{"callback_query":{"id":"cb1","from":{"id":777,"first_name":"Denis"},"data":"ack:%s"}}`, agID)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, telegramWebhookReq(secret, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}

	ag, _ := s.GetAlertGroupByID(agID)
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("status = %s, want acknowledged", ag.Status)
	}
	if len(ft.answered) == 0 {
		t.Error("expected answerCallbackQuery to be called")
	}
}

func TestTelegramWebhook_CallbackUnlinked(t *testing.T) {
	const secret = "tg-secret"
	_, s, e, ft := setupTelegramAPI(t, secret)

	agID := "ag-unlinked"
	s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, DedupKey: "dk-" + agID, Status: model.AlertGroupStatusTriggered,
		Title: "T", TeamID: "devops", Severity: "critical", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	body := fmt.Sprintf(`{"callback_query":{"id":"cb1","from":{"id":999999},"data":"ack:%s"}}`, agID)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, telegramWebhookReq(secret, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	ag, _ := s.GetAlertGroupByID(agID)
	if ag.Status != model.AlertGroupStatusTriggered {
		t.Errorf("unlinked user must not change status; got %s", ag.Status)
	}
	if len(ft.answered) == 0 || !strings.Contains(strings.ToLower(ft.answered[0]), "linked") {
		t.Errorf("expected a not-linked hint, got %v", ft.answered)
	}
}

func TestTelegram_RegisterWebhookOnStartup(t *testing.T) {
	newAPI := func(selfURL string, withIntegration bool) (*API, *fakeTelegramAPI) {
		s := store.NewMockStore()
		if withIntegration {
			cfg, _ := json.Marshal(model.TelegramConfig{BotToken: "BOOT_TOK", SecretToken: "BOOT_SEK"})
			if err := s.CreateIntegration(&model.Integration{Type: model.IntegrationTypeTelegram, Name: "tg", Enabled: true, Config: cfg}); err != nil {
				t.Fatalf("CreateIntegration: %v", err)
			}
		}
		a := NewAPI(s, nil, nil, store.NewIntegrationCache(), selfURL, nil)
		ft := &fakeTelegramAPI{}
		a.SetTelegram(ft)
		return a, ft
	}

	t.Run("enabled integration + selfURL registers webhook", func(t *testing.T) {
		a, ft := newAPI("https://tokay.test", true)
		a.RegisterTelegramWebhookOnStartup(context.Background())
		if ft.setCalls != 1 {
			t.Fatalf("setCalls=%d, want 1", ft.setCalls)
		}
		if ft.setTokens[0] != "BOOT_TOK" || ft.setURLs[0] != "https://tokay.test/telegram/webhook" {
			t.Errorf("SetWebhook(token=%q url=%q), want BOOT_TOK + .../telegram/webhook", ft.setTokens[0], ft.setURLs[0])
		}
	})

	t.Run("no selfURL → no registration", func(t *testing.T) {
		a, ft := newAPI("", true)
		a.RegisterTelegramWebhookOnStartup(context.Background())
		if ft.setCalls != 0 {
			t.Errorf("no selfURL → no SetWebhook, got setCalls=%d", ft.setCalls)
		}
	})

	t.Run("no telegram integration → no-op", func(t *testing.T) {
		a, ft := newAPI("https://tokay.test", false)
		a.RegisterTelegramWebhookOnStartup(context.Background()) // must not panic
		if ft.setCalls != 0 {
			t.Errorf("no integration → no SetWebhook, got setCalls=%d", ft.setCalls)
		}
	})
}
