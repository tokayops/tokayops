package dispatcher

import (
	"errors"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestRegistry_StaticTakesPrecedence(t *testing.T) {
	r := NewProviderRegistry(store.NewMockStore())
	mp := &MockProvider{}
	r.RegisterStatic("slack", mp)

	got, err := r.Provider("slack")
	if err != nil {
		t.Fatalf("Provider: %v", err)
	}
	if got != mp {
		t.Fatal("expected the statically registered instance")
	}
}

func TestRegistry_UnknownProvider(t *testing.T) {
	r := NewProviderRegistry(store.NewMockStore())
	if _, err := r.Provider("nope"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got %v", err)
	}
}

func TestRegistry_FactoryResolvesIntegrationAndCachesByID(t *testing.T) {
	s := store.NewMockStore()
	integ := &model.Integration{
		Type:    model.IntegrationTypeSlack,
		Name:    "slack",
		Enabled: true,
		Config:  []byte(`{"token":"xoxb-1"}`),
	}
	if err := s.CreateIntegration(integ); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	r := NewProviderRegistry(s)
	mp := &MockProvider{}
	calls := 0
	r.RegisterFactory("slack", model.IntegrationTypeSlack, func(i *model.Integration) (Provider, error) {
		calls++
		if i.ID != integ.ID {
			t.Errorf("factory got integration %q, want %q", i.ID, integ.ID)
		}
		return mp, nil
	})

	for i := 0; i < 3; i++ {
		got, err := r.Provider("slack")
		if err != nil {
			t.Fatalf("Provider: %v", err)
		}
		if got != mp {
			t.Fatal("expected the factory-built instance")
		}
	}
	if calls != 1 {
		t.Fatalf("factory should be built once and cached by integration ID, got %d calls", calls)
	}
}

func TestRegistry_MissingIntegration_IsPermanentNotConfigured(t *testing.T) {
	r := NewProviderRegistry(store.NewMockStore())
	r.RegisterFactory("slack", model.IntegrationTypeSlack, func(i *model.Integration) (Provider, error) {
		return &MockProvider{}, nil
	})

	_, err := r.Provider("slack")
	if !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured, got %v", err)
	}
	if !isPermanentError(err) {
		t.Fatal("ErrProviderNotConfigured must be a permanent error")
	}
}

func TestRegistry_DisabledIntegration_NotConfigured(t *testing.T) {
	s := store.NewMockStore()
	integ := &model.Integration{
		Type:    model.IntegrationTypeSlack,
		Name:    "slack",
		Enabled: false,
		Config:  []byte(`{"token":"xoxb-1"}`),
	}
	if err := s.CreateIntegration(integ); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	r := NewProviderRegistry(s)
	r.RegisterFactory("slack", model.IntegrationTypeSlack, func(i *model.Integration) (Provider, error) {
		return &MockProvider{}, nil
	})

	if _, err := r.Provider("slack"); !errors.Is(err, ErrProviderNotConfigured) {
		t.Fatalf("expected ErrProviderNotConfigured for a disabled integration, got %v", err)
	}
}

// Capability lookup is independent of runtime resolution. A
// provider registered only via RegisterCapabilities (no factory, no static)
// is discoverable for the policy editor even when no integration exists.
func TestRegistry_Capabilities_DoNotRequireRuntimeResolution(t *testing.T) {
	r := NewProviderRegistry(store.NewMockStore())
	r.RegisterCapabilities(ProviderCapabilities{
		Name:                 "slack",
		IntegrationType:      model.IntegrationTypeSlack,
		SupportedTargetKinds: []string{"channel", "dm"},
	})

	cap, ok := r.Capabilities("slack")
	if !ok {
		t.Fatal("expected slack capability")
	}
	if cap.Name != "slack" || cap.IntegrationType != model.IntegrationTypeSlack {
		t.Errorf("unexpected capability: %+v", cap)
	}
	// SupportedTargetKinds must be sorted on registration.
	want := []string{"channel", "dm"}
	for i, k := range want {
		if cap.SupportedTargetKinds[i] != k {
			t.Errorf("SupportedTargetKinds[%d]=%q, want %q", i, cap.SupportedTargetKinds[i], k)
		}
	}

	// Runtime resolution still fails - no factory and no static instance.
	if _, err := r.Provider("slack"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("Provider(slack) without factory: want ErrUnknownProvider, got %v", err)
	}
}

func TestRegistry_AllCapabilities_SortedAndProvidersSupporting(t *testing.T) {
	r := NewProviderRegistry(store.NewMockStore())
	r.RegisterCapabilities(ProviderCapabilities{Name: "telegram", SupportedTargetKinds: []string{"dm"}})
	r.RegisterCapabilities(ProviderCapabilities{Name: "slack", SupportedTargetKinds: []string{"dm", "channel"}})
	r.RegisterCapabilities(ProviderCapabilities{Name: "pagerduty", SupportedTargetKinds: []string{"channel"}})

	all := r.AllCapabilities()
	if len(all) != 3 || all[0].Name != "pagerduty" || all[1].Name != "slack" || all[2].Name != "telegram" {
		t.Fatalf("AllCapabilities must be sorted by Name; got %+v", all)
	}

	dm := r.ProvidersSupporting("dm")
	if len(dm) != 2 || dm[0] != "slack" || dm[1] != "telegram" {
		t.Errorf("ProvidersSupporting(dm)=%v, want [slack telegram]", dm)
	}
	if got := r.ProvidersSupporting("sms"); len(got) != 0 {
		t.Errorf("ProvidersSupporting(sms) must be empty, got %v", got)
	}
}

func TestRegistry_DuplicateCapabilitiesPanic(t *testing.T) {
	r := NewProviderRegistry(store.NewMockStore())
	r.RegisterCapabilities(ProviderCapabilities{Name: "slack"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate capability registration")
		}
	}()
	r.RegisterCapabilities(ProviderCapabilities{Name: "slack"})
}
