package store

import (
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
)

func TestIntegrationStore(t *testing.T) {
	if testStore == nil {
		t.Skip("TEST_DB_DSN not set")
	}

	// Setup encryption key for tests
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	// t.Setenv, not os.Setenv + defer os.Unsetenv: the latter does not restore
	// the previous value, it deletes the variable. TestMain sets a default key
	// for the whole package, so unsetting it here left every later test that
	// needs one failing - invisibly in declaration order, and reproducibly
	// under -shuffle.
	t.Setenv(config.EncryptionKeyEnv, hex.EncodeToString(key))

	t.Run("CRUD slack integration", func(t *testing.T) {
		s := setupTestDB(t)

		// Create
		cfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-test", DefaultChannel: "C123"})
		integration := &model.Integration{
			Type:    model.IntegrationTypeSlack,
			Name:    "Test Slack",
			Enabled: true,
			Config:  cfg,
		}

		err := s.CreateIntegration(integration)
		if err != nil {
			t.Fatalf("CreateIntegration failed: %v", err)
		}

		if integration.ID == "" {
			t.Error("Expected ID to be set after create")
		}
		if integration.Direction != model.IntegrationDirectionOutbound {
			t.Errorf("Expected outbound direction, got %s", integration.Direction)
		}

		// Get
		fetched, err := s.GetIntegrationByID(integration.ID)
		if err != nil {
			t.Fatalf("GetIntegrationByID failed: %v", err)
		}

		if fetched.Name != "Test Slack" {
			t.Errorf("Expected name 'Test Slack', got %s", fetched.Name)
		}

		// Verify decryption worked - config should have the token
		var fetchedCfg model.SlackConfig
		if err := json.Unmarshal(fetched.Config, &fetchedCfg); err != nil {
			t.Fatalf("Failed to unmarshal config: %v", err)
		}
		if fetchedCfg.Token != "xoxb-test" {
			t.Errorf("Token not decrypted correctly, got: %s", fetchedCfg.Token)
		}

		// Update
		fetched.Name = "Updated Slack"
		fetched.Enabled = false
		err = s.UpdateIntegration(fetched)
		if err != nil {
			t.Fatalf("UpdateIntegration failed: %v", err)
		}

		updated, _ := s.GetIntegrationByID(integration.ID)
		if updated.Name != "Updated Slack" {
			t.Errorf("Name not updated, got %s", updated.Name)
		}
		if updated.Enabled {
			t.Error("Enabled should be false")
		}

		// Delete
		err = s.DeleteIntegration(integration.ID)
		if err != nil {
			t.Fatalf("DeleteIntegration failed: %v", err)
		}

		_, err = s.GetIntegrationByID(integration.ID)
		if err != ErrIntegrationNotFound {
			t.Errorf("Expected ErrIntegrationNotFound after delete, got %v", err)
		}
	})

	t.Run("duplicate outbound integration rejected", func(t *testing.T) {
		s := setupTestDB(t)

		cfg, _ := json.Marshal(model.SlackConfig{Token: "token1"})
		first := &model.Integration{Type: model.IntegrationTypeSlack, Name: "Slack 1", Config: cfg}
		if err := s.CreateIntegration(first); err != nil {
			t.Fatalf("First create failed: %v", err)
		}

		cfg2, _ := json.Marshal(model.SlackConfig{Token: "token2"})
		second := &model.Integration{Type: model.IntegrationTypeSlack, Name: "Slack 2", Config: cfg2}
		err := s.CreateIntegration(second)
		if err != ErrDuplicateIntegration {
			t.Errorf("Expected ErrDuplicateIntegration, got %v", err)
		}
	})

	t.Run("telegram outbound create + duplicate rejected", func(t *testing.T) {
		s := setupTestDB(t)

		cfg, _ := json.Marshal(model.TelegramConfig{BotToken: "123:abc", SecretToken: "shh", DefaultChatID: "-100999"})
		first := &model.Integration{Type: model.IntegrationTypeTelegram, Name: "TG 1", Enabled: true, Config: cfg}
		if err := s.CreateIntegration(first); err != nil {
			t.Fatalf("create telegram failed: %v", err)
		}
		if first.Direction != model.IntegrationDirectionOutbound {
			t.Errorf("Expected outbound direction, got %s", first.Direction)
		}
		// bot_token round-trips through encryption.
		fetched, err := s.GetIntegrationByID(first.ID)
		if err != nil {
			t.Fatalf("GetIntegrationByID: %v", err)
		}
		var fc model.TelegramConfig
		if err := json.Unmarshal(fetched.Config, &fc); err != nil {
			t.Fatalf("unmarshal config: %v", err)
		}
		if fc.BotToken != "123:abc" {
			t.Errorf("bot_token not decrypted correctly, got %q", fc.BotToken)
		}

		// Second telegram outbound integration is rejected (single-bot rule).
		cfg2, _ := json.Marshal(model.TelegramConfig{BotToken: "999:def"})
		second := &model.Integration{Type: model.IntegrationTypeTelegram, Name: "TG 2", Config: cfg2}
		if err := s.CreateIntegration(second); err != ErrDuplicateIntegration {
			t.Errorf("Expected ErrDuplicateIntegration for 2nd telegram, got %v", err)
		}
	})

	// Counterpart to the rejection above: single-outbound-per-type is the rule
	// (1 Slack + 1 Telegram), but generic_webhook is the deliberate exception -
	// it fans out to many endpoints, so multiple outbound rows are allowed.
	// This is a locked contract: several integrations of one type was declined.
	t.Run("multiple generic_webhook outbound allowed (intentional exception)", func(t *testing.T) {
		s := setupTestDB(t)

		global := model.WebhookScopeGlobal
		for i := 1; i <= 2; i++ {
			cfg, _ := json.Marshal(model.GenericWebhookConfig{URL: "https://example.com/hook" + string(rune('0'+i))})
			gw := &model.Integration{
				Type:   model.IntegrationTypeGenericWebhook,
				Name:   "Generic " + string(rune('0'+i)),
				Scope:  &global,
				Config: cfg,
			}
			if err := s.CreateIntegration(gw); err != nil {
				t.Fatalf("generic_webhook %d should be allowed (multi exception): %v", i, err)
			}
		}
	})

	t.Run("multiple inbound webhooks allowed", func(t *testing.T) {
		s := setupTestDB(t)

		for i := 1; i <= 3; i++ {
			cfg, _ := json.Marshal(model.WebhookConfig{Secret: "secret" + string(rune('0'+i))})
			wh := &model.Integration{Type: model.IntegrationTypeAlertmanagerWebhook, Name: "Alertmanager", Config: cfg}
			if err := s.CreateIntegration(wh); err != nil {
				t.Fatalf("Create webhook %d failed: %v", i, err)
			}
		}

		// All should be retrievable
		all, err := s.GetAllIntegrations()
		if err != nil {
			t.Fatalf("GetAllIntegrations failed: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("Expected 3 integrations, got %d", len(all))
		}
	})

	t.Run("update preserves secret if masked", func(t *testing.T) {
		s := setupTestDB(t)

		cfg, _ := json.Marshal(model.SlackConfig{Token: "original-token"})
		integration := &model.Integration{Type: model.IntegrationTypeSlack, Name: "Slack", Config: cfg}
		s.CreateIntegration(integration)

		// Update with masked config (simulating API update where user didn't change secret)
		maskedCfg, _ := json.Marshal(model.SlackConfig{Token: model.MaskedSecret, DefaultChannel: "C456"})
		integration.Config = maskedCfg
		integration.Name = "Slack Updated"
		s.UpdateIntegration(integration)

		// Fetch and verify token was preserved
		updated, _ := s.GetIntegrationByID(integration.ID)
		var updatedCfg model.SlackConfig
		json.Unmarshal(updated.Config, &updatedCfg)

		if updatedCfg.Token != "original-token" {
			t.Errorf("Original token should be preserved, got: %s", updatedCfg.Token)
		}
		if updatedCfg.DefaultChannel != "C456" {
			t.Errorf("DefaultChannel should be updated, got: %s", updatedCfg.DefaultChannel)
		}
	})

	t.Run("update preserves signing_secret if masked", func(t *testing.T) {
		s := setupTestDB(t)

		cfg, _ := json.Marshal(model.SlackConfig{Token: "tok", SigningSecret: "original-signing-secret"})
		integration := &model.Integration{Type: model.IntegrationTypeSlack, Name: "Slack", Config: cfg}
		s.CreateIntegration(integration)

		// Update with masked signing_secret
		maskedCfg, _ := json.Marshal(model.SlackConfig{Token: model.MaskedSecret, SigningSecret: model.MaskedSecret})
		integration.Config = maskedCfg
		s.UpdateIntegration(integration)

		updated, _ := s.GetIntegrationByID(integration.ID)
		var updatedCfg model.SlackConfig
		json.Unmarshal(updated.Config, &updatedCfg)

		if updatedCfg.SigningSecret != "original-signing-secret" {
			t.Errorf("Signing secret should be preserved, got: %s", updatedCfg.SigningSecret)
		}
	})

	t.Run("update preserves signing_secret if empty", func(t *testing.T) {
		s := setupTestDB(t)

		cfg, _ := json.Marshal(model.SlackConfig{Token: "tok", SigningSecret: "my-secret"})
		integration := &model.Integration{Type: model.IntegrationTypeSlack, Name: "Slack", Config: cfg}
		s.CreateIntegration(integration)

		// Update with empty signing_secret
		emptyCfg, _ := json.Marshal(model.SlackConfig{Token: model.MaskedSecret})
		integration.Config = emptyCfg
		s.UpdateIntegration(integration)

		updated, _ := s.GetIntegrationByID(integration.ID)
		var updatedCfg model.SlackConfig
		json.Unmarshal(updated.Config, &updatedCfg)

		if updatedCfg.SigningSecret != "my-secret" {
			t.Errorf("Signing secret should be preserved when empty, got: %s", updatedCfg.SigningSecret)
		}
	})

	t.Run("GetIntegrationsByType", func(t *testing.T) {
		s := setupTestDB(t)

		// Create one of each type (must be enabled for GetIntegrationsByType)
		slackCfg, _ := json.Marshal(model.SlackConfig{Token: "tok"})
		s.CreateIntegration(&model.Integration{Type: model.IntegrationTypeSlack, Name: "S", Enabled: true, Config: slackCfg})

		whCfg, _ := json.Marshal(model.WebhookConfig{Secret: "sec"})
		s.CreateIntegration(&model.Integration{Type: model.IntegrationTypeAlertmanagerWebhook, Name: "W1", Enabled: true, Config: whCfg})
		s.CreateIntegration(&model.Integration{Type: model.IntegrationTypeAlertmanagerWebhook, Name: "W2", Enabled: true, Config: whCfg})

		// Query by type
		webhooks, err := s.GetIntegrationsByType(model.IntegrationTypeAlertmanagerWebhook)
		if err != nil {
			t.Fatalf("GetIntegrationsByType failed: %v", err)
		}
		if len(webhooks) != 2 {
			t.Errorf("Expected 2 webhooks, got %d", len(webhooks))
		}

		slacks, _ := s.GetIntegrationsByType(model.IntegrationTypeSlack)
		if len(slacks) != 1 {
			t.Errorf("Expected 1 slack, got %d", len(slacks))
		}
	})

	t.Run("CRUD generic_webhook global", func(t *testing.T) {
		s := setupTestDB(t)

		scope := model.WebhookScopeGlobal
		cfg, _ := json.Marshal(model.GenericWebhookConfig{
			URL:    "https://example.com/hook",
			Secret: "wh-secret",
		})
		integration := &model.Integration{
			Type:    model.IntegrationTypeGenericWebhook,
			Name:    "Global Webhook",
			Enabled: true,
			Scope:   &scope,
			Config:  cfg,
		}

		if err := s.CreateIntegration(integration); err != nil {
			t.Fatalf("CreateIntegration failed: %v", err)
		}
		if integration.Direction != model.IntegrationDirectionOutbound {
			t.Errorf("Expected outbound, got %s", integration.Direction)
		}

		fetched, err := s.GetIntegrationByID(integration.ID)
		if err != nil {
			t.Fatalf("GetIntegrationByID failed: %v", err)
		}
		if fetched.Scope == nil || *fetched.Scope != model.WebhookScopeGlobal {
			t.Errorf("Expected scope=global, got %v", fetched.Scope)
		}
		if fetched.TeamID != nil {
			t.Errorf("Expected nil team_id, got %v", fetched.TeamID)
		}

		// Update name
		fetched.Name = "Updated Global"
		if err := s.UpdateIntegration(fetched); err != nil {
			t.Fatalf("UpdateIntegration failed: %v", err)
		}
		updated, _ := s.GetIntegrationByID(integration.ID)
		if updated.Name != "Updated Global" {
			t.Errorf("Name not updated, got %s", updated.Name)
		}

		// Delete
		if err := s.DeleteIntegration(integration.ID); err != nil {
			t.Fatalf("DeleteIntegration failed: %v", err)
		}
	})

	t.Run("CRUD generic_webhook team", func(t *testing.T) {
		s := setupTestDB(t)

		// Create team first (needed for FK)
		s.CreateTeam(&model.Team{ID: "test-team", Name: "Test Team"})

		scope := model.WebhookScopeTeam
		teamID := "test-team"
		cfg, _ := json.Marshal(model.GenericWebhookConfig{
			URL:    "https://example.com/hook",
			Secret: "wh-secret",
		})
		integration := &model.Integration{
			Type:    model.IntegrationTypeGenericWebhook,
			Name:    "Team Webhook",
			Enabled: true,
			Scope:   &scope,
			TeamID:  &teamID,
			Config:  cfg,
		}

		if err := s.CreateIntegration(integration); err != nil {
			t.Fatalf("CreateIntegration failed: %v", err)
		}

		fetched, err := s.GetIntegrationByID(integration.ID)
		if err != nil {
			t.Fatalf("GetIntegrationByID failed: %v", err)
		}
		if fetched.Scope == nil || *fetched.Scope != model.WebhookScopeTeam {
			t.Errorf("Expected scope=team, got %v", fetched.Scope)
		}
		if fetched.TeamID == nil || *fetched.TeamID != "test-team" {
			t.Errorf("Expected team_id=test-team, got %v", fetched.TeamID)
		}
	})

	t.Run("multiple generic_webhook allowed", func(t *testing.T) {
		s := setupTestDB(t)

		scope := model.WebhookScopeGlobal
		for i := 1; i <= 3; i++ {
			cfg, _ := json.Marshal(model.GenericWebhookConfig{
				URL:    "https://example.com/hook",
				Secret: "secret",
			})
			wh := &model.Integration{
				Type:    model.IntegrationTypeGenericWebhook,
				Name:    "Webhook",
				Enabled: true,
				Scope:   &scope,
				Config:  cfg,
			}
			if err := s.CreateIntegration(wh); err != nil {
				t.Fatalf("Create webhook %d failed: %v", i, err)
			}
		}

		all, err := s.GetAllIntegrations()
		if err != nil {
			t.Fatalf("GetAllIntegrations failed: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("Expected 3 integrations, got %d", len(all))
		}
	})

	t.Run("GetIntegrationsByType generic_webhook", func(t *testing.T) {
		s := setupTestDB(t)

		scope := model.WebhookScopeGlobal
		for i := 0; i < 3; i++ {
			cfg, _ := json.Marshal(model.GenericWebhookConfig{URL: "https://example.com", Secret: "s"})
			enabled := i < 2 // first two enabled, third disabled
			wh := &model.Integration{
				Type:    model.IntegrationTypeGenericWebhook,
				Name:    "WH",
				Enabled: enabled,
				Scope:   &scope,
				Config:  cfg,
			}
			if err := s.CreateIntegration(wh); err != nil {
				t.Fatalf("Create failed: %v", err)
			}
		}

		webhooks, err := s.GetIntegrationsByType(model.IntegrationTypeGenericWebhook)
		if err != nil {
			t.Fatalf("GetIntegrationsByType failed: %v", err)
		}
		if len(webhooks) != 2 {
			t.Errorf("Expected 2 enabled webhooks, got %d", len(webhooks))
		}
	})

	t.Run("update preserves generic_webhook config", func(t *testing.T) {
		s := setupTestDB(t)

		scope := model.WebhookScopeGlobal
		cfg, _ := json.Marshal(model.GenericWebhookConfig{
			URL:            "https://example.com/hook",
			Secret:         "original-secret",
			TimeoutSeconds: 15,
			CustomHeaders:  map[string]string{"X-Env": "prod"},
		})
		integration := &model.Integration{
			Type:    model.IntegrationTypeGenericWebhook,
			Name:    "WH",
			Enabled: true,
			Scope:   &scope,
			Config:  cfg,
		}
		s.CreateIntegration(integration)

		// Update with masked secret only - other config fields should be preserved
		maskedCfg, _ := json.Marshal(model.GenericWebhookConfig{Secret: model.MaskedSecret})
		integration.Config = maskedCfg
		integration.Name = "WH Updated"
		s.UpdateIntegration(integration)

		updated, _ := s.GetIntegrationByID(integration.ID)
		var updatedCfg model.GenericWebhookConfig
		json.Unmarshal(updated.Config, &updatedCfg)

		if updatedCfg.Secret != "original-secret" {
			t.Errorf("Secret should be preserved, got: %s", updatedCfg.Secret)
		}
		if updatedCfg.URL != "https://example.com/hook" {
			t.Errorf("URL should be preserved, got: %s", updatedCfg.URL)
		}
		if updatedCfg.TimeoutSeconds != 15 {
			t.Errorf("TimeoutSeconds should be preserved, got: %d", updatedCfg.TimeoutSeconds)
		}
		if updatedCfg.CustomHeaders["X-Env"] != "prod" {
			t.Errorf("CustomHeaders should be preserved, got: %v", updatedCfg.CustomHeaders)
		}
	})
}
