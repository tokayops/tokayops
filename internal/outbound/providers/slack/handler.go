package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// Slack as the outbound worker uses it: resolve an address, make ONE call, and
// say what Slack's answer means.
//
// It shares the renderers with the old path and nothing else. In particular it
// holds no base URL and no team lookup: everything that decides bytes arrives
// in the snapshot, and a handler that could read those live would put this
// instance's configuration into a message that was composed hours ago.

// Handler makes escalation sends for Slack.
type Handler struct {
	tokens   TokenSource
	identity providers.IdentityLookup

	// newClient is how the client is built. It is a field so a test can point
	// it at a server of its own; production has exactly one implementation.
	newClient func(token string) *slackapi.Client
}

// NewHandler builds the channel. The token source is read on every attempt on
// purpose: a rotated token has to apply to work that has not gone out yet, and
// the token is one of the few things that does not change the message.
func NewHandler(tokens TokenSource, identity providers.IdentityLookup) *Handler {
	return &Handler{
		tokens:   tokens,
		identity: identity,
		newClient: func(token string) *slackapi.Client {
			return NewClient(token, HTTPTimeout)
		},
	}
}

// Prepare resolves where this commitment goes, and refuses the calls that
// cannot be made at all.
//
// The address is resolved on every attempt and is only a proposal: an effect
// already bound to an address keeps it, because an identity relinked between
// two attempts would otherwise deliver the same message to two different
// people with nothing to say which one got it.
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
	// Which shape to read it in comes from the KIND of claim. A closed set with
	// no default: two kinds carry two payloads that have nothing to do with
	// each other, and reading one as the other is not a decode failure but a
	// message about the wrong thing.
	//
	// A handover carries no coordinates to check afterwards, and that is not an
	// omission. It is one message that is never changed, so there is nothing a
	// later attempt could aim at and nothing to compare a name against.
	var written keys.Target
	mayBeChanged := false
	switch intent.KeyKind {
	case keys.KindEscalation, keys.KindEscalationReplay:
		payload, err := keys.DecodeEscalationPayloadV1(intent.PayloadSchemaVersion, intent.Payload)
		if err != nil {
			return outbound.Impossible("payload_unreadable", err.Error())
		}
		written, mayBeChanged = payload.Target, true

	case keys.KindHandoff:
		payload, err := keys.DecodeHandoffPayloadV1(intent.PayloadSchemaVersion, intent.Payload)
		if err != nil {
			return outbound.Impossible("payload_unreadable", err.Error())
		}
		written = payload.Target

	default:
		return outbound.Impossible("unsupported_kind", fmt.Sprintf(
			"Slack has nothing to write for a %q commitment", intent.KeyKind))
	}

	// The commitment names a recipient twice: in its own columns, which decide
	// WHERE this goes, and in the payload, which decides WHAT is written. A row
	// where those two disagree would send a person's private message to the
	// channel named in the columns - a leak, not a mangled journal entry - so
	// the two are compared before anything is resolved.
	if written.Kind != intent.TargetKind || written.Ref != intent.TargetRef {
		return outbound.Impossible("target_mismatch", fmt.Sprintf(
			"the commitment is addressed to %s %q and its message is written for %s %q",
			intent.TargetKind, intent.TargetRef, written.Kind, written.Ref))
	}
	if h.tokens == nil || h.tokens.GetSlackToken() == "" {
		return outbound.Impossible("integration_missing",
			"Slack is not configured on this installation")
	}

	// A change is aimed at a message this commitment already made, and the
	// coordinates of it are read HERE for the same reason the payload is. Read
	// inside the call instead, coordinates nobody can parse become an attempt
	// that never touched the network, retried on the family's backoff - and a
	// card has no deadline, so that is forever. Refused here it is what it is:
	// a deterministic refusal, recorded, in front of somebody who can act.
	if mayBeChanged && intent.HasReceipt {
		data, ok := parseData(string(intent.Receipt))
		if !ok {
			return outbound.Impossible("receipt_unreadable",
				"the coordinates of the message to change cannot be read")
		}
		// The coordinates and the name have to be the same message. They are
		// written together and nothing rewrites one without the other, so a row
		// where they disagree is damaged - and the damage is invisible at the
		// call: the change would go to the message the COORDINATES name, and
		// the revision would be recorded as applied to the one the NAME does.
		// Two messages, one of them quietly wrong, and a journal that agrees
		// with neither.
		if named := data.ChannelID + "/" + data.Timestamp; named != intent.ReceiptRef {
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
				"nothing here can turn a user into a Slack account")
		}
		address, err := h.identity(ctx, intent.TargetRef, "slack")
		switch {
		case errors.Is(err, providers.ErrNotLinked):
			return outbound.Impossible("identity_not_linked",
				fmt.Sprintf("%s has not linked a Slack account", intent.TargetRef))
		case err != nil:
			// The lookup itself failed, which says nothing about the person.
			return outbound.NotNow("identity_lookup_failed", err.Error())
		}
		return outbound.Ready(address)

	default:
		return outbound.Impossible("unsupported_target",
			fmt.Sprintf("Slack has no %q to send to", intent.TargetKind))
	}
}

// ExecuteAttempt makes the one external effect of an attempt: a single call to
// Slack, to the address the generation is bound to.
//
// Which call is decided by the attempt, not by this handler: chat.postMessage
// when there is nothing out there yet, chat.update when the attempt is a change
// to a card that exists. Either way it is one call and one effect.
//
// A direct message is the same call. conversations.open is gone: chat.postMessage
// takes a user id in `channel` and opens the conversation itself, so a DM has no
// preparatory call, no second place to fail before the effect, and no
// classification of its own.
//
// It returns the moment Slack answers, and that is the whole point. Anything
// done after the card exists but before the attempt is closed widens the window
// where a crash leaves a message that was delivered and an attempt that says it
// might not have been - which recovery closes as ambiguous, and the retry posts
// the card a second time. Error handling does not help: the danger is the
// process dying, not the call failing.
//
// So the permalink is not fetched and the timeline is not posted in the card's
// thread. The permalink has no reader in this build - a direct message links to
// the alert in TokayOps, not to a card - and the timeline post is not
// enrichment at all: it is a SECOND message, which is a second external effect
// and would need a commitment of its own. Nothing here adds one, and the
// history left the render snapshot with it; bringing either back is a decision
// somebody makes on purpose, not a line added to this function.
func (h *Handler) ExecuteAttempt(ctx context.Context, call outbound.Call) (outbound.Result, error) {
	token := ""
	if h.tokens != nil {
		token = h.tokens.GetSlackToken()
	}
	if token == "" {
		return outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary:  "Slack is not configured on this installation",
		}, ErrNoToken
	}
	client := h.newClient(token)

	options, refusal, err := h.write(call)
	if err != nil {
		return refusal, err
	}

	if call.AttemptKind == outbound.AttemptMutation {
		return updateMessage(ctx, client, call, options)
	}

	channelID, timestamp, err := client.PostMessageContext(ctx, call.Endpoint, options...)
	if err != nil {
		evidence, status := answerOf(err)
		return outbound.Result{
			Evidence: evidence, Status: status, Summary: err.Error(),
		}, err
	}

	result := outbound.Result{Evidence: outbound.ProviderResponse, Status: "ok"}
	if channelID == "" || timestamp == "" {
		// Slack said yes and would not say what it made. Conclude turns this
		// into doubt; what is recorded here is what it actually answered.
		result.Summary = fmt.Sprintf("accepted with channel=%q ts=%q", channelID, timestamp)
		return result, nil
	}

	raw, err := json.Marshal(Data{ChannelID: channelID, Timestamp: timestamp})
	if err != nil {
		result.Summary = "the coordinates could not be recorded: " + err.Error()
		return result, nil
	}
	receipt, err := outbound.NewReceipt(channelID+"/"+timestamp, raw)
	if err != nil {
		result.Summary = "the coordinates could not be recorded: " + err.Error()
		return result, nil
	}
	result.Receipt = receipt
	return result, nil
}

// updateMessage brings the message this commitment already made to the state it
// is now supposed to show.
//
// One call, to the coordinates the commitment holds. The channel is part of
// those coordinates rather than a place to send to: Slack's own documentation
// says it identifies the message, and a message is never moved by an update.
//
// It is the same call for an update and for a resolution, and that is not an
// omission - the last revision of a card is a revision like any other. What
// makes it the last is the state, not the request.
func updateMessage(ctx context.Context, client *slackapi.Client, call outbound.Call,
	options []slackapi.MsgOption) (outbound.Result, error) {

	data, ok := parseData(string(call.Receipt))
	if !ok {
		// The store refuses to make this call without coordinates, so reaching
		// here means the stored ones cannot be read - a broken row rather than
		// a message that moved.
		return outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary:  "the coordinates of the message to change cannot be read",
		}, ErrNoReceipt
	}

	channelID, timestamp, _, err := client.UpdateMessageContext(
		ctx, data.ChannelID, data.Timestamp, options...)
	if err != nil {
		evidence, status := answerOf(err)
		return outbound.Result{
			Evidence: evidence, Status: status, Summary: err.Error(),
		}, err
	}

	result := outbound.Result{Evidence: outbound.ProviderResponse, Status: "ok"}
	if channelID == "" || timestamp == "" {
		// Accepted, and it did not repeat the coordinates back. That is fine
		// for a change: the object it was applied to is the one the commitment
		// already holds, and nothing new was made to be named.
		result.Summary = "the message was updated"
		return result, nil
	}

	raw, err := json.Marshal(Data{ChannelID: channelID, Timestamp: timestamp})
	if err != nil {
		result.Summary = "the coordinates could not be recorded: " + err.Error()
		return result, nil
	}
	receipt, err := outbound.NewReceipt(channelID+"/"+timestamp, raw)
	if err != nil {
		result.Summary = "the coordinates could not be recorded: " + err.Error()
		return result, nil
	}
	// Handed back so the domain can compare them with the ones it asked about.
	// A change that answers with a different message is a channel that has lost
	// track of what it was asked to do, and following it would take this card
	// to somebody else's message.
	result.Receipt = receipt
	return result, nil
}

// ClassifyResponse says what Slack's own answer means, and says "I do not know"
// rather than guessing.
//
// The rule behind the lists is the capability matrix: a rejection is declared
// only where Slack's documentation proves the message was not created.
// Everything else - including its own internal errors, about which the
// documentation says some part of the operation may have succeeded - is doubt,
// and doubt is the domain's default for anything not named here.
func (h *Handler) ClassifyResponse(res outbound.Result) (outbound.Classification, bool) {
	switch res.Status {
	case "ok":
		return outbound.Classification{Outcome: outbound.OutcomeAccepted}, true

	case "ratelimited", "rate_limited", "request_timeout":
		// Slack answered that it did not process the request.
		return outbound.Classification{
			Outcome: outbound.OutcomeRetryableRejection, Class: res.Status,
		}, true

	case "fatal_error", "internal_error", "service_unavailable":
		// "It's possible some aspect of the operation succeeded before the
		// error was raised" - Slack's own words.
		return outbound.Classification{
			Outcome: outbound.OutcomeAmbiguous, Class: res.Status,
		}, true

	case "message_not_found":
		// The message this change is for is gone. It is the one thing an
		// ordinary answer proves about the object, and the only ground on
		// which an operator may be allowed to make a second one.
		absent := keys.DetailDefinitelyAbsent
		return outbound.Classification{
			Outcome: outbound.OutcomePermanentRejection, Class: res.Status,
			Detail: &absent,
		}, true

	case "cant_update_message", "edit_window_closed":
		// The message is there and will not change - which is the opposite
		// fact, and deliberately stated as no fact at all: without proof of
		// absence, nobody may create a second message beside it.
		return outbound.Classification{
			Outcome: outbound.OutcomePermanentRejection, Class: res.Status,
		}, true

	case "channel_not_found", "not_in_channel", "is_archived", "invalid_auth",
		"account_inactive", "token_revoked", "no_permission", "msg_too_long",
		"invalid_blocks", "restricted_action", "cannot_dm_bot", "user_not_found":
		return outbound.Classification{
			Outcome: outbound.OutcomePermanentRejection, Class: res.Status,
		}, true
	}

	if strings.HasPrefix(res.Status, "http_5") {
		return outbound.Classification{
			Outcome: outbound.OutcomeAmbiguous, Class: res.Status,
		}, true
	}
	return outbound.Classification{}, false
}

// write turns the commitment into what Slack is asked to post.
//
// The kind decides where the words come from, and the two are not alike: an
// alert card is drawn from a state frozen at admission and follows that alert
// afterwards, while a shift change IS its payload and is written once. Sharing
// one path would mean one of them rendering from something it does not have.
//
// A refusal here is DefinitelyNotSent: nothing has been called yet, and saying
// so is what keeps a payload nobody can read from being recorded as a call
// whose fate is unknown and retried on the family's backoff forever.
func (h *Handler) write(call outbound.Call) ([]slackapi.MsgOption, outbound.Result, error) {
	switch call.KeyKind {
	case keys.KindHandoff:
		payload, err := keys.DecodeHandoffPayloadV1(call.PayloadSchemaVersion, call.Payload)
		if err != nil {
			return nil, outbound.Result{
				Evidence: outbound.DefinitelyNotSent,
				Summary:  "the commitment's payload cannot be read: " + err.Error(),
			}, err
		}
		return []slackapi.MsgOption{
			slackapi.MsgOptionText(announcement(payload), false),
		}, outbound.Result{}, nil

	case keys.KindEscalation, keys.KindEscalationReplay:
		payload, err := keys.DecodeEscalationPayloadV1(call.PayloadSchemaVersion, call.Payload)
		if err != nil {
			return nil, outbound.Result{
				Evidence: outbound.DefinitelyNotSent,
				Summary:  "the commitment's payload cannot be read: " + err.Error(),
			}, err
		}
		snapshot, drawn := call.Content.Snapshot()
		if !drawn {
			// This kind draws an alert card from a frozen state, and this
			// commitment has none. Not an empty card: a message about nothing
			// is worse than a message that did not go.
			return nil, outbound.Result{
				Evidence: outbound.DefinitelyNotSent,
				Summary: fmt.Sprintf(
					"a %s commitment renders from a state, and this one carries none",
					call.KeyKind),
			}, ErrNoContent
		}
		return messageFor(snapshot.Content(), payload), outbound.Result{}, nil

	default:
		return nil, outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary: fmt.Sprintf(
				"Slack has nothing to write for a %q commitment", call.KeyKind),
		}, ErrNoContent
	}
}

// messageFor turns the snapshot into the call's content: a card for a channel,
// the escalation's own words for a person.
func messageFor(state keys.SnapshotInput, payload keys.EscalationPayloadV1) []slackapi.MsgOption {
	if payload.Target.Kind == keys.TargetUser {
		return []slackapi.MsgOption{slackapi.MsgOptionText(directMessage(state, payload), false)}
	}
	card := Render(state, payload.Interactive)
	return []slackapi.MsgOption{
		slackapi.MsgOptionText(card.Text, false),
		slackapi.MsgOptionBlocks(card.Blocks...),
		slackapi.MsgOptionAttachments(card.Attachment),
	}
}

// directMessage is what a person is told, and it is built from this commitment
// alone.
//
// The link is the alert group in TokayOps, not the card posted in some channel.
// A permalink would have to be read from a NEIGHBOURING delivery, which means
// the first attempt (before that card exists) and a retry (after it does) would
// carry different bytes under one provider key - a difference the request
// fingerprint, taken at Begin from the snapshot, cannot even see.
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
		lines = append(lines, fmt.Sprintf("<%s|Open in TokayOps>", *state.GroupURL))
	}
	return strings.Join(lines, "\n")
}

// answerOf separates Slack answering from Slack not answering, which is the
// distinction the whole classification rests on.
func answerOf(err error) (outbound.Evidence, string) {
	var api slackapi.SlackErrorResponse
	if errors.As(err, &api) {
		return outbound.ProviderResponse, api.Err
	}
	var rateLimited *slackapi.RateLimitedError
	if errors.As(err, &rateLimited) {
		return outbound.ProviderResponse, "ratelimited"
	}
	var status slackapi.StatusCodeError
	if errors.As(err, &status) {
		return outbound.ProviderResponse, fmt.Sprintf("http_%d", status.Code)
	}
	return providers.EvidenceOf(err), ""
}
