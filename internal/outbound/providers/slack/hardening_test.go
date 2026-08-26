package slack

import (
	"context"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// TestTheLegacySendTakesDirectMessagesOnly. What is left of Send is the handoff
// announcement: one direct message, nothing edits it afterwards.
//
// A channel target is refused rather than posted. A card sent from here would
// record nothing about where it landed, so no revision of the alert could ever
// reach it - and the fact that no caller asks for one today is not a reason to
// leave the branch open. Where cards do come from is ExecuteAttempt.
func TestTheLegacySendTakesDirectMessagesOnly(t *testing.T) {
	provider := newSlackProviderForTest("test-token", "http://invalid.local/", "")
	ctx := context.Background()

	if _, err := provider.Send(ctx, providers.NotificationRequest{
		Target: providers.NotificationTarget{Kind: "carrier-pigeon", ID: "x"}}); err == nil {
		t.Error("unknown target kind should error")
	}
	if _, err := provider.Send(ctx, providers.NotificationRequest{
		Target: providers.NotificationTarget{Kind: "channel", ID: "C1"}, Editable: true}); err == nil {
		t.Error("a card is not sent from here any more, and was accepted")
	}
	if _, err := provider.Send(ctx, providers.NotificationRequest{
		Target: providers.NotificationTarget{Kind: "user", ID: "U1"}}); err == nil {
		t.Error("user send without a message should error")
	}
}
