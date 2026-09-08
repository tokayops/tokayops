package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestADirectMessageFallsBackToTheFirehoseUnlessToldNot. The setting decides
// whether a direct message points back to the firehose card when the policy
// posted no channel card of its own. Left out, it is yes - what every
// installation had before there was a setting.
func TestADirectMessageFallsBackToTheFirehoseUnlessToldNot(t *testing.T) {
	for raw, want := range map[string]bool{
		"global:\n  self_url: https://tokay.example\n": true,
		"global:\n  dm_fallback_to_firehose: true\n":   true,
		"global:\n  dm_fallback_to_firehose: false\n":  false,
	} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatalf("read %q: %v", raw, err)
		}
		if got := cfg.Global.DMFallsBackToFirehose(); got != want {
			t.Errorf("%q falls back: %v, want %v", raw, got, want)
		}
	}
}
