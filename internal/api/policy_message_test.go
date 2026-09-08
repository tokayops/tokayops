package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// TestAStepMessageIsATemplateOverFourNames. A policy is saved only with a
// message that renders: the four names are fine, a name the alert does not
// have or a template that does not parse is refused with the reason, and
// plain words are plain words. The two settings nothing reads any more are
// accepted whatever they say.
func TestAStepMessageIsATemplateOverFourNames(t *testing.T) {
	e := echo.New()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	s.CreateUser(admin)

	save := func(t *testing.T, step PolicyStepRequest) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(PolicyRequest{Name: "Words", Steps: []PolicyStepRequest{step}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/policies", bytes.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.Set("user_id", admin.ID)
		_ = api.CreatePolicy(c)
		return rec
	}
	dm := func(message string) PolicyStepRequest {
		return PolicyStepRequest{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "u-1", Message: message}
	}

	for _, message := range []string{"", "please look at db-1", "{{.Title}} is {{.Severity}} for {{.Team}}, {{.AlertsCount}} alert(s)"} {
		if rec := save(t, dm(message)); rec.Code != http.StatusCreated {
			t.Errorf("%q was refused: %d %s", message, rec.Code, rec.Body.String())
		}
	}
	for message, wants := range map[string]string{"{{.Nope}}": "Nope", "{{.Title": "parse"} {
		rec := save(t, dm(message))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), wants) {
			t.Errorf("%q was answered %d %s, want 400 naming %q", message, rec.Code, rec.Body.String(), wants)
		}
	}

	ignored := dm("")
	ignored.TimeoutSeconds, ignored.MaxAttempts = 999999, 100000
	if rec := save(t, ignored); rec.Code != http.StatusCreated {
		t.Errorf("a step naming the settings nothing reads was refused: %d %s", rec.Code, rec.Body.String())
	}
}
