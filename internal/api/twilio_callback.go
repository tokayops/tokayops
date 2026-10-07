package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/providers/twilio"
)

// CallEvents is what the callback route needs: what an attempt was bound to -
// the account whose token signs what Twilio says about it - and the inbox.
type CallEvents interface {
	AttemptBinding(ctx context.Context, attemptID string) (outbound.BoundContext, bool, error)
	RecordProviderEvent(ctx context.Context, event outbound.ProviderEvent) (string, error)
	ApplyProviderEvent(ctx context.Context, eventRowID string,
		translators map[string]outbound.EffectTranslator) (outbound.ApplyResult, error)
}

// SetCallEvents wires the route Twilio reports call progress to. Without it
// the route answers 503 and the outcome of every call comes from the poll.
func (a *API) SetCallEvents(events CallEvents, translators map[string]outbound.EffectTranslator) {
	a.callEvents = events
	a.callTranslators = translators
}

// HandleTwilioCallStatus godoc
// @Summary Twilio call status callback
// @Description Twilio reports how a call goes. Signed with the auth token of the account the call was placed on; the attempt is named in the query.
// @Tags webhooks
// @Accept x-www-form-urlencoded
// @Success 204
// @Failure 403 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /twilio/voice/status [post]
func (a *API) HandleTwilioCallStatus(c echo.Context) error {
	if a.callEvents == nil || a.selfURL == "" {
		return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "call events are not configured"})
	}
	req := c.Request()
	if err := req.ParseForm(); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "unreadable form"})
	}
	ctx := req.Context()

	// The token that signs this is the account's the call was placed on, and
	// that account is the attempt's. An attempt this system did not make, or
	// whose integration is gone, has no token to check against - and what
	// cannot be checked is not kept.
	attemptID := req.URL.Query().Get("a")
	bound, found, err := a.callEvents.AttemptBinding(ctx, attemptID)
	if err != nil {
		log.Printf("twilio callback: read attempt %s: %v", attemptID, err)
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
	if !found || bound.IntegrationID == "" {
		return c.JSON(http.StatusForbidden, ErrorResponse{Error: "unknown call"})
	}
	token, ok := a.twilioToken(bound.IntegrationID)
	if !ok {
		return c.JSON(http.StatusForbidden, ErrorResponse{Error: "unknown call"})
	}

	signed := signedCallbackURL(a.selfURL, req.URL.Path, req.URL.RawQuery)
	if !twilio.ValidSignature(token, signed, req.PostForm, req.Header.Get("X-Twilio-Signature")) {
		return c.JSON(http.StatusForbidden, ErrorResponse{Error: "bad signature"})
	}

	event := outbound.ProviderEvent{
		Provider:       "twilio",
		AccountScope:   req.PostForm.Get("AccountSid"),
		AttemptID:      attemptID,
		ExternalRef:    req.PostForm.Get("CallSid"),
		ProviderStatus: req.PostForm.Get("CallStatus"),
	}
	sequence := req.PostForm.Get("SequenceNumber")
	event.EventID = event.ExternalRef + ":" + sequence
	if n, err := strconv.Atoi(sequence); err == nil {
		event.Sequence = &n
	} else {
		// Without Twilio's number the status keeps the event apart from the
		// others of the same call.
		event.EventID = event.ExternalRef + ":" + event.ProviderStatus
	}
	if at, err := time.Parse(time.RFC1123Z, req.PostForm.Get("Timestamp")); err == nil {
		event.OccurredAt = at
	}
	if code := req.PostForm.Get("SipResponseCode"); code != "" {
		event.Summary = event.ProviderStatus + " (SIP " + code + ")"
	}

	id, err := a.callEvents.RecordProviderEvent(ctx, event)
	if err != nil {
		// Not kept: Twilio is told so, and retries on a 5xx where it was asked
		// to.
		log.Printf("twilio callback: keep %s: %v", event.EventID, err)
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "internal error"})
	}
	// Kept. Applying it may still fail - a row held by another transaction -
	// and the poll's pass applies what is left in the inbox, so the answer to
	// Twilio does not depend on it.
	if _, err := a.callEvents.ApplyProviderEvent(context.WithoutCancel(ctx), id, a.callTranslators); err != nil {
		log.Printf("twilio callback: apply %s: %v", event.EventID, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// twilioToken is the auth token of one Twilio integration, read from the
// database each time: a rotated token signs from the next callback on.
func (a *API) twilioToken(integrationID string) (string, bool) {
	row, err := a.store.GetIntegrationByID(integrationID)
	if err != nil || row == nil || row.Type != model.IntegrationTypeTwilio {
		return "", false
	}
	var cfg model.TwilioConfig
	if err := json.Unmarshal(row.Config, &cfg); err != nil || cfg.AuthToken == "" {
		return "", false
	}
	return cfg.AuthToken, true
}

// signedCallbackURL is the URL Twilio signed: the address this installation is
// reached at from outside - never rebuilt from the request, which behind a
// proxy carries another scheme, host or port - with the path and query Twilio
// called. For HTTPS Twilio drops the port before it signs, so the port goes
// here too.
func signedCallbackURL(selfURL, path, rawQuery string) string {
	base := strings.TrimRight(selfURL, "/")
	if u, err := url.Parse(base); err == nil && u.Scheme == "https" && u.Port() != "" {
		u.Host = u.Hostname()
		base = u.String()
	}
	signed := base + path
	if rawQuery != "" {
		signed += "?" + rawQuery
	}
	return signed
}
