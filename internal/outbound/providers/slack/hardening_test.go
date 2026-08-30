package slack

import (
	"context"
	"testing"
)

// TestSendDMRefusesAnEmptyMessage. What this type sends is a direct message
// and nothing else: no target kind to get wrong, and no way to ask it for a
// card. A card sent from here would record nothing about where it landed, so no
// revision of the alert could ever reach it - cards come from ExecuteAttempt.
func TestSendDMRefusesAnEmptyMessage(t *testing.T) {
	provider := newSlackProviderForTest("test-token", "http://invalid.local/")

	if err := provider.SendDM(context.Background(), "U1", ""); err == nil {
		t.Error("a message with nothing in it was sent")
	}
}
