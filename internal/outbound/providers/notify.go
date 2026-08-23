// Package providers holds what every delivery channel needs in common: how a
// send is asked for, and the one lookup both of them make about a team.
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

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// NotificationTarget is who a message goes to, as this system names them.
type NotificationTarget struct {
	Kind string // "channel" | "user"
	ID   string
}

// NotificationRequest is a typed send request covering both alert cards (have
// an AlertGroup, no free-form Message) and free-form DMs/OTP/handoff (have a
// Message, no AlertGroup). Providers MUST decide behaviour from Target.Kind,
// Editable, AlertGroup and Message - NOT from Kind, which is for
// metrics/context only.
type NotificationRequest struct {
	Kind       string // step kind for metrics/context: slack_channel|slack_dm|firehose|otp|handoff
	Target     NotificationTarget
	Message    string            // free-form text (DM/OTP/handoff); empty for alert cards
	AlertGroup *model.AlertGroup // optional; present for alert cards
	Editable   bool              // true => updatable card (returns payload); false => fire-and-forget
}

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
	var recordHeader tls.RecordHeaderError
	if errors.As(err, &recordHeader) {
		return outbound.DefinitelyNotSent
	}
	var certificate *x509.CertificateInvalidError
	if errors.As(err, &certificate) {
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
