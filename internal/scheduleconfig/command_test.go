package scheduleconfig_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
)

const (
	groupCarol = "6c0a1e2c-1111-4a3b-8c4d-000000000003"
	groupDave  = "6c0a1e2c-1111-4a3b-8c4d-000000000004"
)

// movingClock hands out a time a test can advance between calls, which is what
// makes "captured after the lock" observable at all.
type movingClock struct{ at time.Time }

func (c *movingClock) now() time.Time { return c.at }

func (c *movingClock) advance(d time.Duration) { c.at = c.at.Add(d) }

type commandFixture struct {
	svc   *scheduleconfig.Service
	repo  *fakes.ScheduleConfigRepo
	clock *movingClock
}

// newFixture wires a service over the fake with a movable clock, deterministic
// IDs and a team whose members are every user these tests name.
func newFixture(t *testing.T, opts ...scheduleconfig.Option) *commandFixture {
	t.Helper()
	repo := fakes.NewScheduleConfigRepo()
	repo.SetTeamMembers("devops", "alice", "bob", "carol", "dave")

	clock := &movingClock{at: time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)}
	n := 0
	base := []scheduleconfig.Option{
		scheduleconfig.WithClock(clock.now),
		scheduleconfig.WithIDSource(func() string {
			n++
			return fmt.Sprintf("id-%d", n)
		}),
		scheduleconfig.WithLogger(func(string, ...any) {}),
	}
	return &commandFixture{
		svc:   scheduleconfig.NewService(repo, append(base, opts...)...),
		repo:  repo,
		clock: clock,
	}
}

func groupsConfig(groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	cfg := validConfig()
	cfg.L1.Groups = groups
	return cfg
}

func group(id string, members ...string) rotation.RotationGroup {
	return rotation.RotationGroup{ID: id, Members: members}
}

// mustSave fails the test if the save does not go through.
func (f *commandFixture) mustSave(t *testing.T, cmd scheduleconfig.SaveCommand) *scheduleconfig.SaveResult {
	t.Helper()
	res, err := f.svc.Save(context.Background(), "devops", cmd)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return res
}

func (f *commandFixture) scheduleID(t *testing.T) string {
	t.Helper()
	root, ok := f.repo.RootByTeam("devops")
	if !ok {
		t.Fatal("team owns no schedule")
	}
	return root.ID
}

// callsBetween returns the recorded calls, so a test can assert on order.
func (f *commandFixture) calls() []string { return f.repo.Calls }

func indexOf(calls []string, name string) int {
	for i, call := range calls {
		if call == name {
			return i
		}
	}
	return -1
}

func TestSaveCreatesOneRevisionAtomically(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
		ActorID: "alice",
	})

	f.clock.advance(time.Hour)
	reason := "swap the order"
	saved := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: created.Version,
		Desired:         groupsConfig(group(groupBob, "bob"), group(groupAlice, "alice")),
		ActorID:         "bob",
		Reason:          &reason,
	})

	scheduleID := f.scheduleID(t)
	revisions := f.repo.Revisions(scheduleID)
	if len(revisions) != 2 {
		t.Fatalf("got %d revisions, want 2", len(revisions))
	}
	if revisions[0].EffectiveTo == nil {
		t.Fatal("the previous revision was not closed")
	}
	// End to end, no gap: history is a chain, not a set of intervals that
	// happen to sit near each other.
	if !revisions[0].EffectiveTo.Equal(revisions[1].EffectiveFrom) {
		t.Fatalf("revision boundary %v does not meet %v",
			*revisions[0].EffectiveTo, revisions[1].EffectiveFrom)
	}
	if revisions[1].EffectiveTo != nil {
		t.Fatal("the new revision must be the open-ended tail")
	}
	if revisions[1].CreatedBy == nil || *revisions[1].CreatedBy != "bob" {
		t.Fatalf("created_by = %v, want bob", revisions[1].CreatedBy)
	}
	if revisions[1].ChangeReason == nil || *revisions[1].ChangeReason != reason {
		t.Fatalf("change_reason = %v, want %q", revisions[1].ChangeReason, reason)
	}

	root, _ := f.repo.RootByTeam("devops")
	if root.ConfigVersion != 2 || saved.Version != 2 {
		t.Fatalf("config version = %d (result %d), want 2", root.ConfigVersion, saved.Version)
	}

	events := f.repo.Events(scheduleID)
	if len(events) != 2 {
		t.Fatalf("got %d events, want one per revision", len(events))
	}
	var payload scheduleconfig.ConfigurationChangedPayload
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.Trigger != scheduleconfig.TriggerSaved {
		t.Fatalf("trigger = %q, want %q", payload.Trigger, scheduleconfig.TriggerSaved)
	}
	if payload.OldRevisionID == nil || *payload.OldRevisionID != revisions[0].ID {
		t.Fatalf("old_revision_id = %v, want %s", payload.OldRevisionID, revisions[0].ID)
	}
	if payload.NewRevisionID != revisions[1].ID || payload.NewVersion != 2 || payload.OldVersion != 1 {
		t.Fatalf("event payload does not describe the transition: %+v", payload)
	}
	if payload.ActorID == nil || *payload.ActorID != "bob" {
		t.Fatalf("actor_id = %v, want bob", payload.ActorID)
	}
}

func TestCreateWritesEventWithNullOldRevision(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice")),
		ActorID: "alice",
	})

	events := f.repo.Events(created.Revision.ScheduleID)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	var payload scheduleconfig.ConfigurationChangedPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if payload.Trigger != scheduleconfig.TriggerCreated {
		t.Fatalf("trigger = %q, want %q", payload.Trigger, scheduleconfig.TriggerCreated)
	}
	// There is no revision before the first one, and saying so with a null is
	// what lets a consumer tell a create from an edit without a second field.
	if payload.OldRevisionID != nil {
		t.Fatalf("old_revision_id = %v, want null on a create", *payload.OldRevisionID)
	}
	if payload.OldVersion != 0 {
		t.Fatalf("old_version = %d, want 0", payload.OldVersion)
	}
}

// A no-op is an early return from a transaction that commits, not a rollback:
// nothing happened that needs undoing, and a rollback would read as a failure
// in every log and metric that watches this path.
func TestNoopCommitsWithoutWrites(t *testing.T) {
	f := newFixture(t)
	cfg := groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob"))
	created := f.mustSave(t, scheduleconfig.SaveCommand{Desired: cfg, ActorID: "alice"})

	before := len(f.calls())
	f.clock.advance(time.Hour)
	res := f.mustSave(t, scheduleconfig.SaveCommand{ExpectedVersion: 1, Desired: cfg, ActorID: "alice"})

	if !res.Noop {
		t.Fatal("re-saving an identical configuration must be a no-op")
	}
	for _, call := range f.calls()[before:] {
		switch call {
		case "CloseRevision", "InsertRevision", "AdvanceVersion", "InsertScheduleEvent", "SetScheduleDeleted":
			t.Fatalf("no-op performed a write: %s", call)
		}
	}
	if indexOf(f.calls()[before:], "Commit") < 0 {
		t.Fatal("no-op must commit its empty transaction, not roll back")
	}

	root, _ := f.repo.RootByTeam("devops")
	if root.ConfigVersion != 1 {
		t.Fatalf("no-op changed the config version to %d", root.ConfigVersion)
	}
	if got := len(f.repo.Revisions(created.Revision.ScheduleID)); got != 1 {
		t.Fatalf("no-op wrote a revision: %d total", got)
	}
}

// The response to a no-op has to be answerable without a new revision: the
// editor asks "what version am I on now" the same way either way.
func TestNoopResultCarriesCurrentTailAndVersion(t *testing.T) {
	f := newFixture(t)
	cfg := groupsConfig(group(groupAlice, "alice"))
	created := f.mustSave(t, scheduleconfig.SaveCommand{Desired: cfg, ActorID: "alice"})

	f.clock.advance(time.Hour)
	res := f.mustSave(t, scheduleconfig.SaveCommand{ExpectedVersion: 1, Desired: cfg})

	if res.Revision == nil || res.Revision.ID != created.Revision.ID {
		t.Fatalf("no-op must report the tail in force, got %+v", res.Revision)
	}
	if res.Version != created.Version {
		t.Fatalf("no-op version = %d, want %d", res.Version, created.Version)
	}
	if res.EffectiveAt.IsZero() {
		t.Fatal("no-op must report the instant it was evaluated at")
	}
}

func TestVersionMismatchIsConflict(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 7,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})
	if !errors.Is(err, scheduleconfig.ErrVersionConflict) {
		t.Fatalf("error = %v, want a version conflict", err)
	}
	var conflict *scheduleconfig.VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error %v does not carry the versions", err)
	}
	if conflict.Expected != 7 || conflict.Current != 1 {
		t.Fatalf("conflict = %+v, want expected 7 current 1", conflict)
	}
	if got := len(f.repo.Revisions(f.scheduleID(t))); got != 1 {
		t.Fatalf("a rejected save wrote %d revisions", got)
	}
}

// An instant captured before the lock is an instant from before the wait, and
// applying a change at it places the transition in a slot that has passed.
func TestEffectiveAtCapturedAfterLock(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	// Stand in for the wait: the clock moves while the command runs.
	before := f.clock.at
	f.clock.advance(2 * time.Hour)

	res := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})
	if !res.Revision.EffectiveFrom.Equal(f.clock.at) {
		t.Fatalf("effective_from = %v, want the post-lock instant %v",
			res.Revision.EffectiveFrom, f.clock.at)
	}
	if !res.Revision.EffectiveFrom.After(before) {
		t.Fatal("effective_from was captured before the lock was taken")
	}

	calls := f.calls()
	lockUsers := indexOf(calls, "LockUsers")
	lock := indexOf(calls, "LockSchedule")
	if lockUsers < 0 || lock < 0 || lockUsers > lock {
		t.Fatalf("users must be locked before the schedule, calls: %v", calls)
	}
	// And the set locked is the one the configuration names: locking fewer
	// leaves a gap erasure can commit through.
	last := f.repo.LockedUsers[len(f.repo.LockedUsers)-1]
	if len(last) != 1 || last[0] != "bob" {
		t.Fatalf("locked users = %v, want the users the save names", last)
	}
}

// Only the monotonicity floor may put a revision ahead of the clock, and only
// by one resolution unit.
func TestNoFutureRevision(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	// The clock does not move: two saves in the same instant.
	res := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})
	floor := created.Revision.EffectiveFrom.Add(scheduleconfig.TimestampResolution)
	if !res.Revision.EffectiveFrom.Equal(floor) {
		t.Fatalf("effective_from = %v, want exactly the floor %v", res.Revision.EffectiveFrom, floor)
	}

	// A clock stepping backwards must not produce an inverted interval either.
	f.clock.at = f.clock.at.Add(-time.Hour)
	third := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 2,
		Desired:         groupsConfig(group(groupCarol, "carol")),
	})
	if !third.Revision.EffectiveFrom.After(res.Revision.EffectiveFrom) {
		t.Fatalf("a backwards clock produced %v after %v",
			third.Revision.EffectiveFrom, res.Revision.EffectiveFrom)
	}
}

func TestInsertFailureRollsBackClose(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	boom := errors.New("injected failure")
	f.repo.FailOn["InsertRevision"] = boom
	f.clock.advance(time.Hour)

	_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	revisions := f.repo.Revisions(created.Revision.ScheduleID)
	if len(revisions) != 1 {
		t.Fatalf("got %d revisions after rollback, want 1", len(revisions))
	}
	if revisions[0].EffectiveTo != nil {
		t.Fatalf("the tail stayed closed at %v after rollback", *revisions[0].EffectiveTo)
	}
}

// The revision, the version and the event are one fact. A failure to record
// the event must not leave a committed change nobody was told about.
func TestEventFailureRollsBackEverything(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	boom := errors.New("injected failure")
	f.repo.FailOn["InsertScheduleEvent"] = boom
	f.clock.advance(time.Hour)

	_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the injected failure", err)
	}
	if got := len(f.repo.Revisions(created.Revision.ScheduleID)); got != 1 {
		t.Fatalf("got %d revisions after rollback, want 1", got)
	}
	root, _ := f.repo.RootByTeam("devops")
	if root.ConfigVersion != 1 {
		t.Fatalf("config version advanced to %d despite the rollback", root.ConfigVersion)
	}
}

// The guard exists to catch a plan whose snapshot does not produce the
// rotation the plan promised. Only an injected planner can produce one.
func TestGuardCatchesInjectedPlan(t *testing.T) {
	wrong := groupBob
	f := newFixture(t, scheduleconfig.WithPlanner(
		func(in rotation.TransitionInput) (rotation.TransitionPlan, error) {
			plan, err := rotation.PlanTransition(in)
			if err != nil {
				return plan, err
			}
			// The snapshot puts group A on duty; the plan claims group B.
			plan.L1.ExpectedActiveGroupID = &wrong
			return plan, nil
		}))

	_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice")),
	})
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("error = %v, want an invariant violation", err)
	}
	if f.repo.RootCount() != 0 {
		t.Fatal("a guard violation must roll the whole transaction back")
	}
}

// A composite edit - a timezone change that also drops the active group - is
// a valid successor transition, and the guard must not roll it back. Equality
// of the old and the new active group is only required when the plan says it
// preserves one.
func TestCompositeEditTimezonePlusActiveGroupRemoval(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob"), group(groupCarol, "carol")),
	})
	if *created.Revision.Snapshot.L1.StartPosition != 0 {
		t.Fatalf("fixture assumption broken: start position = %d", *created.Revision.Snapshot.L1.StartPosition)
	}

	// Stay inside the same weekly slot, so the group on duty is still the
	// first one - then remove exactly that group AND change the timezone.
	f.clock.advance(10 * time.Minute)
	desired := groupsConfig(group(groupBob, "bob"), group(groupCarol, "carol"))
	desired.Timezone = "Asia/Tokyo"

	res := f.mustSave(t, scheduleconfig.SaveCommand{ExpectedVersion: 1, Desired: desired})
	if res.Noop {
		t.Fatal("a timezone change plus a group removal is not a no-op")
	}
	if res.Revision.Snapshot.Timezone != "Asia/Tokyo" {
		t.Fatalf("timezone = %q, want Asia/Tokyo", res.Revision.Snapshot.Timezone)
	}
	// The group that was on duty is gone, so the plan hands duty to its
	// successor. The guard must accept that: demanding the old and new active
	// group be equal here would roll back a perfectly valid edit.
	if got := res.Revision.ChangeSummary.L1GroupSelection; got != rotation.SelectionSuccessor {
		t.Fatalf("group selection = %q, want successor", got)
	}
	if res.Revision.Snapshot.L1.Groups[*res.Revision.Snapshot.L1.StartPosition].ID != groupBob {
		t.Fatalf("duty did not pass to the successor group, position %d of %v",
			*res.Revision.Snapshot.L1.StartPosition, res.Revision.Snapshot.L1.Groups)
	}
}

// A save that provably cannot move the rotation must copy the phase pair
// verbatim rather than recompute it: recomputation is how a metadata edit ends
// up shifting who is on call.
func TestMetadataOnlySaveCarryPhaseBytesEqual(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
	})

	desired := groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob"))
	desired.SlackUsergroupID = "S-something-else"

	f.clock.advance(37 * time.Hour)
	res := f.mustSave(t, scheduleconfig.SaveCommand{ExpectedVersion: 1, Desired: desired})

	if res.Revision.ChangeSummary.L1PhaseAction != rotation.PhaseActionCarry {
		t.Fatalf("phase action = %q, want carry", res.Revision.ChangeSummary.L1PhaseAction)
	}
	before, err := json.Marshal(created.Revision.Snapshot.L1)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	after, err := json.Marshal(res.Revision.Snapshot.L1)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("a metadata-only save changed the layer:\n%s\n%s", before, after)
	}
}

// The original bug: adding someone to the group that is currently on duty
// must not restart the rotation or skip the group that was next.
func TestBugScenarioABC_ABDC(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(
			group(groupAlice, "alice"),
			group(groupBob, "bob"),
			group(groupCarol, "carol")),
	})
	anchor := *created.Revision.Snapshot.L1.PhaseAnchorSlotStart
	position := *created.Revision.Snapshot.L1.StartPosition

	f.clock.advance(time.Hour)
	res := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired: groupsConfig(
			group(groupAlice, "alice"),
			group(groupBob, "bob", "dave"),
			group(groupCarol, "carol")),
	})

	layer := res.Revision.Snapshot.L1
	if layer.PhaseAnchorSlotStart == nil || !layer.PhaseAnchorSlotStart.Equal(anchor) {
		t.Fatalf("phase anchor moved to %v, want %v", layer.PhaseAnchorSlotStart, anchor)
	}
	if layer.StartPosition == nil || *layer.StartPosition != position {
		t.Fatalf("start position moved to %v, want %d", layer.StartPosition, position)
	}
	if res.Revision.ChangeSummary.L1PhaseAction != rotation.PhaseActionCarry {
		t.Fatalf("phase action = %q, want carry", res.Revision.ChangeSummary.L1PhaseAction)
	}
	// The order of the groups - and therefore who is next - is untouched.
	ids := []string{layer.Groups[0].ID, layer.Groups[1].ID, layer.Groups[2].ID}
	if ids[0] != groupAlice || ids[1] != groupBob || ids[2] != groupCarol {
		t.Fatalf("group order changed: %v", ids)
	}
	if len(layer.Groups[1].Members) != 2 {
		t.Fatalf("dave was not added to the group on duty: %v", layer.Groups[1].Members)
	}
}

func TestMembershipValidationFullRollback(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "mallory")),
	})

	var notMember *scheduleconfig.UserNotTeamMemberError
	if !errors.As(err, &notMember) {
		t.Fatalf("error = %v, want a membership rejection", err)
	}
	if len(notMember.UserIDs) != 1 || notMember.UserIDs[0] != "mallory" {
		t.Fatalf("offenders = %v, want [mallory]", notMember.UserIDs)
	}
	if f.repo.RootCount() != 0 {
		t.Fatal("a rejected save left a schedule behind")
	}
}

// An erased user is not a member, so the next save cannot quietly put them
// back into the rotation.
func TestSoftDeletedUserIsNotAMember(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
	})

	f.repo.EraseUser("bob")
	f.clock.advance(time.Hour)

	_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired:         groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob", "carol")),
	})
	var notMember *scheduleconfig.UserNotTeamMemberError
	if !errors.As(err, &notMember) || notMember.UserIDs[0] != "bob" {
		t.Fatalf("error = %v, want bob rejected as a non-member", err)
	}
}

// The membership list a command locks against is not the list it validates
// against: the authoritative read happens after the schedule lock, because a
// removal serializes on that lock rather than on the user rows.
func TestSaveRevalidatesMembershipAfterLock(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	// The create branch has no row to lock, so the ordering is asserted on the
	// edit below, with the call log reset to just that command.
	f.clock.advance(time.Hour)
	f.repo.Calls = nil
	f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 1,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})

	calls := f.calls()
	lockIdx := indexOf(calls, "LockSchedule")
	memberIdx := indexOf(calls, "GetTeamMemberIDs")
	if lockIdx < 0 || memberIdx < 0 {
		t.Fatalf("expected both a schedule lock and a membership read, got %v", calls)
	}
	if memberIdx < lockIdx {
		t.Fatalf("membership was validated before the schedule lock: %v", calls)
	}
}

// Erasure promises that everything an erased person wrote is gone. A command
// that committed after their erasure would leave a change reason behind that
// nothing will ever clean up again, so the actor is locked and checked like
// any other user the command names.
func TestCommandsRefuseAnErasedActor(t *testing.T) {
	reason := "left a note"

	t.Run("save", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{
			Desired: groupsConfig(group(groupAlice, "alice")),
			ActorID: "bob",
		})
		f.repo.EraseUser("bob")
		f.clock.advance(time.Hour)

		_, err := f.svc.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
			ExpectedVersion: 1,
			Desired:         groupsConfig(group(groupCarol, "carol")),
			ActorID:         "bob",
			Reason:          &reason,
		})
		if !errors.Is(err, scheduleconfig.ErrActorNotActive) {
			t.Fatalf("error = %v, want ErrActorNotActive", err)
		}
		if got := len(f.repo.Revisions(f.scheduleID(t))); got != 1 {
			t.Fatalf("a refused save wrote %d revisions", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{
			Desired: groupsConfig(group(groupAlice, "alice")),
			ActorID: "alice",
		})
		f.repo.EraseUser("bob")
		f.clock.advance(time.Hour)

		err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{
			ExpectedVersion: 1, ActorID: "bob", Reason: &reason,
		})
		if !errors.Is(err, scheduleconfig.ErrActorNotActive) {
			t.Fatalf("error = %v, want ErrActorNotActive", err)
		}
	})

	t.Run("override", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{
			Desired: groupsConfig(group(groupAlice, "alice")),
			ActorID: "alice",
		})
		f.repo.EraseUser("bob")

		_, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
			UserID:    "carol",
			ValidFrom: f.clock.at.Add(24 * time.Hour),
			ValidTo:   f.clock.at.Add(48 * time.Hour),
			Reason:    &reason,
			ActorID:   "bob",
		})
		if !errors.Is(err, scheduleconfig.ErrActorNotActive) {
			t.Fatalf("error = %v, want ErrActorNotActive", err)
		}
	})

	// The actor is locked along with everyone the command names, so an erasure
	// running beside it can only land on one side of the command.
	t.Run("the actor is in the lock set", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{
			Desired: groupsConfig(group(groupAlice, "alice")),
			ActorID: "dave",
		})
		locked := f.repo.LockedUsers[0]
		var sawActor bool
		for _, id := range locked {
			if id == "dave" {
				sawActor = true
			}
		}
		if !sawActor {
			t.Fatalf("locked users = %v, want the actor among them", locked)
		}
	})
}

// TestWriteOnRootWithoutHistoryIsInvariantViolation: a schedule row with no
// history horizon cannot be produced by any live path, so every command refuses
// it as the corruption it is instead of adopting it at revision 1.
//
// Every write path is listed on purpose. The override commands are the reason
// this test exists at all: they never read the revision chain, so without a
// guard on the locked row they would be the one place a write SUCCEEDED,
// appending an override revision to a schedule that has none.
func TestWriteOnRootWithoutHistoryIsInvariantViolation(t *testing.T) {
	ctx := context.Background()
	validFrom := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	writes := []struct {
		name string
		call func(f *commandFixture) error
	}{
		{"save", func(f *commandFixture) error {
			_, err := f.svc.Save(ctx, "devops", scheduleconfig.SaveCommand{
				Desired: groupsConfig(group(groupAlice, "alice")),
			})
			return err
		}},
		{"delete", func(f *commandFixture) error {
			return f.svc.Delete(ctx, "devops", scheduleconfig.DeleteCommand{})
		}},
		{"create override", func(f *commandFixture) error {
			_, err := f.svc.CreateOverride(ctx, "devops", scheduleconfig.OverrideCommand{
				UserID: "alice", ValidFrom: validFrom, ValidTo: validFrom.Add(time.Hour),
			})
			return err
		}},
		{"update override", func(f *commandFixture) error {
			_, err := f.svc.UpdateOverride(ctx, "legacy-1", "ovr-1", 1, scheduleconfig.OverrideCommand{
				UserID: "alice", ValidFrom: validFrom, ValidTo: validFrom.Add(time.Hour),
			})
			return err
		}},
		{"delete override", func(f *commandFixture) error {
			return f.svc.DeleteOverride(ctx, "legacy-1", "ovr-1", 1, "")
		}},
		{"remove team member", func(f *commandFixture) error {
			return f.svc.RemoveTeamMember(ctx, "devops", "bob")
		}},
	}

	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			f := newFixture(t)
			f.repo.SeedRootWithoutHistory("legacy-1", "devops")

			err := w.call(f)
			if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
			if got := len(f.repo.Revisions("legacy-1")); got != 0 {
				t.Fatalf("revisions were grafted onto an uninitialized root: %d", got)
			}
			if got := len(f.repo.OverrideRevisions("legacy-1")); got != 0 {
				t.Fatalf("override revisions were written to a schedule with no chain: %d", got)
			}
		})
	}
}

func TestDeleteInsertsDeletedRevision(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
	})
	scheduleID := created.Revision.ScheduleID

	f.clock.advance(time.Hour)
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{
		ExpectedVersion: 1,
		ActorID:         "alice",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	revisions := f.repo.Revisions(scheduleID)
	if len(revisions) != 2 {
		t.Fatalf("got %d revisions, want 2", len(revisions))
	}
	deleted := revisions[1]
	if deleted.Kind != scheduleconfig.RevisionDeleted {
		t.Fatalf("tail kind = %q, want %q", deleted.Kind, scheduleconfig.RevisionDeleted)
	}
	// The snapshot is copied so the column stays decodable, not so it can be
	// read as a configuration in force.
	if len(deleted.Snapshot.L1.Groups) != 2 {
		t.Fatalf("deleted revision must carry the last valid snapshot, got %+v", deleted.Snapshot.L1)
	}
	if deleted.Version != 2 {
		t.Fatalf("deleted revision version = %d, want 2", deleted.Version)
	}
	root, _ := f.repo.RootByTeam("devops")
	if root.DeletedAt == nil || !root.DeletedAt.Equal(deleted.EffectiveFrom) {
		t.Fatalf("deleted_at = %v, want %v", root.DeletedAt, deleted.EffectiveFrom)
	}
	if root.ConfigVersion != 2 {
		t.Fatalf("config version = %d, want 2", root.ConfigVersion)
	}

	events := f.repo.Events(scheduleID)
	var payload scheduleconfig.ConfigurationChangedPayload
	if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if payload.Trigger != scheduleconfig.TriggerDeleted {
		t.Fatalf("trigger = %q, want %q", payload.Trigger, scheduleconfig.TriggerDeleted)
	}
}

func TestDeleteOnDeletedConflicts(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	f.clock.advance(time.Hour)
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{ExpectedVersion: 1}); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	f.clock.advance(time.Hour)
	err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{ExpectedVersion: 2})
	if !errors.Is(err, scheduleconfig.ErrScheduleDeleted) {
		t.Fatalf("error = %v, want ErrScheduleDeleted", err)
	}
}

// Without this, an override whose valid_to has not arrived yet survives the
// delete and takes over the rotation again after a recreate.
func TestDeleteTombstonesLiveOverrides(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})
	scheduleID := created.Revision.ScheduleID

	future, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID:    "bob",
		ValidFrom: f.clock.at.Add(48 * time.Hour),
		ValidTo:   f.clock.at.Add(72 * time.Hour),
		ActorID:   "alice",
	})
	if err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	f.clock.advance(time.Hour)
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{
		ExpectedVersion: 1,
		ActorID:         "alice",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	revisions := f.repo.OverrideRevisions(scheduleID)
	if len(revisions) != 2 {
		t.Fatalf("got %d override revisions, want the original plus a tombstone", len(revisions))
	}
	tombstone := revisions[1]
	if !tombstone.Deleted || tombstone.Revision != future.Revision+1 {
		t.Fatalf("tombstone = %+v", tombstone)
	}
	if tombstone.OverrideID != future.OverrideID {
		t.Fatalf("tombstone belongs to %s, want %s", tombstone.OverrideID, future.OverrideID)
	}
	// The history survives whole: the override was live until the delete.
	if !revisions[0].Deleted == false {
		t.Fatal("the original override revision was modified")
	}
}

func TestSaveOnDeletedRecreatesInSameChain(t *testing.T) {
	f := newFixture(t)
	created := f.mustSave(t, scheduleconfig.SaveCommand{
		Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
	})
	scheduleID := created.Revision.ScheduleID

	f.clock.advance(time.Hour)
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{ExpectedVersion: 1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	f.clock.advance(time.Hour)
	res := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 2,
		Desired:         groupsConfig(group(groupBob, "bob"), group(groupAlice, "alice")),
	})

	if !res.Recreated {
		t.Fatal("a save on a deleted schedule is a recreate")
	}
	if res.Version != 3 {
		t.Fatalf("version = %d, want the chain to continue at 3", res.Version)
	}
	root, _ := f.repo.RootByTeam("devops")
	if root.DeletedAt != nil {
		t.Fatalf("deleted_at survived the recreate: %v", *root.DeletedAt)
	}
	// Current: nil - the rotation restarts rather than resuming from a
	// configuration that was not in force.
	if res.Revision.ChangeSummary.L1GroupSelection != rotation.SelectionFirst {
		t.Fatalf("group selection = %q, want first", res.Revision.ChangeSummary.L1GroupSelection)
	}

	revisions := f.repo.Revisions(scheduleID)
	if len(revisions) != 3 {
		t.Fatalf("got %d revisions, want an unbroken chain of 3", len(revisions))
	}
	for i := 1; i < len(revisions); i++ {
		if revisions[i-1].EffectiveTo == nil || !revisions[i-1].EffectiveTo.Equal(revisions[i].EffectiveFrom) {
			t.Fatalf("chain is broken between version %d and %d", revisions[i-1].Version, revisions[i].Version)
		}
	}
}

// Recreating in the same instant as the delete must still produce a non-empty
// interval, or the interval check rejects an otherwise valid operation.
func TestRecreateImmediatelyAfterDelete(t *testing.T) {
	f := newFixture(t)
	f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

	f.clock.advance(time.Hour)
	if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{ExpectedVersion: 1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// No clock movement at all between the delete and the recreate.
	res := f.mustSave(t, scheduleconfig.SaveCommand{
		ExpectedVersion: 2,
		Desired:         groupsConfig(group(groupBob, "bob")),
	})

	revisions := f.repo.Revisions(res.Revision.ScheduleID)
	deleted := revisions[1]
	if deleted.EffectiveTo == nil {
		t.Fatal("the deleted revision was not closed")
	}
	if !deleted.EffectiveTo.After(deleted.EffectiveFrom) {
		t.Fatalf("deleted interval %v..%v is empty or inverted",
			deleted.EffectiveFrom, *deleted.EffectiveTo)
	}
}

func TestRemoveTeamMemberGuard(t *testing.T) {
	t.Run("blocked while in the tail revision", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{
			Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
		})

		err := f.svc.RemoveTeamMember(context.Background(), "devops", "bob")
		var onCall *scheduleconfig.MemberOnCallError
		if !errors.As(err, &onCall) {
			t.Fatalf("error = %v, want a member-on-call refusal", err)
		}
		if len(onCall.Schedules) != 1 || onCall.Schedules[0].TeamID != "devops" {
			t.Fatalf("refusal must name the blocking schedule, got %+v", onCall.Schedules)
		}
		if got := f.repo.TeamMembers("devops"); len(got) != 4 {
			t.Fatalf("membership changed despite the refusal: %v", got)
		}
	})

	t.Run("blocked by a live override", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})
		if _, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
			UserID:    "carol",
			ValidFrom: f.clock.at.Add(24 * time.Hour),
			ValidTo:   f.clock.at.Add(48 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateOverride: %v", err)
		}

		// carol is in no rotation group at all - only the override names her.
		if err := f.svc.RemoveTeamMember(context.Background(), "devops", "carol"); !errors.Is(err, scheduleconfig.ErrMemberOnCall) {
			t.Fatalf("error = %v, want a member-on-call refusal", err)
		}
	})

	t.Run("allowed when free", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})

		if err := f.svc.RemoveTeamMember(context.Background(), "devops", "dave"); err != nil {
			t.Fatalf("RemoveTeamMember: %v", err)
		}
		for _, id := range f.repo.TeamMembers("devops") {
			if id == "dave" {
				t.Fatal("dave was not removed")
			}
		}
	})

	t.Run("allowed when the override has expired", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{Desired: groupsConfig(group(groupAlice, "alice"))})
		if _, err := f.svc.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
			UserID:    "carol",
			ValidFrom: f.clock.at.Add(time.Hour),
			ValidTo:   f.clock.at.Add(2 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateOverride: %v", err)
		}
		f.clock.advance(3 * time.Hour)

		if err := f.svc.RemoveTeamMember(context.Background(), "devops", "carol"); err != nil {
			t.Fatalf("an expired override must not block a removal: %v", err)
		}
	})

	// A team with no schedule at all has no assignment to protect. A root
	// without history is NOT this case and must not be quietly let through -
	// that is covered by TestWriteOnRootWithoutHistoryIsInvariantViolation.
	t.Run("absent schedule skips the guard", func(t *testing.T) {
		f := newFixture(t)
		if err := f.svc.RemoveTeamMember(context.Background(), "devops", "bob"); err != nil {
			t.Fatalf("no schedule at all: %v", err)
		}
	})

	t.Run("deleted schedule blocks nothing", func(t *testing.T) {
		f := newFixture(t)
		f.mustSave(t, scheduleconfig.SaveCommand{
			Desired: groupsConfig(group(groupAlice, "alice"), group(groupBob, "bob")),
		})
		f.clock.advance(time.Hour)
		if err := f.svc.Delete(context.Background(), "devops", scheduleconfig.DeleteCommand{ExpectedVersion: 1}); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if err := f.svc.RemoveTeamMember(context.Background(), "devops", "bob"); err != nil {
			t.Fatalf("a deleted schedule must not block a removal forever: %v", err)
		}
	})
}
