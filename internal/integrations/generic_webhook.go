package integrations

import (
	"encoding/json"
	"errors"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
)

// Lifted verbatim from the old generic_webhook branch. The internal/config
// import (for ValidateWebhookURL) is why descriptors live here, not in
// internal/model — model is the leaf, config sits alongside it.
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
			if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 60 {
				return errors.New("timeout_seconds must be between 0 and 60")
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
