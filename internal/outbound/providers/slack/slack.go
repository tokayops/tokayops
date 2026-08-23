package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/slackcard"
)

// ErrUserNotFound means the email has no matching Slack account.
var ErrUserNotFound = errors.New("slack user not found")

// HTTPTimeout bounds a single Slack API call.
//
// Without it slack-go keeps the &http.Client{} it builds itself, whose Timeout
// is zero, so a black-holed connection is not a slow call but a permanent one.
// It costs two things that do not repair themselves: the goroutine of the job
// step that made it never returns, and the usergroup syncer - which has its own
// client, no lease over it and no retry - simply stops syncing until the
// process is restarted.
//
// It bounds a CALL, not a delivery, and duplicates are not what it fixes.
// sendCard makes three calls, so a step can still outlive the 60s job lease and
// be re-claimed; and a timeout can fire on a request Slack already accepted,
// which the retry then sends again. Both are accepted - see the register.
const HTTPTimeout = 30 * time.Second

// NewClient is the only place a Slack client is built, so the timeout
// cannot be forgotten by the next caller that needs one.
//
// The timeout is a parameter rather than read from the constant here so a test
// can prove the option reaches the client in milliseconds instead of thirty
// seconds; opts is what lets that test point the client at a server of its own.
func NewClient(token string, timeout time.Duration, opts ...slackapi.Option) *slackapi.Client {
	return slackapi.New(token, append(opts, slackapi.OptionHTTPClient(&http.Client{Timeout: timeout}))...)
}

// TokenSource provides dynamic token and config lookup
type TokenSource interface {
	GetSlackToken() string
	GetSlackInteractive() bool
}

type Provider struct {
	tokenSource TokenSource
	selfURL     string               // TokayOps base URL for deep links
	teamLookup  providers.TeamLookup // nil means "assume onboarded", see teamIsOnboarded
	mu          sync.Mutex
	cachedToken string
	client      *slackapi.Client
}

// Data is the opaque provider payload Slack stores in a delivery row. It
// holds the coordinates of the posted message (channel + timestamp), the timeline
// thread timestamp, and a permalink for DM "Open in Slack" links.
type Data struct {
	ChannelID         string `json:"channel_id"`
	Timestamp         string `json:"timestamp"`
	TimelineTimestamp string `json:"timeline_ts,omitempty"`
	Permalink         string `json:"permalink,omitempty"`
}

func parseData(raw string) (*Data, bool) {
	if raw == "" {
		return nil, false
	}

	var data Data
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, false
	}
	if data.ChannelID == "" || data.Timestamp == "" {
		return nil, false
	}
	return &data, true
}

func NewProvider(tokenSource TokenSource, selfURL string, teamLookup providers.TeamLookup) *Provider {
	return &Provider{
		tokenSource: tokenSource,
		selfURL:     selfURL,
		teamLookup:  teamLookup,
	}
}

// RenderCard returns the full card payload for Slack message rendering/replacement.
func (s *Provider) RenderCard(ag *model.AlertGroup, isResolved bool) slackcard.Card {
	return slackcard.Card{
		Text:       s.plainTitle(ag, isResolved),
		Blocks:     s.renderTitleBlocks(ag, isResolved),
		Attachment: s.renderBodyAttachment(ag, isResolved),
	}
}

// ErrNoToken is returned when Slack token is not configured
var ErrNoToken = fmt.Errorf("slack integration not configured (no token)")

// (ErrNoSlackUserID was removed in Epic 7 Sprint 3 — the dispatcher-level
// ErrIdentityNotLinked in identity.go covers it generically.)

// getClient returns a Slack client, recreating it if the token changed.
// Returns error if no token is configured or tokenSource is nil.
func (s *Provider) getClient() (*slackapi.Client, error) {
	if s.tokenSource == nil {
		return nil, ErrNoToken
	}
	token := s.tokenSource.GetSlackToken()
	if token == "" {
		return nil, ErrNoToken
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil || s.cachedToken != token {
		if s.client != nil && s.cachedToken != token {
			log.Printf("Provider: Token changed, recreating client")
		}
		s.cachedToken = token
		s.client = NewClient(token, HTTPTimeout)
	}
	return s.client, nil
}

// SendDM is a thin convenience wrapper over Send for callers that hold a
// concrete *Provider and only need a fire-and-forget DM (internal/api OTP
// and integration handlers). It is NOT part of dispatcher.Provider.
func (s *Provider) SendDM(ctx context.Context, userID, message string) error {
	_, err := s.Send(ctx, providers.NotificationRequest{
		Kind:     "slack_dm",
		Target:   providers.NotificationTarget{Kind: "user", ID: userID},
		Message:  message,
		Editable: false,
	})
	return err
}

// sendDM opens a direct message channel with the user and sends a message.
func (s *Provider) sendDM(ctx context.Context, userID, message string) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}

	params := &slackapi.OpenConversationParameters{
		Users: []string{userID},
	}
	channel, _, _, err := client.OpenConversationContext(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to open conversation: %w", err)
	}

	_, _, err = client.PostMessageContext(ctx, channel.ID, slackapi.MsgOptionText(message, false))
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}

// GetSlackUserIDByEmail looks up a Slack user by email via the users.lookupByEmail API.
// Returns the Slack user ID if found.
func (s *Provider) GetSlackUserIDByEmail(ctx context.Context, email string) (string, error) {
	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	user, err := client.GetUserByEmailContext(ctx, email)
	if err != nil {
		var slackErr slackapi.SlackErrorResponse
		if errors.As(err, &slackErr) && slackErr.Err == "users_not_found" {
			return "", ErrUserNotFound
		}
		return "", fmt.Errorf("slack users.lookupByEmail: %w", err)
	}

	return user.ID, nil
}

// GetEmailBySlackID looks up a Slack user's email via the users.info API.
// Returns the email if the user is found and has a profile email set.
func (s *Provider) GetEmailBySlackID(ctx context.Context, slackUserID string) (string, error) {
	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	user, err := client.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		var slackErr slackapi.SlackErrorResponse
		if errors.As(err, &slackErr) && slackErr.Err == "user_not_found" {
			return "", ErrUserNotFound
		}
		return "", fmt.Errorf("slack users.info: %w", err)
	}

	if user.Profile.Email == "" {
		return "", fmt.Errorf("slack user %s has no email in profile", slackUserID)
	}
	return user.Profile.Email, nil
}

// Send dispatches on the target kind: a "user" target is a fire-and-forget DM
// (returns no payload), a "channel" target posts an editable alert card and
// returns its Data payload. Behaviour keys on Target.Kind / AlertGroup /
// Message — never on req.Kind. An unknown kind is rejected rather than silently
// treated as a channel card.
func (s *Provider) Send(ctx context.Context, req providers.NotificationRequest) (string, error) {
	switch req.Target.Kind {
	case "user":
		if req.Message == "" {
			return "", fmt.Errorf("slack: user send requires a message")
		}
		return "", s.sendDM(ctx, req.Target.ID, req.Message)
	case "channel":
		if req.AlertGroup == nil {
			return "", fmt.Errorf("slack: channel send requires an alert group")
		}
		return s.sendCard(ctx, req.Target.ID, req.AlertGroup)
	default:
		return "", fmt.Errorf("slack: unsupported target kind %q", req.Target.Kind)
	}
}

// sendCard posts an alert-group card (title blocks + colored attachment) to a
// channel, plus the timeline as a threaded reply, and returns the Data
// payload needed to update/resolve it later.
func (s *Provider) sendCard(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
	if ag == nil {
		return "", fmt.Errorf("slack: channel send requires an alert group")
	}
	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	attachment := s.renderBodyAttachment(ag, false)
	title := s.renderTitleBlocks(ag, false)

	channelID, timestamp, err := client.PostMessageContext(ctx, targetID,
		slackapi.MsgOptionText(s.plainTitle(ag, false), false),
		slackapi.MsgOptionBlocks(title...),
		slackapi.MsgOptionAttachments(attachment),
	)
	if err != nil {
		return "", err
	}
	// The editable card's payload must carry valid coordinates; without them the
	// delivery row would be unusable for Update/Resolve.
	if channelID == "" || timestamp == "" {
		return "", fmt.Errorf("slack: postMessage returned empty channel/timestamp (channel=%q ts=%q)", channelID, timestamp)
	}

	permalink := ""
	if channelID != "" && timestamp != "" {
		link, err := client.GetPermalinkContext(ctx, &slackapi.PermalinkParameters{
			Channel: channelID,
			Ts:      timestamp,
		})
		if err != nil {
			log.Printf("Provider: Failed to get permalink for %s: %v", channelID, err)
		} else {
			permalink = link
		}
	}

	log.Printf("Provider: Sent message to %s (ts: %s)", channelID, timestamp)

	// Post Timeline to Thread (second message)
	timelineText := s.renderTimeline(ag)
	var timelineTS string
	if timelineText != "" {
		_, timelineTS, err = client.PostMessageContext(ctx, channelID,
			slackapi.MsgOptionText(timelineText, false),
			slackapi.MsgOptionTS(timestamp),
		)
		if err != nil {
			log.Printf("Provider: Failed to post timeline thread: %v", err)
		}
	}

	// Return data to be saved for updates
	data := Data{
		ChannelID:         channelID,
		Timestamp:         timestamp,
		TimelineTimestamp: timelineTS,
		Permalink:         permalink,
	}
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}

func (s *Provider) Update(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) (string, error) {
	if d == nil || d.ProviderPayload == "" {
		return "", nil
	}

	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	data, ok := parseData(d.ProviderPayload)
	if !ok {
		return "", fmt.Errorf("slack: invalid provider payload for delivery %s", d.ID)
	}

	// 1. Update Main Message
	attachment := s.renderBodyAttachment(ag, false)
	title := s.renderTitleBlocks(ag, false)
	_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, data.Timestamp,
		slackapi.MsgOptionText(s.plainTitle(ag, false), false),
		slackapi.MsgOptionBlocks(title...),
		slackapi.MsgOptionAttachments(attachment),
	)
	if err != nil {
		return "", err
	}

	// 2. Update/Create Timeline Thread
	timelineText := s.renderTimeline(ag)
	if timelineText != "" {
		timelineTS := data.TimelineTimestamp
		if timelineTS != "" {
			// Update existing
			_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, timelineTS,
				slackapi.MsgOptionText(timelineText, false),
			)
			if err != nil {
				log.Printf("Provider: Failed to update timeline: %v", err)
			}
		} else {
			// Create new
			_, newTS, err := client.PostMessageContext(ctx, data.ChannelID,
				slackapi.MsgOptionText(timelineText, false),
				slackapi.MsgOptionTS(data.Timestamp),
			)
			if err == nil {
				data.TimelineTimestamp = newTS
			} else {
				log.Printf("Provider: Failed to create timeline: %v", err)
			}
		}
	}

	// Return updated data (in case TimelineTimestamp changed)
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}

func (s *Provider) Resolve(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) error {
	if d == nil || d.ProviderPayload == "" {
		return nil
	}

	client, err := s.getClient()
	if err != nil {
		return err
	}

	data, ok := parseData(d.ProviderPayload)
	if !ok {
		return fmt.Errorf("slack: invalid provider payload for delivery %s", d.ID)
	}

	// 1. Thread Reply
	_, _, err = client.PostMessageContext(ctx, data.ChannelID,
		slackapi.MsgOptionText("✅ Alert Group Resolved", false),
		slackapi.MsgOptionTS(data.Timestamp),
	)
	if err != nil {
		log.Printf("Provider: Failed to send reply: %v", err)
	}

	// 2. Update Main Message (critical — user-visible)
	attachment := s.renderBodyAttachment(ag, true)
	title := s.renderTitleBlocks(ag, true)

	_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, data.Timestamp,
		slackapi.MsgOptionText(s.plainTitle(ag, true), false),
		slackapi.MsgOptionBlocks(title...),
		slackapi.MsgOptionAttachments(attachment),
	)
	if err != nil {
		return fmt.Errorf("failed to update main message: %w", err)
	}

	// 3. Update Timeline Thread (Final state)
	timelineText := s.renderTimeline(ag)
	timelineTS := data.TimelineTimestamp
	if timelineText != "" && timelineTS != "" {
		_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, timelineTS,
			slackapi.MsgOptionText(timelineText, false),
		)
		if err != nil {
			log.Printf("Provider: Failed to update final timeline: %v", err)
		}
	}

	return nil
}

// Permalink returns the stored permalink for a delivery, if any. It lets the
// executor build "Open in Slack" links without parsing the provider payload.
func (s *Provider) Permalink(d *model.NotificationDelivery) string {
	if d == nil {
		return ""
	}
	data, ok := parseData(d.ProviderPayload)
	if !ok {
		return ""
	}
	return data.Permalink
}

// providers.MessageStatus holds the resolved title and color for a Slack message.
// collectMentions returns a mrkdwn string of Slack user/group mentions from all alerts.
func collectMentions(alerts []model.Alert) string {
	slackUsers := make(map[string]bool)
	for _, a := range alerts {
		if u := a.Labels["slack_user"]; u != "" {
			slackUsers[u] = true
		}
	}
	if len(slackUsers) == 0 {
		return ""
	}
	var parts []string
	for u := range slackUsers {
		if strings.HasPrefix(u, "S") {
			parts = append(parts, fmt.Sprintf("<!subteam^%s>", u))
		} else {
			parts = append(parts, fmt.Sprintf("<@%s>", u))
		}
	}
	return strings.Join(parts, " ")
}

// buildAlertList returns a mrkdwn bullet list of alerts (max 10).
func buildAlertList(alerts []model.Alert) string {
	const maxAlerts = 10
	alertList := ""
	rendered := 0

	for _, a := range alerts {
		if rendered >= maxAlerts {
			break
		}
		rendered++

		dashURL := a.Annotations["dashboard"]
		if dashURL == "" {
			dashURL = a.Labels["dashboard"]
		}
		dashLink := ""
		if dashURL != "" {
			dashLink = fmt.Sprintf(" <%s|[dash]>", dashURL)
		}

		bookURL := a.Annotations["runbook"]
		bookLink := ""
		if bookURL != "" {
			bookLink = fmt.Sprintf(" <%s|[runbook]>", bookURL)
		}

		if a.Status == model.AlertStatusFiring {
			alertList += fmt.Sprintf("• 🔴 %s (Sev: %s)%s%s\n", a.Labels["alertname"], a.Labels["severity"], dashLink, bookLink)
		} else {
			alertList += fmt.Sprintf("• 🟢 %s (Resolved)%s%s\n", a.Labels["alertname"], dashLink, bookLink)
		}
	}
	if remaining := len(alerts) - maxAlerts; remaining > 0 {
		alertList += fmt.Sprintf("_... and %d more alerts_\n", remaining)
	}
	return alertList
}

// truncateText truncates s to fit within maxLen bytes (including suffix),
// cutting at a newline boundary to avoid splitting mrkdwn constructs like <url|label>.
func truncateText(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const suffix = "\n_... truncated_\n"
	if maxLen <= len(suffix) {
		// Edge case: maxLen too small for suffix — hard-cut at rune boundary
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
func (s *Provider) unknownTeamNotice(teamID string) string {
	notice := fmt.Sprintf(
		"*⚠️ Unknown team `%s`*\nTokayOps has no such team, so this alert cannot be acknowledged or resolved from Slack.",
		sanitizeTeamLabel(teamID),
	)
	if s.selfURL != "" {
		notice += fmt.Sprintf(" <%s/#/cfg/teams|Set up the team>", s.selfURL)
	}
	return notice
}

// plainTitle returns the unformatted title for use as the top-level message
// text (notification preview / accessibility fallback).
func (s *Provider) plainTitle(ag *model.AlertGroup, isResolved bool) string {
	firing := providers.CountFiring(ag.Alerts)
	return providers.ResolveStatus(ag, isResolved, firing).Title
}

// renderTitleBlocks returns the title section as top-level blocks. Used as
// both the in-channel header (above the colored attachment) and the unfurl
// preview content — Slack message-link unfurls render only top-level blocks.
func (s *Provider) renderTitleBlocks(ag *model.AlertGroup, isResolved bool) []slackapi.Block {
	firing := providers.CountFiring(ag.Alerts)
	status := providers.ResolveStatus(ag, isResolved, firing)

	titleText := status.Title
	if ag.ExternalURL != "" {
		titleText = fmt.Sprintf("<%s|%s>", ag.ExternalURL, status.Title)
	}
	return []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, "*"+titleText+"*", false, false),
			nil, nil,
		),
	}
}

// renderBodyAttachment returns the colored attachment containing severity/alerts,
// optional action buttons, and footer. The title is NOT included — it lives in
// the top-level blocks (renderTitleBlocks) so Slack message-link unfurls can
// preview it.
func (s *Provider) renderBodyAttachment(ag *model.AlertGroup, isResolved bool) slackapi.Attachment {
	firing := providers.CountFiring(ag.Alerts)
	status := providers.ResolveStatus(ag, isResolved, firing)
	mentions := collectMentions(ag.Alerts)

	var blocks []slackapi.Block

	// Severity + alerts
	bodyText := ""
	if mentions != "" {
		bodyText = mentions + "\n\n"
	}
	bodyText += fmt.Sprintf("*Severity:* %s\n", ag.Severity)

	alertList := buildAlertList(ag.Alerts)
	if len(ag.Alerts) == 0 {
		alertList = "• " + ag.Title
	}
	bodyText += "*Alerts:*\n" + alertList
	bodyText = truncateText(bodyText, 3000)
	blocks = append(blocks, slackapi.NewSectionBlock(
		slackapi.NewTextBlockObject(slackapi.MarkdownType, bodyText, false, false),
		nil, nil,
	))

	// Action buttons (conditional on status, interactive toggle and whether the
	// alert group's team is onboarded at all).
	//
	// The lookup is skipped unless it can change the card. A resolved card never
	// carries buttons, and with interactivity switched off their absence is the
	// administrator's decision rather than a problem to report, so neither state
	// gets buttons OR the notice - and neither should pay a query for it, least
	// of all while the database is the thing that is struggling.
	interactive := s.tokenSource != nil && s.tokenSource.GetSlackInteractive()
	actionable := interactive && !isResolved
	teamOnboarded := true
	if actionable {
		teamOnboarded = providers.TeamIsOnboarded(s.teamLookup, ag.TeamID)
	}

	showAck := actionable && teamOnboarded && ag.Status != model.AlertGroupStatusAcknowledged
	showResolve := actionable && teamOnboarded

	if showAck || showResolve {
		blocks = append(blocks, slackapi.NewDividerBlock())

		var buttons []slackapi.BlockElement
		if showAck {
			ackBtn := slackapi.NewButtonBlockElement(
				model.SlackActionAckAlertGroup, ag.ID,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Acknowledge", true, false),
			)
			ackBtn.WithStyle(slackapi.StyleDanger)
			buttons = append(buttons, ackBtn)
		}
		if showResolve {
			resolveBtn := slackapi.NewButtonBlockElement(
				model.SlackActionResolveAlertGroup, ag.ID,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Resolve", true, false),
			)
			buttons = append(buttons, resolveBtn)
		}
		blocks = append(blocks, slackapi.NewActionBlock("alert_actions", buttons...))
	} else if actionable && !teamOnboarded {
		// Where the buttons would have been, say why they are not there. The
		// claim is deliberately limited to what the lookup proves: the team is
		// absent, so nobody can act on this from Slack. It says nothing about
		// escalation, because alert_groups.team_id has no foreign key and an
		// escalation started before the team was deleted keeps running.
		blocks = append(blocks, slackapi.NewDividerBlock())
		blocks = append(blocks, slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject(slackapi.MarkdownType, s.unknownTeamNotice(ag.TeamID), false, false),
			nil, nil,
		))
	}

	// Context footer
	var footerText string
	if s.selfURL != "" {
		footerText = fmt.Sprintf("ID: <%s/#/ops/alert-groups/%s|%s>", s.selfURL, ag.ID, ag.ID)
	} else {
		footerText = fmt.Sprintf("ID: %s", ag.ID)
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

// renderTimeline generates the combined summary + timeline message for Slack thread
func (s *Provider) renderTimeline(ag *model.AlertGroup) string {
	var sections []string

	// Section 1: Alert Summaries (descriptions)
	summarySection := s.renderAlertSummaries(ag)
	if summarySection != "" {
		sections = append(sections, summarySection)
	}

	// Section 2: Timeline (limit to last 20 events to avoid Slack message truncation)
	const maxTimelineEvents = 20
	if len(ag.TimelineEvents) > 0 {
		events := ag.TimelineEvents
		skippedCount := 0
		if len(events) > maxTimelineEvents {
			skippedCount = len(events) - maxTimelineEvents
			events = events[skippedCount:] // Take last N events
		}

		var lines []string
		if skippedCount > 0 {
			lines = append(lines, fmt.Sprintf("... and %d earlier events", skippedCount))
		}
		for _, e := range events {
			icon := getEventIcon(e.Type)
			localTime := e.CreatedAt.Local()
			_, offset := localTime.Zone()
			offsetHours := offset / 3600
			timeStr := fmt.Sprintf("[%s GMT%+d]", localTime.Format("15:04:05"), offsetHours)
			line := fmt.Sprintf("%s %s %s", timeStr, icon, e.Message)
			if e.Actor != "" && e.Actor != "system" {
				line += " (by " + e.Actor + ")"
			}
			lines = append(lines, line)
		}
		sections = append(sections, "📋 *Timeline:*\n```\n"+strings.Join(lines, "\n")+"\n```")
	}

	if len(sections) == 0 {
		return ""
	}

	return strings.Join(sections, "\n\n")
}

// renderAlertSummaries generates the alert descriptions section
func (s *Provider) renderAlertSummaries(ag *model.AlertGroup) string {
	const maxSummaries = 10
	const maxDescLen = 200

	var summaries []string
	for _, a := range ag.Alerts {
		if len(summaries) >= maxSummaries {
			break
		}

		// Use Annotation['description'] or fallback to summary
		sum := a.Annotations["description"]
		if sum == "" {
			sum = a.Annotations["summary"]
		}
		if sum == "" {
			continue
		}
		if len([]rune(sum)) > maxDescLen {
			sum = string([]rune(sum)[:maxDescLen]) + "..."
		}

		icon := "🔴"
		if a.Status == model.AlertStatusResolved {
			icon = "🟢"
		}
		// Format: 🔴 *AlertName*: Summary text
		summaries = append(summaries, fmt.Sprintf("%s *%s*: %s", icon, a.Labels["alertname"], sum))
	}

	if len(summaries) == 0 {
		return ""
	}
	result := "*Alert Details:*\n" + strings.Join(summaries, "\n")
	if len(ag.Alerts) > maxSummaries {
		result += fmt.Sprintf("\n_... and %d more alert details_", len(ag.Alerts)-maxSummaries)
	}
	return result
}

// getEventIcon returns an emoji icon for the event type
func getEventIcon(eventType model.TimelineEventType) string {
	switch eventType {
	case model.TimelineEventCreated:
		return "[NEW]"
	case model.TimelineEventAlertAdded:
		return "[+]"
	case model.TimelineEventAlertResolved:
		return "[-]"
	case model.TimelineEventAcknowledged:
		return "[ACK]"
	case model.TimelineEventResolved:
		return "[RESOLVED]"
	case model.TimelineEventNotificationSent:
		return "[->]"
	case model.TimelineEventNotificationFailed:
		return "[X]"
	case model.TimelineEventNote:
		return "[NOTE]"
	case model.TimelineEventStatusChange:
		return "[~]"
	default:
		return "[?]"
	}
}
