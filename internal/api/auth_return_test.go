package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/store"
)

func TestSafeReturnHash(t *testing.T) {
	ok := []string{
		"#/ops/alert-groups/ag-1",
		"#/ops/alert-groups",
		"#/cfg/teams",
		"#/cfg/integrations",
		"#/ops/oncall",
		"#/ops/activity",
	}
	for _, hash := range ok {
		if got := safeReturnHash(hash); got != hash {
			t.Errorf("safeReturnHash(%q) = %q, want the route", hash, got)
		}
	}

	rejected := []string{
		"",
		"/",
		"/login.html",
		"https://evil.example/phish",
		"//evil.example",
		`/\evil.example`,
		"#/ops/https://evil.example",
		"#/other/place",
		"#/OPS/alert-groups",
		"#/ops/alert-groups/ag-1%0aSet-Cookie:%20x",
		"#/ops/alert-groups/ag-1\n",
		"javascript:alert(1)",
		"#/ops/alert-groups/" + strings.Repeat("a", maxReturnHashLength),
	}
	for _, hash := range rejected {
		if got := safeReturnHash(hash); got != "" {
			t.Errorf("safeReturnHash(%q) = %q, want it refused", hash, got)
		}
	}
}

type fakeOIDC struct {
	user *auth.OIDCUserInfo
}

func (f *fakeOIDC) IsEnabled() bool { return true }

func (f *fakeOIDC) GetAuthURL(state string) string {
	return "https://idp.example/auth?state=" + url.QueryEscape(state)
}

func (f *fakeOIDC) ExchangeCode(context.Context, string) (*auth.OIDCUserInfo, error) {
	return f.user, nil
}

func (f *fakeOIDC) ValidateDomain(string) bool { return true }

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestOIDCRedirectKeepsTheReturnRoute(t *testing.T) {
	e := echo.New()
	api := NewAPI(store.NewMockStore(), nil, nil, nil, "", nil)
	api.oidcProvider = &fakeOIDC{}

	t.Run("a hash route is stored beside the state", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/redirect?next="+url.QueryEscape("#/ops/alert-groups/ag-1"), nil)
		rec := httptest.NewRecorder()
		if err := api.OIDCRedirect(e.NewContext(req, rec)); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusFound {
			t.Fatalf("status %d, want 302", rec.Code)
		}
		if !strings.HasPrefix(rec.Header().Get("Location"), "https://idp.example/auth?state=") {
			t.Fatalf("Location %q does not start the provider flow", rec.Header().Get("Location"))
		}
		next := cookieNamed(rec, oidcNextCookieName)
		if next == nil {
			t.Fatal("oidc_next was not set")
		}
		if next.HttpOnly != true {
			t.Error("oidc_next should be HttpOnly")
		}
		got, err := url.QueryUnescape(next.Value)
		if err != nil {
			t.Fatal(err)
		}
		if got != "#/ops/alert-groups/ag-1" {
			t.Errorf("oidc_next = %q", got)
		}
		if cookieNamed(rec, "oidc_state") == nil {
			t.Error("oidc_state was not set")
		}
	})

	t.Run("a foreign target clears a route already stored", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/redirect?next="+url.QueryEscape("https://evil.example"), nil)
		req.AddCookie(&http.Cookie{Name: oidcNextCookieName, Value: url.QueryEscape("#/ops/alert-groups/ag-1")})
		rec := httptest.NewRecorder()
		if err := api.OIDCRedirect(e.NewContext(req, rec)); err != nil {
			t.Fatal(err)
		}
		next := cookieNamed(rec, oidcNextCookieName)
		if next == nil || next.MaxAge > 0 {
			t.Fatalf("oidc_next = %#v, want it cleared", next)
		}
	})
}

func TestOIDCCallbackRestoresTheReturnRoute(t *testing.T) {
	e := echo.New()
	api := NewAPI(store.NewMockStore(), nil, nil, nil, "", nil)
	api.oidcProvider = &fakeOIDC{user: &auth.OIDCUserInfo{Email: "sso@example.com", Name: "SSO"}}

	call := func(next string, state string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback?code=abc&state="+url.QueryEscape(state), nil)
		req.AddCookie(&http.Cookie{Name: "oidc_state", Value: "state-1"})
		if next != "" {
			req.AddCookie(&http.Cookie{Name: oidcNextCookieName, Value: url.QueryEscape(next)})
		}
		rec := httptest.NewRecorder()
		if err := api.OIDCCallback(e.NewContext(req, rec)); err != nil {
			t.Fatal(err)
		}
		return rec
	}

	t.Run("the callback lands on the route", func(t *testing.T) {
		rec := call("#/ops/alert-groups/ag-1", "state-1")
		if rec.Code != http.StatusFound {
			t.Fatalf("status %d, want 302", rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/#/ops/alert-groups/ag-1" {
			t.Errorf("Location = %q", got)
		}
		next := cookieNamed(rec, oidcNextCookieName)
		if next == nil || next.MaxAge > 0 {
			t.Fatalf("oidc_next = %#v, want it cleared", next)
		}
	})

	t.Run("a foreign target lands at home", func(t *testing.T) {
		rec := call("https://evil.example/phish", "state-1")
		if got := rec.Header().Get("Location"); got != "/" {
			t.Errorf("Location = %q, want /", got)
		}
	})

	t.Run("a bad state forgets the route", func(t *testing.T) {
		rec := call("#/ops/alert-groups/ag-1", "nope")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rec.Code)
		}
		next := cookieNamed(rec, oidcNextCookieName)
		if next == nil || next.MaxAge > 0 {
			t.Fatalf("oidc_next = %#v, want it cleared", next)
		}
	})
}
