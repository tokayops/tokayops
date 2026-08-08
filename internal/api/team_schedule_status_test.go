package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/store"
)

// on_call_configured used to be read from the legacy schedule row, and the
// read now refuses a revision-managed schedule outright. The loop discarded
// that error, so the insurance meant to make the two eras loud instead made
// every configured team report itself as unconfigured.
func TestListTeamsReportsRevisionSchedulesAsConfigured(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	if configuredTeams(t, e)["devops"] {
		t.Fatal("devops has no schedule yet")
	}

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	createSchedule(t, e, []string{"denis"})

	if !configuredTeams(t, e)["devops"] {
		t.Fatal("a team with a revision-managed schedule must report on_call_configured")
	}
}

// A soft-deleted schedule is not a configured one: the row survives so history
// stays replayable, and reporting it as configured would tell an operator that
// someone is on call when the projection answers nobody.
func TestListTeamsTreatsDeletedScheduleAsUnconfigured(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	created := createSchedule(t, e, []string{"denis"})

	env.SetNow(time.Date(2026, 5, 6, 16, 0, 0, 0, time.UTC))
	rec := doJSON(t, e, http.MethodDelete,
		"/api/v1/teams/devops/schedule?expected_version=1", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete (version %d): want 204, got %d: %s", created.Version, rec.Code, rec.Body.String())
	}

	if configuredTeams(t, e)["devops"] {
		t.Fatal("a deleted schedule must not report on_call_configured")
	}
}

// A row from before the revision model has no configuration in this model, so
// it is not configured here either - the same answer GET /config gives it.
func TestListTeamsTreatsLegacyRootAsUnconfigured(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.Config.SeedLegacyRoot("legacy-1", "devops")

	if configuredTeams(t, e)["devops"] {
		t.Fatal("a pre-revision schedule must not report on_call_configured")
	}
}

// The defect was a swallowed error. A repository failure now fails the
// request: answering 200 with every team marked unconfigured is a wrong
// answer that looks like a fact.
func TestListTeamsFailsOnRepositoryError(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.Config.FailOn["WithinSnapshot"] = errors.New("connection reset")

	rec := doJSON(t, e, http.MethodGet, "/api/v1/teams", nil, "denis")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the schedule repository fails, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The schedule stack is optional wiring. Without it there is nothing to report
// and the team list still has to answer.
func TestListTeamsWorksWithoutScheduleStack(t *testing.T) {
	s := store.NewMockStore()
	defer s.Close()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	rec := doJSON(t, e, http.MethodGet, "/api/v1/teams", nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 from an unwired API, got %d: %s", rec.Code, rec.Body.String())
	}
	if configuredTeamsFrom(t, rec)["devops"] {
		t.Fatal("without the schedule stack no team can be reported as configured")
	}
}

func configuredTeams(t *testing.T, e *echo.Echo) map[string]bool {
	t.Helper()
	rec := doJSON(t, e, http.MethodGet, "/api/v1/teams", nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("list teams: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	return configuredTeamsFrom(t, rec)
}

func configuredTeamsFrom(t *testing.T, rec *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var out TeamListResponse
	decodeJSON(t, rec, &out)
	configured := make(map[string]bool, len(out.Teams))
	for _, team := range out.Teams {
		configured[team.ID] = team.OnCallConfigured
	}
	return configured
}
