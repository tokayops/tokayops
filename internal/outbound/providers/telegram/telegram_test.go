package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// mockTelegramTokenSource implements TokenSource for testing.
// interactiveOff is negated on purpose: the zero value has to mean "buttons on",
// which is what every existing test that renders a card expects.
type mockTelegramTokenSource struct {
	token          string
	interactiveOff bool
}

func (m *mockTelegramTokenSource) GetTelegramToken() string { return m.token }

func (m *mockTelegramTokenSource) GetTelegramInteractive() bool { return !m.interactiveOff }

// fakeBotAPI returns an httptest server that answers sendMessage/editMessageText
// and counts calls per method. sendMessage returns the given message_id.
func fakeBotAPI(t *testing.T, sendMessageID int, counts map[string]int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			counts["sendMessage"]++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":     true,
				"result": map[string]interface{}{"message_id": sendMessageID},
			})
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			counts["editMessageText"]++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": true})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func testAlertGroup() *model.AlertGroup {
	return &model.AlertGroup{
		ID:       "ag-1",
		Title:    "High Latency",
		Severity: "critical",
		Alerts: []model.Alert{
			{Labels: map[string]string{"alertname": "A1", "severity": "critical"}, Status: model.AlertStatusFiring},
		},
	}
}

func TestTelegram_SendPostsTheCard(t *testing.T) {
	counts := map[string]int{}
	server := fakeBotAPI(t, 42, counts)
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "https://tokay.example", WithBaseURL(server.URL))
	ctx := context.Background()
	ag := testAlertGroup()

	payload, err := p.Send(ctx, providers.NotificationRequest{
		Target:     providers.NotificationTarget{Kind: "channel", ID: "@chan"},
		AlertGroup: ag,
		Editable:   true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	data, ok := parseData(payload)
	if !ok || data.ChatID != "@chan" || data.MessageID != 42 {
		t.Fatalf("payload = %q, parsed=%+v ok=%v", payload, data, ok)
	}

	if counts["sendMessage"] != 1 || counts["editMessageText"] != 0 {
		t.Errorf("call counts = %v, want sendMessage=1 and nothing else", counts)
	}
}

func TestTelegram_Send_EmptyMessageID_Errors(t *testing.T) {
	counts := map[string]int{}
	server := fakeBotAPI(t, 0, counts) // message_id 0 → editable contract violation
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "", WithBaseURL(server.URL))
	_, err := p.Send(context.Background(), providers.NotificationRequest{
		Target:     providers.NotificationTarget{Kind: "channel", ID: "@chan"},
		AlertGroup: testAlertGroup(),
		Editable:   true,
	})
	if err == nil {
		t.Fatal("expected error when sendMessage returns message_id 0")
	}
}

func TestTelegram_SendDM_FireAndForget(t *testing.T) {
	counts := map[string]int{}
	server := fakeBotAPI(t, 7, counts)
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "", WithBaseURL(server.URL))
	payload, err := p.Send(context.Background(), providers.NotificationRequest{
		Target:  providers.NotificationTarget{Kind: "user", ID: "123456"},
		Message: "handoff: you are on call",
	})
	if err != nil {
		t.Fatalf("Send DM: %v", err)
	}
	if payload != "" {
		t.Errorf("DM must return empty payload, got %q", payload)
	}
	if counts["sendMessage"] != 1 {
		t.Errorf("sendMessage calls = %d, want 1", counts["sendMessage"])
	}
}

func TestTelegram_Send_Guards(t *testing.T) {
	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "", WithBaseURL("http://unused.invalid"))
	ctx := context.Background()

	if _, err := p.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "channel", ID: "@c"}}); err == nil {
		t.Error("channel send with nil AlertGroup should error")
	}
	if _, err := p.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "user", ID: "1"}}); err == nil {
		t.Error("user send with empty Message should error")
	}
	if _, err := p.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "sms", ID: "1"}}); err == nil {
		t.Error("unsupported target kind should error")
	}
}

func TestTelegram_MissingToken_Permanent(t *testing.T) {
	p := NewProvider(&mockTelegramTokenSource{token: ""}, "", WithBaseURL("http://unused.invalid"))
	ctx := context.Background()

	_, err := p.Send(ctx, providers.NotificationRequest{Target: providers.NotificationTarget{Kind: "channel", ID: "@c"}, AlertGroup: testAlertGroup(), Editable: true})
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("channel send: got %v, want ErrNoToken", err)
	}
	// Whether the dispatcher treats that as permanent is the dispatcher's rule
	// and is asserted there: a channel that classified its own errors for its
	// caller would be two answers to one question.
}

func TestTelegram_RenderCard_HTMLEscaping(t *testing.T) {
	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "https://tokay.example", WithBaseURL("http://unused.invalid"))
	ag := &model.AlertGroup{
		ID:       `ag&"1`,
		Title:    "T",
		Severity: "<sev>",
		Alerts: []model.Alert{
			{Labels: map[string]string{"alertname": `A & B <prod> "x"`, "severity": "critical"}, Status: model.AlertStatusFiring},
		},
	}
	out := p.renderCard(ag, false)

	if strings.Contains(out, "<prod>") || strings.Contains(out, `B "x"`) {
		t.Errorf("dynamic text not escaped: %q", out)
	}
	if !strings.Contains(out, "A &amp; B &lt;prod&gt; &#34;x&#34;") {
		t.Errorf("expected escaped alertname in output: %q", out)
	}
	// Footer href attribute must be attribute-safe (quotes/ampersands escaped).
	if !strings.Contains(out, `&amp;&#34;1`) {
		t.Errorf("footer attribute/text not escaped for ag.ID: %q", out)
	}
	if !strings.Contains(out, `href="https://tokay.example/#/ops/alert-groups/`) {
		t.Errorf("expected footer deep link: %q", out)
	}
}

// countSubstr counts non-overlapping occurrences of sub in s.
func countSubstr(s, sub string) int { return strings.Count(s, sub) }

func TestTelegram_RenderCard_SafeTruncation(t *testing.T) {
	alerts := make([]model.Alert, 1000)
	for i := range alerts {
		alerts[i] = model.Alert{
			Labels: map[string]string{
				"alertname": strings.Repeat("X", 50) + "&<>",
				"severity":  "critical",
			},
			Status: model.AlertStatusFiring,
		}
	}
	ag := &model.AlertGroup{ID: "ag-big", Title: "Big", Severity: "critical", Alerts: alerts}
	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "https://tokay.example", WithBaseURL("http://unused.invalid"))

	out := p.renderCard(ag, false)

	if len(out) > telegramMaxMessageLen {
		t.Fatalf("rendered length %d exceeds limit %d", len(out), telegramMaxMessageLen)
	}
	// Whole-line truncation must never sever HTML: anchor tags stay balanced and
	// the footer anchor is always present and complete.
	if open, close := countSubstr(out, "<a "), countSubstr(out, "</a>"); open != close {
		t.Errorf("unbalanced anchor tags: <a =%d </a>=%d", open, close)
	}
	if !strings.Contains(out, `href="https://tokay.example/#/ops/alert-groups/ag-big"`) {
		t.Errorf("footer link missing/truncated: %q", out[len(out)-120:])
	}
}

func TestTelegram_CardHasAckResolveKeyboard(t *testing.T) {
	var sentBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_ = json.NewDecoder(r.Body).Decode(&sentBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 5}})
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "https://tokay.example", WithBaseURL(server.URL))
	if _, err := p.Send(context.Background(), providers.NotificationRequest{
		Target: providers.NotificationTarget{Kind: "channel", ID: "@c"}, AlertGroup: testAlertGroup(), Editable: true,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sentBody["reply_markup"] == nil {
		t.Fatalf("card sendMessage has no reply_markup: %v", sentBody)
	}
	b, _ := json.Marshal(sentBody["reply_markup"])
	if !strings.Contains(string(b), "ack:ag-1") || !strings.Contains(string(b), "res:ag-1") {
		t.Errorf("keyboard missing ack/res callback_data: %s", b)
	}
}

// A card posted while interactivity is off never gets a keyboard in the first
// place.
func TestTelegram_InteractiveOff_SendHasEmptyKeyboard(t *testing.T) {
	var sentBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_ = json.NewDecoder(r.Body).Decode(&sentBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 8}})
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok", interactiveOff: true}, "https://tokay.example", WithBaseURL(server.URL))
	if _, err := p.Send(context.Background(), providers.NotificationRequest{
		Target: providers.NotificationTarget{Kind: "channel", ID: "@c"}, AlertGroup: testAlertGroup(), Editable: true,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	b, _ := json.Marshal(sentBody["reply_markup"])
	if string(b) != `{"inline_keyboard":[]}` {
		t.Errorf("reply_markup = %s, want an empty inline_keyboard", b)
	}
}

func TestTelegram_NoButtonsWithoutSelfURL(t *testing.T) {
	var sentBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_ = json.NewDecoder(r.Body).Decode(&sentBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"message_id": 9}})
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()

	// Empty selfURL ⇒ no public webhook possible ⇒ omit the (dead) keyboard.
	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "", WithBaseURL(server.URL))
	if _, err := p.Send(context.Background(), providers.NotificationRequest{
		Target: providers.NotificationTarget{Kind: "channel", ID: "@c"}, AlertGroup: testAlertGroup(), Editable: true,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, ok := sentBody["reply_markup"]; ok {
		t.Errorf("card must have no reply_markup when selfURL is empty, got %v", sentBody["reply_markup"])
	}
}

func TestTelegram_AnswerCallback(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			count++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": true})
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "", WithBaseURL(server.URL))
	if err := p.AnswerCallback(context.Background(), "cb1", "Acknowledged"); err != nil {
		t.Fatalf("AnswerCallback: %v", err)
	}
	if count != 1 {
		t.Errorf("answerCallbackQuery calls = %d, want 1", count)
	}
}

func TestTelegram_SetAndDeleteWebhook_ExplicitToken(t *testing.T) {
	var paths []string
	var setBody map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/setWebhook") {
			_ = json.NewDecoder(r.Body).Decode(&setBody)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": true})
	}))
	defer server.Close()

	// tokenSource is intentionally a different token — webhook mgmt must use the
	// EXPLICIT token argument, not the cached one.
	p := NewProvider(&mockTelegramTokenSource{token: "CACHED"}, "", WithBaseURL(server.URL))
	if err := p.SetWebhook(context.Background(), "BOT1", "https://x/telegram/webhook", "sek"); err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	if err := p.DeleteWebhook(context.Background(), "BOT1"); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "/botBOT1/setWebhook") || !strings.Contains(joined, "/botBOT1/deleteWebhook") {
		t.Errorf("explicit token not used in webhook paths: %v", paths)
	}
	if setBody["secret_token"] != "sek" {
		t.Errorf("setWebhook missing secret_token: %v", setBody)
	}
}

func TestTelegram_BotUsername_Caches(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			count++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "result": map[string]interface{}{"username": "tokay_bot"}})
			return
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, "", WithBaseURL(server.URL))
	for i := 0; i < 3; i++ {
		u, err := p.BotUsername(context.Background())
		if err != nil || u != "tokay_bot" {
			t.Fatalf("BotUsername = %q, %v", u, err)
		}
	}
	if count != 1 {
		t.Errorf("getMe should be cached, called %d times", count)
	}
}

// Telegram never posts a card for an unonboarded team today - that path is
// firehose, which is Slack-only - so these lock the shared helper down from the
// Telegram side and keep the two channels from drifting apart.
func TestTelegram_TeamGate(t *testing.T) {
	tests := []struct {
		name         string
		interactive  bool
		isResolved   bool
		lookup       *countingTeamLookup
		useNilHook   bool
		wantKeyboard string
		wantCalls    int
	}{
		{
			name:         "onboarded team keeps its buttons",
			interactive:  true,
			lookup:       &countingTeamLookup{onboarded: true},
			wantKeyboard: "buttons",
			wantCalls:    1,
		},
		{
			name:         "unknown team gets an empty keyboard, not nil",
			interactive:  true,
			lookup:       &countingTeamLookup{onboarded: false},
			wantKeyboard: "empty",
			wantCalls:    1,
		},
		{
			name:         "a failing lookup degrades to onboarded",
			interactive:  true,
			lookup:       &countingTeamLookup{onboarded: false, err: errors.New("db down")},
			wantKeyboard: "buttons",
			wantCalls:    1,
		},
		{
			name:         "no lookup wired up behaves as before",
			interactive:  true,
			useNilHook:   true,
			wantKeyboard: "buttons",
		},
		{
			name:         "interactivity off short-circuits before the lookup",
			interactive:  false,
			lookup:       &countingTeamLookup{onboarded: false},
			wantKeyboard: "empty",
			wantCalls:    0,
		},
		{
			name:         "resolved card short-circuits before the lookup",
			interactive:  true,
			isResolved:   true,
			lookup:       &countingTeamLookup{onboarded: false},
			wantKeyboard: "empty",
			wantCalls:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []Option{WithBaseURL("http://unused.invalid")}
			if !tt.useNilHook {
				opts = append(opts, WithTeamLookup(tt.lookup.fn))
			}
			p := NewProvider(
				&mockTelegramTokenSource{token: "tok", interactiveOff: !tt.interactive},
				"https://tokay.example",
				opts...,
			)

			ag := testAlertGroup()
			ag.TeamID = "payments"

			b, err := json.Marshal(p.keyboardFor(ag, tt.isResolved))
			if err != nil {
				t.Fatalf("marshal keyboard: %v", err)
			}
			switch tt.wantKeyboard {
			case "empty":
				if string(b) != `{"inline_keyboard":[]}` {
					t.Errorf("keyboard = %s, want an empty inline_keyboard", b)
				}
			case "buttons":
				if !strings.Contains(string(b), "ack:") || !strings.Contains(string(b), "res:") {
					t.Errorf("keyboard = %s, want ack/res buttons", b)
				}
			}

			if !tt.useNilHook && tt.lookup.calls != tt.wantCalls {
				t.Errorf("team lookup ran %d times, want %d", tt.lookup.calls, tt.wantCalls)
			}
		})
	}
}

// countingTeamLookup is a providers.TeamLookup that records how often it ran,
// so a test can assert the lookup was skipped, not just that its answer was
// ignored.
type countingTeamLookup struct {
	calls     int
	onboarded bool
	err       error
}

func (c *countingTeamLookup) fn(string) (bool, error) {
	c.calls++
	return c.onboarded, c.err
}
