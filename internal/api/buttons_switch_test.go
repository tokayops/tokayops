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

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// A press reads the switch from the database, not from this instance's cache:
// the instance that handled the change is the only one whose cache knows
// about it. Both tests leave the cache deliberately stale.

func TestASlackPressIsRefusedWhenTheSwitchIsOffInTheDatabase(t *testing.T) {
	const secret = "switch-test-secret"
	api, s, e := setupTestAPIWithCache(t, secret)
	denis, _ := s.GetUserByEmail("denis@example.com")
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: denis.ID, Provider: "slack", ExternalID: "U_DENIS"})
	agID := "ag-switch-" + fmt.Sprintf("%d", time.Now().UnixNano())
	s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "dk-" + agID, Status: model.AlertGroupStatusTriggered,
		Title: "Test Alert", TeamID: "devops", Severity: "critical",
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
	})

	// Switched off in the database after the cache was loaded.
	off, _ := json.Marshal(model.SlackConfig{Token: "xoxb-test", SigningSecret: secret, Interactive: false})
	if _, err := s.UpdateIntegration(context.Background(), "int-slack-test",
		store.IntegrationPatch{Config: off}, "denis"); err != nil {
		t.Fatal(err)
	}
	if !api.integrationCache.GetSlackInteractive() {
		t.Fatal("the cache was refreshed, and the test proves nothing about the database")
	}

	captured := newCapturedEphemeral()
	api.respondEphemeral = captured.post
	req := signedSlackInteractiveRequest(t, secret, SlackActionAckAlertGroup, agID, "U_DENIS")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if msg := captured.waitOne(t); !strings.Contains(msg, "Buttons are switched off for this integration") {
		t.Fatalf("the press was answered %q", msg)
	}
	if ag, _ := s.GetAlertGroupByID(agID); ag.Status != model.AlertGroupStatusTriggered {
		t.Fatalf("a press with the buttons switched off moved the group to %s", ag.Status)
	}
}

func TestATelegramPressIsRefusedWhenTheSwitchIsOffInTheDatabase(t *testing.T) {
	const secret = "tg-switch-secret"
	api, s, e, ft := setupTelegramAPI(t, secret)
	denis, _ := s.GetUserByEmail("denis@example.com")
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: denis.ID, Provider: "telegram", ExternalID: "777"})
	agID := "ag-tg-switch-" + fmt.Sprintf("%d", time.Now().UnixNano())
	s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "dk-" + agID, Status: model.AlertGroupStatusTriggered,
		Title: "Test Alert", TeamID: "devops", Severity: "critical",
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
	})

	off := false
	cfg, _ := json.Marshal(model.TelegramConfig{BotToken: "123:abc", SecretToken: secret, Interactive: &off})
	if _, err := s.UpdateIntegration(context.Background(), "int-tg",
		store.IntegrationPatch{Config: cfg}, "denis"); err != nil {
		t.Fatal(err)
	}
	if !api.integrationCache.GetTelegramInteractive() {
		t.Fatal("the cache was refreshed, and the test proves nothing about the database")
	}

	body := fmt.Sprintf(`{"callback_query":{"id":"cb1","from":{"id":777,"first_name":"Denis"},"data":"ack:%s"}}`, agID)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, telegramWebhookReq(secret, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(fmt.Sprint(ft.answered), "Buttons are switched off for this integration") {
		t.Fatalf("the press was answered %v", ft.answered)
	}
	if ag, _ := s.GetAlertGroupByID(agID); ag.Status != model.AlertGroupStatusTriggered {
		t.Fatalf("a press with the buttons switched off moved the group to %s", ag.Status)
	}
}
