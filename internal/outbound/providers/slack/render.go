package slack

import (
	"fmt"
	"sort"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/slackcard"
)

// Drawing a Slack card, as a function of the snapshot and nothing else.
//
// Nothing here reads configuration, asks the database whether a team exists, or
// looks at what zone this process is in. All three used to happen mid-render,
// and all three meant the same thing: a retry of a delivery could produce
// different bytes than the attempt before it - a different card under the same
// provider key, with no way to tell afterwards which one somebody saw.
//
// What the message may depend on is therefore fixed here by construction: the
// snapshot for everything about the alert, and one flag for whether this
// channel was admitted with buttons. Layout, icons and block order are exactly
// what they were.

// Render is the whole card: the fallback text, the title blocks Slack unfurls
// preview, and the coloured attachment.
func Render(state keys.SnapshotInput, interactive bool) slackcard.Card {
	return slackcard.Card{
		Text:       plainTitle(state),
		Blocks:     renderTitleBlocks(state),
		Attachment: renderBodyAttachment(state, interactive),
	}
}

// plainTitle returns the unformatted title for use as the top-level message
// text (notification preview / accessibility fallback).
func plainTitle(state keys.SnapshotInput) string {
	return providers.ResolveStatus(state).Title
}

// renderTitleBlocks returns the title section as top-level blocks. Used as
// both the in-channel header (above the colored attachment) and the unfurl
// preview content - Slack message-link unfurls render only top-level blocks.
func renderTitleBlocks(state keys.SnapshotInput) []slackapi.Block {
	status := providers.ResolveStatus(state)

	titleText := status.Title
	if state.ExternalURL != nil && *state.ExternalURL != "" {
		titleText = fmt.Sprintf("<%s|%s>", *state.ExternalURL, status.Title)
	}
	return []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "*"+titleText+"*", false, false),
			nil, nil,
		),
	}
}

// renderBodyAttachment returns the colored attachment containing
// severity/alerts, optional action buttons, and footer. The title is NOT
// included - it lives in the top-level blocks (renderTitleBlocks) so Slack
// message-link unfurls can preview it.
func renderBodyAttachment(state keys.SnapshotInput, interactive bool) slackapi.Attachment {
	status := providers.ResolveStatus(state)
	mentions := collectMentions(state.Alerts)

	var blocks []slackapi.Block

	// Severity + alerts
	bodyText := ""
	if mentions != "" {
		bodyText = mentions + "\n\n"
	}
	bodyText += fmt.Sprintf("*Severity:* %s\n", state.Severity)

	alertList := buildAlertList(state.Alerts, state.DisplayTimezone)
	if len(state.Alerts) == 0 {
		alertList = "• " + state.Title
	}
	bodyText += "*Alerts:*\n" + alertList
	bodyText = truncateText(bodyText, 3000)
	blocks = append(blocks, slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, bodyText, false, false),
		nil, nil,
	))

	// Action buttons, on the same three conditions as before - only now all
	// three are properties of the snapshot and the admission rather than of
	// this instant. A resolved card never carries buttons; a channel admitted
	// without interactivity never gets them; and a team TokayOps does not have
	// gets the notice where the buttons would be.
	resolved := state.Status == keys.GroupResolved || state.Status == keys.GroupClosed
	actionable := interactive && !resolved
	showAck := actionable && state.TeamOnboarded && state.Status != keys.GroupAcknowledged
	showResolve := actionable && state.TeamOnboarded

	if showAck || showResolve {
		blocks = append(blocks, slackapi.NewDividerBlock())

		var buttons []slackapi.BlockElement
		if showAck {
			ackBtn := slackapi.NewButtonBlockElement(
				model.SlackActionAckAlertGroup, state.AlertGroupID,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Acknowledge", true, false),
			)
			ackBtn.WithStyle(slackapi.StyleDanger)
			buttons = append(buttons, ackBtn)
		}
		if showResolve {
			resolveBtn := slackapi.NewButtonBlockElement(
				model.SlackActionResolveAlertGroup, state.AlertGroupID,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Resolve", true, false),
			)
			buttons = append(buttons, resolveBtn)
		}
		blocks = append(blocks, slackapi.NewActionBlock("alert_actions", buttons...))
	} else if actionable && !state.TeamOnboarded {
		// Where the buttons would have been, say why they are not there. The
		// claim is deliberately limited to what the lookup proved when the
		// snapshot was taken: the team is absent, so nobody can act on this
		// from Slack. It says nothing about escalation, because
		// alert_groups.team_id has no foreign key and an escalation started
		// before the team was deleted keeps running.
		blocks = append(blocks, slackapi.NewDividerBlock())
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, unknownTeamNotice(state), false, false),
			nil, nil,
		))
	}

	// Context footer
	footerText := fmt.Sprintf("ID: %s", state.AlertGroupID)
	if state.GroupURL != nil && *state.GroupURL != "" {
		footerText = fmt.Sprintf("ID: <%s|%s>", *state.GroupURL, state.AlertGroupID)
	}
	blocks = append(blocks, slackapi.NewContextBlock("alert_footer",
		slackapi.NewTextBlockObject(slackapi.MarkdownType, footerText, false, false),
	))

	return slackapi.Attachment{
		Color:    status.Color,
		Fallback: status.Title,
		Blocks:   slackapi.Blocks{BlockSet: blocks},
	}
}

// collectMentions returns a mrkdwn string of Slack user/group mentions from all
// alerts.
//
// Sorted, and that is not cosmetic. The set was a map and the joining walked
// it, so one snapshot produced a different string on every render - a card
// whose bytes changed between two attempts of the same delivery for no reason
// anybody could see.
func collectMentions(alerts []keys.AlertSnapshot) string {
	seen := make(map[string]bool)
	for _, a := range alerts {
		if a.SlackUser != nil && *a.SlackUser != "" {
			seen[*a.SlackUser] = true
		}
	}
	if len(seen) == 0 {
		return ""
	}

	users := make([]string, 0, len(seen))
	for u := range seen {
		users = append(users, u)
	}
	sort.Strings(users)

	var parts []string
	for _, u := range users {
		if strings.HasPrefix(u, "S") {
			parts = append(parts, fmt.Sprintf("<!subteam^%s>", u))
		} else {
			parts = append(parts, fmt.Sprintf("<@%s>", u))
		}
	}
	return strings.Join(parts, " ")
}

// buildAlertList returns a mrkdwn bullet list of alerts (max 10), each with a
// second line saying what is wrong and since when.
//
// That second line is where the alert's description lives now. It used to be in
// the card's thread, in a message of its own, and the thread went away with the
// rule that one attempt performs one external effect. Nothing carried the
// content over, which left the one field that says what actually broke reaching
// no message at all.
func buildAlertList(alerts []keys.AlertSnapshot, zone string) string {
	const maxAlerts = 10
	alertList := ""
	rendered := 0

	for _, a := range alerts {
		if rendered >= maxAlerts {
			break
		}
		rendered++

		dashLink := ""
		if a.DashboardURL != nil && *a.DashboardURL != "" {
			dashLink = fmt.Sprintf(" <%s|[dash]>", *a.DashboardURL)
		}
		bookLink := ""
		if a.RunbookURL != nil && *a.RunbookURL != "" {
			bookLink = fmt.Sprintf(" <%s|[runbook]>", *a.RunbookURL)
		}

		if a.Status == keys.AlertFiring {
			alertList += fmt.Sprintf("• 🔴 %s (Sev: %s)%s%s\n", a.AlertName, a.Severity, dashLink, bookLink)
		} else {
			alertList += fmt.Sprintf("• 🟢 %s (Resolved)%s%s\n", a.AlertName, dashLink, bookLink)
		}
		alertList += "   _" + alertDetail(a, zone) + "_\n"
	}
	if remaining := len(alerts) - maxAlerts; remaining > 0 {
		alertList += fmt.Sprintf("_... and %d more alerts_\n", remaining)
	}
	return alertList
}

// alertDetail is the second line: the description when there is one, and the
// moment the alert started, which there always is.
func alertDetail(a keys.AlertSnapshot, zone string) string {
	since := "since " + providers.AlertStartedAt(a, zone)
	if description := providers.AlertDescription(a); description != "" {
		return description + " · " + since
	}
	return since
}

// truncateText truncates s to fit within maxLen bytes (including suffix),
// cutting at a newline boundary to avoid splitting mrkdwn constructs like
// <url|label>.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const suffix = "\n_... truncated_\n"
	if maxLen <= len(suffix) {
		// Edge case: maxLen too small for suffix - hard-cut at rune boundary
		runes := []rune(s)
		for len(string(runes)) > maxLen {
			runes = runes[:len(runes)-1]
		}
		return string(runes)
	}
	// Cut at last newline before the byte budget to keep lines intact
	cutAt := maxLen - len(suffix)
	body := s[:cutAt]
	if idx := strings.LastIndex(body, "\n"); idx > 0 {
		body = body[:idx]
	}
	return body + suffix
}

// maxTeamLabelLen bounds the alert's team label before it is put into a card.
// The label is free text carried by the alert, and a section block has a hard
// size limit; a team name longer than this is malformed anyway.
const maxTeamLabelLen = 80

// labelSanitizer neutralises the characters that would break out of the
// notice's markup. The backtick closes the code span the label sits in and the
// line breaks split the block; the other three are mrkdwn-significant. It runs
// as a single pass, so nothing is escaped twice.
var labelSanitizer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	"`", "'",
	"\r", " ",
	"\n", " ",
)

// sanitizeTeamLabel makes an alert's team label safe to interpolate into card
// mrkdwn. Truncation happens BEFORE escaping, so a cut can never sever an
// entity and leave "&am" behind.
func sanitizeTeamLabel(teamID string) string {
	if r := []rune(teamID); len(r) > maxTeamLabelLen {
		teamID = string(r[:maxTeamLabelLen]) + "…"
	}
	return labelSanitizer.Replace(teamID)
}

// unknownTeamNotice is what stands in for the action buttons when the alert
// group names a team TokayOps does not have.
func unknownTeamNotice(state keys.SnapshotInput) string {
	label := ""
	if state.TeamLabel != nil {
		label = *state.TeamLabel
	}
	notice := fmt.Sprintf(
		"*⚠️ Unknown team `%s`*\nTokayOps has no such team, so this alert cannot be acknowledged or resolved from Slack.",
		sanitizeTeamLabel(label),
	)
	if state.TeamSetupURL != nil && *state.TeamSetupURL != "" {
		notice += fmt.Sprintf(" <%s|Set up the team>", *state.TeamSetupURL)
	}
	return notice
}
