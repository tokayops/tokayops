package api

import (
	"context"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
	"github.com/tokayops/tokayops/internal/store"
)

// The API tests run the real handlers and the real services over two doubles:
// MockStore for everything that predates the revision model, and
// fakes.ScheduleConfigRepo for the revision model itself. The legacy mock is
// deliberately NOT extended with revisions.
//
// The two have to agree about one thing - who is in a team - because a handler
// reads membership through the mock while a command validates it through the
// revision contract. The adapters below make membership live in the mock, so a
// test that adds a member sees the same fact from both sides. Without that the
// suite would be testing two universes that never meet.

// testScheduleRepo is the revision fake with membership delegated to the mock.
type testScheduleRepo struct {
	*fakes.ScheduleConfigRepo
	store *store.MockStore
}

func (r *testScheduleRepo) WithinTx(ctx context.Context, fn func(scheduleconfig.ScheduleConfigTx) error) error {
	return r.ScheduleConfigRepo.WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		return fn(&testScheduleTx{ScheduleConfigTx: tx, store: r.store})
	})
}

func (r *testScheduleRepo) WithinSnapshot(ctx context.Context, fn func(scheduleconfig.ScheduleReadView) error) error {
	return r.ScheduleConfigRepo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		return fn(&testScheduleView{ScheduleReadView: view, store: r.store})
	})
}

type testScheduleView struct {
	scheduleconfig.ScheduleReadView
	store *store.MockStore
}

func (v *testScheduleView) GetTeamMemberIDs(ctx context.Context, teamID string) ([]string, error) {
	return activeTeamMemberIDs(v.store, teamID)
}

type testScheduleTx struct {
	scheduleconfig.ScheduleConfigTx
	store *store.MockStore
}

func (t *testScheduleTx) GetTeamMemberIDs(ctx context.Context, teamID string) ([]string, error) {
	return activeTeamMemberIDs(t.store, teamID)
}

func (t *testScheduleTx) ActiveUserIDs(ctx context.Context, userIDs []string) ([]string, error) {
	return activeUserIDs(t.store, userIDs)
}

func (t *testScheduleTx) DeleteTeamMembership(ctx context.Context, teamID, userID string) error {
	return t.store.RemoveTeamMember(teamID, userID)
}

// activeTeamMemberIDs mirrors the store query: members of the team, minus the
// ones that have been erased.
func activeTeamMemberIDs(s *store.MockStore, teamID string) ([]string, error) {
	members, err := s.GetTeamMembers(teamID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range members {
		if _, err := s.GetActiveUserByID(m.ID); err != nil {
			continue
		}
		out = append(out, m.ID)
	}
	return out, nil
}

// activeUserIDs answers "which of these people still exist" from the mock,
// which is where the API tests keep users.
func activeUserIDs(s *store.MockStore, userIDs []string) ([]string, error) {
	var out []string
	for _, id := range userIDs {
		if _, err := s.GetActiveUserByID(id); err != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// testErasureRepo implements the erasure contract over the mock store, so
// erasing a user through the real command actually ends their session and
// removes their memberships in the store the other handlers read.
//
// Tails and Overrides are set by the tests that need the assignment guard to
// fire; the mock knows nothing about revisions, so there is nothing to derive
// them from.
type testErasureRepo struct {
	mu    sync.Mutex
	store *store.MockStore

	Tails     []erasure.ScheduleTail
	Overrides []erasure.OverrideAssignment

	// Calls records the primitives in order, so a test can assert the lock
	// order and that the wipe reached every source.
	Calls []string
}

func newTestErasureRepo(s *store.MockStore) *testErasureRepo {
	return &testErasureRepo{store: s}
}

func (r *testErasureRepo) WithinTx(ctx context.Context, fn func(erasure.Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(&testErasureTx{repo: r})
}

type testErasureTx struct {
	repo *testErasureRepo
}

func (t *testErasureTx) record(method string) { t.repo.Calls = append(t.repo.Calls, method) }

func (t *testErasureTx) LockAdminLifecycle(ctx context.Context) error {
	t.record("LockAdminLifecycle")
	return nil
}

func (t *testErasureTx) LockUser(ctx context.Context, userID string) (*erasure.LockedUser, error) {
	t.record("LockUser")
	user, err := t.repo.store.GetUserByID(userID)
	if err != nil {
		return nil, erasure.ErrUserNotFound
	}
	locked := &erasure.LockedUser{ID: user.ID, Role: string(user.Role)}
	if _, err := t.repo.store.GetActiveUserByID(userID); err != nil {
		at := time.Unix(0, 0).UTC()
		locked.DeletedAt = &at
	}
	return locked, nil
}

func (t *testErasureTx) CountActiveAdmins(ctx context.Context) (int, error) {
	t.record("CountActiveAdmins")
	users, err := t.repo.store.GetAllUsers()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, u := range users {
		if u.Role != model.UserRoleAdmin {
			continue
		}
		if _, err := t.repo.store.GetActiveUserByID(u.ID); err != nil {
			continue
		}
		count++
	}
	return count, nil
}

func (t *testErasureTx) ListScheduleTailsLocked(ctx context.Context) ([]erasure.ScheduleTail, error) {
	t.record("ListScheduleTailsLocked")
	return t.repo.Tails, nil
}

func (t *testErasureTx) ListLiveOverrideHeadsForUser(ctx context.Context, userID string, at time.Time) ([]erasure.OverrideAssignment, error) {
	t.record("ListLiveOverrideHeadsForUser")
	var out []erasure.OverrideAssignment
	for _, o := range t.repo.Overrides {
		if o.ValidTo.After(at) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (t *testErasureTx) SetUserDeletedAt(ctx context.Context, userID string, at time.Time) error {
	t.record("SetUserDeletedAt")
	t.repo.store.EraseUser(userID)
	return nil
}

func (t *testErasureTx) AnonymizeUser(ctx context.Context, userID string) error {
	t.record("AnonymizeUser")
	t.repo.store.AnonymizeUser(userID)
	return nil
}

func (t *testErasureTx) DeleteUserAPITokens(ctx context.Context, userID string) error {
	t.record("DeleteUserAPITokens")
	return nil
}

func (t *testErasureTx) DeleteUserExternalIdentities(ctx context.Context, userID string) error {
	t.record("DeleteUserExternalIdentities")
	return nil
}

func (t *testErasureTx) DeleteUserLinkTokens(ctx context.Context, userID string) error {
	t.record("DeleteUserLinkTokens")
	return nil
}

func (t *testErasureTx) DeleteUserTeamMemberships(ctx context.Context, userID string) error {
	t.record("DeleteUserTeamMemberships")
	teams, err := t.repo.store.GetTeamMembershipsForUser(userID)
	if err != nil {
		return err
	}
	for teamID := range teams {
		if err := t.repo.store.RemoveTeamMember(teamID, userID); err != nil {
			return err
		}
	}
	return nil
}

func (t *testErasureTx) NullifyOverrideRevisionReasons(ctx context.Context, userID string) error {
	t.record("NullifyOverrideRevisionReasons")
	return nil
}

func (t *testErasureTx) NullifyScheduleRevisionChangeReasons(ctx context.Context, userID string) error {
	t.record("NullifyScheduleRevisionChangeReasons")
	return nil
}

var (
	_ scheduleconfig.ScheduleConfigRepository = (*testScheduleRepo)(nil)
	_ scheduleconfig.ScheduleReadRepository   = (*testScheduleRepo)(nil)
	_ scheduleconfig.ScheduleConfigTx         = (*testScheduleTx)(nil)
	_ scheduleconfig.ScheduleReadView         = (*testScheduleView)(nil)
	_ erasure.Repository                      = (*testErasureRepo)(nil)
	_ erasure.Tx                              = (*testErasureTx)(nil)
)
