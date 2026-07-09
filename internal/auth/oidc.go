package auth

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig holds the OIDC provider configuration.
type OIDCConfig struct {
	IssuerURL      string   // OIDC_ISSUER_URL (e.g., https://accounts.google.com)
	ClientID       string   // OIDC_CLIENT_ID
	ClientSecret   string   // OIDC_CLIENT_SECRET
	RedirectURL    string   // OIDC_REDIRECT_URL (e.g., https://tokayops.example.com/api/auth/oidc/callback)
	AllowedDomains []string // OIDC_ALLOWED_DOMAINS (comma-separated in env)
}

// OIDCUserInfo contains user information extracted from OIDC token.
type OIDCUserInfo struct {
	Email   string
	Name    string
	Picture string // Optional, for future use
}

// OIDCProvider handles OIDC authentication flow.
type OIDCProvider struct {
	config       OIDCConfig
	provider     *oidc.Provider
	oauth2Config *oauth2.Config
	verifier     *oidc.IDTokenVerifier
}

// LoadOIDCConfigFromEnv loads OIDC configuration from environment variables.
func LoadOIDCConfigFromEnv() OIDCConfig {
	domains := os.Getenv("OIDC_ALLOWED_DOMAINS")
	var allowedDomains []string
	if domains != "" {
		for _, d := range strings.Split(domains, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				allowedDomains = append(allowedDomains, d)
			}
		}
	}

	// Build redirect URL from TOKAY_SELF_URL (e.g. https://tokayops.example.com -> https://tokayops.example.com/api/auth/oidc/callback)
	redirectURL := ""
	if selfURL := os.Getenv("TOKAY_SELF_URL"); selfURL != "" {
		redirectURL = strings.TrimSuffix(selfURL, "/") + "/api/auth/oidc/callback"
	}

	return OIDCConfig{
		IssuerURL:      os.Getenv("OIDC_ISSUER_URL"),
		ClientID:       os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret:   os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:    redirectURL,
		AllowedDomains: allowedDomains,
	}
}

// NewOIDCProvider creates a new OIDC provider. Returns nil if OIDC is not configured.
func NewOIDCProvider(ctx context.Context, cfg OIDCConfig) (*OIDCProvider, error) {
	if cfg.ClientID == "" || cfg.IssuerURL == "" {
		return nil, nil // OIDC not configured, return nil (not an error)
	}

	// Validate that RedirectURL is set (derived from TOKAY_SELF_URL)
	if cfg.RedirectURL == "" {
		return nil, errors.New("OIDC requires TOKAY_SELF_URL to be configured for redirect URL")
	}

	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}

	oauth2Config := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})

	return &OIDCProvider{
		config:       cfg,
		provider:     provider,
		oauth2Config: oauth2Config,
		verifier:     verifier,
	}, nil
}

// IsEnabled returns true if OIDC is properly configured.
func (p *OIDCProvider) IsEnabled() bool {
	return p != nil && p.config.ClientID != ""
}

// GetAuthURL generates the authorization URL for OIDC login.
func (p *OIDCProvider) GetAuthURL(state string) string {
	if p == nil {
		return ""
	}
	return p.oauth2Config.AuthCodeURL(state)
}

// ExchangeCode exchanges the authorization code for tokens and extracts user info.
func (p *OIDCProvider) ExchangeCode(ctx context.Context, code string) (*OIDCUserInfo, error) {
	if p == nil {
		return nil, errors.New("OIDC provider not configured")
	}

	// Exchange code for token
	oauth2Token, err := p.oauth2Config.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}

	// Extract ID token
	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in response")
	}

	// Verify ID token
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, err
	}

	// Extract claims
	var claims struct {
		Email      string `json:"email"`
		Name       string `json:"name"`
		GivenName  string `json:"given_name"`
		FamilyName string `json:"family_name"`
		Picture    string `json:"picture"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, err
	}

	if claims.Email == "" {
		return nil, errors.New("email not found in token claims")
	}

	// Build full name: prefer name, fallback to given_name + family_name
	fullName := claims.Name
	if fullName == "" && (claims.GivenName != "" || claims.FamilyName != "") {
		fullName = strings.TrimSpace(claims.GivenName + " " + claims.FamilyName)
	}

	return &OIDCUserInfo{
		Email:   claims.Email,
		Name:    fullName,
		Picture: claims.Picture,
	}, nil
}

// ValidateDomain checks if the email domain is in the allowed list.
// Returns true if allowed domains list is empty (no restriction).
func (p *OIDCProvider) ValidateDomain(email string) bool {
	if p == nil {
		return false
	}

	// No domain restriction if list is empty
	if len(p.config.AllowedDomains) == 0 {
		return true
	}

	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return false
	}
	domain := strings.ToLower(parts[1])

	for _, allowed := range p.config.AllowedDomains {
		if strings.ToLower(allowed) == domain {
			return true
		}
	}
	return false
}

// GetAllowedDomains returns the list of allowed domains.
func (p *OIDCProvider) GetAllowedDomains() []string {
	if p == nil {
		return nil
	}
	return p.config.AllowedDomains
}
