package erasure

import (
	"context"
	"sort"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
)

// Service is the only entry point for deleting a user. The HTTP handler maps
// its errors; nothing else may reach the primitives.
type Service struct {
	repo Repository
	now  func() time.Time
}

// Option customizes a Service.
type Option func(*Service)

// WithClock overrides the wall clock, so a test can decide which overrides
// count as expired.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService builds a Service over an erasure repository.
func NewService(repo Repository, opts ...Option) *Service {
	s := &Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Erase soft-deletes a user and wipes everything reachable about them.
//
// The row survives, anonymized: every revision, override and event that names
// the ID has to stay explainable, and rendering history for an ID that no
// longer resolves is worse than rendering "Deleted user".
//
// Everything happens in one transaction, in the global lock order - the
// admin-lifecycle mutex, then the user row, then schedules. Erasing someone
// who still holds an assignment is refused rather than silently editing the
// rotation around them: who is on call does not change without a person
// deciding that it does.
//
// Erasing an already-erased user succeeds without doing anything. The caller
// asked for a state, and it holds.
func (s *Service) Erase(ctx context.Context, userID string) error {
	if userID == "" {
		return ErrUserNotFound
	}

	return s.repo.WithinTx(ctx, func(tx Tx) error {
		if err := tx.LockAdminLifecycle(ctx); err != nil {
			return err
		}
		user, err := tx.LockUser(ctx, userID)
		if err != nil {
			return err
		}
		if user.DeletedAt != nil {
			return nil
		}

		// The schedule scan is the LAST thing that can block, because it takes
		// a shared lock on rows a save holds exclusively. Everything above it
		// can wait too, so the instant is captured only once every lock this
		// transaction needs is held.
		//
		// Capturing it any earlier backdates the erasure across somebody
		// else's commit. An erasure that stamped T0, then waited here for a
		// save that removed the user from the rotation at T1, would leave
		// history claiming they were already gone throughout T0..T1 - while
		// the revision in force still had them on call.
		tails, err := tx.ListScheduleTailsLocked(ctx)
		if err != nil {
			return err
		}
		erasedAt := s.now().UTC()

		if err := s.guardAssignments(ctx, tx, userID, erasedAt, tails); err != nil {
			return err
		}
		if err := guardLastAdmin(ctx, tx, user); err != nil {
			return err
		}

		if err := tx.SetUserDeletedAt(ctx, userID, erasedAt); err != nil {
			return err
		}
		if err := tx.AnonymizeUser(ctx, userID); err != nil {
			return err
		}
		if err := tx.DeleteUserAPITokens(ctx, userID); err != nil {
			return err
		}
		if err := tx.DeleteUserExternalIdentities(ctx, userID); err != nil {
			return err
		}
		if err := tx.DeleteUserLinkTokens(ctx, userID); err != nil {
			return err
		}
		if err := tx.NullifyOverrideRevisionReasons(ctx, userID); err != nil {
			return err
		}
		if err := tx.NullifyScheduleRevisionChangeReasons(ctx, userID); err != nil {
			return err
		}
		return tx.DeleteUserTeamMemberships(ctx, userID)
	})
}

// guardAssignments refuses the erasure while the user is assignable, and it
// looks at BOTH sources.
//
// The rotation snapshot alone is not enough: an override can put someone on
// duty who appears in no group at all, so erasing them would leave a current
// or future assignment pointing at an identity that no longer resolves. An
// expired override does not block - its history stays explainable without the
// person, which is the whole point of anonymizing rather than deleting.
//
// The tails arrive already read, because reading them is what takes the last
// lock and the instant has to be captured after that.
func (s *Service) guardAssignments(ctx context.Context, tx Tx, userID string,
	at time.Time, tails []ScheduleTail) error {

	blocking := blockedByRotation(tails, userID)

	overrides, err := tx.ListLiveOverrideHeadsForUser(ctx, userID, at)
	if err != nil {
		return err
	}
	for _, o := range overrides {
		blocking[o.ScheduleID] = ScheduleRef{ScheduleID: o.ScheduleID, TeamID: o.TeamID}
	}

	if len(blocking) == 0 {
		return nil
	}
	refs := make([]ScheduleRef, 0, len(blocking))
	for _, ref := range blocking {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ScheduleID < refs[j].ScheduleID })
	return &UserOnCallError{Schedules: refs}
}

// blockedByRotation is the half of the guard that needs nothing but the tails
// it is given, keyed by schedule so the two sources cannot report one twice.
func blockedByRotation(tails []ScheduleTail, userID string) map[string]ScheduleRef {
	blocking := map[string]ScheduleRef{}
	for _, tail := range tails {
		if snapshotNames(tail.Snapshot, userID) {
			blocking[tail.ScheduleID] = ScheduleRef{ScheduleID: tail.ScheduleID, TeamID: tail.TeamID}
		}
	}
	return blocking
}

// guardLastAdmin refuses to erase the only administrator left.
//
// A flat count is sound here only because the advisory lock is held: role and
// deleted_at can change only through commands that take the same lock, so
// nothing can turn a second admin into a non-admin between the count and the
// commit. Erasing an admin while another one remains stays allowed, including
// erasing yourself.
func guardLastAdmin(ctx context.Context, tx Tx, user *LockedUser) error {
	if user.Role != string(model.UserRoleAdmin) {
		return nil
	}
	admins, err := tx.CountActiveAdmins(ctx)
	if err != nil {
		return err
	}
	if admins <= 1 {
		return ErrLastAdmin
	}
	return nil
}

// snapshotNames reports whether the user is in any group of any layer.
func snapshotNames(snap rotation.ScheduleRevisionSnapshot, userID string) bool {
	for _, layer := range [2]rotation.RotationLayerSnapshot{snap.L1, snap.L2} {
		for _, g := range layer.Groups {
			for _, m := range g.Members {
				if m == userID {
					return true
				}
			}
		}
	}
	return false
}
