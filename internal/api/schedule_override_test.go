package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

func seedOverrideEnv(t *testing.T, a *API, s interface {
	CreateUser(u *model.User) error
	CreateTeam(team *model.Team) error
	AddTeamMember(teamID, userID string, role model.TeamMemberRole) error
	CreateSchedule(s *model.Schedule) error
	CreateScheduleOverride(o *model.ScheduleOverride) error
}) {
	t.Helper()

	s.CreateUser(&model.User{ID: "admin", Email: "admin@test.com", Name: "Admin", Role: model.UserRoleAdmin})
	s.CreateUser(&model.User{ID: "alice", Email: "alice@test.com", Name: "Alice", Role: model.UserRoleUser})
	s.CreateUser(&model.User{ID: "bob", Email: "bob@test.com", Name: "Bob", Role: model.UserRoleUser})

	s.CreateTeam(&model.Team{ID: "team-1", Name: "Team One"})
	s.AddTeamMember("team-1", "alice", model.TeamMemberRoleMember)
	s.AddTeamMember("team-1", "bob", model.TeamMemberRoleMember)

	s.CreateSchedule(&model.Schedule{
		ID:              "sched-1",
		TeamID:          "team-1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "09:00",
		L1RotationStart: time.Now(),
	})
}

// seedRenderEnv sets up a full environment for render/calendar tests with deterministic times.
func seedRenderEnv(t *testing.T, s *store.MockStore) {
	t.Helper()

	s.CreateUser(&model.User{ID: "admin", Email: "admin@test.com", Name: "Admin", Role: model.UserRoleAdmin})
	s.CreateUser(&model.User{ID: "alice", Email: "alice@test.com", Name: "Alice", Role: model.UserRoleUser})
	s.CreateUser(&model.User{ID: "bob", Email: "bob@test.com", Name: "Bob", Role: model.UserRoleUser})

	s.CreateTeam(&model.Team{ID: "team-1", Name: "Team One"})
	s.AddTeamMember("team-1", "alice", model.TeamMemberRoleMember)
	s.AddTeamMember("team-1", "bob", model.TeamMemberRoleMember)

	s.CreateSchedule(&model.Schedule{
		ID:              "sched-1",
		TeamID:          "team-1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "09:00",
		L1RotationStart: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
	})

	// L1 epoch: alice and bob rotating daily starting March 1
	s.CreateRotationEpoch(&model.RotationEpoch{
		ID:        "epoch-1",
		ScheduleID: "sched-1",
		Layer:     "l1",
		Groups:    [][]string{{"alice"}, {"bob"}},
		StartTime: time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:   nil,
	})
}

// renderSchedule is a helper that calls the render endpoint and returns parsed entries.
func renderSchedule(t *testing.T, e *echo.Echo, teamID, from, until, userID string) RenderResponse {
	t.Helper()
	url := fmt.Sprintf("/api/v1/teams/%s/schedule/render?from=%s&until=%s", teamID, from, until)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	addAuth(req, userID)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("render: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp RenderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("render: failed to parse response: %v", err)
	}
	return resp
}

func TestUpdateScheduleOverride_Success(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedOverrideEnv(t, nil, s)

	// Create an override to edit
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-1",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 2, 9, 0, 0, 0, time.UTC),
		Reason:     "Vacation cover",
	})

	body := `{
		"user_id": "bob",
		"start_time": "2025-03-01T10:00:00Z",
		"end_time": "2025-03-02T10:00:00Z",
		"reason": "Updated reason"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-1", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated model.ScheduleOverride
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}
	if updated.UserID != "bob" {
		t.Errorf("Expected user_id bob, got %s", updated.UserID)
	}
	if updated.Reason != "Updated reason" {
		t.Errorf("Expected reason 'Updated reason', got %s", updated.Reason)
	}
}

func TestUpdateScheduleOverride_LocalTimezone(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedOverrideEnv(t, nil, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-tz",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 2, 9, 0, 0, 0, time.UTC),
	})

	body := `{
		"user_id": "alice",
		"timezone": "Asia/Bangkok",
		"start_time_local": "2025-03-01T16:00",
		"end_time_local": "2025-03-02T16:00",
		"reason": "TZ test"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-tz", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated model.ScheduleOverride
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	// Asia/Bangkok is UTC+7, so 16:00 local = 09:00 UTC
	expectedStart := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	if !updated.StartTime.Equal(expectedStart) {
		t.Errorf("Expected start_time %v, got %v", expectedStart, updated.StartTime)
	}
}

func TestUpdateScheduleOverride_NotFound(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedOverrideEnv(t, nil, s)

	body := `{"user_id": "alice", "start_time": "2025-03-01T09:00:00Z", "end_time": "2025-03-02T09:00:00Z"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/nonexistent", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// ScopeScheduleOverride middleware returns 404 for non-existent override
	if rec.Code != http.StatusNotFound {
		t.Errorf("Expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateScheduleOverride_Conflict(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedOverrideEnv(t, nil, s)

	// Create two non-overlapping overrides
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-a",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 2, 9, 0, 0, 0, time.UTC),
	})
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-b",
		ScheduleID: "sched-1",
		UserID:     "bob",
		StartTime:  time.Date(2025, 3, 3, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 4, 9, 0, 0, 0, time.UTC),
	})

	// Try to update ov-a to overlap with ov-b
	body := `{
		"user_id": "alice",
		"start_time": "2025-03-03T08:00:00Z",
		"end_time": "2025-03-03T10:00:00Z"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-a", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("Expected 409 Conflict, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateScheduleOverride_SelfOverlapAllowed(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedOverrideEnv(t, nil, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-self",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 2, 9, 0, 0, 0, time.UTC),
	})

	// Update the same override with slightly shifted times (still overlaps its own old range)
	body := `{
		"user_id": "alice",
		"start_time": "2025-03-01T10:00:00Z",
		"end_time": "2025-03-02T10:00:00Z"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-self", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200 (self-overlap allowed), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateScheduleOverride_ValidationErrors(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedOverrideEnv(t, nil, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-val",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 2, 9, 0, 0, 0, time.UTC),
	})

	tests := []struct {
		name string
		body string
		code int
	}{
		{
			name: "missing user_id",
			body: `{"start_time": "2025-03-01T09:00:00Z", "end_time": "2025-03-02T09:00:00Z"}`,
			code: http.StatusBadRequest,
		},
		{
			name: "end before start",
			body: `{"user_id": "alice", "start_time": "2025-03-02T09:00:00Z", "end_time": "2025-03-01T09:00:00Z"}`,
			code: http.StatusBadRequest,
		},
		{
			name: "non-team member",
			body: `{"user_id": "nonexistent", "start_time": "2025-03-01T09:00:00Z", "end_time": "2025-03-02T09:00:00Z"}`,
			code: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-val", strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			addAuth(req, "admin")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != tt.code {
				t.Errorf("Expected %d, got %d: %s", tt.code, rec.Code, rec.Body.String())
			}
		})
	}
}

// ========================================
// Calendar Render + Override E2E Tests
// ========================================

func TestRenderSchedule_OverrideMetadata(t *testing.T) {
	// Verify that the render endpoint returns override_id, schedule_id,
	// override_start, override_end, and reason for override entries.
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-render-1",
		ScheduleID: "sched-1",
		UserID:     "bob",
		StartTime:  time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 1, 18, 0, 0, 0, time.UTC),
		Reason:     "Covering for Alice",
	})

	resp := renderSchedule(t, e, "team-1",
		"2025-03-01T09:00:00Z", "2025-03-02T09:00:00Z", "admin")

	// Find the override entry
	var overrideEntry *RenderEntry
	for i := range resp.Entries {
		if resp.Entries[i].Layer == "override" {
			overrideEntry = &resp.Entries[i]
			break
		}
	}
	if overrideEntry == nil {
		t.Fatalf("No override entry found in render response. Entries: %+v", resp.Entries)
	}

	if overrideEntry.OverrideID != "ov-render-1" {
		t.Errorf("override_id: expected ov-render-1, got %s", overrideEntry.OverrideID)
	}
	if overrideEntry.ScheduleID != "sched-1" {
		t.Errorf("schedule_id: expected sched-1, got %s", overrideEntry.ScheduleID)
	}
	if len(overrideEntry.UserIDs) == 0 || overrideEntry.UserIDs[0] != "bob" {
		t.Errorf("user_ids: expected [bob], got %v", overrideEntry.UserIDs)
	}
	if len(overrideEntry.UserNames) == 0 || overrideEntry.UserNames[0] != "Bob" {
		t.Errorf("user_names: expected [Bob], got %v", overrideEntry.UserNames)
	}
	if overrideEntry.Reason != "Covering for Alice" {
		t.Errorf("reason: expected 'Covering for Alice', got %s", overrideEntry.Reason)
	}

	// override_start/override_end should be the original override times
	expectedStart := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 3, 1, 18, 0, 0, 0, time.UTC)
	if overrideEntry.OverrideStart == nil || !overrideEntry.OverrideStart.Equal(expectedStart) {
		t.Errorf("override_start: expected %v, got %v", expectedStart, overrideEntry.OverrideStart)
	}
	if overrideEntry.OverrideEnd == nil || !overrideEntry.OverrideEnd.Equal(expectedEnd) {
		t.Errorf("override_end: expected %v, got %v", expectedEnd, overrideEntry.OverrideEnd)
	}

	// L1 entries should NOT have override metadata
	for _, entry := range resp.Entries {
		if entry.Layer == "l1" {
			if entry.OverrideID != "" {
				t.Errorf("L1 entry should not have override_id, got %s", entry.OverrideID)
			}
		}
	}
}

func TestRenderSchedule_MultiDayOverridePreservesMetadata(t *testing.T) {
	// A multi-day override gets split by calendar days in the render pipeline.
	// Each resulting entry must carry the same override_id and the ORIGINAL
	// override start/end times (not the per-day segment times).
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	// 3-day override: March 1 12:00 → March 3 18:00
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-multiday",
		ScheduleID: "sched-1",
		UserID:     "bob",
		StartTime:  time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 3, 18, 0, 0, 0, time.UTC),
		Reason:     "Multi-day cover",
	})

	resp := renderSchedule(t, e, "team-1",
		"2025-03-01T00:00:00Z", "2025-03-04T00:00:00Z", "admin")

	// Collect override entries
	var overrideEntries []RenderEntry
	for _, entry := range resp.Entries {
		if entry.Layer == "override" {
			overrideEntries = append(overrideEntries, entry)
		}
	}

	// Should have multiple override entries (one per day after split)
	if len(overrideEntries) < 2 {
		t.Fatalf("Expected multiple override entries for multi-day override, got %d. All entries: %+v",
			len(overrideEntries), resp.Entries)
	}

	origStart := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	origEnd := time.Date(2025, 3, 3, 18, 0, 0, 0, time.UTC)

	for i, entry := range overrideEntries {
		if entry.OverrideID != "ov-multiday" {
			t.Errorf("entry[%d]: override_id should be ov-multiday, got %s", i, entry.OverrideID)
		}
		if entry.ScheduleID != "sched-1" {
			t.Errorf("entry[%d]: schedule_id should be sched-1, got %s", i, entry.ScheduleID)
		}
		// Original times should be preserved, not the per-day segment times
		if entry.OverrideStart == nil || !entry.OverrideStart.Equal(origStart) {
			t.Errorf("entry[%d]: override_start should be %v, got %v", i, origStart, entry.OverrideStart)
		}
		if entry.OverrideEnd == nil || !entry.OverrideEnd.Equal(origEnd) {
			t.Errorf("entry[%d]: override_end should be %v, got %v", i, origEnd, entry.OverrideEnd)
		}
		if entry.Reason != "Multi-day cover" {
			t.Errorf("entry[%d]: reason should be 'Multi-day cover', got %s", i, entry.Reason)
		}
	}
}

func TestRenderSchedule_AdjacentOverridesNotMerged(t *testing.T) {
	// Two adjacent overrides for the same user (different override IDs) should
	// appear as separate entries in the render response, not merged.
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-adj-1",
		ScheduleID: "sched-1",
		UserID:     "bob",
		StartTime:  time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 1, 14, 0, 0, 0, time.UTC),
		Reason:     "First block",
	})
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-adj-2",
		ScheduleID: "sched-1",
		UserID:     "bob",
		StartTime:  time.Date(2025, 3, 1, 14, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 1, 16, 0, 0, 0, time.UTC),
		Reason:     "Second block",
	})

	resp := renderSchedule(t, e, "team-1",
		"2025-03-01T09:00:00Z", "2025-03-02T09:00:00Z", "admin")

	var overrideEntries []RenderEntry
	for _, entry := range resp.Entries {
		if entry.Layer == "override" {
			overrideEntries = append(overrideEntries, entry)
		}
	}

	if len(overrideEntries) != 2 {
		t.Fatalf("Expected 2 separate override entries, got %d. Entries: %+v",
			len(overrideEntries), overrideEntries)
	}

	// Verify they have distinct override IDs
	ids := map[string]bool{}
	for _, entry := range overrideEntries {
		ids[entry.OverrideID] = true
	}
	if !ids["ov-adj-1"] || !ids["ov-adj-2"] {
		t.Errorf("Expected override IDs ov-adj-1 and ov-adj-2, got %v", ids)
	}
}

func TestRenderSchedule_UpdateOverrideReflectsInCalendar(t *testing.T) {
	// Edit an override via the API, then verify the render endpoint
	// reflects the updated user, times, and reason.
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-edit",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 1, 18, 0, 0, 0, time.UTC),
		Reason:     "Original",
	})

	// Update the override: change user, shift times, update reason
	body := `{
		"user_id": "bob",
		"start_time": "2025-03-01T14:00:00Z",
		"end_time": "2025-03-01T20:00:00Z",
		"reason": "Updated cover"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-edit", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Render and check
	resp := renderSchedule(t, e, "team-1",
		"2025-03-01T09:00:00Z", "2025-03-02T09:00:00Z", "admin")

	var overrideEntry *RenderEntry
	for i := range resp.Entries {
		if resp.Entries[i].Layer == "override" {
			overrideEntry = &resp.Entries[i]
			break
		}
	}
	if overrideEntry == nil {
		t.Fatalf("No override entry after update. Entries: %+v", resp.Entries)
	}

	if len(overrideEntry.UserIDs) == 0 || overrideEntry.UserIDs[0] != "bob" {
		t.Errorf("After update: user_ids should be [bob], got %v", overrideEntry.UserIDs)
	}
	if overrideEntry.Reason != "Updated cover" {
		t.Errorf("After update: reason should be 'Updated cover', got %s", overrideEntry.Reason)
	}

	// The override_start/override_end should reflect the new times
	expectedStart := time.Date(2025, 3, 1, 14, 0, 0, 0, time.UTC)
	expectedEnd := time.Date(2025, 3, 1, 20, 0, 0, 0, time.UTC)
	if overrideEntry.OverrideStart == nil || !overrideEntry.OverrideStart.Equal(expectedStart) {
		t.Errorf("After update: override_start should be %v, got %v", expectedStart, overrideEntry.OverrideStart)
	}
	if overrideEntry.OverrideEnd == nil || !overrideEntry.OverrideEnd.Equal(expectedEnd) {
		t.Errorf("After update: override_end should be %v, got %v", expectedEnd, overrideEntry.OverrideEnd)
	}
}

func TestRenderSchedule_DeleteOverrideRemovesFromCalendar(t *testing.T) {
	// After deleting an override, the render endpoint should no longer
	// return it; the L1 rotation should fill the gap.
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-del",
		ScheduleID: "sched-1",
		UserID:     "bob",
		StartTime:  time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2025, 3, 1, 18, 0, 0, 0, time.UTC),
		Reason:     "To be deleted",
	})

	// Verify override is present
	resp := renderSchedule(t, e, "team-1",
		"2025-03-01T09:00:00Z", "2025-03-02T09:00:00Z", "admin")
	hasOverride := false
	for _, entry := range resp.Entries {
		if entry.Layer == "override" {
			hasOverride = true
			break
		}
	}
	if !hasOverride {
		t.Fatal("Override should be present before deletion")
	}

	// Delete the override
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/schedules/sched-1/overrides/ov-del", nil)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Render again — no override entries should remain
	resp = renderSchedule(t, e, "team-1",
		"2025-03-01T09:00:00Z", "2025-03-02T09:00:00Z", "admin")
	for _, entry := range resp.Entries {
		if entry.Layer == "override" {
			t.Errorf("Override entry still present after deletion: %+v", entry)
		}
	}
}

func TestRenderSchedule_CreateOverrideViaAPIAndRender(t *testing.T) {
	// Full round-trip: create an override via the POST API, then verify
	// it appears correctly in the render response.
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	// Use future dates so the start_time validation passes
	futureStart := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	futureEnd := futureStart.Add(10 * time.Hour)

	body := fmt.Sprintf(`{
		"user_id": "bob",
		"start_time": "%s",
		"end_time": "%s",
		"reason": "Swap shift"
	}`, futureStart.Format(time.RFC3339), futureEnd.Format(time.RFC3339))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams/team-1/schedule/overrides", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created model.ScheduleOverride
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("Failed to parse create response: %v", err)
	}

	// Render the calendar and find the override
	renderFrom := futureStart.Add(-1 * time.Hour).Format(time.RFC3339)
	renderUntil := futureEnd.Add(1 * time.Hour).Format(time.RFC3339)
	resp := renderSchedule(t, e, "team-1", renderFrom, renderUntil, "admin")

	var found *RenderEntry
	for i := range resp.Entries {
		if resp.Entries[i].OverrideID == created.ID {
			found = &resp.Entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Created override %s not found in render. Entries: %+v", created.ID, resp.Entries)
	}

	if len(found.UserIDs) == 0 || found.UserIDs[0] != "bob" {
		t.Errorf("user_ids: expected [bob], got %v", found.UserIDs)
	}
	if found.Reason != "Swap shift" {
		t.Errorf("reason: expected 'Swap shift', got %s", found.Reason)
	}
	if found.Layer != "override" {
		t.Errorf("layer: expected override, got %s", found.Layer)
	}
}

func TestCreateScheduleOverride_StartTimeInPast(t *testing.T) {
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	body := `{
		"user_id": "bob",
		"start_time": "2020-01-01T10:00:00Z",
		"end_time": "2020-01-01T20:00:00Z",
		"reason": "Past override"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/teams/team-1/schedule/overrides", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Failed to parse error response: %v", err)
	}
	if resp.Error != "start_time cannot be in the past" {
		t.Errorf("expected 'start_time cannot be in the past', got %q", resp.Error)
	}
}

func TestUpdateScheduleOverride_StartTimeInPastAllowed(t *testing.T) {
	// Editing an existing override should allow start_time in the past
	_, s, e := setupTestAPI(t)
	seedRenderEnv(t, s)

	pastStart := time.Date(2020, 1, 1, 10, 0, 0, 0, time.UTC)
	pastEnd := time.Date(2099, 1, 1, 20, 0, 0, 0, time.UTC)
	s.CreateScheduleOverride(&model.ScheduleOverride{
		ID:         "ov-past",
		ScheduleID: "sched-1",
		UserID:     "alice",
		StartTime:  pastStart,
		EndTime:    pastEnd,
		Reason:     "Old override",
	})

	body := `{
		"user_id": "bob",
		"start_time": "2020-01-01T10:00:00Z",
		"end_time": "2099-01-01T20:00:00Z",
		"reason": "Updated old override"
	}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/schedules/sched-1/overrides/ov-past", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	addAuth(req, "admin")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
