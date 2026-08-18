package store

import (
	"encoding/json"
	"log"
	"sync"

	"github.com/tokayops/tokayops/internal/model"
)

// IntegrationCache provides cached access to integration data
type IntegrationCache struct {
	mu                  sync.RWMutex
	slackToken          string
	slackUserToken      string
	slackChannel        string
	slackSigningSecret  string
	slackInteractive    bool
	telegramToken       string
	telegramSecretToken string
	telegramInteractive bool
	webhookSecrets      []string
}

// NewIntegrationCache creates a new IntegrationCache
func NewIntegrationCache() *IntegrationCache {
	return &IntegrationCache{
		webhookSecrets: []string{},
	}
}

// LoadAll loads all integrations from the store into cache
func (c *IntegrationCache) LoadAll(store StoreInterface) error {
	integrations, err := store.GetAllIntegrations()
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Reset
	c.slackToken = ""
	c.slackUserToken = ""
	c.slackChannel = ""
	c.slackSigningSecret = ""
	c.slackInteractive = false
	c.telegramToken = ""
	c.telegramSecretToken = ""
	c.telegramInteractive = false
	c.webhookSecrets = []string{}

	for _, i := range integrations {
		if !i.Enabled {
			continue
		}

		switch i.Type {
		case model.IntegrationTypeSlack:
			var slackCfg model.SlackConfig
			if err := json.Unmarshal(i.Config, &slackCfg); err != nil {
				log.Printf("IntegrationCache: Failed to parse Slack config for integration %s: %v", i.ID, err)
				continue
			}
			c.slackToken = slackCfg.Token
			c.slackUserToken = slackCfg.UserToken
			c.slackChannel = slackCfg.DefaultChannel
			c.slackSigningSecret = slackCfg.SigningSecret
			c.slackInteractive = slackCfg.Interactive
		case model.IntegrationTypeTelegram:
			var tgCfg model.TelegramConfig
			if err := json.Unmarshal(i.Config, &tgCfg); err != nil {
				log.Printf("IntegrationCache: Failed to parse Telegram config for integration %s: %v", i.ID, err)
				continue
			}
			c.telegramToken = tgCfg.BotToken
			c.telegramSecretToken = tgCfg.SecretToken
			c.telegramInteractive = tgCfg.IsInteractive()
		case model.IntegrationTypeAlertmanagerWebhook:
			var webhookCfg model.WebhookConfig
			if err := json.Unmarshal(i.Config, &webhookCfg); err != nil {
				log.Printf("IntegrationCache: Failed to parse webhook config for integration %s: %v", i.ID, err)
				continue
			}
			if webhookCfg.Secret != "" {
				c.webhookSecrets = append(c.webhookSecrets, webhookCfg.Secret)
			}
		case model.IntegrationTypeGenericWebhook:
			// Generic webhook subscriptions are handled by the outbox worker, not cached here
		}
	}

	log.Printf("IntegrationCache: Loaded %d integrations (slack=%v, user_token=%v, telegram=%v, webhooks=%d)",
		len(integrations), c.slackToken != "", c.slackUserToken != "", c.telegramToken != "", len(c.webhookSecrets))
	return nil
}

// GetSlackToken returns the cached Slack bot token
func (c *IntegrationCache) GetSlackToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slackToken
}

// GetSlackUserToken returns the cached Slack user token (for usergroup syncer)
func (c *IntegrationCache) GetSlackUserToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slackUserToken
}

// GetSlackChannel returns the cached default Slack channel
func (c *IntegrationCache) GetSlackChannel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slackChannel
}

// GetSlackSigningSecret returns the cached Slack signing secret
func (c *IntegrationCache) GetSlackSigningSecret() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slackSigningSecret
}

// GetSlackInteractive returns whether interactive buttons are enabled
func (c *IntegrationCache) GetSlackInteractive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slackInteractive
}

// GetTelegramToken returns the cached Telegram bot token
func (c *IntegrationCache) GetTelegramToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.telegramToken
}

// GetTelegramSecretToken returns the cached Telegram webhook secret token
// (X-Telegram-Bot-Api-Secret-Token). Consumed by the webhook middleware in
// Sprint 3; lives on the concrete cache, not the provider TokenSource interface.
func (c *IntegrationCache) GetTelegramSecretToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.telegramSecretToken
}

// GetTelegramInteractive returns whether Ack/Resolve buttons are enabled for
// Telegram. Mirrors GetSlackInteractive; the stored config resolves a missing
// value to true (see model.TelegramConfig.IsInteractive).
func (c *IntegrationCache) GetTelegramInteractive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.telegramInteractive
}

// ValidateWebhookSecret checks if the given secret matches any configured webhook
func (c *IntegrationCache) ValidateWebhookSecret(secret string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// No secrets configured = reject all (secure by default)
	if len(c.webhookSecrets) == 0 {
		return false
	}

	// Empty token = reject
	if secret == "" {
		return false
	}

	for _, s := range c.webhookSecrets {
		if s == secret {
			return true
		}
	}
	return false
}

// HasWebhookSecrets returns true if any webhook secrets are configured
func (c *IntegrationCache) HasWebhookSecrets() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.webhookSecrets) > 0
}
