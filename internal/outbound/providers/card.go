package providers

import (
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// MessageStatus is what an alert group looks like right now: the line at the
// top of a card, the colour beside it, and who acted if anybody did.
//
// It is here rather than in one of the channels because both render it, and a
// copy in each is two cards that eventually disagree about what "acknowledged"
// says.
type MessageStatus struct {
	Title string
	Color string
	Actor string // e.g. "✅ Resolved by Denis" - shown as a separate line
}

// ResolveStatus determines the title text and colour bar from a snapshot.
//
// It takes the snapshot rather than a live alert group and a flag: whether this
// card is the closing one is part of what was frozen, so two attempts of one
// delivery cannot disagree about it.
func ResolveStatus(state keys.SnapshotInput) MessageStatus {
	firing := CountFiring(state.Alerts)

	switch state.Status {
	case keys.GroupResolved, keys.GroupClosed:
		s := MessageStatus{
			Title: fmt.Sprintf("✅ Resolved: %s", state.Title),
			Color: "#36a64f",
		}
		if state.ResolvedBy != nil && *state.ResolvedBy != "" {
			s.Actor = fmt.Sprintf("✅ Resolved by %s", *state.ResolvedBy)
		}
		return s

	case keys.GroupAcknowledged:
		s := MessageStatus{
			Title: fmt.Sprintf("⏸️ Acknowledged: %s (%d Firing)", state.Title, firing),
			Color: "#FFA500",
		}
		if state.AcknowledgedBy != nil && *state.AcknowledgedBy != "" {
			s.Actor = fmt.Sprintf("⏸️ Acknowledged by %s", *state.AcknowledgedBy)
		}
		return s

	default:
		return MessageStatus{
			Title: fmt.Sprintf("🔥 Alert: %s (%d Firing)", state.Title, firing),
			Color: "#FF0000",
		}
	}
}

// CountFiring returns the number of firing alerts.
func CountFiring(alerts []keys.AlertSnapshot) int {
	n := 0
	for _, a := range alerts {
		if a.Status == keys.AlertFiring {
			n++
		}
	}
	return n
}

// AlertDescription is what an alert says is wrong. Empty when the alert carries
// nothing to say.
//
// It does not shorten anything: the bound is keys.AlertDescriptionLimit and it
// is applied when the snapshot is canonicalised, before the digest. Cutting
// here instead would let two snapshots that render identically hold different
// digests - a revision raised, and a real edit sent, for a difference nobody
// can see.
func AlertDescription(a keys.AlertSnapshot) string {
	if a.Description == nil {
		return ""
	}
	return *a.Description
}

// AlertStartedAt is when the alert began, printed in the snapshot's zone.
//
// The zone comes from the snapshot and never from the process: two instances
// rendering one revision have to produce the same bytes, and the second attempt
// of a delivery has to produce the same message as the first.
//
// The offset is printed in full rather than as whole hours. Half-hour zones
// exist, and "GMT+5" for Asia/Kolkata is a time that is wrong by thirty
// minutes rather than a shorter way of saying the right one.
func AlertStartedAt(a keys.AlertSnapshot, zone string) string {
	local := a.StartsAt.In(DisplayZone(zone))
	return local.Format("2006-01-02 15:04") + " GMT" + local.Format("-07:00")
}

// DisplayZone resolves a snapshot's zone name. A snapshot cannot hold one that
// does not load - the protocol refuses it - so the fallback is for the paths
// that render a live row rather than an admitted one.
func DisplayZone(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}
