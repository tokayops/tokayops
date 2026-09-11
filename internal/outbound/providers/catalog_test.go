package providers

import (
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// The catalogue answers two questions and both of them decide something. A
// policy editor offers the steps it lists; an announcement promises a message
// only to a provider it says carries one. Neither has any other source.

func TestACatalogueAnswersAboutOneProviderAtATime(t *testing.T) {
	catalog := NewCatalog()
	catalog.Register(Capability{
		Name:                 "slack",
		IntegrationType:      model.IntegrationTypeSlack,
		SupportedTargetKinds: []string{"dm", "channel"},
	})

	entry, known := catalog.Capabilities("slack")
	if !known {
		t.Fatal("a registered provider is not in the catalogue")
	}
	if entry.IntegrationType != model.IntegrationTypeSlack {
		t.Errorf("the entry names integration type %q", entry.IntegrationType)
	}
	if !entry.Carries("dm") || !entry.Carries("channel") {
		t.Errorf("the entry carries %v, want both kinds", entry.SupportedTargetKinds)
	}
	if entry.Carries("sms") {
		t.Error("the entry claims to carry a kind nobody declared")
	}

	// The bool is the whole point of the signature: a provider this build has
	// never heard of and one that is here and carries nothing you asked about
	// are different things to whoever has to fix them.
	if _, known := catalog.Capabilities("hipchat"); known {
		t.Error("a provider nobody declared is in the catalogue")
	}
}

// TestTheKindsAreSortedOnTheWayIn: the list reaches an API response and a
// policy editor's dropdown, and a set that came back in a different order on
// every start would be a diff in somebody's snapshot test and a moving list in
// front of a person.
func TestTheKindsAreSortedOnTheWayIn(t *testing.T) {
	catalog := NewCatalog()
	catalog.Register(Capability{Name: "slack", SupportedTargetKinds: []string{"dm", "channel"}})

	entry, _ := catalog.Capabilities("slack")
	if entry.SupportedTargetKinds[0] != "channel" || entry.SupportedTargetKinds[1] != "dm" {
		t.Errorf("kinds = %v, want them sorted", entry.SupportedTargetKinds)
	}

	// And the caller's slice is not the catalogue's: a registrar that kept
	// writing to what it passed would be editing the catalogue afterwards, and
	// the sorting above would reorder the caller's own slice under it.
	kinds := []string{"dm", "channel"}
	catalog.Register(Capability{Name: "telegram", SupportedTargetKinds: kinds})
	for i := range kinds {
		kinds[i] = "carrier_pigeon"
	}
	got, _ := catalog.Capabilities("telegram")
	for _, kind := range got.SupportedTargetKinds {
		if kind == "carrier_pigeon" {
			t.Fatalf("the catalogue shares the slice it was given: %v", got.SupportedTargetKinds)
		}
	}
}

// TestEveryProviderInOneList, sorted by name: it is what GET /providers
// answers, and the order is the response's.
func TestEveryProviderInOneList(t *testing.T) {
	catalog := NewCatalog()
	catalog.Register(Capability{Name: "telegram", SupportedTargetKinds: []string{"dm"}})
	catalog.Register(Capability{Name: "slack", SupportedTargetKinds: []string{"dm", "channel"}})
	catalog.Register(Capability{Name: "pagerduty", SupportedTargetKinds: []string{"channel"}})

	all := catalog.AllCapabilities()
	if len(all) != 3 {
		t.Fatalf("the catalogue lists %d providers, want 3", len(all))
	}
	if all[0].Name != "pagerduty" || all[1].Name != "slack" || all[2].Name != "telegram" {
		t.Errorf("listed %+v, want them sorted by name", all)
	}
}

// TestADeclarationThatMakesNoSenseStopsTheStart.
//
// Both cases are programming errors and both are made once, at start-up, before
// anything is serving. A provider with no name is unreachable by every lookup
// there is; a second declaration of one provider would leave which of the two
// answers depending on map order. A process that started anyway would be
// serving a catalogue nobody wrote.
func TestADeclarationThatMakesNoSenseStopsTheStart(t *testing.T) {
	t.Run("a capability with no provider", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("a capability with no name was registered")
			}
		}()
		NewCatalog().Register(Capability{SupportedTargetKinds: []string{"dm"}})
	})

	t.Run("one provider declared twice", func(t *testing.T) {
		catalog := NewCatalog()
		catalog.Register(Capability{Name: "slack", SupportedTargetKinds: []string{"dm"}})
		defer func() {
			if recover() == nil {
				t.Fatal("a provider was declared twice")
			}
		}()
		catalog.Register(Capability{Name: "slack", SupportedTargetKinds: []string{"channel"}})
	})
}
