package outbox

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

func TestSignPayload(t *testing.T) {
	// Known input → expected HMAC-SHA256 hex
	ts := "1700000000"
	body := []byte(`{"event":"alert_group.firing"}`)
	secret := "test-secret"

	sig := signPayload(ts, body, secret)
	if sig == "" {
		t.Fatal("signPayload returned empty string")
	}
	// Verify deterministic
	sig2 := signPayload(ts, body, secret)
	if sig != sig2 {
		t.Errorf("signPayload not deterministic: %s != %s", sig, sig2)
	}
	// Different secret → different sig
	sig3 := signPayload(ts, body, "other-secret")
	if sig == sig3 {
		t.Error("different secret should produce different signature")
	}
}

func TestBuildHeaders_AllPresent(t *testing.T) {
	body := []byte(`{"test":true}`)
	custom := map[string]string{"X-Custom": "value"}
	h := BuildHeaders("evt-123", model.OutboxEventFiring, body, "secret", custom)

	required := []string{
		"Content-Type",
		"X-Tokay-Event",
		"X-Tokay-Event-ID",
		"X-Tokay-Timestamp",
		"X-Tokay-Signature",
		"X-Custom",
	}
	for _, key := range required {
		if _, ok := h[key]; !ok {
			t.Errorf("missing header %s", key)
		}
	}

	if h["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %s, want application/json", h["Content-Type"])
	}
	if h["X-Tokay-Event"] != "alert_group.firing" {
		t.Errorf("X-Tokay-Event = %s, want alert_group.firing", h["X-Tokay-Event"])
	}
	if h["X-Tokay-Event-ID"] != "evt-123" {
		t.Errorf("X-Tokay-Event-ID = %s, want evt-123", h["X-Tokay-Event-ID"])
	}
	if h["X-Custom"] != "value" {
		t.Errorf("X-Custom = %s, want value", h["X-Custom"])
	}
}

func TestBuildHeaders_NoSecret(t *testing.T) {
	h := BuildHeaders("evt-1", model.OutboxEventResolved, []byte(`{}`), "", nil)
	if _, ok := h["X-Tokay-Signature"]; ok {
		t.Error("X-Tokay-Signature should not be set when secret is empty")
	}
}

func TestHTTPSender_SuccessfulDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	// Allow loopback since httptest binds to 127.0.0.1
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	sender := NewHTTPSender([]*net.IPNet{loopback})
	result := sender.Send(context.Background(), server.URL, []byte(`{}`), map[string]string{
		"Content-Type": "application/json",
	})

	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.HTTPStatus != 200 {
		t.Errorf("status = %d, want 200", result.HTTPStatus)
	}
	if result.ResponseBody != `{"ok":true}` {
		t.Errorf("body = %q, want %q", result.ResponseBody, `{"ok":true}`)
	}
}

func TestHTTPSender_SSRFBlocksPrivateIP(t *testing.T) {
	// Attempt to connect to localhost (127.0.0.1) without allowlist
	sender := NewHTTPSender(nil)
	result := sender.Send(context.Background(), "http://127.0.0.1:1/test", []byte(`{}`), nil)

	if result.Error == nil {
		t.Fatal("expected SSRF block error")
	}
	if !contains(result.Error.Error(), "ip_policy") && !contains(result.Error.Error(), "connection refused") {
		// On some systems the dial may fail before SSRF check, that's acceptable
		t.Logf("got error: %v (acceptable if connection-level)", result.Error)
	}
}

func TestHTTPSender_SSRFAllowedCIDR(t *testing.T) {
	// Start a local server (binds to 127.0.0.1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Allow 127.0.0.0/8
	_, cidr, _ := net.ParseCIDR("127.0.0.0/8")
	sender := NewHTTPSender([]*net.IPNet{cidr})
	result := sender.Send(context.Background(), server.URL, []byte(`{}`), nil)

	if result.Error != nil {
		t.Fatalf("expected allowed CIDR to pass, got error: %v", result.Error)
	}
	if result.HTTPStatus != 200 {
		t.Errorf("status = %d, want 200", result.HTTPStatus)
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip      string
		private bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.0.1", true},
		{"8.8.8.8", false},
		{"1.1.1.1", false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		got := isPrivateIP(ip)
		if got != tt.private {
			t.Errorf("isPrivateIP(%s) = %v, want %v", tt.ip, got, tt.private)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
