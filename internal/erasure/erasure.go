// Package erasure defines the narrow unit of work a user-erasure command
// needs. Keeping it behind an interface stops *sql.Tx from crossing into the
// application layer and keeps the erasure surface enumerable: everything that
// can be wiped for a user is one method here.
package erasure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
)

// Typed refusals. Both are states a caller can resolve, not failures.
var (
	// ErrUserNotFound means no user answers to the ID.
	ErrUserNotFound = errors.New("erasure: user not found")

	// ErrUserOnCall means the user still holds a current assignment.
	ErrUserOnCall = errors.New("erasure: user holds a current on-call assignment")

	// ErrLastAdmin means erasing the user would leave the system without an
	// administrator.
	ErrLastAdmin = errors.New("erasure: user is the last active admin")
)

// ScheduleRef names a schedule that blocks an erasure.
type ScheduleRef struct {
	ScheduleID string
	TeamID     string
}

// UserOnCallError lists every schedule that blocks the erasure, from both
// sources: the rotation groups of an active tail revision and any live
// override aimed at the user. Reporting them together is what lets an operator
// fix the schedules in one pass instead of one failed delete at a time.
type UserOnCallError struct {
	Schedules []ScheduleRef
}

func (e *UserOnCallError) Error() string {
	ids := make([]string, len(e.Schedules))
	for i, s := range e.Schedules {
		ids[i] = s.ScheduleID
	}
	return fmt.Sprintf("erasure: user is on call in %s", strings.Join(ids, ", "))
}

func (e *UserOnCallError) Unwrap() error { return ErrUserOnCall }

// LockedUser is what the row lock on a user returns: the two facts every guard
// needs, read under the lock that makes them stable.
type LockedUser struct {
	ID        string
	Role      string
	DeletedAt *time.Time
}

// ScheduleTail is the currently-in-force revision of one live schedule.
// The snapshot travels whole so the membership test lives in the service,
// where it can be exercised without a database.
type ScheduleTail struct {
	ScheduleID string
	TeamID     string
	Snapshot   rotation.ScheduleRevisionSnapshot
}

// OverrideAssignment is one live override head aimed at the user.
type OverrideAssignment struct {
	ScheduleID string
	TeamID     string
	OverrideID string
	ValidFrom  time.Time
	ValidTo    time.Time
}

// Repository hands out one erasure unit of work. Every primitive for one user
// runs inside a single WithinTx call so a partial erasure cannot commit.
type Repository interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error
}

// Tx is the set of erasure primitives plus the locks and reads its guards need.
//
// The two Nullify methods are the only mutation allowed on the append-only
// history tables besides closing a revision: free-text reason fields can name
// a person, so they are declared a deliberate exception to immutability.
// Known residual risk: a third party mentioned inside someone else's text is
// not reachable this way.
type Tx interface {
	// LockAdminLifecycle serializes every operation that can change how many
	// active administrators exist - this command and role assignment.
	//
	// It is a transaction-scoped advisory lock rather than row locks over the
	// admin set for two reasons. Reading a count without serializing admits
	// write skew: two erasures each see two admins and both commit. And
	// locking "the target, then every admin" deadlocks against itself, because
	// the target was already taken outside the ordering.
	//
	// It is step 1 of the global lock order: advisory, then users, then
	// schedules.
	LockAdminLifecycle(ctx context.Context) error

	// LockUser takes FOR UPDATE on one user and returns what the guards need.
	// This row is the serialization point against every command that can
	// assign work to the user; those take a shared lock on it first.
	LockUser(ctx context.Context, userID string) (*LockedUser, error)

	// CountActiveAdmins counts admins that have not been erased. A flat count
	// is enough because the advisory lock is held: nothing else can change a
	// role or a deleted_at while this runs.
	CountActiveAdmins(ctx context.Context) (int, error)

	// ListScheduleTailsLocked returns the in-force revision of every schedule
	// that is not deleted, taking a shared lock on the schedule rows so a
	// concurrent save cannot slip a new assignment past the guard.
	ListScheduleTailsLocked(ctx context.Context) ([]ScheduleTail, error)

	// ListLiveOverrideHeadsForUser returns the override heads aimed at the
	// user that have not expired at `at`, tombstones excluded, on schedules
	// that are not deleted. A future override counts: it would become an
	// unresolvable assignment the moment it starts.
	ListLiveOverrideHeadsForUser(ctx context.Context, userID string, at time.Time) ([]OverrideAssignment, error)

	// SetUserDeletedAt marks the user as erased without removing the row -
	// history keeps referring to the ID.
	SetUserDeletedAt(ctx context.Context, userID string, at time.Time) error

	// AnonymizeUser strips identifying columns. It deliberately leaves id and
	// role alone: role removal has its own invariants (a system must keep at
	// least one administrator) and is not an erasure concern.
	AnonymizeUser(ctx context.Context, userID string) error

	DeleteUserAPITokens(ctx context.Context, userID string) error
	DeleteUserExternalIdentities(ctx context.Context, userID string) error
	DeleteUserLinkTokens(ctx context.Context, userID string) error

	// DeleteUserTeamMemberships removes the user from every team, matching
	// what the legacy delete did.
	DeleteUserTeamMemberships(ctx context.Context, userID string) error

	// NullifyOverrideRevisionReasons clears reason text on override revisions
	// where the user is the target or the author.
	NullifyOverrideRevisionReasons(ctx context.Context, userID string) error

	// NullifyScheduleRevisionChangeReasons clears change reason text on
	// schedule revisions the user authored.
	NullifyScheduleRevisionChangeReasons(ctx context.Context, userID string) error
}
