package integrations

import (
	"encoding/json"

	"github.com/tokayops/tokayops/internal/model"
)

// MaskSecrets returns a copy of the integration with its secret config fields
// replaced by model.MaskedSecret. Which top-level config keys are secret is
// declared per type via Descriptor.SecretFields, so adding a new integration
// type needs only a descriptor - no edit here. A channel is a plugin.
//
// The original integration (and its Config bytes) is never mutated; non-secret
// and unknown fields are preserved verbatim.
func MaskSecrets(i *model.Integration) *model.Integration {
	masked := *i

	d, ok := Get(i.Type)
	if !ok || len(d.SecretFields) == 0 {
		return &masked
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(i.Config, &fields); err != nil {
		// Config isn't a JSON object we can introspect - return the copy
		// unchanged rather than risk leaking a secret we can't locate.
		return &masked
	}

	maskedValue, _ := json.Marshal(model.MaskedSecret)
	changed := false
	for _, key := range d.SecretFields {
		raw, present := fields[key]
		if !present {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			continue // not a non-empty string secret; leave as-is
		}
		fields[key] = maskedValue
		changed = true
	}
	if !changed {
		return &masked
	}

	if b, err := json.Marshal(fields); err == nil {
		masked.Config = b
	}
	return &masked
}
