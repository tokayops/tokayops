package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func setupIntegrationTestAPI(t *testing.T) (*API, *store.MockStore, *echo.Echo) {
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)
	return api, s, e
}

// failingIdentityStore wraps MockStore but makes GetExternalIdentity fail with a
// caller-supplied non-sql.ErrNoRows error - used to exercise the "DB error → 500"
// branch of testSlackIntegration.
type failingIdentityStore struct {
	store.StoreInterface
	err error
}

func (f *failingIdentityStore) GetExternalIdentity(userID, provider string) (*model.ExternalIdentity, error) {
	return nil, f.err
}

func TestListIntegrations(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)

	t.Run("returns empty list for non-admin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
		addAuth(req, "alex") // non-admin user
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d", rec.Code)
		}
	})

	t.Run("admin can list", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
		addAuth(req, "denis") // admin user
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d, body: %s", rec.Code, rec.Body.String())
		}

		var resp IntegrationListResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Integrations == nil {
			t.Error("Expected non-nil integrations array")
		}
	})
}

func TestCreateIntegration(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)

	t.Run("non-admin forbidden", func(t *testing.T) {
		body := `{"type":"slack","name":"Slack","enabled":true,"config":{"token":"xoxb-test"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alex")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d", rec.Code)
		}
	})

	t.Run("admin can create slack integration", func(t *testing.T) {
		body := `{"type":"slack","name":"My Slack","enabled":true,"config":{"token":"xoxb-test","default_channel":"C123"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201, got %d, body: %s", rec.Code, rec.Body.String())
		}

		var created model.Integration
		json.Unmarshal(rec.Body.Bytes(), &created)

		if created.ID == "" {
			t.Error("Expected ID to be set")
		}
		if created.Type != model.IntegrationTypeSlack {
			t.Errorf("Expected type slack, got %s", created.Type)
		}
		if created.Direction != model.IntegrationDirectionOutbound {
			t.Errorf("Expected direction outbound, got %s", created.Direction)
		}

		// Token should be masked in response
		var cfg model.SlackConfig
		json.Unmarshal(created.Config, &cfg)
		if cfg.Token != model.MaskedSecret {
			t.Errorf("Token should be masked in response, got: %s", cfg.Token)
		}
	})

	t.Run("admin can create webhook integration", func(t *testing.T) {
		body := `{"type":"alertmanager_webhook","name":"My Webhook","enabled":true,"config":{"secret":"webhook-secret"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201, got %d, body: %s", rec.Code, rec.Body.String())
		}

		var created model.Integration
		json.Unmarshal(rec.Body.Bytes(), &created)

		if created.Direction != model.IntegrationDirectionInbound {
			t.Errorf("Expected direction inbound, got %s", created.Direction)
		}
	})

	t.Run("invalid type rejected", func(t *testing.T) {
		body := `{"type":"invalid","name":"Bad","enabled":true,"config":{}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d", rec.Code)
		}
	})

	t.Run("missing name rejected", func(t *testing.T) {
		body := `{"type":"slack","enabled":true,"config":{"token":"test"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("masked signing_secret rejected on create", func(t *testing.T) {
		body := `{"type":"slack","name":"Slack","enabled":true,"config":{"token":"xoxb-test","signing_secret":"****"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing token rejected for slack", func(t *testing.T) {
		body := `{"type":"slack","name":"Slack","enabled":true,"config":{"default_channel":"C123"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d", rec.Code)
		}
	})
}

func TestCreateGenericWebhook(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)

	t.Run("create global webhook", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"Global WH","scope":"global","config":{"url":"https://example.com/hook","secret":"s3cret"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("Expected 201, got %d, body: %s", rec.Code, rec.Body.String())
		}

		var created model.Integration
		json.Unmarshal(rec.Body.Bytes(), &created)

		if created.Direction != model.IntegrationDirectionOutbound {
			t.Errorf("Expected outbound, got %s", created.Direction)
		}
		if created.Scope == nil || *created.Scope != model.WebhookScopeGlobal {
			t.Errorf("Expected scope=global, got %v", created.Scope)
		}
		if created.TeamID != nil {
			t.Errorf("Expected no team_id, got %v", created.TeamID)
		}

		var cfg model.GenericWebhookConfig
		json.Unmarshal(created.Config, &cfg)
		if cfg.Secret != model.MaskedSecret {
			t.Errorf("Secret should be masked, got: %s", cfg.Secret)
		}
		if cfg.URL != "https://example.com/hook" {
			t.Errorf("URL should be preserved, got: %s", cfg.URL)
		}
	})

	t.Run("create team webhook", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"Team WH","scope":"team","team_id":"devops","config":{"url":"https://example.com/hook","secret":"s3cret"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("Expected 201, got %d, body: %s", rec.Code, rec.Body.String())
		}

		var created model.Integration
		json.Unmarshal(rec.Body.Bytes(), &created)

		if created.Scope == nil || *created.Scope != model.WebhookScopeTeam {
			t.Errorf("Expected scope=team, got %v", created.Scope)
		}
		if created.TeamID == nil || *created.TeamID != "devops" {
			t.Errorf("Expected team_id=devops, got %v", created.TeamID)
		}
	})

	t.Run("multiple webhooks allowed", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			body := `{"type":"generic_webhook","name":"WH","scope":"global","config":{"url":"https://example.com/hook","secret":"s3cret"}}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			addAuth(req, "denis")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusCreated {
				t.Fatalf("Webhook %d: Expected 201, got %d, body: %s", i, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("missing scope rejected", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"WH","config":{"url":"https://example.com","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing team_id for team scope rejected", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"WH","scope":"team","config":{"url":"https://example.com","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_id with global scope rejected", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"WH","scope":"global","team_id":"devops","config":{"url":"https://example.com","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("scope on non-webhook rejected", func(t *testing.T) {
		body := `{"type":"slack","name":"Slack","scope":"global","config":{"token":"xoxb-test"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("stray team_id on slack rejected", func(t *testing.T) {
		body := `{"type":"slack","name":"Slack","team_id":"devops","config":{"token":"xoxb-test"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty team_id string normalized to nil", func(t *testing.T) {
		body := `{"type":"alertmanager_webhook","name":"AM","team_id":"","config":{"secret":"s3cret"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201 (empty team_id normalized to nil), got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing url rejected", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"WH","scope":"global","config":{"secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("missing secret allowed (optional)", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"WH No Secret","scope":"global","config":{"url":"https://example.com"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("http url rejected by default", func(t *testing.T) {
		os.Unsetenv(config.WebhookAllowHTTPEnv)
		body := `{"type":"generic_webhook","name":"WH","scope":"global","config":{"url":"http://example.com/hook","secret":"s3cret"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("http url accepted with override", func(t *testing.T) {
		os.Setenv(config.WebhookAllowHTTPEnv, "true")
		defer os.Unsetenv(config.WebhookAllowHTTPEnv)

		body := `{"type":"generic_webhook","name":"WH","scope":"global","config":{"url":"http://example.com/hook","secret":"s3cret"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201 with HTTP override, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestGetIntegration(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)

	t.Run("non-admin gets 404 for non-existent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/some-id", nil)
		addAuth(req, "alex")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("not found for non-existent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/non-existent", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})
}

func TestDeleteIntegration(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)

	t.Run("non-admin gets 404 for non-existent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/integrations/some-id", nil)
		addAuth(req, "alex")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("not found for non-existent", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/integrations/non-existent", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})
}

func TestUpdateIntegration(t *testing.T) {
	_, _, e := setupIntegrationTestAPI(t)

	t.Run("non-admin gets 404 for non-existent", func(t *testing.T) {
		body := `{"name":"Updated"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/integrations/some-id", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alex")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("not found for non-existent", func(t *testing.T) {
		body := `{"name":"Updated"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/integrations/non-existent", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("update rejects pre-existing http url when override disabled", func(t *testing.T) {
		// Create a webhook with http URL while override is enabled
		os.Setenv(config.WebhookAllowHTTPEnv, "true")
		scope := model.WebhookScopeGlobal
		createBody := `{"type":"generic_webhook","name":"HTTP WH","scope":"global","config":{"url":"http://example.com/hook","secret":"s3cret"}}`
		createReq := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(createBody))
		createReq.Header.Set("Content-Type", "application/json")
		addAuth(createReq, "denis")
		createRec := httptest.NewRecorder()
		e.ServeHTTP(createRec, createReq)
		if createRec.Code != http.StatusCreated {
			t.Fatalf("Setup: expected 201, got %d, body: %s", createRec.Code, createRec.Body.String())
		}
		var created model.Integration
		json.Unmarshal(createRec.Body.Bytes(), &created)
		_ = scope // used in create body

		// Disable override, then try to rename - should be rejected
		os.Unsetenv(config.WebhookAllowHTTPEnv)
		updateBody := `{"name":"Renamed"}`
		updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/integrations/"+created.ID, strings.NewReader(updateBody))
		updateReq.Header.Set("Content-Type", "application/json")
		addAuth(updateReq, "denis")
		updateRec := httptest.NewRecorder()
		e.ServeHTTP(updateRec, updateReq)

		if updateRec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400 for pre-existing http URL, got %d, body: %s", updateRec.Code, updateRec.Body.String())
		}
	})
}

// mockSlackMessenger implements SlackMessenger for testing
type mockSlackMessenger struct {
	sendDMCalled bool
	lastUserID   string
	lastMessage  string
	returnError  error
}

func (m *mockSlackMessenger) SendDM(ctx context.Context, userID, message string) error {
	m.sendDMCalled = true
	m.lastUserID = userID
	m.lastMessage = message
	return m.returnError
}

func (m *mockSlackMessenger) GetSlackUserIDByEmail(ctx context.Context, email string) (string, error) {
	return "", errors.New("not implemented in test mock")
}

func (m *mockSlackMessenger) GetEmailBySlackID(ctx context.Context, slackUserID string) (string, error) {
	return "", errors.New("not implemented in test mock")
}

func TestTestIntegration(t *testing.T) {
	t.Run("success - sends DM", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		// Create slack integration
		integration := &model.Integration{
			Type:    model.IntegrationTypeSlack,
			Name:    "Test Slack",
			Enabled: true,
			Config:  []byte(`{"token":"xoxb-test"}`),
		}
		s.CreateIntegration(integration)

		// Bind a Slack identity for denis (linked via the link flow).
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID: "denis", Provider: "slack", ExternalID: "U123",
		})

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d, body: %s", rec.Code, rec.Body.String())
		}

		if !mockSlack.sendDMCalled {
			t.Error("Expected SendDM to be called")
		}
		if mockSlack.lastUserID != "U123" {
			t.Errorf("Expected userID U123, got %s", mockSlack.lastUserID)
		}

		var resp TestIntegrationResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if !resp.OK {
			t.Error("Expected ok=true in response")
		}
	})

	t.Run("412 when user has no Slack ID", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		// Create slack integration
		integration := &model.Integration{
			Type:    model.IntegrationTypeSlack,
			Name:    "Test Slack",
			Enabled: true,
			Config:  []byte(`{"token":"xoxb-test"}`),
		}
		s.CreateIntegration(integration)

		// denis has no Slack identity bound - test-DM should respond 412.
		_ = s.UnbindExternalIdentity("denis", "slack")

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusPreconditionFailed {
			t.Errorf("Expected 412, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("500 when GetExternalIdentity returns a DB error (not 412)", func(t *testing.T) {
		// Regression guard: a storage outage must not masquerade as user misconfiguration.
		real := store.NewMockStore()
		s := &failingIdentityStore{StoreInterface: real, err: errors.New("transient DB error")}
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		integration := &model.Integration{
			Type:    model.IntegrationTypeSlack,
			Name:    "Test Slack",
			Enabled: true,
			Config:  []byte(`{"token":"xoxb-test"}`),
		}
		real.CreateIntegration(integration)

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("Expected 500 for DB error, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 for disabled integration", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		// Create disabled slack integration
		integration := &model.Integration{
			Type:    model.IntegrationTypeSlack,
			Name:    "Disabled Slack",
			Enabled: false,
			Config:  []byte(`{"token":"xoxb-test"}`),
		}
		s.CreateIntegration(integration)

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 for non-slack integration", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		// Create webhook integration
		integration := &model.Integration{
			Type:    model.IntegrationTypeAlertmanagerWebhook,
			Name:    "Test Webhook",
			Enabled: true,
			Config:  []byte(`{"secret":"test"}`),
		}
		s.CreateIntegration(integration)

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("400 for unsupported integration type", func(t *testing.T) {
		s := store.NewMockStore()
		api := NewAPI(s, nil, nil, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		integration := &model.Integration{
			Type:    model.IntegrationTypeAlertmanagerWebhook,
			Name:    "Test AM",
			Enabled: true,
			Config:  []byte(`{"secret":"s"}`),
		}
		s.CreateIntegration(integration)

		body := `{}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected 400, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("500 on Slack API error", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{returnError: errors.New("slack: invalid_auth")}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		// Create slack integration
		integration := &model.Integration{
			Type:    model.IntegrationTypeSlack,
			Name:    "Test Slack",
			Enabled: true,
			Config:  []byte(`{"token":"xoxb-test"}`),
		}
		s.CreateIntegration(integration)

		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID: "denis", Provider: "slack", ExternalID: "U123",
		})

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/"+integration.ID+"/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("Expected 500, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 for non-existent integration", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/non-existent/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 for non-admin on non-existent", func(t *testing.T) {
		s := store.NewMockStore()
		mockSlack := &mockSlackMessenger{}
		api := NewAPI(s, nil, mockSlack, nil, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		body := `{"mode":"dm"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/some-id/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alex") // non-admin
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})
}

// setupTeamWebhookEnv creates a test environment with:
// - team-a (alice=team_admin), team-b (charlie=team_admin), bob=team_member of team-a
// - global webhook (int-global), team-a webhook (int-team-a), team-b webhook (int-team-b)
func setupTeamWebhookEnv(t *testing.T) (*store.MockStore, *echo.Echo) {
	t.Helper()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	// Create teams
	s.CreateTeam(&model.Team{ID: "team-a", Name: "Team A"})
	s.CreateTeam(&model.Team{ID: "team-b", Name: "Team B"})

	// Create users
	s.CreateUser(&model.User{ID: "alice", Email: "alice@test.com", Role: model.UserRoleUser})
	s.CreateUser(&model.User{ID: "bob", Email: "bob@test.com", Role: model.UserRoleUser})
	s.CreateUser(&model.User{ID: "charlie", Email: "charlie@test.com", Role: model.UserRoleUser})

	// Team memberships
	s.AddTeamMember("team-a", "alice", model.TeamMemberRoleAdmin)
	s.AddTeamMember("team-a", "bob", model.TeamMemberRoleMember)
	s.AddTeamMember("team-b", "charlie", model.TeamMemberRoleAdmin)

	// Create integrations directly in store
	scopeGlobal := model.WebhookScopeGlobal
	scopeTeam := model.WebhookScopeTeam
	teamA := "team-a"
	teamB := "team-b"

	s.CreateIntegration(&model.Integration{
		Type:    model.IntegrationTypeGenericWebhook,
		Name:    "Global WH",
		Enabled: true,
		Scope:   &scopeGlobal,
		Config:  json.RawMessage(`{"url":"https://example.com/global","secret":"s"}`),
	})
	s.CreateIntegration(&model.Integration{
		Type:    model.IntegrationTypeGenericWebhook,
		Name:    "Team A WH",
		Enabled: true,
		Scope:   &scopeTeam,
		TeamID:  &teamA,
		Config:  json.RawMessage(`{"url":"https://example.com/a","secret":"s"}`),
	})
	s.CreateIntegration(&model.Integration{
		Type:    model.IntegrationTypeGenericWebhook,
		Name:    "Team B WH",
		Enabled: true,
		Scope:   &scopeTeam,
		TeamID:  &teamB,
		Config:  json.RawMessage(`{"url":"https://example.com/b","secret":"s"}`),
	})

	return s, e
}

func TestIntegrationRBAC_TeamAdmin(t *testing.T) {
	s, e := setupTeamWebhookEnv(t)

	// Find integration IDs
	all, _ := s.GetAllIntegrations()
	var globalID, teamAID, teamBID string
	for _, i := range all {
		if i.Scope != nil && *i.Scope == model.WebhookScopeGlobal {
			globalID = i.ID
		} else if i.TeamID != nil && *i.TeamID == "team-a" {
			teamAID = i.ID
		} else if i.TeamID != nil && *i.TeamID == "team-b" {
			teamBID = i.ID
		}
	}

	t.Run("admin lists all integrations", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		var resp IntegrationListResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Total != 3 {
			t.Errorf("Admin should see all 3 integrations, got %d", resp.Total)
		}
	})

	t.Run("team_admin lists only own team webhooks", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
		var resp IntegrationListResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Total != 1 {
			t.Errorf("team_admin alice should see 1 integration (team-a webhook), got %d", resp.Total)
		}
		if resp.Total == 1 && resp.Integrations[0].Name != "Team A WH" {
			t.Errorf("Expected Team A WH, got %s", resp.Integrations[0].Name)
		}
	})

	t.Run("team_member gets 403 on list", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
		addAuth(req, "bob")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403 for team_member, got %d", rec.Code)
		}
	})

	t.Run("team_admin gets own team webhook", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+teamAID, nil)
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d", rec.Code)
		}
	})

	t.Run("team_admin gets 404 for global webhook", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+globalID, nil)
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("team_admin gets 404 for other team webhook", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+teamBID, nil)
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected 404, got %d", rec.Code)
		}
	})

	t.Run("team_admin creates team webhook", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"New WH","scope":"team","team_id":"team-a","config":{"url":"https://example.com/new","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Errorf("Expected 201, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin cannot create global webhook", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"Global","scope":"global","config":{"url":"https://example.com/g","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin cannot create webhook for other team", func(t *testing.T) {
		body := `{"type":"generic_webhook","name":"Other","scope":"team","team_id":"team-b","config":{"url":"https://example.com/o","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin updates own team webhook", func(t *testing.T) {
		body := `{"name":"Updated WH"}`
		req := httptest.NewRequest(http.MethodPut, "/api/v1/integrations/"+teamAID, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("team_admin deletes own team webhook", func(t *testing.T) {
		// Create a disposable webhook for deletion
		scopeTeam := model.WebhookScopeTeam
		teamA := "team-a"
		s.CreateIntegration(&model.Integration{
			Type:    model.IntegrationTypeGenericWebhook,
			Name:    "Disposable",
			Enabled: true,
			Scope:   &scopeTeam,
			TeamID:  &teamA,
			Config:  json.RawMessage(`{"url":"https://example.com/del","secret":"s"}`),
		})

		// Find its ID
		allNow, _ := s.GetAllIntegrations()
		var disposableID string
		for _, i := range allNow {
			if i.Name == "Disposable" {
				disposableID = i.ID
				break
			}
		}

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/integrations/"+disposableID, nil)
		addAuth(req, "alice")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("Expected 204, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestIntegrationRBAC_BackendErrors(t *testing.T) {
	t.Run("store error on GetIntegrationByID returns 500", func(t *testing.T) {
		s, e := setupTeamWebhookEnv(t)

		all, _ := s.GetAllIntegrations()
		var teamAID string
		for _, i := range all {
			if i.TeamID != nil && *i.TeamID == "team-a" {
				teamAID = i.ID
				break
			}
		}

		s.GetIntegrationByIDError = errors.New("db connection lost")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+teamAID, nil)
		addAuth(req, "denis") // admin
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("Expected 500, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("IsAdmin error returns 500 on get", func(t *testing.T) {
		s, e := setupTeamWebhookEnv(t)

		all, _ := s.GetAllIntegrations()
		var teamAID string
		for _, i := range all {
			if i.TeamID != nil && *i.TeamID == "team-a" {
				teamAID = i.ID
				break
			}
		}

		s.GetUserByIDError = errors.New("user store unavailable")

		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/"+teamAID, nil)
		addAuth(req, "alice") // team_admin
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("Expected 500, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-admin create without team_id returns 403", func(t *testing.T) {
		_, e := setupTeamWebhookEnv(t)

		body := `{"type":"generic_webhook","name":"WH","scope":"team","config":{"url":"https://example.com/x","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alice") // team_admin, no team_id in body
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-admin create with empty team_id returns 403", func(t *testing.T) {
		_, e := setupTeamWebhookEnv(t)

		body := `{"type":"generic_webhook","name":"WH","scope":"team","team_id":"","config":{"url":"https://example.com/x","secret":"s"}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		addAuth(req, "alice") // team_admin, empty team_id
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d, body: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestSlackManifest(t *testing.T) {
	t.Run("returns YAML with selfURL", func(t *testing.T) {
		s := store.NewMockStore()
		api := NewAPI(s, nil, nil, nil, "https://tokayops.example.com", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/slack/manifest", nil)
		addAuth(req, "denis")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d, body: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "https://tokayops.example.com/slack/interactive") {
			t.Errorf("Expected manifest to contain selfURL-based interactive URL, got:\n%s", body)
		}
		if strings.Contains(body, "/slack/events") {
			t.Errorf("Manifest should not contain /slack/events (no handler exists), got:\n%s", body)
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/yaml") {
			t.Errorf("Expected Content-Type text/yaml, got %s", ct)
		}
	})

	t.Run("non-admin gets 403", func(t *testing.T) {
		s := store.NewMockStore()
		api := NewAPI(s, nil, nil, nil, "https://tokayops.example.com", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/slack/manifest", nil)
		addAuth(req, "alex")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("Expected 403, got %d", rec.Code)
		}
	})
}
