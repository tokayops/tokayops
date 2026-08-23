package providers

import (
	"fmt"

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
