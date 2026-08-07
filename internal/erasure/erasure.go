// Package erasure defines the narrow unit of work a user-erasure command
// needs. Keeping it behind an interface stops *sql.Tx from crossing into the
// application layer and keeps the erasure surface enumerable: everything that
// can be wiped for a user is one method here.
package erasure

import (
	"context"
	"time"
)

// Repository hands out one erasure unit of work. Every primitive for one user
// runs inside a single WithinTx call so a partial erasure cannot commit.
type Repository interface {
	WithinTx(ctx context.Context, fn func(Tx) error) error
}

// Tx is the set of erasure primitives.
//
// The two Nullify methods are the only mutation allowed on the append-only
// history tables besides closing a revision: free-text reason fields can name
// a person, so they are declared a deliberate exception to immutability.
// Known residual risk: a third party mentioned inside someone else's text is
// not reachable this way.
type Tx interface {
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

	// NullifyOverrideRevisionReasons clears reason text on override revisions
	// where the user is the target or the author.
	NullifyOverrideRevisionReasons(ctx context.Context, userID string) error

	// NullifyScheduleRevisionChangeReasons clears change reason text on
	// schedule revisions the user authored.
	NullifyScheduleRevisionChangeReasons(ctx context.Context, userID string) error
}
