package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// mockTokenSource implements TokenSource for testing
type mockTokenSource struct {
	token       string
	interactive bool
}

func (m *mockTokenSource) GetSlackToken() string {
	return m.token
}

func (m *mockTokenSource) GetSlackInteractive() bool {
	return m.interactive
}

// newSlackProviderForTest creates a Provider with a pre-configured client for testing
// This allows tests to use a mock server without the provider recreating the client
func newSlackProviderForTest(token, apiURL, selfURL string) *Provider {
	return &Provider{
		tokenSource: &mockTokenSource{token: token},
		selfURL:     selfURL,
		client:      slackapi.New(token, slackapi.OptionAPIURL(apiURL)),
		cachedToken: token,
	}
}

func TestSlackSendUpdateResolve(t *testing.T) {
	// Mock Slack API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/chat.postMessage" {
			// Return successful response
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C123",
				"ts":      "123.456",
			})
			return
		}
		if r.URL.Path == "/chat.getPermalink" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":        true,
				"permalink": "https://slackapi.test/archives/C123/p123456",
			})
			return
		}
		if r.URL.Path == "/chat.update" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C123",
				"ts":      "123.456",
			})
			return
		}

		t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	// Initialize Provider with test factory
	provider := newSlackProviderForTest("test-token", server.URL+"/", "")

	ctx := context.Background()
	ag := &model.AlertGroup{
		ID:       "ag-1",
		Title:    "Test Alert Group",
		Severity: "critical",
		Alerts: []model.Alert{
			{Labels: map[string]string{"alertname": "A1"}, Status: "firing"},
		},
	}

	// 1. Send
	dataStr, err := provider.Send(ctx, providers.NotificationRequest{
		Kind:       "channel",
		Target:     providers.NotificationTarget{Kind: "channel", ID: "C123"},
		AlertGroup: ag,
		Editable:   true,
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if dataStr == "" {
		t.Error("Send returned empty data")
	}

	// The delivery carries the provider payload Update/Resolve operate on.
	delivery := &model.NotificationDelivery{ID: "del-1", ProviderPayload: dataStr}

	// 2. Update
	_, err = provider.Update(ctx, delivery, ag)
	if err != nil {
		t.Errorf("Update failed: %v", err)
	}

	// 3. Resolve
	err = provider.Resolve(ctx, delivery, ag)
	if err != nil {
		t.Errorf("Resolve failed: %v", err)
	}
}

// TestSlackProvider_SendDM_OpensConversationAndPosts verifies a user-target Send
// uses conversations.open then chat.postMessage to the opened DM channel.
func TestSlackProvider_SendDM_OpensConversationAndPosts(t *testing.T) {
	var opened, posted bool
	var postedChannel, postedText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.open":
			opened = true
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": map[string]any{"id": "D123"}})
		case "/chat.postMessage":
			posted = true
			_ = r.ParseForm()
			postedChannel = r.PostForm.Get("channel")
			postedText = r.PostForm.Get("text")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "D123", "ts": "1.2"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	if _, err := provider.Send(context.Background(), providers.NotificationRequest{
		Kind:     "slack_dm",
		Target:   providers.NotificationTarget{Kind: "user", ID: "U_TARGET"},
		Message:  "hello there",
		Editable: false,
	}); err != nil {
		t.Fatalf("Send DM: %v", err)
	}
	if !opened {
		t.Error("expected conversations.open to be called")
	}
	if !posted {
		t.Error("expected chat.postMessage to be called")
	}
	if postedChannel != "D123" {
		t.Errorf("expected post to opened DM channel D123, got %q", postedChannel)
	}
	if postedText != "hello there" {
		t.Errorf("expected message text 'hello there', got %q", postedText)
	}
}

// TestSlackProvider_LookupByEmail covers users.lookupByEmail success and the
// users_not_found -> ErrUserNotFound mapping.
func TestSlackProvider_LookupByEmail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/users.lookupByEmail" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			return
		}
		_ = r.ParseForm()
		if r.PostForm.Get("email") == "found@x.test" {
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": "U_FOUND"}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "users_not_found"})
		}
	}))
	defer server.Close()
	provider := newSlackProviderForTest("test-token", server.URL+"/", "")

	id, err := provider.GetSlackUserIDByEmail(context.Background(), "found@x.test")
	if err != nil || id != "U_FOUND" {
		t.Fatalf("lookup found: got id=%q err=%v, want U_FOUND/nil", id, err)
	}

	if _, err := provider.GetSlackUserIDByEmail(context.Background(), "missing@x.test"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("lookup missing: expected ErrUserNotFound, got %v", err)
	}
}

// TestSlackProvider_EmailBySlackID covers users.info: success, user_not_found
// mapping, and the "profile has no email" error.
func TestSlackProvider_EmailBySlackID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/users.info" {
			t.Errorf("unexpected request: %s", r.URL.Path)
			return
		}
		_ = r.ParseForm()
		switch r.PostForm.Get("user") {
		case "U_OK":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": "U_OK", "profile": map[string]any{"email": "ok@x.test"}}})
		case "U_NOEMAIL":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"id": "U_NOEMAIL", "profile": map[string]any{"email": ""}}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "user_not_found"})
		}
	}))
	defer server.Close()
	provider := newSlackProviderForTest("test-token", server.URL+"/", "")

	email, err := provider.GetEmailBySlackID(context.Background(), "U_OK")
	if err != nil || email != "ok@x.test" {
		t.Fatalf("info ok: got email=%q err=%v, want ok@x.test/nil", email, err)
	}
	if _, err := provider.GetEmailBySlackID(context.Background(), "U_MISSING"); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("info missing: expected ErrUserNotFound, got %v", err)
	}
	if _, err := provider.GetEmailBySlackID(context.Background(), "U_NOEMAIL"); err == nil {
		t.Error("info no-email: expected an error for a user with no profile email")
	}
}

// TestSlackProvider_UpdateResolve_InvalidPayload verifies Update/Resolve reject a
// malformed provider payload before making any Slack API call.
func TestSlackProvider_UpdateResolve_InvalidPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no Slack API call expected for an invalid payload, got %s", r.URL.Path)
	}))
	defer server.Close()
	provider := newSlackProviderForTest("test-token", server.URL+"/", "")

	bad := &model.NotificationDelivery{ID: "del-bad", ProviderPayload: "{ not valid json"}
	if _, err := provider.Update(context.Background(), bad, &model.AlertGroup{}); err == nil {
		t.Error("expected Update to reject an invalid provider payload")
	}
	if err := provider.Resolve(context.Background(), bad, &model.AlertGroup{}); err == nil {
		t.Error("expected Resolve to reject an invalid provider payload")
	}
}

// TestSlackProvider_MissingToken verifies that an unconfigured token surfaces as
// ErrNoToken on both channel and DM sends (a permanent, no-retry error).
func TestSlackProvider_MissingToken(t *testing.T) {
	provider := &Provider{tokenSource: &mockTokenSource{token: ""}}

	if _, err := provider.Send(context.Background(), providers.NotificationRequest{
		Kind: "channel", Target: providers.NotificationTarget{Kind: "channel", ID: "C1"}, AlertGroup: &model.AlertGroup{}, Editable: true,
	}); !errors.Is(err, ErrNoToken) {
		t.Errorf("channel send: expected ErrNoToken, got %v", err)
	}
	if _, err := provider.Send(context.Background(), providers.NotificationRequest{
		Kind: "slack_dm", Target: providers.NotificationTarget{Kind: "user", ID: "U1"}, Message: "x",
	}); !errors.Is(err, ErrNoToken) {
		t.Errorf("dm send: expected ErrNoToken, got %v", err)
	}
}

func TestRenderTimeline(t *testing.T) {
	provider := &Provider{}

	// Case 1: Empty timeline
	ag1 := &model.AlertGroup{
		TimelineEvents: nil,
	}
	timeline1 := RenderTimeline(provider.freeze(ag1, false))
	if timeline1 != "" {
		t.Errorf("Expected empty timeline for no events, got: %s", timeline1)
	}

	// Case 2: Timeline with events
	ag2 := &model.AlertGroup{
		TimelineEvents: []*model.TimelineEvent{
			{
				Type:    model.TimelineEventCreated,
				Message: "Alert group created",
			},
			{
				Type:    model.TimelineEventAlertAdded,
				Message: "Alert: TestAlert",
			},
			{
				Type:    model.TimelineEventNotificationSent,
				Message: "Notification sent via slack",
			},
		},
	}
	timeline2 := RenderTimeline(provider.freeze(ag2, false))
	if !strings.Contains(timeline2, "📋 *Timeline:*") {
		t.Errorf("Expected timeline header, got: %s", timeline2)
	}
	if !strings.Contains(timeline2, "```") {
		t.Errorf("Expected code block wrapper, got: %s", timeline2)
	}
	if !strings.Contains(timeline2, "[NEW]") {
		t.Errorf("Expected created icon [NEW], got: %s", timeline2)
	}
	if !strings.Contains(timeline2, "[+]") {
		t.Errorf("Expected alert added icon [+], got: %s", timeline2)
	}
	if !strings.Contains(timeline2, "[->]") {
		t.Errorf("Expected notification sent icon [->], got: %s", timeline2)
	}
	// Check for GMT offset presence
	if !strings.Contains(timeline2, "GMT") {
		t.Errorf("Expected GMT offset in timestamp, got: %s", timeline2)
	}
}

func TestSlackTimelinePosting(t *testing.T) {
	// Mock Slack API to track calls
	var postMessageCalls int
	var lastChannel, lastText, lastThreadTS string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/chat.postMessage" {
			postMessageCalls++

			// Capture request data
			var payload struct {
				Channel  string `json:"channel"`
				Text     string `json:"text"`
				ThreadTS string `json:"thread_ts"`
			}

			// Try decoding as JSON first
			if r.Header.Get("Content-Type") == "application/json" {
				json.NewDecoder(r.Body).Decode(&payload)
			} else {
				// Fallback to Form parsing
				r.ParseForm()
				payload.Channel = r.FormValue("channel")
				payload.Text = r.FormValue("text")
				payload.ThreadTS = r.FormValue("thread_ts")
			}

			lastChannel = payload.Channel
			lastText = payload.Text
			lastThreadTS = payload.ThreadTS

			// Return successful response
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C123",
				"ts":      "100.200", // Main message TS
			})
			return
		}
		if r.URL.Path == "/chat.getPermalink" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":        true,
				"permalink": "https://slackapi.test/archives/C123/p100200",
			})
			return
		}

		t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	ctx := context.Background()

	// Create AlertGroup WITH timeline events
	ag := &model.AlertGroup{
		ID:       "ag-timeline-1",
		Title:    "Timeline Test",
		Severity: "critical",
		TimelineEvents: []*model.TimelineEvent{
			{
				Type:      model.TimelineEventCreated,
				Message:   "Group created",
				CreatedAt: time.Now(),
			},
		},
	}

	// Send should post TWO messages:
	// 1. Main alert card
	// 2. Timeline thread reply
	dataStr, err := provider.Send(ctx, providers.NotificationRequest{
		Kind:       "channel",
		Target:     providers.NotificationTarget{Kind: "channel", ID: "C123"},
		AlertGroup: ag,
		Editable:   true,
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Verify we got 2 PostMessage calls
	if postMessageCalls != 2 {
		t.Errorf("Expected 2 PostMessage calls (Main + Timeline), got %d", postMessageCalls)
	}

	if lastChannel != "C123" {
		t.Errorf("Expected last message to be in channel C123, got %s", lastChannel)
	}

	// Verify the second call was in a thread
	if lastThreadTS != "100.200" {
		t.Errorf("Expected timeline to be in thread 100.200, got %s", lastThreadTS)
	}

	// Verify timeline content
	if !strings.Contains(lastText, "Group created") {
		t.Errorf("Expected timeline text to contain 'Group created', got: %s", lastText)
	}

	// Verify returned data contains TimelineTimestamp
	var data Data
	json.Unmarshal([]byte(dataStr), &data)
	if data.TimelineTimestamp != "100.200" {
		t.Errorf("Expected TimelineTimestamp to be set, got %s", data.TimelineTimestamp)
	}
}

func TestResolve_ReturnsErrorOnMainMessageUpdateFailure(t *testing.T) {
	// Mock Slack API: thread reply succeeds, main message update fails
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/chat.postMessage" {
			// Thread reply succeeds
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C123",
				"ts":      "999.000",
			})
			return
		}
		if r.URL.Path == "/chat.update" {
			// Main message update FAILS
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "channel_not_found",
			})
			return
		}
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	ctx := context.Background()

	slackData := Data{
		ChannelID: "C123",
		Timestamp: "123.456",
	}
	dataBytes, _ := json.Marshal(slackData)

	ag := &model.AlertGroup{
		ID:       "ag-resolve-fail",
		Title:    "Resolve Fail Test",
		Severity: "critical",
	}
	delivery := &model.NotificationDelivery{ID: "del-1", ProviderPayload: string(dataBytes)}

	err := provider.Resolve(ctx, delivery, ag)
	if err == nil {
		t.Fatal("Expected error from Resolve when main message update fails, got nil")
	}
	if !strings.Contains(err.Error(), "failed to update main message") {
		t.Errorf("Expected 'failed to update main message' in error, got: %v", err)
	}
}

func TestResolve_SucceedsWhenThreadReplyFails(t *testing.T) {
	// Mock Slack API: thread reply fails, main message update succeeds
	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/chat.postMessage" {
			callCount++
			// Thread reply fails
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "too_many_attachments",
			})
			return
		}
		if r.URL.Path == "/chat.update" {
			// Main message update succeeds
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C123",
				"ts":      "123.456",
			})
			return
		}
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	ctx := context.Background()

	slackData := Data{
		ChannelID: "C123",
		Timestamp: "123.456",
	}
	dataBytes, _ := json.Marshal(slackData)

	ag := &model.AlertGroup{
		ID:       "ag-resolve-thread-fail",
		Title:    "Thread Fail Test",
		Severity: "critical",
		TimelineEvents: []*model.TimelineEvent{
			{Type: model.TimelineEventCreated, Message: "created", CreatedAt: time.Now()},
		},
	}
	delivery := &model.NotificationDelivery{ID: "del-1", ProviderPayload: string(dataBytes)}

	// Should succeed even though thread reply failed — thread is non-critical
	err := provider.Resolve(ctx, delivery, ag)
	if err != nil {
		t.Errorf("Expected Resolve to succeed when only thread reply fails, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Block Kit v2 rendering tests
// ---------------------------------------------------------------------------

func TestRenderTitleAndBody_Triggered(t *testing.T) {
	provider := &Provider{selfURL: "https://tokayops.example.com", tokenSource: &mockTokenSource{interactive: true}}
	ag := &model.AlertGroup{
		ID:       "ag-v2-1",
		Title:    "HighCPU",
		Status:   model.AlertGroupStatusTriggered,
		Severity: "critical",
		Alerts: []model.Alert{
			{
				Status: model.AlertStatusFiring,
				Labels: map[string]string{"alertname": "HighCPU", "severity": "critical"},
				Annotations: map[string]string{
					"dashboard": "https://grafana/d/cpu",
					"runbook":   "https://wiki/cpu",
				},
			},
		},
	}

	title := renderTitleBlocks(provider.freeze(ag, false))
	att := renderBodyAttachment(provider.freeze(ag, false), provider.interactive())

	// Color bar = red
	if att.Color != "#FF0000" {
		t.Errorf("expected red color, got %s", att.Color)
	}

	// Fallback text for clients that can't render blocks
	if att.Fallback == "" {
		t.Error("expected non-empty Fallback text")
	}
	if !strings.Contains(att.Fallback, "HighCPU") {
		t.Errorf("Fallback should contain alert title, got %q", att.Fallback)
	}

	// 4 blocks in body attachment: severity+alerts, divider, actions, context
	if len(att.Blocks.BlockSet) != 4 {
		t.Fatalf("expected 4 blocks for triggered body, got %d", len(att.Blocks.BlockSet))
	}

	// Title block (from renderTitleBlocks) contains alert title
	titleBlock, ok := title[0].(*slackapi.SectionBlock)
	if !ok {
		t.Fatal("title[0] is not SectionBlock")
	}
	if !strings.Contains(titleBlock.Text.Text, "HighCPU") {
		t.Errorf("title block should contain 'HighCPU', got %q", titleBlock.Text.Text)
	}

	// ActionBlock has 2 buttons: Acknowledge + Resolve
	actionBlock, ok := att.Blocks.BlockSet[2].(*slackapi.ActionBlock)
	if !ok {
		t.Fatal("block 2 is not ActionBlock")
	}
	if len(actionBlock.Elements.ElementSet) != 2 {
		t.Fatalf("expected 2 buttons, got %d", len(actionBlock.Elements.ElementSet))
	}

	ackBtn := actionBlock.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	if ackBtn.ActionID != "ack_alert_group" {
		t.Errorf("ack button ActionID = %q, want ack_alert_group", ackBtn.ActionID)
	}
	if ackBtn.Value != "ag-v2-1" {
		t.Errorf("ack button Value = %q, want ag-v2-1", ackBtn.Value)
	}
	if ackBtn.Style != slackapi.StyleDanger {
		t.Errorf("ack button Style = %q, want danger", ackBtn.Style)
	}

	resolveBtn := actionBlock.Elements.ElementSet[1].(*slackapi.ButtonBlockElement)
	if resolveBtn.ActionID != "resolve_alert_group" {
		t.Errorf("resolve button ActionID = %q, want resolve_alert_group", resolveBtn.ActionID)
	}
	if resolveBtn.Value != "ag-v2-1" {
		t.Errorf("resolve button Value = %q, want ag-v2-1", resolveBtn.Value)
	}

	// Context block (footer) with deep link
	ctxBlock, ok := att.Blocks.BlockSet[3].(*slackapi.ContextBlock)
	if !ok {
		t.Fatal("block 3 is not ContextBlock")
	}
	footerText := ctxBlock.ContextElements.Elements[0].(*slackapi.TextBlockObject)
	if !strings.Contains(footerText.Text, "ag-v2-1") {
		t.Errorf("footer should contain AG ID, got %q", footerText.Text)
	}

	// Alerts section contains dashboard and runbook links
	alertsBlock := att.Blocks.BlockSet[0].(*slackapi.SectionBlock)
	if !strings.Contains(alertsBlock.Text.Text, "[dash]") {
		t.Errorf("alerts block should contain dashboard link, got %q", alertsBlock.Text.Text)
	}
	if !strings.Contains(alertsBlock.Text.Text, "[runbook]") {
		t.Errorf("alerts block should contain runbook link, got %q", alertsBlock.Text.Text)
	}
}

func TestRenderTitleAndBody_Acknowledged(t *testing.T) {
	provider := &Provider{tokenSource: &mockTokenSource{interactive: true}}
	ag := &model.AlertGroup{
		ID:       "ag-v2-2",
		Title:    "HighMem",
		Status:   model.AlertGroupStatusAcknowledged,
		Severity: "warning",
		Alerts: []model.Alert{
			{Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "HighMem", "severity": "warning"}},
		},
	}

	title := renderTitleBlocks(provider.freeze(ag, false))
	att := renderBodyAttachment(provider.freeze(ag, false), provider.interactive())

	if att.Color != "#FFA500" {
		t.Errorf("expected orange color, got %s", att.Color)
	}

	// 4 blocks in body attachment: severity+alerts, divider, actions (Resolve only), context
	if len(att.Blocks.BlockSet) != 4 {
		t.Fatalf("expected 4 blocks for acknowledged body (Resolve button), got %d", len(att.Blocks.BlockSet))
	}

	// Title contains "Acknowledged"
	titleBlock := title[0].(*slackapi.SectionBlock)
	if !strings.Contains(titleBlock.Text.Text, "Acknowledged") {
		t.Errorf("title should contain 'Acknowledged', got %q", titleBlock.Text.Text)
	}

	// ActionBlock has only 1 button (Resolve)
	actionBlock := att.Blocks.BlockSet[2].(*slackapi.ActionBlock)
	if len(actionBlock.Elements.ElementSet) != 1 {
		t.Fatalf("expected 1 button (Resolve only), got %d", len(actionBlock.Elements.ElementSet))
	}
	btn := actionBlock.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	if btn.ActionID != "resolve_alert_group" {
		t.Errorf("button ActionID = %q, want resolve_alert_group", btn.ActionID)
	}
}

func TestRenderTitleAndBody_Resolved(t *testing.T) {
	provider := &Provider{}
	ag := &model.AlertGroup{
		ID:       "ag-v2-3",
		Title:    "DiskFull",
		Status:   model.AlertGroupStatusResolved,
		Severity: "critical",
		Alerts: []model.Alert{
			{Status: model.AlertStatusResolved, Labels: map[string]string{"alertname": "DiskFull"}},
		},
	}

	title := renderTitleBlocks(provider.freeze(ag, true))
	att := renderBodyAttachment(provider.freeze(ag, true), provider.interactive())

	if att.Color != "#36a64f" {
		t.Errorf("expected green color, got %s", att.Color)
	}

	// 2 blocks in body attachment: severity+alerts, context (no action block, no divider)
	if len(att.Blocks.BlockSet) != 2 {
		t.Fatalf("expected 2 blocks for resolved body (no buttons), got %d", len(att.Blocks.BlockSet))
	}

	titleBlock := title[0].(*slackapi.SectionBlock)
	if !strings.Contains(titleBlock.Text.Text, "Resolved") {
		t.Errorf("title should contain 'Resolved', got %q", titleBlock.Text.Text)
	}

	// Last block is context (footer), no action block
	_, isContext := att.Blocks.BlockSet[1].(*slackapi.ContextBlock)
	if !isContext {
		t.Error("last block should be ContextBlock (footer)")
	}
}

func TestRenderTitleAndBody_WithMentions(t *testing.T) {
	provider := &Provider{}
	ag := &model.AlertGroup{
		ID:       "ag-v2-mentions",
		Title:    "MentionTest",
		Severity: "critical",
		Alerts: []model.Alert{
			{
				Status: model.AlertStatusFiring,
				Labels: map[string]string{"alertname": "A1", "slack_user": "U12345"},
			},
			{
				Status: model.AlertStatusFiring,
				Labels: map[string]string{"alertname": "A2", "slack_user": "S67890"},
			},
		},
	}

	att := renderBodyAttachment(provider.freeze(ag, false), provider.interactive())

	// Severity section (block 0 in body attachment) should contain mentions
	severityBlock := att.Blocks.BlockSet[0].(*slackapi.SectionBlock)
	if !strings.Contains(severityBlock.Text.Text, "<@U12345>") {
		t.Errorf("severity block should contain user mention, got %q", severityBlock.Text.Text)
	}
	if !strings.Contains(severityBlock.Text.Text, "<!subteam^S67890>") {
		t.Errorf("severity block should contain group mention, got %q", severityBlock.Text.Text)
	}
}

func TestRenderTitleAndBody_WithExternalURL(t *testing.T) {
	provider := &Provider{}
	ag := &model.AlertGroup{
		ID:          "ag-v2-url",
		Title:       "URLTest",
		Severity:    "critical",
		ExternalURL: "https://alertmanager.example.com/alerts/1",
		Alerts: []model.Alert{
			{Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "A1"}},
		},
	}

	title := renderTitleBlocks(provider.freeze(ag, false))

	titleBlock := title[0].(*slackapi.SectionBlock)
	if !strings.Contains(titleBlock.Text.Text, "https://alertmanager.example.com/alerts/1") {
		t.Errorf("title should contain external URL as link, got %q", titleBlock.Text.Text)
	}
}

func TestSendProducesBlockKit(t *testing.T) {
	var postAttachments, postBlocks, postText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat.postMessage":
			r.ParseForm()
			postAttachments = r.FormValue("attachments")
			postBlocks = r.FormValue("blocks")
			postText = r.FormValue("text")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true, "channel": "C123", "ts": "111.222",
			})
		case "/chat.getPermalink":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true, "permalink": "https://slackapi.test/p",
			})
		}
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	dataStr, err := provider.Send(context.Background(), providers.NotificationRequest{
		Kind:       "channel",
		Target:     providers.NotificationTarget{Kind: "channel", ID: "C123"},
		AlertGroup: &model.AlertGroup{ID: "ag-fmt", Title: "FmtTest", Severity: "critical"},
		Editable:   true,
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Persisted data still parses cleanly and carries channel + timestamp.
	var data Data
	if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	if data.ChannelID == "" || data.Timestamp == "" {
		t.Errorf("expected non-empty ChannelID/Timestamp, got %+v", data)
	}

	// chat.postMessage payload must carry Block Kit content (section block inside the attachment).
	if !strings.Contains(postAttachments, `"type":"section"`) {
		t.Errorf("chat.postMessage attachments should contain Block Kit section, got: %s", postAttachments)
	}

	// Top-level blocks should contain the title
	if postBlocks == "" {
		t.Error("chat.postMessage should have non-empty blocks (title)")
	}
	if !strings.Contains(postBlocks, "FmtTest") {
		t.Errorf("chat.postMessage blocks should contain alert title, got: %s", postBlocks)
	}

	// Fallback text should be populated and contain the title
	if postText == "" {
		t.Error("chat.postMessage should have non-empty text (fallback)")
	}
	if !strings.Contains(postText, "FmtTest") {
		t.Errorf("chat.postMessage text should contain alert title, got: %s", postText)
	}
}

func TestUpdateProducesBlockKit(t *testing.T) {
	var lastUpdateAttachments, lastUpdateBlocks, lastUpdateText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.update" {
			r.ParseForm()
			lastUpdateAttachments = r.FormValue("attachments")
			lastUpdateBlocks = r.FormValue("blocks")
			lastUpdateText = r.FormValue("text")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true, "channel": "C123", "ts": "111.222",
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "channel": "C123", "ts": "111.222"})
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	ag := &model.AlertGroup{
		ID: "ag-dispatch", Title: "DispatchTest", Severity: "critical",
		Status: model.AlertGroupStatusAcknowledged,
	}

	// Data no longer carries a format flag — the renderer is always Block Kit.
	// Legacy persisted rows that contained "format":"" or "format":"v1" simply
	// have the unknown field ignored on unmarshal.
	t.Run("fresh row", func(t *testing.T) {
		data := Data{ChannelID: "C123", Timestamp: "111.222"}
		b, _ := json.Marshal(data)
		delivery := &model.NotificationDelivery{ID: "del-1", ProviderPayload: string(b)}

		_, err := provider.Update(context.Background(), delivery, ag)
		if err != nil {
			t.Fatalf("Update failed: %v", err)
		}
		if !strings.Contains(lastUpdateAttachments, `"type":"section"`) {
			t.Errorf("update attachments should contain Block Kit section, got: %s", lastUpdateAttachments)
		}
		if lastUpdateBlocks == "" {
			t.Error("update should have non-empty blocks (title)")
		}
		if !strings.Contains(lastUpdateBlocks, "DispatchTest") {
			t.Errorf("update blocks should contain alert title, got: %s", lastUpdateBlocks)
		}
		if lastUpdateText == "" {
			t.Error("update should have non-empty text (fallback)")
		}
		if !strings.Contains(lastUpdateText, "DispatchTest") {
			t.Errorf("update text should contain alert title, got: %s", lastUpdateText)
		}
	})

	t.Run("legacy row with stray format field", func(t *testing.T) {
		// Hand-rolled JSON simulating a pre-deletion payload row.
		// Unknown "format" key is ignored by json.Unmarshal — the row is
		// still rendered with the (now sole) Block Kit renderer.
		delivery := &model.NotificationDelivery{ID: "del-2", ProviderPayload: `{"channel_id":"C123","timestamp":"111.222","format":"v1"}`}

		_, err := provider.Update(context.Background(), delivery, ag)
		if err != nil {
			t.Fatalf("Update failed: %v", err)
		}
		if !strings.Contains(lastUpdateAttachments, `"type":"section"`) {
			t.Errorf("legacy row should still upgrade to Block Kit on update, got: %s", lastUpdateAttachments)
		}
	})
}

func TestResolveProducesBlockKit(t *testing.T) {
	var lastUpdateAttachments, lastUpdateBlocks, lastUpdateText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.update" {
			r.ParseForm()
			lastUpdateAttachments = r.FormValue("attachments")
			lastUpdateBlocks = r.FormValue("blocks")
			lastUpdateText = r.FormValue("text")
		}
		// All calls succeed (postMessage for thread reply, update for main message)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "channel": "C123", "ts": "111.222"})
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	ag := &model.AlertGroup{
		ID: "ag-resolve-dispatch", Title: "ResolveDispatch", Severity: "critical",
		Status: model.AlertGroupStatusResolved,
	}

	t.Run("fresh row", func(t *testing.T) {
		data := Data{ChannelID: "C123", Timestamp: "111.222"}
		b, _ := json.Marshal(data)
		delivery := &model.NotificationDelivery{ID: "del-1", ProviderPayload: string(b)}

		err := provider.Resolve(context.Background(), delivery, ag)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		if !strings.Contains(lastUpdateAttachments, `"type":"section"`) {
			t.Errorf("resolve attachments should contain Block Kit section, got: %s", lastUpdateAttachments)
		}
		if lastUpdateBlocks == "" {
			t.Error("resolve should have non-empty blocks (title)")
		}
		if !strings.Contains(lastUpdateBlocks, "ResolveDispatch") {
			t.Errorf("resolve blocks should contain alert title, got: %s", lastUpdateBlocks)
		}
		if lastUpdateText == "" {
			t.Error("resolve should have non-empty text (fallback)")
		}
		if !strings.Contains(lastUpdateText, "ResolveDispatch") {
			t.Errorf("resolve text should contain alert title, got: %s", lastUpdateText)
		}
	})

	t.Run("legacy row with stray format field", func(t *testing.T) {
		delivery := &model.NotificationDelivery{ID: "del-2", ProviderPayload: `{"channel_id":"C123","timestamp":"111.222","format":""}`}

		err := provider.Resolve(context.Background(), delivery, ag)
		if err != nil {
			t.Fatalf("Resolve failed: %v", err)
		}
		if !strings.Contains(lastUpdateAttachments, `"type":"section"`) {
			t.Errorf("legacy row should still upgrade to Block Kit on resolve, got: %s", lastUpdateAttachments)
		}
	})
}

func TestRenderBodyAttachment_AlertListTruncation(t *testing.T) {
	// Create 10 alerts with very long URLs to exceed the 3000-char Block Kit limit
	var alerts []model.Alert
	longURL := "https://grafana.example.com/d/" + strings.Repeat("x", 200)
	for i := 0; i < 10; i++ {
		alerts = append(alerts, model.Alert{
			Status: model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": "VeryLongAlertName" + strings.Repeat("A", 50),
				"severity":  "critical",
			},
			Annotations: map[string]string{
				"dashboard": longURL,
				"runbook":   longURL,
			},
		})
	}

	provider := &Provider{}
	ag := &model.AlertGroup{
		ID: "ag-trunc", Title: "TruncTest", Severity: "critical", Alerts: alerts,
	}

	att := renderBodyAttachment(provider.freeze(ag, false), provider.interactive())

	// Alerts section block (index 0 in body attachment) must be ≤ 3000 chars
	alertsBlock := att.Blocks.BlockSet[0].(*slackapi.SectionBlock)
	text := alertsBlock.Text.Text
	if len(text) > 3000 {
		t.Errorf("alerts block text exceeds 3000 chars: %d", len(text))
	}

	// Must be valid UTF-8 (no broken multi-byte chars)
	if !utf8.ValidString(text) {
		t.Error("truncated text is not valid UTF-8")
	}

	// Should contain some alert content and truncation marker
	if !strings.Contains(text, "🔴") {
		t.Error("truncated text should still contain some alert content")
	}
	if !strings.Contains(text, "truncated") {
		t.Error("expected truncation marker in text")
	}

	// buildAlertList itself should NOT truncate (v1 backward compat)
	rawList := buildAlertList(provider.freeze(ag, false).Alerts)
	if strings.Contains(rawList, "truncated") {
		t.Error("buildAlertList should not truncate — that's renderBodyAttachment's job")
	}
}

// countingTeamLookup is a providers.TeamLookup that records how often it ran, so a test
// can assert the lookup was skipped, not just that its answer was ignored.
type countingTeamLookup struct {
	calls     int
	onboarded bool
	err       error
}

func (c *countingTeamLookup) fn(string) (bool, error) {
	c.calls++
	return c.onboarded, c.err
}

// findActionBlock reports whether the attachment offers Ack/Resolve.
func findActionBlock(att slackapi.Attachment) *slackapi.ActionBlock {
	for _, b := range att.Blocks.BlockSet {
		if ab, ok := b.(*slackapi.ActionBlock); ok {
			return ab
		}
	}
	return nil
}

// findUnknownTeamNotice returns the notice section's text, or "" when absent.
// The footer is a context block, so only sections are candidates.
func findUnknownTeamNotice(att slackapi.Attachment) string {
	for _, b := range att.Blocks.BlockSet {
		sb, ok := b.(*slackapi.SectionBlock)
		if !ok || sb.Text == nil {
			continue
		}
		if strings.Contains(sb.Text.Text, "Unknown team") {
			return sb.Text.Text
		}
	}
	return ""
}

func teamGateAlertGroup(teamID string) *model.AlertGroup {
	return &model.AlertGroup{
		ID:       "ag-team-gate",
		Title:    "High Latency",
		TeamID:   teamID,
		Severity: "critical",
		Status:   model.AlertGroupStatusTriggered,
		Alerts: []model.Alert{
			{Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "A1", "severity": "critical"}},
		},
	}
}

// Buttons are offered only where somebody can actually press them. An alert
// group's team is a label off the alert, so it can name a team that was never
// set up here, and for that team RBAC denies everyone but a global admin.
func TestSlack_TeamGate(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		isResolved  bool
		lookup      *countingTeamLookup
		useNilHook  bool
		wantButtons bool
		wantNotice  bool
		wantCalls   int
	}{
		{
			name:        "onboarded team keeps its buttons",
			interactive: true,
			lookup:      &countingTeamLookup{onboarded: true},
			wantButtons: true,
			wantCalls:   1,
		},
		{
			name:        "unknown team gets the notice instead of buttons",
			interactive: true,
			lookup:      &countingTeamLookup{onboarded: false},
			wantNotice:  true,
			wantCalls:   1,
		},
		{
			// A database blip must not strip buttons from teams that are set up
			// and announce it in the channel everyone reads.
			name:        "a failing lookup degrades to onboarded",
			interactive: true,
			lookup:      &countingTeamLookup{onboarded: false, err: errors.New("db down")},
			wantButtons: true,
			wantCalls:   1,
		},
		{
			name:        "no lookup wired up behaves as before",
			interactive: true,
			useNilHook:  true,
			wantButtons: true,
		},
		{
			// Interactivity off is the administrator's decision, not a fault, so
			// it earns no notice - and no query either.
			name:        "interactivity off gives neither buttons nor notice",
			interactive: false,
			lookup:      &countingTeamLookup{onboarded: false},
			wantCalls:   0,
		},
		{
			name:        "resolved card gives neither buttons nor notice",
			interactive: true,
			isResolved:  true,
			lookup:      &countingTeamLookup{onboarded: false},
			wantCalls:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &Provider{
				tokenSource: &mockTokenSource{token: "tok", interactive: tt.interactive},
				selfURL:     "https://tokay.example",
			}
			if !tt.useNilHook {
				provider.teamLookup = tt.lookup.fn
			}

			att := renderBodyAttachment(
				provider.freeze(teamGateAlertGroup("payments"), tt.isResolved),
				provider.interactive())

			if got := findActionBlock(att) != nil; got != tt.wantButtons {
				t.Errorf("buttons present = %v, want %v", got, tt.wantButtons)
			}

			notice := findUnknownTeamNotice(att)
			if got := notice != ""; got != tt.wantNotice {
				t.Errorf("notice present = %v, want %v (notice=%q)", got, tt.wantNotice, notice)
			}
			if tt.wantNotice && !strings.Contains(notice, "payments") {
				t.Errorf("notice does not name the team label: %q", notice)
			}

			if !tt.useNilHook && tt.lookup.calls != tt.wantCalls {
				t.Errorf("team lookup ran %d times, want %d", tt.lookup.calls, tt.wantCalls)
			}
		})
	}
}

// The team label is free text off the alert and it lands inside a code span, so
// a backtick would close that span early and a newline would split the block.
func TestSlack_UnknownTeamNotice_SanitisesLabel(t *testing.T) {
	tests := []struct {
		name        string
		teamID      string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "backtick cannot close the code span",
			teamID:      "pay`ments",
			wantAbsent:  []string{"pay`ments"},
			wantPresent: []string{"pay'ments"},
		},
		{
			name:       "line breaks do not split the block",
			teamID:     "pay\nments\rteam",
			wantAbsent: []string{"\npay", "ments\rteam"},
		},
		{
			name:        "mrkdwn specials are escaped",
			teamID:      "a<b>&c",
			wantAbsent:  []string{"a<b>&c"},
			wantPresent: []string{"a&lt;b&gt;&amp;c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unknownTeamNotice(noticeState(tt.teamID, "https://tokay.example"))
			for _, want := range tt.wantPresent {
				if !strings.Contains(got, want) {
					t.Errorf("notice %q does not contain %q", got, want)
				}
			}
			for _, bad := range tt.wantAbsent {
				if strings.Contains(got, bad) {
					t.Errorf("notice %q still contains raw %q", got, bad)
				}
			}
		})
	}

	t.Run("a long label is capped", func(t *testing.T) {
		long := strings.Repeat("x", maxTeamLabelLen*3)
		got := unknownTeamNotice(noticeState(long, "https://tokay.example"))
		if strings.Contains(got, long) {
			t.Error("full oversized label was interpolated")
		}
		if !strings.Contains(got, "…") {
			t.Errorf("truncated label is not marked as truncated: %q", got)
		}
	})

	t.Run("truncation cannot sever an escape entity", func(t *testing.T) {
		// Every rune is escaped to 5 characters, so a cut applied after escaping
		// would land inside an entity.
		got := sanitizeTeamLabel(strings.Repeat("&", maxTeamLabelLen*2))
		if strings.HasSuffix(strings.TrimSuffix(got, "…"), "&am") {
			t.Errorf("entity was severed: %q", got)
		}
		if strings.Count(got, "&amp;") != maxTeamLabelLen {
			t.Errorf("expected %d whole entities, got %q", maxTeamLabelLen, got)
		}
	})

	t.Run("the link is omitted without a selfURL", func(t *testing.T) {
		withURL := unknownTeamNotice(noticeState("payments", "https://tokay.example"))
		if !strings.Contains(withURL, "/#/cfg/teams|Set up the team>") {
			t.Errorf("expected a setup link, got %q", withURL)
		}
		without := unknownTeamNotice(noticeState("payments", ""))
		if strings.Contains(without, "Set up the team") {
			t.Errorf("expected no link without selfURL, got %q", without)
		}
	})
}

// noticeState is the little of a snapshot the unknown-team notice reads: the
// label the alert carried, and where a person would go to fix it.
func noticeState(teamID, selfURL string) keys.SnapshotInput {
	state := keys.SnapshotInput{AlertGroupID: "ag", Status: keys.GroupTriggered}
	if teamID != "" {
		label := teamID
		state.TeamLabel = &label
	}
	if selfURL != "" {
		setup := selfURL + "/#/cfg/teams"
		state.TeamSetupURL = &setup
	}
	return state
}
