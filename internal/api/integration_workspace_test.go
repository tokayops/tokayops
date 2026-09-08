package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// Slack is not asked which workspace a token belongs to in tests: the answer
// is given here, and a test that needs another sets its own.
func init() {
	slackWorkspaceURL = func(context.Context, string) (string, error) {
		return "https://acme.slack.com/", nil
	}
}

// answerForSlack makes auth.test answer per token for one test: a token in
// the list is refused, any other names the workspace.
func answerForSlack(t *testing.T, refused ...string) {
	t.Helper()
	was := slackWorkspaceURL
	slackWorkspaceURL = func(_ context.Context, token string) (string, error) {
		for _, bad := range refused {
			if token == bad {
				return "", errors.New("invalid_auth")
			}
		}
		return "https://acme.slack.com/", nil
	}
	t.Cleanup(func() { slackWorkspaceURL = was })
}

func slackConfigOf(t *testing.T, e *echo.Echo, id string) model.SlackConfig {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+id, nil)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("read the integration: %d %s", rec.Code, rec.Body.String())
	}
	var shown model.Integration
	if err := json.Unmarshal(rec.Body.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	var cfg model.SlackConfig
	if err := json.Unmarshal(shown.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func saveSlack(t *testing.T, e *echo.Echo, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestSavingASlackIntegrationRecordsItsWorkspace. The workspace a token
// belongs to is asked of Slack when the integration is saved and kept in its
// configuration, shown like any setting that is not a secret. A new token
// Slack refuses records no workspace - a link into a workspace the token
// cannot reach is worse than none - while a save that keeps the token keeps
// the workspace it had: Slack being unreachable is not evidence about it.
func TestSavingASlackIntegrationRecordsItsWorkspace(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)
	answerForSlack(t, "xoxb-bad")

	rec := saveSlack(t, e, http.MethodPost, "/api/v1/integrations",
		`{"type":"slack","name":"Slack","enabled":true,"config":{"token":"xoxb-good","interactive":true}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created model.Integration
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if cfg := slackConfigOf(t, e, created.ID); cfg.TeamURL != "https://acme.slack.com/" || cfg.Token != model.MaskedSecret {
		t.Fatalf("after the save the configuration shows workspace %q and token %q", cfg.TeamURL, cfg.Token)
	}

	// The same token, masked, while Slack is refusing: the workspace stays.
	answerForSlack(t, "xoxb-good", "xoxb-bad")
	if rec := saveSlack(t, e, http.MethodPut, "/api/v1/integrations/"+created.ID,
		fmt.Sprintf(`{"config":{"token":%q,"interactive":false}}`, model.MaskedSecret)); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if cfg := slackConfigOf(t, e, created.ID); cfg.TeamURL != "https://acme.slack.com/" || cfg.Interactive {
		t.Fatalf("a save that kept the token lost the workspace: %+v", cfg)
	}

	// A new token Slack refuses: the workspace is cleared, the save goes through.
	if rec := saveSlack(t, e, http.MethodPut, "/api/v1/integrations/"+created.ID,
		`{"config":{"token":"xoxb-bad"}}`); rec.Code != http.StatusOK {
		t.Fatalf("update with a refused token: %d %s", rec.Code, rec.Body.String())
	}
	if cfg := slackConfigOf(t, e, created.ID); cfg.TeamURL != "" {
		t.Fatalf("a refused token kept a workspace: %q", cfg.TeamURL)
	}

	// A new token Slack accepts: recorded again.
	if rec := saveSlack(t, e, http.MethodPut, "/api/v1/integrations/"+created.ID,
		`{"config":{"token":"xoxb-newer"}}`); rec.Code != http.StatusOK {
		t.Fatalf("update with a good token: %d %s", rec.Code, rec.Body.String())
	}
	if cfg := slackConfigOf(t, e, created.ID); cfg.TeamURL != "https://acme.slack.com/" {
		t.Fatalf("a good token recorded %q", cfg.TeamURL)
	}
}

// TestTestingASlackIntegrationRecordsItsWorkspace. An integration saved
// before there was a workspace to record gets it when it is tested; a test
// Slack refuses records nothing and says so.
func TestTestingASlackIntegrationRecordsItsWorkspace(t *testing.T) {
	s := store.NewMockStore()
	api := NewAPI(s, nil, &mockSlackMessenger{}, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)
	integration := &model.Integration{
		Type: model.IntegrationTypeSlack, Name: "Slack", Enabled: true,
		Config: []byte(`{"token":"xoxb-old"}`),
	}
	if err := s.CreateIntegration(integration); err != nil {
		t.Fatal(err)
	}
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: "denis", Provider: "slack", ExternalID: "U123"})

	answerForSlack(t, "xoxb-refused")
	if rec := saveSlack(t, e, http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", `{}`); rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body.String())
	}
	if cfg := slackConfigOf(t, e, integration.ID); cfg.TeamURL != "https://acme.slack.com/" {
		t.Fatalf("the test recorded %q", cfg.TeamURL)
	}

	answerForSlack(t, "xoxb-old")
	rec := saveSlack(t, e, http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", `{}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "auth.test") {
		t.Fatalf("a refused test answered %d %s", rec.Code, rec.Body.String())
	}
	if cfg := slackConfigOf(t, e, integration.ID); cfg.TeamURL != "https://acme.slack.com/" {
		t.Fatalf("a refused test changed the workspace to %q", cfg.TeamURL)
	}
}
