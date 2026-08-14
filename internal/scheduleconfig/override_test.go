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

	if err := f.svc.CancelOverride(context.Background(), scheduleID, created.OverrideID, 2, "alice", nil); err != nil {
		t.Fatalf("CancelOverride: %v", err)
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

// Recording a stand-in that already happened is allowed, and is not in tension
// with refusing to cancel one that already happened.
//
// The two are opposites: entering "bob covered yesterday, we forgot" ADDS a
// fact, and the revision that carries it says when it was recorded. Cancelling
// a shift that was served would remove one. The append-only table keeps both
// readable; only one of them changes what the calendar says happened.
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
	if err := f.svc.CancelOverride(context.Background(), scheduleID, created.OverrideID, 1, "alice", nil); !errors.Is(err, scheduleconfig.ErrScheduleDeleted) {
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

// ---------------------------------------------------------------------------
// Temporal semantics: the future can be cancelled, the past cannot be un-lived
// ---------------------------------------------------------------------------

// liveHead is the override's current head, or nil when it is tombstoned.
func (f *commandFixture) liveHead(t *testing.T, scheduleID, overrideID string) *scheduleconfig.OverrideRevision {
	t.Helper()
	var head *scheduleconfig.OverrideRevision
	for _, rev := range f.repo.OverrideRevisions(scheduleID) {
		if rev.OverrideID != overrideID {
			continue
		}
		r := rev
		if head == nil || r.Revision > head.Revision {
			head = &r
		}
	}
	if head != nil && head.Deleted {
		return nil
	}
	return head
}

// TestCancelOverrideBeforeItStartsRemovesIt: nothing was served, so there is
// nothing to keep. This is the one case where a tombstone is still right.
func TestCancelOverrideBeforeItStartsRemovesIt(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)
	from := f.clock.at.Add(24 * time.Hour)
	created := f.createOverride(t, "bob", from, from.Add(2*time.Hour))

	if err := f.svc.CancelOverride(context.Background(), scheduleID, created.OverrideID, 1, "alice", nil); err != nil {
		t.Fatalf("CancelOverride: %v", err)
	}
	if head := f.liveHead(t, scheduleID, created.OverrideID); head != nil {
		t.Fatalf("an override that never started is still live: %+v", head)
	}
}

// TestCancelOverrideInForceKeepsTheHoursAlreadyServed is the whole point of
// the change: the stand-in covered from 08:00, and cancelling at 10:00 must not
// make yesterday's calendar say somebody else was on duty at 09:00.
func TestCancelOverrideInForceKeepsTheHoursAlreadyServed(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(-time.Hour)
	to := f.clock.at.Add(3 * time.Hour)
	created := f.createOverride(t, "bob", from, to)

	f.clock.advance(30 * time.Minute)
	cancelledAt := f.clock.at
	reason := "back early"
	if err := f.svc.CancelOverride(context.Background(), scheduleID, created.OverrideID, 1,
		"alice", &reason); err != nil {
		t.Fatalf("CancelOverride: %v", err)
	}

	head := f.liveHead(t, scheduleID, created.OverrideID)
	if head == nil {
		t.Fatal("the override was removed instead of being ended")
	}
	if !head.ValidFrom.Equal(scheduleconfig.NormalizeTimestamp(from)) {
		t.Errorf("valid_from = %v, want the untouched %v", head.ValidFrom, from)
	}
	if !head.ValidTo.Equal(scheduleconfig.NormalizeTimestamp(cancelledAt)) {
		t.Errorf("valid_to = %v, want the moment of the cancel %v", head.ValidTo, cancelledAt)
	}
	if head.UserID != "bob" {
		t.Errorf("user = %q, want the person who actually covered", head.UserID)
	}
	// The reason is the canceller's, not the creator's, and both are readable:
	// the earlier revision still carries whatever it carried.
	if head.Reason == nil || *head.Reason != reason {
		t.Errorf("reason = %v, want the canceller's own", head.Reason)
	}
}

// TestCancelOverrideThatHasEndedIsRefused: 204 would tell the caller they
// removed a shift, when what they did was rewrite who was on duty.
func TestCancelOverrideThatHasEndedIsRefused(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(-3 * time.Hour)
	to := f.clock.at.Add(-time.Hour)
	created := f.createOverride(t, "bob", from, to)

	err := f.svc.CancelOverride(context.Background(), scheduleID, created.OverrideID, 1, "alice", nil)
	var ended *scheduleconfig.OverrideAlreadyEndedError
	if !errors.As(err, &ended) {
		t.Fatalf("error = %v, want OverrideAlreadyEndedError", err)
	}
	if len(f.repo.OverrideRevisions(scheduleID)) != 1 {
		t.Fatal("the refusal wrote a revision")
	}
}

// TestCancelExactlyAtTheStartRemovesIt: the boundary is <=, not <, and not out
// of taste - truncating to valid_to == valid_from would violate the CHECK on
// the table, and it means the same thing anyway: nobody stood in for anybody.
func TestCancelExactlyAtTheStartRemovesIt(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(time.Hour)
	created := f.createOverride(t, "bob", from, from.Add(2*time.Hour))

	f.clock.advance(time.Hour) // now == valid_from
	if err := f.svc.CancelOverride(context.Background(), scheduleID, created.OverrideID, 1, "alice", nil); err != nil {
		t.Fatalf("CancelOverride: %v", err)
	}
	if head := f.liveHead(t, scheduleID, created.OverrideID); head != nil {
		t.Fatalf("cancelling at the exact start left it live: %+v", head)
	}
}

// TestUpdateOverrideInForceSplitsIt: revision N+1 wins the whole projection, so
// replacing an override that is running would rewrite the hours already served.
// The served part is closed where it stops, and the change becomes its own
// override from now.
func TestUpdateOverrideInForceSplitsIt(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(-time.Hour)
	to := f.clock.at.Add(3 * time.Hour)
	created := f.createOverride(t, "bob", from, to)

	f.clock.advance(time.Hour)
	splitAt := f.clock.at
	updated, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			UserID: "carol", ValidFrom: from, ValidTo: to, ActorID: "alice",
		})
	if err != nil {
		t.Fatalf("UpdateOverride: %v", err)
	}

	if updated.OverrideID == created.OverrideID {
		t.Fatal("the change kept the same override id, so it replaced the served hours")
	}
	if updated.Revision != 1 {
		t.Errorf("the new override starts at revision %d, want 1", updated.Revision)
	}
	if !updated.ValidFrom.Equal(splitAt) || !updated.ValidTo.Equal(scheduleconfig.NormalizeTimestamp(to)) {
		t.Errorf("new override = %v..%v, want %v..%v", updated.ValidFrom, updated.ValidTo, splitAt, to)
	}
	if updated.UserID != "carol" {
		t.Errorf("new override is held by %q, want carol", updated.UserID)
	}

	old := f.liveHead(t, scheduleID, created.OverrideID)
	if old == nil {
		t.Fatal("the served part was removed")
	}
	if old.UserID != "bob" {
		t.Errorf("the served part is held by %q, want the person who served it", old.UserID)
	}
	if !old.ValidTo.Equal(splitAt) {
		t.Errorf("the served part ends at %v, want the split %v", old.ValidTo, splitAt)
	}
	// Half-open, so the two do not overlap and the existing non-overlap check
	// passes without an exception for them.
	if old.ValidTo.After(updated.ValidFrom) {
		t.Error("the two halves overlap")
	}
}

// A future override has served nothing, so one revision may replace another -
// including moving it into the past, which records a fact rather than erasing
// one.
func TestUpdateOverrideBeforeItStartsReplacesIt(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(24 * time.Hour)
	created := f.createOverride(t, "bob", from, from.Add(2*time.Hour))

	updated, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			UserID: "carol", ValidFrom: from, ValidTo: from.Add(4 * time.Hour), ActorID: "alice",
		})
	if err != nil {
		t.Fatalf("UpdateOverride: %v", err)
	}
	if updated.OverrideID != created.OverrideID || updated.Revision != 2 {
		t.Fatalf("update produced %+v, want revision 2 of the same override", updated)
	}
}

// TestUpdateOverrideThatHasEndedIsRefused: same rule as cancel, same reason.
func TestUpdateOverrideThatHasEndedIsRefused(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(-3 * time.Hour)
	created := f.createOverride(t, "bob", from, f.clock.at.Add(-time.Hour))

	_, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			UserID: "carol", ValidFrom: from, ValidTo: f.clock.at.Add(time.Hour), ActorID: "alice",
		})
	if !errors.Is(err, scheduleconfig.ErrOverrideAlreadyEnded) {
		t.Fatalf("error = %v, want ErrOverrideAlreadyEnded", err)
	}
}

// TestUpdateOverrideInForceIgnoresASubmittedValidFrom: the hours already served
// are not editable, so the new segment starts now whatever the caller sent.
func TestUpdateOverrideInForceIgnoresASubmittedValidFrom(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(-time.Hour)
	to := f.clock.at.Add(3 * time.Hour)
	created := f.createOverride(t, "bob", from, to)

	f.clock.advance(time.Hour)
	splitAt := f.clock.at
	updated, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			// Two hours earlier than the split, i.e. into hours bob served.
			UserID: "carol", ValidFrom: f.clock.at.Add(-2 * time.Hour), ValidTo: to, ActorID: "alice",
		})
	if err != nil {
		t.Fatalf("UpdateOverride: %v", err)
	}
	if !updated.ValidFrom.Equal(splitAt) {
		t.Fatalf("new override starts at %v, want the split %v - the served hours are not editable",
			updated.ValidFrom, splitAt)
	}
}

// TestUpdateOverrideInForceRefusesToEndInThePast: shortening to now is what
// cancel does; shortening below it would erase duty somebody served.
func TestUpdateOverrideInForceRefusesToEndInThePast(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	from := f.clock.at.Add(-time.Hour)
	created := f.createOverride(t, "bob", from, f.clock.at.Add(3*time.Hour))

	f.clock.advance(time.Hour)
	for _, validTo := range []time.Time{f.clock.at, f.clock.at.Add(-30 * time.Minute)} {
		_, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
			scheduleconfig.OverrideCommand{
				UserID: "carol", ValidFrom: from, ValidTo: validTo, ActorID: "alice",
			})
		if !errors.Is(err, scheduleconfig.ErrOverrideEndsInThePast) {
			t.Fatalf("valid_to %v: error = %v, want ErrOverrideEndsInThePast", validTo, err)
		}
	}
}

// TestSplitDoesNotAttributeSomebodyElsesReason: every revision's reason and
// recorded_by belong to the same person, or the audit trail says one person
// wrote words another person actually wrote.
//
// The split writes two revisions in one act, and both carry the editor's
// reason - including the truncation, which is the one that used to inherit.
func TestSplitDoesNotAttributeSomebodyElsesReason(t *testing.T) {
	f := overrideFixture(t)
	scheduleID := f.scheduleID(t)

	alicesReason := "alice is at the dentist"
	from := f.clock.at.Add(-time.Hour)
	created, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID: "bob", ValidFrom: from, ValidTo: f.clock.at.Add(3 * time.Hour),
		Reason: &alicesReason, ActorID: "alice",
	})
	if err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	f.clock.advance(time.Hour)
	carolsReason := "carol takes it from here"
	updated, err := f.svc.UpdateOverride(context.Background(), scheduleID, created.OverrideID, 1,
		scheduleconfig.OverrideCommand{
			UserID: "carol", ValidFrom: from, ValidTo: f.clock.at.Add(2 * time.Hour),
			Reason: &carolsReason, ActorID: "bob",
		})
	if err != nil {
		t.Fatalf("UpdateOverride: %v", err)
	}

	truncated := f.liveHead(t, scheduleID, created.OverrideID)
	if truncated == nil {
		t.Fatal("the served part was removed")
	}
	if truncated.Reason == nil || *truncated.Reason != carolsReason {
		t.Errorf("the truncation carries %v under recorded_by %v; both must be the editor's",
			truncated.Reason, truncated.RecordedBy)
	}
	if updated.Reason == nil || *updated.Reason != carolsReason {
		t.Errorf("the new segment carries %v, want the editor's reason", updated.Reason)
	}

	// And alice's words are not lost - they are on the revision she wrote.
	history := f.repo.OverrideRevisions(scheduleID)
	if history[0].Reason == nil || *history[0].Reason != alicesReason {
		t.Errorf("revision 1 no longer carries the reason its author wrote: %v", history[0].Reason)
	}
}

// The same rule where the schedule itself is deleted: the deleter's reason
// goes on the revisions the deletion writes.
func TestDeletingAScheduleDoesNotInheritOverrideReasons(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
		ActorID: "alice",
	})
	scheduleID := created.Revision.ScheduleID

	theirs := "covering the on-call week"
	running, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID: "bob", ValidFrom: f.clock.at.Add(-time.Hour), ValidTo: f.clock.at.Add(3 * time.Hour),
		Reason: &theirs, ActorID: "alice",
	})
	if err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	f.clock.advance(time.Hour)
	mine := "team disbanded"
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{
		ExpectedVersion: 1, ActorID: "bob", Reason: &mine,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	head := f.liveHead(t, scheduleID, running.OverrideID)
	if head == nil {
		t.Fatal("the override in force was erased instead of ended")
	}
	if head.Reason == nil || *head.Reason != mine {
		t.Errorf("the ending revision carries %v under recorded_by %v; both must be the deleter's",
			head.Reason, head.RecordedBy)
	}
}
