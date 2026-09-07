package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

func ptr(v string) *string { return &v }

// threadState is a group with more alerts than the thread shows and a history
// with lines before the window, printed in a zone that is not UTC.
func threadState() keys.SnapshotInput {
	var alerts []keys.AlertSnapshot
	for i := 1; i <= 12; i++ {
		status := keys.AlertFiring
		if i == 2 {
			status = keys.AlertResolved
		}
		alerts = append(alerts, keys.AlertSnapshot{
			Fingerprint: fmt.Sprintf("fp-%d", i), Status: status,
			StartsAt:  time.Unix(1700000000, 0).UTC(),
			AlertName: fmt.Sprintf("DiskWillFill%d", i), Severity: "critical",
			Description: ptr(fmt.Sprintf("disk %d nearly full", i)),
		})
	}
	return keys.SnapshotInput{
		AlertGroupID: "ag-1", Revision: 3, Status: keys.GroupAcknowledged,
		Title: "Disk filling up", Severity: "critical", DisplayTimezone: "Europe/Berlin",
		AcknowledgedBy: ptr("nina"), Alerts: alerts,
		GroupURL: ptr("https://tokay.example/#/ops/alert-groups/ag-1"),
		Timeline: []keys.TimelineEventSnapshot{
			{ID: "e3", Type: keys.EventCreated, Message: "Alert group created",
				CreatedAt: time.Unix(1700000000, 0).UTC()},
			{ID: "e4", Type: keys.EventAcknowledged, Message: "Acknowledged by nina", Actor: ptr("nina"),
				CreatedAt: time.Unix(1700000120, 0).UTC()},
			{ID: "e5", Type: keys.EventNote, Message: "looking at it", Actor: ptr("nina"),
				CreatedAt: time.Unix(1700000600, 0).UTC()},
		},
		TimelineOmitted: 2,
	}
}

// TestTheThreadIsTheseWords is the golden text: ten alerts and a count of the
// rest, the history oldest first in the snapshot's zone with a count of what
// the window left out, and the icons per type.
func TestTheThreadIsTheseWords(t *testing.T) {
	lines := []string{"📋 *Alert Details*"}
	for i := 1; i <= 10; i++ {
		icon := "🔴"
		if i == 2 {
			icon = "🟢"
		}
		lines = append(lines, fmt.Sprintf("%s *DiskWillFill%d*: disk %d nearly full", icon, i, i))
	}
	lines = append(lines,
		"_... and 2 more alert details_",
		"",
		"📋 *Timeline*",
		"```",
		"... and 2 earlier events",
		"[23:13:20 CET] [NEW] Alert group created",
		"[23:15:20 CET] [ACK] Acknowledged by nina (by nina)",
		"[23:23:20 CET] [NOTE] looking at it (by nina)",
		"```")
	want := strings.Join(lines, "\n")

	if got := RenderThread(threadState()); got != want {
		t.Fatalf("the thread reads:\n%s\n\nand the protocol says:\n%s", got, want)
	}
}

// TestTheReplyNamesThePersonWhoResolved. A resolution by a person carries the
// name; one by the system - every alert cleared - is announced without one,
// and so is a snapshot that names nobody.
func TestTheReplyNamesThePersonWhoResolved(t *testing.T) {
	state := threadState()
	for name, want := range map[string]string{
		"":               "✅ Alert Group Resolved",
		keys.SystemActor: "✅ Alert Group Resolved",
		"nina":           "✅ Resolved by nina",
	} {
		state.ResolvedBy = ptr(name)
		if got := RenderReply(state); got != want {
			t.Errorf("resolved by %q reads %q, want %q", name, got, want)
		}
	}
	state.ResolvedBy = nil
	if got := RenderReply(state); got != "✅ Alert Group Resolved" {
		t.Errorf("resolved by nobody reads %q", got)
	}
}

// TestWhatArrivesFromOutsideCannotSpeakMrkdwn. Every field of text in these
// messages came from outside - a label, a display name, the words of a note -
// and Slack reads mrkdwn in all of them. A label reading <!channel> would
// page the channel; three backticks in a note would close the block the
// history sits in. Each is escaped once, where it is written: in the thread,
// in the reply, on the card and in a direct message.
func TestWhatArrivesFromOutsideCannotSpeakMrkdwn(t *testing.T) {
	state := threadState()
	state.Title = "<!here> Disk filling up"
	state.Severity = "<critical>"
	state.Alerts = state.Alerts[:1]
	state.Alerts[0].AlertName = "<!channel> DiskWillFill"
	state.Alerts[0].Description = ptr("a & b <@U0001>")
	state.Timeline[2].Message = "```\n<!here> look"
	state.Timeline[2].Actor = ptr("<!channel>")
	state.AcknowledgedBy = ptr("<!subteam^S1>")
	state.ResolvedBy = ptr("<!everyone>")
	// The addresses come from outside too - annotations and Alertmanager's
	// own URL - and a > inside one ends the link it is put in.
	state.ExternalURL = ptr("https://a|x> <!channel> <@U0001> <https://b")
	state.Alerts[0].DashboardURL = ptr("javascript:alert(1)")
	state.Alerts[0].RunbookURL = ptr("https://r.example/runbook|[runbook]><!here>")
	raw := []string{"<!channel>", "<@U0001>", "<!here>", "<!subteam^S1>", "<!everyone>", "<critical>", "javascript:"}

	thread := RenderThread(state)
	for _, r := range raw {
		if strings.Contains(thread, r) {
			t.Errorf("the thread carries %q as Slack would read it:\n%s", r, thread)
		}
	}
	if strings.Count(thread, "```") != 2 {
		t.Errorf("a note closed the history's block:\n%s", thread)
	}
	if !strings.Contains(thread, "&lt;!channel&gt; DiskWillFill") {
		t.Errorf("the alert's name did not survive as text:\n%s", thread)
	}

	if reply := RenderReply(state); reply != "✅ Resolved by &lt;!everyone&gt;" {
		t.Errorf("the reply reads %q", reply)
	}

	card := mrkdwnOf(t, Render(state, true))
	for _, r := range raw {
		if strings.Contains(card, r) {
			t.Errorf("the card carries %q as Slack would read it:\n%s", r, card)
		}
	}

	dm := directMessage(state, keys.EscalationPayloadV2{Target: keys.Target{Kind: keys.TargetUser, Ref: "u-1"}}, outbound.BoundContext{})
	for _, r := range raw {
		if strings.Contains(dm, r) {
			t.Errorf("the direct message carries %q as Slack would read it:\n%s", r, dm)
		}
	}
	if !strings.Contains(dm, "<https://tokay.example/#/ops/alert-groups/ag-1|Open in TokayOps>") {
		t.Errorf("the link this build writes itself was escaped away:\n%s", dm)
	}
}

// mrkdwnOf is the card as Slack reads it: JSON spells < > and & as escapes,
// and a check against the encoded form would find nothing and prove nothing.
func mrkdwnOf(t *testing.T, card any) string {
	t.Helper()
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("encode the card: %v", err)
	}
	return strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(encoded))
}

// TestAnAddressFromOutsideIsLinkedOnlyWhenItIsOne. A dashboard, a runbook and
// Alertmanager's own URL are linked when they are http or https addresses
// with nothing mrkdwn could read; anything else is left out of the card
// rather than escaped into a link that goes nowhere.
func TestAnAddressFromOutsideIsLinkedOnlyWhenItIsOne(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://alertmanager.example/#/alerts?receiver=ops":  true,
		"http://grafana.example/d/abc?var-host=db-1&from=now": true,
		"https://a|x> <!channel>":                             false,
		"https://a.example/x|[dash]>":                         false,
		"javascript:alert(1)":                                 false,
		"ftp://files.example/runbook":                         false,
		"https://":                                            false,
		"https://a b.example":                                 false,
		"grafana.example/d/abc":                               false,
		"":                                                    false,
	} {
		state := handlerState(t).Content()
		state.ExternalURL = ptr(raw)
		state.Alerts[0].DashboardURL = ptr(raw)
		state.Alerts[0].RunbookURL = ptr(raw)
		card := mrkdwnOf(t, Render(state, false))
		linked := strings.Contains(card, "<"+raw+"|")
		if linked != want {
			t.Errorf("%q linked: %v, want %v\n%s", raw, linked, want, card)
		}
		// A refused address is left out altogether, not printed as text. The
		// two that are prefixes of the card's own links are not checked.
		if !want && raw != "" && raw != "https://" && strings.Contains(card, raw) {
			t.Errorf("%q was put into the card without being linked:\n%s", raw, card)
		}
	}
}

// TestASatelliteIsPostedUnderItsCard. A thread and a reply are one post each,
// in the card's channel under the card's timestamp, which the endpoint names.
// A change to the thread goes by the thread's own coordinates like any change
// and carries no thread_ts. An endpoint that names no card stops before the
// network.
func TestASatelliteIsPostedUnderItsCard(t *testing.T) {
	api := newSlackAPI(t)
	api.answer = func(path string, form map[string]any) map[string]any {
		if strings.HasSuffix(path, "chat.postMessage") {
			return map[string]any{"ok": true, "channel": "C0001", "ts": "1700000000.000200"}
		}
		return nil
	}
	handler := handlerFor(api)

	thread := handlerCall(t, keys.Target{Kind: keys.TargetThread, Ref: "C0001"}, false)
	thread.Endpoint = "C0001/1700000000.000100"
	result, err := handler.ExecuteAttempt(context.Background(), thread)
	if err != nil {
		t.Fatalf("post the thread: %v", err)
	}
	if len(api.posts) != 1 {
		t.Fatalf("the thread was %d post(s)", len(api.posts))
	}
	post := api.posts[0]
	if post["channel"] != "C0001" || post["thread_ts"] != "1700000000.000100" {
		t.Fatalf("the thread went to channel %v under %v", post["channel"], post["thread_ts"])
	}
	if text, _ := post["text"].(string); !strings.HasPrefix(text, "📋 *Alert Details*") {
		t.Fatalf("the thread reads %q", text)
	}
	if _, ok := post["blocks"]; ok {
		t.Fatal("the thread carries blocks, and it is text")
	}
	if ref := result.Receipt.Ref(); ref != "C0001/1700000000.000200" {
		t.Fatalf("the thread's receipt names %q, not its own message", ref)
	}

	resolved := handlerState(t).Content()
	resolved.ResolvedBy = ptr("nina")
	ended, err := keys.NewRenderSnapshot(resolved)
	if err != nil {
		t.Fatal(err)
	}
	reply := handlerCall(t, keys.Target{Kind: keys.TargetThreadReply, Ref: "C0001"}, false)
	reply.Endpoint = "C0001/1700000000.000100"
	reply.Content = snapshotContent(t, ended)
	if _, err := handler.ExecuteAttempt(context.Background(), reply); err != nil {
		t.Fatalf("post the reply: %v", err)
	}
	if post := api.posts[1]; post["text"] != "✅ Resolved by nina" || post["thread_ts"] != "1700000000.000100" {
		t.Fatalf("the reply reads %v under %v", post["text"], post["thread_ts"])
	}

	var update map[string]any
	api.answer = func(path string, form map[string]any) map[string]any {
		if strings.HasSuffix(path, "chat.update") {
			update = form
			return map[string]any{"ok": true, "channel": "C0001", "ts": "1700000000.000200"}
		}
		return nil
	}
	change := thread
	change.AttemptKind = outbound.AttemptMutation
	change.Receipt = result.Receipt.Raw()
	change.ReceiptRef = result.Receipt.Ref()
	if _, err := handler.ExecuteAttempt(context.Background(), change); err != nil {
		t.Fatalf("change the thread: %v", err)
	}
	if update == nil || update["ts"] != "1700000000.000200" || update["channel"] != "C0001" {
		t.Fatalf("the change went to %v", update)
	}
	if _, ok := update["thread_ts"]; ok {
		t.Fatal("a change to the thread carried thread_ts, which chat.update does not take")
	}
	if len(api.posts) != 2 {
		t.Fatalf("the change was posted as a new message")
	}

	nowhere := thread
	nowhere.Endpoint = "garbage"
	refusal, err := handler.ExecuteAttempt(context.Background(), nowhere)
	if !errors.Is(err, ErrNoContent) || refusal.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("an endpoint naming no card was answered %v, %v", refusal, err)
	}
	if len(api.posts) != 2 {
		t.Fatal("an endpoint naming no card reached the network")
	}
}

// TestASatelliteIsPreparedFromItsCard. The endpoint of a satellite is the
// card's coordinates, read from the card the store hands over. A card without
// a message, or with coordinates nobody can read, is a refusal before the
// network - though the first is the domain's to state before the channel is
// asked, and reaches here only if that gate was skipped.
func TestASatelliteIsPreparedFromItsCard(t *testing.T) {
	handler := handlerFor(newSlackAPI(t))
	for _, kind := range []keys.TargetKind{keys.TargetThread, keys.TargetThreadReply} {
		intent := intentFor(t, keys.Target{Kind: kind, Ref: "C0001"})

		prepared := handler.Prepare(context.Background(), intent)
		if got := prepared.Request("i", "t", "w").ErrorClass; prepared.Outcome() != outbound.PreparationPermanent ||
			got != "parent_without_message" {
			t.Fatalf("a %s with no card was prepared as %s %q", kind, prepared.Outcome(), got)
		}

		intent.Parent = &outbound.ParentState{ID: "card", Status: outbound.StatusIdle,
			ReceiptRecorded: true, ReceiptRef: "C0001/1700000000.000100"}
		prepared = handler.Prepare(context.Background(), intent)
		if got := prepared.Request("i", "t", "w").BoundEndpoint; prepared.Outcome() != outbound.PreparationReady ||
			got != "C0001/1700000000.000100" {
			t.Fatalf("a %s under a card was prepared as %s at %q", kind, prepared.Outcome(), got)
		}

		intent.Parent.ReceiptRef = "garbage"
		prepared = handler.Prepare(context.Background(), intent)
		if got := prepared.Request("i", "t", "w").ErrorClass; prepared.Outcome() != outbound.PreparationPermanent ||
			got != "parent_receipt_unreadable" {
			t.Fatalf("a %s under unreadable coordinates was prepared as %s %q", kind, prepared.Outcome(), got)
		}
	}
}
