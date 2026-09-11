package api

import "github.com/tokayops/tokayops/internal/outbound/providers"

// NewProviderCapsAdapter wraps the channel catalogue so it
// satisfies ProviderCapabilitiesLookup at the API boundary. The two types
// (providers.Capability vs api.ProviderCapability) are field-
// identical but kept separate so the API contract isn't pinned to the
// catalogue's own type.
func NewProviderCapsAdapter(r *providers.Catalog) ProviderCapabilitiesLookup {
	return providerCapsAdapter{r: r}
}

type providerCapsAdapter struct {
	r *providers.Catalog
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
