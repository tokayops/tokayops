package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/providers/twilio"
	"github.com/tokayops/tokayops/internal/store"
)

type fakeCallEvents struct {
	bindings map[string]outbound.BoundContext
	recorded []outbound.ProviderEvent
	applied  []string
}

func (f *fakeCallEvents) AttemptBinding(_ context.Context, id string) (outbound.BoundContext, bool, error) {
	b, ok := f.bindings[id]
	return b, ok, nil
}
func (f *fakeCallEvents) RecordProviderEvent(_ context.Context, e outbound.ProviderEvent) (string, error) {
	f.recorded = append(f.recorded, e)
	return "row-1", nil
}
func (f *fakeCallEvents) ApplyProviderEvent(_ context.Context, id string,
	_ map[string]outbound.EffectTranslator) (outbound.ApplyResult, error) {
	f.applied = append(f.applied, id)
	return outbound.ApplyResult{}, nil
}

const callbackToken = "the-account-token"

func callbackHarness(t *testing.T, selfURL string) (*echo.Echo, *fakeCallEvents) {
	t.Helper()
	s := store.NewMockStore()
	cfg, _ := json.Marshal(model.TwilioConfig{
		AccountSID: "AC" + strings.Repeat("0", 32), AuthToken: callbackToken, FromNumber: "+15005550006",
		Coverage: []string{"+"}, CPS: 1, MaxConcurrent: 1,
	})
	if err := s.CreateIntegration(&model.Integration{ID: "tw-1", Type: model.IntegrationTypeTwilio,
		Direction: model.IntegrationDirectionOutbound, Name: "tw", Enabled: true, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	events := &fakeCallEvents{bindings: map[string]outbound.BoundContext{
		"att-1": {IntegrationID: "tw-1", AccountScope: "AC" + strings.Repeat("0", 32)},
	}}
	a := NewAPI(s, nil, nil, nil, selfURL, nil)
	a.SetCallEvents(events, map[string]outbound.EffectTranslator{})
	e := echo.New()
	a.RegisterRoutes(e)
	return e, events
}

func callbackForm() url.Values {
	return url.Values{
		"AccountSid":      {"AC" + strings.Repeat("0", 32)},
		"CallSid":         {"CA1"},
		"CallStatus":      {"completed"},
		"SequenceNumber":  {"3"},
		"SipResponseCode": {"200"},
		"Timestamp":       {"Mon, 16 Aug 2010 03:45:01 +0000"},
	}
}

func postCallback(e *echo.Echo, query string, form url.Values, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/twilio/voice/status?"+query, strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	req.Header.Set("X-Twilio-Signature", signature)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// A callback signed by the account the call was placed on is kept and applied;
// what it says is read as the event of that attempt.
func TestASignedCallbackIsKept(t *testing.T) {
	e, events := callbackHarness(t, "https://tokay.example")
	form := callbackForm()
	signature := twilio.Signature(callbackToken, "https://tokay.example/twilio/voice/status?a=att-1", form)

	rec := postCallback(e, "a=att-1", form, signature)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(events.recorded) != 1 || len(events.applied) != 1 {
		t.Fatalf("recorded %d, applied %d", len(events.recorded), len(events.applied))
	}
	got := events.recorded[0]
	if got.AttemptID != "att-1" || got.ExternalRef != "CA1" || got.EventID != "CA1:3" ||
		got.ProviderStatus != "completed" || got.Sequence == nil || *got.Sequence != 3 || got.OccurredAt.IsZero() {
		t.Fatalf("event = %+v", got)
	}
}

// What cannot be checked is not kept: a wrong signature, an attempt nothing
// here made, a signature over the URL as the request arrived rather than as
// Twilio called it.
func TestAnUncheckedCallbackIsRefused(t *testing.T) {
	form := callbackForm()
	for _, tc := range []struct {
		name, query, signedURL, token string
	}{
		{"wrong token", "a=att-1", "https://tokay.example/twilio/voice/status?a=att-1", "another-token"},
		{"unknown attempt", "a=att-9", "https://tokay.example/twilio/voice/status?a=att-9", callbackToken},
		{"the URL the proxy passed on", "a=att-1", "http://10.0.0.5:8080/twilio/voice/status?a=att-1", callbackToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, events := callbackHarness(t, "https://tokay.example")
			rec := postCallback(e, tc.query, form, twilio.Signature(tc.token, tc.signedURL, form))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d", rec.Code)
			}
			if len(events.recorded) != 0 {
				t.Fatal("an unchecked callback was kept")
			}
		})
	}
}

// Twilio drops the port of an HTTPS URL before it signs.
func TestTheSignedURLHasNoHTTPSPort(t *testing.T) {
	e, events := callbackHarness(t, "https://tokay.example:8443/")
	form := callbackForm()
	signature := twilio.Signature(callbackToken, "https://tokay.example/twilio/voice/status?a=att-1", form)
	if rec := postCallback(e, "a=att-1", form, signature); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(events.recorded) != 1 {
		t.Fatal("the callback was not kept")
	}
	if got := signedCallbackURL("http://tokay.example:8080", "/p", "a=1"); got != "http://tokay.example:8080/p?a=1" {
		t.Fatalf("an HTTP port was dropped: %s", got)
	}
}
