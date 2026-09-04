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

// NewProvider builds Slack as the API layer uses it: a direct message on
// somebody's behalf, and the interactivity setting.
//
// It carries no base URL and no team lookup any more. Both existed for the
// freeze that drew a card from a live alert group, and a card is drawn from the
// state frozen at admission now - so the links in it, and whether the team is
// onboarded, are decided there and travel in the snapshot.
func NewProvider(tokenSource TokenSource) *Provider {
	return &Provider{tokenSource: tokenSource}
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

// SendDM is one message to one person, sent and forgotten.
//
// The whole of what this type sends. It exists for the calls that are not a
// commitment - an OTP somebody just asked for, a note from an integration
// handler - and those have no receipt, no revision and nothing to update
// afterwards. Anything a commitment promises goes through Handler, which
// records where the message landed so a later revision can reach it.
//
// It used to be a case inside a generic request shaped like a job step, with a
// target kind, an alert group and an editable flag. Every one of those had one
// answer here and the shape invited a caller to ask for a card nothing could
// ever update.
func (s *Provider) SendDM(ctx context.Context, userID, message string) error {
	if message == "" {
		return fmt.Errorf("slack: a direct message with nothing in it")
	}
	return s.sendDM(ctx, userID, message)
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

// providers.MessageStatus holds the resolved title and color for a Slack message.
