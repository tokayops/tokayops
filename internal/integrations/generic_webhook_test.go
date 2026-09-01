package integrations

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func validateWebhook(t *testing.T, cfg model.GenericWebhookConfig) error {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := Get(model.IntegrationTypeGenericWebhook)
	if !ok {
		t.Fatal("no descriptor for generic_webhook")
	}
	return d.ValidateConfig(raw, false)
}

// TestAWebhookTimeoutIsBoundedByTheFamily: thirty is the ceiling, and the two
// values the old limit allowed above it are refused - the delivery attempt's
// deadline is a sum of this ceiling and would otherwise cut them off quietly.
func TestAWebhookTimeoutIsBoundedByTheFamily(t *testing.T) {
	for seconds, wantErr := range map[int]bool{0: false, 1: false, 30: false, 31: true, 60: true, -1: true} {
		err := validateWebhook(t, model.GenericWebhookConfig{URL: "https://hooks.example.com/a", TimeoutSeconds: seconds})
		if (err != nil) != wantErr {
			t.Errorf("timeout_seconds=%d: %v", seconds, err)
		}
	}
}

// TestOurHeaderNamesAreNotTheSubscribersToSet: in any case, with any value.
func TestOurHeaderNamesAreNotTheSubscribersToSet(t *testing.T) {
	for _, name := range []string{"X-Tokay-Event-ID", "x-tokay-event-id", "X-TOKAY-SIGNATURE",
		"X-Tokay-Timestamp", "x-tokay-event", "Content-Type", "content-type", "X-Tokay-Anything"} {
		err := validateWebhook(t, model.GenericWebhookConfig{URL: "https://hooks.example.com/a",
			CustomHeaders: map[string]string{name: "x"}})
		if err == nil {
			t.Errorf("custom header %s was accepted", name)
		}
	}
	if err := validateWebhook(t, model.GenericWebhookConfig{URL: "https://hooks.example.com/a",
		CustomHeaders: map[string]string{"X-Team": "sre", "Authorization": "Bearer x"}}); err != nil {
		t.Fatalf("ordinary custom headers were refused: %v", err)
	}
}
