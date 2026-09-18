package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

func TestIntegrationCache_LoadAll(t *testing.T) {
	t.Run("loads slack config", func(t *testing.T) {
		slackConfig, _ := json.Marshal(model.SlackConfig{
			Token:          "xoxb-test-token",
			DefaultChannel: "C123",
		})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{
			ID:      "int-1",
			Type:    model.IntegrationTypeSlack,
			Enabled: true,
			Config:  slackConfig,
		})

		cache := NewIntegrationCache()
		if err := cache.LoadAll(store); err != nil {
			t.Fatalf("LoadAll failed: %v", err)
		}

		if cache.GetSlackToken() != "xoxb-test-token" {
			t.Errorf("Expected token 'xoxb-test-token', got '%s'", cache.GetSlackToken())
		}
		if cache.GetSlackChannel() != "C123" {
			t.Errorf("Expected channel 'C123', got '%s'", cache.GetSlackChannel())
		}
	})

	t.Run("loads slack signing secret", func(t *testing.T) {
		slackConfig, _ := json.Marshal(model.SlackConfig{
			Token:         "xoxb-test-token",
			SigningSecret: "test-signing-secret",
		})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{
			ID:      "int-ss",
			Type:    model.IntegrationTypeSlack,
			Enabled: true,
			Config:  slackConfig,
		})

		cache := NewIntegrationCache()
		if err := cache.LoadAll(store); err != nil {
			t.Fatalf("LoadAll failed: %v", err)
		}

		if cache.GetSlackSigningSecret() != "test-signing-secret" {
			t.Errorf("Expected signing secret 'test-signing-secret', got '%s'", cache.GetSlackSigningSecret())
		}
	})

	t.Run("disabled integration does not populate signing secret", func(t *testing.T) {
		slackConfig, _ := json.Marshal(model.SlackConfig{
			Token:         "xoxb-test",
			SigningSecret: "should-not-load",
		})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{
			ID:      "int-disabled",
			Type:    model.IntegrationTypeSlack,
			Enabled: false,
			Config:  slackConfig,
		})

		cache := NewIntegrationCache()
		cache.LoadAll(store)

		if cache.GetSlackSigningSecret() != "" {
			t.Errorf("Disabled integration should not populate signing secret, got '%s'", cache.GetSlackSigningSecret())
		}
	})

	t.Run("loads telegram token and secret token", func(t *testing.T) {
		tgConfig, _ := json.Marshal(model.TelegramConfig{
			BotToken:    "123:abc",
			SecretToken: "webhook-secret",
		})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{
			ID:      "int-tg",
			Type:    model.IntegrationTypeTelegram,
			Enabled: true,
			Config:  tgConfig,
		})

		cache := NewIntegrationCache()
		if err := cache.LoadAll(store); err != nil {
			t.Fatalf("LoadAll failed: %v", err)
		}
		if cache.GetTelegramToken() != "123:abc" {
			t.Errorf("Expected telegram token '123:abc', got '%s'", cache.GetTelegramToken())
		}
		if cache.GetTelegramSecretToken() != "webhook-secret" {
			t.Errorf("Expected secret token 'webhook-secret', got '%s'", cache.GetTelegramSecretToken())
		}

		// Reload with empty store clears it.
		cache.LoadAll(NewMockStore())
		if cache.GetTelegramToken() != "" || cache.GetTelegramSecretToken() != "" {
			t.Error("reload should clear telegram token/secret")
		}
	})

	t.Run("disabled telegram integration does not populate token", func(t *testing.T) {
		tgConfig, _ := json.Marshal(model.TelegramConfig{BotToken: "123:abc"})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{ID: "int-tg-off", Type: model.IntegrationTypeTelegram, Enabled: false, Config: tgConfig})

		cache := NewIntegrationCache()
		cache.LoadAll(store)
		if cache.GetTelegramToken() != "" {
			t.Errorf("disabled telegram integration should not load token, got '%s'", cache.GetTelegramToken())
		}
	})

	t.Run("loads multiple webhook secrets", func(t *testing.T) {
		wh1, _ := json.Marshal(model.WebhookConfig{Secret: "secret1"})
		wh2, _ := json.Marshal(model.WebhookConfig{Secret: "secret2"})

		store := NewMockStore()
		store.CreateIntegration(&model.Integration{ID: "wh-1", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: wh1})
		store.CreateIntegration(&model.Integration{ID: "wh-2", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: wh2})

		cache := NewIntegrationCache()
		cache.LoadAll(store)

		if _, ok := cache.WebhookIntegrationID("secret1"); !ok {
			t.Error("secret1 should be valid")
		}
		if _, ok := cache.WebhookIntegrationID("secret2"); !ok {
			t.Error("secret2 should be valid")
		}
		if _, ok := cache.WebhookIntegrationID("wrong"); ok {
			t.Error("wrong secret should be invalid")
		}
	})

	t.Run("ignores disabled integrations", func(t *testing.T) {
		slackConfig, _ := json.Marshal(model.SlackConfig{Token: "disabled-token"})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{ID: "int-1", Type: model.IntegrationTypeSlack, Enabled: false, Config: slackConfig})

		cache := NewIntegrationCache()
		cache.LoadAll(store)

		if cache.GetSlackToken() != "" {
			t.Error("Disabled integration should not load token")
		}
	})

	t.Run("rejects all when no webhooks configured", func(t *testing.T) {
		cache := NewIntegrationCache()
		cache.LoadAll(NewMockStore())

		// With no secrets configured, all requests should be rejected (secure by default)
		if _, ok := cache.WebhookIntegrationID("any-secret"); ok {
			t.Error("Should reject when no webhooks configured")
		}
		if _, ok := cache.WebhookIntegrationID(""); ok {
			t.Error("Should reject empty secret")
		}
		if cache.HasWebhookSecrets() {
			t.Error("Should report no webhook secrets")
		}
	})

	t.Run("reload clears old data", func(t *testing.T) {
		slackConfig, _ := json.Marshal(model.SlackConfig{Token: "old-token"})
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{ID: "int-1", Type: model.IntegrationTypeSlack, Enabled: true, Config: slackConfig})

		cache := NewIntegrationCache()
		cache.LoadAll(store)

		// Reload with empty store
		cache.LoadAll(NewMockStore())

		if cache.GetSlackToken() != "" {
			t.Error("Reload should clear old token")
		}
	})
}

func TestIntegrationCache_ThreadSafe(t *testing.T) {
	cache := NewIntegrationCache()
	slackConfig, _ := json.Marshal(model.SlackConfig{Token: "token"})
	store := NewMockStore()
	store.CreateIntegration(&model.Integration{ID: "1", Type: model.IntegrationTypeSlack, Enabled: true, Config: slackConfig})

	done := make(chan bool)

	// Concurrent reads and writes
	go func() {
		for i := 0; i < 100; i++ {
			cache.LoadAll(store)
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 100; i++ {
			cache.GetSlackToken()
			cache.GetSlackChannel()
			cache.WebhookIntegrationID("test")
		}
		done <- true
	}()

	<-done
	<-done
}

// The cache is what the send path actually reads, so the "absent means enabled"
// rule has to survive the round trip through stored JSON, not just live in the
// model accessor.
func TestIntegrationCache_TelegramInteractive(t *testing.T) {
	load := func(t *testing.T, cfg json.RawMessage) *IntegrationCache {
		t.Helper()
		store := NewMockStore()
		store.CreateIntegration(&model.Integration{
			ID:      "int-tg",
			Type:    model.IntegrationTypeTelegram,
			Enabled: true,
			Config:  cfg,
		})
		cache := NewIntegrationCache()
		if err := cache.LoadAll(store); err != nil {
			t.Fatalf("LoadAll failed: %v", err)
		}
		return cache
	}

	t.Run("config written before the field existed stays enabled", func(t *testing.T) {
		cache := load(t, json.RawMessage(`{"bot_token":"123:abc","secret_token":"shh"}`))
		if !cache.GetTelegramInteractive() {
			t.Error("interactive = false, want true for a record with no interactive key")
		}
	})

	t.Run("explicit false is honoured", func(t *testing.T) {
		cache := load(t, json.RawMessage(`{"bot_token":"123:abc","secret_token":"shh","interactive":false}`))
		if cache.GetTelegramInteractive() {
			t.Error("interactive = true, want the stored false")
		}
	})

	t.Run("explicit true is honoured", func(t *testing.T) {
		cache := load(t, json.RawMessage(`{"bot_token":"123:abc","secret_token":"shh","interactive":true}`))
		if !cache.GetTelegramInteractive() {
			t.Error("interactive = false, want the stored true")
		}
	})

	t.Run("no telegram integration at all", func(t *testing.T) {
		cache := NewIntegrationCache()
		if err := cache.LoadAll(NewMockStore()); err != nil {
			t.Fatalf("LoadAll failed: %v", err)
		}
		if cache.GetTelegramInteractive() {
			t.Error("interactive = true with no integration configured")
		}
	})
}

// TestTheCacheNamesTheIntegrationASecretBelongsTo, and answers the same way on
// every instance when two integrations were given one secret: the first by id.
// Nothing else could be deterministic - the database returns them in whatever
// order it likes.
func TestTheCacheNamesTheIntegrationASecretBelongsTo(t *testing.T) {
	shared, _ := json.Marshal(model.WebhookConfig{Secret: "shared"})
	own, _ := json.Marshal(model.WebhookConfig{Secret: "own"})
	off, _ := json.Marshal(model.WebhookConfig{Secret: "off"})

	s := NewMockStore()
	// Created in the order that is NOT the id order, so an answer that
	// followed insertion would be visible.
	s.CreateIntegration(&model.Integration{ID: "am-2", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: shared})
	s.CreateIntegration(&model.Integration{ID: "am-1", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: shared})
	s.CreateIntegration(&model.Integration{ID: "am-3", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: own})
	s.CreateIntegration(&model.Integration{ID: "am-4", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: false, Config: off})

	cache := NewIntegrationCache()
	if err := cache.LoadAll(s); err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, c := range []struct {
		name, secret, wantID string
		wantKnown            bool
	}{
		{"a secret of its own", "own", "am-3", true},
		{"a secret two integrations share", "shared", "am-1", true},
		{"a secret of a disabled integration", "off", "", false},
		{"a secret nobody has", "stranger", "", false},
		{"no secret at all", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			id, known := cache.WebhookIntegrationID(c.secret)
			if known != c.wantKnown || id != c.wantID {
				t.Errorf("answered %q/%v, want %q/%v", id, known, c.wantID, c.wantKnown)
			}
		})
	}
}

// TestAnInstanceThatHandledNoChangeCatchesUp is the rotation behind a load
// balancer: one instance saves the new token and reloads its own cache, and
// every other instance goes on answering from what it loaded before. The old
// token is refused against the database; the new one has to reach it at all,
// and only a reload does that.
func TestAnInstanceThatHandledNoChangeCatchesUp(t *testing.T) {
	before, _ := json.Marshal(model.WebhookConfig{Secret: "old-token"})
	after, _ := json.Marshal(model.WebhookConfig{Secret: "new-token"})

	s := NewMockStore()
	s.CreateIntegration(&model.Integration{
		ID: "am-1", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: before,
	})

	handled, other := NewIntegrationCache(), NewIntegrationCache()
	for _, cache := range []*IntegrationCache{handled, other} {
		if err := cache.LoadAll(s); err != nil {
			t.Fatalf("load: %v", err)
		}
	}

	// The token is rotated, and the instance that took the request reloads.
	if _, err := s.UpdateIntegration(context.Background(), "am-1",
		IntegrationPatch{Config: after}, "denis"); err != nil {
		t.Fatalf("rotate the token: %v", err)
	}
	if err := handled.LoadAll(s); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, ok := handled.WebhookIntegrationID("new-token"); !ok {
		t.Error("the instance that handled the change does not know the new token")
	}
	if _, ok := other.WebhookIntegrationID("new-token"); ok {
		t.Error("the other instance knows a token it was never told about")
	}

	// Which is what the refresh is for: the same reload, on a timer.
	if err := other.LoadAll(s); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := other.WebhookIntegrationID("new-token"); !ok {
		t.Error("a refreshed cache still does not know the new token")
	}
	if _, ok := other.WebhookIntegrationID("old-token"); ok {
		t.Error("a refreshed cache still knows the token that was rotated away")
	}
}

// TestTheRefreshLoopFollowsTheDatabase drives the loop itself, so that a
// refresh that never reloads - or one nobody starts - is a failure here rather
// than something noticed when a rotated token stops working in production.
//
// What it cannot see is the wiring: that main.go starts this loop at all is
// not something a test in this package can hold.
func TestTheRefreshLoopFollowsTheDatabase(t *testing.T) {
	before, _ := json.Marshal(model.WebhookConfig{Secret: "loop-old"})
	after, _ := json.Marshal(model.WebhookConfig{Secret: "loop-new"})

	s := NewMockStore()
	s.CreateIntegration(&model.Integration{
		ID: "am-loop", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: before,
	})

	cache := NewIntegrationCache()
	if err := cache.LoadAll(s); err != nil {
		t.Fatalf("load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cache.Refresh(ctx, s, 5*time.Millisecond)

	if _, err := s.UpdateIntegration(context.Background(), "am-loop",
		IntegrationPatch{Config: after}, "denis"); err != nil {
		t.Fatalf("rotate the token: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := cache.WebhookIntegrationID("loop-new"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh loop never picked up the rotated token")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, ok := cache.WebhookIntegrationID("loop-old"); ok {
		t.Error("the refresh loop kept the token that was rotated away")
	}
}

// TestALoadThatFoundNothingNewSaysNothing. This runs every thirty seconds on
// every instance, so a line per load would be the loudest thing in the log and
// would say the same thing every time.
func TestALoadThatFoundNothingNewSaysNothing(t *testing.T) {
	cfg, _ := json.Marshal(model.WebhookConfig{Secret: "quiet-log"})
	s := NewMockStore()
	s.CreateIntegration(&model.Integration{
		ID: "am-log", Type: model.IntegrationTypeAlertmanagerWebhook, Enabled: true, Config: cfg,
	})
	cache := NewIntegrationCache()

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	load := func() string {
		t.Helper()
		logged.Reset()
		if err := cache.LoadAll(s); err != nil {
			t.Fatalf("load: %v", err)
		}
		return logged.String()
	}

	if first := load(); !strings.Contains(first, "loaded") {
		t.Errorf("the first load said nothing:\n%s", first)
	}
	if again := load(); again != "" {
		t.Errorf("a load that found nothing new left:\n%s", again)
	}

	rotated, _ := json.Marshal(model.WebhookConfig{Secret: "quiet-log-2"})
	if _, err := s.UpdateIntegration(context.Background(), "am-log",
		IntegrationPatch{Config: rotated}, "denis"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if changed := load(); !strings.Contains(changed, "loaded") {
		t.Errorf("a load that found a change said nothing:\n%s", changed)
	}
}
