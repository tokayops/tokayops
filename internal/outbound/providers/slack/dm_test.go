package slack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// TestTheDirectMessageLinksToTheAlertAndToItsCard. The words are the policy's
// when it has any and the snapshot's otherwise; the link to the alert is
// always there; the link to the card is there exactly when the generation
// bound a card AND the workspace it is in. The permalink is built from the two
// without asking Slack, in the shape Slack documents, with or without the
// slash the workspace's address ends in.
func TestTheDirectMessageLinksToTheAlertAndToItsCard(t *testing.T) {
	state := handlerState(t).Content()
	title := mrkdwn(providers.ResolveStatus(state).Title)
	words := keys.EscalationPayloadV1{
		Slot: keys.Slot{Kind: keys.SlotPolicy, Index: 1}, Target: keys.Target{Kind: keys.TargetUser, Ref: "u-1"},
	}
	override := "disk on db-1 is full, please look"
	own := words
	own.MessageOverride = &override
	card := outbound.BoundContext{CardReceiptRef: "C0001/1700000000.000100", TeamURL: "https://acme.slack.com/"}

	const alert = "<https://tokay.example/#/ops/alert-groups/ag-1|Open in TokayOps>"
	const primary = "Primary message: <https://acme.slack.com/archives/C0001/p1700000000000100|Open in Slack>"
	for _, tc := range []struct {
		name    string
		payload keys.EscalationPayloadV1
		context outbound.BoundContext
		want    string
	}{
		{name: "the snapshot's words, no card", payload: words,
			want: title + "\nSeverity: critical\n" + alert},
		{name: "the snapshot's words, a card", payload: words, context: card,
			want: title + "\nSeverity: critical\n" + alert + "\n" + primary},
		{name: "the policy's words, no card", payload: own,
			want: override + "\n" + alert},
		{name: "the policy's words, a card", payload: own, context: card,
			want: override + "\n" + alert + "\n" + primary},
		{name: "a card in no known workspace", payload: words,
			context: outbound.BoundContext{CardReceiptRef: "C0001/1700000000.000100"},
			want:    title + "\nSeverity: critical\n" + alert},
		{name: "a workspace with no card", payload: words,
			context: outbound.BoundContext{TeamURL: "https://acme.slack.com/"},
			want:    title + "\nSeverity: critical\n" + alert},
		{name: "a workspace without the slash", payload: words,
			context: outbound.BoundContext{CardReceiptRef: "C0001/1700000000.000100", TeamURL: "https://acme.slack.com"},
			want:    title + "\nSeverity: critical\n" + alert + "\n" + primary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := directMessage(state, tc.payload, tc.context); got != tc.want {
				t.Fatalf("the message reads:\n%s\n\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestTheCardComesFromTheGenerationNotFromTheAttempt. What the call carries as
// its bound context is what the message links to - nothing else is read - and
// a context this build cannot read stops before the network rather than
// going out without the link.
func TestTheCardComesFromTheGenerationNotFromTheAttempt(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	call := handlerCall(t, keys.Target{Kind: keys.TargetUser, Ref: "u-1"}, false)
	call.Endpoint = "D0001"
	call.BoundContext = json.RawMessage(`{"card_receipt_ref":"C0001/1700000000.000100","team_url":"https://acme.slack.com/"}`)
	if _, err := handler.ExecuteAttempt(context.Background(), call); err != nil {
		t.Fatalf("send: %v", err)
	}
	text, _ := api.posts[0]["text"].(string)
	if !strings.Contains(text, "Primary message: <https://acme.slack.com/archives/C0001/p1700000000000100|Open in Slack>") {
		t.Fatalf("the message does not link to the card it was bound to:\n%s", text)
	}

	call.BoundContext = json.RawMessage(`{"card_receipt_ref":"C0001/1700000000.000100","louder":true}`)
	refusal, err := handler.ExecuteAttempt(context.Background(), call)
	if err == nil || refusal.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("a context this build cannot read was sent: %v, %v", refusal, err)
	}
	if len(api.posts) != 1 {
		t.Fatal("a context this build cannot read reached the network")
	}
}
