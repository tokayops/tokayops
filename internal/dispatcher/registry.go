package dispatcher

import (
	"fmt"
	"sort"

	"github.com/tokayops/tokayops/internal/model"
)

// ProviderCapabilities describes what a provider class can do, independent of
// whether an enabled integration exists right now.
//
// It is a catalogue and nothing else. Resolving a name to a working provider
// used to live here too, for the executors that made calls from a job step;
// those are gone, and with them the store this registry held and the whole
// question of whether an integration happens to be enabled. What is left
// answers "what could this channel do", which is what a policy editor needs to
// offer a step and what an announcement needs to know before it promises one -
// and neither of them should get a different answer because Slack was switched
// off for ten minutes.
type ProviderCapabilities struct {
	Name                 string                // provider key - e.g. "slack"
	IntegrationType      model.IntegrationType // backing integration type
	SupportedTargetKinds []string              // sorted; e.g. ["channel","dm"]
}

// ProviderRegistry is the catalogue of what the channels of this build can do.
type ProviderRegistry struct {
	capabilities map[string]ProviderCapabilities
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{capabilities: make(map[string]ProviderCapabilities)}
}

// RegisterCapabilities records what a provider class can do. Capability
// metadata is declared once at startup and read by API / UI to enumerate
// available step types without touching the DB or trying to construct the
// provider.
//
// Panics on duplicate Name - declaration is a startup-time concern and a
// duplicate is a programming error, not a runtime condition.
func (r *ProviderRegistry) RegisterCapabilities(c ProviderCapabilities) {
	if c.Name == "" {
		panic("RegisterCapabilities: empty Name")
	}
	if _, exists := r.capabilities[c.Name]; exists {
		panic(fmt.Sprintf("RegisterCapabilities: duplicate capabilities for provider %s", c.Name))
	}
	// Stable order in API responses.
	sorted := append([]string(nil), c.SupportedTargetKinds...)
	sort.Strings(sorted)
	c.SupportedTargetKinds = sorted
	r.capabilities[c.Name] = c
}

// Capabilities looks up capability metadata for a provider name. The bool
// distinguishes "unknown provider" from "registered but nothing here yet".
func (r *ProviderRegistry) Capabilities(name string) (ProviderCapabilities, bool) {
	c, ok := r.capabilities[name]
	return c, ok
}

// AllCapabilities returns every registered capability descriptor, sorted by
// provider name. Used by GET /providers and by handoff_notifier to enumerate
// dm-capable providers.
func (r *ProviderRegistry) AllCapabilities() []ProviderCapabilities {
	out := make([]ProviderCapabilities, 0, len(r.capabilities))
	for _, c := range r.capabilities {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ProvidersSupporting returns the names of providers whose capabilities
// advertise targetKind, sorted for stability.
func (r *ProviderRegistry) ProvidersSupporting(targetKind string) []string {
	out := make([]string, 0, len(r.capabilities))
	for name, c := range r.capabilities {
		for _, k := range c.SupportedTargetKinds {
			if k == targetKind {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
