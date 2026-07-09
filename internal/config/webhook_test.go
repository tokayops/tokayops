package config

import (
	"os"
	"testing"
)

func TestValidateWebhookURL(t *testing.T) {
	t.Run("https allowed", func(t *testing.T) {
		if err := ValidateWebhookURL("https://example.com/hook"); err != nil {
			t.Errorf("Expected no error, got: %v", err)
		}
	})

	t.Run("http rejected by default", func(t *testing.T) {
		os.Unsetenv(WebhookAllowHTTPEnv)
		err := ValidateWebhookURL("http://example.com/hook")
		if err == nil {
			t.Error("Expected error for http URL")
		}
	})

	t.Run("http allowed with override", func(t *testing.T) {
		os.Setenv(WebhookAllowHTTPEnv, "true")
		defer os.Unsetenv(WebhookAllowHTTPEnv)

		if err := ValidateWebhookURL("http://example.com/hook"); err != nil {
			t.Errorf("Expected no error with override, got: %v", err)
		}
	})

	t.Run("invalid scheme rejected", func(t *testing.T) {
		err := ValidateWebhookURL("ftp://example.com")
		if err == nil {
			t.Error("Expected error for ftp URL")
		}
	})

	t.Run("missing scheme rejected", func(t *testing.T) {
		err := ValidateWebhookURL("example.com/hook")
		if err == nil {
			t.Error("Expected error for URL without scheme")
		}
	})

	t.Run("https without host rejected", func(t *testing.T) {
		if err := ValidateWebhookURL("https://"); err == nil {
			t.Error("Expected error for https:// without host")
		}
	})

	t.Run("https with path only rejected", func(t *testing.T) {
		if err := ValidateWebhookURL("https:///path"); err == nil {
			t.Error("Expected error for https:///path without host")
		}
	})

	t.Run("port without hostname rejected", func(t *testing.T) {
		if err := ValidateWebhookURL("https://:443"); err == nil {
			t.Error("Expected error for https://:443 without hostname")
		}
	})
}

func TestParseAllowedPrivateCIDRs(t *testing.T) {
	t.Run("empty returns nil", func(t *testing.T) {
		os.Unsetenv(WebhookAllowPrivateCIDREnv)
		nets, err := ParseAllowedPrivateCIDRs()
		if err != nil {
			t.Errorf("Expected no error, got: %v", err)
		}
		if nets != nil {
			t.Errorf("Expected nil, got: %v", nets)
		}
	})

	t.Run("single CIDR", func(t *testing.T) {
		os.Setenv(WebhookAllowPrivateCIDREnv, "10.0.0.0/8")
		defer os.Unsetenv(WebhookAllowPrivateCIDREnv)

		nets, err := ParseAllowedPrivateCIDRs()
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if len(nets) != 1 {
			t.Errorf("Expected 1 CIDR, got %d", len(nets))
		}
	})

	t.Run("multiple CIDRs", func(t *testing.T) {
		os.Setenv(WebhookAllowPrivateCIDREnv, "10.0.0.0/8, 172.16.0.0/12")
		defer os.Unsetenv(WebhookAllowPrivateCIDREnv)

		nets, err := ParseAllowedPrivateCIDRs()
		if err != nil {
			t.Fatalf("Expected no error, got: %v", err)
		}
		if len(nets) != 2 {
			t.Errorf("Expected 2 CIDRs, got %d", len(nets))
		}
	})

	t.Run("invalid CIDR rejected", func(t *testing.T) {
		os.Setenv(WebhookAllowPrivateCIDREnv, "not-a-cidr")
		defer os.Unsetenv(WebhookAllowPrivateCIDREnv)

		_, err := ParseAllowedPrivateCIDRs()
		if err == nil {
			t.Error("Expected error for invalid CIDR")
		}
	})

	t.Run("mixed valid and invalid rejected", func(t *testing.T) {
		os.Setenv(WebhookAllowPrivateCIDREnv, "10.0.0.0/8, bad")
		defer os.Unsetenv(WebhookAllowPrivateCIDREnv)

		_, err := ParseAllowedPrivateCIDRs()
		if err == nil {
			t.Error("Expected error for mixed CIDRs with invalid entry")
		}
	})
}
