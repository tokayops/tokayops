package twilio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

type fakeDirectory struct {
	contact      *model.PhoneContact
	integrations []*model.Integration
	refused      []string
}

func (d *fakeDirectory) GetPhoneContact(context.Context, string) (*model.PhoneContact, error) {
	return d.contact, nil
}
func (d *fakeDirectory) GetIntegrationsByType(model.IntegrationType) ([]*model.Integration, error) {
	return d.integrations, nil
}
func (d *fakeDirectory) GetIntegrationByID(id string) (*model.Integration, error) {
	for _, in := range d.integrations {
		if in.ID == id {
			return in, nil
		}
	}
	return nil, nil
}
func (d *fakeDirectory) RefusedIntegrations(context.Context, string) ([]string, error) {
	return d.refused, nil
}

type fakeCaller struct {
	err     error
	status  string
	made    []Call
	madeVia []model.TwilioConfig
	asked   []model.TwilioConfig
}

func (c *fakeCaller) CreateCall(_ context.Context, cfg model.TwilioConfig, call Call) (string, error) {
	c.made = append(c.made, call)
	c.madeVia = append(c.madeVia, cfg)
	if c.err != nil {
		return "", c.err
	}
	return "CA9", nil
}

func (c *fakeCaller) CallStatus(_ context.Context, cfg model.TwilioConfig, _ string) (string, error) {
	c.asked = append(c.asked, cfg)
	return c.status, nil
}

func twilioRow(id, from string, priority int, coverage ...string) *model.Integration {
	cfg, _ := json.Marshal(model.TwilioConfig{
		AccountSID: "AC" + strings.Repeat("0", 31) + id[len(id)-1:], AuthToken: "token-" + id,
		FromNumber: from, Coverage: coverage, Priority: priority, CPS: 1, MaxConcurrent: 1,
	})
	return &model.Integration{ID: id, Type: model.IntegrationTypeTwilio, Name: id, Enabled: true, Config: cfg}
}

func callIntent(t *testing.T) outbound.Intent {
	t.Helper()
	commitment := keys.EscalationCommitment{
		Slot: keys.Slot{Kind: keys.SlotPolicy, Index: 1}, Provider: keys.ProviderPhone,
		Target:         keys.Target{Kind: keys.TargetUser, Ref: "alice"},
		Timing:         keys.TimingSpec{Kind: keys.TimingRelativeToAdmission},
		CompletionMode: keys.CompletionOnProviderReceipt, AmbiguityPolicy: keys.PolicyRetry,
	}
	payload, err := json.Marshal(keys.EscalationPayloadV2{Slot: commitment.Slot, Target: commitment.Target})
	if err != nil {
		t.Fatal(err)
	}
	return outbound.Intent{
		ID: "intent-1", KeyKind: keys.KindEscalation, Provider: keys.ProviderPhone,
		TargetKind: keys.TargetUser, TargetRef: "alice", Form: outbound.FormOneShot,
		CompletionMode: keys.CompletionOnProviderReceipt, PayloadSchemaVersion: 2, Payload: payload,
	}
}

func verified(number string) *model.PhoneContact {
	at := time.Now()
	return &model.PhoneContact{Value: number, VerifiedAt: &at}
}

// prepared reads a preparation the way the store does: through the request it
// becomes.
func prepared(p outbound.Preparation) outbound.BeginAttemptRequest {
	return p.Request("intent-1", "t", "w")
}

func TestPreparationDecidesWhereTheCallGoes(t *testing.T) {
	ru := twilioRow("ru-1", "+79990000001", 1, "+7")
	world := twilioRow("world-2", "+15005550002", 5, "+")

	for _, tc := range []struct {
		name    string
		contact *model.PhoneContact
		refused []string
		bound   string
		selfURL string
		outcome outbound.PreparationOutcome
		class   string
		chosen  string
	}{
		{name: "no number", outcome: outbound.PreparationNoContact, class: "no_number"},
		{name: "not verified", contact: &model.PhoneContact{Value: "+79161234567"},
			outcome: outbound.PreparationNoContact, class: "unverified"},
		{name: "only the worldwide one covers it", contact: verified("+442071234567"),
			outcome: outbound.PreparationReady, chosen: "world-2"},
		{name: "the cheaper one first", contact: verified("+79161234567"),
			outcome: outbound.PreparationReady, chosen: "ru-1"},
		{name: "past the one that refused", contact: verified("+79161234567"), refused: []string{"ru-1"},
			outcome: outbound.PreparationReady, chosen: "world-2"},
		{name: "every one refused", contact: verified("+79161234567"), refused: []string{"ru-1", "world-2"},
			outcome: outbound.PreparationPermanent, class: "integrations_exhausted"},
		{name: "bound to a number since replaced", contact: verified("+79161234567"), bound: "+79160000000",
			outcome: outbound.PreparationNoContact, class: "unverified"},
		{name: "no public address", contact: verified("+79161234567"), selfURL: "-",
			outcome: outbound.PreparationTransient, class: "no_self_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []*model.Integration{ru, world}
			if tc.name == "only the worldwide one covers it" {
				rows = []*model.Integration{world}
			}
			self := "https://tokay.example"
			if tc.selfURL == "-" {
				self = ""
			}
			channel := NewChannel(&fakeDirectory{contact: tc.contact, integrations: rows, refused: tc.refused},
				&fakeCaller{}, self)
			intent := callIntent(t)
			if tc.bound != "" {
				intent.GenerationBound, intent.BoundEndpoint = true, tc.bound
			}
			req := prepared(channel.Prepare(context.Background(), intent))
			if req.Preparation != tc.outcome {
				t.Fatalf("outcome = %s (%s: %s), want %s", req.Preparation, req.ErrorClass, req.Summary, tc.outcome)
			}
			if tc.class != "" && req.ErrorClass != tc.class {
				t.Fatalf("class = %s, want %s", req.ErrorClass, tc.class)
			}
			if tc.chosen != "" && req.BoundContext.IntegrationID != tc.chosen {
				t.Fatalf("chose %s, want %s", req.BoundContext.IntegrationID, tc.chosen)
			}
		})
	}

	// A number no integration covers at all is nobody to reach, not a failure.
	channel := NewChannel(&fakeDirectory{contact: verified("+442071234567"),
		integrations: []*model.Integration{ru}}, &fakeCaller{}, "https://tokay.example")
	if req := prepared(channel.Prepare(context.Background(), callIntent(t))); req.Preparation != outbound.PreparationNoContact ||
		req.ErrorClass != "not_covered" {
		t.Fatalf("a number nobody covers: %s %s", req.Preparation, req.ErrorClass)
	}
}

// Inside a generation the integration is not chosen again: the store keeps the
// bound one, and the channel proposes none.
func TestABoundGenerationKeepsItsIntegration(t *testing.T) {
	channel := NewChannel(&fakeDirectory{contact: verified("+79161234567"),
		integrations: []*model.Integration{twilioRow("ru-1", "+79990000001", 1, "+7")}},
		&fakeCaller{}, "https://tokay.example")
	intent := callIntent(t)
	intent.GenerationBound, intent.BoundEndpoint = true, "+79161234567"
	req := prepared(channel.Prepare(context.Background(), intent))
	if req.Preparation != outbound.PreparationReady || !req.BoundContext.Empty() {
		t.Fatalf("a bound generation: %s, context %+v", req.Preparation, req.BoundContext)
	}
}

// The call goes through the bound integration, from the bound number, and tells
// Twilio where to report - the attempt named in the query, the retries in the
// fragment.
func TestACallIsPlacedTheWayItsGenerationIsBound(t *testing.T) {
	caller := &fakeCaller{}
	row := twilioRow("ru-1", "+79990000009", 1, "+7")
	channel := NewChannel(&fakeDirectory{integrations: []*model.Integration{row}}, caller, "https://tokay.example/")
	result, err := channel.ExecuteAttempt(context.Background(), outbound.Call{
		AttemptID: "att 1", Endpoint: "+79161234567",
		BoundContext: outbound.BoundContext{IntegrationID: "ru-1", FromNumber: "+79990000001"},
	})
	if err != nil || result.Status != "accepted" || result.Receipt.Ref() != "CA9" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	made := caller.made[0]
	if caller.madeVia[0].FromNumber != "+79990000001" {
		t.Errorf("called from %s, not the bound number", caller.madeVia[0].FromNumber)
	}
	if made.To != "+79161234567" {
		t.Errorf("called %s", made.To)
	}
	if made.StatusCallback != "https://tokay.example/twilio/voice/status?a="+url.QueryEscape("att 1")+"#rc=3&rp=ct,rt,5xx" {
		t.Errorf("callback = %s", made.StatusCallback)
	}
	if strings.Join(made.Events, ",") != "initiated,ringing,answered,completed" {
		t.Errorf("events = %v", made.Events)
	}
	if !strings.Contains(made.TwiML, "unacknowledged alerts") {
		t.Errorf("twiml = %s", made.TwiML)
	}
}

func TestWhatACallAttemptProves(t *testing.T) {
	row := twilioRow("ru-1", "+79990000001", 1, "+7")
	bound := outbound.BoundContext{IntegrationID: "ru-1", FromNumber: "+79990000001"}
	for _, tc := range []struct {
		name     string
		err      error
		evidence outbound.Evidence
		outcome  outbound.Outcome
		known    bool
	}{
		{"refused number", &Refusal{HTTPStatus: 400, Code: 21211}, outbound.ProviderResponse, outbound.OutcomePermanentRejection, true},
		{"too many requests", &Refusal{HTTPStatus: 429, Code: 20429}, outbound.ProviderResponse, outbound.OutcomeRetryableRejection, true},
		{"never built", fmt.Errorf("%w: bad url", ErrNotCreated), outbound.DefinitelyNotSent, "", false},
		{"no answer", fmt.Errorf("%w: %w", ErrUnknown, context.DeadlineExceeded), outbound.PossiblySent, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := NewChannel(&fakeDirectory{integrations: []*model.Integration{row}},
				&fakeCaller{err: tc.err}, "https://tokay.example")
			call := outbound.Call{AttemptID: "a", Endpoint: "+79161234567", BoundContext: bound}
			result, _ := channel.ExecuteAttempt(context.Background(), call)
			if result.Evidence != tc.evidence {
				t.Fatalf("evidence = %s, want %s", result.Evidence, tc.evidence)
			}
			if tc.evidence != outbound.ProviderResponse {
				return
			}
			classified, known := channel.ClassifyResponse(call, result)
			if known != tc.known || classified.Outcome != tc.outcome {
				t.Fatalf("classified %+v %v", classified, known)
			}
		})
	}

	// The integration was deleted while the call was owed: nothing left this
	// process.
	channel := NewChannel(&fakeDirectory{}, &fakeCaller{}, "https://tokay.example")
	result, _ := channel.ExecuteAttempt(context.Background(), outbound.Call{BoundContext: bound})
	if result.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("a call through a deleted integration: %s", result.Evidence)
	}
}

// Twilio's statuses, as the domain reads them.
func TestTwilioStatusesInTheDomainsWords(t *testing.T) {
	channel := NewChannel(&fakeDirectory{}, &fakeCaller{}, "")
	for status, want := range map[string]outbound.EffectState{
		"queued": outbound.EffectInProgress, "initiated": outbound.EffectInProgress,
		"ringing": outbound.EffectInProgress, "in-progress": outbound.EffectInProgress,
		"completed": outbound.EffectHappened, "busy": outbound.EffectHappened, "no-answer": outbound.EffectHappened,
		"failed": outbound.EffectNotPlaced, "canceled": outbound.EffectWithdrawn,
	} {
		got, _, known := channel.EffectStateOf(outbound.ProviderEvent{ProviderStatus: status})
		if !known || got != want {
			t.Errorf("%s = %s %v, want %s", status, got, known, want)
		}
	}
	if _, _, known := channel.EffectStateOf(outbound.ProviderEvent{ProviderStatus: "answered-by-robot"}); known {
		t.Error("a status this build never saw was read")
	}
}

// A poll asks with the account the attempt was bound to, not with whichever
// integration a call would go through now.
func TestAPollAsksTheAccountTheCallWasPlacedOn(t *testing.T) {
	caller := &fakeCaller{status: "completed"}
	first, second := twilioRow("ru-1", "+79990000001", 1, "+7"), twilioRow("world-2", "+15005550002", 0, "+")
	channel := NewChannel(&fakeDirectory{integrations: []*model.Integration{first, second}}, caller, "")
	event, err := channel.Poll(context.Background(), outbound.EffectRef{
		ExternalRef: "CA1", Context: outbound.BoundContext{IntegrationID: "ru-1"},
	})
	if err != nil || event.ProviderStatus != "completed" {
		t.Fatalf("poll = %+v, %v", event, err)
	}
	if caller.asked[0].AuthToken != "token-ru-1" {
		t.Fatalf("asked with %s", caller.asked[0].AuthToken)
	}
	if _, err := channel.Poll(context.Background(), outbound.EffectRef{ExternalRef: "CA1"}); err == nil {
		t.Fatal("a poll with no binding asked somebody")
	}
}

// A binding is usable while its integration is switched on and still on the
// account the call was bound to. A token rotated within the account is used
// from the next request on.
func TestABindingIsUsableOnlyAsItWasMade(t *testing.T) {
	bound := outbound.BoundContext{IntegrationID: "ru-1", AccountScope: "AC" + strings.Repeat("0", 31) + "1",
		FromNumber: "+79990000001"}
	place := func(row *model.Integration) (*fakeCaller, outbound.Result) {
		caller := &fakeCaller{status: "completed"}
		channel := NewChannel(&fakeDirectory{integrations: []*model.Integration{row}}, caller, "https://tokay.example")
		result, _ := channel.ExecuteAttempt(context.Background(),
			outbound.Call{AttemptID: "a", Endpoint: "+79161234567", BoundContext: bound})
		return caller, result
	}
	ask := func(row *model.Integration) (*fakeCaller, error) {
		caller := &fakeCaller{status: "completed"}
		channel := NewChannel(&fakeDirectory{integrations: []*model.Integration{row}}, caller, "https://tokay.example")
		_, err := channel.Poll(context.Background(), outbound.EffectRef{ExternalRef: "CA1", Context: bound})
		return caller, err
	}

	t.Run("switched off: no new call, the old one is still asked about", func(t *testing.T) {
		row := twilioRow("ru-1", "+79990000001", 1, "+7")
		row.Enabled = false
		caller, result := place(row)
		if result.Evidence != outbound.DefinitelyNotSent || len(caller.made) != 0 {
			t.Fatalf("a call through a switched-off integration: %+v, %d made", result, len(caller.made))
		}
		if caller, err := ask(row); err != nil || len(caller.asked) != 1 {
			t.Fatalf("a call made before it was switched off was not asked about: %v", err)
		}
	})

	t.Run("moved to another account: neither placed nor asked", func(t *testing.T) {
		row := twilioRow("ru-2", "+79990000001", 1, "+7")
		row.ID = "ru-1"
		caller, result := place(row)
		if result.Evidence != outbound.DefinitelyNotSent || len(caller.made) != 0 {
			t.Fatalf("a call through another account: %+v, %d made", result, len(caller.made))
		}
		if caller, err := ask(row); err == nil || len(caller.asked) != 0 {
			t.Fatalf("a call was asked about in an account it was not made on: %v", err)
		}
	})

	t.Run("token rotated within the account", func(t *testing.T) {
		row := twilioRow("ru-1", "+79990000001", 1, "+7")
		var cfg model.TwilioConfig
		_ = json.Unmarshal(row.Config, &cfg)
		cfg.AuthToken = "rotated"
		row.Config, _ = json.Marshal(cfg)
		caller, result := place(row)
		if result.Status != "accepted" || caller.madeVia[0].AuthToken != "rotated" {
			t.Fatalf("a rotated token was not used: %+v", result)
		}
	})
}
