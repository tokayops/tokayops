package integrations

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
)

// Lifted verbatim from the old alertmanager_webhook branch.
func init() {
	Register(Descriptor{
		Type:         model.IntegrationTypeAlertmanagerWebhook,
		Direction:    model.IntegrationDirectionInbound,
		SecretFields: []string{"secret"},
		ValidateConfig: func(cfg json.RawMessage, isUpdate bool) error {
			var c model.WebhookConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return errors.New("invalid webhook config: " + err.Error())
			}
			if !isUpdate {
				if c.Secret == "" {
					return errors.New("webhook secret is required")
				}
				if c.Secret == model.MaskedSecret {
					return errors.New("cannot use masked value as secret")
				}
			}
			// Zero is how the field is cleared, and how it arrives from a
			// build that never had it: nothing is claimed about silence.
			if c.QuietAfterSeconds != 0 {
				if c.QuietAfterSeconds < model.QuietAfterMin || c.QuietAfterSeconds > model.QuietAfterMax {
					return fmt.Errorf("quiet_after_seconds must be between %d and %d, or 0 to leave it unset",
						model.QuietAfterMin, model.QuietAfterMax)
				}
				// Whole minutes: the form takes minutes, so a value that is
				// not one would change every time somebody opened it.
				if c.QuietAfterSeconds%60 != 0 {
					return errors.New("quiet_after_seconds must be a whole number of minutes")
				}
			}
			return nil
		},
	})
}
