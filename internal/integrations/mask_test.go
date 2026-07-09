package integrations

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func TestMaskSecrets(t *testing.T) {
	t.Run("masks slack token", func(t *testing.T) {
		cfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-real-token", DefaultChannel: "C123"})
		i := &model.Integration{ID: "int-1", Type: model.IntegrationTypeSlack, Config: cfg}

		masked := MaskSecrets(i)

		var maskedCfg model.SlackConfig
		json.Unmarshal(masked.Config, &maskedCfg)
		if maskedCfg.Token != model.MaskedSecret {
			t.Errorf("Token should be masked, got: %s", maskedCfg.Token)
		}
		if maskedCfg.DefaultChannel != "C123" {
			t.Errorf("DefaultChannel should not be masked, got: %s", maskedCfg.DefaultChannel)
		}
	})

	t.Run("masks webhook secret", func(t *testing.T) {
		cfg, _ := json.Marshal(model.WebhookConfig{Secret: "super-secret"})
		i := &model.Integration{ID: "int-2", Type: model.IntegrationTypeAlertmanagerWebhook, Config: cfg}

		masked := MaskSecrets(i)

		var maskedCfg model.WebhookConfig
		json.Unmarshal(masked.Config, &maskedCfg)
		if maskedCfg.Secret != model.MaskedSecret {
			t.Errorf("Secret should be masked, got: %s", maskedCfg.Secret)
		}
	})

	t.Run("masks slack signing_secret", func(t *testing.T) {
		cfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-real-token", SigningSecret: "abc123secret"})
		i := &model.Integration{ID: "int-ss", Type: model.IntegrationTypeSlack, Config: cfg}

		masked := MaskSecrets(i)

		var maskedCfg model.SlackConfig
		json.Unmarshal(masked.Config, &maskedCfg)
		if maskedCfg.SigningSecret != model.MaskedSecret {
			t.Errorf("SigningSecret should be masked, got: %s", maskedCfg.SigningSecret)
		}
		if maskedCfg.Token != model.MaskedSecret {
			t.Errorf("Token should be masked, got: %s", maskedCfg.Token)
		}

		// Original must not be modified.
		var originalCfg model.SlackConfig
		json.Unmarshal(i.Config, &originalCfg)
		if originalCfg.SigningSecret != "abc123secret" {
			t.Error("Original signing_secret should not be modified")
		}
	})

	t.Run("masks generic_webhook secret, preserves rest", func(t *testing.T) {
		cfg, _ := json.Marshal(model.GenericWebhookConfig{
			URL:            "https://example.com/hook",
			Secret:         "my-secret",
			TimeoutSeconds: 10,
			CustomHeaders:  map[string]string{"X-Env": "prod"},
		})
		i := &model.Integration{ID: "int-gw", Type: model.IntegrationTypeGenericWebhook, Config: cfg}

		masked := MaskSecrets(i)

		var maskedCfg model.GenericWebhookConfig
		json.Unmarshal(masked.Config, &maskedCfg)
		if maskedCfg.Secret != model.MaskedSecret {
			t.Errorf("Secret should be masked, got: %s", maskedCfg.Secret)
		}
		if maskedCfg.URL != "https://example.com/hook" {
			t.Errorf("URL should be preserved, got: %s", maskedCfg.URL)
		}
		if maskedCfg.CustomHeaders["X-Env"] != "prod" {
			t.Errorf("CustomHeaders should be preserved, got: %v", maskedCfg.CustomHeaders)
		}
	})

	t.Run("does not modify original", func(t *testing.T) {
		cfg, _ := json.Marshal(model.SlackConfig{Token: "original"})
		i := &model.Integration{ID: "int-3", Type: model.IntegrationTypeSlack, Config: cfg}

		_ = MaskSecrets(i)

		var originalCfg model.SlackConfig
		json.Unmarshal(i.Config, &originalCfg)
		if originalCfg.Token != "original" {
			t.Error("Original should not be modified")
		}
	})

	t.Run("empty secret field left untouched", func(t *testing.T) {
		// A Slack config with only an empty token must not gain a "****"
		// placeholder for a value that was never set.
		cfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-x"})
		i := &model.Integration{ID: "int-4", Type: model.IntegrationTypeSlack, Config: cfg}

		masked := MaskSecrets(i)

		var maskedCfg model.SlackConfig
		json.Unmarshal(masked.Config, &maskedCfg)
		if maskedCfg.UserToken != "" {
			t.Errorf("absent user_token should stay empty, got: %s", maskedCfg.UserToken)
		}
		if maskedCfg.Token != model.MaskedSecret {
			t.Errorf("token should be masked, got: %s", maskedCfg.Token)
		}
	})
}
