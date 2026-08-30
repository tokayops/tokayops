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
// It bounds a CALL, not a delivery, and duplicates are not what it fixes: a
// timeout can fire on a request Slack already accepted, and the retry then
// sends it again. That is accepted - see the register. An attempt is one call
// now, so the older half of this note, about a step outliving its job lease
// while making three, no longer applies.
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

// ErrNoReceipt is a change to a message whose stored coordinates cannot be
// read. The store refuses to ask for one without them, so this is a damaged
// row rather than a message that went missing.
var ErrNoReceipt = fmt.Errorf("slack: the message to change has no readable coordinates")

// ErrNoContent: the commitment carries no frozen state, and a card is drawn
// from one. It is a contract violation rather than a delivery failure - the
// commitment was routed to a channel that cannot render it.
var ErrNoContent = fmt.Errorf("slack: this commitment carries no state to render")

// (ErrNoSlackUserID is gone: a channel preparing an attempt answers
// "identity_not_linked" for a person with no account here, whichever provider
// it is.)

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
// and integration handlers). It is not part of the delivery path.
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

// Send is the job engine's one remaining call into this channel: a
// fire-and-forget direct message, which is what a handover announcement is. It
// returns no payload because nothing edits it afterwards.
//
// A channel target is refused rather than posted. It used to post an alert
// card, and that is the delivery domain's work now - it goes through
// ExecuteAttempt, which records where the card landed so a later revision can
// reach it. Posting one from here would make a card nothing can update, and
// the fact that no caller does it today is not a reason to leave the branch
// open. Behaviour keys on Target.Kind, never on req.Kind.
func (s *Provider) Send(ctx context.Context, req providers.NotificationRequest) (string, error) {
	if req.Target.Kind != "user" {
		return "", fmt.Errorf("slack: %q is not sent from here any more", req.Target.Kind)
	}
	if req.Message == "" {
		return "", fmt.Errorf("slack: user send requires a message")
	}
	return "", s.sendDM(ctx, req.Target.ID, req.Message)
}

// providers.MessageStatus holds the resolved title and color for a Slack message.
