package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// fixedCaps is a tiny ProviderCapabilitiesLookup for handler tests so we
// don't have to spin up a full dispatcher.
type fixedCaps []ProviderCapability

func (f fixedCaps) Capabilities(name string) (ProviderCapability, bool) {
	for _, c := range f {
		if c.Name == name {
			return c, true
		}
	}
	return ProviderCapability{}, false
}
func (f fixedCaps) AllCapabilities() []ProviderCapability { return []ProviderCapability(f) }

// The /providers endpoint sits behind AuthMiddleware (it's a logged-in
// policy-editor concern, not a public capability dump). The handler itself
// has no auth-dependent logic, so we exercise it directly via Echo's context
// rather than wire up a full login flow.
func TestListProviders_ReturnsCapabilities(t *testing.T) {
	caps := fixedCaps{
		{Name: "slack", IntegrationType: model.IntegrationTypeSlack, SupportedTargetKinds: []string{"channel", "dm"}},
	}
	a := NewAPI(store.NewMockStore(), nil, nil, nil, "", caps)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := a.ListProviders(c); err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var resp ProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Name != "slack" {
		t.Fatalf("unexpected providers: %+v", resp.Providers)
	}
}

func TestListProviders_IncludesTelegram(t *testing.T) {
	caps := fixedCaps{
		{Name: "slack", IntegrationType: model.IntegrationTypeSlack, SupportedTargetKinds: []string{"channel", "dm"}},
		{Name: "telegram", IntegrationType: model.IntegrationTypeTelegram, SupportedTargetKinds: []string{"channel", "dm"}},
	}
	a := NewAPI(store.NewMockStore(), nil, nil, nil, "", caps)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := a.ListProviders(c); err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	var resp ProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var tg *ProviderCapability
	for i := range resp.Providers {
		if resp.Providers[i].Name == "telegram" {
			tg = &resp.Providers[i]
		}
	}
	if tg == nil {
		t.Fatalf("telegram not in providers: %+v", resp.Providers)
	}
	kinds := map[string]bool{}
	for _, k := range tg.SupportedTargetKinds {
		kinds[k] = true
	}
	if !kinds["dm"] || !kinds["channel"] {
		t.Errorf("telegram should support dm+channel, got %v", tg.SupportedTargetKinds)
	}
}

func TestListProviders_NilLookupReturnsEmpty(t *testing.T) {
	a := NewAPI(store.NewMockStore(), nil, nil, nil, "", nil)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := a.ListProviders(c); err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp ProvidersResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Providers) != 0 {
		t.Fatalf("expected empty list with nil caps, got %+v", resp.Providers)
	}
}

func TestValidatePolicyStep_CapabilityGate(t *testing.T) {
	caps := fixedCaps{
		{Name: "slack", IntegrationType: model.IntegrationTypeSlack, SupportedTargetKinds: []string{"channel", "dm"}},
		{Name: "telegram", IntegrationType: model.IntegrationTypeTelegram, SupportedTargetKinds: []string{"channel", "dm"}},
	}

	cases := []struct {
		name    string
		step    PolicyStepRequest
		wantErr bool
	}{
		{
			name:    "slack dm to user → ok",
			step:    PolicyStepRequest{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "U1"},
			wantErr: false,
		},
		{
			name:    "slack channel to channel → ok",
			step:    PolicyStepRequest{Provider: "slack", TargetKind: "channel", TargetType: "channel", TargetID: "C1"},
			wantErr: false,
		},
		{
			name:    "telegram dm to user → ok",
			step:    PolicyStepRequest{Provider: "telegram", TargetKind: "dm", TargetType: "user", TargetID: "U1"},
			wantErr: false,
		},
		{
			name:    "telegram channel to channel → ok",
			step:    PolicyStepRequest{Provider: "telegram", TargetKind: "channel", TargetType: "channel", TargetID: "C1"},
			wantErr: false,
		},
		{
			name:    "telegram channel to user → 400 (taxonomy incompat)",
			step:    PolicyStepRequest{Provider: "telegram", TargetKind: "channel", TargetType: "user", TargetID: "U1"},
			wantErr: true,
		},
		{
			name:    "unknown provider → 400",
			step:    PolicyStepRequest{Provider: "pagerduty", TargetKind: "dm", TargetType: "user", TargetID: "U1"},
			wantErr: true,
		},
		{
			name:    "kind not supported by provider → 400",
			step:    PolicyStepRequest{Provider: "slack", TargetKind: "sms", TargetType: "user", TargetID: "U1"},
			wantErr: true,
		},
		{
			name:    "taxonomy incompat: dm + channel target → 400",
			step:    PolicyStepRequest{Provider: "slack", TargetKind: "dm", TargetType: "channel", TargetID: "C1"},
			wantErr: true,
		},
		{
			name:    "empty provider → 400",
			step:    PolicyStepRequest{TargetKind: "dm", TargetType: "user", TargetID: "U1"},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validatePolicyStep(c.step, caps)
			if (err != nil) != c.wantErr {
				t.Fatalf("validatePolicyStep err=%v wantErr=%v", err, c.wantErr)
			}
		})
	}
}
