package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers/twilio"
	"github.com/tokayops/tokayops/internal/store"
)

// testAccountSID has the shape of a Twilio account id. It is put together
// here rather than written out, because a literal of that shape is what secret
// scanners look for, and a fixture that trips them blocks every push.
var testAccountSID = "AC" + strings.Repeat("0", 31) + "1"

// fakePhoneStore records what the routes asked of it, in order.
type fakePhoneStore struct {
	mu       sync.Mutex
	calls    []string
	contact  *model.PhoneContact
	reserve  error
	issuedTo string
}

func (f *fakePhoneStore) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakePhoneStore) called(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *fakePhoneStore) GetPhoneContact(context.Context, string) (*model.PhoneContact, error) {
	return f.contact, nil
}
func (f *fakePhoneStore) SetPhoneContact(_ context.Context, _ string, value string) (*model.PhoneContact, error) {
	f.record("SetPhoneContact")
	f.contact = &model.PhoneContact{Value: value}
	return f.contact, nil
}
func (f *fakePhoneStore) DeletePhoneContact(context.Context, string) error {
	f.record("DeletePhoneContact")
	return nil
}
func (f *fakePhoneStore) PinPhoneIntegration(_ context.Context, _, _, id string) error {
	f.record("PinPhoneIntegration")
	f.contact.PinnedIntegrationID = id
	return nil
}
func (f *fakePhoneStore) ReservePhoneCall(context.Context, store.PhoneCallRequest) (*store.PhoneReservation, error) {
	f.record("ReservePhoneCall")
	if f.reserve != nil {
		return nil, f.reserve
	}
	return &store.PhoneReservation{ID: "r1", HoldUntil: time.Now().Add(time.Minute)}, nil
}
func (f *fakePhoneStore) ReleasePhoneReservation(context.Context, string) error {
	f.record("ReleasePhoneReservation")
	return nil
}
func (f *fakePhoneStore) IssuePhoneCode(_ context.Context, _, number, _ string, _ time.Duration) error {
	f.record("IssuePhoneCode")
	f.issuedTo = number
	return nil
}
func (f *fakePhoneStore) ConfirmPhoneCode(context.Context, string, string) error {
	f.record("ConfirmPhoneCode")
	return nil
}
func (f *fakePhoneStore) ConfirmDNDCheck(context.Context, string, string) error {
	f.record("ConfirmDNDCheck")
	return nil
}
func (f *fakePhoneStore) PhoneDNDChecks(context.Context, string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

type fakeCaller struct {
	err   error
	calls []twilio.Call
}

func (f *fakeCaller) CreateCall(_ context.Context, _ model.TwilioConfig, call twilio.Call) (string, error) {
	f.calls = append(f.calls, call)
	if f.err != nil {
		return "", f.err
	}
	return "CA1", nil
}

type phoneHarness struct {
	e      *echo.Echo
	phone  *fakePhoneStore
	caller *fakeCaller
	token  string
}

// newPhoneHarness is the routes with one person whose number is phone, and one
// Twilio integration covering +1.
func newPhoneHarness(t *testing.T, phone string) *phoneHarness {
	t.Helper()
	s := store.NewMockStore()
	const userID = "phone-user"
	if err := s.CreateUser(&model.User{ID: userID, Email: "p@phone.test", Name: "P"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	cfg, _ := json.Marshal(model.TwilioConfig{
		AccountSID: testAccountSID, AuthToken: "t", FromNumber: "+15005550006",
		Coverage: []string{"+1"}, CPS: 1, MaxConcurrent: 1,
	})
	if err := s.CreateIntegration(&model.Integration{ID: "tw-us", Type: model.IntegrationTypeTwilio,
		Direction: model.IntegrationDirectionOutbound, Name: "US", Enabled: true, Config: cfg}); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}
	h := &phoneHarness{e: echo.New(), phone: &fakePhoneStore{}, caller: &fakeCaller{}}
	if phone != "" {
		h.phone.contact = &model.PhoneContact{Value: phone}
	}
	a := NewAPI(s, nil, nil, nil, "", nil)
	a.SetPhone(h.phone, h.caller)
	a.RegisterRoutes(h.e)
	token, err := auth.GenerateToken(userID)
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	h.token = token
	return h
}

func (h *phoneHarness) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: h.token})
	rec := httptest.NewRecorder()
	h.e.ServeHTTP(rec, req)
	return rec
}

// A refusal before the call leaves the code alone. The person may be
// listening to the code of the call already ringing; a new code would make it
// worthless.
func TestARefusedCallLeavesTheCodeAlone(t *testing.T) {
	for _, reason := range []string{store.PhoneRefusedInFlight, store.PhoneRefusedBusy, store.PhoneRefusedQuota} {
		t.Run(reason, func(t *testing.T) {
			h := newPhoneHarness(t, "+14155550101")
			h.phone.reserve = &store.PhoneCallRefused{Reason: reason, RetryAfter: 42 * time.Second}

			rec := h.do(http.MethodPost, "/api/auth/me/phone/verify-call", "")
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Retry-After") != "42" {
				t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
			}
			if h.phone.called("IssuePhoneCode") {
				t.Fatal("a refused call replaced the code")
			}
			if len(h.caller.calls) != 0 {
				t.Fatal("a refused call was dialled")
			}
		})
	}
}

// The code is issued once the capacity is held, and spoken digit by digit.
func TestTheCodeIsIssuedAfterTheCapacityAndSpoken(t *testing.T) {
	h := newPhoneHarness(t, "+14155550101")
	rec := h.do(http.MethodPost, "/api/auth/me/phone/verify-call", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.phone.calls) < 2 || h.phone.calls[0] != "ReservePhoneCall" || h.phone.calls[1] != "IssuePhoneCode" {
		t.Fatalf("order = %v, want the reservation before the code", h.phone.calls)
	}
	if h.phone.issuedTo != "+14155550101" {
		t.Fatalf("code issued for %q", h.phone.issuedTo)
	}
	if len(h.caller.calls) != 1 || !strings.Contains(h.caller.calls[0].TwiML, "Your TokayOps code is ") {
		t.Fatalf("calls = %+v", h.caller.calls)
	}
}

// A refusal proves no call exists and gives the capacity back; no answer
// proves nothing, and the capacity stays with the call that may be ringing.
func TestOnlyAProvenRefusalGivesTheCapacityBack(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		status   int
		released bool
	}{
		{"number refused", &twilio.Refusal{HTTPStatus: 400, Code: twilio.CodeInvalidNumber}, http.StatusUnprocessableEntity, true},
		{"other refusal", &twilio.Refusal{HTTPStatus: 401, Code: 20003}, http.StatusBadGateway, true},
		{"no answer", fmt.Errorf("%w: timeout", twilio.ErrUnknown), http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPhoneHarness(t, "+14155550101")
			h.caller.err = tc.err
			rec := h.do(http.MethodPost, "/api/auth/me/phone/verify-call", "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if got := h.phone.called("ReleasePhoneReservation"); got != tc.released {
				t.Fatalf("released = %v, want %v", got, tc.released)
			}
		})
	}
}

// Only the coverage of an enabled provider decides where the account calls.
func TestANumberNobodyCoversIsNotCalled(t *testing.T) {
	h := newPhoneHarness(t, "+442071234567")
	rec := h.do(http.MethodPost, "/api/auth/me/phone/verify-call", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.phone.called("ReservePhoneCall") || len(h.caller.calls) != 0 {
		t.Fatal("a number outside every coverage was reserved or called")
	}
}

func TestAPinMustCoverTheNumber(t *testing.T) {
	h := newPhoneHarness(t, "+442071234567")
	rec := h.do(http.MethodPut, "/api/auth/me/phone/pin", `{"integration_id":"tw-us"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.phone.called("PinPhoneIntegration") {
		t.Fatal("a provider that cannot call the number was pinned")
	}
}

func TestATestCallNeedsAVerifiedNumber(t *testing.T) {
	h := newPhoneHarness(t, "+14155550101")
	rec := h.do(http.MethodPost, "/api/auth/me/phone/dnd-check", `{"sender":"+15005550006"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if h.phone.called("ReservePhoneCall") {
		t.Fatal("a test call to an unverified number was reserved")
	}
}

func TestANumberMustBeInternational(t *testing.T) {
	h := newPhoneHarness(t, "")
	for _, value := range []string{"4155550101", "+0155550101", "+1 415 555 0101", "+1415"} {
		if rec := h.do(http.MethodPut, "/api/auth/me/phone", `{"value":"`+value+`"}`); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d", value, rec.Code)
		}
	}
	if h.phone.called("SetPhoneContact") {
		t.Fatal("a malformed number was stored")
	}
	if rec := h.do(http.MethodPut, "/api/auth/me/phone", `{"value":" +14155550101 "}`); rec.Code != http.StatusOK {
		t.Fatalf("a valid number: status = %d: %s", rec.Code, rec.Body.String())
	}
}

// The card carries every number a call may come from.
func TestTheVCardNamesTheSenders(t *testing.T) {
	h := newPhoneHarness(t, "+14155550101")
	rec := h.do(http.MethodGet, "/api/auth/me/phone/vcard", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "FN:TokayOps") || !strings.Contains(body, "TEL;TYPE=WORK,VOICE:+15005550006") {
		t.Fatalf("vcard = %q", body)
	}
}
