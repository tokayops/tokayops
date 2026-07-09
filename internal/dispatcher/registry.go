package dispatcher

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// ErrUnknownProvider means no provider is registered under the requested name.
var ErrUnknownProvider = errors.New("unknown provider")

// ErrProviderNotConfigured means the provider's backing integration is absent or disabled.
var ErrProviderNotConfigured = errors.New("provider not configured")

// ErrMissingProvider means a job step reached an executor with no provider name.
// Every builder sets one post-Sprint-4, so an empty value is a build-invariant
// violation — permanent (see worker.go isPermanentError); retrying cannot help.
var ErrMissingProvider = errors.New("step has no provider")

// ProviderResolver resolves a provider name to a Provider instance. Executors depend
// on this interface so tests can supply a fixed map (staticProviders) while production
// uses the integration-aware ProviderRegistry.
type ProviderResolver interface {
	Provider(name string) (Provider, error)
}

// staticProviders is a ProviderResolver backed by a fixed map — used by tests and
// simple wiring where no per-integration resolution is needed.
type staticProviders map[string]Provider

func (s staticProviders) Provider(name string) (Provider, error) {
	p, ok := s[name]
	if !ok {
		return nil, fmt.Errorf("provider %q: %w", name, ErrUnknownProvider)
	}
	return p, nil
}

// providerFactory builds a Provider bound to a specific integration. The integ arg is
// the Sprint-5 seam: today the Slack factory ignores it (the shared IntegrationCache is
// the token source), but the registry still keys instances by integ.ID so multiple
// integrations of one type become possible without touching call sites.
type providerFactory func(integ *model.Integration) (Provider, error)

type factoryReg struct {
	integType model.IntegrationType
	build     providerFactory
}

// ProviderCapabilities describes what a provider class can do, independent
// of whether an enabled integration exists right now. The policy editor and
// frontend dropdown read capabilities; the dispatcher uses Provider() for
// runtime resolution. Splitting the two means policy validation never returns
// a 400 just because the Slack integration was temporarily disabled.
type ProviderCapabilities struct {
	Name                 string                // provider key — e.g. "slack"
	IntegrationType      model.IntegrationType // backing integration type
	SupportedTargetKinds []string              // sorted; e.g. ["channel","dm"]
}

// ProviderRegistry resolves providers either from explicitly registered instances
// (static, by name) or from a factory that is bound to the integration of a given type.
// Factory-built instances are cached per integration ID.
type ProviderRegistry struct {
	store        store.StoreInterface
	static       map[string]Provider
	factories    map[string]factoryReg
	capabilities map[string]ProviderCapabilities

	mu        sync.Mutex
	instances map[string]Provider // keyed by integration ID
}

func NewProviderRegistry(s store.StoreInterface) *ProviderRegistry {
	return &ProviderRegistry{
		store:        s,
		static:       make(map[string]Provider),
		factories:    make(map[string]factoryReg),
		capabilities: make(map[string]ProviderCapabilities),
		instances:    make(map[string]Provider),
	}
}

// RegisterStatic registers a fixed Provider instance under a name. Takes precedence
// over any factory registered under the same name.
func (r *ProviderRegistry) RegisterStatic(name string, p Provider) {
	r.static[name] = p
}

// RegisterFactory registers a factory that builds a Provider bound to the (single,
// for now) enabled integration of integType.
func (r *ProviderRegistry) RegisterFactory(name string, integType model.IntegrationType, build providerFactory) {
	r.factories[name] = factoryReg{integType: integType, build: build}
}

// RegisterCapabilities records what a provider class can do. Capability
// metadata is declared once at startup and read by API / UI to enumerate
// available step types without touching the DB or trying to construct the
// provider.
//
// Panics on duplicate Name — declaration is a startup-time concern and a
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

// Provider resolves a provider name to an instance. Static registrations win; otherwise
// the factory's integration is resolved by type (must exist and be enabled) and the
// instance is cached by integration ID.
func (r *ProviderRegistry) Provider(name string) (Provider, error) {
	if p, ok := r.static[name]; ok {
		return p, nil
	}

	reg, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("provider %q: %w", name, ErrUnknownProvider)
	}

	integ, err := r.store.GetIntegrationByType(reg.integType)
	if errors.Is(err, store.ErrIntegrationNotFound) {
		return nil, fmt.Errorf("provider %q has no integration: %w", name, ErrProviderNotConfigured)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve integration for provider %q: %w", name, err)
	}
	// Check Enabled here rather than trusting the query filter — not every store
	// implementation filters disabled rows.
	if integ == nil || !integ.Enabled {
		return nil, fmt.Errorf("provider %q has no enabled integration: %w", name, ErrProviderNotConfigured)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.instances[integ.ID]; ok {
		return p, nil
	}
	p, err := reg.build(integ)
	if err != nil {
		return nil, fmt.Errorf("build provider %q for integration %s: %w", name, integ.ID, err)
	}
	r.instances[integ.ID] = p
	return p, nil
}
