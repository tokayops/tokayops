package slack

import (
	"fmt"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// alertsBlockText extracts the markdown text from the severity+alerts section
// inside the body attachment returned by renderBodyAttachment. The layout is:
//
//	BlockSet[0] = severity + alerts section  ← we want this
//	BlockSet[1] = divider (only if interactive buttons present)
//	BlockSet[2] = action block (only if interactive)
//	BlockSet[N-1] = context footer
func alertsBlockText(t *testing.T, att slackapi.Attachment) string {
	t.Helper()
	if len(att.Blocks.BlockSet) < 1 {
		t.Fatalf("expected at least 1 block, got %d", len(att.Blocks.BlockSet))
	}
	sec, ok := att.Blocks.BlockSet[0].(*slackapi.SectionBlock)
	if !ok {
		t.Fatalf("block 0 is not SectionBlock, got %T", att.Blocks.BlockSet[0])
	}
	return sec.Text.Text
}

// titleBlockText extracts the markdown text from the title blocks
// returned by renderTitleBlocks.
func titleBlockText(t *testing.T, blocks []slackapi.Block) string {
	t.Helper()
	if len(blocks) < 1 {
		t.Fatalf("expected at least 1 title block, got %d", len(blocks))
	}
	sec, ok := blocks[0].(*slackapi.SectionBlock)
	if !ok {
		t.Fatalf("title block 0 is not SectionBlock, got %T", blocks[0])
	}
	return sec.Text.Text
}

// TestRegression_RenderMessageV2_Truncation verifies that the v2 renderer
// truncates the alert list when there are more than 10 alerts.
// Bug guard: no truncation → Slack message exceeds character limit → msg_too_long error.
func TestRegression_RenderMessageV2_Truncation(t *testing.T) {
	provider := &Provider{}

	// Create 15 alerts
	alerts := make([]model.Alert, 15)
	for i := 0; i < 15; i++ {
		alerts[i] = model.Alert{
			Fingerprint: fmt.Sprintf("fp-%d", i),
			StartsAt:    alertStart,
			Status:      model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
				"severity":  "critical",
			},
		}
	}

	ag := &model.AlertGroup{
		Status:   model.AlertGroupStatusTriggered,
		ID:       "ag-truncate",
		Title:    "ManyAlerts",
		Severity: "critical",
		Alerts:   alerts,
	}

	titleBlocks := renderTitleBlocks(frozen(t, ag))
	att := renderBodyAttachment(frozen(t, ag), provider.interactive())
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
	provider := &Provider{}

	alerts := make([]model.Alert, 5)
	for i := 0; i < 5; i++ {
		alerts[i] = model.Alert{
			Fingerprint: fmt.Sprintf("fp-%d", i),
			StartsAt:    alertStart,
			Status:      model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
				"severity":  "critical",
			},
		}
	}

	ag := &model.AlertGroup{
		Status:   model.AlertGroupStatusTriggered,
		ID:       "ag-small",
		Title:    "SmallGroup",
		Severity: "critical",
		Alerts:   alerts,
	}

	att := renderBodyAttachment(frozen(t, ag), provider.interactive())
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
	provider := &Provider{}

	alerts := make([]model.Alert, 12)
	for i := 0; i < 12; i++ {
		alerts[i] = model.Alert{
			Fingerprint: fmt.Sprintf("fp-%d", i),
			StartsAt:    alertStart,
			Status:      model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
				"severity":  "critical",
			},
		}
	}
	// Put slack_user on alert #12 (beyond truncation limit)
	alerts[11].Labels["slack_user"] = "U99999"

	ag := &model.AlertGroup{
		Status:   model.AlertGroupStatusTriggered,
		ID:       "ag-mentions",
		Title:    "MentionsTest",
		Severity: "critical",
		Alerts:   alerts,
	}

	att := renderBodyAttachment(frozen(t, ag), provider.interactive())
	body := alertsBlockText(t, att)

	// slack_user mention from alert #12 should still be included
	if !strings.Contains(body, "<@U99999>") {
		t.Errorf("Expected slack_user mention from alert beyond truncation limit, got: %s", body)
	}
}

// TestRegression_AlertList_Truncation verifies that the card caps how many
// alerts it lists.
//
// The descriptions moved into that list on 2026-08-25, from the threaded
// message they used to have to themselves, so the cap that used to belong to
// the summaries section is this one now.
func TestRegression_AlertList_Truncation(t *testing.T) {
	alerts := make([]model.Alert, 15)
	for i := 0; i < 15; i++ {
		alerts[i] = model.Alert{
			Fingerprint: fmt.Sprintf("fp-%d", i),
			StartsAt:    alertStart,
			Status:      model.AlertStatusFiring,
			Labels: map[string]string{
				"alertname": fmt.Sprintf("Alert%d", i+1),
			},
			Annotations: map[string]string{
				"description": fmt.Sprintf("Description for alert %d", i+1),
			},
		}
	}

	ag := &model.AlertGroup{
		Status: model.AlertGroupStatusTriggered,
		ID:     "ag-sum-trunc",
		Alerts: alerts,
	}

	state := frozen(t, ag)
	result := buildAlertList(state.Alerts, state.DisplayTimezone)

	// Count alert lines (each bullet starts with an icon)
	lines := strings.Split(result, "\n")
	summaryCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "• 🔴") || strings.HasPrefix(trimmed, "• 🟢") {
			summaryCount++
		}
	}

	if summaryCount > 10 {
		t.Errorf("Expected at most 10 alerts listed, got %d", summaryCount)
	}
	if !strings.Contains(result, "more alerts") {
		t.Error("Expected a notice for the alerts that did not fit")
	}
	// Every listed alert says what is wrong, not just what it is called.
	if !strings.Contains(result, "Description for alert 1") {
		t.Error("the alert's description did not reach the card")
	}
}

// TestRegression_AlertList_LongDescription verifies that a very long alert
// description is shortened rather than pushing the alerts below it out of the
// card: the body is capped, and what is cut is cut here on purpose.
func TestRegression_AlertList_LongDescription(t *testing.T) {

	longDesc := strings.Repeat("x", 500) // 500 chars

	ag := &model.AlertGroup{
		Status: model.AlertGroupStatusTriggered,
		ID:     "ag-long-desc",
		Alerts: []model.Alert{
			{
				Fingerprint: "fp-long",
				StartsAt:    alertStart,
				Status:      model.AlertStatusFiring,
				Labels:      map[string]string{"alertname": "LongAlert"},
				Annotations: map[string]string{"description": longDesc},
			},
		},
	}

	state := frozen(t, ag)
	result := buildAlertList(state.Alerts, state.DisplayTimezone)

	if strings.Contains(result, longDesc) {
		t.Error("Long description should be truncated, but full text was found")
	}

	// And the cut happens before the digest, not here: two descriptions that
	// differ only past the bound have to produce the same request. Rendering
	// them apart would mean a revision raised for a difference nobody sees.
	other := ag.Alerts[0].Annotations["description"] + "-and-a-different-tail"
	ag.Alerts[0].Annotations["description"] = other
	twin := frozen(t, ag)
	if got := buildAlertList(twin.Alerts, twin.DisplayTimezone); got != result {
		t.Errorf("two descriptions cut to the same value rendered differently:\n%s\n%s",
			result, got)
	}
	if !strings.Contains(result, "...") {
		t.Error("Expected '...' suffix for truncated description")
	}
	if len(result) > 300 {
		t.Errorf("Result too long (%d chars), expected the description to be cut", len(result))
	}
}

// frozen is the state a card is drawn from, built the way admission builds it.
//
// Through SnapshotOf rather than by hand: what a channel renders is what the
// domain froze, and a test that assembled the input itself could draw a card
// from a state no producer could ever admit. There is only one door now - the
// tolerant freeze that drew from a live row went with the executors that
// needed it.
func frozen(t *testing.T, ag *model.AlertGroup) keys.SnapshotInput {
	return frozenFor(t, ag, false, true)
}

func frozenFor(t *testing.T, ag *model.AlertGroup, resolved, onboarded bool) keys.SnapshotInput {
	t.Helper()
	snapshot, err := providers.SnapshotOf(providers.GroupView{
		Group: ag, IsResolved: resolved, SelfURL: "https://tokay.example",
		TeamOnboarded: onboarded, Zone: "UTC",
	})
	if err != nil {
		t.Fatalf("freeze the state: %v", err)
	}
	return snapshot.Content()
}

// alertStart is when the fixtures' alerts began. Fixed, because a card prints
// it and a test that rendered "now" would assert against the clock.
var alertStart = time.Unix(1700000000, 0).UTC()
