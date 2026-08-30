// Package providers holds what every delivery channel needs in common: how long
// a name may be, what the channels of this build can do, and how a network
// answer is read.
//
// It is deliberately thin. The channels are independent of each other - a Slack
// change must not be able to alter what Telegram sends - so what lives here is
// only what would otherwise be written twice and drift.
package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"syscall"

	"github.com/tokayops/tokayops/internal/outbound"
)

// TeamLookup reports whether an alert group's team is onboarded in TokayOps,
// meaning a row for it exists in the teams table. An alert group's team is a
// free-text label carried by the alert rather than a foreign key, so it
// routinely names a team that was never set up here.
type TeamLookup func(teamID string) (bool, error)

// TeamIsOnboarded resolves the lookup, degrading to "onboarded" whenever it
// cannot answer: no lookup wired up, or a failing one.
//
// The direction of that degradation is the point. Deciding "not onboarded" on a
// database blip would strip the buttons from teams that are set up perfectly
// well and post a notice saying so into the channel the whole organisation
// reads, and it would do it exactly when the database is already in trouble,
// which is when alerts arrive in bulk. Falling back to the previous behaviour
// is the quiet failure.
//
// Both channels go through here rather than repeating the rule, so the two
// cannot drift apart.
func TeamIsOnboarded(lookup TeamLookup, teamID string) bool {
	if lookup == nil {
		return true
	}
	onboarded, err := lookup(teamID)
	if err != nil {
		log.Printf("providers: team lookup failed for %q, assuming onboarded: %v", teamID, err)
		return true
	}
	return onboarded
}

// IdentityLookup turns a person this system knows about into the address one
// provider knows them by.
//
// It is a function rather than an interface because that is all it is, and
// because the thing that implements it - a row in external_identities - belongs
// to a package the channels must not import.
type IdentityLookup func(ctx context.Context, userID, provider string) (string, error)

// ErrNotLinked says the person has no address on that provider. It is not a
// failure to retry: nobody's account links itself, so the delivery ends where a
// person can see it.
var ErrNotLinked = errors.New("providers: no linked identity for this provider")

// EvidenceOf says what a transport error proves about the request.
//
// The default is deliberately the expensive one. A request that may have gone
// out and is then called a clean failure gets retried, and the retry is a
// second page nobody can explain; a request that never left and is called
// doubtful costs one ambiguous record. So only the failures that happen BEFORE
// anything is written - no route, no connection, no handshake - are certain,
// and everything else is doubt.
func EvidenceOf(err error) outbound.Evidence {
	if err == nil {
		return outbound.ProviderResponse
	}

	var dns *net.DNSError
	if errors.As(err, &dns) {
		return outbound.DefinitelyNotSent
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return outbound.DefinitelyNotSent
	}
	// The handshake. A certificate that does not verify is the commonest one
	// and it arrives as tls.CertificateVerificationError; the x509 errors below
	// are the ones a caller doing its own verification can still surface, and
	// they are VALUES rather than pointers - matched as pointers, as the
	// previous version did, none of them ever fired and every expired
	// certificate was recorded as a message that might have been delivered.
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return outbound.DefinitelyNotSent
	}
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return outbound.DefinitelyNotSent
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return outbound.DefinitelyNotSent
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return outbound.DefinitelyNotSent
	}
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return outbound.DefinitelyNotSent
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return outbound.DefinitelyNotSent
	}

	// Timeouts, resets, and anything unrecognised: the request may have been
	// written and answered by a provider that never got to tell us.
	return outbound.PossiblySent
}
