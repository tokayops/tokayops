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
	// deterministic refusal with a record saying so, and a commitment that
	// stops where a person can see it.
	if _, err := payloadOf(intent.PayloadSchemaVersion, intent.Payload); err != nil {
		return outbound.Impossible("payload_unreadable", err.Error())
	}
	if h.tokens == nil || h.tokens.GetSlackToken() == "" {
		return outbound.Impossible("integration_missing",
			"Slack is not configured on this installation")
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

// ExecuteAttempt makes the one external effect of an attempt: a single
// chat.postMessage, to the address the generation is bound to.
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
// and belongs to a commitment of its own. Sprint 2 adds it as one.
func (h *Handler) ExecuteAttempt(ctx context.Context, call outbound.Call) (outbound.Result, error) {
	payload, err := payloadOf(call.PayloadSchemaVersion, call.Payload)
	if err != nil {
		// Nothing was sent, and nothing will be until the payload is readable.
		// It is reported as an ordinary refusal rather than a permanent one
		// because the domain classifies transport, not content: the retry will
		// fail the same way, visibly, until somebody looks.
		return outbound.Result{
			Evidence: outbound.DefinitelyNotSent,
			Summary:  "the commitment's payload cannot be read: " + err.Error(),
		}, err
	}

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

	state := call.State.Content()
	options := messageFor(state, payload)

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

// ClassifyResponse says what Slack's own answer means, and says "I do not know"
// rather than guessing.
//
// The rule behind the lists is the capability matrix: a rejection is declared
// only where Slack's documentation proves the message was not created.
// Everything else - including its own internal errors, about which the
// documentation says some part of the operation may have succeeded - is doubt,
// and doubt is the domain's default for anything not named here.
func (h *Handler) ClassifyResponse(res outbound.Result) (outbound.Outcome, string, bool) {
	switch res.Status {
	case "ok":
		return outbound.OutcomeAccepted, "", true

	case "ratelimited", "rate_limited", "request_timeout":
		// Slack answered that it did not process the request.
		return outbound.OutcomeRetryableRejection, res.Status, true

	case "fatal_error", "internal_error", "service_unavailable":
		// "It's possible some aspect of the operation succeeded before the
		// error was raised" - Slack's own words.
		return outbound.OutcomeAmbiguous, res.Status, true

	case "channel_not_found", "not_in_channel", "is_archived", "invalid_auth",
		"account_inactive", "token_revoked", "no_permission", "msg_too_long",
		"invalid_blocks", "restricted_action", "cannot_dm_bot", "user_not_found":
		return outbound.OutcomePermanentRejection, res.Status, true
	}

	if strings.HasPrefix(res.Status, "http_5") {
		return outbound.OutcomeAmbiguous, res.Status, true
	}
	return "", "", false
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

// payloadOf reads a commitment's payload under the schema it says it is in,
// rather than under today's.
func payloadOf(schemaVersion int, raw []byte) (keys.EscalationPayloadV1, error) {
	var payload keys.EscalationPayloadV1
	if schemaVersion != payload.SchemaVersion() {
		return payload, fmt.Errorf("payload schema %d is not one this build renders",
			schemaVersion)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("the payload cannot be read: %w", err)
	}
	if err := payload.Target.Validate(); err != nil {
		return payload, err
	}
	return payload, nil
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
