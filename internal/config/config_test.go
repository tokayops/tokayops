package config

import (
	"os"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	// Create temp config file
	content := `
config_version: 3
global:
  firehose_critical_channel: "C1"
  firehose_warning_channel: "C2"
`
	tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(tmpfile.Name())
	if err != nil {
		t.Errorf("LoadConfig() error = %v", err)
		return
	}

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() error = %v", err)
	}

	if cfg.Global.FirehoseCriticalChannel != "C1" {
		t.Errorf("Expected global.firehose_critical_channel = C1, got %s", cfg.Global.FirehoseCriticalChannel)
	}
}
