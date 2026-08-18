package dispatcher

import "log"

// TeamLookup reports whether an alert group's team is onboarded in TokayOps,
// meaning a row for it exists in the teams table. An alert group's team is a
// free-text label carried by the alert rather than a foreign key, so it
// routinely names a team that was never set up here.
type TeamLookup func(teamID string) (bool, error)

// teamIsOnboarded resolves the lookup, degrading to "onboarded" whenever it
// cannot answer: no lookup wired up, or a failing one.
//
// The direction of that degradation is the point. Deciding "not onboarded" on a
// database blip would strip the buttons from teams that are set up perfectly
// well and post a notice saying so into the channel the whole organisation
// reads, and it would do it exactly when the database is already in trouble,
// which is when alerts arrive in bulk. Falling back to the previous behaviour
// is the quiet failure.
//
// Both providers go through here rather than repeating the rule, so the two
// channels cannot drift apart.
func teamIsOnboarded(lookup TeamLookup, teamID string) bool {
	if lookup == nil {
		return true
	}
	onboarded, err := lookup(teamID)
	if err != nil {
		log.Printf("dispatcher: team lookup failed for %q, assuming onboarded: %v", teamID, err)
		return true
	}
	return onboarded
}
