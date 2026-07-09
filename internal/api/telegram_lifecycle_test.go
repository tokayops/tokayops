package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// Locks the Sprint-3-review webhook lifecycle contract: setWebhook on enable,
// no delete on a same-token edit, delete-old + set-new on rotation, delete on
// disable/delete. Drives the integration handlers directly over a MockStore +
// a recording fake TelegramAPI (no DB, no real Bot API).
func newLifecycleAPI(t *testing.T) (*API, *fakeTelegramAPI) {
	t.Helper()
	a := NewAPI(store.NewMockStore(), nil, nil, store.NewIntegrationCache(), "https://tokay.test", nil)
	ft := &fakeTelegramAPI{}
	a.SetTelegram(ft)
	return a, ft
}

func lcCreate(t *testing.T, a *API, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := echo.New().NewContext(req, rec)
	if err := a.CreateIntegration(c); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}
	return rec
}

func lcUpdate(t *testing.T, a *API, id, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/integrations/"+id, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := a.UpdateIntegration(c); err != nil {
		t.Fatalf("UpdateIntegration: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateIntegration: %d %s", rec.Code, rec.Body.String())
	}
}

func lcDelete(t *testing.T, a *API, id string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/integrations/"+id, nil)
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	if err := a.DeleteIntegration(c); err != nil {
		t.Fatalf("DeleteIntegration: %v", err)
	}
}

func lcCreateEnabled(t *testing.T, a *API, botToken, secret string) string {
	t.Helper()
	body := fmt.Sprintf(`{"type":"telegram","name":"tg","enabled":true,"config":{"bot_token":%q,"secret_token":%q}}`, botToken, secret)
	rec := lcCreate(t, a, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	id, _ := resp["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %s", rec.Body.String())
	}
	return id
}

func TestTelegramWebhookLifecycle(t *testing.T) {
	t.Run("create enabled registers webhook", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		lcCreateEnabled(t, a, "TOK1", "SEK1")

		if ft.setCalls != 1 || ft.delCalls != 0 {
			t.Fatalf("setCalls=%d delCalls=%d, want 1/0", ft.setCalls, ft.delCalls)
		}
		if ft.setTokens[0] != "TOK1" || ft.setSecrets[0] != "SEK1" {
			t.Errorf("setWebhook token/secret = %q/%q, want TOK1/SEK1", ft.setTokens[0], ft.setSecrets[0])
		}
		if ft.setURLs[0] != "https://tokay.test/telegram/webhook" {
			t.Errorf("setWebhook url = %q", ft.setURLs[0])
		}
	})

	t.Run("same-token update re-affirms without delete", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "TOK1", "SEK1")
		setBefore, delBefore := ft.setCalls, ft.delCalls

		lcUpdate(t, a, id, `{"name":"tg-renamed","enabled":true,"config":{"bot_token":"TOK1","secret_token":"SEK1"}}`)

		if ft.delCalls != delBefore {
			t.Errorf("same-token update must not DeleteWebhook (delCalls %d→%d)", delBefore, ft.delCalls)
		}
		if ft.setCalls != setBefore+1 {
			t.Errorf("same-token update should re-affirm SetWebhook (setCalls %d→%d)", setBefore, ft.setCalls)
		}
	})

	t.Run("token rotation deletes old + sets new", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "TOK1", "SEK1")

		lcUpdate(t, a, id, `{"enabled":true,"config":{"bot_token":"TOK2","secret_token":"SEK2"}}`)

		if len(ft.delTokens) == 0 || ft.delTokens[len(ft.delTokens)-1] != "TOK1" {
			t.Errorf("rotation should DeleteWebhook(TOK1), got delTokens=%v", ft.delTokens)
		}
		if ft.setTokens[len(ft.setTokens)-1] != "TOK2" {
			t.Errorf("rotation should SetWebhook(TOK2), got setTokens=%v", ft.setTokens)
		}
	})

	t.Run("disable deletes webhook, no new set", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "TOK1", "SEK1")
		setBefore := ft.setCalls

		lcUpdate(t, a, id, `{"enabled":false}`)

		if len(ft.delTokens) == 0 || ft.delTokens[len(ft.delTokens)-1] != "TOK1" {
			t.Errorf("disable should DeleteWebhook(TOK1), got delTokens=%v", ft.delTokens)
		}
		if ft.setCalls != setBefore {
			t.Errorf("disable should not SetWebhook (setCalls %d→%d)", setBefore, ft.setCalls)
		}
	})

	t.Run("delete deletes webhook", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "TOK1", "SEK1")

		lcDelete(t, a, id)

		if len(ft.delTokens) == 0 || ft.delTokens[len(ft.delTokens)-1] != "TOK1" {
			t.Errorf("delete should DeleteWebhook(TOK1), got delTokens=%v", ft.delTokens)
		}
	})
}
