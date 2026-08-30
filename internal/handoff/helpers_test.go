package handoff

import "github.com/tokayops/tokayops/internal/outbound/providers"

// staticCapabilities is the capability registry as these tests need it: a
// provider name maps to the target kinds it carries here.
//
// A name that is absent is not registered at all, which is a different answer
// from a name registered with no "dm" in its kinds - and telling those two
// apart is the whole reason the builder asks per name.
type staticCapabilities map[string][]string

func (s staticCapabilities) Capabilities(name string) (providers.Capability, bool) {
	kinds, known := s[name]
	if !known {
		return providers.Capability{}, false
	}
	return providers.Capability{Name: name, SupportedTargetKinds: kinds}, true
}

// staticDmProviders is the common case: every named provider is registered and
// carries a direct message.
func staticDmProviders(names ...string) staticCapabilities {
	out := make(staticCapabilities, len(names))
	for _, name := range names {
		out[name] = []string{"dm"}
	}
	return out
}
