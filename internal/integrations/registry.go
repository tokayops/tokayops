// Package integrations is the data-driven type registry that replaces the
// hardcoded switches in internal/api and internal/model. Each supported
// integration type declares itself here (Direction + ValidateConfig); callers
// look the descriptor up by type instead of branching on string constants.
//
// This package lives outside internal/model so descriptors can import
// internal/config (URL validation for generic_webhook) without creating an
// import cycle on the leaf model package.
package integrations

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/tokayops/tokayops/internal/model"
)

// Descriptor is what every integration type registers about itself.
//
// ValidateConfig is the per-type body that used to live in
// internal/api/integration_handlers.go's switch on IntegrationType. The
// isUpdate flag preserves the create-vs-update semantics (allows masked or
// empty secrets on update, requires real secrets on create).
type Descriptor struct {
	Type           model.IntegrationType
	Direction      model.IntegrationDirection
	ValidateConfig func(cfg json.RawMessage, isUpdate bool) error

	// SecretFields are the top-level config JSON keys whose string values are
	// secrets and must be masked in API responses. Declaring them here (instead
	// of a per-type switch in model.Integration) keeps "add a channel = add a
	// descriptor" true. See MaskSecrets.
	SecretFields []string
}

var (
	mu       sync.RWMutex
	registry = map[model.IntegrationType]Descriptor{}
)

// Register adds a descriptor. Panics on duplicate Type - descriptors are
// declared at package init() time, and a duplicate registration is a
// programming error, not a runtime condition.
func Register(d Descriptor) {
	mu.Lock()
	defer mu.Unlock()
	if d.Type == "" {
		panic("integrations.Register: empty Type")
	}
	if d.ValidateConfig == nil {
		panic(fmt.Sprintf("integrations.Register(%s): nil ValidateConfig", d.Type))
	}
	if _, exists := registry[d.Type]; exists {
		panic(fmt.Sprintf("integrations.Register: duplicate descriptor for type %s", d.Type))
	}
	registry[d.Type] = d
}

// Get looks the descriptor for t up.
func Get(t model.IntegrationType) (Descriptor, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := registry[t]
	return d, ok
}

// ValidTypes returns every registered type, sorted by string for stable
// API / UI / test output.
func ValidTypes() []model.IntegrationType {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]model.IntegrationType, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// IsValidType returns true iff t has a registered descriptor.
func IsValidType(t model.IntegrationType) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := registry[t]
	return ok
}

// DirectionFor returns the registered Direction for t. Unknown types return
// (_, false) - the legacy GetDirectionForType silently defaulted to outbound,
// which masked typos; callers now have to handle the missing case explicitly.
func DirectionFor(t model.IntegrationType) (model.IntegrationDirection, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := registry[t]
	if !ok {
		return "", false
	}
	return d.Direction, true
}

// ValidateConfig is a convenience over Get + Descriptor.ValidateConfig used by
// the integration_handlers create/update flow.
func ValidateConfig(t model.IntegrationType, cfg json.RawMessage, isUpdate bool) error {
	d, ok := Get(t)
	if !ok {
		return fmt.Errorf("unknown integration type: %s", t)
	}
	return d.ValidateConfig(cfg, isUpdate)
}

// resetForTests clears the registry between tests so init() side-effects from
// other tests don't leak. Used only from registry_test.go.
func resetForTests() {
	mu.Lock()
	defer mu.Unlock()
	registry = map[model.IntegrationType]Descriptor{}
}
