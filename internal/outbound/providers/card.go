package providers

import (
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
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
	Actor string // e.g. "✅ Resolved by Denis" — shown as separate line
}

// ResolveStatus determines the title text and colour bar from the state of an
// alert group.
func ResolveStatus(ag *model.AlertGroup, isResolved bool, firing int) MessageStatus {
	if isResolved {
		s := MessageStatus{
			Title: fmt.Sprintf("✅ Resolved: %s", ag.Title),
			Color: "#36a64f",
		}
		if ag.ResolvedBy != "" {
			s.Actor = fmt.Sprintf("✅ Resolved by %s", ag.ResolvedBy)
		}
		return s
	}
	if ag.Status == model.AlertGroupStatusAcknowledged {
		s := MessageStatus{
			Title: fmt.Sprintf("⏸️ Acknowledged: %s (%d Firing)", ag.Title, firing),
			Color: "#FFA500",
		}
		if ag.AcknowledgedBy != "" {
			s.Actor = fmt.Sprintf("⏸️ Acknowledged by %s", ag.AcknowledgedBy)
		}
		return s
	}
	return MessageStatus{
		Title: fmt.Sprintf("🔥 Alert: %s (%d Firing)", ag.Title, firing),
		Color: "#FF0000",
	}
}

// CountFiring returns the number of firing alerts.
func CountFiring(alerts []model.Alert) int {
	n := 0
	for _, a := range alerts {
		if a.Status == model.AlertStatusFiring {
			n++
		}
	}
	return n
}
