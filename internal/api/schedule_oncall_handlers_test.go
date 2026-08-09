package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
)

const onCallPath = "/api/v1/teams/devops/schedule/on-call"

// The projection the editor and every widget read. It has to name both pairs
// of boundaries, because a mid-shift edit makes them differ and one field
// cannot honestly mean both.
func TestScheduleOnCallReportsBothBoundaryPairs(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	// Wednesday, inside a week that started at Monday 11:00.
	now := time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC)
	env.SetNow(now)
	createSchedule(t, e, []string{"denis"}, []string{"alex"})

	rec := doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out ScheduleOnCallResponse
	decodeJSON(t, rec, &out)

	if out.ScheduleID == "" {
		t.Fatal("schedule_id must be present so the editor can address override mutations")
	}
	if out.OnCall.L1 == nil {
		t.Fatalf("nobody on L1 for a configured schedule: %s", rec.Body.String())
	}
	if got := out.OnCall.L1.UserIDs; len(got) != 1 || got[0] != "denis" {
		t.Fatalf("l1 user_ids = %v, want [denis]", got)
	}
	if !out.OnCall.At.Equal(now) {
		t.Fatalf("at = %s, want the service clock %s", out.OnCall.At, now)
	}

	l1 := out.OnCall.L1
	if l1.GridSlotStart.IsZero() || l1.GridSlotEnd.IsZero() {
		t.Fatalf("grid slot boundaries missing: %+v", l1)
	}
	if l1.AssignmentStart.IsZero() || l1.AssignmentEnd.IsZero() {
		t.Fatalf("assignment boundaries missing: %+v", l1)
	}
	if l1.Source != "rotation" {
		t.Fatalf("source = %q, want rotation", l1.Source)
	}
	if out.OnCall.Warnings == nil {
		t.Fatal("warnings must be an empty array, not null: a client should not branch on the difference")
	}
}

// An edit in the middle of a shift is the case §13 exists for: the handoff
// stays where the grid put it, and the assignment starts when the new
// composition actually took effect. A response that moved grid_slot_start to
// the edit would be claiming a handoff that never happened.
func TestScheduleOnCallSeparatesHandoffFromAssignment(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	start := time.Date(2026, 5, 6, 9, 0, 0, 0, time.UTC)
	env.SetNow(start)
	created := createSchedule(t, e, []string{"denis"})

	editedAt := start.Add(4 * time.Hour)
	env.SetNow(editedAt)
	rec := doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(created.Version, []string{"denis", "alex"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
	var out ScheduleOnCallResponse
	decodeJSON(t, rec, &out)
	if out.OnCall.L1 == nil {
		t.Fatalf("nobody on call after an edit: %s", rec.Body.String())
	}

	if !out.OnCall.L1.AssignmentStart.Equal(editedAt) {
		t.Fatalf("assignment_start = %s, want the moment of the edit %s",
			out.OnCall.L1.AssignmentStart, editedAt)
	}
	if !out.OnCall.L1.GridSlotStart.Before(editedAt) {
		t.Fatalf("grid_slot_start = %s, want the handoff, which is before the edit at %s",
			out.OnCall.L1.GridSlotStart, editedAt)
	}
}

// Three states, one answer: the endpoint reports who is on duty, and "nobody"
// is true for all of them. 404 is reserved for a team that does not exist.
func TestScheduleOnCallAnswersNobodyRatherThanNotFound(t *testing.T) {
	cases := []struct {
		name string
		// configured is whether a schedule exists in this model at all, which
		// is a different question from whether anyone is on duty.
		configured bool
		setup      func(t *testing.T, e *echo.Echo, env *scheduleTestEnv)
	}{
		{"no schedule at all", false, func(t *testing.T, e *echo.Echo, env *scheduleTestEnv) {}},
		{"schedule from before the revision model", false, func(t *testing.T, e *echo.Echo, env *scheduleTestEnv) {
			env.Config.SeedLegacyRoot("legacy-1", "devops")
		}},
		{"deleted schedule", true, func(t *testing.T, e *echo.Echo, env *scheduleTestEnv) {
			created := createSchedule(t, e, []string{"denis"})
			env.SetNow(time.Now().UTC().Add(time.Hour))
			rec := doJSON(t, e, http.MethodDelete,
				"/api/v1/teams/devops/schedule?expected_version="+strconv.FormatInt(created.Version, 10), nil, "denis")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("delete: want 204, got %d: %s", rec.Code, rec.Body.String())
			}
			env.SetNow(time.Now().UTC().Add(2 * time.Hour))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, s, e, env := setupScheduleAPI(t)
			defer s.Close()
			tc.setup(t, e, env)

			rec := doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var out ScheduleOnCallResponse
			decodeJSON(t, rec, &out)
			if out.OnCall.L1 != nil || out.OnCall.L2 != nil {
				t.Fatalf("want nobody on duty, got %+v", out.OnCall)
			}
			// "Nobody on duty" is also what a live schedule between shifts
			// answers, so the response has to say separately whether there is
			// a schedule here at all - otherwise a widget cannot decide
			// between offering "Configure" and saying "nobody right now".
			if tc.configured {
				if out.ScheduleID == "" {
					t.Fatal("a deleted schedule still has an id, and the client needs it")
				}
				if out.DeletedAt == nil {
					t.Fatal("a deleted schedule must report deleted_at")
				}
			} else if out.ScheduleID != "" {
				t.Fatalf("schedule_id = %q, want empty for a team with no schedule in this model",
					out.ScheduleID)
			}
			if out.OnCall.At.IsZero() {
				t.Fatal("at must be set even when nobody is on duty")
			}
			if len(out.OnCall.Warnings) != 0 {
				t.Fatalf("warnings = %v, want none", out.OnCall.Warnings)
			}
		})
	}
}

// §13: "a collision is no less real for being seen through the current view
// rather than the history". The projection carried the warning already; the
// DTO used to drop it on the floor.
func TestScheduleOnCallSurfacesOverrideOverlap(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	now := time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC)
	env.SetNow(now)
	createSchedule(t, e, []string{"denis"})
	scheduleID := scheduleIDOf(t, env, "devops")

	// Written through the repository rather than the API: the command side
	// refuses overlapping overrides, so the only way to reach the state the
	// warning describes is to put it there. That is the point - the warning
	// exists for data the guard did not catch.
	seedOverlappingOverrides(t, env, scheduleID, now)

	rec := doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out ScheduleOnCallResponse
	decodeJSON(t, rec, &out)

	if !hasWarning(out.OnCall.Warnings, string(schedulerender.WarnOverrideOverlap)) {
		t.Fatalf("warnings = %+v, want override_overlap", out.OnCall.Warnings)
	}
	if out.OnCall.L1 == nil || out.OnCall.L1.Source != "override" {
		t.Fatalf("an overlap still resolves to one assignment, got %+v", out.OnCall.L1)
	}
}

// The same warning has to reach the save and preview projections, which share
// the converter. Testing only the new endpoint would let the other two rot.
func TestSaveAndPreviewCarryOnCallWarnings(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	now := time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC)
	env.SetNow(now)
	created := createSchedule(t, e, []string{"denis"})
	seedOverlappingOverrides(t, env, scheduleIDOf(t, env, "devops"), now)

	rec := doJSON(t, e, http.MethodPost, "/api/v1/teams/devops/schedule/preview",
		configRequest(created.Version, []string{"denis", "alex"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var preview SchedulePreviewResponse
	decodeJSON(t, rec, &preview)
	if !hasWarning(preview.OnCallBefore.Warnings, string(schedulerender.WarnOverrideOverlap)) {
		t.Fatalf("preview on_call_before warnings = %+v, want override_overlap", preview.OnCallBefore.Warnings)
	}

	rec = doJSON(t, e, http.MethodPut, "/api/v1/teams/devops/schedule/config",
		configRequest(created.Version, []string{"denis", "alex"}), "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("save: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var saved PutScheduleConfigResponse
	decodeJSON(t, rec, &saved)
	if !hasWarning(saved.OnCallAfter.Warnings, string(schedulerender.WarnOverrideOverlap)) {
		t.Fatalf("save on_call_after warnings = %+v, want override_overlap", saved.OnCallAfter.Warnings)
	}
}

// Reading who is on duty is a view permission: an outsider sees the calendar,
// so refusing them the current assignment would be inconsistent rather than
// safer.
func TestScheduleOnCallIsReadableByViewers(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	createSchedule(t, e, []string{"denis"})

	for _, user := range []string{"denis", "alex"} {
		rec := doJSON(t, e, http.MethodGet, onCallPath, nil, user)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d: %s", user, rec.Code, rec.Body.String())
		}
	}
}

// seedOverlappingOverrides writes two live override heads covering `at`.
func seedOverlappingOverrides(t *testing.T, env *scheduleTestEnv, scheduleID string, at time.Time) {
	t.Helper()
	err := env.Config.WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		for i, userID := range []string{"denis", "alex"} {
			rev := scheduleconfig.OverrideRevision{
				RevisionID: "ovr-rev-" + strconv.Itoa(i),
				ScheduleID: scheduleID,
				OverrideID: "ovr-" + strconv.Itoa(i),
				Revision:   1,
				Layer:      scheduleconfig.LayerL1,
				UserID:     userID,
				ValidFrom:  at.Add(-time.Hour),
				ValidTo:    at.Add(time.Hour),
				RecordedAt: at.Add(time.Duration(i) * time.Microsecond),
			}
			if err := tx.InsertOverrideRevision(context.Background(), &rev); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed overlapping overrides: %v", err)
	}
}

func hasWarning(warnings []ScheduleWarningDTO, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// The wiring guard covers the route like every other schedule route: an
// unwired API must refuse rather than dereference a nil renderer.
func TestScheduleOnCallRefusesWhenUnwired(t *testing.T) {
	s := store.NewMockStore()
	defer s.Close()
	api := NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	api.RegisterRoutes(e)

	rec := doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 from an unwired API, got %d: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeJSON(t, rec, &body)
	if body.Code != CodeServiceUnavailable {
		t.Fatalf("code = %q, want %q", body.Code, CodeServiceUnavailable)
	}
}

// The endpoint answers from one read-only transaction.
//
// This is a cost test more than a correctness one: it is the most frequently
// read thing in the product, and it used to open a second snapshot just to
// fetch the root row. The assertion is here because that kind of regression is
// invisible in behaviour - the answer stays right, it just costs twice as
// much.
func TestScheduleOnCallReadsOneSnapshot(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	createSchedule(t, e, []string{"denis"})

	// Only the request under test is counted; creating the schedule takes its
	// own reads.
	env.Config.Calls = nil

	rec := doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// One entry into the read repository for the whole answer.
	snapshots := 0
	for _, call := range env.Config.Calls {
		if call == "WithinSnapshot" {
			snapshots++
		}
	}
	if snapshots != 1 {
		t.Fatalf("the endpoint opened %d read transactions, want exactly 1: %v",
			snapshots, env.Config.Calls)
	}

	// And it still answers: schedule state and duty from the same read.
	var out ScheduleOnCallResponse
	decodeJSON(t, rec, &out)
	if out.ScheduleID == "" || out.DeletedAt != nil || out.OnCall.L1 == nil {
		t.Fatalf("inconsistent answer: %+v", out)
	}
}

// A repository failure is not "nobody on call": answering an empty projection
// would report the schedule as unstaffed, which is the worst way for this
// endpoint to fail.
func TestScheduleOnCallFailsLoudOnRepositoryError(t *testing.T) {
	_, s, e, env := setupScheduleAPI(t)
	defer s.Close()

	env.SetNow(time.Date(2026, 5, 6, 15, 0, 0, 0, time.UTC))
	createSchedule(t, e, []string{"denis"})
	env.Config.FailOn["WithinSnapshot"] = errors.New("connection reset")

	rec := doJSON(t, e, http.MethodGet, onCallPath, nil, "denis")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}
