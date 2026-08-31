package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
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
func newSlackProviderForTest(token, apiURL string) *Provider {
	return &Provider{
		tokenSource: &mockTokenSource{token: token},
		client:      slackapi.New(token, slackapi.OptionAPIURL(apiURL)),
		cachedToken: token,
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

	provider := newSlackProviderForTest("test-token", server.URL+"/")
	if err := provider.SendDM(context.Background(), "U_TARGET", "hello there"); err != nil {
		t.Fatalf("send a direct message: %v", err)
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
	provider := newSlackProviderForTest("test-token", server.URL+"/")

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
	provider := newSlackProviderForTest("test-token", server.URL+"/")

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

// TestSlackProvider_MissingToken verifies that an unconfigured token surfaces
// as ErrNoToken - a permanent refusal, not something to retry.
func TestSlackProvider_MissingToken(t *testing.T) {
	provider := &Provider{tokenSource: &mockTokenSource{token: ""}}

	if err := provider.SendDM(context.Background(), "U1", "x"); !errors.Is(err, ErrNoToken) {
		t.Errorf("dm send: expected ErrNoToken, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Block Kit v2 rendering tests
// ---------------------------------------------------------------------------

func TestRenderTitleAndBody_Triggered(t *testing.T) {
	provider := &Provider{tokenSource: &mockTokenSource{interactive: true}}
	ag := &model.AlertGroup{
		ID:       "ag-v2-1",
		Title:    "HighCPU",
		Status:   model.AlertGroupStatusTriggered,
		Severity: "critical",
		Alerts: []model.Alert{
			{
				Fingerprint: "fp-5",
				StartsAt:    alertStart,
				Status:      model.AlertStatusFiring,
				Labels:      map[string]string{"alertname": "HighCPU", "severity": "critical"},
				Annotations: map[string]string{
					"dashboard": "https://grafana/d/cpu",
					"runbook":   "https://wiki/cpu",
				},
			},
		},
	}

	title := renderTitleBlocks(frozen(t, ag))
	att := renderBodyAttachment(frozen(t, ag), provider.interactive())

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
			{Fingerprint: "fp-1", StartsAt: alertStart, Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "HighMem", "severity": "warning"}},
		},
	}

	title := renderTitleBlocks(frozen(t, ag))
	att := renderBodyAttachment(frozen(t, ag), provider.interactive())

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
			{Fingerprint: "fp-2", StartsAt: alertStart, Status: model.AlertStatusResolved, Labels: map[string]string{"alertname": "DiskFull"}},
		},
	}

	title := renderTitleBlocks(frozenFor(t, ag, true, true))
	att := renderBodyAttachment(frozenFor(t, ag, true, true), provider.interactive())

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
		Status:   model.AlertGroupStatusTriggered,
		ID:       "ag-v2-mentions",
		Title:    "MentionTest",
		Severity: "critical",
		Alerts: []model.Alert{
			{
				Fingerprint: "fp-6",
				StartsAt:    alertStart,
				Status:      model.AlertStatusFiring,
				Labels:      map[string]string{"alertname": "A1", "slack_user": "U12345"},
			},
			{
				Fingerprint: "fp-7",
				StartsAt:    alertStart,
				Status:      model.AlertStatusFiring,
				Labels:      map[string]string{"alertname": "A2", "slack_user": "S67890"},
			},
		},
	}

	att := renderBodyAttachment(frozen(t, ag), provider.interactive())

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
	ag := &model.AlertGroup{
		Status:      model.AlertGroupStatusTriggered,
		ID:          "ag-v2-url",
		Title:       "URLTest",
		Severity:    "critical",
		ExternalURL: "https://alertmanager.example.com/alerts/1",
		Alerts: []model.Alert{
			{Fingerprint: "fp-3", StartsAt: alertStart, Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "A1"}},
		},
	}

	title := renderTitleBlocks(frozen(t, ag))

	titleBlock := title[0].(*slackapi.SectionBlock)
	if !strings.Contains(titleBlock.Text.Text, "https://alertmanager.example.com/alerts/1") {
		t.Errorf("title should contain external URL as link, got %q", titleBlock.Text.Text)
	}
}

func TestRenderBodyAttachment_AlertListTruncation(t *testing.T) {
	// Create 10 alerts with very long URLs to exceed the 3000-char Block Kit limit
	var alerts []model.Alert
	longURL := "https://grafana.example.com/d/" + strings.Repeat("x", 200)
	for i := 0; i < 10; i++ {
		alerts = append(alerts, model.Alert{
			Fingerprint: fmt.Sprintf("fp-long-%d", i),
			StartsAt:    alertStart,
			Status:      model.AlertStatusFiring,
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
		Status: model.AlertGroupStatusTriggered,
		ID:     "ag-trunc", Title: "TruncTest", Severity: "critical", Alerts: alerts,
	}

	att := renderBodyAttachment(frozen(t, ag), provider.interactive())

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
	state := frozen(t, ag)
	rawList := buildAlertList(state.Alerts, state.DisplayTimezone)
	if strings.Contains(rawList, "truncated") {
		t.Error("buildAlertList should not truncate - that's renderBodyAttachment's job")
	}
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
			{Fingerprint: "fp-4", StartsAt: alertStart, Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "A1", "severity": "critical"}},
		},
	}
}

// TestSlack_TeamGate: whether a card carries buttons, or the notice that says
// why it cannot.
//
// Both halves come from the state now. Whether the team is onboarded was
// decided when the state was frozen and travels in the snapshot; whether this
// installation offers interactivity at all is the administrator's setting, read
// per attempt. Nothing asks a database mid-render - the lookup that used to
// happen here belonged to the freeze that drew a card from a live row, and a
// blip in it could strip the buttons off every team that IS set up.
func TestSlack_TeamGate(t *testing.T) {
	tests := []struct {
		name        string
		interactive bool
		isResolved  bool
		onboarded   bool
		wantButtons bool
		wantNotice  bool
	}{
		{
			name:        "onboarded team keeps its buttons",
			interactive: true, onboarded: true, wantButtons: true,
		},
		{
			name:        "a team TokayOps does not have gets the notice instead of buttons",
			interactive: true, onboarded: false, wantNotice: true,
		},
		{
			// Interactivity off is the administrator's decision, not a fault,
			// so it earns no notice either.
			name:        "interactivity off gives neither buttons nor notice",
			interactive: false, onboarded: true,
		},
		{
			name:        "a resolved card gives neither buttons nor notice",
			interactive: true, isResolved: true, onboarded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &Provider{
				tokenSource: &mockTokenSource{token: "tok", interactive: tt.interactive},
			}
			state := frozenFor(t, teamGateAlertGroup("payments"), tt.isResolved, tt.onboarded)

			att := renderBodyAttachment(state, provider.interactive())

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
