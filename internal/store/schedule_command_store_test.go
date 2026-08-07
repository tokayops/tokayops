package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// cmdConfig is a valid configuration over the named members, one group each.
func cmdConfig(members ...string) rotation.ScheduleConfiguration {
	cfg := revTestConfig()
	groups := make([]rotation.RotationGroup, len(members))
	ids := [...]string{revGroupAlice, revGroupBob}
	for i, m := range members {
		groups[i] = rotation.RotationGroup{ID: ids[i], Members: []string{m}}
	}
	cfg.L1.Groups = groups
	return cfg
}

// seedCommandSchedule creates a team, its members and a revision-model
// schedule, and returns the schedule ID.
func seedCommandSchedule(t *testing.T, s *Store, at time.Time) string {
	t.Helper()
	seedTeam(t, s, "devops", "alice", "bob")
	rev, err := newTestScheduleService(s, at).CreateSchedule(
		context.Background(), "devops", cmdConfig("alice", "bob"), "alice", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return rev.ScheduleID
}

func TestSetScheduleDeletedRoundTrip(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	scheduleID := seedCommandSchedule(t, s, at)
	ctx := context.Background()

	deletedAt := at.Add(time.Hour)
	err := s.ScheduleConfigRepository().WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		if err := tx.SetScheduleDeleted(ctx, scheduleID, &deletedAt); err != nil {
			return err
		}
		root, err := tx.GetScheduleRoot(ctx, scheduleID)
		if err != nil {
			return err
		}
		if root.DeletedAt == nil || !root.DeletedAt.Equal(deletedAt) {
			return fmt.Errorf("deleted_at = %v, want %v", root.DeletedAt, deletedAt)
		}
		// nil clears it: the flag is a projection of the chain, and a recreate
		// has to be able to move it back.
		if err := tx.SetScheduleDeleted(ctx, scheduleID, nil); err != nil {
			return err
		}
		if root, err = tx.GetScheduleRoot(ctx, scheduleID); err != nil {
			return err
		}
		if root.DeletedAt != nil {
			return fmt.Errorf("deleted_at = %v, want it cleared", *root.DeletedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SetScheduleDeleted round trip: %v", err)
	}

	err = s.ScheduleConfigRepository().WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.SetScheduleDeleted(ctx, "no-such-schedule", &deletedAt)
	})
	if !errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
		t.Fatalf("error = %v, want ErrScheduleNotFound", err)
	}
}

func TestMaxOverrideRecordedAt(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	scheduleID := seedCommandSchedule(t, s, at)
	ctx := context.Background()

	err := s.ScheduleConfigRepository().WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		max, err := tx.MaxOverrideRecordedAt(ctx, scheduleID)
		if err != nil {
			return err
		}
		if max != nil {
			return fmt.Errorf("max over an empty history = %v, want nil", *max)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("empty history: %v", err)
	}

	// Two logical overrides with different recorded times: the maximum is over
	// the schedule, not over one override.
	first := at.Add(time.Hour)
	second := at.Add(2 * time.Hour)
	err = s.ScheduleConfigRepository().WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		for i, recordedAt := range []time.Time{second, first} {
			if err := tx.InsertOverrideRevision(ctx, &scheduleconfig.OverrideRevision{
				RevisionID: fmt.Sprintf("ovr-rev-%d", i),
				OverrideID: fmt.Sprintf("ovr-%d", i),
				ScheduleID: scheduleID,
				Revision:   1,
				UserID:     "alice",
				ValidFrom:  at.Add(24 * time.Hour),
				ValidTo:    at.Add(25 * time.Hour),
				RecordedAt: recordedAt,
			}); err != nil {
				return err
			}
		}
		max, err := tx.MaxOverrideRecordedAt(ctx, scheduleID)
		if err != nil {
			return err
		}
		if max == nil || !max.Equal(second) {
			return fmt.Errorf("max = %v, want %v", max, second)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("MaxOverrideRecordedAt: %v", err)
	}
}

func TestListRevisionsPaginationAndGetByID(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	scheduleID := seedCommandSchedule(t, s, at)
	ctx := context.Background()

	// Three more revisions, the last of them a deleted period.
	svc := newTestScheduleService(s, at.Add(time.Hour))
	if _, err := svc.Save(ctx, "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 1, Desired: cmdConfig("bob", "alice"), ActorID: "alice",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	svc = newTestScheduleService(s, at.Add(2*time.Hour))
	if _, err := svc.Save(ctx, "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 2, Desired: cmdConfig("alice"), ActorID: "alice",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	svc = newTestScheduleService(s, at.Add(3*time.Hour))
	if err := svc.Delete(ctx, "devops", scheduleconfig.DeleteCommand{
		ExpectedVersion: 3, ActorID: "alice",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	err := s.ScheduleReadRepository().WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		page, err := view.ListRevisions(ctx, scheduleID, 2, nil)
		if err != nil {
			return err
		}
		if len(page) != 2 {
			return fmt.Errorf("page size = %d, want 2", len(page))
		}
		// Newest first: an audit trail is read from the top.
		if page[0].Version != 4 || page[1].Version != 3 {
			return fmt.Errorf("versions = %d, %d; want 4, 3", page[0].Version, page[1].Version)
		}
		// Both kinds are returned: a deleted period is part of the history.
		if page[0].Kind != scheduleconfig.RevisionDeleted {
			return fmt.Errorf("newest revision kind = %q, want deleted", page[0].Kind)
		}

		cursor := page[1].Version
		next, err := view.ListRevisions(ctx, scheduleID, 2, &cursor)
		if err != nil {
			return err
		}
		if len(next) != 2 || next[0].Version != 2 || next[1].Version != 1 {
			return fmt.Errorf("second page = %+v", next)
		}

		one, err := view.GetRevisionByID(ctx, scheduleID, page[0].ID)
		if err != nil {
			return err
		}
		if one.Version != 4 {
			return fmt.Errorf("GetRevisionByID returned version %d", one.Version)
		}
		// Scoped by schedule, so a revision of another schedule is not found
		// even when the ID is right.
		if _, err := view.GetRevisionByID(ctx, "other-schedule", page[0].ID); !errors.Is(err, scheduleconfig.ErrRevisionNotFound) {
			return fmt.Errorf("cross-schedule lookup error = %v, want ErrRevisionNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
}

// The head is the last thing that happened to an override, tombstone included.
// The projection hides tombstones, and an update that could not tell "deleted"
// from "never existed" would restart the numbering and resurrect it.
func TestOverrideHeadsIncludeTombstone(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	scheduleID := seedCommandSchedule(t, s, at)
	ctx := context.Background()

	err := s.ScheduleConfigRepository().WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		for i, deleted := range []bool{false, true} {
			if err := tx.InsertOverrideRevision(ctx, &scheduleconfig.OverrideRevision{
				RevisionID: fmt.Sprintf("gone-rev-%d", i),
				OverrideID: "gone",
				ScheduleID: scheduleID,
				Revision:   int64(i + 1),
				UserID:     "alice",
				ValidFrom:  at.Add(24 * time.Hour),
				ValidTo:    at.Add(25 * time.Hour),
				Deleted:    deleted,
				RecordedAt: at.Add(time.Duration(i) * time.Minute),
			}); err != nil {
				return err
			}
		}
		return tx.InsertOverrideRevision(ctx, &scheduleconfig.OverrideRevision{
			RevisionID: "live-rev-1",
			OverrideID: "live",
			ScheduleID: scheduleID,
			Revision:   1,
			UserID:     "bob",
			ValidFrom:  at.Add(48 * time.Hour),
			ValidTo:    at.Add(49 * time.Hour),
			RecordedAt: at.Add(time.Hour),
		})
	})
	if err != nil {
		t.Fatalf("seed overrides: %v", err)
	}

	err = s.ScheduleReadRepository().WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		head, err := view.GetOverrideHead(ctx, scheduleID, "gone")
		if err != nil {
			return err
		}
		if !head.Deleted || head.Revision != 2 {
			return fmt.Errorf("head of a deleted override = %+v", head)
		}

		editor, err := view.ListOverrideHeads(ctx, scheduleID, false)
		if err != nil {
			return err
		}
		if len(editor) != 1 || editor[0].OverrideID != "live" {
			return fmt.Errorf("editor view = %+v, want only the live override", editor)
		}

		all, err := view.ListOverrideHeads(ctx, scheduleID, true)
		if err != nil {
			return err
		}
		if len(all) != 2 {
			return fmt.Errorf("full head list = %+v, want both", all)
		}

		// The projection still hides it.
		projection, err := view.GetOverrideProjectionInRange(ctx, scheduleID, nil, nil, nil)
		if err != nil {
			return err
		}
		if len(projection) != 1 || projection[0].OverrideID != "live" {
			return fmt.Errorf("projection = %+v", projection)
		}

		if _, err := view.GetOverrideHead(ctx, scheduleID, "never-existed"); !errors.Is(err, scheduleconfig.ErrOverrideNotFound) {
			return fmt.Errorf("missing override error = %v, want ErrOverrideNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("override heads: %v", err)
	}
}

func TestGetTeamMemberIDsExcludesSoftDeleted(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	seedCommandSchedule(t, s, at)
	// carol is a member but not in the rotation, so she can actually be
	// erased: someone on call is refused, which is a different test.
	if err := s.CreateUser(&model.User{ID: "carol", Name: "carol", Email: "carol@example.test"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.AddTeamMember("devops", "carol", model.TeamMemberRoleMember); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}
	ctx := context.Background()

	read := func(t *testing.T) []string {
		t.Helper()
		var ids []string
		if err := s.ScheduleReadRepository().WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
			var err error
			ids, err = view.GetTeamMemberIDs(ctx, "devops")
			return err
		}); err != nil {
			t.Fatalf("GetTeamMemberIDs: %v", err)
		}
		return ids
	}

	if got := read(t); len(got) != 3 {
		t.Fatalf("members = %v, want alice, bob and carol", got)
	}

	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "carol"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	got := read(t)
	if len(got) != 2 {
		t.Fatalf("members after erasure = %v, want alice and bob", got)
	}
	for _, id := range got {
		if id == "carol" {
			t.Fatal("an erased user is still reported as a team member")
		}
	}

	// And the same read inside a command transaction, which is where the save
	// pipeline actually validates membership.
	var inTx []string
	if err := s.ScheduleConfigRepository().WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		var err error
		inTx, err = tx.GetTeamMemberIDs(ctx, "devops")
		return err
	}); err != nil {
		t.Fatalf("GetTeamMemberIDs in a transaction: %v", err)
	}
	if len(inTx) != 2 {
		t.Fatalf("members inside a transaction = %v, want alice and bob", inTx)
	}
}

// The legacy readers must refuse a schedule governed by revisions. Answering
// from its stale mutable columns would not fail - it would page the wrong
// person, which is worse.
func TestLegacyReadersRefuseRevisionSchedules(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	scheduleID := seedCommandSchedule(t, s, at)

	if _, err := s.GetScheduleByTeamID("devops"); !errors.Is(err, ErrScheduleSuperseded) {
		t.Fatalf("GetScheduleByTeamID = %v, want ErrScheduleSuperseded", err)
	}
	if _, err := s.GetScheduleByID(scheduleID); !errors.Is(err, ErrScheduleSuperseded) {
		t.Fatalf("GetScheduleByID = %v, want ErrScheduleSuperseded", err)
	}

	// A row from before the upgrade still reads as it always did.
	seedTeam(t, s, "legacy-team")
	legacy := &model.Schedule{
		ID: "legacy-1", TeamID: "legacy-team", Timezone: "UTC",
		L1RotationType: model.RotationDaily, L1HandoffTime: "09:00", L1RotationStart: at,
	}
	if err := s.CreateSchedule(legacy); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	got, err := s.GetScheduleByTeamID("legacy-team")
	if err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	if got.ID != "legacy-1" || got.ConfigVersion != 0 {
		t.Fatalf("legacy schedule = %+v", got)
	}
}

// Every user mutation carries deleted_at IS NULL and checks it affected a row.
// Without that, an erasure is a stage the next update quietly undoes.
func TestErasedUserCannotBeRefilled(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")
	ctx := context.Background()

	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	if err := s.UpdateUser(&model.User{ID: "bob", Name: "Bob Again", Email: "bob@back.test",
		Role: model.UserRoleUser}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("UpdateUser = %v, want ErrUserNotFound", err)
	}
	if err := s.UpdateUserPassword("bob", "hash"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("UpdateUserPassword = %v, want ErrUserNotFound", err)
	}
	if err := s.UpdateUserAuthProvider("bob", "oidc"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("UpdateUserAuthProvider = %v, want ErrUserNotFound", err)
	}
	if err := s.AddTeamMember("devops", "bob", model.TeamMemberRoleMember); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("AddTeamMember = %v, want ErrUserNotFound", err)
	}

	// The row is untouched: anonymized, and still resolvable for history.
	user, err := s.GetUserByID("bob")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.Name != AnonymizedUserName || user.Email != "" || user.PasswordHash != "" {
		t.Fatalf("erased row was refilled: %+v", user)
	}
	if _, err := s.GetActiveUserByID("bob"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("GetActiveUserByID = %v, want ErrUserNotFound", err)
	}
}

// After erasing one of two admins, the survivor is the last one - and the
// demotion guard has to see that. Counting erased admins as living is how the
// system ends up with no administrator at all.
func TestDemotionBlockedWhenOtherAdminIsDeleted(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops")
	ctx := context.Background()

	for _, id := range []string{"root", "second-root"} {
		if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@example.test",
			Role: model.UserRoleAdmin}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		if err := s.SetUserRole(id, model.UserRoleAdmin); err != nil {
			t.Fatalf("SetUserRole %s: %v", id, err)
		}
	}

	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "second-root"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if count, err := s.CountAdmins(); err != nil || count != 1 {
		t.Fatalf("CountAdmins = %d (%v), want 1 active admin", count, err)
	}
	if err := s.SetUserRole("root", model.UserRoleUser); err == nil {
		t.Fatal("the last living admin was demoted")
	}
}

// Two erasures of two admins, in parallel: exactly one must be refused, and
// neither may deadlock.
func TestConcurrentAdminErasuresKeepOneAdmin(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops")
	ctx := context.Background()

	for _, id := range []string{"root-a", "root-b"} {
		if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@example.test",
			Role: model.UserRoleAdmin}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		if err := s.SetUserRole(id, model.UserRoleAdmin); err != nil {
			t.Fatalf("SetUserRole %s: %v", id, err)
		}
	}

	svc := erasure.NewService(s.ErasureRepository())
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, id := range []string{"root-a", "root-b"} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start
			errs[i] = svc.Erase(ctx, id)
		}(i, id)
	}
	close(start)
	wg.Wait()

	refused := 0
	for i, err := range errs {
		requireNoDeadlock(t, err)
		if errors.Is(err, erasure.ErrLastAdmin) {
			refused++
			continue
		}
		if err != nil {
			t.Fatalf("erasure %d failed unexpectedly: %v", i, err)
		}
	}
	if refused != 1 {
		t.Fatalf("%d of 2 concurrent admin erasures were refused, want exactly 1", refused)
	}
	if count, err := s.CountAdmins(); err != nil || count != 1 {
		t.Fatalf("CountAdmins = %d (%v), want exactly one surviving admin", count, err)
	}
}

// An erasure and a demotion take the same advisory lock, so they cannot both
// decide there is another administrator to fall back on.
func TestConcurrentEraseAndDemote(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops")
	ctx := context.Background()

	for _, id := range []string{"root-a", "root-b"} {
		if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@example.test",
			Role: model.UserRoleAdmin}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		if err := s.SetUserRole(id, model.UserRoleAdmin); err != nil {
			t.Fatalf("SetUserRole %s: %v", id, err)
		}
	}

	var (
		wg        sync.WaitGroup
		eraseErr  error
		demoteErr error
		start     = make(chan struct{})
		eraseSvc  = erasure.NewService(s.ErasureRepository())
	)
	wg.Add(2)
	go func() { defer wg.Done(); <-start; eraseErr = eraseSvc.Erase(ctx, "root-a") }()
	go func() { defer wg.Done(); <-start; demoteErr = s.SetUserRole("root-b", model.UserRoleUser) }()
	close(start)
	wg.Wait()

	requireNoDeadlock(t, eraseErr)
	requireNoDeadlock(t, demoteErr)

	count, err := s.CountAdmins()
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if count < 1 {
		t.Fatalf("the system was left with %d administrators (erase: %v, demote: %v)",
			count, eraseErr, demoteErr)
	}
}

// A create and an erasure of the user it names must not both commit: the
// snapshot carries no foreign key, so an erased user in an active tail is
// invisible to the database and permanent.
func TestConcurrentInitialCreateAndErasure(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")
	ctx := context.Background()

	var (
		wg        sync.WaitGroup
		createErr error
		eraseErr  error
		start     = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, createErr = newTestScheduleService(s, time.Now().UTC()).CreateSchedule(
			ctx, "devops", cmdConfig("alice", "bob"), "alice", nil)
	}()
	go func() {
		defer wg.Done()
		<-start
		eraseErr = erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob")
	}()
	close(start)
	wg.Wait()

	requireNoDeadlock(t, createErr)
	requireNoDeadlock(t, eraseErr)
	assertNoErasedUserOnCall(t, s, "bob", createErr, eraseErr)
}

// The same race with an existing schedule: a save that adds someone, against
// the erasure of that someone.
func TestConcurrentSaveMembershipAndErasure(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	seedTeam(t, s, "devops", "alice", "bob")
	ctx := context.Background()

	if _, err := newTestScheduleService(s, at).CreateSchedule(
		ctx, "devops", cmdConfig("alice"), "alice", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	var (
		wg       sync.WaitGroup
		saveErr  error
		eraseErr error
		start    = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, saveErr = newTestScheduleService(s, at.Add(time.Hour)).Save(ctx, "devops",
			scheduleconfig.SaveCommand{ExpectedVersion: 1, Desired: cmdConfig("alice", "bob"), ActorID: "alice"})
	}()
	go func() {
		defer wg.Done()
		<-start
		eraseErr = erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob")
	}()
	close(start)
	wg.Wait()

	requireNoDeadlock(t, saveErr)
	requireNoDeadlock(t, eraseErr)
	assertNoErasedUserOnCall(t, s, "bob", saveErr, eraseErr)
}

// An override can name someone who is in no group, so it is a second way to
// put an erased user on call.
func TestConcurrentCreateOverrideAndErasure(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	seedTeam(t, s, "devops", "alice", "bob")
	ctx := context.Background()

	if _, err := newTestScheduleService(s, at).CreateSchedule(
		ctx, "devops", cmdConfig("alice"), "alice", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	var (
		wg          sync.WaitGroup
		overrideErr error
		eraseErr    error
		start       = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, overrideErr = newTestScheduleService(s, at.Add(time.Hour)).CreateOverride(ctx, "devops",
			scheduleconfig.OverrideCommand{
				UserID:    "bob",
				ValidFrom: at.Add(24 * time.Hour),
				ValidTo:   at.Add(48 * time.Hour),
				ActorID:   "alice",
			})
	}()
	go func() {
		defer wg.Done()
		<-start
		eraseErr = erasure.NewService(s.ErasureRepository(),
			erasure.WithClock(func() time.Time { return at })).Erase(ctx, "bob")
	}()
	close(start)
	wg.Wait()

	requireNoDeadlock(t, overrideErr)
	requireNoDeadlock(t, eraseErr)

	erased := eraseErr == nil
	liveOverride := overrideErr == nil
	if erased && liveOverride {
		t.Fatal("an erased user became the target of a live override")
	}
}

// A save and an override command serialize on the same schedule row lock.
func TestConcurrentSaveAndOverrideSerialize(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	scheduleID := seedCommandSchedule(t, s, at)
	ctx := context.Background()

	var (
		wg          sync.WaitGroup
		saveErr     error
		overrideErr error
		start       = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, saveErr = newTestScheduleService(s, at.Add(time.Hour)).Save(ctx, "devops",
			scheduleconfig.SaveCommand{ExpectedVersion: 1, Desired: cmdConfig("bob", "alice"), ActorID: "alice"})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, overrideErr = newTestScheduleService(s, at.Add(time.Hour)).CreateOverride(ctx, "devops",
			scheduleconfig.OverrideCommand{
				UserID:    "bob",
				ValidFrom: at.Add(24 * time.Hour),
				ValidTo:   at.Add(48 * time.Hour),
				ActorID:   "alice",
			})
	}()
	close(start)
	wg.Wait()

	requireNoDeadlock(t, saveErr)
	requireNoDeadlock(t, overrideErr)
	if saveErr != nil {
		t.Fatalf("Save: %v", saveErr)
	}
	if overrideErr != nil {
		t.Fatalf("CreateOverride: %v", overrideErr)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE schedule_id = $1`, scheduleID); got != 2 {
		t.Fatalf("got %d revisions, want 2", got)
	}
}

// The whole point of the global lock order is that no combination of commands
// can deadlock. Anything else is a defect, so 40P01 fails the test outright
// rather than being retried away.
func TestParallelCommandsNeverDeadlock(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)
	seedTeam(t, s, "devops", "alice", "bob")
	ctx := context.Background()

	if _, err := newTestScheduleService(s, at).CreateSchedule(
		ctx, "devops", cmdConfig("alice", "bob"), "alice", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	for _, id := range []string{"root-a", "root-b"} {
		if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@example.test",
			Role: model.UserRoleAdmin}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		if err := s.SetUserRole(id, model.UserRoleAdmin); err != nil {
			t.Fatalf("SetUserRole %s: %v", id, err)
		}
	}

	const rounds = 12
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fatal []error
	)
	record := func(err error) {
		if err == nil {
			return
		}
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "40P01" {
			mu.Lock()
			fatal = append(fatal, err)
			mu.Unlock()
		}
	}

	for i := 0; i < rounds; i++ {
		wg.Add(4)
		go func(i int) {
			defer wg.Done()
			svc := newTestScheduleService(s, at.Add(time.Duration(i)*time.Minute))
			_, err := svc.Save(ctx, "devops", scheduleconfig.SaveCommand{
				ExpectedVersion: int64(i + 1), Desired: cmdConfig("bob", "alice"), ActorID: "alice"})
			record(err)
		}(i)
		go func(i int) {
			defer wg.Done()
			svc := newTestScheduleService(s, at.Add(time.Duration(i)*time.Minute))
			_, err := svc.CreateOverride(ctx, "devops", scheduleconfig.OverrideCommand{
				UserID:    "alice",
				ValidFrom: at.Add(time.Duration(100+i) * time.Hour),
				ValidTo:   at.Add(time.Duration(101+i) * time.Hour),
				ActorID:   "alice",
			})
			record(err)
		}(i)
		go func() {
			defer wg.Done()
			record(erasure.NewService(s.ErasureRepository()).Erase(ctx, "root-a"))
		}()
		go func() {
			defer wg.Done()
			record(s.SetUserRole("root-b", model.UserRoleUser))
		}()
	}
	wg.Wait()

	if len(fatal) > 0 {
		t.Fatalf("%d commands deadlocked (40P01); the first was %v", len(fatal), fatal[0])
	}
}

// TD3's completeness criterion: every column that refers to a user is either
// wiped by erasure or listed here as a deliberate survivor. A new column that
// nobody classified fails this test, which is the whole point - erasure
// completeness cannot be maintained by memory.
func TestErasureCoversEveryUserDataSource(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")
	ctx := context.Background()

	// Columns that erasure clears, empties or removes the row of.
	erased := map[string]bool{
		"users.email":                        true,
		"users.name":                         true,
		"users.password_hash":                true,
		"users.auth_provider":                true,
		"users.deleted_at":                   true,
		"api_tokens.user_id":                 true,
		"external_identities.user_id":        true,
		"link_tokens.user_id":                true,
		"team_members.user_id":               true,
		"schedule_override_revisions.reason": true,
		"schedule_revisions.change_reason":   true,
	}
	// Columns that survive by design: immutable identity references that
	// history is joined on, plus legacy tables the cutover removes.
	byDesign := map[string]bool{
		"users.id":                                true,
		"users.role":                              true,
		"schedule_revisions.created_by":           true,
		"schedule_override_revisions.user_id":     true,
		"schedule_override_revisions.recorded_by": true,
		"schedule_overrides.user_id":              true,
		"schedule_overrides.created_by":           true,
		"schedule_users.user_id":                  true,
		"alert_groups.acked_by":                   true,
		"alert_groups.resolved_by":                true,
		"alert_groups.assigned_to":                true,
		"notification_deliveries.user_id":         true,
		"job_steps.user_id":                       true,
		"escalation_steps.target_user_id":         true,
		"timeline_events.actor":                   true,
	}

	rows, err := s.db.Query(`
		SELECT table_name, column_name
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND (column_name = 'user_id'
		       OR column_name = 'created_by'
		       OR column_name = 'recorded_by'
		       OR column_name = 'acked_by'
		       OR column_name = 'resolved_by'
		       OR column_name = 'assigned_to'
		       OR column_name = 'target_user_id'
		       OR (table_name = 'users' AND column_name IN
		           ('id', 'email', 'name', 'role', 'password_hash', 'auth_provider', 'deleted_at')))
		ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatalf("information_schema scan: %v", err)
	}
	defer rows.Close()

	var unclassified []string
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("scan: %v", err)
		}
		key := table + "." + column
		if !erased[key] && !byDesign[key] {
			unclassified = append(unclassified, key)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(unclassified) > 0 {
		t.Fatalf("columns referring to a user that nobody classified as erased or kept by design: %v",
			unclassified)
	}

	// And an end-to-end erasure with data in each reachable source.
	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID: "bob", Provider: "slack", ExternalID: "U123", DisplayName: "Bob",
	}); err != nil {
		t.Fatalf("BindExternalIdentity: %v", err)
	}
	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	for _, q := range []struct {
		name  string
		query string
	}{
		{"api tokens", `SELECT COUNT(*) FROM api_tokens WHERE user_id = $1`},
		{"external identities", `SELECT COUNT(*) FROM external_identities WHERE user_id = $1`},
		{"link tokens", `SELECT COUNT(*) FROM link_tokens WHERE user_id = $1`},
		{"team memberships", `SELECT COUNT(*) FROM team_members WHERE user_id = $1`},
	} {
		if got := countRows(t, s, q.query, "bob"); got != 0 {
			t.Fatalf("%s survived the erasure: %d rows", q.name, got)
		}
	}
}

// requireNoDeadlock fails the test on PostgreSQL 40P01. A deadlock is a defect
// in the lock order, not a transient condition to retry.
func requireNoDeadlock(t *testing.T, err error) {
	t.Helper()
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "40P01" {
		t.Fatalf("deadlock detected: %v", err)
	}
}

// assertNoErasedUserOnCall checks the invariant both orders of commit have to
// preserve: an erased user never appears in an active tail revision.
func assertNoErasedUserOnCall(t *testing.T, s *Store, userID string, errs ...error) {
	t.Helper()
	if _, err := s.GetActiveUserByID(userID); err != nil {
		// The user was erased: no live schedule may name them.
		var tails []erasure.ScheduleTail
		err := s.ErasureRepository().WithinTx(context.Background(), func(tx erasure.Tx) error {
			var err error
			tails, err = tx.ListScheduleTailsLocked(context.Background())
			return err
		})
		if err != nil {
			t.Fatalf("ListScheduleTailsLocked: %v", err)
		}
		for _, tail := range tails {
			for _, layer := range [2]rotation.RotationLayerSnapshot{tail.Snapshot.L1, tail.Snapshot.L2} {
				for _, g := range layer.Groups {
					for _, m := range g.Members {
						if m == userID {
							t.Fatalf("erased user %s is in the active tail of %s (errors: %v)",
								userID, tail.ScheduleID, errs)
						}
					}
				}
			}
		}
	}
}
