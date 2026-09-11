package providers

import (
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Freezing a live alert group into the shape the channels render from.
//
// The channels draw a card from a snapshot and from nothing else, which is what
// makes two attempts of one delivery produce the same bytes. The path that
// still starts from a live row - the job executors, until they are gone - needs
// somebody to take that snapshot, and this is it: everything that decides bytes
// is resolved HERE, at one moment, rather than read again halfway through a
// render.

// GroupView is a live alert group plus the things around it that end up in the
// message: where this instance lives, whether the alert's team is set up, and
// which zone times are printed in.
type GroupView struct {
	Group      *model.AlertGroup
	IsResolved bool

	// SelfURL is this instance's base URL. The links built from it are frozen
	// into the snapshot whole, because a link is part of a message and a
	// relocated instance must not change a card that was already sent.
	SelfURL string

	TeamOnboarded bool

	// Zone is an IANA name. Not a *time.Location and not the process's own
	// zone: two instances in different zones rendering one snapshot have to
	// produce the same message.
	Zone string

	// Timeline is the group's history as the producer read it, and
	// TimelineOmitted how many earlier lines the producer did not hand over.
	// Given here rather than read from the group row: the row does not carry
	// its history, and a freeze that fetched it would read at a second
	// moment. The snapshot keeps the most recent keys.TimelineLength lines and
	// counts the rest.
	Timeline        []*model.TimelineEvent
	TimelineOmitted int64

	// Interactive names the providers whose action buttons are switched on,
	// as the producer read them from the database at this moment. The card
	// draws its buttons from the snapshot this becomes, and from nowhere else.
	Interactive []string
}

// RenderInputs is what a revision freezes besides the group row: the tail of
// the history and which providers have buttons on. Read from the database at
// the moment of the freeze, by the producer of revision 0 and by every raise
// alike, and handed to the freeze rather than fetched by it.
type RenderInputs struct {
	Timeline        []*model.TimelineEvent
	TimelineOmitted int64
	Interactive     []string
}

// HistoryOf is the render projection of timeline rows. The type is carried
// across verbatim, like the statuses: one this build cannot name reaches
// admission and is refused there rather than becoming a line about something
// else. A nil row is skipped.
func HistoryOf(events []*model.TimelineEvent) []keys.TimelineEventSnapshot {
	var lines []keys.TimelineEventSnapshot
	for _, event := range events {
		if event == nil {
			continue
		}
		lines = append(lines, keys.TimelineEventSnapshot{
			ID:        event.ID,
			Type:      keys.TimelineEventType(event.Type),
			Message:   event.Message,
			Actor:     optional(event.Actor),
			CreatedAt: event.CreatedAt,
		})
	}
	return lines
}

// SnapshotOf freezes what a card is drawn from, canonical and validated - the
// form a delivery is admitted and keyed under.
func SnapshotOf(view GroupView) (keys.RenderSnapshot, error) {
	return keys.NewRenderSnapshot(ViewOf(view))
}

// ViewOf is the same freeze without the validation: it puts the fields where a
// renderer can find them and states nothing about whether they are admissible.
// What it produces goes to SnapshotOf, which decides that.
func ViewOf(view GroupView) keys.SnapshotInput {
	if view.Group == nil {
		return keys.SnapshotInput{Status: keys.GroupProcessing, DisplayTimezone: "UTC"}
	}
	group := view.Group

	in := keys.SnapshotInput{
		AlertGroupID:    group.ID,
		Status:          groupStatus(group.Status, view.IsResolved),
		Title:           group.Title,
		Severity:        group.Severity,
		TeamOnboarded:   view.TeamOnboarded,
		DisplayTimezone: zoneName(view.Zone),
	}
	if group.TeamID != "" {
		label := group.TeamID
		in.TeamLabel = &label
	}
	if group.ExternalURL != "" {
		external := group.ExternalURL
		in.ExternalURL = &external
	}
	if group.AcknowledgedBy != "" {
		by := group.AcknowledgedBy
		in.AcknowledgedBy = &by
	}
	if group.ResolvedBy != "" {
		by := group.ResolvedBy
		in.ResolvedBy = &by
	}
	if view.SelfURL != "" {
		groupURL := fmt.Sprintf("%s/#/ops/alert-groups/%s", view.SelfURL, group.ID)
		teamSetupURL := view.SelfURL + "/#/cfg/teams"
		in.GroupURL = &groupURL
		in.TeamSetupURL = &teamSetupURL
	}

	for _, alert := range group.Alerts {
		in.Alerts = append(in.Alerts, alertSnapshot(alert, group.CreatedAt))
	}

	// The history, as handed over.
	in.Timeline = HistoryOf(view.Timeline)
	in.TimelineOmitted = view.TimelineOmitted
	in.InteractiveProviders = append([]string(nil), view.Interactive...)
	return in
}

// alertSnapshot resolves the label-or-annotation fallbacks once, so a renderer
// reads a field instead of guessing which map an operator put a URL in.
//
// An alert that arrived without a start time gets the moment this system first
// saw its group. That is a fact of our own record rather than an invention, and
// the alternative is worse than inexact: a payload that omitted startsAt would
// make the whole escalation unadmittable, and nobody would be paged because a
// field nobody reads was empty. Identity is not filled in the same way - a
// fingerprint cannot be invented, and admission refuses without one.
func alertSnapshot(alert model.Alert, firstSeen time.Time) keys.AlertSnapshot {
	out := keys.AlertSnapshot{
		Fingerprint: alert.Fingerprint,
		Status:      alertStatus(alert.Status),
		StartsAt:    alert.StartsAt,
		AlertName:   alert.Labels["alertname"],
		Severity:    alert.Labels["severity"],
	}

	if out.StartsAt.IsZero() {
		out.StartsAt = firstSeen
	}

	out.SlackUser = optional(alert.Labels["slack_user"])
	dashboard := alert.Annotations["dashboard"]
	if dashboard == "" {
		dashboard = alert.Labels["dashboard"]
	}
	out.DashboardURL = optional(dashboard)
	out.RunbookURL = optional(alert.Annotations["runbook"])

	description := alert.Annotations["description"]
	if description == "" {
		description = alert.Annotations["summary"]
	}
	out.Description = optional(description)
	return out
}

// groupStatus maps the live status, with "being resolved right now" beating
// whatever the row still says: the resolve path renders the closing card before
// the status change is visible.
//
// A status this build does not know is carried across verbatim rather than
// turned into "processing". It then fails admission, which is the point: a
// substitution here would send a message about a state the alert was never in,
// under a digest saying that is what was accepted.
func groupStatus(status model.AlertGroupStatus, isResolved bool) keys.GroupStatus {
	if isResolved {
		return keys.GroupResolved
	}
	switch status {
	case model.AlertGroupStatusNew:
		return keys.GroupNew
	case model.AlertGroupStatusProcessing:
		return keys.GroupProcessing
	case model.AlertGroupStatusTriggered:
		return keys.GroupTriggered
	case model.AlertGroupStatusAcknowledged:
		return keys.GroupAcknowledged
	case model.AlertGroupStatusResolved:
		return keys.GroupResolved
	case model.AlertGroupStatusClosed:
		return keys.GroupClosed
	default:
		return keys.GroupStatus(status)
	}
}

// alertStatus maps what an alert is doing, and carries an unknown value across
// rather than calling it firing: "firing" is a claim about the world.
func alertStatus(status model.AlertStatus) keys.AlertStatus {
	switch status {
	case model.AlertStatusFiring:
		return keys.AlertFiring
	case model.AlertStatusResolved:
		return keys.AlertResolved
	default:
		return keys.AlertStatus(status)
	}
}

// zoneName refuses "Local", which is not a zone but a question about the
// machine asking. A snapshot that carried it would render differently on every
// instance, which is the one thing it exists to prevent.
func zoneName(zone string) string {
	if zone == "" || zone == "Local" {
		return "UTC"
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return "UTC"
	}
	return zone
}

// ProcessZone is the zone the legacy path prints times in: whatever this
// process was told to use, and UTC when it was told nothing.
//
// It exists only for that path. Everything admitted through the outbound domain
// carries the zone in its snapshot, decided once by whoever composed it.
func ProcessZone() string { return zoneName(time.Local.String()) }

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
