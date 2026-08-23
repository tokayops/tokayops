package dispatcher

import "github.com/tokayops/tokayops/internal/outbound/providers"

// TeamLookup is the channels' lookup, re-exported here because the dispatcher
// is what wires one up and hands it to them.
type TeamLookup = providers.TeamLookup

// teamIsOnboarded is the same rule the channels use.
func teamIsOnboarded(lookup TeamLookup, teamID string) bool {
	return providers.TeamIsOnboarded(lookup, teamID)
}
