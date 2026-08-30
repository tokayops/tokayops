package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// A shift change as Slack writes it.

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
		IntentID: "intent-1", AttemptID: "attempt-1", Provider: "slack",
		AttemptKind: outbound.AttemptCreate, Operation: outbound.OperationSend,
		Endpoint: "U0001", ProviderKey: "create-key",
		KeyKind: keys.KindHandoff, Family: "handoff",
		Content: content, Payload: raw,
		PayloadSchemaVersion: payload.SchemaVersion(),
	}
}

// TestAShiftChangeIsWrittenInSlack. Every fact the payload carries reaches the
// message, in the schedule's own zone and under a first line that says which of
// the two events this is.
func TestAShiftChangeIsWrittenInSlack(t *testing.T) {
	for _, tc := range []struct {
		kind  keys.HandoffKind
		first string
	}{
		{keys.HandoffShiftChange, ":mega: You are now on-call for team `Backend`."},
		{
			keys.HandoffAddedToActiveShift,
			":heavy_plus_sign: You have been added to the on-call shift in progress for team `Backend`.",
		},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			api := newSlackAPI(t)
			handler := handlerFor(api)

			result, err := handler.ExecuteAttempt(context.Background(),
				announcementCall(t, announcementPayload(t, tc.kind, "Backend")))
			if err != nil {
				t.Fatalf("send the announcement: %v", err)
			}
			if result.Evidence != outbound.ProviderResponse {
				t.Fatalf("the send is recorded as %q", result.Evidence)
			}
			if len(api.posts) != 1 {
				t.Fatalf("Slack was called %d times, want once", len(api.posts))
			}

			text, _ := api.posts[0]["text"].(string)
			lines := strings.Split(text, "\n")
			if lines[0] != tc.first {
				t.Errorf("first line = %q, want %q", lines[0], tc.first)
			}
			for _, want := range []string{
				"Rotation shift started:         Mon May 4, 11:00 (Asia/Bangkok)",
				"Your assignment effective from: Mon May 4, 14:00 (Asia/Bangkok)",
				"Assignment ends:                Tue May 5, 11:00 (Asia/Bangkok)",
				"may change if the schedule is modified",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("the message is missing %q:\n%s", want, text)
				}
			}
			// A shift change is a message, not a card: no blocks, no
			// attachments, nothing to press.
			if _, blocks := api.posts[0]["blocks"]; blocks {
				t.Error("the announcement was posted as a card")
			}
		})
	}
}

// TestATeamNameCannotBreakOutOfTheAnnouncement.
//
// The name is free text somebody typed into a form, and it lands in mrkdwn.
// Every character below means something to Slack: the backtick would end the
// span the name sits in, the angle brackets open a link, the ampersand starts
// an entity, and a newline splits the line the labels are aligned in.
//
// The asterisk is why the name is in a span rather than in bold. mrkdwn has no
// escape for it, so a name containing one cannot be made safe inside emphasis -
// inside a span it is a character like any other.
func TestATeamNameCannotBreakOutOfTheAnnouncement(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	const hostile = "Back`end <&> *ops*\nsecond line"
	if _, err := handler.ExecuteAttempt(context.Background(),
		announcementCall(t, announcementPayload(t, keys.HandoffShiftChange, hostile))); err != nil {
		t.Fatalf("send the announcement: %v", err)
	}

	text, _ := api.posts[0]["text"].(string)
	first := strings.SplitN(text, "\n", 2)[0]
	for _, forbidden := range []string{"`end", "<", ">", "\r"} {
		if strings.Contains(first, forbidden) {
			t.Errorf("the first line still carries %q: %q", forbidden, first)
		}
	}
	if !strings.Contains(first, "&amp;") || !strings.Contains(first, "&lt;") {
		t.Errorf("the name was not escaped: %q", first)
	}
	// The name stays on the line it was put on, and the labels below still
	// line up.
	if !strings.HasSuffix(first, "`.") {
		t.Errorf("the name broke out of its span: %q", first)
	}
	if !strings.Contains(text, "Rotation shift started:         Mon May 4, 11:00 (Asia/Bangkok)") {
		t.Errorf("the rest of the announcement was disturbed:\n%s", text)
	}
}

// TestALongTeamNameIsCutBeforeItIsEscaped. The name is the only part of an
// announcement with no length of its own, and it is bounded by the same rule a
// card's team label is - by runes, before escaping, so a cut can sever neither
// a character nor an entity.
func TestALongTeamNameIsCutBeforeItIsEscaped(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	long := strings.Repeat("&", 400)
	if _, err := handler.ExecuteAttempt(context.Background(),
		announcementCall(t, announcementPayload(t, keys.HandoffShiftChange, long))); err != nil {
		t.Fatalf("send the announcement: %v", err)
	}

	text, _ := api.posts[0]["text"].(string)
	first := strings.SplitN(text, "\n", 2)[0]
	if !strings.Contains(first, "…") {
		t.Errorf("a name over the limit arrived whole: %q", first)
	}
	if strings.Contains(first, "&am`") || strings.HasSuffix(first, "&am") {
		t.Errorf("the cut went through an entity: %q", first)
	}
	if !strings.HasSuffix(first, "`.") {
		t.Errorf("the name broke out of its span: %q", first)
	}
}

// TestAnAnnouncementNobodyCanReadStopsBeforeTheNetwork: the same rule the
// escalation payload has. A shift change nothing can read is a refusal with
// proof, not a call whose fate is unknown.
func TestAnAnnouncementNobodyCanReadStopsBeforeTheNetwork(t *testing.T) {
	api := newSlackAPI(t)
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
	if len(api.posts) != 0 {
		t.Fatal("Slack was called anyway")
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
		ID: "intent-1", Provider: "slack", KeyKind: keys.KindHandoff,
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
		return "U0001", nil
	})
	handler := NewHandler(&mockTokenSource{token: "tok"}, linked)

	payload := announcementPayload(t, keys.HandoffShiftChange, "Backend")
	prepared := handler.Prepare(context.Background(), announcementIntent(t, payload, "u-alice"))
	if prepared.Outcome() != outbound.PreparationReady {
		t.Fatalf("an announcement was refused: %+v", prepared)
	}
	if got := prepared.Request("intent-1", "token-1", "worker-1").BoundEndpoint; got != "U0001" {
		t.Fatalf("the resolved address is %q", got)
	}
}

// TestAnAnnouncementWrittenForSomebodyElseIsRefused. The commitment names its
// recipient twice, and the rule that they must agree is not an escalation rule:
// an announcement addressed to one person and written for another tells the
// wrong person about a shift that is not theirs.
func TestAnAnnouncementWrittenForSomebodyElseIsRefused(t *testing.T) {
	linked := providers.IdentityLookup(func(context.Context, string, string) (string, error) {
		return "U0001", nil
	})
	handler := NewHandler(&mockTokenSource{token: "tok"}, linked)

	payload := announcementPayload(t, keys.HandoffShiftChange, "Backend")
	prepared := handler.Prepare(context.Background(), announcementIntent(t, payload, "u-bob"))
	if prepared.Outcome() != outbound.PreparationPermanent {
		t.Fatalf("an announcement written for somebody else was %s", prepared.Outcome())
	}
}
