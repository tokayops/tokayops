package integrations

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func TestTelegram_Descriptor(t *testing.T) {
	if !IsValidType(model.IntegrationTypeTelegram) {
		t.Fatal("telegram should be a registered type")
	}
	dir, ok := DirectionFor(model.IntegrationTypeTelegram)
	if !ok || dir != model.IntegrationDirectionOutbound {
		t.Fatalf("DirectionFor(telegram) = (%v,%v), want outbound,true", dir, ok)
	}
}

func TestTelegram_ValidateConfig(t *testing.T) {
	// Create: bot_token required.
	if err := ValidateConfig(model.IntegrationTypeTelegram, json.RawMessage(`{}`), false); err == nil {
		t.Error("create with empty bot_token should error")
	}
	// Create: masked secrets rejected.
	if err := ValidateConfig(model.IntegrationTypeTelegram, json.RawMessage(`{"bot_token":"****"}`), false); err == nil {
		t.Error("create with masked bot_token should error")
	}
	if err := ValidateConfig(model.IntegrationTypeTelegram, json.RawMessage(`{"bot_token":"123:abc","secret_token":"****"}`), false); err == nil {
		t.Error("create with masked secret_token should error")
	}
	// Create: secret_token required (the webhook needs it regardless of the
	// interactive switch).
	if err := ValidateConfig(model.IntegrationTypeTelegram, json.RawMessage(`{"bot_token":"123:abc"}`), false); err == nil {
		t.Error("create without secret_token should error")
	}
	// Create: real bot_token + secret_token accepted.
	if err := ValidateConfig(model.IntegrationTypeTelegram, json.RawMessage(`{"bot_token":"123:abc","secret_token":"shh"}`), false); err != nil {
		t.Errorf("create with real bot_token + secret_token should validate: %v", err)
	}
	// Update: empty/masked accepted (keep existing).
	if err := ValidateConfig(model.IntegrationTypeTelegram, json.RawMessage(`{"bot_token":"****"}`), true); err != nil {
		t.Errorf("update with masked bot_token should not error: %v", err)
	}
}

func TestTelegram_MaskSecrets(t *testing.T) {
	i := &model.Integration{
		Type:   model.IntegrationTypeTelegram,
		Config: json.RawMessage(`{"bot_token":"123:abc","secret_token":"shh","default_chat_id":"-100999"}`),
	}
	masked := MaskSecrets(i)

	var c map[string]string
	if err := json.Unmarshal(masked.Config, &c); err != nil {
		t.Fatalf("unmarshal masked config: %v", err)
	}
	if c["bot_token"] != model.MaskedSecret || c["secret_token"] != model.MaskedSecret {
		t.Errorf("secrets not masked: %v", c)
	}
	if c["default_chat_id"] != "-100999" {
		t.Errorf("default_chat_id should be preserved, got %q", c["default_chat_id"])
	}
	// Original must be untouched.
	if strings.Contains(string(i.Config), model.MaskedSecret) {
		t.Error("MaskSecrets mutated the original integration config")
	}
}
