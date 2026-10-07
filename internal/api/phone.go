package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers/twilio"
	"github.com/tokayops/tokayops/internal/store"
)

// A person's phone: the number, the call with a code that proves it is theirs,
// and the test calls that show a call gets through their Do Not Disturb.
//
// These calls are not commitments of the delivery domain. The person is at the
// form and needs the refusal now, the way a Slack code works. They do go through
// the account's limiter, because the provider queues whatever it cannot place
// at once and holds it outside every deadline this system has.

// PhoneStore is what the phone routes need from persistence.
type PhoneStore interface {
	GetPhoneContact(ctx context.Context, userID string) (*model.PhoneContact, error)
	SetPhoneContact(ctx context.Context, userID, value string) (*model.PhoneContact, error)
	DeletePhoneContact(ctx context.Context, userID string) error
	PinPhoneIntegration(ctx context.Context, userID, number, integrationID string) error
	ReservePhoneCall(ctx context.Context, req store.PhoneCallRequest) (*store.PhoneReservation, error)
	ReleasePhoneReservation(ctx context.Context, id string) error
	IssuePhoneCode(ctx context.Context, userID, number, code string, ttl time.Duration) error
	ConfirmPhoneCode(ctx context.Context, userID, code string) error
	ConfirmDNDCheck(ctx context.Context, userID, sender string) error
	PhoneDNDChecks(ctx context.Context, userID string) (map[string]time.Time, error)
}

// PhoneCaller places a call. Satisfied by *twilio.Client.
type PhoneCaller interface {
	CreateCall(ctx context.Context, cfg model.TwilioConfig, call twilio.Call) (string, error)
}

// SetPhone wires the phone routes. Without it they answer 503.
func (a *API) SetPhone(ps PhoneStore, caller PhoneCaller) {
	a.phone = ps
	a.phoneCaller = caller
}

const (
	// phoneCodeTTL is longer than Slack's five minutes: the call has to ring,
	// be answered and listened to before the code is typed.
	phoneCodeTTL = 10 * time.Minute
	// phoneRing and phoneTalk bound one call at the provider. A code is said
	// twice in well under a minute.
	phoneRing = 30 * time.Second
	phoneTalk = 60 * time.Second
	// phoneHold is how long a call's capacity is spent when nothing says it
	// ended sooner: ringing, talking, and the provider putting it through.
	phoneHold = phoneRing + phoneTalk + 30*time.Second
	// phoneCallTimeout bounds the request to the provider.
	phoneCallTimeout = 15 * time.Second
)

// PhoneSender is one number a call to the person may come from.
type PhoneSender struct {
	Number        string     `json:"number"`
	IntegrationID string     `json:"integration_id"`
	Integration   string     `json:"integration"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
}

// PhoneResponse is the person's phone as the profile shows it.
type PhoneResponse struct {
	Contact *model.PhoneContact `json:"contact"`
	// Covered is whether any enabled provider can call the number.
	Covered bool `json:"covered"`
	// PinActive is whether the pinned provider is the one a call would use now.
	PinActive bool `json:"pin_active"`
	// Providers are the enabled providers that cover the number, in the order
	// they are tried.
	Providers []PhoneSender `json:"providers"`
	// Senders are the distinct numbers a call may come from, with the time the
	// person confirmed each gets through Do Not Disturb.
	Senders []PhoneSender `json:"senders"`
}

// SetPhoneRequest is a number to store.
type SetPhoneRequest struct {
	Value string `json:"value"`
}

// ConfirmPhoneCodeRequest is the code the call spoke.
type ConfirmPhoneCodeRequest struct {
	Code string `json:"code"`
}

// PinPhoneRequest names the provider to prefer; empty clears the pin.
type PinPhoneRequest struct {
	IntegrationID string `json:"integration_id"`
}

// PhoneSenderRequest names a sender number.
type PhoneSenderRequest struct {
	Sender string `json:"sender"`
}

// phoneUser is the user of a session request. API tokens are refused through
// sessionOnly: a person's phone is theirs to change, not a script's.
func (a *API) phoneUser(c echo.Context) (string, bool) {
	if !a.sessionOnly(c) {
		return "", false
	}
	userID, ok := c.Get("user_id").(string)
	if !ok || userID == "" {
		_ = c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
		return "", false
	}
	return userID, true
}

// phoneReady answers 503 when the phone routes are not wired.
func (a *API) phoneReady(c echo.Context) bool {
	if a.phone == nil || a.phoneCaller == nil {
		_ = c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: "phone calls are not configured"})
		return false
	}
	return true
}

func (a *API) twilioIntegrations() ([]twilio.Integration, error) {
	rows, err := a.store.GetIntegrationsByType(model.IntegrationTypeTwilio)
	if err != nil {
		return nil, err
	}
	return twilio.Decode(rows)
}

// GetMyPhone godoc
// @Summary Get my phone
// @Description The phone number, whether it is verified, which providers can call it, and the sender numbers with their Do Not Disturb checks.
// @Tags auth
// @Produce json
// @Success 200 {object} PhoneResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone [get]
func (a *API) GetMyPhone(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	resp, err := a.phoneResponse(c.Request().Context(), userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.JSON(http.StatusOK, resp)
}

func (a *API) phoneResponse(ctx context.Context, userID string) (*PhoneResponse, error) {
	contact, err := a.phone.GetPhoneContact(ctx, userID)
	if err != nil {
		return nil, err
	}
	resp := &PhoneResponse{Contact: contact, Providers: []PhoneSender{}, Senders: []PhoneSender{}}
	if contact == nil {
		return resp, nil
	}
	all, err := a.twilioIntegrations()
	if err != nil {
		return nil, err
	}
	checks, err := a.phone.PhoneDNDChecks(ctx, userID)
	if err != nil {
		return nil, err
	}
	covering := twilio.Covering(all, contact.Value)
	resp.Covered = len(covering) > 0
	if picked, ok := twilio.Pick(all, contact.Value, contact.PinnedIntegrationID); ok {
		resp.PinActive = contact.PinnedIntegrationID != "" && picked.ID == contact.PinnedIntegrationID
	}
	seen := map[string]bool{}
	for _, in := range covering {
		sender := PhoneSender{Number: in.Config.FromNumber, IntegrationID: in.ID, Integration: in.Name}
		resp.Providers = append(resp.Providers, sender)
		if seen[sender.Number] {
			continue
		}
		seen[sender.Number] = true
		if at, ok := checks[sender.Number]; ok {
			at := at
			sender.CheckedAt = &at
		}
		resp.Senders = append(resp.Senders, sender)
	}
	return resp, nil
}

// SetMyPhone godoc
// @Summary Set my phone
// @Description Store the phone number, in E.164 form. A different number starts unverified and unpinned.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body SetPhoneRequest true "Number"
// @Success 200 {object} PhoneResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone [put]
func (a *API) SetMyPhone(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	var req SetPhoneRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}
	value := strings.TrimSpace(req.Value)
	if !model.ValidE164(value) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: "the number must be in international form: a plus, the country code and the number, like +14155550100"})
	}
	ctx := c.Request().Context()
	if _, err := a.phone.SetPhoneContact(ctx, userID, value); err != nil {
		return a.phoneError(c, err)
	}
	resp, err := a.phoneResponse(ctx, userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.JSON(http.StatusOK, resp)
}

// DeleteMyPhone godoc
// @Summary Remove my phone
// @Description Remove the phone number and the Do Not Disturb checks made for it.
// @Tags auth
// @Success 204
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone [delete]
func (a *API) DeleteMyPhone(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	if err := a.phone.DeletePhoneContact(c.Request().Context(), userID); err != nil {
		return a.phoneError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// RequestPhoneCode godoc
// @Summary Call my phone with a code
// @Description Call the number and speak a six-digit code that verifies it. One call at a time; a few per hour.
// @Tags auth
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 409 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Failure 429 {object} ErrorResponse
// @Failure 502 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone/verify-call [post]
func (a *API) RequestPhoneCode(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	ctx := c.Request().Context()
	contact, err := a.phone.GetPhoneContact(ctx, userID)
	if err != nil {
		return a.phoneError(c, err)
	}
	if contact == nil {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "add a phone number first"})
	}
	all, err := a.twilioIntegrations()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	through, ok := twilio.Pick(all, contact.Value, contact.PinnedIntegrationID)
	if !ok {
		return c.JSON(http.StatusUnprocessableEntity, ErrorResponse{Error: "no phone provider can call this number"})
	}

	// Capacity first, the code after: a refusal here must leave the code of a
	// call already ringing alone.
	reservation, err := a.reservePhoneCall(ctx, userID, "verify", contact.Value, through, all, false)
	if err != nil {
		return a.phoneError(c, err)
	}

	code, err := a.issuePhoneCode(ctx, userID, contact.Value)
	if err != nil {
		// Nothing was dialled; the capacity goes back.
		_ = a.phone.ReleasePhoneReservation(ctx, reservation.ID)
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to issue a code"})
	}
	spoken := twilio.SpokenDigits(code)
	text := fmt.Sprintf("Your TokayOps code is %s. Again: %s.", spoken, spoken)
	return a.placePhoneCall(c, reservation, through, contact.Value, text, "calling")
}

func (a *API) issuePhoneCode(ctx context.Context, userID, number string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < otpIssueRetries; attempt++ {
		code, err := generateSlackOTP()
		if err != nil {
			return "", err
		}
		if lastErr = a.phone.IssuePhoneCode(ctx, userID, number, code, phoneCodeTTL); lastErr == nil {
			return code, nil
		}
	}
	return "", lastErr
}

func (a *API) reservePhoneCall(ctx context.Context, userID, purpose, number string,
	through twilio.Integration, all []twilio.Integration, requireVerified bool) (*store.PhoneReservation, error) {

	cps, concurrent := twilio.AccountLimits(all, through.Config.AccountSID)
	return a.phone.ReservePhoneCall(ctx, store.PhoneCallRequest{
		UserID:          userID,
		Purpose:         purpose,
		ToNumber:        number,
		FromNumber:      through.Config.FromNumber,
		IntegrationID:   through.ID,
		AccountScope:    through.Config.AccountSID,
		CPS:             cps,
		MaxConcurrent:   concurrent,
		Hold:            phoneHold,
		RequireVerified: requireVerified,
	})
}

// placePhoneCall makes the call the reservation was taken for. A refusal gives
// the capacity back; an answer that never came keeps it, because the call may
// be ringing.
func (a *API) placePhoneCall(c echo.Context, reservation *store.PhoneReservation,
	through twilio.Integration, number, text, done string) error {

	callCtx, cancel := context.WithTimeout(c.Request().Context(), phoneCallTimeout)
	defer cancel()
	_, err := a.phoneCaller.CreateCall(callCtx, through.Config, twilio.Call{
		To:    number,
		TwiML: twilio.Say(through.Config.SayLanguage(), text),
		Ring:  phoneRing,
		Limit: phoneTalk,
	})
	switch {
	case err == nil:
		return c.JSON(http.StatusOK, map[string]string{"message": done})
	case errors.Is(err, twilio.ErrNotCreated):
		_ = a.phone.ReleasePhoneReservation(context.WithoutCancel(c.Request().Context()), reservation.ID)
		var refusal *twilio.Refusal
		if errors.As(err, &refusal) && refusal.Code == twilio.CodeInvalidNumber {
			return c.JSON(http.StatusUnprocessableEntity, ErrorResponse{Error: "the phone provider cannot call this number"})
		}
		return c.JSON(http.StatusBadGateway, ErrorResponse{Error: "the phone provider refused the call: " + err.Error()})
	default:
		return c.JSON(http.StatusBadGateway, ErrorResponse{
			Error: "the call may have gone through; wait a minute before asking again", Code: "call_outcome_unknown"})
	}
}

// ConfirmMyPhone godoc
// @Summary Confirm my phone
// @Description Enter the code the call spoke. A code spoken to an earlier number verifies nothing.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body ConfirmPhoneCodeRequest true "Code"
// @Success 200 {object} PhoneResponse
// @Failure 400 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone/confirm [post]
func (a *API) ConfirmMyPhone(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	var req ConfirmPhoneCodeRequest
	if err := c.Bind(&req); err != nil || strings.TrimSpace(req.Code) == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "code is required"})
	}
	ctx := c.Request().Context()
	if err := a.phone.ConfirmPhoneCode(ctx, userID, strings.TrimSpace(req.Code)); err != nil {
		return a.phoneError(c, err)
	}
	resp, err := a.phoneResponse(ctx, userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.JSON(http.StatusOK, resp)
}

// PinMyPhoneProvider godoc
// @Summary Prefer a phone provider
// @Description Prefer one of the providers that cover the number; an empty id clears the preference.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body PinPhoneRequest true "Provider"
// @Success 200 {object} PhoneResponse
// @Failure 409 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone/pin [put]
func (a *API) PinMyPhoneProvider(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	var req PinPhoneRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}
	ctx := c.Request().Context()
	contact, err := a.phone.GetPhoneContact(ctx, userID)
	if err != nil {
		return a.phoneError(c, err)
	}
	if contact == nil {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "add a phone number first"})
	}
	if req.IntegrationID != "" {
		all, err := a.twilioIntegrations()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		covers := false
		for _, in := range twilio.Covering(all, contact.Value) {
			covers = covers || in.ID == req.IntegrationID
		}
		if !covers {
			return c.JSON(http.StatusUnprocessableEntity, ErrorResponse{Error: "that provider cannot call this number"})
		}
	}
	if err := a.phone.PinPhoneIntegration(ctx, userID, contact.Value, req.IntegrationID); err != nil {
		return a.phoneError(c, err)
	}
	resp, err := a.phoneResponse(ctx, userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.JSON(http.StatusOK, resp)
}

// RequestPhoneTestCall godoc
// @Summary Test call from one sender number
// @Description Call the verified number from one sender number, to check the call gets through Do Not Disturb.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body PhoneSenderRequest true "Sender number"
// @Success 200 {object} map[string]string
// @Failure 409 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Failure 429 {object} ErrorResponse
// @Failure 502 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone/dnd-check [post]
func (a *API) RequestPhoneTestCall(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	var req PhoneSenderRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}
	ctx := c.Request().Context()
	contact, err := a.phone.GetPhoneContact(ctx, userID)
	if err != nil {
		return a.phoneError(c, err)
	}
	if !contact.Verified() {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "verify the number first"})
	}
	all, err := a.twilioIntegrations()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	var through *twilio.Integration
	for _, in := range twilio.Covering(all, contact.Value) {
		if in.Config.FromNumber == req.Sender {
			in := in
			through = &in
			break
		}
	}
	if through == nil {
		return c.JSON(http.StatusUnprocessableEntity, ErrorResponse{Error: "no provider that covers this number calls from that sender"})
	}
	reservation, err := a.reservePhoneCall(ctx, userID, "dnd_check", contact.Value, *through, all, true)
	if err != nil {
		return a.phoneError(c, err)
	}
	return a.placePhoneCall(c, reservation, *through, contact.Value,
		"This is a TokayOps test call. If your phone rang in Do Not Disturb, mark it in your profile.", "calling")
}

// ConfirmPhoneTestCall godoc
// @Summary Mark that a test call came through
// @Description Record that a call from a sender rang through Do Not Disturb. The person's own word; needs a verified number.
// @Tags auth
// @Accept json
// @Produce json
// @Param request body PhoneSenderRequest true "Sender number"
// @Success 200 {object} PhoneResponse
// @Failure 409 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone/dnd-confirm [post]
func (a *API) ConfirmPhoneTestCall(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	var req PhoneSenderRequest
	if err := c.Bind(&req); err != nil || req.Sender == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "sender is required"})
	}
	ctx := c.Request().Context()
	if err := a.phone.ConfirmDNDCheck(ctx, userID, req.Sender); err != nil {
		return a.phoneError(c, err)
	}
	resp, err := a.phoneResponse(ctx, userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.JSON(http.StatusOK, resp)
}

// GetMyPhoneVCard godoc
// @Summary Contact card of the sender numbers
// @Description A vCard named TokayOps with every number a call to this phone may come from, to add to contacts and allow through Do Not Disturb.
// @Tags auth
// @Produce text/vcard
// @Success 200 {string} string
// @Failure 409 {object} ErrorResponse
// @Failure 503 {object} ErrorResponse
// @Router /api/auth/me/phone/vcard [get]
func (a *API) GetMyPhoneVCard(c echo.Context) error {
	userID, ok := a.phoneUser(c)
	if !ok {
		return nil
	}
	if !a.phoneReady(c) {
		return nil
	}
	resp, err := a.phoneResponse(c.Request().Context(), userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	if len(resp.Senders) == 0 {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "no provider can call this number yet"})
	}
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:TokayOps\r\nN:;TokayOps;;;\r\n")
	for _, sender := range resp.Senders {
		b.WriteString("TEL;TYPE=WORK,VOICE:" + sender.Number + "\r\n")
	}
	b.WriteString("END:VCARD\r\n")
	c.Response().Header().Set("Content-Disposition", `attachment; filename="tokayops.vcf"`)
	return c.Blob(http.StatusOK, "text/vcard; charset=utf-8", []byte(b.String()))
}

// phoneError turns what the store refused into an answer.
func (a *API) phoneError(c echo.Context, err error) error {
	var refused *store.PhoneCallRefused
	switch {
	case errors.As(err, &refused):
		seconds := int(refused.RetryAfter.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		c.Response().Header().Set("Retry-After", fmt.Sprint(seconds))
		message := map[string]string{
			store.PhoneRefusedQuota:    "too many calls to this number; try again later",
			store.PhoneRefusedInFlight: "a call is already on its way; wait for it",
			store.PhoneRefusedBusy:     "the phone line is busy; try again shortly",
		}[refused.Reason]
		return c.JSON(http.StatusTooManyRequests, ErrorResponse{Error: message, Code: refused.Reason})
	case errors.Is(err, store.ErrPhoneContactChanged):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "the phone number changed; reload and try again"})
	case errors.Is(err, store.ErrPhoneNotVerified):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "verify the number first"})
	case errors.Is(err, store.ErrLinkTokenInvalid):
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid code"})
	case errors.Is(err, store.ErrLinkTokenExpired):
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "code expired or too many attempts; ask for a new call"})
	case errors.Is(err, store.ErrUserNotFound):
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
	default:
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
}
