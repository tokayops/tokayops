package store

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// mergeSecrets is a pure function (no DB) — unit-test the telegram case directly.
func TestMergeSecrets_Telegram(t *testing.T) {
	existing := json.RawMessage(`{"bot_token":"123:real","secret_token":"realsecret","default_chat_id":"-100999"}`)

	t.Run("masked/empty keeps existing secrets, preserves default_chat_id", func(t *testing.T) {
		incoming := json.RawMessage(`{"bot_token":"****","secret_token":"","default_chat_id":""}`)
		merged := mergeSecrets(model.IntegrationTypeTelegram, existing, incoming)

		var c model.TelegramConfig
		if err := json.Unmarshal(merged, &c); err != nil {
			t.Fatalf("unmarshal merged: %v", err)
		}
		if c.BotToken != "123:real" {
			t.Errorf("bot_token = %q, want kept existing", c.BotToken)
		}
		if c.SecretToken != "realsecret" {
			t.Errorf("secret_token = %q, want kept existing", c.SecretToken)
		}
		if c.DefaultChatID != "-100999" {
			t.Errorf("default_chat_id = %q, want kept existing", c.DefaultChatID)
		}
	})

	t.Run("real new values override", func(t *testing.T) {
		incoming := json.RawMessage(`{"bot_token":"999:new","secret_token":"newsecret","default_chat_id":"-100777"}`)
		merged := mergeSecrets(model.IntegrationTypeTelegram, existing, incoming)

		var c model.TelegramConfig
		if err := json.Unmarshal(merged, &c); err != nil {
			t.Fatalf("unmarshal merged: %v", err)
		}
		if c.BotToken != "999:new" || c.SecretToken != "newsecret" || c.DefaultChatID != "-100777" {
			t.Errorf("new values not applied: %+v", c)
		}
	})
}
