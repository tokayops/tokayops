package model

import (
	"encoding/json"
	"time"
)

// IntegrationType defines the type of integration
type IntegrationType string

const (
	IntegrationTypeSlack               IntegrationType = "slack"
	IntegrationTypeTelegram            IntegrationType = "telegram"
	IntegrationTypeAlertmanagerWebhook IntegrationType = "alertmanager_webhook"
	IntegrationTypeGenericWebhook      IntegrationType = "generic_webhook"
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

// SlackConfig is the config schema for Slack integrations
type SlackConfig struct {
	Token          string `json:"token"`                     // Bot token for notifications
	UserToken      string `json:"user_token,omitempty"`      // User token for usergroup syncer (optional)
	DefaultChannel string `json:"default_channel,omitempty"` // Default channel for notifications
	SigningSecret  string `json:"signing_secret,omitempty"`  // Signing secret for request verification
	Interactive    bool   `json:"interactive"`               // Enable Ack/Resolve buttons in Slack messages
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

// WebhookConfig is the config schema for Alertmanager webhook integrations
type WebhookConfig struct {
	Secret string `json:"secret"`
}

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
