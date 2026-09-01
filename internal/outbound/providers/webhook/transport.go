package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The wire contract with a subscriber, and the two protections around it.
//
// The headers and the signature are the public contract: a subscriber verifies
// X-Tokay-Signature over "<timestamp>.<body>" with the shared secret and
// deduplicates by X-Tokay-Event-ID. They moved here from the old outbox worker
// byte for byte - a subscriber's verification code must not notice the move.

// Header names this system owns. A subscriber's custom headers may not replace
// them: X-Tokay-Event-ID is what every retry is deduplicated by, and a
// configuration able to overwrite it would turn each retry after doubt into a
// new event for the receiver.
const (
	HeaderContentType = "Content-Type"
	HeaderEvent       = "X-Tokay-Event"
	HeaderEventID     = "X-Tokay-Event-ID"
	HeaderTimestamp   = "X-Tokay-Timestamp"
	HeaderSignature   = "X-Tokay-Signature"
)

// IsReservedHeader says whether a custom header name would collide with one of
// ours. Case-insensitive, because http.Header canonicalises names and a
// subscriber writing x-tokay-event-id would otherwise win.
func IsReservedHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return lower == "content-type" || strings.HasPrefix(lower, "x-tokay-")
}

// Headers builds the request headers for one delivery. Custom headers first,
// ours LAST, so ours win: the check on the way into the configuration stops new
// collisions, but a configuration saved before that check existed may still
// carry one, and the request is where it has to lose.
func Headers(eventID string, eventType model.OutboxEventType, body []byte, secret string,
	custom map[string]string, timestamp string) map[string]string {

	h := make(map[string]string, len(custom)+5)
	for k, v := range custom {
		if IsReservedHeader(k) {
			continue
		}
		h[k] = v
	}
	h[HeaderContentType] = "application/json"
	h[HeaderEvent] = string(eventType)
	h[HeaderEventID] = eventID
	h[HeaderTimestamp] = timestamp
	if secret != "" {
		h[HeaderSignature] = "sha256=" + Sign(timestamp, body, secret)
	}
	return h
}

// Sign computes the hex HMAC-SHA256 over "<timestamp>.<body>", exactly as the
// documentation tells subscribers to verify it.
func Sign(timestamp string, body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// EffectiveTimeout is how long a subscriber is given to answer: its own setting,
// clamped to the family's ceiling, thirty seconds when it set nothing.
//
// The clamp lives here, on the value read before the call, and nowhere else.
// Preparation does not carry the timeout into the attempt, so preparation
// cannot clamp it; a value saved when the API still allowed sixty seconds
// would otherwise run to the attempt deadline and quietly break the profile
// the family's numbers are computed against. The test endpoint uses the same
// function for the same reason.
func EffectiveTimeout(cfg model.GenericWebhookConfig) time.Duration {
	policy, err := outbound.PolicyOf(outbound.FamilyWebhook)
	if err != nil {
		// The family is a compile-time constant of this build; a missing
		// policy is a wiring defect, and the ceiling is the safe answer.
		return outbound.WebhookMaxSubscriberTimeout
	}
	if cfg.TimeoutSeconds <= 0 {
		return policy.MaxSubscriberTimeout
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout > policy.MaxSubscriberTimeout {
		return policy.MaxSubscriberTimeout
	}
	return timeout
}

// privateRanges are the address ranges a subscriber may not live in unless the
// installation allows them explicitly: an outgoing webhook is a request this
// system makes on a stranger's behalf, and a stranger must not be able to point
// it at the metadata service or the database.
var privateRanges = func() []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8",
		"169.254.0.0/16", "::1/128", "fe80::/10",
	} {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		nets = append(nets, ipNet)
	}
	return nets
}()

func isPrivate(ip net.IP) bool {
	for _, cidr := range privateRanges {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// Resolver turns a host name into addresses. It is a function so a test can
// answer differently for the preparation and for the dial - which is the race
// the second check below exists for.
type Resolver func(ctx context.Context, host string) ([]net.IP, error)

// SystemResolver asks the system's resolver.
func SystemResolver(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// ipPolicy decides whether a host may be posted to: refused if ANY address it
// resolves to is private and not allowed. All addresses, not the first - a name
// with one public and one private address would otherwise pass or fail by the
// order the resolver happened to answer in.
type ipPolicy struct {
	allowed []*net.IPNet
	resolve Resolver
}

// ErrBlockedAddress is a subscriber whose address the installation forbids.
var ErrBlockedAddress = fmt.Errorf("webhook: the address is in a private range this installation does not allow")

func (p ipPolicy) allowedIP(ip net.IP) bool {
	if !isPrivate(ip) {
		return true
	}
	for _, cidr := range p.allowed {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

// check resolves the host and refuses a blocked one. A resolver failure is
// returned as is: it is a state of the network, not of the configuration, and
// the caller decides what that means where it stands.
func (p ipPolicy) check(ctx context.Context, host string) error {
	ips, err := p.resolve(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
	}
	for _, ip := range ips {
		if !p.allowedIP(ip) {
			return fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, ip)
		}
	}
	return nil
}

// dial is the second check, at the socket. Between the preparation's resolve
// and this one the name may have moved (DNS rebinding), and the guard has to
// stand where the connection is opened. It dials the address it vetted, not the
// name again.
func (p ipPolicy) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := p.resolve(ctx, host)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	if len(ips) == 0 {
		return nil, &net.OpError{Op: "dial", Net: network,
			Err: &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}}
	}
	for _, ip := range ips {
		if !p.allowedIP(ip) {
			return nil, &net.OpError{Op: "dial", Net: network,
				Err: fmt.Errorf("%w: %s resolves to %s", ErrBlockedAddress, host, ip)}
		}
	}
	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
}
