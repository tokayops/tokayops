package scheduleconfig

import (
	"context"
	"time"
)

// ScheduleReadRepository is the read side of schedule configuration.
//
// It hands out a consistent database snapshot instead of individual reads,
// and that is the whole point of the interface: one rendered answer is built
// from the schedule root, the revisions overlapping the range and the
// override projection. Those are three statements, and a Save committing
// between any two of them would splice two different states into one answer -
// a rotation read from the old tail revision next to overrides read after the
// edit. READ COMMITTED does not help, because it takes a fresh snapshot per
// statement.
//
// There is deliberately no way to run a single read outside a snapshot: the
// consistency of a render must not depend on every caller remembering to open
// one.
type ScheduleReadRepository interface {
	// WithinSnapshot runs fn against one immutable view of the database. An
	// error from fn is returned as is; nothing here writes, so there is
	// nothing to roll back.
	WithinSnapshot(ctx context.Context, fn func(ScheduleReadView) error) error
}

// ScheduleReadView is the set of reads a renderer or an on-call projection
// needs. Every method observes the same state.
//
// The command-side ScheduleConfigTx embeds this view, so a command can read
// with the same contract inside its own write transaction.
type ScheduleReadView interface {
	// GetScheduleRoot returns the aggregate root, including a soft-deleted
	// one: history stays readable after a delete.
	GetScheduleRoot(ctx context.Context, scheduleID string) (*ScheduleRoot, error)

	// GetScheduleRootByTeam is the same lookup by the team that owns the
	// schedule.
	GetScheduleRootByTeam(ctx context.Context, teamID string) (*ScheduleRoot, error)

	// ListScheduleRoots returns every schedule root, soft-deleted ones
	// included, ordered by ID.
	//
	// There is deliberately no includeDeleted flag. The consumer of this is the
	// bulk on-call projection, and a schedule that was deleted is an answer it
	// has to carry: the handoff notifier learns from it that a duty ended.
	// Which consumer wants deleted rows dropped is knowledge that belongs to
	// that consumer, not to the read contract.
	ListScheduleRoots(ctx context.Context) ([]ScheduleRoot, error)

	// GetRevisionsInRange returns the revisions whose half-open effective
	// interval overlaps [from, until), ordered by effective_from. Revisions
	// of both kinds are returned: the caller decides what a deleted period
	// means, and hiding it here would turn it back into an unexplained gap.
	//
	// An empty result is not an error - a range can legitimately precede the
	// first revision of a schedule.
	GetRevisionsInRange(ctx context.Context, scheduleID string, from, until time.Time) ([]ScheduleRevision, error)

	// GetEffectiveRevision returns the revision in force at `at`, which may
	// be a deleted-kind revision. Callers must branch on Kind rather than
	// assume a revision means an active schedule.
	//
	// Two revisions in force at one instant is ErrRevisionOverlap, never a
	// choice between them. The exclusion constraint forbids the pair, so
	// finding it means damage - and returning either one would put an
	// arbitrary group on duty and tell nobody. This is part of the contract,
	// not of one implementation: a double that resolved it silently would let
	// the behaviour this refuses pass every unit test.
	GetEffectiveRevision(ctx context.Context, scheduleID string, at time.Time) (*ScheduleRevision, error)

	// GetOverrideProjectionInRange returns the WINNING revision per
	// override_id - not every revision of it - among those overlapping the
	// range, ordered by valid_from then override_id.
	//
	// A nil bound means unbounded on that side. The projection is always the
	// current one: what was known at an earlier system time is not a product
	// capability, and the contract stopped promising it.
	// state; a value means the state as it was recorded at that system time,
	// which is what lets history be replayed as it was known then.
	GetOverrideProjectionInRange(ctx context.Context, scheduleID string, from, until *time.Time) ([]OverrideRevision, error)

	// GetRevisionByID returns one revision of one schedule. The schedule ID is
	// part of the lookup, not a convenience: a revision ID guessed from
	// another team's schedule must answer "not found", not hand over its
	// snapshot.
	GetRevisionByID(ctx context.Context, scheduleID, revisionID string) (*ScheduleRevision, error)

	// ListRevisions pages the audit trail newest first. beforeVersion nil
	// starts at the tail; a value continues from the last version the caller
	// saw, exclusive. Both kinds are returned - a deleted period is part of
	// the history someone is auditing.
	//
	// limit is applied as given; normalizing it to a sane page size belongs to
	// the handler that knows what the API promises.
	ListRevisions(ctx context.Context, scheduleID string, limit int, beforeVersion *int64) ([]ScheduleRevision, error)

	// GetTeamMemberIDs lists the ACTIVE members of a team: erased users are
	// excluded, so a soft-deleted person can never be validated back into a
	// rotation by the next save.
	//
	// It takes no lock. Membership is read for validation after the schedule
	// lock is already held, and that lock - not this read - is what serializes
	// it against a concurrent membership change.
	GetTeamMemberIDs(ctx context.Context, teamID string) ([]string, error)

	// GetOverrideHead returns the LAST revision of one logical override,
	// including a tombstone. The projection deliberately hides tombstones, but
	// an update, a delete and an ownership check all have to tell "deleted"
	// apart from "never existed": without the tombstone, editing a removed
	// override would start its numbering again at revision 1.
	GetOverrideHead(ctx context.Context, scheduleID, overrideID string) (*OverrideRevision, error)

	// ListOverrideHeads returns the head revision of every logical override of
	// a schedule. includeDeleted false drops tombstoned ones, which is the
	// editor's view: the overrides that currently exist.
	ListOverrideHeads(ctx context.Context, scheduleID string, includeDeleted bool) ([]OverrideRevision, error)
}
