package config

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
)

const (
	WebhookAllowHTTPEnv        = "TOKAY_WEBHOOK_ALLOW_HTTP"
	WebhookAllowPrivateCIDREnv = "TOKAY_WEBHOOK_ALLOW_PRIVATE_CIDRS"
)

// ValidateWebhookURL checks URL is valid and has allowed scheme.
// By default only https is allowed; set TOKAY_WEBHOOK_ALLOW_HTTP=true to allow http.
func ValidateWebhookURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("url must include a host")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" {
		return nil
	}
	if scheme == "http" {
		if strings.EqualFold(os.Getenv(WebhookAllowHTTPEnv), "true") {
			return nil
		}
		return fmt.Errorf("http urls are not allowed (set %s=true to override)", WebhookAllowHTTPEnv)
	}
	return fmt.Errorf("unsupported url scheme %q, must be https", scheme)
}

// ParseAllowedPrivateCIDRs parses TOKAY_WEBHOOK_ALLOW_PRIVATE_CIDRS env var.
// Returns parsed CIDRs or error if any CIDR is malformed.
// Empty env var means no private CIDRs are allowed (default).
func ParseAllowedPrivateCIDRs() ([]*net.IPNet, error) {
	raw := os.Getenv(WebhookAllowPrivateCIDREnv)
	if raw == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, cidr := range strings.Split(raw, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q in %s: %w", cidr, WebhookAllowPrivateCIDREnv, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// LogWebhookSecurityWarnings logs warnings for insecure webhook settings.
func LogWebhookSecurityWarnings() {
	if strings.EqualFold(os.Getenv(WebhookAllowHTTPEnv), "true") {
		log.Printf("WARN: %s is enabled — webhook URLs may use plain HTTP", WebhookAllowHTTPEnv)
	}
	cidrs, _ := ParseAllowedPrivateCIDRs()
	if len(cidrs) > 0 {
		log.Printf("WARN: %s is set — webhooks may target private IPs: %v", WebhookAllowPrivateCIDREnv, cidrs)
	}
}
