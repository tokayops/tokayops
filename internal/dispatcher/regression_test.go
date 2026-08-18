package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
)

// TestRegression_Resolve_ErrorLeak verifies that SlackProvider.Resolve()
// returns nil even when the timeline update fails.
// Bug: return err on the last line leaked the timeline update error,
// causing the resolution job step to fail permanently.
func TestRegression_Resolve_ErrorLeak(t *testing.T) {
	// Mock Slack API: postMessage succeeds, but chat.update fails
	updateCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/chat.postMessage" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C123",
				"ts":      "100.200",
			})
			return
		}
		if r.URL.Path == "/chat.update" {
			updateCalls++
			// First update (main message) succeeds
			if updateCalls == 1 {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"ok":      true,
					"channel": "C123",
					"ts":      "100.200",
				})
				return
			}
			// Second update (timeline) fails with msg_too_long
			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "msg_too_long",
			})
			return
		}

		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	ctx := context.Background()

	data := SlackData{
		ChannelID:         "C123",
		Timestamp:         "100.200",
		TimelineTimestamp: "100.300",
	}
	dataBytes, _ := json.Marshal(data)

	ag := &model.AlertGroup{
		ID:       "ag-resolve-err",
		Title:    "Test Resolve Error",
		Severity: "critical",
		Status:   model.AlertGroupStatusResolved,
		Alerts: []model.Alert{
			{Labels: map[string]string{"alertname": "A1"}, Status: model.AlertStatusResolved},
		},
		TimelineEvents: []*model.TimelineEvent{
			{Type: model.TimelineEventCreated, Message: "created"},
		},
	}
	delivery := &model.NotificationDelivery{ID: "del-1", ProviderPayload: string(dataBytes)}

	err := provider.Resolve(ctx, delivery, ag)

	// EXPECTED: nil (timeline error is non-critical, logged but not returned)
	// BUG (unfixed): returns "msg_too_long" error → step fails permanently
	if err != nil {
		t.Errorf("Resolve() should return nil even when timeline update fails, got: %v", err)
	}
}

// alertsBlockText extracts the markdown text from the severity+alerts section
// inside the body attachment returned by renderBodyAttachment. The layout is:
//
//	BlockSet[0] = severity + alerts section  ← we want this
//	BlockSet[1] = divider (only if interactive buttons present)
//	BlockSet[2] = action block (only if interactive)
//	BlockSet[N-1] = context footer
func alertsBlockText(t *testing.T, att slack.Attachment) string {
	t.Helper()
	if len(att.Blocks.BlockSet) < 1 {
		t.Fatalf("expected at least 1 block, got %d", len(att.Blocks.BlockSet))
	}
	sec, ok := att.Blocks.BlockSet[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("block 0 is not SectionBlock, got %T", att.Blocks.BlockSet[0])
	}
	return sec.Text.Text
}

// titleBlockText extracts the markdown text from the title blocks
// returned by renderTitleBlocks.
func titleBlockText(t *testing.T, blocks []slack.Block) string {
	t.Helper()
	if len(blocks) < 1 {
		t.Fatalf("expected at least 1 title block, got %d", len(blocks))
	}
	sec, ok := blocks[0].(*slack.SectionBlock)
	if !ok {
		t.Fatalf("title block 0 is not SectionBlock, got %T", blocks[0])
	}
	return sec.Text.Text
}

// TestRegression_RenderMessageV2_Truncation verifies that the v2 renderer
// truncates the alert list when there are more than 10 alerts.
// Bug guard: no truncation → Slack message exceeds character limit → msg_too_long error.
func TestRegression_RenderMessageV2_Truncation(t *testing.T) {
	provider := &SlackProvider{}

	// Create 15 alerts
	alerts := make([]model.Alert, 15)
	for i := 0; i < 15; i++ {
		alerts[i] = model.Alert{
			Status: model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
				"severity":  "critical",
			},
		}
	}

	ag := &model.AlertGroup{
		ID:       "ag-truncate",
		Title:    "ManyAlerts",
		Severity: "critical",
		Alerts:   alerts,
	}

	titleBlocks := provider.renderTitleBlocks(ag, false)
	att := provider.renderBodyAttachment(ag, false)
	body := alertsBlockText(t, att)

	// Count bullet points (alert lines)
	lines := strings.Split(body, "\n")
	bulletCount := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "•") {
			bulletCount++
		}
	}

	// max 10 alert lines + "... and 5 more alerts"
	if bulletCount > 10 {
		t.Errorf("Expected at most 10 alert lines, got %d (message too long for Slack)", bulletCount)
	}

	// Should contain truncation notice
	if !strings.Contains(body, "more alerts") {
		t.Errorf("Expected '... and N more alerts' truncation notice, got: %s", body)
	}

	// Firing count should still reflect ALL alerts (not just rendered ones)
	title := titleBlockText(t, titleBlocks)
	if !strings.Contains(title, "15 Firing") {
		t.Errorf("Expected '15 Firing' in title (all alerts counted), got: %s", title)
	}
}

// TestRegression_RenderMessageV2_SmallGroup verifies no truncation for small groups.
func TestRegression_RenderMessageV2_SmallGroup(t *testing.T) {
	provider := &SlackProvider{}

	alerts := make([]model.Alert, 5)
	for i := 0; i < 5; i++ {
		alerts[i] = model.Alert{
			Status: model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
				"severity":  "critical",
			},
		}
	}

	ag := &model.AlertGroup{
		ID:       "ag-small",
		Title:    "SmallGroup",
		Severity: "critical",
		Alerts:   alerts,
	}

	att := provider.renderBodyAttachment(ag, false)
	body := alertsBlockText(t, att)

	// All 5 alerts should be rendered
	lines := strings.Split(body, "\n")
	bulletCount := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "•") {
			bulletCount++
		}
	}
	if bulletCount != 5 {
		t.Errorf("Expected 5 alert lines for small group, got %d", bulletCount)
	}

	// No truncation notice
	if strings.Contains(body, "more alerts") {
		t.Errorf("Should not have truncation notice for small group, got: %s", body)
	}
}

// TestRegression_RenderMessageV2_SlackUsersMentionsPreserved verifies that
// slack_user mentions are collected from ALL alerts, not just the first 10.
func TestRegression_RenderMessageV2_SlackUsersMentionsPreserved(t *testing.T) {
	provider := &SlackProvider{}

	alerts := make([]model.Alert, 12)
	for i := 0; i < 12; i++ {
		alerts[i] = model.Alert{
			Status: model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
				"severity":  "critical",
			},
		}
	}
	// Put slack_user on alert #12 (beyond truncation limit)
	alerts[11].Labels["slack_user"] = "U99999"

	ag := &model.AlertGroup{
		ID:       "ag-mentions",
		Title:    "MentionsTest",
		Severity: "critical",
		Alerts:   alerts,
	}

	att := provider.renderBodyAttachment(ag, false)
	body := alertsBlockText(t, att)

	// slack_user mention from alert #12 should still be included
	if !strings.Contains(body, "<@U99999>") {
		t.Errorf("Expected slack_user mention from alert beyond truncation limit, got: %s", body)
	}
}

// TestRegression_RenderAlertSummaries_Truncation verifies that
// renderAlertSummaries() truncates both the number of summaries
// and the description length.
func TestRegression_RenderAlertSummaries_Truncation(t *testing.T) {
	provider := &SlackProvider{}

	alerts := make([]model.Alert, 15)
	for i := 0; i < 15; i++ {
		alerts[i] = model.Alert{
			Status: model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
			},
			Annotations: map[string]string{
				"description": fmt.Sprintf("Description for alert %d", i+1),
			},
		}
	}

	ag := &model.AlertGroup{
		ID:     "ag-sum-trunc",
		Alerts: alerts,
	}

	result := provider.renderAlertSummaries(ag)

	// Count summary lines (each starts with icon)
	lines := strings.Split(result, "\n")
	summaryCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "🔴") || strings.HasPrefix(trimmed, "🟢") {
			summaryCount++
		}
	}

	// EXPECTED: max 10 summaries + truncation notice
	// BUG (unfixed): all 15 summaries rendered
	if summaryCount > 10 {
		t.Errorf("Expected at most 10 alert summaries, got %d", summaryCount)
	}
	if !strings.Contains(result, "more alert details") {
		t.Error("Expected truncation notice for alert summaries")
	}
}

// TestRegression_RenderAlertSummaries_LongDescription verifies that
// very long alert descriptions are truncated.
func TestRegression_RenderAlertSummaries_LongDescription(t *testing.T) {
	provider := &SlackProvider{}

	longDesc := strings.Repeat("x", 500) // 500 chars

	ag := &model.AlertGroup{
		ID: "ag-long-desc",
		Alerts: []model.Alert{
			{
				Status:      model.AlertStatusFiring,
				Labels:      map[string]string{"alertname": "LongAlert"},
				Annotations: map[string]string{"description": longDesc},
			},
		},
	}

	result := provider.renderAlertSummaries(ag)

	// EXPECTED: description truncated to ~200 chars + "..."
	// BUG (unfixed): full 500 chars rendered
	if strings.Contains(result, longDesc) {
		t.Error("Long description should be truncated, but full text was found")
	}
	if !strings.Contains(result, "...") {
		t.Error("Expected '...' suffix for truncated description")
	}
	// The result should be significantly shorter than the original
	if len(result) > 300 {
		t.Errorf("Result too long (%d chars), expected truncation to ~200 char description", len(result))
	}
}
