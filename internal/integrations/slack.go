package integrations

import (
	"encoding/json"
	"errors"

	"github.com/tokayops/tokayops/internal/model"
)

// Lifted verbatim from internal/api/integration_handlers.go's old
// `case model.IntegrationTypeSlack:` branch.
func init() {
	Register(Descriptor{
		Type:         model.IntegrationTypeSlack,
		Direction:    model.IntegrationDirectionOutbound,
		SecretFields: []string{"token", "user_token", "signing_secret"},
		ValidateConfig: func(cfg json.RawMessage, isUpdate bool) error {
			var c model.SlackConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return errors.New("invalid slack config: " + err.Error())
			}
			// On create: require real token, reject masked.
			// On update: empty/masked means "keep existing".
			if !isUpdate {
				if c.Token == "" {
					return errors.New("slack token is required")
				}
				if c.Token == model.MaskedSecret {
					return errors.New("cannot use masked value as token")
				}
				if c.UserToken == model.MaskedSecret {
					return errors.New("cannot use masked value as user_token")
				}
				if c.SigningSecret == model.MaskedSecret {
					return errors.New("cannot use masked value as signing_secret")
				}
			}
			return nil
		},
	})
}
