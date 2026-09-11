package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/store"
)

// Locks the webhook lifecycle contract: setWebhook on enable,
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

// The effects of lifecycle commands run in the reverse order of their commits.
//
// Transactions are serialised by the row lock; the Telegram calls after them
// are not, and two instances can run them in either order. The reconcile
// decides by the row as it stands, so the order of the effects does not
// decide what ends up registered. The barrier is the test itself: the commands
// are run through the store, and their effects are then run backwards.
func TestTelegramEffectsInReverseOrderRegisterOnlyTheCurrentToken(t *testing.T) {
	ctx := context.Background()
	rotate := func(t *testing.T, a *API, id, token string) store.IntegrationChange {
		t.Helper()
		cfg := fmt.Sprintf(`{"bot_token":%q,"secret_token":"SEK"}`, token)
		change, err := a.store.UpdateIntegration(ctx, id, store.IntegrationPatch{Config: []byte(cfg)}, "nina")
		if err != nil {
			t.Fatalf("rotate to %s: %v", token, err)
		}
		return change
	}

	t.Run("two rotations, effects backwards", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "T1", "SEK")
		first := rotate(t, a, id, "T2")  // T1 -> T2, committed
		second := rotate(t, a, id, "T3") // T2 -> T3, committed

		a.reconcileTelegramWebhook(ctx, id, second.Before, second.After)
		a.reconcileTelegramWebhook(ctx, id, first.Before, first.After)

		if got := ft.registeredTokens(); !reflect.DeepEqual(got, []string{"T3"}) {
			t.Fatalf("registered %v, want only the current token T3", got)
		}
	})

	t.Run("an edit and a deletion, effects backwards", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "T1", "SEK")
		edit := rotate(t, a, id, "T2")
		deletion, err := a.store.DeleteIntegration(ctx, id, "nina")
		if err != nil {
			t.Fatal(err)
		}

		a.reconcileTelegramWebhook(ctx, id, deletion.Before)
		a.reconcileTelegramWebhook(ctx, id, edit.Before, edit.After)

		if got := ft.registeredTokens(); len(got) != 0 {
			t.Fatalf("a deleted integration has a webhook registered: %v", got)
		}
	})

	t.Run("rotating back never removes the live token", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "T1", "SEK")
		away := rotate(t, a, id, "T2")
		a.reconcileTelegramWebhook(ctx, id, away.Before, away.After)
		back := rotate(t, a, id, "T1")
		deletesBefore := len(ft.delTokens)
		a.reconcileTelegramWebhook(ctx, id, back.Before, back.After)

		if got := ft.registeredTokens(); !reflect.DeepEqual(got, []string{"T1"}) {
			t.Fatalf("registered %v, want T1", got)
		}
		for _, token := range ft.delTokens[deletesBefore:] {
			if token == "T1" {
				t.Fatalf("rotating back to T1 removed T1 on the way: %v", ft.delTokens[deletesBefore:])
			}
		}
	})

	t.Run("switching off with the same token removes it", func(t *testing.T) {
		a, ft := newLifecycleAPI(t)
		id := lcCreateEnabled(t, a, "T1", "SEK")
		off := false
		change, err := a.store.UpdateIntegration(ctx, id, store.IntegrationPatch{Enabled: &off}, "nina")
		if err != nil {
			t.Fatal(err)
		}
		a.reconcileTelegramWebhook(ctx, id, change.Before, change.After)
		if got := ft.registeredTokens(); len(got) != 0 {
			t.Fatalf("a switched-off bot has a webhook registered: %v", got)
		}
	})
}
