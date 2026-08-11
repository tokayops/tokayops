//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/testutil"
)

// seedRevisionSchedule creates a schedule through the API and returns its
// config_version. Going through the endpoint rather than writing rows is what
// makes these fixtures the same thing a user would have: a schedule root and
// its first revision, written together.
func seedRevisionSchedule(t *testing.T, env *APIIntegrationEnv, teamID, actorID string, groups ...[]string) int64 {
	t.Helper()
	monday := 1
	ids := [...]string{"5f0a1e2c-3333-4a3b-8c4d-000000000001", "5f0a1e2c-3333-4a3b-8c4d-000000000002"}
	dto := make([]api.ScheduleGroupDTO, len(groups))
	for i, members := range groups {
		dto[i] = api.ScheduleGroupDTO{ID: ids[i], UserIDs: members}
	}
	body, err := json.Marshal(api.PutScheduleConfigRequest{
		ScheduleConfigDTO: api.ScheduleConfigDTO{
			Timezone: "UTC",
			L1: api.ScheduleL1DTO{
				Enabled: true, RotationType: "daily", HandoffTime: "09:00", Groups: dto,
			},
			L2: api.ScheduleL2DTO{
				EscalationTimeoutMinutes: 5, RotationType: "weekly",
				HandoffTime: "09:00", HandoffDay: &monday,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	req := createAuthenticatedRequest(t, http.MethodPut,
		"/api/v1/teams/"+teamID+"/schedule/config", body, actorID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create schedule config: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out api.PutScheduleConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	return out.Version
}

// scheduleIDOfTeam reads the schedule ID from the configuration endpoint.
func scheduleIDOfTeam(t *testing.T, env *APIIntegrationEnv, teamID, actorID string) string {
	t.Helper()
	req := createAuthenticatedRequest(t, http.MethodGet,
		"/api/v1/teams/"+teamID+"/schedule/config", nil, actorID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out api.ScheduleConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return out.ScheduleID
}

// Overlap is a property of the current override projection, so the service
// checks it under the schedule lock rather than leaning on a row constraint.
func TestSchedule_Overrides_Conflict(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "sched-team")
	user1 := testutil.SeedUser(t, env.S, "u1@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, user1.ID, model.TeamMemberRoleMember)
	seedRevisionSchedule(t, env, team.ID, admin.ID, []string{user1.ID})

	createOverride := func(from, to time.Time) int {
		body, _ := json.Marshal(api.ScheduleOverrideRequest{
			UserID: user1.ID, ValidFrom: from, ValidTo: to,
		})
		req := createAuthenticatedRequest(t, http.MethodPost,
			"/api/v1/teams/"+team.ID+"/schedule/overrides", body, admin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)
		return rec.Code
	}

	now := time.Now().UTC().Truncate(time.Minute)
	if code := createOverride(now.Add(time.Hour), now.Add(3*time.Hour)); code != http.StatusCreated {
		t.Fatalf("first override: want 201, got %d", code)
	}
	if code := createOverride(now.Add(2*time.Hour), now.Add(4*time.Hour)); code != http.StatusConflict {
		t.Errorf("overlapping override: want 409, got %d", code)
	}
	// Adjacent, not overlapping: the interval is half-open.
	if code := createOverride(now.Add(3*time.Hour), now.Add(4*time.Hour)); code != http.StatusCreated {
		t.Errorf("adjacent override: want 201, got %d", code)
	}
}

func TestSchedule_Delete(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "del-team")
	user1 := testutil.SeedUser(t, env.S, "del-u1@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, user1.ID, model.TeamMemberRoleMember)
	version := seedRevisionSchedule(t, env, team.ID, admin.ID, []string{user1.ID})

	deleteAt := func(expected int64) *httptest.ResponseRecorder {
		req := createAuthenticatedRequest(t, http.MethodDelete,
			fmt.Sprintf("/api/v1/teams/%s/schedule?expected_version=%d", team.ID, expected), nil, admin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)
		return rec
	}

	if rec := deleteAt(version); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// The configuration endpoint keeps answering, with deleted_at set: the
	// editor prefills a recreate from it rather than making a second request.
	req := createAuthenticatedRequest(t, http.MethodGet,
		"/api/v1/teams/"+team.ID+"/schedule/config", nil, admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET config after delete: want 200, got %d", rec.Code)
	}
	var cfg api.ScheduleConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.DeletedAt == nil {
		t.Fatal("a deleted schedule must report deleted_at")
	}

	// Deleting again is a conflict, not a second delete.
	if rec := deleteAt(cfg.Version); rec.Code != http.StatusConflict {
		t.Errorf("double delete: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSchedule_Creation_Validation(t *testing.T) {
	// Simple test to ensure API validates dependencies
	env := setupAPITest(t)
	admin := testutil.SeedUser(t, env.S, "admin@example.com")

	// Try creating override for non-existent team
	req := createAuthenticatedRequest(t, http.MethodPost, "/api/v1/teams/non-existent/schedule/overrides", []byte("{}"), admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for non-existent team, got %d", rec.Code)
	}
}

// TestSchedule_RenderSchedule_MultiUserGroup checks that a rendered shift for
// a multi-person group names everyone in it.
func TestSchedule_RenderSchedule_MultiUserGroup(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "render-multi-team")
	uA := testutil.SeedUser(t, env.S, "ra@example.com")
	uB := testutil.SeedUser(t, env.S, "rb@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, uA.ID, model.TeamMemberRoleMember)
	testutil.SeedTeamMember(t, env.S, team.ID, uB.ID, model.TeamMemberRoleMember)
	seedRevisionSchedule(t, env, team.ID, admin.ID, []string{uA.ID, uB.ID})

	resp := renderRange(t, env, team.ID, admin.ID,
		time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(24*time.Hour))
	if len(resp.Entries) == 0 {
		t.Fatal("expected at least one rendered shift")
	}

	found := false
	for _, e := range resp.Entries {
		if e.Layer != "l1" || len(e.UserIDs) != 2 {
			continue
		}
		got := map[string]bool{e.UserIDs[0]: true, e.UserIDs[1]: true}
		if got[uA.ID] && got[uB.ID] {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no L1 shift named both members: %+v", resp.Entries)
	}
}

// TestSchedule_RenderSchedule_OverrideMetadata checks that an override shows up
// in the rendered range carrying the identity a caller can act on.
func TestSchedule_RenderSchedule_OverrideMetadata(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "render-ovr-team")
	base := testutil.SeedUser(t, env.S, "base@example.com")
	stand := testutil.SeedUser(t, env.S, "stand@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, base.ID, model.TeamMemberRoleMember)
	testutil.SeedTeamMember(t, env.S, team.ID, stand.ID, model.TeamMemberRoleMember)
	seedRevisionSchedule(t, env, team.ID, admin.ID, []string{base.ID})

	from := time.Now().UTC().Truncate(time.Minute).Add(time.Hour)
	until := from.Add(2 * time.Hour)
	reason := "swap"
	body, _ := json.Marshal(api.ScheduleOverrideRequest{
		UserID: stand.ID, ValidFrom: from, ValidTo: until, Reason: &reason,
	})
	req := createAuthenticatedRequest(t, http.MethodPost,
		"/api/v1/teams/"+team.ID+"/schedule/overrides", body, admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create override: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created api.ScheduleOverrideDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode override: %v", err)
	}

	resp := renderRange(t, env, team.ID, admin.ID, from.Add(-time.Hour), until.Add(time.Hour))
	var got *api.ShiftDTO
	for i := range resp.Entries {
		if resp.Entries[i].Source == "override" {
			got = &resp.Entries[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no override entry in the rendered range: %+v", resp.Entries)
	}
	if got.OverrideID != created.OverrideID {
		t.Errorf("override_id = %q, want %q", got.OverrideID, created.OverrideID)
	}
	if len(got.UserIDs) != 1 || got.UserIDs[0] != stand.ID {
		t.Errorf("override entry names %v, want the stand-in %s", got.UserIDs, stand.ID)
	}
	if !got.Start.Equal(from) || !got.End.Equal(until) {
		t.Errorf("override entry spans %v..%v, want %v..%v", got.Start, got.End, from, until)
	}
	// The override interrupts the rotation, so the base user is still on duty
	// on either side of it.
	if len(resp.Entries) < 2 {
		t.Errorf("expected the rotation to resume around the override: %+v", resp.Entries)
	}
}

// renderRange calls the render endpoint and decodes the response.
func renderRange(t *testing.T, env *APIIntegrationEnv, teamID, actorID string, from, until time.Time) api.ScheduleRenderResponse {
	t.Helper()
	url := fmt.Sprintf("/api/v1/teams/%s/schedule/render?from=%s&until=%s",
		teamID, from.Format(time.RFC3339), until.Format(time.RFC3339))
	req := createAuthenticatedRequest(t, http.MethodGet, url, nil, actorID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET render: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out api.ScheduleRenderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode render: %v", err)
	}
	return out
}

// The Slack usergroup travels in the configuration snapshot, so setting it and
// clearing it has to survive a round trip through the editor's own endpoints.
// The syncer reads it from there and from nowhere else, which is what makes an
// unsaved usergroup a silently dead integration rather than a visible error.
func TestSchedule_SlackUsergroupID_Persistence(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "usergroup-team")
	user1 := testutil.SeedUser(t, env.S, "oncall@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, user1.ID, model.TeamMemberRoleMember)

	monday := 1
	put := func(version int64, usergroup string) api.PutScheduleConfigResponse {
		t.Helper()
		body, err := json.Marshal(api.PutScheduleConfigRequest{
			ExpectedVersion: version,
			ScheduleConfigDTO: api.ScheduleConfigDTO{
				Timezone:         "UTC",
				SlackUsergroupID: usergroup,
				L1: api.ScheduleL1DTO{
					Enabled: true, RotationType: "daily", HandoffTime: "11:00",
					Groups: []api.ScheduleGroupDTO{{
						ID:      "5f0a1e2c-3333-4a3b-8c4d-000000000001",
						UserIDs: []string{user1.ID},
					}},
				},
				L2: api.ScheduleL2DTO{
					EscalationTimeoutMinutes: 5, RotationType: "weekly",
					HandoffTime: "11:00", HandoffDay: &monday,
				},
			},
		})
		if err != nil {
			t.Fatalf("marshal config: %v", err)
		}
		req := createAuthenticatedRequest(t, http.MethodPut,
			"/api/v1/teams/"+team.ID+"/schedule/config", body, admin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("save config: want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var out api.PutScheduleConfigResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode save: %v", err)
		}
		return out
	}

	get := func() api.ScheduleConfigResponse {
		t.Helper()
		req := createAuthenticatedRequest(t, http.MethodGet,
			"/api/v1/teams/"+team.ID+"/schedule/config", nil, admin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET config: want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var out api.ScheduleConfigResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode config: %v", err)
		}
		return out
	}

	created := put(0, "S12345678")
	if got := get().Config.SlackUsergroupID; got != "S12345678" {
		t.Errorf("slack_usergroup_id = %q, wanted it persisted", got)
	}

	// Clearing it is the case worth its own step: an empty string has to reach
	// the snapshot as an empty string rather than be dropped as "unchanged",
	// or a team could never stop syncing a usergroup.
	put(created.Version, "")
	if got := get().Config.SlackUsergroupID; got != "" {
		t.Errorf("slack_usergroup_id = %q, want it cleared", got)
	}
}
