package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// Telegram as the outbound worker uses it. The same shape as the Slack handler,
// and for the same reasons: one call per attempt, an address it does not
// choose, and an answer it translates rather than interprets.

// Handler makes escalation sends for Telegram.
type Handler struct {
	tokens   TokenSource
	identity providers.IdentityLookup
	baseURL  string
	client   *http.Client
}

// NewHandler builds the channel. The token is read per attempt so a rotated one
// applies to work that has not gone out.
func NewHandler(tokens TokenSource, identity providers.IdentityLookup, opts ...HandlerOption) *Handler {
	h := &Handler{
		tokens:   tokens,
		identity: identity,
		baseURL:  telegramDefaultBaseURL,
		client:   effectClient(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// HandlerOption exists for one reason: pointing a test at a Bot API of its own.
type HandlerOption func(*Handler)

// WithHandlerBaseURL overrides the Bot API address.
func WithHandlerBaseURL(u string) HandlerOption {
	return func(h *Handler) { h.baseURL = strings.TrimRight(u, "/") }
}

// Prepare resolves where this commitment goes.
//
// A Telegram chat id and a person's chat are the same kind of address, so the
// only difference between the two targets is where the id comes from: the
// commitment itself, or the identity somebody linked.
func (h *Handler) Prepare(ctx context.Context, intent outbound.Intent) outbound.Preparation {
	// The payload is read HERE, before anything opens an attempt.
	//
	// Read inside the call instead, an unreadable payload becomes a network
	// attempt that never touched the network: recorded as a call whose fate is
	// unknown, retried on the family's backoff, and repeated for as long as the
	// commitment lives. Refused here it is what it actually is - a
	// deterministic refusal, in front of somebody who can act.
	//
	// What that refusal MEANS is not this channel's to say. A decoder that will
	// not read something cannot tell a damaged payload from one written in a
	// shape it has never been taught, and the two want opposite answers: the
	// first ends the commitment, the second belongs to whichever build knows
	// the shape. So the domain asks that question itself before it records
	// anything, and this refusal reaches the journal only when the shape is one
	// this build does know.
	payload, err := keys.DecodeEscalationPayloadV1(intent.PayloadSchemaVersion, intent.Payload)
	if err != nil {
		return outbound.Impossible("payload_unreadable", err.Error())
	}

	// The commitment names a recipient twice: in its own columns, which decide
	// WHERE this goes, and in the payload, which decides WHAT is written. A row
	// where those two disagree would send a person's private message to the
	// channel named in the columns - a leak, not a mangled journal entry - so
	// the two are compared before anything is resolved.
	if payload.Target.Kind != intent.TargetKind || payload.Target.Ref != intent.TargetRef {
		return outbound.Impossible("target_mismatch", fmt.Sprintf(
			"the commitment is addressed to %s %q and its message is written for %s %q",
			intent.TargetKind, intent.TargetRef, payload.Target.Kind, payload.Target.Ref))
	}
	if h.tokens == nil || h.tokens.GetTelegramToken() == "" {
		return outbound.Impossible("integration_missing",
			"Telegram is not configured on this installation")
	}

	// The same rule as the payload above, for the same reason: a change aimed
	// at coordinates nobody can read is a refusal, not a call to retry. A card
	// has no deadline, so a retry loop over a broken row never ends.
	if intent.HasReceipt {
		data, ok := messageAt(intent.Receipt)
		if !ok {
			return outbound.Impossible("receipt_unreadable",
				"the coordinates of the message to change cannot be read")
		}
		// The same rule as Slack's, for the same reason: coordinates and name
		// that disagree send the change to one message and record it against
		// another.
		if named := fmt.Sprintf("%s/%d", data.ChatID, data.MessageID); named != intent.ReceiptRef {
			return outbound.Impossible("receipt_mismatch", fmt.Sprintf(
				"the commitment holds the coordinates of %s and calls it %s",
				named, intent.ReceiptRef))
		}
	}

	switch intent.TargetKind {
	case keys.TargetChannel:
		return outbound.Ready(intent.TargetRef)

	case keys.TargetUser:
		if h.identity == nil {
			return outbound.Impossible("identity_lookup_missing",
				"nothing here can turn a user into a Telegram chat")
		}
		address, err := h.identity(ctx, intent.TargetRef, "telegram")
		switch {
		case errors.Is(err, providers.ErrNotLinked):
			return outbound.Impossible("identity_not_linked",
				fmt.Sprintf("%s has not linked a Telegram account", intent.TargetRef))
		case err != nil:
			return outbound.NotNow("identity_lookup_failed", err.Error())
		}
		return outbound.Ready(address)

	default:
		return outbound.Impossible("unsupported_target",
			fmt.Sprintf("Telegram has no %q to send to", intent.TargetKind))
	}
}

// ExecuteAttempt makes one call to the bound chat and nothing else: sendMessage
// when there is nothing out there yet, editMessageText when the attempt is a
// change to a message that exists.
//
// Telegram has no thread and no permalink, so there is no enrichment either -
// what comes back is already the whole receipt.
func (h *Handler) ExecuteAttempt(ctx context.Context, call outbound.Call) (outbound.Result, error) {
	payload, err := keys.DecodeEscalationPayloadV1(call.PayloadSchemaVersion, call.Payload)
	if err != nil {
		return outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary:  "the commitment's payload cannot be read: " + err.Error(),
		}, err
	}

	token := ""
	if h.tokens != nil {
		token = h.tokens.GetTelegramToken()
	}
	if token == "" {
		return outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary:  "Telegram is not configured on this installation",
		}, ErrNoToken
	}

	snapshot, drawn := call.Content.Snapshot()
	if !drawn {
		// This channel draws an alert card from a frozen state, and this
		// commitment has none. Not an empty card: a message about nothing is
		// worse than a message that did not go.
		return outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary: fmt.Sprintf(
				"a %s commitment renders from a state, and this one carries none",
				call.KeyKind),
		}, ErrNoContent
	}
	state := snapshot.Content()
	body := map[string]interface{}{
		"chat_id":                  call.Endpoint,
		"disable_web_page_preview": true,
	}

	// A card is HTML this package built and escaped; a direct message is
	// somebody's own words, sent as they are. Marking free text as HTML makes a
	// perfectly ordinary "a < b" or an unclosed tag unsendable - the message
	// fails to go out for the shape of its text, which is not a delivery
	// failure anybody can act on.
	//
	// A direct message carries no keyboard either: buttons belong on the card
	// in a channel, which is where an acknowledgement is visible to everybody
	// the alert was raised for.
	if payload.Target.Kind == keys.TargetUser {
		body["text"] = directMessage(state, payload)
	} else {
		body["text"] = RenderCard(state)
		body["parse_mode"] = "HTML"
		if keyboard := KeyboardFor(state, payload.Interactive); keyboard != nil {
			body["reply_markup"] = keyboard
		}
	}

	method := "sendMessage"
	if call.AttemptKind == outbound.AttemptMutation {
		// A change goes to the message this commitment already made, named by
		// the coordinates it holds. The chat is part of that name rather than a
		// place to send to: editing never moves a message.
		coordinates, ok := messageAt(call.Receipt)
		if !ok {
			return outbound.Result{
				Evidence: outbound.DefinitelyNotSent,
				Summary:  "the coordinates of the message to change cannot be read",
			}, ErrNoReceipt
		}
		method = "editMessageText"
		body["chat_id"] = coordinates.ChatID
		body["message_id"] = coordinates.MessageID
	}

	answer, err := callBotAPI(ctx, h.client, h.baseURL, token, method, body)
	if err != nil {
		// No envelope came back at all: whether the request was written is what
		// decides, and only the transport can say.
		return outbound.Result{
			Evidence: providers.EvidenceOf(err), Summary: err.Error(),
		}, err
	}
	if !answer.OK {
		return outbound.Result{
				Evidence: outbound.ProviderResponse,
				Status:   statusOf(answer),
				Summary:  answer.Description,
			}, fmt.Errorf("telegram %s failed (code %d): %s",
				method, answer.ErrorCode, answer.Description)
	}

	var sent struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID json.Number `json:"id"`
		} `json:"chat"`
	}
	result := outbound.Result{Evidence: outbound.ProviderResponse, Status: "ok"}
	if err := json.Unmarshal(answer.Result, &sent); err != nil {
		result.Summary = "accepted with a result that cannot be read: " + err.Error()
		return result, nil
	}
	if sent.MessageID == 0 || sent.Chat.ID.String() == "" {
		// Telegram said yes and would not say what it made; Conclude turns
		// that into doubt.
		result.Summary = fmt.Sprintf("accepted with chat=%q message_id=%d",
			sent.Chat.ID.String(), sent.MessageID)
		return result, nil
	}

	raw, err := json.Marshal(Data{ChatID: sent.Chat.ID.String(), MessageID: sent.MessageID})
	if err != nil {
		result.Summary = "the coordinates could not be recorded: " + err.Error()
		return result, nil
	}
	receipt, err := outbound.NewReceipt(
		fmt.Sprintf("%s/%d", sent.Chat.ID.String(), sent.MessageID), raw)
	if err != nil {
		result.Summary = "the coordinates could not be recorded: " + err.Error()
		return result, nil
	}
	result.Receipt = receipt
	return result, nil
}

// ClassifyResponse says what Telegram's own answer means.
//
// Telegram answers with an HTTP-ish code and a sentence, and only some of those
// sentences prove the message was not created. Those are named here; everything
// else is doubt, decided by the domain.
func (h *Handler) ClassifyResponse(res outbound.Result) (outbound.Classification, bool) {
	if res.Status == "ok" {
		return outbound.Classification{Outcome: outbound.OutcomeAccepted}, true
	}

	code, description, ok := strings.Cut(res.Status, ":")
	if !ok {
		return outbound.Classification{}, false
	}
	lower := strings.ToLower(description)

	switch code {
	case "429":
		// Telegram says outright that it did not process it, and for how long.
		return outbound.Classification{
			Outcome: outbound.OutcomeRetryableRejection, Class: "rate_limited",
		}, true

	case "403":
		// Blocked, kicked, or never allowed: the message was not delivered and
		// nothing about retrying changes that.
		return outbound.Classification{
			Outcome: outbound.OutcomePermanentRejection, Class: "forbidden",
		}, true

	case "400":
		// The one 400 that is not a refusal at all. Telegram answers a change
		// that would leave the message exactly as it is with an error, and what
		// it is saying is that the outside ALREADY shows what we are asking
		// for - which is the revision applied, not a failure to apply it.
		if strings.Contains(lower, "message is not modified") {
			return outbound.Classification{Outcome: outbound.OutcomeAccepted}, true
		}
		if strings.Contains(lower, "message to edit not found") {
			// The message is gone. The one fact an ordinary answer proves about
			// the object, and the only ground for making a second one.
			absent := keys.DetailDefinitelyAbsent
			return outbound.Classification{
				Outcome: outbound.OutcomePermanentRejection, Class: "message_gone",
				Detail: &absent,
			}, true
		}
		if strings.Contains(lower, "message can't be edited") {
			// It is there and will not change: the opposite fact, and stated as
			// no fact at all, because nobody may create a second message
			// without proof that the first is gone.
			return outbound.Classification{
				Outcome: outbound.OutcomePermanentRejection, Class: "not_editable",
			}, true
		}
		for _, refusal := range []string{
			"chat not found", "bot was blocked", "user is deactivated",
			"peer_id_invalid", "message is too long",
		} {
			if strings.Contains(lower, refusal) {
				return outbound.Classification{
					Outcome: outbound.OutcomePermanentRejection, Class: "rejected",
				}, true
			}
		}
		// Some other 400. Telegram validates before it acts, but this build
		// does not know that this one did.
		return outbound.Classification{}, false

	case "500", "502", "503", "504":
		return outbound.Classification{
			Outcome: outbound.OutcomeAmbiguous, Class: "provider_unavailable",
		}, true
	}
	return outbound.Classification{}, false
}

// messageAt reads back the coordinates a commitment holds. They are the name of
// the message, not a place to send to.
func messageAt(raw json.RawMessage) (Data, bool) {
	if len(raw) == 0 {
		return Data{}, false
	}
	var data Data
	if err := json.Unmarshal(raw, &data); err != nil {
		return Data{}, false
	}
	if data.ChatID == "" || data.MessageID == 0 {
		return Data{}, false
	}
	return data, true
}

// directMessage is what a person is told, in plain text.
//
// Built from this commitment alone: the escalation's own words if it has any,
// and otherwise what the snapshot says, with the link to the alert in TokayOps.
// Nothing here reads a neighbouring delivery - a permalink that exists on the
// retry and not on the first attempt is two different messages under one key.
func directMessage(state keys.SnapshotInput, payload keys.EscalationPayloadV1) string {
	if payload.MessageOverride != nil && *payload.MessageOverride != "" {
		return *payload.MessageOverride
	}

	status := providers.ResolveStatus(state)
	lines := []string{status.Title}
	if state.Severity != "" {
		lines = append(lines, "Severity: "+state.Severity)
	}
	if state.GroupURL != nil && *state.GroupURL != "" {
		lines = append(lines, *state.GroupURL)
	}
	return strings.Join(lines, "\n")
}

// statusOf packs Telegram's answer into one string, because that is what a
// completion fingerprint is taken over and what the classification reads.
func statusOf(answer *tgResponse) string {
	return fmt.Sprintf("%d:%s", answer.ErrorCode, answer.Description)
}
