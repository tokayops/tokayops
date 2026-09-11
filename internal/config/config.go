package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

const CurrentConfigVersion = 3 // v3: Removed providers, webhook_secret (use Integrations API)

// GlobalConfig contains system-wide settings for routing and logging.
type GlobalConfig struct {
	// The firehose: one Slack channel per severity that gets a card for every
	// alert of that severity, whatever the policy does. A severity whose
	// channel is empty gets no firehose card, and so does a severity that is
	// none of the three.
	FirehoseCriticalChannel string `yaml:"firehose_critical_channel"`
	FirehoseWarningChannel  string `yaml:"firehose_warning_channel"`
	FirehoseInfoChannel     string `yaml:"firehose_info_channel"`
	SelfURL                 string `yaml:"self_url"` // TokayOps base URL for deep links in Slack messages

	// DMFallbackToFirehose decides whether a direct message points back to
	// the firehose card when the policy posted no channel card of its own.
	// Unset means yes, as it always did.
	DMFallbackToFirehose *bool `yaml:"dm_fallback_to_firehose"`
}

// DMFallsBackToFirehose is the setting with its default applied.
func (g GlobalConfig) DMFallsBackToFirehose() bool {
	return g.DMFallbackToFirehose == nil || *g.DMFallbackToFirehose
}

// Config is the root configuration structure (v2)
type Config struct {
	ConfigVersion int          `yaml:"config_version"` // Must be 2
	Global        GlobalConfig `yaml:"global"`         // Global routing settings
}

func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	decoder := yaml.NewDecoder(f)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}

	// Apply environment variable overrides
	if err := cfg.applyEnvOverrides(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyEnvOverrides applies environment variable overrides to the config
func (c *Config) applyEnvOverrides() error {
	// Override self URL
	if selfURL := os.Getenv("TOKAY_SELF_URL"); selfURL != "" {
		c.Global.SelfURL = selfURL
	}

	return nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Check config version
	if c.ConfigVersion != CurrentConfigVersion {
		return fmt.Errorf("unsupported config_version: %d, expected %d", c.ConfigVersion, CurrentConfigVersion)
	}

	return nil
}
