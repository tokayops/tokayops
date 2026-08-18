package store

import (
	"encoding/json"
	"testing"

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

		if !cache.ValidateWebhookSecret("secret1") {
			t.Error("secret1 should be valid")
		}
		if !cache.ValidateWebhookSecret("secret2") {
			t.Error("secret2 should be valid")
		}
		if cache.ValidateWebhookSecret("wrong") {
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
		if cache.ValidateWebhookSecret("any-secret") {
			t.Error("Should reject when no webhooks configured")
		}
		if cache.ValidateWebhookSecret("") {
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
			cache.ValidateWebhookSecret("test")
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
