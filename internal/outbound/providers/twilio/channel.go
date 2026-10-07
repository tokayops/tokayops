package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// The phone channel: a call that says there are alerts waiting, placed through
// one of the Twilio integrations that can reach the person.
//
// Its acceptance only means "queued". Twilio places the call afterwards and
// says how it went in callbacks; the domain waits for that word, and asks for
// it when it does not come.

// Directory is what the channel reads to decide where a call goes: the
// person's phone, the integrations, and which of them already refused this
// commitment.
type Directory interface {
	GetPhoneContact(ctx context.Context, userID string) (*model.PhoneContact, error)
	GetIntegrationsByType(integrationType model.IntegrationType) ([]*model.Integration, error)
	GetIntegrationByID(id string) (*model.Integration, error)
	RefusedIntegrations(ctx context.Context, intentID string) ([]string, error)
}

// Caller is the Twilio API as the channel uses it. Satisfied by *Client.
type Caller interface {
	CreateCall(ctx context.Context, cfg model.TwilioConfig, call Call) (string, error)
	CallStatus(ctx context.Context, cfg model.TwilioConfig, sid string) (string, error)
}

// CallbackPath is where Twilio reports how a call goes.
const CallbackPath = "/twilio/voice/status"

// The call's numbers. A minute to ring - the default Twilio gives a person to
// get to the phone - and a minute to say what it has to say twice.
const (
	callRing  = 60 * time.Second
	callTalk  = 60 * time.Second
	callWords = "TokayOps has unacknowledged alerts for you. Open TokayOps."
)

// callbackOverrides are Twilio's own instructions for retrying a callback that
// did not get through: three tries, on a refused connection, a read timeout or
// a 5xx. Without them Twilio tries once and only on a refused connection. The
// fragment is not part of the signed URL.
const callbackOverrides = "#rc=3&rp=ct,rt,5xx"

// statusEvents are the moments of a call Twilio reports.
var statusEvents = []string{"initiated", "ringing", "answered", "completed"}

// Channel is the phone channel.
type Channel struct {
	directory Directory
	caller    Caller
	selfURL   string
}

// NewChannel is the phone channel. selfURL is the public address of this
// installation, the one Twilio's callbacks reach and sign.
func NewChannel(directory Directory, caller Caller, selfURL string) *Channel {
	return &Channel{directory: directory, caller: caller, selfURL: strings.TrimRight(selfURL, "/")}
}

var _ outbound.ReceiptChannel = (*Channel)(nil)

// Prepare decides where the call goes, before any attempt opens.
//
// When the generation opens, the channel chooses the integration: the ones
// that cover the person's verified number, less the ones that already refused
// this commitment, the person's pin first. Inside a generation the store keeps
// the integration it bound; what this checks then is that the number it was
// bound to is still the person's verified one.
func (c *Channel) Prepare(ctx context.Context, intent outbound.Intent) outbound.Preparation {
	if intent.KeyKind != keys.KindEscalation && intent.KeyKind != keys.KindEscalationReplay {
		return outbound.Impossible("unsupported_kind",
			fmt.Sprintf("a call has nothing to say for a %q commitment", intent.KeyKind))
	}
	payload, err := keys.DecodeEscalationPayload(intent.PayloadSchemaVersion, intent.Payload)
	if err != nil {
		return outbound.Impossible("payload_unreadable", err.Error())
	}
	if payload.Target.Kind != intent.TargetKind || payload.Target.Ref != intent.TargetRef {
		return outbound.Impossible("target_mismatch", fmt.Sprintf(
			"the commitment is addressed to %s %q and its message is written for %s %q",
			intent.TargetKind, intent.TargetRef, payload.Target.Kind, payload.Target.Ref))
	}
	if intent.TargetKind != keys.TargetUser {
		return outbound.Impossible("unsupported_target",
			fmt.Sprintf("a call goes to a person, not to a %q", intent.TargetKind))
	}
	if c.selfURL == "" {
		// Without a public address Twilio has nowhere to say how the call
		// went, and the outcome would come only from asking, a minute and a
		// half later. Honest to wait for the address to be configured.
		return outbound.NotNow("no_self_url", "TOKAY_SELF_URL is not set; call outcomes would have nowhere to go")
	}

	contact, err := c.directory.GetPhoneContact(ctx, intent.TargetRef)
	if err != nil {
		return outbound.NotNow("contact_lookup_failed", err.Error())
	}
	switch {
	case contact == nil:
		return outbound.NoContact("no_number", fmt.Sprintf("%s has no phone number", intent.TargetRef))
	case !contact.Verified():
		return outbound.NoContact("unverified", fmt.Sprintf("%s has not verified their phone number", intent.TargetRef))
	case intent.GenerationBound && intent.BoundEndpoint != contact.Value:
		// Bound to a number the person no longer vouches for: the store
		// would call the bound one whatever this proposes.
		return outbound.NoContact("unverified",
			fmt.Sprintf("%s replaced the number this call was bound to", intent.TargetRef))
	}

	rows, err := c.directory.GetIntegrationsByType(model.IntegrationTypeTwilio)
	if err != nil {
		return outbound.NotNow("integrations_unreadable", err.Error())
	}
	all, err := Decode(rows)
	if err != nil {
		return outbound.NotNow("integrations_unreadable", err.Error())
	}
	covering := Covering(all, contact.Value)
	if len(covering) == 0 {
		return outbound.NoContact("not_covered", "no phone provider covers the number")
	}
	if intent.GenerationBound {
		// The integration is the generation's; it is not chosen again.
		return outbound.Ready(contact.Value)
	}

	refused, err := c.directory.RefusedIntegrations(ctx, intent.ID)
	if err != nil {
		return outbound.NotNow("refusals_unreadable", err.Error())
	}
	left := withoutRefused(covering, refused)
	if len(left) == 0 {
		return outbound.Impossible("integrations_exhausted",
			fmt.Sprintf("every provider that covers the number refused this call (%d)", len(refused)))
	}
	chosen, _ := Pick(left, contact.Value, contact.PinnedIntegrationID)
	return outbound.ReadyWith(contact.Value, outbound.BoundContext{
		IntegrationID: chosen.ID,
		AccountScope:  chosen.Config.AccountSID,
		FromNumber:    chosen.Config.FromNumber,
	})
}

func withoutRefused(covering []Integration, refused []string) []Integration {
	out := make([]Integration, 0, len(covering))
	for _, in := range covering {
		gone := false
		for _, id := range refused {
			gone = gone || in.ID == id
		}
		if !gone {
			out = append(out, in)
		}
	}
	return out
}

// ExecuteAttempt places the call through the integration the generation is
// bound to, from the number it was bound to.
func (c *Channel) ExecuteAttempt(ctx context.Context, call outbound.Call) (outbound.Result, error) {
	// A new call only through an integration that is still switched on and
	// still on the account the generation was bound to. Either way round the
	// binding is unusable: nothing leaves this process, the commitment tries
	// again later and, if nothing changes, ends with its deadline. It is not
	// moved to another integration - a request that may have placed a call is
	// repeated the way it was made, or not at all.
	cfg, err := c.config(call.BoundContext, true)
	if err != nil {
		return outbound.Result{Evidence: outbound.DefinitelyNotSent, Summary: err.Error()}, nil
	}
	cfg.FromNumber = call.BoundContext.FromNumber

	words := callWords + " " + callWords
	sid, err := c.caller.CreateCall(ctx, cfg, Call{
		To:             call.Endpoint,
		TwiML:          Say(cfg.SayLanguage(), words),
		Ring:           callRing,
		Limit:          callTalk,
		StatusCallback: c.selfURL + CallbackPath + "?a=" + url.QueryEscape(call.AttemptID) + callbackOverrides,
		Events:         statusEvents,
	})
	var refusal *Refusal
	switch {
	case err == nil:
		raw, _ := json.Marshal(map[string]string{"sid": sid})
		receipt, rerr := outbound.NewReceipt(sid, raw)
		if rerr != nil {
			return outbound.Result{Evidence: outbound.ProviderResponse, Status: "accepted_without_sid",
				Summary: rerr.Error()}, nil
		}
		return outbound.Result{Evidence: outbound.ProviderResponse, Status: "accepted", Receipt: receipt,
			Summary: "call " + sid + " queued"}, nil
	case errors.As(err, &refusal):
		return outbound.Result{Evidence: outbound.ProviderResponse,
			Status:  fmt.Sprintf("%d:%d", refusal.HTTPStatus, refusal.Code),
			Summary: refusal.Error()}, nil
	case errors.Is(err, ErrNotCreated):
		return outbound.Result{Evidence: outbound.DefinitelyNotSent, Summary: err.Error()}, nil
	default:
		// No answer, or one that does not say no: the call may exist.
		return outbound.Result{Evidence: providers.EvidenceOf(err), Summary: err.Error()}, nil
	}
}

// ClassifyResponse says what Twilio's own answer means.
func (c *Channel) ClassifyResponse(_ outbound.Call, res outbound.Result) (outbound.Classification, bool) {
	if res.Status == "accepted" {
		return outbound.Classification{Outcome: outbound.OutcomeAccepted}, true
	}
	httpStatus, code, ok := strings.Cut(res.Status, ":")
	if !ok {
		return outbound.Classification{}, false
	}
	status, err := strconv.Atoi(httpStatus)
	if err != nil {
		return outbound.Classification{}, false
	}
	switch {
	case status == 429 || code == "20429":
		// Twilio does not process a request it answers with 429.
		return outbound.Classification{Outcome: outbound.OutcomeRetryableRejection, Class: "rate_limited"}, true
	case status >= 400 && status < 500:
		// Refused, and the call was not created: 21211 for a number Twilio
		// cannot dial, 20003 for credentials it does not accept. Another
		// integration may do better; the domain decides whether to try.
		return outbound.Classification{Outcome: outbound.OutcomePermanentRejection, Class: "twilio_" + code}, true
	default:
		return outbound.Classification{}, false
	}
}

// EffectStateOf reads a Twilio status.
func (c *Channel) EffectStateOf(event outbound.ProviderEvent) (outbound.EffectState, string, bool) {
	switch event.ProviderStatus {
	case "queued", "initiated", "ringing", "in-progress":
		return outbound.EffectInProgress, event.ProviderStatus, true
	case "completed", "busy", "no-answer":
		// The call was put through and ended. Answered, by a person or a
		// machine; busy; not answered or declined - none of it says somebody
		// heard it, and all of it says the phone was called.
		return outbound.EffectHappened, event.ProviderStatus, true
	case "failed":
		return outbound.EffectNotPlaced, event.ProviderStatus, true
	case "canceled":
		return outbound.EffectWithdrawn, event.ProviderStatus, true
	default:
		return "", "", false
	}
}

// Poll asks Twilio where one call stands, with the account the attempt that
// made it was bound to.
func (c *Channel) Poll(ctx context.Context, ref outbound.EffectRef) (outbound.ProviderEvent, error) {
	// Asking about a call already made is allowed through an integration that
	// was switched off since; asking another account is not - the call is not
	// there, and its silence would read as nothing at all.
	cfg, err := c.config(ref.Context, false)
	if err != nil {
		return outbound.ProviderEvent{}, err
	}
	status, err := c.caller.CallStatus(ctx, cfg, ref.ExternalRef)
	if err != nil {
		return outbound.ProviderEvent{}, err
	}
	return outbound.ProviderEvent{
		Provider: "twilio", AccountScope: cfg.AccountSID, ProviderStatus: status,
		OccurredAt: time.Now().UTC(), Summary: "asked: " + status,
	}, nil
}

// config is the configuration of the integration a call is bound to, read
// from the database each time: a token rotated within the account is used from
// the next request on. The binding is unusable when the integration is gone,
// when it now names another account - the call and its record are in the
// account it was made on, whose token is not kept - or, for a new call, when
// it has been switched off.
func (c *Channel) config(bound outbound.BoundContext, placing bool) (model.TwilioConfig, error) {
	if bound.IntegrationID == "" {
		return model.TwilioConfig{}, errors.New("the call is bound to no integration")
	}
	row, err := c.directory.GetIntegrationByID(bound.IntegrationID)
	if err != nil {
		return model.TwilioConfig{}, fmt.Errorf("read twilio integration %s: %w", bound.IntegrationID, err)
	}
	if row == nil || row.Type != model.IntegrationTypeTwilio {
		return model.TwilioConfig{}, fmt.Errorf("twilio integration %s is gone", bound.IntegrationID)
	}
	if placing && !row.Enabled {
		return model.TwilioConfig{}, fmt.Errorf("twilio integration %s is switched off", bound.IntegrationID)
	}
	var cfg model.TwilioConfig
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return model.TwilioConfig{}, fmt.Errorf("read the config of twilio integration %s: %w", bound.IntegrationID, err)
	}
	if bound.AccountScope != "" && cfg.AccountSID != bound.AccountScope {
		return model.TwilioConfig{}, fmt.Errorf("twilio integration %s is on another account than the call", bound.IntegrationID)
	}
	return cfg, nil
}
