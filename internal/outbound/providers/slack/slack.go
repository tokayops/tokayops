package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
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
	return slackapi.New(token, append(opts,
		slackapi.OptionHTTPClient(&http.Client{
			Timeout: timeout,
			// Redirects are not followed, and that is about the journal rather
			// than about Slack. A provider that accepted the POST and answered
			// 3xx would send the client somewhere else; if THAT hop fails to
			// resolve or handshake, the error looks exactly like a request
			// that never left - and the retry it earns posts a second message.
			// Stopping at the first response keeps the proof honest: a 3xx is
			// an answer from the provider, and an answer nobody recognises is
			// doubt.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}))...)
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

// Data is the opaque provider payload Slack stores in a delivery row. It holds
// the coordinates of the posted message (channel + timestamp) and a permalink
// for DM "Open in Slack" links.
//
// The thread timestamp left it with the threaded timeline: a second message in
// a thread is a second external effect, and one attempt performs one (NFR-6).
// Rows written before it went away still parse - an unknown field is ignored.
type Data struct {
	ChannelID string `json:"channel_id"`
	Timestamp string `json:"timestamp"`
	Permalink string `json:"permalink,omitempty"`
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

// RenderCard returns the full card payload for Slack message
// rendering/replacement.
//
// This is the path that still starts from a live row: it freezes the group into
// a snapshot here and then renders from that, so the card is drawn by exactly
// the same pure function the outbound handlers use. What differs is only where
// the inputs came from.
func (s *Provider) RenderCard(ag *model.AlertGroup, isResolved bool) slackcard.Card {
	return Render(s.freeze(ag, isResolved), s.interactive())
}

// freeze takes the snapshot the renderers work from, reading the configuration,
// the team lookup and the process zone once - at this instant - instead of
// halfway through drawing a card.
func (s *Provider) freeze(ag *model.AlertGroup, isResolved bool) keys.SnapshotInput {
	onboarded := true
	// Skipped unless it can change the card: a resolved card never carries
	// buttons, and with interactivity off their absence is the administrator's
	// decision rather than something to report - so neither state gets buttons
	// OR the notice, and neither should pay a query for it, least of all while
	// the database is the thing that is struggling.
	if s.interactive() && !isResolved && ag != nil {
		onboarded = providers.TeamIsOnboarded(s.teamLookup, ag.TeamID)
	}

	return providers.RenderableOf(providers.GroupView{
		Group:         ag,
		IsResolved:    isResolved,
		SelfURL:       s.selfURL,
		TeamOnboarded: onboarded,
		Zone:          providers.ProcessZone(),
	})
}

func (s *Provider) interactive() bool {
	return s.tokenSource != nil && s.tokenSource.GetSlackInteractive()
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
// channel and returns the Data payload needed to update/resolve it later.
func (s *Provider) sendCard(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
	if ag == nil {
		return "", fmt.Errorf("slack: channel send requires an alert group")
	}
	client, err := s.getClient()
	if err != nil {
		return "", err
	}

	state := s.freeze(ag, false)
	card := Render(state, s.interactive())

	channelID, timestamp, err := client.PostMessageContext(ctx, targetID,
		slackapi.MsgOptionText(card.Text, false),
		slackapi.MsgOptionBlocks(card.Blocks...),
		slackapi.MsgOptionAttachments(card.Attachment),
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

	// Return data to be saved for updates
	data := Data{
		ChannelID: channelID,
		Timestamp: timestamp,
		Permalink: permalink,
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
	state := s.freeze(ag, false)
	card := Render(state, s.interactive())
	_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, data.Timestamp,
		slackapi.MsgOptionText(card.Text, false),
		slackapi.MsgOptionBlocks(card.Blocks...),
		slackapi.MsgOptionAttachments(card.Attachment),
	)
	if err != nil {
		return "", err
	}

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

	state := s.freeze(ag, true)
	card := Render(state, s.interactive())

	_, _, _, err = client.UpdateMessageContext(ctx, data.ChannelID, data.Timestamp,
		slackapi.MsgOptionText(card.Text, false),
		slackapi.MsgOptionBlocks(card.Blocks...),
		slackapi.MsgOptionAttachments(card.Attachment),
	)
	if err != nil {
		return fmt.Errorf("failed to update main message: %w", err)
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
