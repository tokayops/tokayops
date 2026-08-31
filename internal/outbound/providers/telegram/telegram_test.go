package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
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
		Status:   model.AlertGroupStatusTriggered,
		Alerts: []model.Alert{{
			Fingerprint: "fp-1",
			StartsAt:    time.Unix(1700000000, 0).UTC(),
			Labels:      map[string]string{"alertname": "A1", "severity": "critical"},
			Status:      model.AlertStatusFiring,
		}},
	}
}

func TestTelegram_SendDM_FireAndForget(t *testing.T) {
	counts := map[string]int{}
	server := fakeBotAPI(t, 7, counts)
	defer server.Close()

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, WithBaseURL(server.URL))
	if err := p.SendDM(context.Background(), "123456", "handoff: you are on call"); err != nil {
		t.Fatalf("send a direct message: %v", err)
	}
	if counts["sendMessage"] != 1 {
		t.Errorf("sendMessage calls = %d, want 1", counts["sendMessage"])
	}
}

// TestTelegram_SendDM_RefusesAnEmptyMessage. There is nothing else to guard
// against here any more: this takes a chat and words, and there is no target
// kind to get wrong and no card to ask for by mistake.
func TestTelegram_SendDM_RefusesAnEmptyMessage(t *testing.T) {
	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, WithBaseURL("http://unused.invalid"))

	if err := p.SendDM(context.Background(), "12345", ""); err == nil {
		t.Error("a message with nothing in it was sent")
	}
}

func TestTelegram_MissingToken_Permanent(t *testing.T) {
	p := NewProvider(&mockTelegramTokenSource{token: ""}, WithBaseURL("http://unused.invalid"))
	ctx := context.Background()

	err := p.SendDM(ctx, "12345", "you are on call")
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("direct message: got %v, want ErrNoToken", err)
	}
	// Whether the caller treats that as permanent is the caller's rule
	// and is asserted there: a channel that classified its own errors for its
	// caller would be two answers to one question.
}

func TestTelegram_RenderCard_HTMLEscaping(t *testing.T) {
	ag := &model.AlertGroup{
		Status:   model.AlertGroupStatusTriggered,
		ID:       `ag&"1`,
		Title:    "T",
		Severity: "<sev>",
		Alerts: []model.Alert{
			{
				Fingerprint: "fp-escaping",
				StartsAt:    time.Unix(1700000000, 0).UTC(),
				Labels:      map[string]string{"alertname": `A & B <prod> "x"`, "severity": "critical"},
				Status:      model.AlertStatusFiring,
			},
		},
	}
	out := RenderCard(frozen(t, ag))

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
			Fingerprint: fmt.Sprintf("fp-big-%d", i),
			StartsAt:    time.Unix(1700000000, 0).UTC(),
			Labels: map[string]string{
				"alertname": strings.Repeat("X", 50) + "&<>",
				"severity":  "critical",
			},
			Status: model.AlertStatusFiring,
		}
	}
	ag := &model.AlertGroup{
		ID: "ag-big", Title: "Big", Severity: "critical",
		Status: model.AlertGroupStatusTriggered, Alerts: alerts,
	}

	out := RenderCard(frozen(t, ag))

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

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, WithBaseURL(server.URL))
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

	// tokenSource is intentionally a different token - webhook mgmt must use the
	// EXPLICIT token argument, not the cached one.
	p := NewProvider(&mockTelegramTokenSource{token: "CACHED"}, WithBaseURL(server.URL))
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

	p := NewProvider(&mockTelegramTokenSource{token: "tok"}, WithBaseURL(server.URL))
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

// TestTelegram_TeamGate: whether a card carries buttons.
//
// Both halves of it come from the state now. Whether the team is onboarded was
// decided when the state was frozen and travels in the snapshot; whether this
// installation offers interactivity at all is the administrator's setting, read
// per attempt. Nothing here asks a database mid-render any more - the lookup
// that used to happen here belonged to the freeze that drew from a live row.
func TestTelegram_TeamGate(t *testing.T) {
	tests := []struct {
		name         string
		interactive  bool
		isResolved   bool
		onboarded    bool
		wantKeyboard string
	}{
		{
			name:        "onboarded team keeps its buttons",
			interactive: true, onboarded: true, wantKeyboard: "buttons",
		},
		{
			name:        "a team TokayOps does not have gets an empty keyboard, not nil",
			interactive: true, onboarded: false, wantKeyboard: "empty",
		},
		{
			name:        "interactivity off leaves nothing to press",
			interactive: false, onboarded: true, wantKeyboard: "empty",
		},
		{
			name:        "a resolved card is not acted on",
			interactive: true, isResolved: true, onboarded: true, wantKeyboard: "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewProvider(
				&mockTelegramTokenSource{token: "tok", interactiveOff: !tt.interactive},
				WithBaseURL("http://unused.invalid"),
			)

			ag := testAlertGroup()
			ag.TeamID = "payments"
			state := frozenFor(t, ag, tt.isResolved, tt.onboarded)

			b, err := json.Marshal(KeyboardFor(state, p.interactive()))
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
		})
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
