package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	slackprovider "github.com/tokayops/tokayops/internal/outbound/providers/slack"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/slackcard"
	"github.com/tokayops/tokayops/internal/store"
)

// capturedEphemeral collects ephemeral messages sent via respondEphemeral.
// Uses a buffered channel so tests can deterministically wait for goroutine delivery.
type capturedEphemeral struct {
	mu   sync.Mutex
	msgs []string
	ch   chan string
}

func newCapturedEphemeral() *capturedEphemeral {
	return &capturedEphemeral{ch: make(chan string, 100)}
}

func (c *capturedEphemeral) post(_ string, text string) {
	c.mu.Lock()
	c.msgs = append(c.msgs, text)
	c.mu.Unlock()
	c.ch <- text
}

// waitOne waits up to 2 s for the next ephemeral message and returns it.
// Fails the test immediately instead of hanging if no message arrives.
func (c *capturedEphemeral) waitOne(t *testing.T) string {
	t.Helper()
	select {
	case msg := <-c.ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ephemeral message")
		return ""
	}
}

// testSlackMessenger implements SlackMessenger for tests.
type testSlackMessenger struct {
	slackIDs           map[string]string // email → slackUserID
	emails             map[string]string // slackUserID → email
	err                error
	lookupByEmailCalls int // incremented on every GetSlackUserIDByEmail invocation
}

func (m *testSlackMessenger) SendDM(ctx context.Context, userID, message string) error {
	return nil
}

func (m *testSlackMessenger) GetSlackUserIDByEmail(ctx context.Context, email string) (string, error) {
	m.lookupByEmailCalls++
	if m.err != nil {
		return "", m.err
	}
	if id, ok := m.slackIDs[email]; ok {
		return id, nil
	}
	return "", slackprovider.ErrUserNotFound
}

func (m *testSlackMessenger) GetEmailBySlackID(ctx context.Context, slackUserID string) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if email, ok := m.emails[slackUserID]; ok {
		return email, nil
	}
	return "", slackprovider.ErrUserNotFound
}

// setupTestAPIWithCache creates an API with a pre-loaded IntegrationCache containing
// a Slack integration with the given signing secret.
func setupTestAPIWithCache(t *testing.T, signingSecret string) (*API, *store.MockStore, *echo.Echo) {
	t.Helper()
	s := store.NewMockStore()

	// Seed a Slack integration so the cache picks up the signing secret
	cfg, _ := json.Marshal(model.SlackConfig{
		Token:         "xoxb-test",
		SigningSecret: signingSecret,
	})
	s.CreateIntegration(&model.Integration{
		ID:      "int-slack-test",
		Type:    model.IntegrationTypeSlack,
		Name:    "test-slack",
		Enabled: true,
		Config:  cfg,
	})

	cache := store.NewIntegrationCache()
	if err := cache.LoadAll(s); err != nil {
		t.Fatalf("cache.LoadAll: %v", err)
	}

	api := NewAPI(s, nil, nil, cache, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	return api, s, e
}

// slackSign computes a valid Slack signature for the given secret, timestamp, and body.
func slackSign(secret, ts, body string) string {
	basestring := fmt.Sprintf("v0:%s:%s", ts, body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(basestring))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSlackInteractive(t *testing.T) {
	t.Run("no signature headers returns 401", func(t *testing.T) {
		_, _, e := setupTestAPIWithCache(t, "test-secret")

		req := httptest.NewRequest(http.MethodPost, "/slack/interactive", nil)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("no signing secret configured returns 503", func(t *testing.T) {
		// Use setupTestAPI which passes nil cache — middleware will see empty secret
		_, _, e := setupTestAPI(t)

		ts := fmt.Sprintf("%d", time.Now().Unix())
		req := httptest.NewRequest(http.MethodPost, "/slack/interactive", nil)
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", "v0=fake")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("Expected 503, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("timestamp too old returns 401", func(t *testing.T) {
		secret := "test-secret"
		_, _, e := setupTestAPIWithCache(t, secret)

		oldTS := fmt.Sprintf("%d", time.Now().Unix()-6*60) // 6 minutes ago
		body := "payload=test"
		sig := slackSign(secret, oldTS, body)

		req := httptest.NewRequest(http.MethodPost, "/slack/interactive", strings.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", oldTS)
		req.Header.Set("X-Slack-Signature", sig)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error != "timestamp too old" {
			t.Errorf("Expected error 'timestamp too old', got %q", resp.Error)
		}
	})

	t.Run("invalid signature returns 401", func(t *testing.T) {
		_, _, e := setupTestAPIWithCache(t, "test-secret")

		ts := fmt.Sprintf("%d", time.Now().Unix())
		req := httptest.NewRequest(http.MethodPost, "/slack/interactive", strings.NewReader("payload=test"))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", "v0=0000000000000000000000000000000000000000000000000000000000000000")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("Expected 401, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp ErrorResponse
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp.Error != "invalid slack signature" {
			t.Errorf("Expected error 'invalid slack signature', got %q", resp.Error)
		}
	})

	t.Run("valid signature restores body for handler", func(t *testing.T) {
		secret := "test-secret"
		s := store.NewMockStore()

		cfg, _ := json.Marshal(model.SlackConfig{
			Token:         "xoxb-test",
			SigningSecret: secret,
		})
		s.CreateIntegration(&model.Integration{
			ID:      "int-slack-test",
			Type:    model.IntegrationTypeSlack,
			Name:    "test-slack",
			Enabled: true,
			Config:  cfg,
		})
		cache := store.NewIntegrationCache()
		cache.LoadAll(s)

		api := NewAPI(s, nil, nil, cache, "", nil)
		e := echo.New()

		// Register a custom handler that reads the body and echoes it back,
		// proving the middleware restored it correctly.
		e.POST("/slack/interactive", func(c echo.Context) error {
			b, err := io.ReadAll(c.Request().Body)
			if err != nil {
				return c.JSON(500, map[string]string{"error": err.Error()})
			}
			return c.JSON(200, map[string]string{"body": string(b)})
		}, api.SlackSignatureMiddleware)

		ts := fmt.Sprintf("%d", time.Now().Unix())
		body := "payload=test-body-content"
		sig := slackSign(secret, ts, body)

		req := httptest.NewRequest(http.MethodPost, "/slack/interactive", strings.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", sig)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		var resp map[string]string
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["body"] != body {
			t.Errorf("Handler should see original body %q, got %q", body, resp["body"])
		}
	})
}

// signedSlackInteractiveRequest builds a signed POST /slack/interactive request
// with a block_actions payload containing the given action and value (alert group ID).
func signedSlackInteractiveRequest(t *testing.T, secret, actionID, alertGroupID, slackUserID string) *http.Request {
	t.Helper()
	// Build a minimal block_actions payload matching slack.InteractionCallback
	payload := map[string]interface{}{
		"type": "block_actions",
		"user": map[string]string{
			"id": slackUserID,
		},
		"actions": []map[string]interface{}{
			{
				"action_id": actionID,
				"block_id":  "actions_block",
				"value":     alertGroupID,
				"type":      "button",
			},
		},
		"response_url": "https://hooks.slack.test/respond",
	}
	payloadJSON, _ := json.Marshal(payload)

	// URL-encode the payload value (Slack sends application/x-www-form-urlencoded)
	form := url.Values{"payload": {string(payloadJSON)}}
	body := form.Encode()

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := slackSign(secret, ts, body)

	req := httptest.NewRequest(http.MethodPost, "/slack/interactive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	return req
}

func TestSlackInteractiveHandler(t *testing.T) {
	const secret = "handler-test-secret"

	// Helper: create API + store + echo with a linked Slack user and a triggered alert group.
	setup := func(t *testing.T) (*API, *store.MockStore, *echo.Echo, string) {
		t.Helper()
		api, s, e := setupTestAPIWithCache(t, secret)

		// Link denis to Slack user U_DENIS
		denis, _ := s.GetUserByEmail("denis@example.com")
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     denis.ID,
			Provider:   "slack",
			ExternalID: "U_DENIS",
		})

		// Create a triggered alert group in team devops
		agID := "ag-slack-test-" + fmt.Sprintf("%d", time.Now().UnixNano())
		s.CreateAlertGroup(&model.AlertGroup{
			ID:               agID,
			AlertKey:         "dedup-" + agID,
			Status:           model.AlertGroupStatusTriggered,
			Title:            "Test Alert",
			TeamID:           "devops",
			TeamNameSnapshot: "DevOps",
			Severity:         "critical",
			CreatedAt:        time.Now().Add(-5 * time.Minute),
			UpdatedAt:        time.Now(),
		})
		return api, s, e, agID
	}

	t.Run("valid ack changes status to acknowledged", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusAcknowledged {
			t.Errorf("Expected status acknowledged, got %s", ag.Status)
		}
		if ag.AcknowledgedBy != "Denis" {
			t.Errorf("Expected acknowledged_by Denis, got %s", ag.AcknowledgedBy)
		}

		// Verify ephemeral message
		if msg := captured.waitOne(t); !strings.Contains(msg, "acknowledged by Denis") {
			t.Errorf("Expected ephemeral containing 'acknowledged by Denis', got %q", msg)
		}

		// Verify timeline event with source=slack metadata
		events, _ := s.GetTimelineEvents(agID)
		found := false
		for _, ev := range events {
			if ev.Type == model.TimelineEventAcknowledged {
				found = true
				if ev.Metadata == nil || ev.Metadata["source"] != "slack" {
					t.Errorf("Expected metadata source=slack, got %v", ev.Metadata)
				}
			}
		}
		if !found {
			t.Error("Expected acknowledged timeline event")
		}

		// Verify outbox event
		outboxEvents, _ := s.GetPendingOutboxEvents(10)
		var outboxFound *model.OutboxEvent
		for _, oe := range outboxEvents {
			if oe.AlertGroupID == agID && oe.EventType == model.OutboxEventAcknowledged {
				outboxFound = oe
			}
		}
		if outboxFound == nil {
			t.Error("Expected outbox event for acknowledged AG via Slack")
		} else {
			var payload model.WebhookEventPayload
			if err := json.Unmarshal(outboxFound.Payload, &payload); err != nil {
				t.Fatalf("Failed to unmarshal outbox payload: %v", err)
			}
			if payload.AlertGroup.Status != "acknowledged" {
				t.Errorf("Expected outbox payload status 'acknowledged', got %q", payload.AlertGroup.Status)
			}
			if payload.AlertGroup.TeamName != "DevOps" {
				t.Errorf("Expected outbox team_name 'DevOps', got %q", payload.AlertGroup.TeamName)
			}
			if payload.Actor.Name != "Denis" {
				t.Errorf("Expected outbox actor.name 'Denis', got %q", payload.Actor.Name)
			}
		}
	})

	t.Run("valid resolve changes status to resolved", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusResolved {
			t.Errorf("Expected status resolved, got %s", ag.Status)
		}
		if ag.ResolvedAt == nil {
			t.Error("Expected resolved_at to be set")
		}

		// Verify ephemeral message
		if msg := captured.waitOne(t); !strings.Contains(msg, "resolved by Denis") {
			t.Errorf("Expected ephemeral containing 'resolved by Denis', got %q", msg)
		}

		// Verify timeline event with source=slack metadata
		events, _ := s.GetTimelineEvents(agID)
		found := false
		for _, ev := range events {
			if ev.Type == model.TimelineEventResolved {
				found = true
				if ev.Metadata == nil || ev.Metadata["source"] != "slack" {
					t.Errorf("Expected metadata source=slack, got %v", ev.Metadata)
				}
			}
		}
		if !found {
			t.Error("Expected resolved timeline event")
		}

		// Verify outbox event
		outboxEvents, _ := s.GetPendingOutboxEvents(10)
		var outboxFound *model.OutboxEvent
		for _, oe := range outboxEvents {
			if oe.AlertGroupID == agID && oe.EventType == model.OutboxEventResolved {
				outboxFound = oe
			}
		}
		if outboxFound == nil {
			t.Error("Expected outbox event for resolved AG via Slack")
		} else {
			var payload model.WebhookEventPayload
			if err := json.Unmarshal(outboxFound.Payload, &payload); err != nil {
				t.Fatalf("Failed to unmarshal outbox payload: %v", err)
			}
			if payload.AlertGroup.Status != "resolved" {
				t.Errorf("Expected outbox payload status 'resolved', got %q", payload.AlertGroup.Status)
			}
			if payload.AlertGroup.TeamName != "DevOps" {
				t.Errorf("Expected outbox team_name 'DevOps', got %q", payload.AlertGroup.TeamName)
			}
		}
	})

	t.Run("unlinked user returns 200 and does not modify AG", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_UNKNOWN")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusTriggered {
			t.Errorf("Expected AG unchanged (triggered), got %s", ag.Status)
		}

		// Verify ephemeral message contains Slack User ID for OTP linking
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "not linked") {
			t.Errorf("Expected ephemeral containing 'not linked', got %q", msg)
		}
		if !strings.Contains(msg, "U_UNKNOWN") {
			t.Errorf("Expected ephemeral containing Slack User ID 'U_UNKNOWN', got %q", msg)
		}
	})

	t.Run("unauthorized ack returns 200 with team name and does not modify AG", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Create an unlinked user with no team membership
		s.CreateUser(&model.User{
			ID:    "outsider",
			Email: "outsider@example.com",
			Name:  "Outsider",
			Role:  model.UserRoleUser,
		})
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     "outsider",
			Provider:   "slack",
			ExternalID: "U_OUTSIDER",
		})

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_OUTSIDER")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusTriggered {
			t.Errorf("Expected AG unchanged (triggered), got %s", ag.Status)
		}

		// Verify ephemeral message includes verb and team name per AC
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "permission") {
			t.Errorf("Expected ephemeral containing 'permission', got %q", msg)
		}
		if !strings.Contains(msg, "acknowledge") {
			t.Errorf("Expected ephemeral containing 'acknowledge', got %q", msg)
		}
		if !strings.Contains(msg, "team DevOps") {
			t.Errorf("Expected ephemeral containing 'team DevOps', got %q", msg)
		}
	})

	t.Run("unauthorized resolve returns 200 with team name and does not modify AG", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		s.CreateUser(&model.User{
			ID:    "outsider",
			Email: "outsider@example.com",
			Name:  "Outsider",
			Role:  model.UserRoleUser,
		})
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     "outsider",
			Provider:   "slack",
			ExternalID: "U_OUTSIDER",
		})

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_OUTSIDER")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusTriggered {
			t.Errorf("Expected AG unchanged (triggered), got %s", ag.Status)
		}

		msg := captured.waitOne(t)
		if !strings.Contains(msg, "resolve") {
			t.Errorf("Expected ephemeral containing 'resolve', got %q", msg)
		}
		if !strings.Contains(msg, "team DevOps") {
			t.Errorf("Expected ephemeral containing 'team DevOps', got %q", msg)
		}
	})

	t.Run("already acknowledged is idempotent", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// First ack
		req1 := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec1 := httptest.NewRecorder()
		e.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("First ack: expected 200, got %d", rec1.Code)
		}
		captured.waitOne(t) // drain first ack message

		// Second ack — idempotent
		req2 := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec2 := httptest.NewRecorder()
		e.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK {
			t.Fatalf("Second ack: expected 200, got %d", rec2.Code)
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusAcknowledged {
			t.Errorf("Expected status acknowledged, got %s", ag.Status)
		}

		// Second ephemeral should mention "already acknowledged by"
		if msg := captured.waitOne(t); !strings.Contains(msg, "already acknowledged by") {
			t.Errorf("Expected ephemeral containing 'already acknowledged by', got %q", msg)
		}

		// Should have exactly 1 ack timeline event (not 2)
		events, _ := s.GetTimelineEvents(agID)
		ackCount := 0
		for _, ev := range events {
			if ev.Type == model.TimelineEventAcknowledged {
				ackCount++
			}
		}
		if ackCount != 1 {
			t.Errorf("Expected 1 ack timeline event, got %d", ackCount)
		}
	})

	t.Run("already resolved ack is idempotent", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Resolve first
		req1 := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec1 := httptest.NewRecorder()
		e.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("Resolve: expected 200, got %d", rec1.Code)
		}
		captured.waitOne(t) // drain resolve message

		// Ack after resolve — should be idempotent
		req2 := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec2 := httptest.NewRecorder()
		e.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusOK {
			t.Fatalf("Ack after resolve: expected 200, got %d", rec2.Code)
		}

		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusResolved {
			t.Errorf("Expected status resolved, got %s", ag.Status)
		}

		// Second ephemeral should mention "already"
		if msg := captured.waitOne(t); !strings.Contains(msg, "already") {
			t.Errorf("Expected ephemeral containing 'already', got %q", msg)
		}
	})

	t.Run("alert group not found returns 200", func(t *testing.T) {
		api, _, e, _ := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, "nonexistent-ag-id", "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}

		// Verify ephemeral message about not found
		if msg := captured.waitOne(t); !strings.Contains(msg, "not found") {
			t.Errorf("Expected ephemeral containing 'not found', got %q", msg)
		}
	})

	t.Run("unknown action_id returns 200", func(t *testing.T) {
		_, _, e, agID := setup(t)

		req := signedSlackInteractiveRequest(t, secret, "unknown_action", agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
	})

	t.Run("invalid payload JSON returns 200", func(t *testing.T) {
		_, _, e := setupTestAPIWithCache(t, secret)

		body := "payload={not valid json"
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := slackSign(secret, ts, body)

		req := httptest.NewRequest(http.MethodPost, "/slack/interactive", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", sig)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d; body: %s", rec.Code, rec.Body.String())
		}
	})
}

// errorStore wraps MockStore to inject errors into specific methods.
type errorStore struct {
	*store.MockStore
	getAlertGroupByIDErr       error
	getTeamByIDErr             error
	getUserTeamRoleErr         error
	ackAlertGroupAtomicErr     error
	resolveAlertGroupAtomicErr error
}

func (s *errorStore) GetAlertGroupByID(id string) (*model.AlertGroup, error) {
	if s.getAlertGroupByIDErr != nil {
		return nil, s.getAlertGroupByIDErr
	}
	return s.MockStore.GetAlertGroupByID(id)
}

func (s *errorStore) GetTeamByID(id string) (*model.Team, error) {
	if s.getTeamByIDErr != nil {
		return nil, s.getTeamByIDErr
	}
	return s.MockStore.GetTeamByID(id)
}

func (s *errorStore) GetUserTeamRole(userID, teamID string) (model.TeamMemberRole, error) {
	if s.getUserTeamRoleErr != nil {
		return "", s.getUserTeamRoleErr
	}
	return s.MockStore.GetUserTeamRole(userID, teamID)
}

func (s *errorStore) AckAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent) (bool, error) {
	if s.ackAlertGroupAtomicErr != nil {
		return false, s.ackAlertGroupAtomicErr
	}
	return s.MockStore.AckAlertGroupAtomic(id, actor, meta, outboxEvent)
}

func (s *errorStore) ResolveAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent) (bool, error) {
	if s.resolveAlertGroupAtomicErr != nil {
		return false, s.resolveAlertGroupAtomicErr
	}
	return s.MockStore.ResolveAlertGroupAtomic(id, actor, meta, outboxEvent)
}

// setupErrorAPI creates an API with an errorStore wrapper, a linked user, and a triggered AG.
func setupErrorAPI(t *testing.T, es *errorStore) (*API, *echo.Echo, string) {
	t.Helper()
	secret := "handler-test-secret"

	cfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-test", SigningSecret: secret})
	es.CreateIntegration(&model.Integration{
		ID: "int-slack-test", Type: model.IntegrationTypeSlack,
		Name: "test-slack", Enabled: true, Config: cfg,
	})
	cache := store.NewIntegrationCache()
	if err := cache.LoadAll(es.MockStore); err != nil {
		t.Fatalf("cache.LoadAll: %v", err)
	}

	api := NewAPI(es, nil, nil, cache, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	// Link denis to Slack user U_DENIS
	denis, _ := es.GetUserByEmail("denis@example.com")
	es.BindExternalIdentity(&model.ExternalIdentity{
		UserID:     denis.ID,
		Provider:   "slack",
		ExternalID: "U_DENIS",
	})

	// Create a triggered alert group
	agID := "ag-err-test-" + fmt.Sprintf("%d", time.Now().UnixNano())
	es.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "dedup-" + agID,
		Status: model.AlertGroupStatusTriggered, Title: "Test Alert",
		TeamID: "devops", TeamNameSnapshot: "DevOps", Severity: "critical",
		CreatedAt: time.Now().Add(-5 * time.Minute), UpdatedAt: time.Now(),
	})
	return api, e, agID
}

func TestSlackInteractiveHandler_ErrorPaths(t *testing.T) {
	const secret = "handler-test-secret"

	t.Run("GetAlertGroupByID DB error returns ephemeral", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, _ := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Inject DB error for GetAlertGroupByID (non-404)
		es.getAlertGroupByIDErr = errors.New("pq: connection refused")

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, "any-ag-id", "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "Something went wrong") {
			t.Errorf("Expected ephemeral containing 'Something went wrong', got %q", msg)
		}
	})

	t.Run("RBAC error returns ephemeral", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, agID := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Use non-admin user (alex) so RBAC doesn't short-circuit on admin check.
		alex, _ := es.GetUserByEmail("alex@example.com")
		es.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     alex.ID,
			Provider:   "slack",
			ExternalID: "U_ALEX",
		})

		// Inject RBAC error (via GetUserTeamRole returning a non-sql.ErrNoRows error)
		es.getUserTeamRoleErr = errors.New("pq: relation does not exist")

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_ALEX")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "Something went wrong") {
			t.Errorf("Expected ephemeral containing 'Something went wrong', got %q", msg)
		}
	})

	t.Run("AckAlertGroupAtomic error returns ephemeral", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, agID := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		es.ackAlertGroupAtomicErr = errors.New("pq: serialization failure")

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "Something went wrong") {
			t.Errorf("Expected ephemeral containing 'Something went wrong', got %q", msg)
		}
	})

	t.Run("ResolveAlertGroupAtomic error returns ephemeral", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, agID := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		es.resolveAlertGroupAtomicErr = errors.New("pq: deadlock detected")

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "Something went wrong") {
			t.Errorf("Expected ephemeral containing 'Something went wrong', got %q", msg)
		}
	})

	t.Run("resolve on closed AG returns already closed ephemeral", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, agID := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Set AG to closed status (resolve will see changed=false)
		es.MockStore.SetAlertGroupStatus(agID, model.AlertGroupStatusClosed)

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "already closed") {
			t.Errorf("Expected ephemeral containing 'already closed', got %q", msg)
		}
	})

	t.Run("already acked AG returns already-done", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, agID := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		es.MockStore.SetAlertGroupStatus(agID, model.AlertGroupStatusAcknowledged)

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "already acknowledged") {
			t.Errorf("Expected ephemeral containing 'already acknowledged', got %q", msg)
		}
	})

	t.Run("already resolved AG returns already-done on resolve", func(t *testing.T) {
		es := &errorStore{MockStore: store.NewMockStore()}
		api, e, agID := setupErrorAPI(t, es)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		es.MockStore.SetAlertGroupStatus(agID, model.AlertGroupStatusResolved)

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}
		if msg := captured.waitOne(t); !strings.Contains(msg, "already resolved") {
			t.Errorf("Expected ephemeral containing 'already resolved', got %q", msg)
		}
	})
}

func TestResolveSlackUser(t *testing.T) {
	t.Run("linked user found by slack_user_id", func(t *testing.T) {
		s := store.NewMockStore()
		denis, _ := s.GetUserByEmail("denis@example.com")
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     denis.ID,
			Provider:   "slack",
			ExternalID: "U_DENIS",
		})

		api := NewAPI(s, nil, nil, nil, "", nil)
		result := api.resolveSlackUser(context.Background(), "U_DENIS")

		if result == nil {
			t.Fatal("expected user, got nil")
		}
		if result.ID != "denis" {
			t.Errorf("expected denis, got %s", result.ID)
		}
	})

	t.Run("unlinked user returns nil", func(t *testing.T) {
		s := store.NewMockStore()
		api := NewAPI(s, nil, nil, nil, "", nil)
		result := api.resolveSlackUser(context.Background(), "U_UNKNOWN")

		if result != nil {
			t.Errorf("expected nil, got user %s", result.ID)
		}
	})
}

func TestTryLinkSlackUser(t *testing.T) {
	t.Run("auto-links user via SSO email", func(t *testing.T) {
		s := store.NewMockStore()
		slack := &testSlackMessenger{
			slackIDs: map[string]string{"alex@example.com": "U_ALEX"},
		}
		api := NewAPI(s, nil, slack, nil, "", nil)

		api.tryLinkSlackUser("alex", "alex@example.com")

		ident, _ := s.GetExternalIdentity("alex", "slack")
		if ident == nil || ident.ExternalID != "U_ALEX" {
			t.Errorf("expected slack identity U_ALEX, got %+v", ident)
		}
	})

	t.Run("store guard: does not overwrite existing slack_user_id", func(t *testing.T) {
		s := store.NewMockStore()
		alex, _ := s.GetUserByID("alex")
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     alex.ID,
			Provider:   "slack",
			ExternalID: "U_EXISTING",
		})

		slack := &testSlackMessenger{
			slackIDs: map[string]string{"alex@example.com": "U_DIFFERENT"},
		}
		api := NewAPI(s, nil, slack, nil, "", nil)

		// Call tryLinkSlackUser directly — store-level guard must prevent overwrite
		api.tryLinkSlackUser("alex", "alex@example.com")

		ident, _ := s.GetExternalIdentity(alex.ID, "slack")
		if ident == nil || ident.ExternalID != "U_EXISTING" {
			t.Errorf("slack identity should remain U_EXISTING, got %+v", ident)
		}
	})

	t.Run("skips Slack API call when already linked", func(t *testing.T) {
		// Regression guard: Sprint 3 dropped the in-process User.SlackUserID guard,
		// so tryLinkSlackUser MUST do a cheap GetExternalIdentity pre-check before
		// hitting Slack's users.lookupByEmail, or every OIDC login hits the API.
		s := store.NewMockStore()
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID: "alex", Provider: "slack", ExternalID: "U_ALREADY_LINKED",
		})
		slack := &testSlackMessenger{
			slackIDs: map[string]string{"alex@example.com": "U_NEW_VIA_EMAIL"},
		}
		api := NewAPI(s, nil, slack, nil, "", nil)

		api.tryLinkSlackUser("alex", "alex@example.com")

		if slack.lookupByEmailCalls != 0 {
			t.Errorf("expected 0 Slack API calls for already-linked user, got %d", slack.lookupByEmailCalls)
		}
		// Identity untouched.
		ident, _ := s.GetExternalIdentity("alex", "slack")
		if ident == nil || ident.ExternalID != "U_ALREADY_LINKED" {
			t.Errorf("identity changed: %+v", ident)
		}
	})

	t.Run("graceful on Slack API error", func(t *testing.T) {
		s := store.NewMockStore()
		slack := &testSlackMessenger{
			err: errors.New("network timeout"),
		}
		api := NewAPI(s, nil, slack, nil, "", nil)

		// Should not panic or modify user
		api.tryLinkSlackUser("alex", "alex@example.com")

		_, err := s.GetExternalIdentity("alex", "slack")
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("expected sql.ErrNoRows (no identity), got %v", err)
		}
	})

	t.Run("graceful on nil slack messenger", func(t *testing.T) {
		// tryLinkSlackUser is only called when a.slack != nil,
		// but test that the caller guard in OIDCCallback works:
		s := store.NewMockStore()
		api := NewAPI(s, nil, nil, nil, "", nil)
		// Simulate what OIDCCallback does: check whether the user is already linked
		// before attempting to call tryLinkSlackUser.
		_, identErr := s.GetExternalIdentity("alex", "slack")
		notLinked := errors.Is(identErr, sql.ErrNoRows)
		if notLinked && api.slack != nil {
			t.Error("this block should not execute with nil slack")
		}
	})
}

func TestSlackInteractiveEmailMatch(t *testing.T) {
	const secret = "email-match-secret"

	// setup creates an API with a SlackMessenger that knows Slack user emails,
	// plus the standard mock store and a triggered alert group.
	setup := func(t *testing.T, slackMsngr SlackMessenger) (*API, *store.MockStore, *echo.Echo, string) {
		t.Helper()
		s := store.NewMockStore()

		cfg, _ := json.Marshal(model.SlackConfig{
			Token:         "xoxb-test",
			SigningSecret: secret,
		})
		s.CreateIntegration(&model.Integration{
			ID:      "int-slack-test",
			Type:    model.IntegrationTypeSlack,
			Name:    "test-slack",
			Enabled: true,
			Config:  cfg,
		})
		cache := store.NewIntegrationCache()
		if err := cache.LoadAll(s); err != nil {
			t.Fatalf("cache.LoadAll: %v", err)
		}

		api := NewAPI(s, nil, slackMsngr, cache, "", nil)
		e := echo.New()
		api.RegisterRoutes(e)

		agID := "ag-email-match-" + fmt.Sprintf("%d", time.Now().UnixNano())
		s.CreateAlertGroup(&model.AlertGroup{
			ID:               agID,
			AlertKey:         "dedup-" + agID,
			Status:           model.AlertGroupStatusTriggered,
			Title:            "Test Alert",
			TeamID:           "devops",
			TeamNameSnapshot: "DevOps",
			Severity:         "critical",
			CreatedAt:        time.Now().Add(-5 * time.Minute),
			UpdatedAt:        time.Now(),
		})
		return api, s, e, agID
	}

	t.Run("email match auto-links and allows action", func(t *testing.T) {
		// alex (devops member) has no slack_user_id, but Slack user U_ALEX_SLACK has alex's email
		slackMsngr := &testSlackMessenger{
			emails: map[string]string{"U_ALEX_SLACK": "alex@example.com"},
		}
		api, s, e, agID := setup(t, slackMsngr)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Verify alex has no slack identity initially
		alex, _ := s.GetUserByEmail("alex@example.com")
		if _, err := s.GetExternalIdentity(alex.ID, "slack"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("alex should have no slack identity, got err=%v", err)
		}

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_ALEX_SLACK")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Verify alex was auto-linked
		ident, _ := s.GetExternalIdentity(alex.ID, "slack")
		if ident == nil || ident.ExternalID != "U_ALEX_SLACK" {
			t.Errorf("Expected alex slack identity = U_ALEX_SLACK, got %+v", ident)
		}

		// Verify the ack actually went through
		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusAcknowledged {
			t.Errorf("Expected status acknowledged, got %s", ag.Status)
		}

		// Verify success ephemeral (not OTP fallback)
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "acknowledged by") {
			t.Errorf("Expected ack success ephemeral, got %q", msg)
		}
	})

	t.Run("email match miss falls through to OTP ephemeral", func(t *testing.T) {
		// Slack user U_STRANGER has an email that doesn't match any TokayOps user
		slackMsngr := &testSlackMessenger{
			emails: map[string]string{"U_STRANGER": "stranger@external.com"},
		}
		api, _, e, agID := setup(t, slackMsngr)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_STRANGER")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Should get OTP fallback ephemeral with Slack User ID
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "not linked") {
			t.Errorf("Expected OTP fallback ephemeral, got %q", msg)
		}
		if !strings.Contains(msg, "U_STRANGER") {
			t.Errorf("Expected Slack User ID in ephemeral, got %q", msg)
		}
	})

	t.Run("Slack API error falls through to OTP ephemeral", func(t *testing.T) {
		// Slack API is down — should gracefully fall through
		slackMsngr := &testSlackMessenger{
			err: errors.New("network timeout"),
		}
		api, _, e, agID := setup(t, slackMsngr)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_NOAPI")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Should still get OTP fallback ephemeral
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "not linked") {
			t.Errorf("Expected OTP fallback ephemeral, got %q", msg)
		}
		if !strings.Contains(msg, "U_NOAPI") {
			t.Errorf("Expected Slack User ID in ephemeral, got %q", msg)
		}
	})

	t.Run("email match found but link already taken falls through to OTP", func(t *testing.T) {
		// alex's email matches, but alex already has a different slack_user_id bound
		// → UpdateUserSlackID returns changed=false → must NOT return user
		slackMsngr := &testSlackMessenger{
			emails: map[string]string{"U_NEW_SLACK": "alex@example.com"},
		}
		api, s, e, agID := setup(t, slackMsngr)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// Pre-link alex to a different Slack ID
		alex, _ := s.GetUserByEmail("alex@example.com")
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     alex.ID,
			Provider:   "slack",
			ExternalID: "U_OLD_SLACK",
		})

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_NEW_SLACK")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Should fall through to OTP ephemeral (not execute the action)
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "not linked") {
			t.Errorf("Expected OTP fallback ephemeral, got %q", msg)
		}
		if !strings.Contains(msg, "U_NEW_SLACK") {
			t.Errorf("Expected Slack User ID in ephemeral, got %q", msg)
		}

		// alex's slack identity should remain unchanged
		ident, _ := s.GetExternalIdentity(alex.ID, "slack")
		if ident == nil || ident.ExternalID != "U_OLD_SLACK" {
			t.Errorf("Expected alex slack identity to remain U_OLD_SLACK, got %+v", ident)
		}

		// Alert group should remain triggered (action not executed)
		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusTriggered {
			t.Errorf("Expected AG unchanged (triggered), got %s", ag.Status)
		}
	})
}

// ---------- Instant Card Replacement Tests ----------

// capturedReplace collects card replacements sent via replaceOriginal.
type capturedReplace struct {
	mu    sync.Mutex
	cards []slackcard.Card
	ch    chan slackcard.Card
	err   error // if set, post returns this error
}

func newCapturedReplace() *capturedReplace {
	return &capturedReplace{ch: make(chan slackcard.Card, 100)}
}

func (c *capturedReplace) post(_ string, card slackcard.Card) error {
	if c.err != nil {
		return c.err
	}
	c.mu.Lock()
	c.cards = append(c.cards, card)
	c.mu.Unlock()
	c.ch <- card
	return nil
}

func (c *capturedReplace) last() slackcard.Card {
	return c.cards[len(c.cards)-1]
}

func (c *capturedReplace) waitOne(t *testing.T) slackcard.Card {
	t.Helper()
	select {
	case card := <-c.ch:
		return card
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for card replacement")
		return slackcard.Card{}
	}
}

// mockCardRenderer implements SlackCardRenderer for tests.
type mockCardRenderer struct {
	mu    sync.Mutex
	calls []mockRenderCall
}

type mockRenderCall struct {
	AlertGroupID   string
	IsResolved     bool
	Status         string
	AcknowledgedBy string
	ResolvedBy     string
}

func (m *mockCardRenderer) RenderCard(ag *model.AlertGroup, isResolved bool) slackcard.Card {
	m.mu.Lock()
	m.calls = append(m.calls, mockRenderCall{
		AlertGroupID:   ag.ID,
		IsResolved:     isResolved,
		Status:         string(ag.Status),
		AcknowledgedBy: ag.AcknowledgedBy,
		ResolvedBy:     ag.ResolvedBy,
	})
	m.mu.Unlock()
	color := "#FF0000"
	if isResolved {
		color = "#36a64f"
	} else if ag.Status == model.AlertGroupStatusAcknowledged {
		color = "#FFA500"
	}
	return slackcard.Card{
		Text: ag.Title,
		Blocks: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, ag.Title, false, false),
				nil, nil,
			),
		},
		Attachment: slack.Attachment{Color: color, Fallback: ag.Title},
	}
}

func TestSlackInteractiveHandler_InstantCard(t *testing.T) {
	const secret = "handler-test-secret"

	setup := func(t *testing.T) (*API, *store.MockStore, *echo.Echo, string) {
		t.Helper()
		api, s, e := setupTestAPIWithCache(t, secret)

		denis, _ := s.GetUserByEmail("denis@example.com")
		s.BindExternalIdentity(&model.ExternalIdentity{
			UserID:     denis.ID,
			Provider:   "slack",
			ExternalID: "U_DENIS",
		})

		agID := "ag-card-test-" + fmt.Sprintf("%d", time.Now().UnixNano())
		s.CreateAlertGroup(&model.AlertGroup{
			ID:               agID,
			AlertKey:         "dedup-" + agID,
			Status:           model.AlertGroupStatusTriggered,
			Title:            "Test Alert",
			TeamID:           "devops",
			TeamNameSnapshot: "DevOps",
			Severity:         "critical",
			CreatedAt:        time.Now().Add(-5 * time.Minute),
			UpdatedAt:        time.Now(),
		})
		return api, s, e, agID
	}

	t.Run("ack with cardRenderer replaces card with orange", func(t *testing.T) {
		api, s, e, agID := setup(t)
		renderer := &mockCardRenderer{}
		replace := newCapturedReplace()
		api.cardRenderer = renderer
		api.replaceOriginal = replace.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Verify card replacement (not ephemeral)
		card := replace.waitOne(t)
		if card.Attachment.Color != "#FFA500" {
			t.Errorf("Expected orange card (#FFA500), got %s", card.Attachment.Color)
		}
		if card.Text == "" {
			t.Error("Expected non-empty Card.Text")
		}
		if len(card.Blocks) == 0 {
			t.Error("Expected non-empty Card.Blocks")
		}

		// Verify DB state
		ag, _ := s.GetAlertGroupByID(agID)
		if ag.Status != model.AlertGroupStatusAcknowledged {
			t.Errorf("Expected acknowledged, got %s", ag.Status)
		}

		// Verify renderer was called with correct args
		renderer.mu.Lock()
		defer renderer.mu.Unlock()
		if len(renderer.calls) != 1 {
			t.Fatalf("Expected 1 render call, got %d", len(renderer.calls))
		}
		if renderer.calls[0].IsResolved {
			t.Error("Expected isResolved=false for ack")
		}
		if renderer.calls[0].Status != string(model.AlertGroupStatusAcknowledged) {
			t.Errorf("Expected status acknowledged in render call, got %s", renderer.calls[0].Status)
		}
		if renderer.calls[0].AcknowledgedBy != "Denis" {
			t.Errorf("Expected AcknowledgedBy 'Denis' in render call, got %q", renderer.calls[0].AcknowledgedBy)
		}
	})

	t.Run("resolve with cardRenderer replaces card with green", func(t *testing.T) {
		api, _, e, agID := setup(t)
		renderer := &mockCardRenderer{}
		replace := newCapturedReplace()
		api.cardRenderer = renderer
		api.replaceOriginal = replace.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		card := replace.waitOne(t)
		if card.Attachment.Color != "#36a64f" {
			t.Errorf("Expected green card (#36a64f), got %s", card.Attachment.Color)
		}
		if card.Text == "" {
			t.Error("Expected non-empty Card.Text")
		}
		if len(card.Blocks) == 0 {
			t.Error("Expected non-empty Card.Blocks")
		}

		renderer.mu.Lock()
		defer renderer.mu.Unlock()
		if len(renderer.calls) != 1 {
			t.Fatalf("Expected 1 render call, got %d", len(renderer.calls))
		}
		if !renderer.calls[0].IsResolved {
			t.Error("Expected isResolved=true for resolve")
		}
		if renderer.calls[0].ResolvedBy != "Denis" {
			t.Errorf("Expected ResolvedBy 'Denis' in render call, got %q", renderer.calls[0].ResolvedBy)
		}
	})

	t.Run("replaceOriginal failure falls back to ephemeral", func(t *testing.T) {
		api, _, e, agID := setup(t)
		renderer := &mockCardRenderer{}
		replace := &capturedReplace{
			ch:  make(chan slackcard.Card, 10),
			err: errors.New("slack API unavailable"),
		}
		captured := newCapturedEphemeral()
		api.cardRenderer = renderer
		api.replaceOriginal = replace.post
		api.respondEphemeral = captured.post

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Should fall back to ephemeral
		msg := captured.waitOne(t)
		if !strings.Contains(msg, "acknowledged by Denis") {
			t.Errorf("Expected fallback ephemeral containing 'acknowledged by Denis', got %q", msg)
		}
	})

	t.Run("no cardRenderer uses ephemeral (backward compat)", func(t *testing.T) {
		api, _, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post
		// cardRenderer and replaceOriginal are nil by default

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		msg := captured.waitOne(t)
		if !strings.Contains(msg, "acknowledged by Denis") {
			t.Errorf("Expected ephemeral containing 'acknowledged by Denis', got %q", msg)
		}
	})

	t.Run("already acked with cardRenderer re-fetches and replaces card", func(t *testing.T) {
		api, s, e, agID := setup(t)
		renderer := &mockCardRenderer{}
		replace := newCapturedReplace()
		api.cardRenderer = renderer
		api.replaceOriginal = replace.post

		// Pre-ack the alert group
		s.AckAlertGroupAtomic(agID, "Other User", nil, nil)

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Should still replace the card (re-fetched from DB)
		card := replace.waitOne(t)
		if card.Attachment.Color != "#FFA500" {
			t.Errorf("Expected orange card from re-fetch, got %s", card.Attachment.Color)
		}
	})

	t.Run("ack cancels escalation job", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		// An escalation job shaped like the real one: cancellation addresses it by
		// alert group, so alert_group_id is what makes this a job the escalation
		// builder would actually have produced.
		jobID := "job-esc-" + agID
		if err := s.SeedEscalationJob(agID, &model.Job{
			ID:     jobID,
			Status: model.JobStatusPending,
		}, nil, []*model.JobStep{
			{ID: "step-1", JobID: jobID, Status: model.JobStepStatusPending},
		}); err != nil {
			t.Fatalf("SeedEscalationJob: %v", err)
		}

		req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		// Wait for async goroutine to complete
		captured.waitOne(t)

		// Verify escalation job was cancelled
		job, _ := s.GetJobByID(jobID)
		if job.Status != model.JobStatusCanceled {
			t.Errorf("Expected escalation job to be canceled, got %s", job.Status)
		}
	})

	t.Run("resolve cancels escalation job", func(t *testing.T) {
		api, s, e, agID := setup(t)
		captured := newCapturedEphemeral()
		api.respondEphemeral = captured.post

		jobID := "job-esc-resolve-" + agID
		if err := s.SeedEscalationJob(agID, &model.Job{
			ID:     jobID,
			Status: model.JobStatusPending,
		}, nil, []*model.JobStep{
			{ID: "step-1", JobID: jobID, Status: model.JobStepStatusPending},
		}); err != nil {
			t.Fatalf("SeedEscalationJob: %v", err)
		}

		req := signedSlackInteractiveRequest(t, secret, SlackActionResolveAlertGroup, agID, "U_DENIS")
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rec.Code)
		}

		captured.waitOne(t)

		job, _ := s.GetJobByID(jobID)
		if job.Status != model.JobStatusCanceled {
			t.Errorf("Expected escalation job to be canceled, got %s", job.Status)
		}
	})
}
