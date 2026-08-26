package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// What a send refuses before it reaches Slack, and what it refuses after Slack
// answers with something unusable. Both are about not persisting a delivery
// nobody can act on afterwards.

// P2.1: an editable channel send must produce valid coordinates; if Slack returns an
// empty timestamp the send fails rather than persisting a useless payload.
func TestSlackSend_EmptyTimestamp_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "channel": "C123", "ts": ""})
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	_, err := provider.Send(context.Background(), providers.NotificationRequest{
		Kind:       "channel",
		Target:     providers.NotificationTarget{Kind: "channel", ID: "C123"},
		AlertGroup: &model.AlertGroup{ID: "ag", Title: "T", Severity: "critical"},
		Editable:   true,
	})
	if err == nil {
		t.Fatal("expected error when postMessage returns an empty timestamp")
	}
}

// P3: Send validates the target shape and rejects unknown kinds instead of silently
// posting a channel card.
func TestSlackSend_KindValidation(t *testing.T) {
	provider := newSlackProviderForTest("test-token", "http://invalid.local/", "")
	ctx := context.Background()

	if _, err := provider.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "carrier-pigeon", ID: "x"}}); err == nil {
		t.Error("unknown target kind should error")
	}
	if _, err := provider.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "channel", ID: "C1"}, Editable: true}); err == nil {
		t.Error("channel send without an alert group should error")
	}
	if _, err := provider.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "user", ID: "U1"}}); err == nil {
		t.Error("user send without a message should error")
	}
}
