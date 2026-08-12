package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// seedLegacySchedule writes a schedule in the pre-upgrade shape, with a row in
// every table that hangs off it by cascade.
func seedLegacySchedule(t *testing.T, s *Store, teamID, scheduleID, userID string) {
	t.Helper()
	// No members: this fixture writes a pre-upgrade row directly, so there is
	// no save pipeline here to validate membership for.
	seedTeam(t, s, teamID)
	if err := s.CreateUser(&model.User{ID: userID, Email: userID + "@example.com", Name: userID}); err != nil {
		t.Fatalf("CreateUser %s: %v", userID, err)
	}

	start := time.Date(2026, 1, 5, 11, 0, 0, 0, time.UTC)
	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, team_id, timezone, l1_rotation_start) VALUES ($1, $2, 'UTC', $3)`,
		scheduleID, teamID, start); err != nil {
		t.Fatalf("insert legacy schedule: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO schedule_users (schedule_id, layer, user_id, position) VALUES ($1, 'l1', $2, 0)`,
		scheduleID, userID); err != nil {
		t.Fatalf("insert legacy schedule user: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO schedule_overrides (id, schedule_id, user_id, start_time, end_time, created_by)
		 VALUES ($1, $2, $3, $4, $5, $3)`,
		"legacy-ovr-"+scheduleID, scheduleID, userID, start.Add(24*time.Hour), start.Add(30*time.Hour)); err != nil {
		t.Fatalf("insert legacy override: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO rotation_epochs (id, schedule_id, layer, user_ids, start_time)
		 VALUES ($1, $2, 'l1', $3, $4)`,
		"legacy-epoch-"+scheduleID, scheduleID, `[["`+userID+`"]]`, start); err != nil {
		t.Fatalf("insert legacy epoch: %v", err)
	}
}

func markerCount(t *testing.T, s *Store) int {
	t.Helper()
	return countRows(t, s, `SELECT COUNT(*) FROM migration_markers WHERE name = $1`, legacyScheduleResetMarker)
}

func TestLegacyScheduleResetCascades(t *testing.T) {
	s := setupTestDB(t)
	seedLegacySchedule(t, s, "devops", "sched-devops", "alice")
	seedLegacySchedule(t, s, "platform", "sched-platform", "bob")

	res, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if res.AlreadyApplied {
		t.Fatal("first run reported as already applied")
	}
	if res.SchedulesDeleted != 2 {
		t.Fatalf("deleted %d schedules, want 2", res.SchedulesDeleted)
	}

	for _, table := range []string{"schedules", "schedule_users", "schedule_overrides", "rotation_epochs"} {
		// nosemgrep: string-formatted-query - table names are hardcoded, not user input
		if n := countRows(t, s, `SELECT COUNT(*) FROM `+table); n != 0 {
			t.Fatalf("%s still holds %d row(s)", table, n)
		}
	}
	// Teams and users are not schedule data and must survive.
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams`); n != 2 {
		t.Fatalf("got %d teams, want 2", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM users`); n != 2 {
		t.Fatalf("got %d users, want 2", n)
	}
	if markerCount(t, s) != 1 {
		t.Fatal("marker was not written")
	}
}

// The marker is what makes a second run harmless: without it, a rerun after
// the operator has recreated schedules would delete them.
func TestLegacyScheduleResetIsNotRepeatable(t *testing.T) {
	s := setupTestDB(t)
	seedLegacySchedule(t, s, "devops", "sched-devops", "alice")

	if _, err := s.ResetLegacySchedules(); err != nil {
		t.Fatalf("first reset: %v", err)
	}

	// Recreate a schedule the way the new write path does. The reset removed
	// the schedule rows, not the team or its people, but the legacy fixture
	// only created one of them - the save pipeline needs both on the team.
	for _, id := range []string{"alice", "bob"} {
		if _, err := s.GetUserByID(id); err != nil {
			if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@example.test"}); err != nil {
				t.Fatalf("CreateUser %s: %v", id, err)
			}
		}
		if err := s.AddTeamMember("devops", id, model.TeamMemberRoleMember); err != nil {
			t.Fatalf("AddTeamMember %s: %v", id, err)
		}
	}

	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev, err := createViaSave(context.Background(), newTestScheduleService(s, start), "devops", revTestConfig(), "", nil)
	if err != nil {
		t.Fatalf("recreate schedule: %v", err)
	}

	res, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("second reset: %v", err)
	}
	if !res.AlreadyApplied {
		t.Fatal("second run did not report AlreadyApplied")
	}
	if res.SchedulesDeleted != 0 {
		t.Fatalf("second run deleted %d schedules", res.SchedulesDeleted)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE id = $1`, rev.ScheduleID); n != 1 {
		t.Fatal("the recreated schedule was deleted by a repeat run")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("got %d revisions after a repeat run, want 1", n)
	}
	if markerCount(t, s) != 1 {
		t.Fatal("marker was duplicated")
	}
}

// Revision data without the marker means someone already used the new write
// path against this database. Deleting then would destroy live schedules.
func TestLegacyScheduleResetRefusesWhenRevisionsExistWithoutMarker(t *testing.T) {
	s := setupTestDB(t)
	seedLegacySchedule(t, s, "platform", "sched-platform", "bob")

	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	seedTeam(t, s, "devops", "alice", "bob")
	if _, err := createViaSave(context.Background(), newTestScheduleService(s, start), "devops", revTestConfig(), "", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	res, err := s.ResetLegacySchedules()
	if !errors.Is(err, ErrLegacyResetUnexpectedState) {
		t.Fatalf("error = %v, want ErrLegacyResetUnexpectedState", err)
	}
	if res != nil {
		t.Fatalf("result returned alongside the error: %+v", res)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules`); n != 2 {
		t.Fatalf("got %d schedules, want both untouched", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("got %d revisions, want 1", n)
	}
	if markerCount(t, s) != 0 {
		t.Fatal("marker was written despite the refusal")
	}
}

func TestLegacyScheduleResetOnEmptyDatabase(t *testing.T) {
	s := setupTestDB(t)

	res, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if res.SchedulesDeleted != 0 || res.AlreadyApplied {
		t.Fatalf("result = %+v, want a no-op first run", res)
	}
	if markerCount(t, s) != 1 {
		t.Fatal("marker was not written")
	}
}
