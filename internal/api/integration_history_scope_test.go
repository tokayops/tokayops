package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// The history of a deleted webhook stays readable, by the people who could read
// it while it lived, through the two reading routes and no other.

func historyEnv(t *testing.T) (*store.MockStore, *echo.Echo) {
	t.Helper()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)
	// kim administers devops without being an administrator of the installation.
	if err := s.CreateUser(&model.User{ID: "kim", Email: "kim@example.com", Name: "Kim", Role: model.UserRoleUser}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember("devops", "kim", model.TeamMemberRoleAdmin); err != nil {
		t.Fatal(err)
	}
	return s, e
}

// deletedIntegration creates and deletes one integration. The id is given: the
// double numbers integrations by how many it holds, so after a deletion it
// would hand the next one the same id, and the tombstones would collide.
func deletedIntegration(t *testing.T, s *store.MockStore, id string, kind model.IntegrationType, scope model.WebhookScope, team string) string {
	t.Helper()
	integration := &model.Integration{ID: id, Type: kind, Direction: model.IntegrationDirectionOutbound, Name: "gone",
		Enabled: true, Config: json.RawMessage(`{"url":"https://example.com/hook","token":"x"}`)}
	if kind == model.IntegrationTypeGenericWebhook {
		integration.Scope = &scope
		if scope == model.WebhookScopeTeam {
			integration.TeamID = &team
		}
	}
	if err := s.CreateIntegration(integration); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteIntegration(context.Background(), integration.ID, "nina"); err != nil {
		t.Fatal(err)
	}
	return integration.ID
}

func statusFor(e *echo.Echo, method, path, user string) int {
	req := httptest.NewRequest(method, path, nil)
	addAuth(req, user)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

func TestTheHistoryOfADeletedWebhookStaysReadableByThoseWhoCouldReadIt(t *testing.T) {
	s, e := historyEnv(t)
	teamHook := deletedIntegration(t, s, "gone-team-hook", model.IntegrationTypeGenericWebhook, model.WebhookScopeTeam, "devops")
	globalHook := deletedIntegration(t, s, "gone-global-hook", model.IntegrationTypeGenericWebhook, model.WebhookScopeGlobal, "")
	slack := deletedIntegration(t, s, "gone-slack", model.IntegrationTypeSlack, "", "")

	for _, tc := range []struct {
		name   string
		method string
		path   string
		user   string
		want   int
	}{
		{"admin lists the team hook's history", http.MethodGet, "/api/v1/integrations/" + teamHook + "/deliveries", "denis", http.StatusOK},
		{"the team's admin lists it too", http.MethodGet, "/api/v1/integrations/" + teamHook + "/deliveries", "kim", http.StatusOK},
		{"a team member does not", http.MethodGet, "/api/v1/integrations/" + teamHook + "/deliveries", "alex", http.StatusNotFound},
		{"admin lists the global hook's history", http.MethodGet, "/api/v1/integrations/" + globalHook + "/deliveries", "denis", http.StatusOK},
		{"a team admin does not see a global hook", http.MethodGet, "/api/v1/integrations/" + globalHook + "/deliveries", "kim", http.StatusNotFound},
		{"a detail route resolves the same way; the delivery itself decides the rest", http.MethodGet,
			"/api/v1/integrations/" + teamHook + "/deliveries/none", "alex", http.StatusNotFound},
		{"a deleted Slack integration has no history route", http.MethodGet, "/api/v1/integrations/" + slack + "/deliveries", "denis", http.StatusNotFound},
		{"an integration that never existed", http.MethodGet, "/api/v1/integrations/never/deliveries", "denis", http.StatusNotFound},
		{"a replay to a deleted subscriber is not resolved", http.MethodPost,
			"/api/v1/integrations/" + teamHook + "/deliveries/none/replay", "denis", http.StatusNotFound},
		{"the integration itself is gone", http.MethodGet, "/api/v1/integrations/" + teamHook, "denis", http.StatusNotFound},
	} {
		if got := statusFor(e, tc.method, tc.path, tc.user); got != tc.want {
			t.Errorf("%s: %s %s as %s answered %d, want %d", tc.name, tc.method, tc.path, tc.user, got, tc.want)
		}
	}
}
