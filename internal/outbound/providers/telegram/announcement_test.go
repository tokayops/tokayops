package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// A shift change as Telegram writes it. The same facts as the Slack
// announcement, in the syntax Telegram actually renders.

func announcementPayload(t *testing.T, kind keys.HandoffKind, team string) keys.HandoffPayloadV1 {
	t.Helper()
	return keys.HandoffPayloadV1{
		Kind: kind, TeamName: team, ScheduleID: "sched-1", Timezone: "Asia/Bangkok",
		GridSlotStart:   time.Date(2026, 5, 4, 4, 0, 0, 0, time.UTC), // 11:00 local
		AssignmentStart: time.Date(2026, 5, 4, 7, 0, 0, 0, time.UTC), // 14:00 local
		AssignmentEnd:   time.Date(2026, 5, 5, 4, 0, 0, 0, time.UTC), // 11:00 next day
		Target:          keys.Target{Kind: keys.TargetUser, Ref: "u-alice"},
	}
}

// announcementCall is the call the worker makes for a shift change: no frozen
// state at all, and everything the message says inside the payload.
func announcementCall(t *testing.T, payload keys.HandoffPayloadV1) outbound.Call {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the announcement: %v", err)
	}
	digest, err := keys.PayloadDigest(keys.KindHandoff, payload.SchemaVersion(), raw)
	if err != nil {
		t.Fatalf("digest the announcement: %v", err)
	}
	content, err := outbound.NewPayloadContent(digest)
	if err != nil {
		t.Fatalf("build the content: %v", err)
	}
	return outbound.Call{
		IntentID: "intent-1", AttemptID: "attempt-1", Provider: "telegram",
		AttemptKind: outbound.AttemptCreate, Operation: outbound.OperationSend,
		Endpoint: "12345", ProviderKey: "create-key",
		KeyKind: keys.KindHandoff, Family: "handoff",
		Content: content, Payload: raw,
		PayloadSchemaVersion: payload.SchemaVersion(),
	}
}

// TestAShiftChangeIsWrittenInTelegram.
//
// Every fact the payload carries reaches the message, and none of Slack's
// spelling comes with it: a reader here gets an emoji their client draws and
// bold their client renders, where before they got ":mega:" and "*Backend*" as
// literal characters.
func TestAShiftChangeIsWrittenInTelegram(t *testing.T) {
	for _, tc := range []struct {
		kind  keys.HandoffKind
		first string
	}{
		{keys.HandoffShiftChange, "📣 You are now on-call for team <b>Backend</b>."},
		{
			keys.HandoffAddedToActiveShift,
			"➕ You have been added to the on-call shift in progress for team <b>Backend</b>.",
		},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			api := newBotAPI(t)
			handler := handlerFor(api)

			result, err := handler.ExecuteAttempt(context.Background(),
				announcementCall(t, announcementPayload(t, tc.kind, "Backend")))
			if err != nil {
				t.Fatalf("send the announcement: %v", err)
			}
			if result.Evidence != outbound.ProviderResponse {
				t.Fatalf("the send is recorded as %q", result.Evidence)
			}
			if len(api.calls) != 1 {
				t.Fatalf("Telegram was called %d times, want once", len(api.calls))
			}

			sent := api.calls[0]
			text, _ := sent["text"].(string)
			if lines := strings.Split(text, "\n"); lines[0] != tc.first {
				t.Errorf("first line = %q, want %q", lines[0], tc.first)
			}
			if mode, _ := sent["parse_mode"].(string); mode != "HTML" {
				t.Errorf("parse_mode = %q; the emphasis would arrive as tags", mode)
			}
			for _, want := range []string{
				"Rotation shift started: Mon May 4, 11:00 (Asia/Bangkok)",
				"Your assignment effective from: Mon May 4, 14:00 (Asia/Bangkok)",
				"Assignment ends: Tue May 5, 11:00 (Asia/Bangkok)",
				"may change if the schedule is modified",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("the message is missing %q:\n%s", want, text)
				}
			}
			// Nothing from the other channel: an announcement written for
			// Slack arrives here as characters, which is the bug this split
			// exists to end.
			for _, foreign := range []string{":mega:", ":clock1:", ":heavy_plus_sign:", "*"} {
				if strings.Contains(text, foreign) {
					t.Errorf("the message carries Slack's %q:\n%s", foreign, text)
				}
			}
			// A shift change has nothing to press.
			if _, keyboard := sent["reply_markup"]; keyboard {
				t.Error("the announcement was sent with a keyboard")
			}
		})
	}
}

// TestATeamNameCannotBreakOutOfTheAnnouncement. The name is free text somebody
// typed into a form and it lands inside a tag, so it is escaped - an unclosed
// tag or a bare ampersand would make Telegram refuse the whole message, which
// turns a team's name into a delivery failure nobody can act on.
func TestATeamNameCannotBreakOutOfTheAnnouncement(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	const hostile = `Back<b>end</b> & "ops"`
	if _, err := handler.ExecuteAttempt(context.Background(),
		announcementCall(t, announcementPayload(t, keys.HandoffShiftChange, hostile))); err != nil {
		t.Fatalf("send the announcement: %v", err)
	}

	text, _ := api.calls[0]["text"].(string)
	first := strings.SplitN(text, "\n", 2)[0]
	if strings.Contains(first, "<b>end</b>") {
		t.Errorf("the name's tags were left live: %q", first)
	}
	if !strings.Contains(first, "&lt;b&gt;end&lt;/b&gt;") || !strings.Contains(first, "&amp;") {
		t.Errorf("the name was not escaped: %q", first)
	}
	// And the emphasis this package put there is still emphasis.
	if !strings.HasPrefix(first, "📣 You are now on-call for team <b>") ||
		!strings.HasSuffix(first, "</b>.") {
		t.Errorf("the announcement's own markup was disturbed: %q", first)
	}
}

// TestALongTeamNameStillSendsAndStillReads.
//
// The name is the only part of an announcement with no length of its own, and
// nothing between the form that accepts it and the Bot API bounds it. Whole, a
// long enough one puts the message over the API's hard limit; the API answers
// "message is too long", and an announcement that was perfectly valid ends as a
// permanent failure for every person on that team, every shift, until somebody
// renames it.
//
// The cut happens before the escaping, so what arrives is still valid HTML - a
// cut through "&amp;" would leave "&am", which Telegram refuses for a different
// reason and just as permanently.
func TestALongTeamNameStillSendsAndStillReads(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	// Every rune of it escapes to five characters, which is the worst case for
	// the length, and a cut landing inside one is the worst case for the shape.
	// Long enough to be cut, and shaped so the cut lands where the escaping
	// changed the length: eighty runes of the NAME end inside the run of
	// ampersands, so a cut applied afterwards would fall in the middle of an
	// entity instead.
	long := strings.Repeat("a", 79) + strings.Repeat("&", 400)
	if _, err := handler.ExecuteAttempt(context.Background(),
		announcementCall(t, announcementPayload(t, keys.HandoffShiftChange, long))); err != nil {
		t.Fatalf("send the announcement: %v", err)
	}

	text, _ := api.calls[0]["text"].(string)
	if bare := looseAmpersand(text); bare != "" {
		t.Errorf("the cut left %q, which is not an entity Telegram will accept:\n%s",
			bare, first(text))
	}
	if len(text) > telegramMaxMessageLen {
		t.Fatalf("the message is %d bytes, over the %d the Bot API takes",
			len(text), telegramMaxMessageLen)
	}
	// Cut, and said so.
	first := strings.SplitN(text, "\n", 2)[0]
	if !strings.Contains(first, "…") {
		t.Errorf("a name over the limit arrived whole: %q", first)
	}
	if !strings.HasSuffix(first, "</b>.") {
		t.Errorf("the announcement's own markup was disturbed: %q", first)
	}
	// And the message still says what it is for.
	if !strings.Contains(text, "Assignment ends: Tue May 5, 11:00 (Asia/Bangkok)") {
		t.Errorf("the shift's own facts were cut instead of the name:\n%s", text)
	}
}

// TestATeamNameIsCutOnACharacterBoundary: by runes, not by bytes. A cut through
// a multi-byte character leaves a byte sequence that is not text at all, and
// what Telegram does with it is not worth finding out.
func TestATeamNameIsCutOnACharacterBoundary(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	long := strings.Repeat("я", 200)
	if _, err := handler.ExecuteAttempt(context.Background(),
		announcementCall(t, announcementPayload(t, keys.HandoffShiftChange, long))); err != nil {
		t.Fatalf("send the announcement: %v", err)
	}

	text, _ := api.calls[0]["text"].(string)
	if !utf8.ValidString(text) {
		t.Fatal("the message is not valid UTF-8")
	}
	// Against the length every channel shares, not against one written here:
	// a channel that kept a length of its own has to fail this.
	if got := strings.Count(text, "я"); got != providers.MaxTeamNameLen {
		t.Errorf("the name was cut to %d runes, want the %d every channel cuts at",
			got, providers.MaxTeamNameLen)
	}
}

// TestAnAnnouncementNobodyCanReadStopsBeforeTheNetwork: the same rule the
// escalation payload has. A shift change nothing can read is a refusal with
// proof, not a call whose fate is unknown.
func TestAnAnnouncementNobodyCanReadStopsBeforeTheNetwork(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	call := announcementCall(t, announcementPayload(t, keys.HandoffShiftChange, "Backend"))
	call.Payload = []byte(`{"kind":"handoff","team_name":"Backend"}`)

	result, err := handler.ExecuteAttempt(context.Background(), call)
	if err == nil {
		t.Fatal("an announcement nobody can read was sent anyway")
	}
	if result.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("a call that never went out is recorded as %q", result.Evidence)
	}
	if len(api.calls) != 0 {
		t.Fatal("Telegram was called anyway")
	}
}

// announcementIntent is the commitment as Prepare sees it: a handover, aimed at
// a person, with the announcement in its payload.
func announcementIntent(t *testing.T, payload keys.HandoffPayloadV1, ref string) outbound.Intent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the announcement: %v", err)
	}
	return outbound.Intent{
		ID: "intent-1", Provider: "telegram", KeyKind: keys.KindHandoff,
		TargetKind:           keys.TargetUser,
		TargetRef:            ref,
		PayloadSchemaVersion: payload.SchemaVersion(),
		Payload:              raw,
	}
}

// TestAnAnnouncementIsPreparedAsAHandover.
//
// The payload is read in the shape the KIND says it is in. Read as an
// escalation - which is what every commitment used to be - a perfectly good
// announcement is refused as unreadable, and nobody is ever told they came on
// call.
func TestAnAnnouncementIsPreparedAsAHandover(t *testing.T) {
	linked := providers.IdentityLookup(func(context.Context, string, string) (string, error) {
		return "12345", nil
	})
	handler := NewHandler(&mockTelegramTokenSource{token: "tok"}, linked)

	payload := announcementPayload(t, keys.HandoffShiftChange, "Backend")
	prepared := handler.Prepare(context.Background(), announcementIntent(t, payload, "u-alice"))
	if prepared.Outcome() != outbound.PreparationReady {
		t.Fatalf("an announcement was refused: %+v", prepared)
	}
	if got := prepared.Request("intent-1", "token-1", "worker-1").BoundEndpoint; got != "12345" {
		t.Fatalf("the resolved address is %q", got)
	}
}

// TestAnAnnouncementWrittenForSomebodyElseIsRefused. The commitment names its
// recipient twice, and the rule that they must agree is not an escalation rule:
// an announcement addressed to one person and written for another tells the
// wrong person about a shift that is not theirs.
func TestAnAnnouncementWrittenForSomebodyElseIsRefused(t *testing.T) {
	linked := providers.IdentityLookup(func(context.Context, string, string) (string, error) {
		return "12345", nil
	})
	handler := NewHandler(&mockTelegramTokenSource{token: "tok"}, linked)

	payload := announcementPayload(t, keys.HandoffShiftChange, "Backend")
	prepared := handler.Prepare(context.Background(), announcementIntent(t, payload, "u-bob"))
	if prepared.Outcome() != outbound.PreparationPermanent {
		t.Fatalf("an announcement written for somebody else was %s", prepared.Outcome())
	}
}

// looseAmpersand answers with the first ampersand that does not begin one of
// the entities html.EscapeString produces.
//
// Telegram parses the whole message as HTML and refuses all of it over one of
// these, so a message carrying one is a message that never arrives - which is
// what a cut applied AFTER escaping leaves behind.
func looseAmpersand(text string) string {
	entities := []string{"&amp;", "&lt;", "&gt;", "&#34;", "&#39;"}
	for i := 0; i < len(text); i++ {
		if text[i] != '&' {
			continue
		}
		whole := false
		for _, entity := range entities {
			if strings.HasPrefix(text[i:], entity) {
				whole = true
				i += len(entity) - 1
				break
			}
		}
		if !whole {
			end := i + 6
			if end > len(text) {
				end = len(text)
			}
			return text[i:end]
		}
	}
	return ""
}

func first(text string) string { return strings.SplitN(text, "\n", 2)[0] }
