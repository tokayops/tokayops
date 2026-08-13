package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

func markerCount(t *testing.T, s *Store, name string) int {
	t.Helper()
	return countRows(t, s, `SELECT COUNT(*) FROM migration_markers WHERE name = $1`, name)
}

func TestLegacyScheduleResetCascades(t *testing.T) {
	s := setupTestDB(t)
	makeSchemaLegacy(t, s)
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
	if !res.PhysicalCleanup || res.RowsAlreadyReset {
		t.Fatalf("result = %+v, want a cleanup that deleted the rows itself", res)
	}

	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules`); n != 0 {
		t.Fatalf("schedules still holds %d row(s)", n)
	}
	// The dependent rows went with them, and so did the tables: the cascade is
	// no longer the only thing that had to happen here.
	for _, table := range legacyScheduleTables {
		if legacyTableExists(t, s.db, table) {
			t.Errorf("%s survived the cleanup", table)
		}
	}
	// Teams and users are not schedule data and must survive.
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams`); n != 2 {
		t.Fatalf("got %d teams, want 2", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM users`); n != 2 {
		t.Fatalf("got %d users, want 2", n)
	}
	if markerCount(t, s, legacyScheduleResetMarker) != 1 {
		t.Error("the reset marker was not written")
	}
	if markerCount(t, s, legacyScheduleDropMarker) != 1 {
		t.Error("the drop marker was not written")
	}
}

// The marker is what makes a second run harmless: without it, a rerun after
// the operator has recreated schedules would delete them.
func TestLegacyScheduleResetIsNotRepeatable(t *testing.T) {
	s := setupTestDB(t)
	makeSchemaLegacy(t, s)
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
	if res.SchedulesDeleted != 0 || res.PhysicalCleanup {
		t.Fatalf("second run did work: %+v", res)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE id = $1`, rev.ScheduleID); n != 1 {
		t.Fatal("the recreated schedule was deleted by a repeat run")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("got %d revisions after a repeat run, want 1", n)
	}
	if markerCount(t, s, legacyScheduleDropMarker) != 1 {
		t.Fatal("the drop marker was duplicated")
	}
}

// TestLegacyScheduleResetCleansUpADatabaseThatAlreadyRanTheReset is the branch
// that did not exist before the physical cutover, and the one where getting it
// wrong is worst.
//
// Such a database has the reset marker but still carries the legacy tables: it
// upgraded before this release. It needs the cleanup, and the schedules in it
// are the recreated, revision-managed ones the whole epic exists for. Deleting
// them - which is what the pre-cutover code path would do without its marker
// short-circuit - is the catastrophe this test exists to prevent.
func TestLegacyScheduleResetCleansUpADatabaseThatAlreadyRanTheReset(t *testing.T) {
	s := setupTestDB(t)
	makeSchemaLegacy(t, s)

	// The state an earlier upgrade left: the reset marker is there, the legacy
	// schema is not gone, and the schedules are the new ones.
	if _, err := s.db.Exec(
		`INSERT INTO migration_markers (name) VALUES ($1)`, legacyScheduleResetMarker); err != nil {
		t.Fatalf("seed the old marker: %v", err)
	}
	seedTeam(t, s, "devops", "alice", "bob")
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev, err := createViaSave(context.Background(), newTestScheduleService(s, start), "devops", revTestConfig(), "", nil)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	res, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if res.AlreadyApplied {
		t.Fatal("a database that never had the physical cleanup reported AlreadyApplied")
	}
	if res.SchedulesDeleted != 0 {
		t.Fatalf("the cleanup deleted %d schedule(s); it must touch no data here", res.SchedulesDeleted)
	}
	if !res.PhysicalCleanup || !res.RowsAlreadyReset {
		t.Fatalf("result = %+v, want a cleanup that reports it touched no data", res)
	}

	// The live schedule and its history are exactly where they were.
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE id = $1`, rev.ScheduleID); n != 1 {
		t.Fatal("the cleanup destroyed a live revision-managed schedule")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE schedule_id = $1`, rev.ScheduleID); n != 1 {
		t.Fatal("the cleanup destroyed the revision history")
	}
	for _, table := range legacyScheduleTables {
		if legacyTableExists(t, s.db, table) {
			t.Errorf("%s survived the cleanup", table)
		}
	}
	if err := s.RequireCutoverSchema(); err != nil {
		t.Fatalf("the schema is not final after the cleanup: %v", err)
	}
}

// The intermediate state: the rows were reset, but something in the database
// still has no horizon. SET NOT NULL would refuse it with a bare DDL error
// naming a column, which tells the one person who can fix it nothing.
func TestLegacyScheduleResetRefusesARootWithoutHorizonByName(t *testing.T) {
	s := setupTestDB(t)
	makeSchemaLegacy(t, s)

	if _, err := s.db.Exec(
		`INSERT INTO migration_markers (name) VALUES ($1)`, legacyScheduleResetMarker); err != nil {
		t.Fatalf("seed the old marker: %v", err)
	}
	seedLegacySchedule(t, s, "devops", "sched-stranded", "alice")

	res, err := s.ResetLegacySchedules()
	if !errors.Is(err, ErrLegacyResetRootWithoutHorizon) {
		t.Fatalf("error = %v, want ErrLegacyResetRootWithoutHorizon", err)
	}
	if res != nil {
		t.Fatalf("result returned alongside the error: %+v", res)
	}
	if !strings.Contains(err.Error(), "sched-stranded") {
		t.Fatalf("the refusal must name the schedule that blocks it: %v", err)
	}
	// Refused means refused: nothing was dropped on the way to finding out.
	if !legacyTableExists(t, s.db, "rotation_epochs") {
		t.Error("the cleanup ran despite the refusal")
	}
	if markerCount(t, s, legacyScheduleDropMarker) != 0 {
		t.Error("the drop marker was written despite the refusal")
	}
}

// Revision data without the marker means someone already used the new write
// path against this database. Deleting then would destroy live schedules.
func TestLegacyScheduleResetRefusesWhenRevisionsExistWithoutMarker(t *testing.T) {
	s := setupTestDB(t)
	makeSchemaLegacy(t, s)
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
	if markerCount(t, s, legacyScheduleResetMarker) != 0 {
		t.Error("the reset marker was written despite the refusal")
	}
	if markerCount(t, s, legacyScheduleDropMarker) != 0 {
		t.Error("the drop marker was written despite the refusal")
	}
}

// A database that never carried the legacy schema at all still records both
// markers, so a second run is the no-op it is everywhere else.
func TestLegacyScheduleResetOnFreshDatabase(t *testing.T) {
	s := setupTestDB(t)

	res, err := s.ResetLegacySchedules()
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	// Not RowsAlreadyReset: nothing had been reset here, there was simply
	// nothing to reset. The two share a count and the CLI says different
	// things about them.
	if res.SchedulesDeleted != 0 || res.AlreadyApplied || !res.PhysicalCleanup || res.RowsAlreadyReset {
		t.Fatalf("result = %+v, want a no-op first run that records the cleanup", res)
	}
	if markerCount(t, s, legacyScheduleResetMarker) != 1 {
		t.Error("the reset marker was not written")
	}
	if markerCount(t, s, legacyScheduleDropMarker) != 1 {
		t.Error("the drop marker was not written")
	}
	if err := s.RequireCutoverSchema(); err != nil {
		t.Fatalf("a fresh database is not in the final shape: %v", err)
	}
}
