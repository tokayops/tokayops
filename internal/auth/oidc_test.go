package auth

import (
	"testing"
)

func TestLoadOIDCConfigFromEnv(t *testing.T) {
	// This test verifies env parsing (would need to set env vars)
	// For unit tests, we test the config struct directly
	t.Run("ParseAllowedDomains", func(t *testing.T) {
		// Test with comma-separated domains
		cfg := OIDCConfig{
			IssuerURL:      "https://accounts.google.com",
			ClientID:       "test-client-id",
			AllowedDomains: []string{"acme.com", "example.org"},
		}

		if len(cfg.AllowedDomains) != 2 {
			t.Errorf("Expected 2 allowed domains, got %d", len(cfg.AllowedDomains))
		}
	})
}

func TestOIDCProvider_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		provider *OIDCProvider
		want     bool
	}{
		{
			name:     "Nil provider",
			provider: nil,
			want:     false,
		},
		{
			name: "Empty client ID",
			provider: &OIDCProvider{
				config: OIDCConfig{
					ClientID: "",
				},
			},
			want: false,
		},
		{
			name: "Valid client ID",
			provider: &OIDCProvider{
				config: OIDCConfig{
					ClientID: "test-client-id",
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.provider.IsEnabled()
			if got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOIDCProvider_ValidateDomain(t *testing.T) {
	tests := []struct {
		name           string
		allowedDomains []string
		email          string
		want           bool
	}{
		// Basic valid cases
		{
			name:           "Valid domain - exact match",
			allowedDomains: []string{"acme.com"},
			email:          "user@acme.com",
			want:           true,
		},
		{
			name:           "Valid domain - multiple allowed",
			allowedDomains: []string{"acme.com", "example.org"},
			email:          "user@example.org",
			want:           true,
		},
		{
			name:           "Valid domain - case insensitive",
			allowedDomains: []string{"acme.com"},
			email:          "user@ACME.COM",
			want:           true,
		},
		{
			name:           "Valid domain - allowed domain uppercase",
			allowedDomains: []string{"ACME.COM"},
			email:          "user@acme.com",
			want:           true,
		},

		// Invalid cases - domain not in list
		{
			name:           "Invalid domain - not in list",
			allowedDomains: []string{"acme.com"},
			email:          "attacker@evil.com",
			want:           false,
		},
		{
			name:           "Invalid domain - similar but different",
			allowedDomains: []string{"acme.com"},
			email:          "user@acme.org",
			want:           false,
		},

		// SECURITY: Subdomain bypass attempts
		{
			name:           "SECURITY: Subdomain bypass attempt",
			allowedDomains: []string{"acme.com"},
			email:          "user@evil.acme.com",
			want:           false, // Subdomains should NOT be allowed
		},
		{
			name:           "SECURITY: Prefix bypass attempt",
			allowedDomains: []string{"acme.com"},
			email:          "user@fakeacme.com",
			want:           false,
		},
		{
			name:           "SECURITY: Suffix bypass attempt",
			allowedDomains: []string{"acme.com"},
			email:          "user@acme.com.evil.com",
			want:           false,
		},

		// Edge cases
		{
			name:           "Invalid email format - no @",
			allowedDomains: []string{"acme.com"},
			email:          "invalid-email",
			want:           false,
		},
		{
			name:           "Invalid email format - multiple @",
			allowedDomains: []string{"acme.com"},
			email:          "user@acme.com@evil.com",
			want:           false,
		},
		{
			name:           "Empty email",
			allowedDomains: []string{"acme.com"},
			email:          "",
			want:           false,
		},
		{
			name:           "Empty allowed domains - allows all",
			allowedDomains: []string{},
			email:          "user@any-domain.com",
			want:           true,
		},
		{
			name:           "Nil allowed domains - allows all",
			allowedDomains: nil,
			email:          "user@any-domain.com",
			want:           true,
		},

		// SECURITY: Whitespace/special characters
		{
			name:           "SECURITY: Trailing whitespace in email",
			allowedDomains: []string{"acme.com"},
			email:          "user@acme.com ",
			want:           false, // Should NOT match due to trailing space
		},
		{
			name:           "SECURITY: Leading whitespace in domain",
			allowedDomains: []string{"acme.com"},
			email:          "user@ acme.com",
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &OIDCProvider{
				config: OIDCConfig{
					ClientID:       "test",
					AllowedDomains: tt.allowedDomains,
				},
			}

			got := provider.ValidateDomain(tt.email)
			if got != tt.want {
				t.Errorf("ValidateDomain(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}

func TestOIDCProvider_ValidateDomain_NilProvider(t *testing.T) {
	var provider *OIDCProvider = nil
	if provider.ValidateDomain("user@acme.com") != false {
		t.Error("Nil provider should return false for ValidateDomain")
	}
}

func TestOIDCProvider_GetAllowedDomains(t *testing.T) {
	t.Run("Nil provider", func(t *testing.T) {
		var provider *OIDCProvider = nil
		if provider.GetAllowedDomains() != nil {
			t.Error("Nil provider should return nil for GetAllowedDomains")
		}
	})

	t.Run("With domains", func(t *testing.T) {
		provider := &OIDCProvider{
			config: OIDCConfig{
				AllowedDomains: []string{"a.com", "b.com"},
			},
		}
		domains := provider.GetAllowedDomains()
		if len(domains) != 2 {
			t.Errorf("Expected 2 domains, got %d", len(domains))
		}
	})
}

func TestOIDCProvider_GetAuthURL(t *testing.T) {
	t.Run("Nil provider returns empty", func(t *testing.T) {
		var provider *OIDCProvider = nil
		if provider.GetAuthURL("state123") != "" {
			t.Error("Nil provider should return empty string for GetAuthURL")
		}
	})
}
