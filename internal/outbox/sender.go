package outbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
)

// DeliveryResult holds the outcome of a single webhook HTTP call.
type DeliveryResult struct {
	HTTPStatus   int
	Error        error
	ResponseBody string // truncated to 1KB
}

// WebhookSender abstracts HTTP delivery for testing.
type WebhookSender interface {
	Send(ctx context.Context, url string, body []byte, headers map[string]string) *DeliveryResult
}

// privateRanges are the CIDR ranges considered "private" for SSRF protection.
var privateRanges []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fe80::/10",
	} {
		_, ipNet, _ := net.ParseCIDR(cidr)
		privateRanges = append(privateRanges, ipNet)
	}
}

// HTTPSender implements WebhookSender with SSRF protection.
type HTTPSender struct {
	allowedCIDRs []*net.IPNet
}

// NewHTTPSender creates a sender with the given allowed private CIDRs.
func NewHTTPSender(allowedCIDRs []*net.IPNet) *HTTPSender {
	return &HTTPSender{allowedCIDRs: allowedCIDRs}
}

func (s *HTTPSender) Send(ctx context.Context, url string, body []byte, headers map[string]string) *DeliveryResult {
	transport := &http.Transport{
		DialContext: s.safeDial,
	}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return &DeliveryResult{Error: fmt.Errorf("build request: %w", err)}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	duration := time.Since(start).Seconds()
	metrics.OutboxDeliveryDuration.Observe(duration)

	if err != nil {
		return &DeliveryResult{Error: err}
	}
	defer resp.Body.Close()

	// Read up to 1KB of response body
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return &DeliveryResult{
		HTTPStatus:   resp.StatusCode,
		ResponseBody: string(respBody),
	}
}

// safeDial resolves the hostname, checks the IP against private ranges,
// and blocks if the IP is private and not in the allowed list.
func (s *HTTPSender) safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	resolver := &net.Resolver{}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("dns resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("dns resolve %s: no addresses", host)
	}

	ip := ips[0].IP
	if isPrivateIP(ip) && !s.isAllowed(ip) {
		metrics.OutboxDeliveryBlockedTotal.Inc()
		return nil, fmt.Errorf("ip_policy: blocked private IP %s", ip)
	}

	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
}

func isPrivateIP(ip net.IP) bool {
	for _, cidr := range privateRanges {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *HTTPSender) isAllowed(ip net.IP) bool {
	for _, cidr := range s.allowedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// BuildHeaders constructs the outbound webhook headers.
func BuildHeaders(eventID string, eventType model.OutboxEventType, body []byte,
	secret string, customHeaders map[string]string) map[string]string {

	ts := fmt.Sprintf("%d", time.Now().Unix())

	h := map[string]string{
		"Content-Type":       "application/json",
		"X-Tokay-Event":     string(eventType),
		"X-Tokay-Event-ID":  eventID,
		"X-Tokay-Timestamp": ts,
	}

	if secret != "" {
		sig := signPayload(ts, body, secret)
		h["X-Tokay-Signature"] = "sha256=" + sig
	}

	for k, v := range customHeaders {
		h[k] = v
	}
	return h
}

// signPayload computes HMAC-SHA256 over "<timestamp>.<body>".
func signPayload(timestamp string, body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
