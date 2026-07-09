package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// recordingMessenger captures SendDM calls so a test can read the OTP code that
// was delivered synchronously inside the request-code handler.
type recordingMessenger struct {
	mu      sync.Mutex
	lastDM  string
	dmCount int
	sendErr error
}

func (m *recordingMessenger) SendDM(_ context.Context, _, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	m.dmCount++
	m.lastDM = message
	return nil
}

// GetSlackUserIDByEmail / GetEmailBySlackID are unused by the OTP flow.
func (m *recordingMessenger) GetSlackUserIDByEmail(_ context.Context, _ string) (string, error) {
	return "", errors.New("not used in OTP flow")
}
func (m *recordingMessenger) GetEmailBySlackID(_ context.Context, _ string) (string, error) {
	return "", errors.New("not used in OTP flow")
}

// extractOTP pulls the 6-digit code out of the DM body the handler sends:
// "Your TokayOps One-Time Password is: <code>. It expires in 5 minutes."
func extractOTP(msg string) string {
	const prefix = "Your TokayOps One-Time Password is: "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(prefix):]
	if j := strings.Index(rest, "."); j >= 0 {
		return rest[:j]
	}
	return rest
}

// newSlackOTPAPI builds an API over a MockStore with a recording Slack messenger
// and a seeded, authenticated user. Returns a helper that issues authenticated
// requests as that user.
func newSlackOTPAPI(t *testing.T, userID string) (*store.MockStore, *recordingMessenger, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	s := store.NewMockStore()
	if err := s.CreateUser(&model.User{ID: userID, Email: userID + "@otp.test", Name: "OTP User"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	msgr := &recordingMessenger{}
	a := NewAPI(s, nil, msgr, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		token, err := auth.GenerateToken(userID)
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		req.AddCookie(&http.Cookie{Name: AuthCookieName, Value: token})
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	return s, msgr, do
}

// TestSlackOTP_RequestAndConfirm_HappyPath proves the live (non-job) OTP flow:
// request-code issues a token and DMs the code synchronously; confirm-code with
// that code creates the external_identity.
func TestSlackOTP_RequestAndConfirm_HappyPath(t *testing.T) {
	s, msgr, do := newSlackOTPAPI(t, "u1")

	rec := do(http.MethodPost, "/api/auth/me/slack/request-code", `{"slack_user_id":"U_SLACK_1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("request-code: got %d: %s", rec.Code, rec.Body.String())
	}
	// SendDM ran synchronously within the handler (the recorder only returns after
	// ServeHTTP completes, so the DM must already be captured).
	if msgr.dmCount != 1 {
		t.Fatalf("expected exactly 1 synchronous DM, got %d", msgr.dmCount)
	}
	code := extractOTP(msgr.lastDM)
	if len(code) != 6 {
		t.Fatalf("expected a 6-digit OTP in DM %q, extracted %q", msgr.lastDM, code)
	}

	// Wrong code must be rejected (400) and must not bind anything.
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	rec = do(http.MethodPost, "/api/auth/me/slack/confirm-code", `{"code":"`+wrong+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("wrong code: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Correct code binds the identity.
	rec = do(http.MethodPost, "/api/auth/me/slack/confirm-code", `{"code":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm-code: got %d: %s", rec.Code, rec.Body.String())
	}
	ids, err := s.ListUserIdentities("u1")
	if err != nil {
		t.Fatalf("ListUserIdentities: %v", err)
	}
	found := false
	for _, ei := range ids {
		if ei.Provider == "slack" && ei.ExternalID == "U_SLACK_1" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected slack identity U_SLACK_1 after confirm, got %+v", ids)
	}
}

// TestSlackOTP_ExpiredCode_Rejected confirms an expired token maps to 400.
func TestSlackOTP_ExpiredCode_Rejected(t *testing.T) {
	s, _, do := newSlackOTPAPI(t, "u1")

	// Issue a token that is already expired, bypassing request-code.
	if err := s.IssueLinkToken("u1", "slack", "U_SLACK_1", "654321", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("IssueLinkToken: %v", err)
	}
	rec := do(http.MethodPost, "/api/auth/me/slack/confirm-code", `{"code":"654321"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expired code: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSlackOTP_RequestCode_AlreadyLinkedToAnotherUser_Conflict confirms that
// requesting a code for a Slack account already bound to a different user is a 409.
func TestSlackOTP_RequestCode_AlreadyLinkedToAnotherUser_Conflict(t *testing.T) {
	s, msgr, do := newSlackOTPAPI(t, "u1")

	// Another user already owns U_TAKEN.
	if err := s.CreateUser(&model.User{ID: "u2", Email: "u2@otp.test", Name: "Other"}); err != nil {
		t.Fatalf("CreateUser u2: %v", err)
	}
	if err := s.BindExternalIdentity(&model.ExternalIdentity{UserID: "u2", Provider: "slack", ExternalID: "U_TAKEN"}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}

	rec := do(http.MethodPost, "/api/auth/me/slack/request-code", `{"slack_user_id":"U_TAKEN"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("already-linked: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if msgr.dmCount != 0 {
		t.Errorf("no DM should be sent when the slack account is already taken, got %d", msgr.dmCount)
	}
}
