package integrations

import (
	"encoding/json"
	"errors"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers/webhook"
)

// Lifted verbatim from the old generic_webhook branch. The internal/config
// import (for ValidateWebhookURL) is why descriptors live here, not in
// internal/model - model is the leaf, config sits alongside it.
func init() {
	Register(Descriptor{
		Type:         model.IntegrationTypeGenericWebhook,
		Direction:    model.IntegrationDirectionOutbound,
		SecretFields: []string{"secret"},
		ValidateConfig: func(cfg json.RawMessage, isUpdate bool) error {
			var c model.GenericWebhookConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return errors.New("invalid generic webhook config: " + err.Error())
			}
			if !isUpdate {
				if c.URL == "" {
					return errors.New("webhook url is required")
				}
			}
			if c.Secret == model.MaskedSecret {
				return errors.New("cannot use masked value as secret")
			}
			// Thirty, not sixty: the delivery attempt has a deadline that is a
			// sum of this ceiling and the reads around it, and a longer value
			// would be quietly cut off by the attempt rather than honoured.
			// Values saved under the old limit are clamped at every read
			// before the call (webhook.EffectiveTimeout), so they keep
			// delivering.
			if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 30 {
				return errors.New("timeout_seconds must be between 0 and 30")
			}
			// Header names this system owns are not the subscriber's to set:
			// a configuration able to replace X-Tokay-Event-ID would turn
			// every retry into a new event for the receiver, and the signature
			// into whatever the configuration says. Case-insensitive, because
			// the transport canonicalises names.
			for name := range c.CustomHeaders {
				if webhook.IsReservedHeader(name) {
					return errors.New("custom header " + name + " is reserved for TokayOps")
				}
			}
			if c.URL != "" {
				if err := config.ValidateWebhookURL(c.URL); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
