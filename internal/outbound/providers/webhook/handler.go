package webhook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// An outgoing webhook as the outbound worker uses it: resolve where a subscriber
// lives, make ONE POST, and say what the subscriber's answer means.
//
// What it reads and when is the whole design (D5, overlay webhook). The address
// is resolved in preparation and bound to the generation, so a retry goes where
// the first attempt went. The secret, the custom headers and the timeout are
// read again on every attempt, from the database and not from a cache: the
// cache is per process and refreshed only by the process that handled the
// change, and a secret rotated through one instance would leave every other
// signing with the old one until a restart.

// SubscriberConfigs is what the channel needs from the store: one subscriber's
// configuration, by the integration id the commitment names. The bool says
// whether the subscriber exists; an error is the database failing, and the two
// are kept apart because they mean opposite things to a delivery - one ends it,
// the other waits.
type SubscriberConfigs interface {
	SubscriberConfig(ctx context.Context, integrationID string) (model.GenericWebhookConfig, bool, error)
}

// Handler makes deliveries to subscribers.
type Handler struct {
	configs SubscriberConfigs
	policy  ipPolicy
	budget  time.Duration

	// newClient builds the HTTP client for one call. A field so a test can
	// see the client's policy; production has one implementation.
	newClient func(timeout time.Duration) *http.Client
}

// NewHandler builds the channel. allowedCIDRs are the private ranges this
// installation permits a subscriber to live in - none by default.
func NewHandler(configs SubscriberConfigs, allowedCIDRs []*net.IPNet) *Handler {
	return NewHandlerResolving(configs, allowedCIDRs, SystemResolver)
}

// NewHandlerResolving is NewHandler with the resolver supplied, for the tests
// that need a name to answer one thing in preparation and another at the dial.
func NewHandlerResolving(configs SubscriberConfigs, allowedCIDRs []*net.IPNet, resolve Resolver) *Handler {
	policy := ipPolicy{allowed: allowedCIDRs, resolve: resolve}
	budget := outbound.WebhookConfigReadBudget
	if p, err := outbound.PolicyOf(outbound.FamilyWebhook); err == nil {
		budget = p.ConfigReadBudget
	}
	return &Handler{
		configs: configs,
		policy:  policy,
		budget:  budget,
		newClient: func(timeout time.Duration) *http.Client {
			return &http.Client{
				Timeout:   timeout,
				Transport: &http.Transport{DialContext: policy.dial},
				// A redirect is not followed. The address is bound to the
				// generation, and a followed redirect would post somewhere the
				// journal does not say; and a chain of redirects is the classic
				// way to carry a request past an address check. The 3xx comes
				// back as the answer and is classified as one.
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}
		},
	}
}

// Prepare resolves where this commitment goes and refuses the calls that cannot
// be made at all.
//
// It reads the subscriber's configuration to REFUSE before an attempt is
// opened: no subscriber, an empty URL or a forbidden address are deterministic
// refusals, and refused here they are what they are - recorded, in front of
// somebody who can act - instead of becoming a call that never touched the
// network, retried on the family's backoff for as long as the commitment lives.
func (h *Handler) Prepare(ctx context.Context, intent outbound.Intent) outbound.Preparation {
	payload, class, summary, ok := h.readPayload(intent.KeyKind, intent.PayloadSchemaVersion, intent.Payload)
	if !ok {
		return outbound.Impossible(class, summary)
	}
	if payload.Target.Kind != intent.TargetKind || payload.Target.Ref != intent.TargetRef {
		return outbound.Impossible("target_mismatch", fmt.Sprintf(
			"the commitment is addressed to %s %q and its payload is for %s %q",
			intent.TargetKind, intent.TargetRef, payload.Target.Kind, payload.Target.Ref))
	}

	cfg, found, err := h.configs.SubscriberConfig(ctx, intent.TargetRef)
	switch {
	case err != nil:
		// The database, not the subscriber. Nothing is known about the
		// configuration, so nothing is decided about the commitment.
		return outbound.NotNow("config_read_failed", err.Error())
	case !found:
		return outbound.Impossible("integration_missing",
			fmt.Sprintf("subscriber %s does not exist", intent.TargetRef))
	}
	if cfg.URL == "" {
		return outbound.Impossible("url_missing",
			fmt.Sprintf("subscriber %s has no URL", intent.TargetRef))
	}
	target, err := url.Parse(cfg.URL)
	if err != nil || target.Hostname() == "" {
		return outbound.Impossible("url_invalid",
			fmt.Sprintf("subscriber %s has an unusable URL", intent.TargetRef))
	}

	// The address policy, on every address the name resolves to. A blocked
	// address is a configuration that will not change by itself, so it ends
	// the commitment; a resolver that does not answer is the network, and the
	// commitment waits.
	if err := h.policy.check(ctx, target.Hostname()); err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return outbound.Impossible("ip_policy", err.Error())
		}
		return outbound.NotNow("dns", err.Error())
	}
	return outbound.Ready(cfg.URL)
}

// ExecuteAttempt makes the one external effect: a POST of the event body to the
// address the generation is bound to.
//
// The configuration is read again here, for the secret, the headers and the
// timeout - never for the address, which is call.Endpoint. The read has a
// budget of its own inside the attempt; a failure of it is a request that
// provably never left, and is said to be, because the general rule for an
// unrecognised error is doubt and doubt here would be false.
func (h *Handler) ExecuteAttempt(ctx context.Context, call outbound.Call) (outbound.Result, error) {
	payload, _, summary, ok := h.readPayload(call.KeyKind, call.PayloadSchemaVersion, call.Payload)
	if !ok {
		return outbound.Result{Evidence: outbound.DefinitelyNotSent, Summary: summary}, errors.New(summary)
	}
	if call.Endpoint == "" {
		return outbound.Result{Evidence: outbound.DefinitelyNotSent,
			Summary: "no address is bound to this generation"}, errors.New("webhook: no address")
	}

	readCtx, cancelRead := context.WithTimeout(ctx, h.budget)
	cfg, found, err := h.configs.SubscriberConfig(readCtx, payload.Target.Ref)
	cancelRead()
	switch {
	case err != nil:
		return outbound.Result{Evidence: outbound.DefinitelyNotSent,
			Summary: "the subscriber's configuration could not be read: " + err.Error()}, err
	case !found:
		// Deleted between the preparation and the call. Nothing went out; the
		// next preparation refuses for good.
		err := fmt.Errorf("webhook: subscriber %s no longer exists", payload.Target.Ref)
		return outbound.Result{Evidence: outbound.DefinitelyNotSent, Summary: err.Error()}, err
	}

	body := []byte(payload.Body)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	headers := Headers(payload.EventID, model.OutboxEventType(payload.EventType), body,
		cfg.Secret, cfg.CustomHeaders, timestamp)

	timeout := EffectiveTimeout(cfg)
	callCtx, cancelCall := context.WithTimeout(ctx, timeout)
	defer cancelCall()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, call.Endpoint, strings.NewReader(payload.Body))
	if err != nil {
		return outbound.Result{Evidence: outbound.DefinitelyNotSent, Summary: "build the request: " + err.Error()}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.newClient(timeout).Do(req)
	if err != nil {
		return outbound.Result{Evidence: providers.EvidenceOf(err), Summary: err.Error()}, err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return outbound.Result{
		Evidence: outbound.ProviderResponse,
		Status:   strconv.Itoa(resp.StatusCode),
		Summary:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(answer))),
	}, nil
}

// readPayload is the one decoder both halves use, and the closed switch over
// kinds: this channel delivers webhook events and nothing else. When it cannot
// read, it answers with the refusal's class and summary, and the caller decides
// whether that is a preparation refused or a call that never left.
func (h *Handler) readPayload(kind keys.Kind, schema int, raw []byte) (keys.WebhookPayloadV1, string, string, bool) {
	switch kind {
	case keys.KindWebhookEvent, keys.KindWebhookReplay:
		payload, err := keys.DecodeWebhookPayloadV1(schema, raw)
		if err != nil {
			return keys.WebhookPayloadV1{}, "payload_unreadable", err.Error(), false
		}
		return payload, "", "", true
	default:
		return keys.WebhookPayloadV1{}, "unsupported_kind",
			fmt.Sprintf("a webhook has nothing to post for a %q commitment", kind), false
	}
}

// ClassifyResponse says what an HTTP status from a subscriber means, BY RANGE.
//
// There is no documentation to prove anything here, so the ranges are the
// contract this system declares to subscribers (D5, overlay webhook): 2xx is
// acceptance; 429 and 408 say the request was not processed; every other 4xx
// says the request itself is wrong and will not be retried; 3xx is a redirect
// this system does not follow. Everything else - 5xx included - is left to the
// domain, whose answer is doubt.
func (h *Handler) ClassifyResponse(res outbound.Result) (outbound.Classification, bool) {
	code, err := strconv.Atoi(res.Status)
	if err != nil {
		return outbound.Classification{}, false
	}
	switch {
	case code >= 200 && code < 300:
		return outbound.Classification{Outcome: outbound.OutcomeAccepted}, true
	case code == http.StatusTooManyRequests:
		return outbound.Classification{Outcome: outbound.OutcomeRetryableRejection, Class: "rate_limited"}, true
	case code == http.StatusRequestTimeout:
		return outbound.Classification{Outcome: outbound.OutcomeRetryableRejection, Class: "request_timeout"}, true
	case code >= 300 && code < 400:
		return outbound.Classification{Outcome: outbound.OutcomePermanentRejection, Class: "redirect_not_followed"}, true
	case code >= 400 && code < 500:
		return outbound.Classification{Outcome: outbound.OutcomePermanentRejection, Class: "rejected_4xx"}, true
	default:
		return outbound.Classification{}, false
	}
}
