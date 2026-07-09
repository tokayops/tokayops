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
	SecretToken   string `json:"secret_token,omitempty"`    // X-Telegram-Bot-Api-Secret-Token for webhook verification (Sprint 3)
	DefaultChatID string `json:"default_chat_id,omitempty"` // Default chat id (convenience; not used in send path in S1)
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
// (Sprint 4) and MaskSecrets (Sprint 5) moved to internal/integrations. Type
// metadata — including which config fields are secret — is declared next to the
// per-type Descriptor instead of as a switch here. Use integrations.MaskSecrets.
