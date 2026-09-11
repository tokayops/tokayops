package providers

import (
	"fmt"
	"sort"

	"github.com/tokayops/tokayops/internal/model"
)

// What the channels of this build can do, as a catalogue.
//
// It lives beside the channels rather than in whatever wires them up, and it
// answers one question: what COULD this provider do. Resolving a name to a
// working provider used to live with it, for the calls a job step made, and
// went with the steps - and with it went the store it needed to ask whether an
// integration was enabled.
//
// That separation is the point rather than a leftover. A policy editor offering
// a Slack step, and an announcement deciding whether to promise one, must get
// the same answer whether or not somebody switched Slack off ten minutes ago:
// the first would otherwise be a product that changes shape when a token is
// rotated, and the second would leave a shift change unannounced for the
// duration of an outage that has nothing to do with it.

// Capability describes what one provider class can do.
type Capability struct {
	Name                 string                // provider key - e.g. "slack"
	IntegrationType      model.IntegrationType // backing integration type
	SupportedTargetKinds []string              // sorted; e.g. ["channel","dm"]
}

// Carries reports whether this provider delivers to the given kind of target.
//
// Asked of ONE provider by name, and there is no "which providers carry a
// direct message" beside it on purpose: from a list of the qualifying ones,
// every absent provider looks the same, and a channel that is registered and
// does not carry direct messages is a setting somebody can change while one
// this build has never heard of is a channel that was taken away.
func (c Capability) Carries(targetKind string) bool {
	for _, kind := range c.SupportedTargetKinds {
		if kind == targetKind {
			return true
		}
	}
	return false
}

// Catalog is every capability this build declares, by provider name.
type Catalog struct {
	entries map[string]Capability
}

func NewCatalog() *Catalog {
	return &Catalog{entries: make(map[string]Capability)}
}

// Register records what a provider class can do. Declared once at start-up and
// read afterwards, so nothing here is guarded for concurrent writers.
//
// Panics on a duplicate name: declaration is a start-up concern, and two
// declarations of one provider is a programming error rather than a runtime
// condition.
func (c *Catalog) Register(entry Capability) {
	if entry.Name == "" {
		panic("providers: a capability with no provider name")
	}
	if _, exists := c.entries[entry.Name]; exists {
		panic(fmt.Sprintf("providers: duplicate capabilities for provider %s", entry.Name))
	}
	// Stable order in API responses.
	sorted := append([]string(nil), entry.SupportedTargetKinds...)
	sort.Strings(sorted)
	entry.SupportedTargetKinds = sorted
	c.entries[entry.Name] = entry
}

// Capabilities looks a provider up by name. The bool is the difference between
// "this build has no such channel" and "it has one that carries nothing you
// asked about", and the two are different things to whoever has to fix them.
func (c *Catalog) Capabilities(name string) (Capability, bool) {
	entry, known := c.entries[name]
	return entry, known
}

// AllCapabilities is every declared provider, sorted by name.
func (c *Catalog) AllCapabilities() []Capability {
	out := make([]Capability, 0, len(c.entries))
	for _, entry := range c.entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
