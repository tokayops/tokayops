package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/store"
)

// The mock store seeds team "devops" with denis (global admin, team admin) and
// alex (team member), which is exactly the shape these endpoints need.
const (
	cfgGroupA = "aaaaaaaa-0000-4000-8000-000000000001"
	cfgGroupB = "aaaaaaaa-0000-4000-8000-000000000002"
)

// configRequest builds a valid payload with one L1 group per argument.
func configRequest(expectedVersion int64, groups ...[]string) PutScheduleConfigRequest {
	monday := 1
	dto := make([]ScheduleGroupDTO, len(groups))
	ids := [...]string{cfgGroupA, cfgGroupB}
	for i, members := range groups {
		dto[i] = ScheduleGroupDTO{ID: ids[i], UserIDs: members}
	}
	return PutScheduleConfigRequest{
		ExpectedVersion: expectedVersion,
		ScheduleConfigDTO: ScheduleConfigDTO{
			Timezone: "UTC",
			L1: ScheduleL1DTO{
				Enabled:      true,
				RotationType: "weekly",
				HandoffTime:  "11:00",
				HandoffDay:   &monday,
				Groups:       dto,
			},
			L2: ScheduleL2DTO{
				EscalationTimeoutMinutes: 5,
				RotationType:             "weekly",
				HandoffTime:              "11:00",
				HandoffDay:               &monday,
			},
		},
	}
}

func doJSON(t *testing.T, e *echo.Echo, method, url string, body any, user string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, url, reader)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, user)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// createSchedule puts the first configuration and returns the response.
func createSchedule(t *testing.T, e *echo.Echo, groups ...[]string) PutScheduleConfigResponse {
	t.Helper()
	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(0, groups...), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("create schedule: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out PutScheduleConfigResponse
	decodeJSON(t, rec, &out)
	return out
}

// The previous version of this guard never fired - c.JSON returns nil, so the
// `if err := check(c); err != nil` around it was always false, and the first
// unwired request reached the handler and panicked on a nil service. The check
// is middleware now, and this is the test that says so.
func TestScheduleRoutesRefuseWhenUnwired(t *testing.T) {
	s := store.NewMockStore()
	defer s.Close()
	api := NewAPI(s, nil, nil, nil, "", nil) // deliberately unwired
	e := echo.New()
	api.RegisterRoutes(e)

	from := time.Now().UTC()
	routes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/teams/devops/schedule/config", nil},
		{http.MethodPut, "/api/v1/teams/devops/schedule/config", configRequest(0, []string{"denis"})},
		{http.MethodPost, "/api/v1/teams/devops/schedule/preview", configRequest(0, []string{"denis"})},
		{http.MethodDelete, "/api/v1/teams/devops/schedule?expected_version=1", nil},
		{http.MethodGet, "/api/v1/teams/devops/schedule/revisions", nil},
		{http.MethodGet, "/api/v1/teams/devops/schedule/revisions/rev-1", nil},
		{http.MethodGet, "/api/v1/teams/devops/schedule/render?from=" +
			from.Format(time.RFC3339) + "&until=" + from.Add(time.Hour).Format(time.RFC3339), nil},
		{http.MethodGet, "/api/v1/teams/devops/schedule/on-call", nil},
		{http.MethodGet, "/api/v1/teams/devops/schedule/overrides", nil},
		{http.MethodPost, "/api/v1/teams/devops/schedule/overrides", ScheduleOverrideRequest{UserID: "alex"}},
		{http.MethodPut, "/api/v1/schedules/s1/overrides/o1", ScheduleOverrideRequest{UserID: "alex"}},
		{http.MethodDelete, "/api/v1/schedules/s1/overrides/o1?expected_revision=1", nil},
		{http.MethodDelete, "/api/v1/teams/devops/members/alex", nil},
		{http.MethodDelete, "/api/v1/users/alex", nil},

		// Deleting a team is on this list because what retains a team is its
		// schedule history: the command needs the same stack, and answering
		// 204 without it would delete a team whose blockers nobody looked at.
		{http.MethodDelete, "/api/v1/teams/devops", nil},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			rec := doJSON(t, e, r.method, r.path, r.body, "denis")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("want 503 from an unwired API, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// One endpoint, three outcomes, chosen by the state of the schedule rather
// than by the client: that is the property PUT /config exists to have.
func TestPutConfigDispatchesCreateSaveRecreate(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	created := createSchedule(t, e, []string{"denis"}, []string{"alex"})
	if !created.Created || created.Recreated || created.Noop {
		t.Fatalf("first save should be a create, got %+v", created)
	}
	if created.Version != 1 {
		t.Fatalf("version after create = %d, want 1", created.Version)
	}

	// An edit: same endpoint, non-zero expected version.
	edit := configRequest(created.Version, []string{"denis", "alex"})
	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config", edit, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var saved PutScheduleConfigResponse
	decodeJSON(t, rec, &saved)
	if saved.Created || saved.Recreated || saved.Noop {
		t.Fatalf("second save should be an edit, got %+v", saved)
	}
	if saved.Version != 2 {
		t.Fatalf("version after edit = %d, want 2", saved.Version)
	}

	// Re-sending the same configuration writes nothing but still answers with
	// a version and a revision: the editor must not need a second shape.
	rec = doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(saved.Version, []string{"denis", "alex"}), "denis")
	var noop PutScheduleConfigResponse
	decodeJSON(t, rec, &noop)
	if !noop.Noop {
		t.Fatalf("re-saving an unchanged configuration should be a no-op, got %+v", noop)
	}
	if noop.Version != saved.Version || noop.RevisionID == "" {
		t.Fatalf("no-op must carry the current version and revision, got %+v", noop)
	}

	// Delete, then recreate through the same endpoint.
	env.SetNow(time.Now().UTC().Add(time.Hour))
	rec = doJSON(t, e, http.MethodDelete,
		"/api/v1/teams/devops/schedule?expected_version=2", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	env.SetNow(time.Now().UTC().Add(2 * time.Hour))
	rec = doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(3, []string{"denis"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("recreate: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var recreated PutScheduleConfigResponse
	decodeJSON(t, rec, &recreated)
	if !recreated.Recreated || recreated.Created {
		t.Fatalf("save on a deleted schedule should recreate, got %+v", recreated)
	}
	if recreated.Version != 4 {
		t.Fatalf("version continues across delete/recreate: got %d, want 4", recreated.Version)
	}
}

// A stale editor tab must be told which version it collided with, not just
// that it collided: without the current version it cannot reload.
func TestStaleModalConflict(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	createSchedule(t, e, []string{"denis"})

	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(0, []string{"alex"}), "denis")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale save: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["current_version"] != float64(1) {
		t.Fatalf("409 body must carry current_version 1, got %v", body)
	}
}

func TestPreviewEndpointDoesNotWrite(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	before := env.Config.RootCount()
	rec := doJSON(t, e, http.MethodPost, "/api/v1/teams/devops/schedule/preview",
		configRequest(0, []string{"denis"}, []string{"alex"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out SchedulePreviewResponse
	decodeJSON(t, rec, &out)

	if out.EvaluatedAt.IsZero() {
		t.Fatal("preview must report the instant it evaluated")
	}
	if out.BaseVersion != 0 {
		t.Fatalf("base_version for a schedule that does not exist = %d, want 0", out.BaseVersion)
	}
	if !out.OnCallChanged {
		t.Fatal("creating a rotation where there was none changes who is on call")
	}
	if len(out.Entries) == 0 {
		t.Fatal("preview returned no entries")
	}
	if got := env.Config.RootCount(); got != before {
		t.Fatalf("preview wrote something: %d roots, want %d", got, before)
	}
}

func TestGetConfigStates(t *testing.T) {
	t.Run("Absent404", func(t *testing.T) {
		_, s, e, _ := setupScheduleAPI(t)
		defer s.Close()

		rec := doJSON(t, e, http.MethodGet, "/api/v1/teams/devops/schedule/config", nil, "denis")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// A deleted schedule answers 200 with deleted_at and the last valid
	// configuration, so the editor can prefill a recreate without a second
	// request. A 410 with no body would have forced exactly that request.
	t.Run("Deleted200WithDeletedAt", func(t *testing.T) {
		_, s, e, env := setupScheduleAPI(t)
		defer s.Close()

		createSchedule(t, e, []string{"denis"}, []string{"alex"})
		env.SetNow(time.Now().UTC().Add(time.Hour))
		if rec := doJSON(t, e, http.MethodDelete,
			"/api/v1/teams/devops/schedule?expected_version=1", nil, "denis"); rec.Code != http.StatusNoContent {
			t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
		}

		rec := doJSON(t, e, http.MethodGet, "/api/v1/teams/devops/schedule/config", nil, "denis")
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var out ScheduleConfigResponse
		decodeJSON(t, rec, &out)
		if out.DeletedAt == nil {
			t.Fatal("deleted schedule must report deleted_at")
		}
		if len(out.Config.L1.Groups) != 2 {
			t.Fatalf("deleted schedule must carry the last valid configuration, got %+v", out.Config)
		}
		if out.Version != 2 {
			t.Fatalf("version after delete = %d, want 2", out.Version)
		}
	})
}

func TestRenderNewShapeWithHistoryIncomplete(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	env.SetNow(now)
	createSchedule(t, e, []string{"denis"}, []string{"alex"})

	// The range starts before this schedule's history begins.
	from := now.Add(-48 * time.Hour).Format(time.RFC3339)
	until := now.Add(72 * time.Hour).Format(time.RFC3339)
	rec := doJSON(t, e, http.MethodGet,
		fmt.Sprintf("/api/v1/teams/devops/schedule/render?from=%s&until=%s", from, until), nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("render: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out ScheduleRenderResponse
	decodeJSON(t, rec, &out)

	if out.HistoryComplete {
		t.Fatal("a range preceding history_complete_from must not claim complete history")
	}
	if len(out.Entries) == 0 {
		t.Fatal("render returned no entries")
	}
	for _, entry := range out.Entries {
		// A shift is a run of duty: who, from when, until when. Grid
		// boundaries and provenance are not part of it - the calendar never
		// read them, and one slot's boundaries live on the on-call DTO.
		if entry.Start.IsZero() || entry.End.IsZero() || len(entry.UserIDs) == 0 {
			t.Fatalf("entry must say who is on duty and when, got %+v", entry)
		}
	}
}

func TestRenderRangeCappedAt90Days(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()
	createSchedule(t, e, []string{"denis"})

	from := time.Now().UTC()
	until := from.Add(91 * 24 * time.Hour)
	rec := doJSON(t, e, http.MethodGet, fmt.Sprintf(
		"/api/v1/teams/devops/schedule/render?from=%s&until=%s",
		from.Format(time.RFC3339), until.Format(time.RFC3339)), nil, "denis")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-long range: want 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRevisionsEndpointsReadOnly(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	created := createSchedule(t, e, []string{"denis"})

	rec := doJSON(t, e, http.MethodGet, "/api/v1/teams/devops/schedule/revisions", nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("list revisions: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list ScheduleRevisionListResponse
	decodeJSON(t, rec, &list)
	if len(list.Revisions) != 1 || list.Revisions[0].RevisionID != created.RevisionID {
		t.Fatalf("expected the one revision just created, got %+v", list.Revisions)
	}
	if list.Revisions[0].Config != nil {
		t.Fatal("the list must not carry snapshots")
	}

	rec = doJSON(t, e, http.MethodGet,
		"/api/v1/teams/devops/schedule/revisions/"+created.RevisionID, nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("get revision: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var one ScheduleRevisionDTO
	decodeJSON(t, rec, &one)
	if one.Config == nil {
		t.Fatal("the single-revision endpoint must carry the snapshot")
	}

	// A revision ID that belongs to nothing this team owns is not found, not
	// readable because the caller guessed it.
	rec = doJSON(t, e, http.MethodGet,
		"/api/v1/teams/devops/schedule/revisions/someone-elses-revision", nil, "denis")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign revision: want 404, got %d", rec.Code)
	}

	// Nothing here writes.
	if got := len(env.Config.Revisions(one.RevisionID)); got != 0 {
		t.Fatalf("reading revisions created state: %d", got)
	}
}

func TestRevisionsPagination(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	env.SetNow(now)
	createSchedule(t, e, []string{"denis"})
	for i, members := range [][]string{{"alex"}, {"denis", "alex"}, {"denis"}} {
		env.SetNow(now.Add(time.Duration(i+1) * time.Hour))
		rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
			configRequest(int64(i+1), members), "denis")
		if rec.Code != http.StatusOK {
			t.Fatalf("save %d: want 200, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	rec := doJSON(t, e, http.MethodGet,
		"/api/v1/teams/devops/schedule/revisions?limit=2", nil, "denis")
	var page ScheduleRevisionListResponse
	decodeJSON(t, rec, &page)
	if len(page.Revisions) != 2 {
		t.Fatalf("page size = %d, want 2", len(page.Revisions))
	}
	if page.Revisions[0].Version != 4 || page.Revisions[1].Version != 3 {
		t.Fatalf("revisions must come newest first, got %d then %d",
			page.Revisions[0].Version, page.Revisions[1].Version)
	}
	if page.NextBeforeVersion == nil || *page.NextBeforeVersion != 3 {
		t.Fatalf("next_before_version = %v, want 3", page.NextBeforeVersion)
	}

	rec = doJSON(t, e, http.MethodGet, fmt.Sprintf(
		"/api/v1/teams/devops/schedule/revisions?limit=2&before_version=%d", *page.NextBeforeVersion),
		nil, "denis")
	var second ScheduleRevisionListResponse
	decodeJSON(t, rec, &second)
	if len(second.Revisions) != 2 || second.Revisions[0].Version != 2 {
		t.Fatalf("second page = %+v", second.Revisions)
	}
	if second.NextBeforeVersion != nil {
		t.Fatalf("the last page must not advertise a cursor, got %v", *second.NextBeforeVersion)
	}
}

func TestOverrideEndpointsExpectedRevision(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	env.SetNow(now)
	createSchedule(t, e, []string{"denis"})

	reason := "vacation"
	body := ScheduleOverrideRequest{
		UserID:    "alex",
		ValidFrom: now.Add(24 * time.Hour),
		ValidTo:   now.Add(48 * time.Hour),
		Reason:    &reason,
	}
	rec := doJSON(t, e, http.MethodPost, "/api/v1/teams/devops/schedule/overrides", body, "denis")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create override: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created ScheduleOverrideDTO
	decodeJSON(t, rec, &created)
	if created.Revision != 1 {
		t.Fatalf("first override revision = %d, want 1", created.Revision)
	}

	scheduleID := scheduleIDOf(t, env, "devops")
	updatePath := fmt.Sprintf("/api/v1/schedules/%s/overrides/%s", scheduleID, created.OverrideID)

	// Editing from a stale revision must be refused with the current one.
	stale := body
	stale.ExpectedRevision = 99
	rec = doJSON(t, e, http.MethodPut, updatePath, stale, "denis")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale override edit: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var conflict map[string]any
	decodeJSON(t, rec, &conflict)
	if conflict["current_revision"] != float64(1) {
		t.Fatalf("409 body must carry current_revision, got %v", conflict)
	}

	update := body
	update.ExpectedRevision = 1
	update.ValidTo = now.Add(72 * time.Hour)
	rec = doJSON(t, e, http.MethodPut, updatePath, update, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("update override: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated ScheduleOverrideDTO
	decodeJSON(t, rec, &updated)
	if updated.Revision != 2 {
		t.Fatalf("updated override revision = %d, want 2", updated.Revision)
	}

	// The delete carries its expected revision in the query, and refuses
	// without one rather than skipping the check.
	rec = doJSON(t, e, http.MethodDelete, updatePath, nil, "denis")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete without expected_revision: want 400, got %d", rec.Code)
	}
	rec = doJSON(t, e, http.MethodDelete, updatePath+"?expected_revision=2", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete override: want 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// The history survives the delete; only the projection stops showing it.
	if got := len(env.Config.OverrideRevisions(scheduleID)); got != 3 {
		t.Fatalf("override history holds %d revisions, want 3", got)
	}
}

// Without this endpoint the editor has nowhere to get expected_revision, the
// reason text or the original bounds after a page reload.
func TestOverridesGetReturnsCurrentHeads(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	env.SetNow(now)
	createSchedule(t, e, []string{"denis"})

	reason := "conference"
	body := ScheduleOverrideRequest{
		UserID:    "alex",
		ValidFrom: now.Add(24 * time.Hour),
		ValidTo:   now.Add(48 * time.Hour),
		Reason:    &reason,
	}
	rec := doJSON(t, e, http.MethodPost, "/api/v1/teams/devops/schedule/overrides", body, "denis")
	var created ScheduleOverrideDTO
	decodeJSON(t, rec, &created)

	rec = doJSON(t, e, http.MethodGet, "/api/v1/teams/devops/schedule/overrides", nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("list overrides: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list ScheduleOverrideListResponse
	decodeJSON(t, rec, &list)
	if len(list.Overrides) != 1 {
		t.Fatalf("want one override head, got %+v", list.Overrides)
	}
	head := list.Overrides[0]
	if head.Revision != created.Revision || head.OverrideID != created.OverrideID {
		t.Fatalf("head does not match what was created: %+v vs %+v", head, created)
	}
	if head.Reason == nil || *head.Reason != reason {
		t.Fatalf("head must carry the reason, got %v", head.Reason)
	}
	if !head.ValidFrom.Equal(body.ValidFrom) || !head.ValidTo.Equal(body.ValidTo) {
		t.Fatalf("head must carry the original bounds, got %v..%v", head.ValidFrom, head.ValidTo)
	}

	// A tombstoned override is not "an override that exists".
	path := fmt.Sprintf("/api/v1/schedules/%s/overrides/%s?expected_revision=1",
		scheduleIDOf(t, env, "devops"), created.OverrideID)
	if rec = doJSON(t, e, http.MethodDelete, path, nil, "denis"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete override: got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, e, http.MethodGet, "/api/v1/teams/devops/schedule/overrides", nil, "denis")
	decodeJSON(t, rec, &list)
	if len(list.Overrides) != 0 {
		t.Fatalf("deleted override still listed: %+v", list.Overrides)
	}
}

func TestRemoveTeamMemberBlocked409(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	createSchedule(t, e, []string{"denis"}, []string{"alex"})

	rec := doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops/members/alex", nil, "denis")
	if rec.Code != http.StatusConflict {
		t.Fatalf("removing a member who is in the rotation: want 409, got %d: %s",
			rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeJSON(t, rec, &body)
	schedules, _ := body["schedules"].([]any)
	if len(schedules) != 1 {
		t.Fatalf("409 body must list the blocking schedules, got %v", body)
	}

	// Once out of the rotation, the removal goes through.
	rec = doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(1, []string{"denis"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("drop alex from the rotation: got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, e, http.MethodDelete, "/api/v1/teams/devops/members/alex", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("removing a free member: want 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteUserBlocked409WithScheduleList(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(env *scheduleTestEnv)
	}{
		{
			name: "in the active tail revision",
			setup: func(env *scheduleTestEnv) {
				env.Erasure.Tails = []erasure.ScheduleTail{{
					ScheduleID: "sched-1",
					TeamID:     "devops",
					Snapshot: rotation.ScheduleRevisionSnapshot{
						L1: rotation.RotationLayerSnapshot{
							Groups: []rotation.RotationGroup{{ID: cfgGroupA, Members: []string{"alex"}}},
						},
					},
				}}
			},
		},
		{
			// An override can name someone who is in no group at all, so the
			// snapshot alone would miss them.
			name: "target of a live override",
			setup: func(env *scheduleTestEnv) {
				env.Erasure.Overrides = []erasure.OverrideAssignment{{
					ScheduleID: "sched-1",
					TeamID:     "devops",
					OverrideID: "ovr-1",
					ValidFrom:  time.Now().UTC(),
					ValidTo:    time.Now().UTC().Add(24 * time.Hour),
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, s, e, env := setupScheduleAPI(t)
			defer s.Close()
			tc.setup(env)

			rec := doJSON(t, e, http.MethodDelete, "/api/v1/users/alex", nil, "denis")
			if rec.Code != http.StatusConflict {
				t.Fatalf("want 409, got %d: %s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			decodeJSON(t, rec, &body)
			schedules, _ := body["schedules"].([]any)
			if len(schedules) != 1 {
				t.Fatalf("409 body must list the blocking schedules, got %v", body)
			}
			if _, err := s.GetActiveUserByID("alex"); err != nil {
				t.Fatal("a refused erasure must leave the user intact")
			}
		})
	}
}

func TestDeleteUserBlockedLastAdmin(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	// denis is the only admin the mock seeds.
	rec := doJSON(t, e, http.MethodDelete, "/api/v1/users/denis", nil, "denis")
	if rec.Code != http.StatusConflict {
		t.Fatalf("erasing the last admin: want 409, got %d: %s", rec.Code, rec.Body.String())
	}

	// With a second admin present, the same request succeeds.
	if err := s.CreateUser(&model.User{ID: "second", Email: "second@test.com",
		Name: "Second", Role: model.UserRoleAdmin}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rec = doJSON(t, e, http.MethodDelete, "/api/v1/users/denis", nil, "denis")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("erasing an admin while another remains: want 204, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// A soft delete has to end the session on the next request. Before this, an
// erased user's JWT kept working until it expired on its own.
func TestErasedSessionRejected401(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	rec := doJSON(t, e, http.MethodGet, "/api/v1/teams", nil, "alex")
	if rec.Code != http.StatusOK {
		t.Fatalf("alex should be able to list teams, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec = doJSON(t, e, http.MethodDelete, "/api/v1/users/alex", nil, "denis"); rec.Code != http.StatusNoContent {
		t.Fatalf("erase alex: got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, e, http.MethodGet, "/api/v1/teams", nil, "alex")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an erased user's session must be rejected: want 401, got %d", rec.Code)
	}
}

// Erasure anonymizes rather than deletes, so history that names the ID still
// resolves - to "Deleted user" rather than to nothing.
func TestHistoricalHydrationShowsDeletedUser(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	if rec := doJSON(t, e, http.MethodDelete, "/api/v1/users/alex", nil, "denis"); rec.Code != http.StatusNoContent {
		t.Fatalf("erase alex: got %d: %s", rec.Code, rec.Body.String())
	}

	// The admin API no longer knows them.
	if rec := doJSON(t, e, http.MethodGet, "/api/v1/users/alex", nil, "denis"); rec.Code != http.StatusNotFound {
		t.Fatalf("erased user in the admin API: want 404, got %d", rec.Code)
	}
	// The display read still does.
	user, err := s.GetUserByID("alex")
	if err != nil {
		t.Fatalf("history hydration must still resolve an erased ID: %v", err)
	}
	if user.Name != store.AnonymizedUserName {
		t.Fatalf("erased user renders as %q, want %q", user.Name, store.AnonymizedUserName)
	}
	if user.Email != "" {
		t.Fatalf("erased user kept an email address: %q", user.Email)
	}
}

// A user who has been erased must not be writable back into the system by any
// of the mutations that used to fill the row in again.
func TestErasedUserCannotBeRefilled(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	if rec := doJSON(t, e, http.MethodDelete, "/api/v1/users/alex", nil, "denis"); rec.Code != http.StatusNoContent {
		t.Fatalf("erase alex: got %d: %s", rec.Code, rec.Body.String())
	}

	rec := doJSON(t, e, http.MethodPut, "/api/v1/users/alex",
		map[string]string{"name": "Alex Again", "email": "alex@test.com"}, "denis")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("updating an erased user: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, e, http.MethodPut, "/api/v1/users/alex/password",
		map[string]string{"password": "SomethingNew123!"}, "denis")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("setting a password on an erased user: want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, e, http.MethodPost, "/api/v1/teams/triage/members",
		map[string]string{"user_id": "alex"}, "denis")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("granting an erased user a membership: want 404, got %d: %s", rec.Code, rec.Body.String())
	}

	user, err := s.GetUserByID("alex")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.Name != store.AnonymizedUserName || user.Email != "" {
		t.Fatalf("erased row was refilled: %+v", user)
	}
}

// The mapper is the only place a status is chosen, so its table is worth
// asserting line by line: a handler cannot compensate for a wrong answer here.
func TestErrorMappingTable(t *testing.T) {
	api, s, _, _ := setupScheduleAPI(t)
	defer s.Close()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"schedule not found", scheduleconfig.ErrScheduleNotFound, http.StatusNotFound},
		{"revision not found", scheduleconfig.ErrRevisionNotFound, http.StatusNotFound},
		{"override not found", scheduleconfig.ErrOverrideNotFound, http.StatusNotFound},
		{"team not found", scheduleconfig.ErrTeamNotFound, http.StatusNotFound},
		{"user not found", erasure.ErrUserNotFound, http.StatusNotFound},
		{"schedule exists", scheduleconfig.ErrScheduleExists, http.StatusConflict},
		{"schedule deleted", scheduleconfig.ErrScheduleDeleted, http.StatusConflict},
		{"last admin", erasure.ErrLastAdmin, http.StatusConflict},
		{"version conflict", &scheduleconfig.VersionConflictError{Expected: 1, Current: 2}, http.StatusConflict},
		{"override revision conflict", &scheduleconfig.OverrideRevisionConflictError{Expected: 1, Current: 2}, http.StatusConflict},
		{"override overlap", &scheduleconfig.OverrideOverlapError{
			Conflicts: []scheduleconfig.OverrideRef{{OverrideID: "o1"}}}, http.StatusConflict},
		{"member on call", &scheduleconfig.MemberOnCallError{
			Schedules: []scheduleconfig.ScheduleRef{{ScheduleID: "s1", TeamID: "devops"}}}, http.StatusConflict},
		{"user on call", &erasure.UserOnCallError{
			Schedules: []erasure.ScheduleRef{{ScheduleID: "s1", TeamID: "devops"}}}, http.StatusConflict},
		{"not a team member", &scheduleconfig.UserNotTeamMemberError{
			UserIDs: []string{"mallory"}}, http.StatusUnprocessableEntity},
		{"validation", &scheduleconfig.ValidationError{Field: "timezone", Msg: "unknown"}, http.StatusBadRequest},
		{"snapshot decode", fmt.Errorf("%w: broken", scheduleconfig.ErrSnapshotDecode), http.StatusInternalServerError},
		{"invariant violation", scheduleconfig.ErrInvariantViolation, http.StatusInternalServerError},
	}

	e := echo.New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			if err := api.mapScheduleError(c, tc.err); err != nil {
				t.Fatalf("mapScheduleError returned %v", err)
			}
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestScheduleConfigRBACMatrix(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	// charlie is authenticated but belongs to no team; alex is a plain member
	// of devops; denis is both the team admin and a global admin.
	if err := s.CreateUser(&model.User{ID: "charlie", Email: "charlie@test.com",
		Name: "Charlie", Role: model.UserRoleUser}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	createSchedule(t, e, []string{"denis"})

	forbidden := func(code int) bool { return code == http.StatusForbidden }
	allowed := func(code int) bool { return code != http.StatusForbidden }

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		user   string
		check  func(int) bool
	}{
		{"view config as outsider", http.MethodGet, "/api/v1/teams/devops/schedule/config", nil, "charlie", allowed},
		{"view render as member", http.MethodGet,
			"/api/v1/teams/devops/schedule/render?from=" +
				time.Now().UTC().Format(time.RFC3339) + "&until=" +
				time.Now().UTC().Add(time.Hour).Format(time.RFC3339), nil, "alex", allowed},
		{"list overrides as member", http.MethodGet, "/api/v1/teams/devops/schedule/overrides", nil, "alex", allowed},
		{"on-call as outsider", http.MethodGet, "/api/v1/teams/devops/schedule/on-call", nil, "charlie", allowed},

		{"save config as member", http.MethodPut, "/api/v1/teams/devops/schedule/config",
			configRequest(1, []string{"denis"}), "alex", forbidden},
		{"save config as team admin", http.MethodPut, "/api/v1/teams/devops/schedule/config",
			configRequest(1, []string{"denis", "alex"}), "denis", allowed},
		{"preview as member", http.MethodPost, "/api/v1/teams/devops/schedule/preview",
			configRequest(1, []string{"denis"}), "alex", forbidden},
		{"revisions as member", http.MethodGet, "/api/v1/teams/devops/schedule/revisions", nil, "alex", forbidden},
		{"revisions as team admin", http.MethodGet, "/api/v1/teams/devops/schedule/revisions", nil, "denis", allowed},
		{"delete schedule as member", http.MethodDelete,
			"/api/v1/teams/devops/schedule?expected_version=1", nil, "alex", forbidden},
		{"create override as outsider", http.MethodPost, "/api/v1/teams/devops/schedule/overrides",
			ScheduleOverrideRequest{UserID: "alex"}, "charlie", forbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, e, tc.method, tc.path, tc.body, tc.user)
			if !tc.check(rec.Code) {
				t.Fatalf("status %d for %s %s as %s: %s", rec.Code, tc.method, tc.path, tc.user,
					rec.Body.String())
			}
		})
	}
}

// scheduleIDOf reads the schedule ID a team owns out of the fake.
func scheduleIDOf(t *testing.T, env *scheduleTestEnv, teamID string) string {
	t.Helper()
	root, ok := env.Config.RootByTeam(teamID)
	if !ok {
		t.Fatalf("team %s owns no schedule", teamID)
	}
	return root.ID
}

// TestOverrideOfAnotherScheduleIsRefused: the override routes are keyed by
// schedule ID and override ID, so naming someone else's override through a
// schedule of your own must not reach it.
//
// There used to be a test for this, and it asserted the property against a
// store helper no production path called - so the live guard had no coverage at
// all. The helper is gone with the rest of the legacy store; the requirement is
// not.
//
// Two layers refuse this, and the test pins both on purpose. The command refuses
// it under the schedule lock, which is what makes the answer correct; the
// authorization scope refuses it first, which is what keeps a mismatched pair
// from ever opening a transaction. Asserting only the status code would pass
// with the scope guard deleted - verified by removing it - because the command
// answers 404 either way.
func TestOverrideOfAnotherScheduleIsRefused(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC))
	createSchedule(t, e, []string{"denis"})

	validFrom := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	rec := doJSON(t, e, http.MethodPost, "/api/v1/teams/devops/schedule/overrides",
		ScheduleOverrideRequest{
			UserID:    "alex",
			ValidFrom: validFrom,
			ValidTo:   validFrom.Add(4 * time.Hour),
		}, "denis")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create override: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var override ScheduleOverrideDTO
	decodeJSON(t, rec, &override)
	// Without this the 404s below could be a missing override ID rather than
	// the guard, and the test would pass while proving nothing.
	if override.OverrideID == "" {
		t.Fatalf("the fixture produced no override id: %s", rec.Body.String())
	}

	root, ok := env.Config.RootByTeam("devops")
	if !ok {
		t.Fatal("the team owns no schedule after a create")
	}
	// A second schedule, owned by another team. denis is a global admin, so
	// authorization on that team passes and the only thing left to refuse the
	// request is the override-belongs-to-schedule check itself.
	other := "schedule-elsewhere"
	env.Config.SeedRoot(scheduleconfig.ScheduleRoot{
		ID: other, TeamID: "other-team", ConfigVersion: 1, HistoryCompleteFrom: validFrom,
	})
	if other == root.ID {
		t.Fatal("the fixture needs two distinct schedules")
	}

	for _, tc := range []struct {
		name   string
		method string
		url    string
		body   any
	}{
		{"update", http.MethodPut,
			"/api/v1/schedules/" + other + "/overrides/" + override.OverrideID,
			ScheduleOverrideRequest{
				UserID: "alex", ValidFrom: validFrom, ValidTo: validFrom.Add(time.Hour),
				ExpectedRevision: override.Revision,
			}},
		{"delete", http.MethodDelete,
			"/api/v1/schedules/" + other + "/overrides/" + override.OverrideID +
				"?expected_revision=" + strconv.FormatInt(override.Revision, 10), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env.Config.Calls = nil
			rec := doJSON(t, e, tc.method, tc.url, tc.body, "denis")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 for an override of another schedule: %s",
					rec.Code, rec.Body.String())
			}
			// The scope answered, so the command never ran. Drop this and the
			// test still passes with the scope guard removed, because the
			// command refuses the same request one layer down.
			for _, call := range env.Config.Calls {
				if call == "WithinTx" {
					t.Fatalf("a mismatched override opened a write transaction: %v", env.Config.Calls)
				}
			}
		})
	}

	// And the override itself is untouched: a refused request must not be a
	// partial one.
	rec = doJSON(t, e, http.MethodGet, "/api/v1/teams/devops/schedule/overrides", nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("list overrides: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list ScheduleOverrideListResponse
	decodeJSON(t, rec, &list)
	if len(list.Overrides) != 1 {
		t.Fatalf("got %d overrides, want the original still there", len(list.Overrides))
	}
}

// A save that committed has succeeded, and the response must not depend on
// anything that happens afterwards.
//
// It used to render the resulting duty in a second read AFTER the commit, so a
// failure there answered 500 for a command that had already been applied - the
// response lied about the outcome. The structural guarantee is that there is
// no read to fail: the handler opens no read transaction once the command
// returns.
func TestSaveDoesNotReadAfterTheCommit(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	created := createSchedule(t, e, []string{"denis"})

	// Only the save under test is counted.
	env.Config.Calls = nil

	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(created.Version, []string{"denis", "alex"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("save: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The command itself runs in a write transaction; what must not appear is
	// a read transaction opened after it.
	for i, call := range env.Config.Calls {
		if call != "WithinSnapshot" {
			continue
		}
		committed := false
		for _, earlier := range env.Config.Calls[:i] {
			if earlier == "Commit" {
				committed = true
			}
		}
		if committed {
			t.Fatalf("the handler read again after the commit: %v", env.Config.Calls)
		}
	}

	// And the answer says what the save did, without describing the world.
	var out PutScheduleConfigResponse
	decodeJSON(t, rec, &out)
	if out.RevisionID == "" || out.Version != created.Version+1 {
		t.Fatalf("save answer = %+v, want the new revision and version", out)
	}
	if strings.Contains(rec.Body.String(), "on_call_after") {
		t.Errorf("the save response still describes who is on duty: %s", rec.Body.String())
	}
}

// The team list reads the schedule side once, not once per team.
//
// It used to call GetScheduleRootByTeam in a loop, so the page got one query
// slower every time somebody added a team - and the loop was invisible from
// the outside, because the answer was identical.
func TestListTeamsReadsSchedulesInOneQuery(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	createSchedule(t, e, []string{"denis"})
	for _, id := range []string{"team-b", "team-c", "team-d"} {
		if err := s.CreateTeam(&model.Team{ID: id, Name: id}); err != nil {
			t.Fatalf("CreateTeam %s: %v", id, err)
		}
	}

	env.Config.Calls = nil
	rec := doJSON(t, e, http.MethodGet, "/api/v1/teams", nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("list teams: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	perTeam := 0
	for _, call := range env.Config.Calls {
		if call == "GetScheduleRootByTeam" {
			perTeam++
		}
	}
	if perTeam != 0 {
		t.Fatalf("the page made %d per-team schedule reads, want none: %v", perTeam, env.Config.Calls)
	}

	// And it still answers: the team with a schedule is reported configured.
	var out TeamListResponse
	decodeJSON(t, rec, &out)
	for _, team := range out.Teams {
		if team.ID == "devops" && !team.OnCallConfigured {
			t.Fatal("the team with a schedule is not reported as configured")
		}
	}
}
