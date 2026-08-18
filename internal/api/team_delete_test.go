package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Deleting a team used to answer 500 with the text of a Postgres constraint
// whenever anything referenced the team. There were two such things, and a
// third state in which the delete silently destroyed data. All four outcomes
// are pinned here, because the difference between them is the whole point of
// moving the operation into a transaction.

func TestDeleteTeamWithoutBlockersIs204(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete an unencumbered team: want 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.GetTeamByID("devops"); err == nil {
		t.Fatal("the team is still there after a 204")
	}
}

func TestDeleteTeamRemovesMembershipsWithIt(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	if err := s.AddTeamMember("devops", "alex", model.TeamMemberRoleMember); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}

	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// team_members -> teams has no ON DELETE action, so nothing removes these
	// rows on their own. Losing this leaves memberships pointing at a team
	// that no longer exists.
	members, err := s.GetTeamMembers("devops")
	if err == nil && len(members) != 0 {
		t.Fatalf("memberships outlived the team: %v", members)
	}
}

func TestDeleteTeamWithScheduleHistoryIs409(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, env *scheduleTestEnv)
	}{
		{name: "active schedule"},
		{
			// A soft-deleted schedule still owns its revisions, which is the
			// entire point of soft-deleting it. Deleting the schedule first is
			// not a way around this.
			name: "soft-deleted schedule",
			setup: func(t *testing.T, env *scheduleTestEnv) {
				t.Helper()
				*env.now = env.now.Add(time.Hour)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, e, env := setupScheduleAPI(t)
			defer s.Close()

			createSchedule(t, e, []string{"denis"})
			scheduleID := scheduleIDOf(t, env, "devops")
			if tc.setup != nil {
				tc.setup(t, env)
				rec := doJSON(t, e, http.MethodDelete,
					"/api/v1/teams/devops/schedule?expected_version=1", nil, "denis")
				if rec.Code != http.StatusNoContent {
					t.Fatalf("soft-delete the schedule: got %d: %s", rec.Code, rec.Body.String())
				}
			}

			rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops", nil, "denis")
			if rec.Code != http.StatusConflict {
				t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body.String())
			}

			var body map[string]any
			decodeJSON(t, rec, &body)
			if body["code"] != CodeTeamHasScheduleHistory {
				t.Errorf("code = %v, want %s", body["code"], CodeTeamHasScheduleHistory)
			}
			// The refusal names the row that retains the team, so an operator
			// reading it does not have to go looking.
			if body["schedule_id"] != scheduleID {
				t.Errorf("schedule_id = %v, want %s", body["schedule_id"], scheduleID)
			}
			if _, err := s.GetTeamByID("devops"); err != nil {
				t.Error("a refused delete must leave the team alone")
			}
		})
	}
}

func TestDeleteTeamWithTeamScopedIntegrationIs409(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	teamID := "devops"
	scope := model.WebhookScopeTeam
	if err := s.CreateIntegration(&model.Integration{
		ID:        "int-team-webhook",
		Type:      model.IntegrationTypeGenericWebhook,
		Direction: model.IntegrationDirectionOutbound,
		Name:      "team webhook",
		Enabled:   true,
		Scope:     &scope,
		TeamID:    &teamID,
	}); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops", nil, "denis")
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["code"] != CodeTeamHasIntegrations {
		t.Errorf("code = %v, want %s", body["code"], CodeTeamHasIntegrations)
	}
	if _, err := s.GetTeamByID("devops"); err != nil {
		t.Error("a refused delete must leave the team alone")
	}
}

// Both blockers at once answer about the schedule, because that answer is
// terminal: history cannot be removed, so listing a removable blocker next to
// it would suggest a way forward that does not exist.
func TestDeleteTeamAnswersAboutTheTerminalBlockerFirst(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	createSchedule(t, e, []string{"denis"})

	teamID := "devops"
	scope := model.WebhookScopeTeam
	if err := s.CreateIntegration(&model.Integration{
		ID: "int-team-webhook", Type: model.IntegrationTypeGenericWebhook,
		Direction: model.IntegrationDirectionOutbound, Name: "team webhook",
		Enabled: true, Scope: &scope, TeamID: &teamID,
	}); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops", nil, "denis")
	var body map[string]any
	decodeJSON(t, rec, &body)
	if rec.Code != http.StatusConflict || body["code"] != CodeTeamHasScheduleHistory {
		t.Fatalf("want 409 %s, got %d %v", CodeTeamHasScheduleHistory, rec.Code, body["code"])
	}
}

// TestDeleteTeamRefusesOnTheExistenceOfARootNotItsContents pins the reason the
// delete is safe, which is narrower than it is tempting to state.
//
// A root with no revision chain owns nothing for ON DELETE RESTRICT to defend,
// so the cascade from teams would take it without a word. Nothing rules such a
// root out either: history_complete_from NOT NULL closes the pre-revision row,
// not the general case - raw SQL can still write a root with a horizon and no
// chain. What makes the delete safe is that it refuses as soon as a root
// EXISTS, without asking what is in it, and this test is the thing that fails
// if that check is ever narrowed to some property of the row.
func TestDeleteTeamRefusesOnTheExistenceOfARootNotItsContents(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.Config.SeedRoot(scheduleconfig.ScheduleRoot{
		ID: "chainless-1", TeamID: "devops", ConfigVersion: 1,
		HistoryCompleteFrom: time.Now().UTC(),
	})

	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops", nil, "denis")
	var body map[string]any
	decodeJSON(t, rec, &body)
	if rec.Code != http.StatusConflict || body["code"] != CodeTeamHasScheduleHistory {
		t.Fatalf("want 409 %s, got %d %v", CodeTeamHasScheduleHistory, rec.Code, body["code"])
	}
	if _, err := s.GetTeamByID("devops"); err != nil {
		t.Error("the team was deleted despite the refusal")
	}
	if _, ok := env.Config.RootByTeam("devops"); !ok {
		t.Error("the schedule row was destroyed; that is the defect this test exists for")
	}
}

func TestDeleteTeamThatDoesNotExistIs404(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	// Answered by the row lock inside the transaction rather than by a read
	// before it: between such a read and the write, the team can go away.
	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/ghost", nil, "denis")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["code"] != CodeTeamNotFound {
		t.Errorf("code = %v, want %s", body["code"], CodeTeamNotFound)
	}
}
