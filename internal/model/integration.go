package model

import (
	"encoding/json"
	"strings"
	"time"
)

// IntegrationType defines the type of integration
type IntegrationType string

const (
	IntegrationTypeSlack               IntegrationType = "slack"
	IntegrationTypeTelegram            IntegrationType = "telegram"
	IntegrationTypeAlertmanagerWebhook IntegrationType = "alertmanager_webhook"
	IntegrationTypeGenericWebhook      IntegrationType = "generic_webhook"
	IntegrationTypeTwilio              IntegrationType = "twilio"
)

// WebhookScope defines the scope of a generic_webhook subscription
type WebhookScope string

const (
	WebhookScopeGlobal WebhookScope = "global"
	WebhookScopeTeam   WebhookScope = "team"
)

// IntegrationDirection defines whether integration is inbound or outbound
type IntegrationDirection string

const (
	IntegrationDirectionInbound  IntegrationDirection = "inbound"
	IntegrationDirectionOutbound IntegrationDirection = "outbound"
)

// Integration represents an external integration configuration
type Integration struct {
	ID        string               `json:"id"`
	Type      IntegrationType      `json:"type"`
	Direction IntegrationDirection `json:"direction"`
	Name      string               `json:"name"`
	Enabled   bool                 `json:"enabled"`
	Scope     *WebhookScope        `json:"scope,omitempty"`
	TeamID    *string              `json:"team_id,omitempty"`
	Config    json.RawMessage      `json:"config" swaggertype:"object"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

// IntegrationTombstone is what remains of a deleted integration: enough to
// decide who may still read its delivery history, and nothing else. The scope
// is the one it had when it was deleted, on purpose - it describes a
// historical object, and who could see that history does not change afterwards.
type IntegrationTombstone struct {
	ID        string
	Type      IntegrationType
	Scope     *WebhookScope
	TeamID    *string
	DeletedAt time.Time
}

// SlackConfig is the config schema for Slack integrations
type SlackConfig struct {
	Token          string `json:"token"`                     // Bot token for notifications
	UserToken      string `json:"user_token,omitempty"`      // User token for usergroup syncer (optional)
	DefaultChannel string `json:"default_channel,omitempty"` // Default channel for notifications
	SigningSecret  string `json:"signing_secret,omitempty"`  // Signing secret for request verification
	Interactive    bool   `json:"interactive"`               // Enable Ack/Resolve buttons in Slack messages
	TeamURL        string `json:"team_url,omitempty"`        // The workspace's URL as auth.test names it; not a secret
}

// TelegramConfig is the config schema for Telegram integrations.
// bot_token / secret_token are secrets; the json tags MUST match the
// telegram Descriptor.SecretFields so MaskSecrets/mergeSecrets find them.
type TelegramConfig struct {
	BotToken      string `json:"bot_token"`                 // Bot API token for notifications
	SecretToken   string `json:"secret_token,omitempty"`    // X-Telegram-Bot-Api-Secret-Token for webhook verification
	DefaultChatID string `json:"default_chat_id,omitempty"` // Default chat id (convenience; not used in send path)
	// Interactive enables Ack/Resolve buttons in Telegram cards. A nil value
	// means "not set" and resolves to true: records written before this field
	// existed had interactivity switched on unconditionally, and an upgrade
	// must not silently take their buttons away. Read it via IsInteractive().
	Interactive *bool `json:"interactive,omitempty"`
}

// IsInteractive reports whether Ack/Resolve buttons are enabled, defaulting to
// true when the field was never set.
func (c TelegramConfig) IsInteractive() bool {
	if c.Interactive == nil {
		return true
	}
	return *c.Interactive
}

// TwilioConfig is the config schema for a Twilio voice integration: one
// account, one sender number, and the numbers it may call.
//
// auth_token is a secret; the json tag MUST match the twilio
// Descriptor.SecretFields so MaskSecrets/mergeSecrets find it.
type TwilioConfig struct {
	AccountSID string `json:"account_sid"`
	AuthToken  string `json:"auth_token"`
	// FromNumber is the number the call comes from, in E.164. A person adds it
	// to their contacts and to the exceptions of Do Not Disturb, so it is one
	// fixed number and not a pool.
	FromNumber string `json:"from_number"`
	// Coverage is the E.164 prefixes this integration may call: "+7" for a
	// carrier that reaches Russian numbers only, "+" for the whole world. A
	// number no enabled integration covers is not called at all - this is also
	// the list of countries a person can make the account call.
	Coverage []string `json:"coverage"`
	// Priority orders the integrations that cover a number; lower goes first.
	Priority int `json:"priority"`
	// CPS and MaxConcurrent are the account's limits as this integration
	// states them. The provider applies them per account, and so does the
	// limiter: integrations on one account share the lowest of their values.
	CPS           int `json:"cps"`
	MaxConcurrent int `json:"max_concurrent"`
	// Language is the voice of <Say>, as Twilio names it. Empty means en-US.
	Language string `json:"language,omitempty"`
}

// SayLanguage is the language the calls of this integration speak in.
func (c TwilioConfig) SayLanguage() string {
	if c.Language == "" {
		return "en-US"
	}
	return c.Language
}

// Covers reports whether number starts with one of the coverage prefixes.
func (c TwilioConfig) Covers(number string) bool {
	for _, prefix := range c.Coverage {
		if strings.HasPrefix(number, prefix) {
			return true
		}
	}
	return false
}

// WebhookConfig is the config schema for Alertmanager webhook integrations
type WebhookConfig struct {
	Secret string `json:"secret"`

	// StaleAfterSeconds is how long a silence about an alert group is normal
	// for the Alertmanager sending through this integration: after it, the
	// view marks the group stale.
	//
	// It is the operator's number, not one this system works out. What is
	// normal is the route's repeat_interval plus its group_interval, and
	// neither is in anything Alertmanager sends. Zero or absent means nothing
	// is claimed, and nothing is shown.
	//
	// Whole minutes, because that is what the form takes and shows: a value of
	// 90 would come back as 2 minutes and be saved as 120 the next time
	// somebody opened the form, which is a setting that changes by being
	// looked at.
	StaleAfterSeconds int `json:"stale_after_seconds,omitempty"`
}

// StaleAfterBounds are the seconds a declared silence may be. A minute is the
// shortest interval Alertmanager is ever configured to repeat at; a week is
// past the point where silence that long is news.
const (
	StaleAfterMin = 60
	StaleAfterMax = 604800
)

// GenericWebhookConfig is the config schema for generic outbound webhook integrations
type GenericWebhookConfig struct {
	URL            string            `json:"url"`
	Secret         string            `json:"secret"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	CustomHeaders  map[string]string `json:"custom_headers,omitempty"`
}

// MaskedSecret is the placeholder shown in API responses
const MaskedSecret = "****"

// Note: ValidIntegrationTypes / IsValidIntegrationType / GetDirectionForType
// and MaskSecrets moved to internal/integrations. Type
// metadata - including which config fields are secret - is declared next to the
// per-type Descriptor instead of as a switch here. Use integrations.MaskSecrets.
