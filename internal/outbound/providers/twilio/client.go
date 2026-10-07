// Package twilio places voice calls through the Twilio REST API.
//
// This build has one operation: create a call that speaks a short text. The
// calls that verify a person's number and test their Do Not Disturb settings
// use it synchronously, from the request of the person standing at the form;
// they are not commitments of the delivery domain.
package twilio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// DefaultBaseURL is the Twilio REST API.
const DefaultBaseURL = "https://api.twilio.com"

// ErrNotCreated wraps every failure that proves no call exists: the provider
// answered, and the answer was a refusal. A call refused this way costs
// nothing and may be asked for again.
var ErrNotCreated = errors.New("twilio did not create the call")

// ErrUnknown wraps every failure after which a call may exist: the request may
// have been taken and the answer lost. Twilio has no idempotency key for
// creating a call, and nothing of ours on the call to look it up by, so there is
// no way to find out; the caller treats the call as possibly ringing.
var ErrUnknown = errors.New("twilio may have created the call")

// Refusal is the provider's own account of why it did not create a call.
type Refusal struct {
	HTTPStatus int
	// Code is Twilio's error code, 21211 for a number it cannot dial. Zero
	// when the body carried none.
	Code    int
	Message string
}

func (r *Refusal) Error() string {
	if r.Code != 0 {
		return fmt.Sprintf("twilio refused the call: %d %s (HTTP %d)", r.Code, r.Message, r.HTTPStatus)
	}
	return fmt.Sprintf("twilio refused the call (HTTP %d)", r.HTTPStatus)
}

// Unwrap lets errors.Is find ErrNotCreated.
func (r *Refusal) Unwrap() error { return ErrNotCreated }

// CodeInvalidNumber is the refusal of a To number Twilio cannot dial.
const CodeInvalidNumber = 21211

// Client talks to one Twilio API endpoint; the account comes with each call.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient is a client for the real API.
func NewClient() *Client {
	return &Client{BaseURL: DefaultBaseURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Call is one outgoing call to create.
type Call struct {
	To    string
	TwiML string
	// Ring is how long the phone may ring before Twilio gives up.
	Ring time.Duration
	// Limit is the longest the call may last once answered.
	Limit time.Duration
	// StatusCallback is where Twilio reports how the call goes, and Events
	// which of its moments it reports. Empty for a call nobody follows up -
	// the code a person types back is the only answer that one needs.
	StatusCallback string
	Events         []string
}

// CreateCall asks Twilio to place a call from cfg's number and returns its
// SID. Its error wraps ErrNotCreated or ErrUnknown, and nothing else: the one
// thing a caller must be able to tell is whether a call may be ringing.
func (c *Client) CreateCall(ctx context.Context, cfg model.TwilioConfig, call Call) (string, error) {
	form := url.Values{}
	form.Set("To", call.To)
	form.Set("From", cfg.FromNumber)
	form.Set("Twiml", call.TwiML)
	form.Set("Timeout", strconv.Itoa(int(call.Ring/time.Second)))
	form.Set("TimeLimit", strconv.Itoa(int(call.Limit/time.Second)))
	if call.StatusCallback != "" {
		form.Set("StatusCallback", call.StatusCallback)
		for _, event := range call.Events {
			form.Add("StatusCallbackEvent", event)
		}
	}

	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Calls.json",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(cfg.AccountSID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		// Nothing left this process.
		return "", fmt.Errorf("%w: build the request: %v", ErrNotCreated, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(cfg.AccountSID, cfg.AuthToken)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Both wrapped: the caller asks whether a call may exist, and the
		// domain asks the transport error itself whether the request ever
		// left - a refused connection proves it did not.
		return "", fmt.Errorf("%w: %w", ErrUnknown, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("%w: read the answer: %v", ErrUnknown, err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var created struct {
			SID string `json:"sid"`
		}
		if err := json.Unmarshal(body, &created); err != nil || created.SID == "" {
			// Twilio said yes and did not say what it made.
			return "", fmt.Errorf("%w: an answer with no call sid", ErrUnknown)
		}
		return created.SID, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// A 4xx is a refusal of this request, 429 included: Twilio does not
		// process a request it answers with 429.
		refusal := &Refusal{HTTPStatus: resp.StatusCode}
		var problem struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &problem) == nil {
			refusal.Code, refusal.Message = problem.Code, problem.Message
		}
		return "", refusal
	default:
		// A 5xx does not say the request was not taken.
		return "", fmt.Errorf("%w: HTTP %d", ErrUnknown, resp.StatusCode)
	}
}

// CallStatus is where one call stands, in Twilio's words: queued, ringing,
// in-progress, completed, busy, no-answer, failed, canceled.
func (c *Client) CallStatus(ctx context.Context, cfg model.TwilioConfig, sid string) (string, error) {
	endpoint := fmt.Sprintf("%s/2010-04-01/Accounts/%s/Calls/%s.json",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(cfg.AccountSID), url.PathEscape(sid))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(cfg.AccountSID, cfg.AuthToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("twilio answered HTTP %d about call %s", resp.StatusCode, sid)
	}
	var call struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &call); err != nil || call.Status == "" {
		return "", fmt.Errorf("twilio said nothing readable about call %s", sid)
	}
	return call.Status, nil
}
