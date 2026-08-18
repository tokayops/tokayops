package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// One status, several meanings. 409 alone tells the editor nothing about what
// to do next - reload the form, reload the override list, show the conflicting
// intervals, switch to recreate - and a client that told them apart by parsing
// the message would break on the first rewording.
//
// The table is asserted through the mapper rather than through requests: some
// of these are only reachable in a race, and the contract is that EVERY
// schedule error names itself, not just the ones a test can provoke.
func TestScheduleErrorsCarryMachineCodes(t *testing.T) {
	reason := "conflict"
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"schedule not found", scheduleconfig.ErrScheduleNotFound, http.StatusNotFound, CodeScheduleNotFound},
		{"revision not found", scheduleconfig.ErrRevisionNotFound, http.StatusNotFound, CodeRevisionNotFound},
		{"override not found", scheduleconfig.ErrOverrideNotFound, http.StatusNotFound, CodeOverrideNotFound},
		{"team not found", scheduleconfig.ErrTeamNotFound, http.StatusNotFound, CodeTeamNotFound},
		{"user not found", erasure.ErrUserNotFound, http.StatusNotFound, CodeUserNotFound},

		{"schedule exists", scheduleconfig.ErrScheduleExists, http.StatusConflict, CodeScheduleExists},
		{"schedule deleted", scheduleconfig.ErrScheduleDeleted, http.StatusConflict, CodeScheduleDeleted},
		{"last admin", erasure.ErrLastAdmin, http.StatusConflict, CodeLastAdmin},
		{"actor erased", scheduleconfig.ErrActorNotActive, http.StatusUnauthorized, CodeActorNotActive},

		{"version conflict",
			&scheduleconfig.VersionConflictError{Expected: 1, Current: 2},
			http.StatusConflict, CodeVersionConflict},
		{"override revision conflict",
			&scheduleconfig.OverrideRevisionConflictError{Expected: 1, Current: 2},
			http.StatusConflict, CodeRevisionConflict},
		{"override overlap",
			&scheduleconfig.OverrideOverlapError{Conflicts: []scheduleconfig.OverrideRef{{
				OverrideID: "o1", UserID: "alex",
				ValidFrom: time.Now().UTC(), ValidTo: time.Now().UTC().Add(time.Hour),
			}}},
			http.StatusConflict, CodeOverrideOverlap},
		{"user not a team member",
			&scheduleconfig.UserNotTeamMemberError{UserIDs: []string{"charlie"}},
			http.StatusUnprocessableEntity, CodeUserNotTeamMember},
		{"member on call",
			&scheduleconfig.MemberOnCallError{Schedules: []scheduleconfig.ScheduleRef{{
				ScheduleID: "s1", TeamID: "devops",
			}}},
			http.StatusConflict, CodeMemberOnCall},
		{"validation",
			&scheduleconfig.ValidationError{Field: "timezone", Msg: reason},
			http.StatusBadRequest, CodeValidationFailed},

		// The renderer's damage sentinel is part of the same contract. It used
		// to fall through to the generic internal error, which meant half the
		// schedule surface answered with a machine code and the other half with
		// prose - and the half without one was the half a caller is least able
		// to guess about.
		{"revision chain has a hole",
			fmt.Errorf("wrapped: %w", schedulerender.ErrRevisionGap),
			http.StatusInternalServerError, CodeInvariantViolation},
	}

	api := &API{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := echo.New()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			if err := api.mapScheduleError(e.NewContext(req, rec), tc.err); err != nil {
				t.Fatalf("mapScheduleError: %v", err)
			}
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if body["code"] != tc.code {
				t.Fatalf("code = %v, want %q (body %s)", body["code"], tc.code, rec.Body.String())
			}
		})
	}
}

// The structured bodies keep carrying the details the reaction needs. A code
// that arrived without them would tell the editor what happened and leave it
// unable to say anything useful about it.
func TestScheduleConflictBodiesKeepTheirDetails(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	created := createSchedule(t, e, []string{"denis"})

	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(created.Version+41, []string{"denis"}), "denis")
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale save: want 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeJSON(t, rec, &body)
	if body["code"] != CodeVersionConflict {
		t.Fatalf("code = %v, want %q", body["code"], CodeVersionConflict)
	}
	if body["current_version"] == nil || body["expected_version"] == nil {
		t.Fatalf("a version conflict must still name both versions: %s", rec.Body.String())
	}
}

// The checks a handler makes before the service is reached are part of the
// same contract. Leaving them code-less would force a prose fallback for half
// the surface, which is exactly where the fallback would be forgotten.
func TestDirectHandlerRejectionsCarryCodes(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	createSchedule(t, e, []string{"denis"})

	from := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{"render range beyond the cap", http.MethodGet,
			fmt.Sprintf("/api/v1/teams/devops/schedule/render?from=%s&until=%s",
				from.Format(time.RFC3339), from.AddDate(0, 0, 120).Format(time.RFC3339)),
			nil, http.StatusBadRequest, CodeRangeTooLarge},
		{"render without a from", http.MethodGet,
			"/api/v1/teams/devops/schedule/render?until=" + from.Format(time.RFC3339),
			nil, http.StatusBadRequest, CodeInvalidParameter},
		{"delete without expected_version", http.MethodDelete,
			"/api/v1/teams/devops/schedule", nil, http.StatusBadRequest, CodeInvalidParameter},
		{"revision listing with a bad cursor", http.MethodGet,
			"/api/v1/teams/devops/schedule/revisions?before_version=soon",
			nil, http.StatusBadRequest, CodeInvalidParameter},
		{"invalid configuration", http.MethodPut, "/api/v1/teams/devops/schedule/config",
			invalidTimezoneRequest(1), http.StatusBadRequest, CodeValidationFailed},
		{"a user who is not in the team", http.MethodPut, "/api/v1/teams/devops/schedule/config",
			configRequest(1, []string{"nobody"}), http.StatusUnprocessableEntity, CodeUserNotTeamMember},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, e, tc.method, tc.path, tc.body, "denis")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			var body map[string]any
			decodeJSON(t, rec, &body)
			if body["code"] != tc.code {
				t.Fatalf("code = %v, want %q (body %s)", body["code"], tc.code, rec.Body.String())
			}
		})
	}
}

// A malformed body is a rejection like any other and names itself too.
func TestMalformedBodyCarriesACode(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	req := httptest.NewRequest(http.MethodPut, "/api/v1/teams/devops/schedule/config",
		strings.NewReader("{not json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeJSON(t, rec, &body)
	if body.Code != CodeInvalidRequestBody {
		t.Fatalf("code = %q, want %q", body.Code, CodeInvalidRequestBody)
	}
}

func invalidTimezoneRequest(version int64) PutScheduleConfigRequest {
	req := configRequest(version, []string{"denis"})
	req.Timezone = "Mars/Olympus"
	return req
}
