package integrations

import (
	"encoding/json"
	"errors"

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
			return nil
		},
	})
}
