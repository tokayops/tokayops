package integrations

import (
	"encoding/json"
	"errors"

	"github.com/tokayops/tokayops/internal/model"
)

// Telegram integration descriptor (Epic 8). Mirrors slack.go: one Register call
// declares the type, direction, secret fields (for masking), and config validation.
// Masking is fully driven by SecretFields via MaskSecrets — no per-type code.
func init() {
	Register(Descriptor{
		Type:         model.IntegrationTypeTelegram,
		Direction:    model.IntegrationDirectionOutbound,
		SecretFields: []string{"bot_token", "secret_token"},
		ValidateConfig: func(cfg json.RawMessage, isUpdate bool) error {
			var c model.TelegramConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return errors.New("invalid telegram config: " + err.Error())
			}
			// On create: require real bot_token, reject masked.
			// On update: empty/masked means "keep existing".
			if !isUpdate {
				if c.BotToken == "" {
					return errors.New("telegram bot_token is required")
				}
				if c.BotToken == model.MaskedSecret {
					return errors.New("cannot use masked value as bot_token")
				}
				// secret_token is required: interactivity (the webhook) is mandatory
				// for telegram in Epic 8, and the webhook middleware rejects calls
				// without a configured secret. Requiring it avoids dead Ack/Resolve buttons.
				if c.SecretToken == "" {
					return errors.New("telegram secret_token is required")
				}
				if c.SecretToken == model.MaskedSecret {
					return errors.New("cannot use masked value as secret_token")
				}
			}
			return nil
		},
	})
}
