package integrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/tokayops/tokayops/internal/model"
)

var (
	twilioAccountSID = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	// A coverage prefix is the start of an E.164 number: a plus and up to
	// fifteen digits. "+" alone covers every number.
	twilioCoveragePrefix = regexp.MustCompile(`^\+[0-9]{0,15}$`)
)

// twilioMaxCPS is the highest rate an account can raise itself to. Above it the
// provider decides, and a number here that the account does not have would
// only move the queue from this side to theirs.
const twilioMaxCPS = 5

// Twilio voice integration descriptor. Several may exist - one per sender
// number and coverage - which the unique index on outbound types allows for
// this type.
func init() {
	Register(Descriptor{
		Type:         model.IntegrationTypeTwilio,
		Direction:    model.IntegrationDirectionOutbound,
		SecretFields: []string{"auth_token"},
		ValidateConfig: func(cfg json.RawMessage, isUpdate bool) error {
			var c model.TwilioConfig
			if err := json.Unmarshal(cfg, &c); err != nil {
				return errors.New("invalid twilio config: " + err.Error())
			}
			return validateTwilio(c, isUpdate)
		},
	})
}

func validateTwilio(c model.TwilioConfig, isUpdate bool) error {
	if !twilioAccountSID.MatchString(c.AccountSID) {
		return errors.New("twilio account_sid must be AC followed by 32 hex digits")
	}
	// On create: require a real auth_token, reject masked.
	// On update: empty/masked means "keep existing".
	if !isUpdate {
		if c.AuthToken == "" {
			return errors.New("twilio auth_token is required")
		}
		if c.AuthToken == model.MaskedSecret {
			return errors.New("cannot use masked value as auth_token")
		}
	}
	if !model.ValidE164(c.FromNumber) {
		return errors.New("twilio from_number must be an E.164 number, like +14155550100")
	}
	if len(c.Coverage) == 0 {
		return errors.New(`twilio coverage must name at least one prefix; "+" covers every number`)
	}
	for _, prefix := range c.Coverage {
		if !twilioCoveragePrefix.MatchString(prefix) {
			return fmt.Errorf("twilio coverage prefix %q must be a plus followed by digits", prefix)
		}
	}
	if c.CPS < 1 || c.CPS > twilioMaxCPS {
		return fmt.Errorf("twilio cps must be between 1 and %d", twilioMaxCPS)
	}
	if c.MaxConcurrent < 1 {
		return errors.New("twilio max_concurrent must be at least 1")
	}
	if c.Priority < 0 {
		return errors.New("twilio priority must not be negative")
	}
	return nil
}
