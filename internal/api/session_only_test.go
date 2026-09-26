package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// actionSpy is the store and the chat providers, recording every call that
// would change something or hand something out.
type actionSpy struct {
	*store.MockStore
	mu    sync.Mutex
	calls []string
}

func (s *actionSpy) record(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
}

func (s *actionSpy) UpdateUser(u *model.User) error {
	s.record("UpdateUser")
	return s.MockStore.UpdateUser(u)
}
func (s *actionSpy) CreateAPIToken(t *model.APIToken) error {
	s.record("CreateAPIToken")
	return s.MockStore.CreateAPIToken(t)
}
func (s *actionSpy) GetUserAPITokens(userID string) ([]*model.APIToken, error) {
	s.record("GetUserAPITokens")
	return s.MockStore.GetUserAPITokens(userID)
}
func (s *actionSpy) DeleteAPIToken(id string) error {
	s.record("DeleteAPIToken")
	return s.MockStore.DeleteAPIToken(id)
}
func (s *actionSpy) IssueLinkToken(userID, provider, externalID, token string, expiresAt time.Time) error {
	s.record("IssueLinkToken")
	return s.MockStore.IssueLinkToken(userID, provider, externalID, token, expiresAt)
}
func (s *actionSpy) ConfirmIdentityLink(userID, provider, token string) (*model.ExternalIdentity, error) {
	s.record("ConfirmIdentityLink")
	return s.MockStore.ConfirmIdentityLink(userID, provider, token)
}
func (s *actionSpy) UnbindExternalIdentity(userID, provider string) error {
	s.record("UnbindExternalIdentity")
	return s.MockStore.UnbindExternalIdentity(userID, provider)
}

// SendDM makes the spy the Slack messenger too: a code sent is an action.
func (s *actionSpy) SendDM(context.Context, string, string) error {
	s.record("SendDM")
	return nil
}
func (s *actionSpy) GetSlackUserIDByEmail(context.Context, string) (string, error) { return "", nil }
func (s *actionSpy) GetEmailBySlackID(context.Context, string) (string, error)     { return "", nil }

// An API token is refused on the routes a stolen token must not reach, and the
// refusal is all that happens: nothing is changed, nothing is sent, nothing is
// listed after it. Checking the status alone is what let the action run behind
// a 403.
func TestAnAPITokenIsRefusedAndNothingHappens(t *testing.T) {
	const userID, secret = "token-owner", "api-token-secret"
	spy := &actionSpy{MockStore: store.NewMockStore()}
	if err := spy.MockStore.CreateUser(&model.User{ID: userID, Email: "o@x.test", Name: "Owner"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	hash := sha256.Sum256([]byte(secret))
	if err := spy.MockStore.CreateAPIToken(&model.APIToken{
		ID: "tok-1", UserID: userID, Name: "ci", TokenHash: hex.EncodeToString(hash[:]), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	for _, provider := range []string{"slack", "telegram"} {
		if err := spy.MockStore.BindExternalIdentity(&model.ExternalIdentity{
			UserID: userID, Provider: provider, ExternalID: "ext-" + provider,
		}); err != nil {
			t.Fatalf("bind %s: %v", provider, err)
		}
	}
	a := NewAPI(spy, nil, spy, nil, "https://tokay.test", nil)
	a.SetTelegram(&fakeTelegramAPI{username: "tokay_bot"})
	e := echo.New()
	a.RegisterRoutes(e)

	const refusal = `{"error":"session authentication required"}`
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/auth/me", `{"name":"Renamed"}`},
		{http.MethodPost, "/api/auth/me/slack/request-code", `{"slack_user_id":"U999"}`},
		{http.MethodPost, "/api/auth/me/slack/confirm-code", `{"code":"123456"}`},
		{http.MethodDelete, "/api/auth/me/slack", ``},
		{http.MethodPost, "/api/auth/me/telegram/link", ``},
		{http.MethodDelete, "/api/auth/me/telegram", ``},
		{http.MethodGet, "/api/v1/tokens", ``},
		{http.MethodPost, "/api/v1/tokens", `{"name":"another"}`},
		{http.MethodDelete, "/api/v1/tokens/tok-1", ``},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			spy.mu.Lock()
			spy.calls = nil
			spy.mu.Unlock()

			req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			req.Header.Set("Authorization", "Bearer "+secret)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			// Exactly the refusal: a second document behind it is the action's
			// answer, written after the 403 went out.
			if got := strings.TrimSpace(rec.Body.String()); got != refusal {
				t.Fatalf("body = %s, want only the refusal", got)
			}
			spy.mu.Lock()
			defer spy.mu.Unlock()
			if len(spy.calls) != 0 {
				t.Fatalf("the refused request still did %v", spy.calls)
			}
		})
	}

	// And the state says the same.
	if user, _ := spy.MockStore.GetUserByID(userID); user.Name != "Owner" {
		t.Errorf("the name changed to %q", user.Name)
	}
	for _, provider := range []string{"slack", "telegram"} {
		if id, _ := spy.MockStore.GetExternalIdentity(userID, provider); id == nil {
			t.Errorf("the %s link was removed", provider)
		}
	}
	if tokens, _ := spy.MockStore.GetUserAPITokens(userID); len(tokens) != 1 {
		t.Errorf("the user has %d tokens, want the one they had", len(tokens))
	}
}
