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

// seedRevisionSchedule creates a schedule through the revision-model endpoint
// and returns its config_version. The legacy store writer is deliberately not
// used: a row it produces is refused by the new commands, which is the point
// of the guard.
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

func TestSchedule_Timezones_And_Updates(t *testing.T) {
	env := setupAPITest(t)

	// Setup Data
	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "tz-team")
	user1 := testutil.SeedUser(t, env.S, "u1-tz@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, user1.ID, model.TeamMemberRoleMember)

	// 1. Create Schedule with Specific Timezone via API
	createReq := map[string]interface{}{
		"team_id":           team.ID,
		"timezone":          "Europe/Moscow",
		"l1_rotation_type":  "weekly",
		"l1_handoff_time":   "09:00",
		"l1_handoff_day":    1, // Monday
		"l1_rotation_start": time.Now().Format(time.RFC3339),
		"l1_users":          []string{user1.ID},
	}
	jsonBody, _ := json.Marshal(createReq)
	req := createAuthenticatedRequest(t, http.MethodPut, "/api/v1/teams/"+team.ID+"/schedule", jsonBody, admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("Expected 200 OK or 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	// 1.1 Set Groups for the Schedule (Required as Upsert doesn't handle groups - mimicking UI behavior)
	groupsReq := map[string]interface{}{
		"groups": [][]string{{user1.ID}},
	}
	groupsJson, _ := json.Marshal(groupsReq)
	reqGroups := createAuthenticatedRequest(t, http.MethodPut, "/api/v1/teams/"+team.ID+"/schedule/l1-groups", groupsJson, admin.ID)
	recGroups := httptest.NewRecorder()
	env.Echo.ServeHTTP(recGroups, reqGroups)
	if recGroups.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for setting groups, got %d: %s", recGroups.Code, recGroups.Body.String())
	}

	// Verify DB
	sched, err := env.S.GetScheduleByTeamID(team.ID)
	if err != nil {
		t.Fatalf("Failed to fetch schedule: %v", err)
	}
	if sched.Timezone != "Europe/Moscow" {
		t.Errorf("Expected timezone Europe/Moscow, got %s", sched.Timezone)
	}
	// Verify L1 Users persistence via RotationEpochs
	// We didn't seed users into the rotation_epochs table, and UpsertTeamSchedule might not be creating one.
	// But GetScheduleByTeamID doesn't load epochs either.
	// So we need to fetch epochs to see if any exist.
	epochs, err := env.S.GetRotationEpochs(sched.ID, "l1", time.Now().Add(-24*time.Hour), time.Now().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Failed to fetch epochs: %v", err)
	}
	if len(epochs) == 0 {
		t.Fatalf("Expected at least one rotation epoch after creation, got 0. L1Users might have been dropped.")
	}
	if len(epochs[0].Groups) == 0 || len(epochs[0].Groups[0]) == 0 || epochs[0].Groups[0][0] != user1.ID {
		t.Errorf("Expected user %s in epoch groups, got %v", user1.ID, epochs[0].Groups)
	}

	// 2. Update Schedule (Change Timezone and Handoff) - PUT is a full replacement
	// First fetch existing to keep other fields
	sched, err = env.S.GetScheduleByTeamID(team.ID)
	if err != nil {
		t.Fatalf("Failed to fetch schedule for update: %v", err)
	}

	updateReq := map[string]interface{}{
		"team_id":           team.ID,
		"timezone":          "Asia/Tokyo", // Changed
		"l1_rotation_type":  sched.L1RotationType,
		"l1_handoff_time":   "18:00", // Changed
		"l1_handoff_day":    sched.L1HandoffDay,
		"l1_rotation_start": sched.L1RotationStart.Format(time.RFC3339),
		"l1_users":          []string{user1.ID},
	}
	jsonBody, _ = json.Marshal(updateReq)
	req = createAuthenticatedRequest(t, http.MethodPut, "/api/v1/teams/"+team.ID+"/schedule", jsonBody, admin.ID)
	rec = httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for update, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2.1 Set Groups for the Updated Schedule (Mimic UI)
	groupsReqUpdate := map[string]interface{}{
		"groups": [][]string{{user1.ID}},
	}
	groupsJsonUpdate, _ := json.Marshal(groupsReqUpdate)
	reqGroupsUpdate := createAuthenticatedRequest(t, http.MethodPut, "/api/v1/teams/"+team.ID+"/schedule/l1-groups", groupsJsonUpdate, admin.ID)
	recGroupsUpdate := httptest.NewRecorder()
	env.Echo.ServeHTTP(recGroupsUpdate, reqGroupsUpdate)
	if recGroupsUpdate.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for setting groups update, got %d: %s", recGroupsUpdate.Code, recGroupsUpdate.Body.String())
	}

	// Verify Updates Persisted
	updatedSched, err := env.S.GetScheduleByTeamID(team.ID)
	if err != nil {
		t.Fatalf("Failed to fetch updated schedule: %v", err)
	}
	if updatedSched.Timezone != "Asia/Tokyo" {
		t.Errorf("Expected timezone Asia/Tokyo, got %s", updatedSched.Timezone)
	}
	if updatedSched.L1HandoffTime != "18:00" {
		t.Errorf("Expected handoff 18:00, got %s", updatedSched.L1HandoffTime)
	}
	// Verify existing fields didn't break
	if updatedSched.L1RotationType != model.RotationWeekly {
		t.Errorf("Expected rotation type preserved as weekly, got %s", updatedSched.L1RotationType)
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

// TestSchedule_SetL1Groups_Validation covers all validation rules of the
// PUT /api/v1/teams/:id/schedule/l1-groups endpoint.
func TestSchedule_SetL1Groups_Validation(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "validation-team")
	uA := testutil.SeedUser(t, env.S, "ua@example.com")
	uB := testutil.SeedUser(t, env.S, "ub@example.com")
	uOutsider := testutil.SeedUser(t, env.S, "outsider@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, uA.ID, model.TeamMemberRoleMember)
	testutil.SeedTeamMember(t, env.S, team.ID, uB.ID, model.TeamMemberRoleMember)
	// uOutsider deliberately not added to team

	// Schedule must exist (handler returns 404 if missing)
	if err := env.S.CreateSchedule(&model.Schedule{
		ID:              "sched-validation",
		TeamID:          team.ID,
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "09:00",
		L1RotationStart: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	put := func(t *testing.T, body map[string]interface{}) int {
		t.Helper()
		jsonBody, _ := json.Marshal(body)
		req := createAuthenticatedRequest(t, http.MethodPut,
			"/api/v1/teams/"+team.ID+"/schedule/l1-groups", jsonBody, admin.ID)
		rec := httptest.NewRecorder()
		env.Echo.ServeHTTP(rec, req)
		return rec.Code
	}

	// 1. Empty group inside groups → 400
	if code := put(t, map[string]interface{}{"groups": [][]string{{}}}); code != http.StatusBadRequest {
		t.Errorf("Empty inner group: expected 400, got %d", code)
	}

	// 2. Duplicate user within a single group → 400
	if code := put(t, map[string]interface{}{"groups": [][]string{{uA.ID, uA.ID}}}); code != http.StatusBadRequest {
		t.Errorf("Duplicate user in group: expected 400, got %d", code)
	}

	// 3. User exists but is not a team member → 400
	if code := put(t, map[string]interface{}{"groups": [][]string{{uOutsider.ID}}}); code != http.StatusBadRequest {
		t.Errorf("Non-member user: expected 400, got %d", code)
	}

	// 4. User does not exist at all → 400
	if code := put(t, map[string]interface{}{"groups": [][]string{{"u-nonexistent"}}}); code != http.StatusBadRequest {
		t.Errorf("Nonexistent user: expected 400, got %d", code)
	}

	// 5. Valid groups → 200, epoch created with the groups
	if code := put(t, map[string]interface{}{"groups": [][]string{{uA.ID, uB.ID}}}); code != http.StatusOK {
		t.Errorf("Valid groups: expected 200, got %d", code)
	}
	epoch, err := env.S.GetCurrentEpoch("sched-validation", "l1")
	if err != nil {
		t.Fatalf("GetCurrentEpoch after valid set: %v", err)
	}
	if len(epoch.Groups) != 1 || len(epoch.Groups[0]) != 2 {
		t.Errorf("Expected 1 group of 2 users, got %v", epoch.Groups)
	}

	// 6. Empty groups array → 200, current epoch closed, no new epoch
	if code := put(t, map[string]interface{}{"groups": [][]string{}}); code != http.StatusOK {
		t.Errorf("Empty groups (clear): expected 200, got %d", code)
	}
	if _, err := env.S.GetCurrentEpoch("sched-validation", "l1"); err == nil {
		t.Errorf("Expected no current epoch after clear, but GetCurrentEpoch succeeded")
	}

	// 7. Same user in multiple groups (allowed, dedup is per-group only)
	if code := put(t, map[string]interface{}{"groups": [][]string{{uA.ID}, {uA.ID, uB.ID}}}); code != http.StatusOK {
		t.Errorf("User in multiple groups: expected 200, got %d", code)
	}
}

// TestSchedule_GetTeamSchedule_ReturnsL1Groups verifies that the GET endpoint
// returns the L1 rotation as a nested array of populated user objects.
func TestSchedule_GetTeamSchedule_ReturnsL1Groups(t *testing.T) {
	env := setupAPITest(t)

	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "get-groups-team")
	uA := testutil.SeedUser(t, env.S, "ga@example.com")
	uB := testutil.SeedUser(t, env.S, "gb@example.com")
	uC := testutil.SeedUser(t, env.S, "gc@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, uA.ID, model.TeamMemberRoleMember)
	testutil.SeedTeamMember(t, env.S, team.ID, uB.ID, model.TeamMemberRoleMember)
	testutil.SeedTeamMember(t, env.S, team.ID, uC.ID, model.TeamMemberRoleMember)

	if err := env.S.CreateSchedule(&model.Schedule{
		ID:              "sched-get-groups",
		TeamID:          team.ID,
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "09:00",
		L1RotationStart: time.Now(),
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// Set groups via API
	groupsBody, _ := json.Marshal(map[string]interface{}{
		"groups": [][]string{{uA.ID, uB.ID}, {uC.ID}},
	})
	req := createAuthenticatedRequest(t, http.MethodPut,
		"/api/v1/teams/"+team.ID+"/schedule/l1-groups", groupsBody, admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Set groups: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// GET schedule
	getReq := createAuthenticatedRequest(t, http.MethodGet, "/api/v1/teams/"+team.ID+"/schedule", nil, admin.ID)
	getRec := httptest.NewRecorder()
	env.Echo.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET schedule: expected 200, got %d", getRec.Code)
	}

	// Parse with nested array of user objects
	var resp struct {
		L1Groups [][]struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"l1_groups"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v (body: %s)", err, getRec.Body.String())
	}

	if len(resp.L1Groups) != 2 {
		t.Fatalf("Expected 2 groups in response, got %d", len(resp.L1Groups))
	}
	if len(resp.L1Groups[0]) != 2 {
		t.Errorf("Group 0: expected 2 users, got %d", len(resp.L1Groups[0]))
	}
	if len(resp.L1Groups[1]) != 1 {
		t.Errorf("Group 1: expected 1 user, got %d", len(resp.L1Groups[1]))
	}
	// Verify user IDs preserved in order
	if resp.L1Groups[0][0].ID != uA.ID || resp.L1Groups[0][1].ID != uB.ID {
		t.Errorf("Group 0 IDs: expected [%s, %s], got [%s, %s]",
			uA.ID, uB.ID, resp.L1Groups[0][0].ID, resp.L1Groups[0][1].ID)
	}
	if resp.L1Groups[1][0].ID != uC.ID {
		t.Errorf("Group 1 ID: expected %s, got %s", uC.ID, resp.L1Groups[1][0].ID)
	}
	// Names populated
	if resp.L1Groups[0][0].Name == "" || resp.L1Groups[0][1].Name == "" || resp.L1Groups[1][0].Name == "" {
		t.Errorf("Expected populated user names, got empty: %+v", resp.L1Groups)
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

func TestSchedule_SlackUsergroupID_Persistence(t *testing.T) {
	env := setupAPITest(t)

	// Setup Data
	admin := testutil.SeedUser(t, env.S, "admin@example.com")
	team := testutil.SeedTeam(t, env.S, "usergroup-team")
	user1 := testutil.SeedUser(t, env.S, "oncall@example.com")
	testutil.SeedTeamMember(t, env.S, team.ID, user1.ID, model.TeamMemberRoleMember)

	// 1. Create Schedule with slack_usergroup_id
	createReq := map[string]interface{}{
		"team_id":            team.ID,
		"timezone":           "UTC",
		"slack_usergroup_id": "S12345678",
		"l1_rotation_type":   "daily",
		"l1_handoff_time":    "11:00",
		"l1_rotation_start":  time.Now().Format(time.RFC3339),
	}
	jsonBody, _ := json.Marshal(createReq)
	req := createAuthenticatedRequest(t, http.MethodPut, "/api/v1/teams/"+team.ID+"/schedule", jsonBody, admin.ID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("Expected 200/201, got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Verify via GET
	getReq := createAuthenticatedRequest(t, http.MethodGet, "/api/v1/teams/"+team.ID+"/schedule", nil, admin.ID)
	getRec := httptest.NewRecorder()
	env.Echo.ServeHTTP(getRec, getReq)

	if getRec.Code != http.StatusOK {
		t.Fatalf("Expected 200 for GET, got %d", getRec.Code)
	}

	var schedule model.Schedule
	if err := json.Unmarshal(getRec.Body.Bytes(), &schedule); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if schedule.SlackUsergroupID != "S12345678" {
		t.Errorf("Expected slack_usergroup_id=S12345678, got %s", schedule.SlackUsergroupID)
	}

	// 3. Update to clear usergroup ID
	updateReq := map[string]interface{}{
		"team_id":            team.ID,
		"timezone":           "UTC",
		"slack_usergroup_id": "", // Clear
		"l1_rotation_type":   "daily",
		"l1_handoff_time":    "11:00",
		"l1_rotation_start":  time.Now().Format(time.RFC3339),
	}
	jsonBody, _ = json.Marshal(updateReq)
	req = createAuthenticatedRequest(t, http.MethodPut, "/api/v1/teams/"+team.ID+"/schedule", jsonBody, admin.ID)
	rec = httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 for update, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Verify cleared
	getReq = createAuthenticatedRequest(t, http.MethodGet, "/api/v1/teams/"+team.ID+"/schedule", nil, admin.ID)
	getRec = httptest.NewRecorder()
	env.Echo.ServeHTTP(getRec, getReq)

	var schedule2 model.Schedule // Use fresh variable to avoid stale data from omitempty
	if err := json.Unmarshal(getRec.Body.Bytes(), &schedule2); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if schedule2.SlackUsergroupID != "" {
		t.Errorf("Expected slack_usergroup_id to be cleared, got %s", schedule2.SlackUsergroupID)
	}

	// 5. Verify GetSchedulesWithUsergroup excludes this schedule
	schedules, err := env.S.GetSchedulesWithUsergroup()
	if err != nil {
		t.Fatalf("GetSchedulesWithUsergroup failed: %v", err)
	}
	for _, s := range schedules {
		if s.ID == schedule2.ID {
			t.Errorf("Schedule with empty usergroup should not be returned by GetSchedulesWithUsergroup")
		}
	}
}
