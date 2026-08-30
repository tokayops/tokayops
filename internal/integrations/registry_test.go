package integrations

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func TestValidTypes_SortedAndComplete(t *testing.T) {
	got := ValidTypes()
	want := []model.IntegrationType{
		model.IntegrationTypeAlertmanagerWebhook,
		model.IntegrationTypeGenericWebhook,
		model.IntegrationTypeSlack,
		model.IntegrationTypeTelegram,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidTypes() = %v, want %v (sorted asc)", got, want)
	}
}

func TestIsValidType(t *testing.T) {
	if !IsValidType(model.IntegrationTypeSlack) {
		t.Error("slack should be valid")
	}
	if !IsValidType(model.IntegrationTypeAlertmanagerWebhook) {
		t.Error("alertmanager_webhook should be valid")
	}
	if !IsValidType(model.IntegrationTypeGenericWebhook) {
		t.Error("generic_webhook should be valid")
	}
	if IsValidType(model.IntegrationType("invalid")) {
		t.Error("invalid type should not be valid")
	}
}

func TestDirectionFor(t *testing.T) {
	cases := []struct {
		in   model.IntegrationType
		want model.IntegrationDirection
		ok   bool
	}{
		{model.IntegrationTypeSlack, model.IntegrationDirectionOutbound, true},
		{model.IntegrationTypeAlertmanagerWebhook, model.IntegrationDirectionInbound, true},
		{model.IntegrationTypeGenericWebhook, model.IntegrationDirectionOutbound, true},
		{model.IntegrationType("unknown"), "", false},
	}
	for _, c := range cases {
		t.Run(string(c.in), func(t *testing.T) {
			got, ok := DirectionFor(c.in)
			if ok != c.ok || got != c.want {
				t.Fatalf("DirectionFor(%s) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestRegister_DuplicatePanics guards against silently overwriting a descriptor
// - a programming error worth surfacing loudly. resetForTests gives us an
// empty slate so we don't blow up on the init()-time registrations.
func TestRegister_DuplicatePanics(t *testing.T) {
	saved := snapshot()
	defer restore(saved)
	resetForTests()

	d := Descriptor{
		Type:           "x",
		Direction:      model.IntegrationDirectionOutbound,
		ValidateConfig: func(_ json.RawMessage, _ bool) error { return nil },
	}
	Register(d)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	Register(d)
}

func TestValidateConfig_UnknownType(t *testing.T) {
	err := ValidateConfig("nope", json.RawMessage(`{}`), false)
	if err == nil || !strings.Contains(err.Error(), "unknown integration type") {
		t.Fatalf("expected unknown-type error, got %v", err)
	}
}

func TestValidateConfig_Slack(t *testing.T) {
	// Empty token rejected on create.
	if err := ValidateConfig(model.IntegrationTypeSlack, json.RawMessage(`{}`), false); err == nil {
		t.Error("expected slack token required error on create")
	}
	// Empty body accepted on update.
	if err := ValidateConfig(model.IntegrationTypeSlack, json.RawMessage(`{}`), true); err != nil {
		t.Errorf("update with empty body should not error: %v", err)
	}
	// Real token accepted.
	if err := ValidateConfig(model.IntegrationTypeSlack, json.RawMessage(`{"token":"xoxb-x"}`), false); err != nil {
		t.Errorf("real token should validate: %v", err)
	}
}

// helpers

func snapshot() map[model.IntegrationType]Descriptor {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[model.IntegrationType]Descriptor, len(registry))
	for k, v := range registry {
		out[k] = v
	}
	return out
}

func restore(s map[model.IntegrationType]Descriptor) {
	mu.Lock()
	defer mu.Unlock()
	registry = s
}
