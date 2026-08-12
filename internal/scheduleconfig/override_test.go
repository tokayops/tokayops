package scheduleconfig_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// overrideFixture is a fixture with a schedule already in place, since every
// override command needs one.
func overrideFixture(t *testing.T) *commandFixture {
	t.Helper()
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
		ActorID: "alice",
	})
	return f
}

func (f *commandFixture) createOverride(t *testing.T, userID string, from, to time.Time) *scheduleconfig.OverrideRevision {
	t.Helper()
	rev, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID:    userID,
		ValidFrom: from,
		ValidTo:   to,
		ActorID:   "alice",
	})
	if err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}
	return rev
}

// Create writes revision 1, update appends, delete appends a tombstone -
// nothing is ever overwritten or removed.
func TestOverrideRevisionLifecycle(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)
	from := f.clock.at.Add(24 * time.Hour)

	created := f.createOverride(t, "bob", from, from.Add(2*time.Hour))
	if created.Revision != 1 {
		t.Fatalf("first revision = %d, want 1", created.Revision)
	}
	if created.Layer != scheduleconfig.LayerL1 {
		t.Fatalf("layer = %q, want l1", created.Layer)
	}

	updated, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			UserID:    "carol",
			ValidFrom: from,
			ValidTo:   from.Add(3 * time.Hour),
			ActorID:   "alice",
		})
	if err != nil {
		t.Fatalf("UpdateOverride: %v", err)
	}
	if updated.Revision != 2 || updated.OverrideID != created.OverrideID {
		t.Fatalf("update produced %+v, want revision 2 of the same override", updated)
	}

	if err := f.svc.DeleteOverride(context.Background(), scheduleID, created.OverrideID, 2, "alice"); err != nil {
		t.Fatalf("DeleteOverride: %v", err)
	}

	history := f.repo.OverrideRevisions(scheduleID)
	if len(history) != 3 {
		t.Fatalf("history holds %d revisions, want 3 (append-only)", len(history))
	}
	if !history[2].Deleted {
		t.Fatal("the delete must append a tombstone rather than remove a row")
	}

	// Once tombstoned, the override is not found: an update must not restart
	// its numbering and resurrect it.
	_, err = f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 3,
		scheduleconfig.OverrideCommand{
			UserID:    "bob",
			ValidFrom: from,
			ValidTo:   from.Add(time.Hour),
		})
	if !errors.Is(err, scheduleconfig.ErrOverrideNotFound) {
		t.Fatalf("error = %v, want ErrOverrideNotFound", err)
	}
}

func TestOverrideExpectedRevisionConflict(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)
	from := f.clock.at.Add(24 * time.Hour)
	created := f.createOverride(t, "bob", from, from.Add(2*time.Hour))

	_, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 5,
		scheduleconfig.OverrideCommand{UserID: "bob", ValidFrom: from, ValidTo: from.Add(time.Hour)})

	var conflict *scheduleconfig.OverrideRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want an override revision conflict", err)
	}
	if conflict.Expected != 5 || conflict.Current != 1 {
		t.Fatalf("conflict = %+v, want expected 5 current 1", conflict)
	}
	if got := len(f.repo.OverrideRevisions(scheduleID)); got != 1 {
		t.Fatalf("a rejected update wrote a revision: %d total", got)
	}
}

// recorded_at is audit only, and normalized to what the database stores.
//
// It used to be forced monotonic against a MAX(recorded_at) query on every
// override command, so that as-of reads could resolve a head by time. As-of
// reads are gone, the head is decided by the revision chain, and the query
// went with them - one round trip less per write.
func TestOverrideRecordedAtIsAtDatabaseResolution(t *testing.T) {
	f := overrideFixture(t)
	from := f.clock.at.Add(24 * time.Hour)

	rev := f.createOverride(t, "bob", from, from.Add(time.Hour))
	if rev.RecordedAt.Truncate(scheduleconfig.TimestampResolution) != rev.RecordedAt {
		t.Fatalf("recorded_at %v is not at database resolution", rev.RecordedAt)
	}
}

func TestOverrideOverlapRejectedSameLayer(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)
	from := f.clock.at.Add(24 * time.Hour)
	first := f.createOverride(t, "bob", from, from.Add(4*time.Hour))

	_, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID:    "carol",
		ValidFrom: from.Add(2 * time.Hour),
		ValidTo:   from.Add(6 * time.Hour),
	})
	var overlap *scheduleconfig.OverrideOverlapError
	if !errors.As(err, &overlap) {
		t.Fatalf("error = %v, want an overlap rejection", err)
	}
	if len(overlap.Conflicts) != 1 || overlap.Conflicts[0].OverrideID != first.OverrideID {
		t.Fatalf("the rejection must name what it collided with, got %+v", overlap.Conflicts)
	}

	// An override does not conflict with the version of itself it replaces.
	if _, err := f.svc.UpdateOverride(context.Background(), scheduleID, first.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			UserID:    "bob",
			ValidFrom: from,
			ValidTo:   from.Add(6 * time.Hour),
		}); err != nil {
		t.Fatalf("extending an override collided with itself: %v", err)
	}

	// Adjacent, not overlapping: the interval is half-open.
	if _, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID:    "carol",
		ValidFrom: from.Add(6 * time.Hour),
		ValidTo:   from.Add(8 * time.Hour),
	}); err != nil {
		t.Fatalf("an adjacent override was rejected as overlapping: %v", err)
	}
}

// An append-only table is its own audit trail, config_version does not change,
// and there are no consumers of an override event.
func TestOverrideDoesNotAdvanceVersionOrEmitEvent(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)
	from := f.clock.at.Add(24 * time.Hour)

	created := f.createOverride(t, "bob", from, from.Add(time.Hour))
	if _, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{UserID: "bob", ValidFrom: from, ValidTo: from.Add(2 * time.Hour)}); err != nil {
		t.Fatalf("UpdateOverride: %v", err)
	}

	root, _ := f.repo.RootByTeam("devops")
	if root.ConfigVersion != 1 {
		t.Fatalf("config version = %d, want it unchanged at 1", root.ConfigVersion)
	}
	// An override command touches override revisions only: the schedule's own
	// revision chain must not gain anything.
	if got := len(f.repo.Revisions(scheduleID)); got != 1 {
		t.Fatalf("got %d schedule revisions, want only the one from the create", got)
	}
}

// A retroactive correction is a legitimate record: the table is append-only
// and an as-of query still shows what was known at any earlier moment.
func TestOverridePastValidFromAllowed(t *testing.T) {
	f := overrideFixture(t)
	past := f.clock.at.Add(-48 * time.Hour)

	rev := f.createOverride(t, "bob", past, past.Add(time.Hour))
	if !rev.ValidFrom.Equal(past) {
		t.Fatalf("valid_from = %v, want the past instant %v", rev.ValidFrom, past)
	}
}

// An inactive schedule has nobody on duty, so there is nobody to stand in for
// - and allowing it would let an override outlive the delete that ended it.
func TestOverrideCommandsRefuseDeletedSchedule(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)
	from := f.clock.at.Add(24 * time.Hour)
	created := f.createOverride(t, "bob", from, from.Add(time.Hour))

	f.clock.advance(time.Hour)
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{ExpectedVersion: 1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID: "bob", ValidFrom: from, ValidTo: from.Add(time.Hour),
	}); !errors.Is(err, scheduleconfig.ErrScheduleDeleted) {
		t.Fatalf("create on a deleted schedule: %v, want ErrScheduleDeleted", err)
	}
	if _, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{UserID: "bob", ValidFrom: from, ValidTo: from.Add(time.Hour)},
	); !errors.Is(err, scheduleconfig.ErrScheduleDeleted) {
		t.Fatalf("update on a deleted schedule: %v, want ErrScheduleDeleted", err)
	}
	if err := f.svc.DeleteOverride(context.Background(), scheduleID, created.OverrideID, 1, "alice"); !errors.Is(err, scheduleconfig.ErrScheduleDeleted) {
		t.Fatalf("delete on a deleted schedule: %v, want ErrScheduleDeleted", err)
	}
}

// The target of an override has to be a team member, exactly as a rotation
// group member does - an override can otherwise put an outsider on call.
func TestOverrideTargetMustBeTeamMember(t *testing.T) {
	f := overrideFixture(t)
	from := f.clock.at.Add(24 * time.Hour)

	_, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID: "mallory", ValidFrom: from, ValidTo: from.Add(time.Hour),
	})
	var notMember *scheduleconfig.UserNotTeamMemberError
	if !errors.As(err, &notMember) {
		t.Fatalf("error = %v, want a membership rejection", err)
	}
}
