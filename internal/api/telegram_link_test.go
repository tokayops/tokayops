package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

func TestTelegramLink_RequestUnbindRoundTrip(t *testing.T) {
	s := store.NewMockStore()
	const userID = "tg-link-user"
	if err := s.CreateUser(&model.User{ID: userID, Email: userID + "@tg.test", Name: "TG User"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a := NewAPI(s, nil, nil, nil, "https://tokay.test", nil) // selfURL set → linking enabled
	a.SetTelegram(&fakeTelegramAPI{username: "tokay_bot"})
	e := echo.New()
	a.RegisterRoutes(e)

	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		token, err := auth.GenerateToken(userID)
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	// 1. Request link → 200 with a t.me deep link.
	rec := do(http.MethodPost, "/api/auth/me/telegram/link")
	if rec.Code != http.StatusOK {
		t.Fatalf("link: got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp["link"], "https://t.me/tokay_bot?start=") {
		t.Errorf("unexpected link: %q", resp["link"])
	}

	// 2. Once linked, a second request is rejected (409).
	if err := s.BindExternalIdentity(&model.ExternalIdentity{UserID: userID, Provider: "telegram", ExternalID: "424242"}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if rec := do(http.MethodPost, "/api/auth/me/telegram/link"); rec.Code != http.StatusConflict {
		t.Errorf("second link with existing identity: got %d, want 409", rec.Code)
	}

	// 3. Unbind → 204, then linking is available again.
	if rec := do(http.MethodDelete, "/api/auth/me/telegram"); rec.Code != http.StatusNoContent {
		t.Errorf("unbind: got %d, want 204", rec.Code)
	}
	if rec := do(http.MethodPost, "/api/auth/me/telegram/link"); rec.Code != http.StatusOK {
		t.Errorf("link after unbind: got %d, want 200", rec.Code)
	}
}

// TestTelegramLink_RequiresSelfURL: without TOKAY_SELF_URL the /start webhook can't be
// registered, so linking can never complete — the endpoint fails fast (503) instead
// of issuing a dead deep link.
func TestTelegramLink_RequiresSelfURL(t *testing.T) {
	s := store.NewMockStore()
	const userID = "tg-nourl-user"
	if err := s.CreateUser(&model.User{ID: userID, Email: userID + "@tg.test", Name: "TG User"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a := NewAPI(s, nil, nil, nil, "", nil) // selfURL empty → linking unavailable
	a.SetTelegram(&fakeTelegramAPI{username: "tokay_bot"})
	e := echo.New()
	a.RegisterRoutes(e)

	req := httptest.NewRequest(http.MethodPost, "/api/auth/me/telegram/link", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	token, err := auth.GenerateToken(userID)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("link without TOKAY_SELF_URL: got %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "tokay_self_url") {
		t.Errorf("503 body should explain TOKAY_SELF_URL, got %s", rec.Body.String())
	}
}
