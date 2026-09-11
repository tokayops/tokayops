package store

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// mergeSecrets is a pure function (no DB) - unit-test the telegram case directly.
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

// interactive is a *bool so that "absent" stays distinguishable from "off".
// An update that leaves the field out must not reset it, because a nil value
// resolves to true and would switch the buttons back on behind the operator.
func TestMergeSecrets_Telegram_Interactive(t *testing.T) {
	off := false
	existingOff, err := json.Marshal(model.TelegramConfig{
		BotToken: "123:real", SecretToken: "realsecret", Interactive: &off,
	})
	if err != nil {
		t.Fatalf("marshal existing: %v", err)
	}

	t.Run("absent new value keeps the existing false", func(t *testing.T) {
		incoming := json.RawMessage(`{"bot_token":"****","secret_token":""}`)
		merged := mergeSecrets(model.IntegrationTypeTelegram, existingOff, incoming)

		var c model.TelegramConfig
		if err := json.Unmarshal(merged, &c); err != nil {
			t.Fatalf("unmarshal merged: %v", err)
		}
		if c.Interactive == nil {
			t.Fatal("interactive was dropped to nil, which resolves to true")
		}
		if c.IsInteractive() {
			t.Error("interactive = true, want the existing false to survive")
		}
	})

	t.Run("explicit true replaces the existing false", func(t *testing.T) {
		incoming := json.RawMessage(`{"bot_token":"****","secret_token":"","interactive":true}`)
		merged := mergeSecrets(model.IntegrationTypeTelegram, existingOff, incoming)

		var c model.TelegramConfig
		if err := json.Unmarshal(merged, &c); err != nil {
			t.Fatalf("unmarshal merged: %v", err)
		}
		if !c.IsInteractive() {
			t.Error("interactive = false, want the explicit true to win")
		}
	})

	t.Run("absent on both sides resolves to enabled", func(t *testing.T) {
		existing := json.RawMessage(`{"bot_token":"123:real","secret_token":"realsecret"}`)
		incoming := json.RawMessage(`{"bot_token":"****","secret_token":""}`)
		merged := mergeSecrets(model.IntegrationTypeTelegram, existing, incoming)

		var c model.TelegramConfig
		if err := json.Unmarshal(merged, &c); err != nil {
			t.Fatalf("unmarshal merged: %v", err)
		}
		if !c.IsInteractive() {
			t.Error("a record predating the field must keep its buttons")
		}
	})
}
