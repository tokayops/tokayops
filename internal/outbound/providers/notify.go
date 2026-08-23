// Package providers holds what every delivery channel needs in common: how a
// send is asked for, and the one lookup both of them make about a team.
//
// It is deliberately thin. The channels are independent of each other - a Slack
// change must not be able to alter what Telegram sends - so what lives here is
// only what would otherwise be written twice and drift.
package providers

import (
	"log"

	"github.com/tokayops/tokayops/internal/model"
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
