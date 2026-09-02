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

// A subscriber may live only at a public address, unless the installation
// allows the range explicitly: an outgoing webhook is a request this system
// makes on a stranger's behalf, and a stranger must not be able to point it at
// the metadata service, the database, or anything else this process can reach
// and they cannot.
//
// "Public" is a POSITIVE classification, and for IPv6 that is the load-bearing
// half. IPv4 space is fully allocated, so there "public" is the whole space
// minus the closed IANA special-purpose registry. IPv6 is mostly unallocated:
// only 2000::/3 is global unicast, and everything outside it - site-local,
// the SRv6 SID space, the local-use NAT64 prefix, the unallocated sevenths of
// the space - is refused without being named. A rule that ended in "anything
// else is public" was still a denylist, and IANA keeps adding to what it had
// not named.
func isPublic(ip net.IP) bool {
	if len(ip) == 0 {
		return false
	}
	// An IPv4 address carried inside an IPv6 one - mapped, or embedded by a
	// translation prefix - is judged as the IPv4 address it names: through
	// NAT64 or 6to4, 64:ff9b::a00:7 is 10.0.0.7. Only the well-known NAT64
	// prefix embeds predictably; the local-use one (64:ff9b:1::/48) puts the
	// IPv4 wherever the network chose, cannot be judged, and falls outside
	// 2000::/3 below.
	if v4 := embeddedIPv4(ip); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		for _, r := range reservedIPv4 {
			if r.Contains(v4) {
				return false
			}
		}
		return true
	}
	// An IPv6 address that names no IPv4: public means allocated global
	// unicast, minus the special-purpose ranges carved out inside it.
	if !globalUnicastV6.Contains(ip) {
		return false
	}
	for _, r := range reservedIPv6 {
		if r.Contains(ip) {
			return false
		}
	}
	return true
}

// reservedIPv4 are the special-purpose IPv4 ranges of the IANA registry that
// are not globally reachable and that net.IP has no predicate for.
var reservedIPv4 = parseCIDRs(
	"0.0.0.0/8",       // "this" network
	"100.64.0.0/10",   // shared address space (RFC 6598)
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.88.99.0/24",  // 6to4 relay anycast, deprecated
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"240.0.0.0/4",     // reserved, and the broadcast address
)

// globalUnicastV6 is the one third of the IPv6 space IANA allocates global
// unicast from. Nothing outside it is a public host address.
var globalUnicastV6 = parseCIDRs("2000::/3")[0]

// reservedIPv6 are the special-purpose ranges INSIDE 2000::/3 that are not
// globally reachable (or, like Teredo, are tunneling machinery no subscriber
// lives at). 2002::/16 is absent on purpose: a 6to4 address is judged as the
// IPv4 it embeds. The globally reachable carve-outs - the PCP, TURN, AMT and
// AS112 anycasts - are deliberately not here.
var reservedIPv6 = parseCIDRs(
	"2001::/32",     // Teredo
	"2001:2::/48",   // benchmarking
	"2001:10::/28",  // ORCHID, deprecated
	"2001:20::/28",  // ORCHIDv2
	"2001:30::/28",  // drone remote ID
	"2001:db8::/32", // documentation
	"3fff::/20",     // documentation
)

var (
	nat64WellKnown = parseCIDRs("64:ff9b::/96")[0]
	sixToFour      = parseCIDRs("2002::/16")[0]
)

// embeddedIPv4 is the IPv4 address an address carries, or nil: an IPv4 address
// itself, an IPv4-mapped IPv6 address, or the address a translation prefix
// embeds.
func embeddedIPv4(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	if len(ip) != net.IPv6len {
		return nil
	}
	switch {
	case nat64WellKnown.Contains(ip):
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	case sixToFour.Contains(ip):
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}

func parseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err)
		}
		nets = append(nets, ipNet)
	}
	return nets
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
// resolves to is not public and not allowed. All addresses, not the first - a
// name with one public and one private address would otherwise pass or fail by
// the order the resolver happened to answer in.
type ipPolicy struct {
	allowed []*net.IPNet
	resolve Resolver
}

// ErrBlockedAddress is a subscriber whose address the installation forbids.
var ErrBlockedAddress = fmt.Errorf("webhook: the address is not public and this installation does not allow it")

// allowedIP: public, or inside a range the installation allowed. The allowlist
// is judged over the same address the policy judged, so an allowed IPv4 range
// covers the address whichever way it is written.
func (p ipPolicy) allowedIP(ip net.IP) bool {
	if isPublic(ip) {
		return true
	}
	judged := ip
	if v4 := embeddedIPv4(ip); v4 != nil {
		judged = v4
	}
	for _, cidr := range p.allowed {
		if cidr.Contains(ip) || cidr.Contains(judged) {
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
