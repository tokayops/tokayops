package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

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
	// webhookSources is the secret an Alertmanager sends with, to the
	// integration it belongs to. Only the id: what that integration says about
	// itself is read from the database when a payload arrives, because this
	// cache is refreshed by whichever instance handled the change and by no
	// other.
	webhookSources map[string]string

	// loaded is a digest of what the last load found; the log says so only when
	// it changes. salt keeps that digest to this process.
	loaded [32]byte
	salt   []byte
}

// NewIntegrationCache creates a new IntegrationCache
func NewIntegrationCache() *IntegrationCache {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		// Only if the system has no randomness at all. A constant salt still
		// tells two loads apart, which is all this is for.
		log.Printf("IntegrationCache: no randomness for the load digest: %v", err)
	}
	return &IntegrationCache{
		webhookSources: map[string]string{},
		salt:           salt,
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
	c.webhookSources = map[string]string{}

	// By id, so the answer to a secret two integrations share is the same on
	// every instance and after every reload.
	sort.Slice(integrations, func(a, b int) bool { return integrations[a].ID < integrations[b].ID })

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
				// Two integrations with one secret is a configuration
				// somebody made by pasting twice. It still authenticates - the
				// secret is the secret - but only one of them can be the
				// sender, and it is the first by id, said out loud.
				if first, taken := c.webhookSources[webhookCfg.Secret]; taken {
					log.Printf("IntegrationCache: integrations %s and %s share a webhook secret; "+
						"payloads with it count as %s", first, i.ID, first)
					continue
				}
				c.webhookSources[webhookCfg.Secret] = i.ID
			}
		case model.IntegrationTypeGenericWebhook:
			// Generic webhook subscribers are not cached: the webhook channel reads
			// a subscriber's configuration from the database on every attempt
			// (SubscriberConfig), so a rotated secret is used by every instance at
			// once rather than by the one that handled the change.
		}
	}

	// Said when it is news. This runs on a timer as well as on a change, and a
	// line every thirty seconds saying the same thing is a log nobody reads.
	//
	// What counts as news is the whole of what was loaded, a rotated secret
	// included - which is why the comparison is over a digest and the line is
	// over counts. The digest is salted per process: it exists to tell two
	// loads apart here, and nothing outside should be able to match it against
	// a secret.
	loaded := c.fingerprint()
	if loaded != c.loaded {
		log.Printf("IntegrationCache: loaded %d integrations (slack=%v, user_token=%v, telegram=%v, webhooks=%d)",
			len(integrations), c.slackToken != "", c.slackUserToken != "",
			c.telegramToken != "", len(c.webhookSources))
		c.loaded = loaded
	}
	return nil
}

// fingerprint is everything this cache holds, as one value to compare against
// the last load. Caller holds the lock.
func (c *IntegrationCache) fingerprint() [32]byte {
	secrets := make([]string, 0, len(c.webhookSources))
	for secret, id := range c.webhookSources {
		secrets = append(secrets, secret+"\x00"+id)
	}
	sort.Strings(secrets)

	var buf bytes.Buffer
	buf.Write(c.salt)
	for _, part := range append([]string{
		c.slackToken, c.slackUserToken, c.slackChannel, c.slackSigningSecret,
		c.telegramToken, c.telegramSecretToken,
		fmt.Sprintf("%v/%v", c.slackInteractive, c.telegramInteractive),
	}, secrets...) {
		buf.WriteString(part)
		buf.WriteByte(0)
	}
	return sha256.Sum256(buf.Bytes())
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
// (X-Telegram-Bot-Api-Secret-Token). Consumed by the webhook middleware; lives
// on the concrete cache, not on the provider TokenSource interface.
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

// IntegrationCacheRefresh is how long an instance may go on answering from a
// cache somebody else's request changed.
//
// A change is reloaded at once by the instance that handled it and by nobody
// else, so without this an Alertmanager whose token was rotated - or one just
// created - is refused by every other instance until it restarts. Behind a load
// balancer that is a notification that lands on the wrong instance and is lost.
// Half a minute is short enough that a rotation is a blip and long enough that
// this is one small query per instance per interval.
// IntegrationCacheRefresh is that interval, and the wiring's only use for it.
const IntegrationCacheRefresh = 30 * time.Second

// Refresh keeps this cache in step with the database until the context ends.
//
// It answers nothing: the cache is a filter and a name, and what a payload is
// allowed to do is settled against the database on the way in. What this fixes
// is the other direction - a secret that has just become valid, which no
// database check can rescue, because a token the cache does not know never
// reaches one.
// The interval is the caller's so that a test can drive the loop itself; the
// wiring passes integrationCacheRefresh.
func (c *IntegrationCache) Refresh(ctx context.Context, store StoreInterface, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.LoadAll(store); err != nil {
				// Left as it was: an old answer is better than none, and the
				// database check still refuses whoever may no longer send.
				log.Printf("IntegrationCache: refresh failed, keeping what was loaded: %v", err)
			}
		}
	}
}

// WebhookIntegrationID answers which integration a secret belongs to.
//
// It is a filter and a name, not the decision: an instance that has not
// reloaded this cache since the integration was disabled or its secret rotated
// still holds the old one. What a payload is allowed to do is settled against
// the database, by the caller, with the id this answers.
//
// No secrets configured means reject all - secure by default - and so does an
// empty token.
func (c *IntegrationCache) WebhookIntegrationID(secret string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.webhookSources) == 0 || secret == "" {
		return "", false
	}
	id, ok := c.webhookSources[secret]
	return id, ok
}

// HasWebhookSecrets returns true if any webhook secrets are configured
func (c *IntegrationCache) HasWebhookSecrets() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.webhookSources) > 0
}
