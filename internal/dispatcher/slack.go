package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/slackcard"
)

// ErrSlackUserNotFound means the email has no matching Slack account.
var ErrSlackUserNotFound = errors.New("slack user not found")

// SlackTokenSource provides dynamic token and config lookup
type SlackTokenSource interface {
	GetSlackToken() string
	GetSlackInteractive() bool
}

type SlackProvider struct {
	tokenSource SlackTokenSource
	selfURL     string // TokayOps base URL for deep links
	mu          sync.Mutex
	cachedToken string
	client      *slack.Client
}

var _ Provider = (*SlackProvider)(nil)

// SlackData is the opaque provider payload Slack stores in a delivery row. It
// holds the coordinates of the posted message (channel + timestamp), the timeline
// thread timestamp, and a permalink for DM "Open in Slack" links.
type SlackData struct {
	ChannelID         string `json:"channel_id"`
	Timestamp         string `json:"timestamp"`
	TimelineTimestamp string `json:"timeline_ts,omitempty"`
	Permalink         string `json:"permalink,omitempty"`
}

func parseSlackData(raw string) (*SlackData, bool) {
	if raw == "" {
		return nil, false
	}

	var data SlackData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, false
	}
	if data.ChannelID == "" || data.Timestamp == "" {
		return nil, false
	}
	return &data, true
}

func NewSlackProvider(tokenSource SlackTokenSource, selfURL string) *SlackProvider {
	return &SlackProvider{
		tokenSource: tokenSource,
		selfURL:     selfURL,
	}
}

// RenderCard returns the full card payload for Slack message rendering/replacement.
func (s *SlackProvider) RenderCard(ag *model.AlertGroup, isResolved bool) slackcard.Card {
	return slackcard.Card{
		Text:       s.plainTitle(ag, isResolved),
		Blocks:     s.renderTitleBlocks(ag, isResolved),
		Attachment: s.renderBodyAttachment(ag, isResolved),
	}
}

// ErrNoSlackToken is returned when Slack token is not configured
var ErrNoSlackToken = fmt.Errorf("slack integration not configured (no token)")

// (ErrNoSlackUserID was removed in Epic 7 Sprint 3 — the dispatcher-level
// ErrIdentityNotLinked in identity.go covers it generically.)

// getClient returns a Slack client, recreating it if the token changed.
// Returns error if no token is configured or tokenSource is nil.
func (s *SlackProvider) getClient() (*slack.Client, error) {
	if s.tokenSource == nil {
		return nil, ErrNoSlackToken
	}
	token := s.tokenSource.GetSlackToken()
	if token == "" {
		return nil, ErrNoSlackToken
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client == nil || s.cachedToken != token {
		if s.client != nil && s.cachedToken != token {
			log.Printf("SlackProvider: Token changed, recreating client")
		}
		s.cachedToken = token
		s.client = slack.New(token)
	}
	return s.client, nil
}

// SendDM is a thin convenience wrapper over Send for callers that hold a
// concrete *SlackProvider and only need a fire-and-forget DM (internal/api OTP
// and integration handlers). It is NOT part of dispatcher.Provider.
func (s *SlackProvider) SendDM(ctx context.Context, userID, message string) error {
	_, err := s.Send(ctx, NotificationRequest{
		Kind:     "slack_dm",
		Target:   NotificationTarget{Kind: "user", ID: userID},
		Message:  message,
		Editable: false,
	})
	return err
}

// sendDM opens a direct message channel with the user and sends a message.
func (s *SlackProvider) sendDM(ctx context.Context, userID, message string) error {
	client, err := s.getClient()
	if err != nil {
		return err
	}

	params := &slack.OpenConversationParameters{
		Users: []string{userID},
	}
	channel, _, _, err := client.OpenConversationContext(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to open conversation: %w", err)
	}

	_, _, err = client.PostMessageContext(ctx, channel.ID, slack.MsgOptionText(message, false))
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	return nil
}

// GetSlackUserIDByEmail looks up a Slack user by email via the users.lookupByEmail API.
// Returns the Slack user ID if found.
func (s *SlackProvider) GetSlackUserIDByEmail(ctx context.Context, email string) (string, error) {
	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	user, err := client.GetUserByEmailContext(ctx, email)
	if err != nil {
		var slackErr slack.SlackErrorResponse
		if errors.As(err, &slackErr) && slackErr.Err == "users_not_found" {
			return "", ErrSlackUserNotFound
		}
		return "", fmt.Errorf("slack users.lookupByEmail: %w", err)
	}

	return user.ID, nil
}

// GetEmailBySlackID looks up a Slack user's email via the users.info API.
// Returns the email if the user is found and has a profile email set.
func (s *SlackProvider) GetEmailBySlackID(ctx context.Context, slackUserID string) (string, error) {
	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	user, err := client.GetUserInfoContext(ctx, slackUserID)
	if err != nil {
		var slackErr slack.SlackErrorResponse
		if errors.As(err, &slackErr) && slackErr.Err == "user_not_found" {
			return "", ErrSlackUserNotFound
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
// returns its SlackData payload. Behaviour keys on Target.Kind / AlertGroup /
// Message — never on req.Kind. An unknown kind is rejected rather than silently
// treated as a channel card.
func (s *SlackProvider) Send(ctx context.Context, req NotificationRequest) (string, error) {
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
// channel, plus the timeline as a threaded reply, and returns the SlackData
// payload needed to update/resolve it later.
func (s *SlackProvider) sendCard(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
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
		slack.MsgOptionText(s.plainTitle(ag, false), false),
		slack.MsgOptionBlocks(title...),
		slack.MsgOptionAttachments(attachment),
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
		link, err := client.GetPermalinkContext(ctx, &slack.PermalinkParameters{
			Channel: channelID,
			Ts:      timestamp,
		})
		if err != nil {
			log.Printf("SlackProvider: Failed to get permalink for %s: %v", channelID, err)
		} else {
			permalink = link
		}
	}

	log.Printf("SlackProvider: Sent message to %s (ts: %s)", channelID, timestamp)

	// Post Timeline to Thread (second message)
	timelineText := s.renderTimeline(ag)
	var timelineTS string
	if timelineText != "" {
		_, timelineTS, err = client.PostMessageContext(ctx, channelID,
			slack.MsgOptionText(timelineText, false),
			slack.MsgOptionTS(timestamp),
		)
		if err != nil {
			log.Printf("SlackProvider: Failed to post timeline thread: %v", err)
		}
	}

	// Return data to be saved for updates
	data := SlackData{
		ChannelID:         channelID,
		Timestamp:         timestamp,
		TimelineTimestamp: timelineTS,
		Permalink:         permalink,
	}
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}

func (s *SlackProvider) Update(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) (string, error) {
	if d == nil || d.ProviderPayload == "" {
		return "", nil
	}

	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	data, ok := parseSlackData(d.ProviderPayload)
	if !ok {
		return "", fmt.Errorf("slack: invalid provider payload for delivery %s", d.ID)
	}

	// 1. Update Main Message
	attachment := s.renderBodyAttachment(ag, false)
	title := s.renderTitleBlocks(ag, false)
	_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, data.Timestamp,
		slack.MsgOptionText(s.plainTitle(ag, false), false),
		slack.MsgOptionBlocks(title...),
		slack.MsgOptionAttachments(attachment),
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
				slack.MsgOptionText(timelineText, false),
			)
			if err != nil {
				log.Printf("SlackProvider: Failed to update timeline: %v", err)
			}
		} else {
			// Create new
			_, newTS, err := client.PostMessageContext(ctx, data.ChannelID,
				slack.MsgOptionText(timelineText, false),
				slack.MsgOptionTS(data.Timestamp),
			)
			if err == nil {
				data.TimelineTimestamp = newTS
			} else {
				log.Printf("SlackProvider: Failed to create timeline: %v", err)
			}
		}
	}

	// Return updated data (in case TimelineTimestamp changed)
	bytes, _ := json.Marshal(data)
	return string(bytes), nil
}

func (s *SlackProvider) Resolve(ctx context.Context, d *model.NotificationDelivery, ag *model.AlertGroup) error {
	if d == nil || d.ProviderPayload == "" {
		return nil
	}

	client, err := s.getClient()
	if err != nil {
		return err
	}

	data, ok := parseSlackData(d.ProviderPayload)
	if !ok {
		return fmt.Errorf("slack: invalid provider payload for delivery %s", d.ID)
	}

	// 1. Thread Reply
	_, _, err = client.PostMessageContext(ctx, data.ChannelID,
		slack.MsgOptionText("✅ Alert Group Resolved", false),
		slack.MsgOptionTS(data.Timestamp),
	)
	if err != nil {
		log.Printf("SlackProvider: Failed to send reply: %v", err)
	}

	// 2. Update Main Message (critical — user-visible)
	attachment := s.renderBodyAttachment(ag, true)
	title := s.renderTitleBlocks(ag, true)

	_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, data.Timestamp,
		slack.MsgOptionText(s.plainTitle(ag, true), false),
		slack.MsgOptionBlocks(title...),
		slack.MsgOptionAttachments(attachment),
	)
	if err != nil {
		return fmt.Errorf("failed to update main message: %w", err)
	}

	// 3. Update Timeline Thread (Final state)
	timelineText := s.renderTimeline(ag)
	timelineTS := data.TimelineTimestamp
	if timelineText != "" && timelineTS != "" {
		_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, timelineTS,
			slack.MsgOptionText(timelineText, false),
		)
		if err != nil {
			log.Printf("SlackProvider: Failed to update final timeline: %v", err)
		}
	}

	return nil
}

// Permalink returns the stored permalink for a delivery, if any. It lets the
// executor build "Open in Slack" links without parsing the provider payload.
func (s *SlackProvider) Permalink(d *model.NotificationDelivery) string {
	if d == nil {
		return ""
	}
	data, ok := parseSlackData(d.ProviderPayload)
	if !ok {
		return ""
	}
	return data.Permalink
}

// messageStatus holds the resolved title and color for a Slack message.
type messageStatus struct {
	title string
	color string
	actor string // e.g. "✅ Resolved by Denis" — shown as separate line
}

// resolveStatus determines the title text and color bar based on alert group state.
func resolveStatus(ag *model.AlertGroup, isResolved bool, firing int) messageStatus {
	if isResolved {
		s := messageStatus{
			title: fmt.Sprintf("✅ Resolved: %s", ag.Title),
			color: "#36a64f",
		}
		if ag.ResolvedBy != "" {
			s.actor = fmt.Sprintf("✅ Resolved by %s", ag.ResolvedBy)
		}
		return s
	}
	if ag.Status == model.AlertGroupStatusAcknowledged {
		s := messageStatus{
			title: fmt.Sprintf("⏸️ Acknowledged: %s (%d Firing)", ag.Title, firing),
			color: "#FFA500",
		}
		if ag.AcknowledgedBy != "" {
			s.actor = fmt.Sprintf("⏸️ Acknowledged by %s", ag.AcknowledgedBy)
		}
		return s
	}
	return messageStatus{
		title: fmt.Sprintf("🔥 Alert: %s (%d Firing)", ag.Title, firing),
		color: "#FF0000",
	}
}

// countFiring returns the number of firing alerts.
func countFiring(alerts []model.Alert) int {
	n := 0
	for _, a := range alerts {
		if a.Status == model.AlertStatusFiring {
			n++
		}
	}
	return n
}

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

// plainTitle returns the unformatted title for use as the top-level message
// text (notification preview / accessibility fallback).
func (s *SlackProvider) plainTitle(ag *model.AlertGroup, isResolved bool) string {
	firing := countFiring(ag.Alerts)
	return resolveStatus(ag, isResolved, firing).title
}

// renderTitleBlocks returns the title section as top-level blocks. Used as
// both the in-channel header (above the colored attachment) and the unfurl
// preview content — Slack message-link unfurls render only top-level blocks.
func (s *SlackProvider) renderTitleBlocks(ag *model.AlertGroup, isResolved bool) []slack.Block {
	firing := countFiring(ag.Alerts)
	status := resolveStatus(ag, isResolved, firing)

	titleText := status.title
	if ag.ExternalURL != "" {
		titleText = fmt.Sprintf("<%s|%s>", ag.ExternalURL, status.title)
	}
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "*"+titleText+"*", false, false),
			nil, nil,
		),
	}
}

// renderBodyAttachment returns the colored attachment containing severity/alerts,
// optional action buttons, and footer. The title is NOT included — it lives in
// the top-level blocks (renderTitleBlocks) so Slack message-link unfurls can
// preview it.
func (s *SlackProvider) renderBodyAttachment(ag *model.AlertGroup, isResolved bool) slack.Attachment {
	firing := countFiring(ag.Alerts)
	status := resolveStatus(ag, isResolved, firing)
	mentions := collectMentions(ag.Alerts)

	var blocks []slack.Block

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
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, bodyText, false, false),
		nil, nil,
	))

	// Action buttons (conditional on status + interactive toggle)
	interactive := s.tokenSource != nil && s.tokenSource.GetSlackInteractive()
	showAck := interactive && !isResolved && ag.Status != model.AlertGroupStatusAcknowledged
	showResolve := interactive && !isResolved

	if showAck || showResolve {
		blocks = append(blocks, slack.NewDividerBlock())

		var buttons []slack.BlockElement
		if showAck {
			ackBtn := slack.NewButtonBlockElement(
				model.SlackActionAckAlertGroup, ag.ID,
				slack.NewTextBlockObject(slack.PlainTextType, "Acknowledge", true, false),
			)
			ackBtn.WithStyle(slack.StyleDanger)
			buttons = append(buttons, ackBtn)
		}
		if showResolve {
			resolveBtn := slack.NewButtonBlockElement(
				model.SlackActionResolveAlertGroup, ag.ID,
				slack.NewTextBlockObject(slack.PlainTextType, "Resolve", true, false),
			)
			buttons = append(buttons, resolveBtn)
		}
		blocks = append(blocks, slack.NewActionBlock("alert_actions", buttons...))
	}

	// Context footer
	var footerText string
	if s.selfURL != "" {
		footerText = fmt.Sprintf("ID: <%s/#/ops/alert-groups/%s|%s>", s.selfURL, ag.ID, ag.ID)
	} else {
		footerText = fmt.Sprintf("ID: %s", ag.ID)
	}
	blocks = append(blocks, slack.NewContextBlock("alert_footer",
		slack.NewTextBlockObject(slack.MarkdownType, footerText, false, false),
	))

	return slack.Attachment{
		Color:    status.color,
		Fallback: status.title,
		Blocks:   slack.Blocks{BlockSet: blocks},
	}
}

// renderTimeline generates the combined summary + timeline message for Slack thread
func (s *SlackProvider) renderTimeline(ag *model.AlertGroup) string {
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
func (s *SlackProvider) renderAlertSummaries(ag *model.AlertGroup) string {
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
