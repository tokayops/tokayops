package dispatcher

import (
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// TestRegistry_CapabilitiesAreIndependentOfAnyIntegration. What a channel can
// do is a property of this build, and it is answered whether or not an
// integration for it exists or is switched on - a policy editor that hid the
// Slack step while Slack was disabled for ten minutes would be offering a
// different product every time somebody touched a token.
func TestRegistry_CapabilitiesAreIndependentOfAnyIntegration(t *testing.T) {
	r := NewProviderRegistry()
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
}

func TestRegistry_AllCapabilities_SortedAndProvidersSupporting(t *testing.T) {
	r := NewProviderRegistry()
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
	r := NewProviderRegistry()
	r.RegisterCapabilities(ProviderCapabilities{Name: "slack"})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate capability registration")
		}
	}()
	r.RegisterCapabilities(ProviderCapabilities{Name: "slack"})
}
