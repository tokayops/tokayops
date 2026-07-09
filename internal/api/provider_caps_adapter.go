package api

import "github.com/tokayops/tokayops/internal/dispatcher"

// NewProviderCapsAdapter wraps the dispatcher's capability registry so it
// satisfies ProviderCapabilitiesLookup at the API boundary. The two types
// (dispatcher.ProviderCapabilities vs api.ProviderCapability) are field-
// identical but kept separate so the API contract isn't pinned to the
// dispatcher's internal type.
func NewProviderCapsAdapter(r *dispatcher.ProviderRegistry) ProviderCapabilitiesLookup {
	return providerCapsAdapter{r: r}
}

type providerCapsAdapter struct {
	r *dispatcher.ProviderRegistry
}

func (a providerCapsAdapter) Capabilities(name string) (ProviderCapability, bool) {
	c, ok := a.r.Capabilities(name)
	if !ok {
		return ProviderCapability{}, false
	}
	return ProviderCapability{
		Name:                 c.Name,
		IntegrationType:      c.IntegrationType,
		SupportedTargetKinds: c.SupportedTargetKinds,
	}, true
}

func (a providerCapsAdapter) AllCapabilities() []ProviderCapability {
	src := a.r.AllCapabilities()
	out := make([]ProviderCapability, len(src))
	for i, c := range src {
		out[i] = ProviderCapability{
			Name:                 c.Name,
			IntegrationType:      c.IntegrationType,
			SupportedTargetKinds: c.SupportedTargetKinds,
		}
	}
	return out
}
