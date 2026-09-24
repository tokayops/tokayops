package ingester

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/store"
)

// Who may send, and what they say about silence.
//
// The cache is a filter and a name: it is reloaded by whichever instance
// handled a change, so on every other one it still holds an integration that
// was disabled or a secret that was rotated. What a payload is allowed to do is
// settled against the database on the way in.

func intakeIngester(t *testing.T) (*store.MockStore, *echo.Echo) {
	t.Helper()
	s := store.NewMockStore()
	seedDefaultTeams(s)
	ing := NewIngester(s, &config.Config{}, &mockSecretValidator{secrets: map[string]bool{"secret123": true}})
	e := echo.New()
	ing.RegisterRoutes(e)
	return s, e
}

func postOneFiring(t *testing.T, e *echo.Echo, groupKey string) *httptest.ResponseRecorder {
	t.Helper()
	payload := `{"status":"firing","groupKey":"` + groupKey + `",` +
		`"commonLabels":{"team":"devops","severity":"critical","alertname":"A"},` +
		`"alerts":[{"status":"firing","labels":{"alertname":"A","team":"devops"},"fingerprint":"fp-1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestAnIntegrationThatMayNoLongerSendIsRefused, even though its secret is
// still in this instance's cache. Before this, an integration disabled on one
// instance went on being accepted by every other one until it restarted.
func TestAnIntegrationThatMayNoLongerSendIsRefused(t *testing.T) {
	s, e := intakeIngester(t)
	s.RefuseIntake(true)

	if rec := postOneFiring(t, e, "refused"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the webhook answered %d, want 401", rec.Code)
	}
	if ag, _ := s.GetActiveAlertGroupByAlertKey("refused"); ag != nil {
		t.Error("a payload from an integration that may not send opened an incident")
	}
}

// TestAnIntakeThatCannotBeCheckedAsksAlertmanagerToComeBack. The database that
// would answer is the database the payload would be stored in, so turning
// Alertmanager away would lose the alert; 500 is the answer that keeps it.
func TestAnIntakeThatCannotBeCheckedAsksAlertmanagerToComeBack(t *testing.T) {
	s, e := intakeIngester(t)
	s.FailIntake(errors.New("the database is having a bad minute"))

	if rec := postOneFiring(t, e, "unchecked"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("the webhook answered %d, want 500", rec.Code)
	}
}

// TestWhatTheIntegrationDeclaresReachesTheIncident, on the payload that opens
// it and on the ones after.
func TestWhatTheIntegrationDeclaresReachesTheIncident(t *testing.T) {
	s, e := intakeIngester(t)
	s.SetIntakeStaleAfter(14700)

	if rec := postOneFiring(t, e, "declared"); rec.Code != http.StatusOK {
		t.Fatalf("the webhook answered %d: %s", rec.Code, rec.Body.String())
	}
	ag, err := s.GetActiveAlertGroupByAlertKey("declared")
	if err != nil || ag == nil {
		t.Fatalf("the incident was not opened: %v", err)
	}
	if ag.StaleAfterSeconds == nil || *ag.StaleAfterSeconds != 14700 {
		t.Fatalf("the incident says %v, want 14700", ag.StaleAfterSeconds)
	}

	// The operator clears the field, and the next payload says so.
	s.SetIntakeStaleAfter(0)
	if rec := postOneFiring(t, e, "declared"); rec.Code != http.StatusOK {
		t.Fatalf("the webhook answered %d: %s", rec.Code, rec.Body.String())
	}
	ag, err = s.GetActiveAlertGroupByAlertKey("declared")
	if err != nil || ag == nil {
		t.Fatalf("read the incident: %v", err)
	}
	if ag.StaleAfterSeconds != nil {
		t.Errorf("the incident still says %v after the field was cleared", *ag.StaleAfterSeconds)
	}
}

// TestAnUnknownTokenNeverReachesTheDatabase. The cache answers first, so a
// token nobody has costs nothing but a lookup.
func TestAnUnknownTokenNeverReachesTheDatabase(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	s.FailIntake(errors.New("nobody should have asked"))
	ing := NewIngester(s, &config.Config{}, &mockSecretValidator{secrets: map[string]bool{"secret123": true}})
	e := echo.New()
	ing.RegisterRoutes(e)

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=nobody", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("the webhook answered %d, want 401", rec.Code)
	}
}
