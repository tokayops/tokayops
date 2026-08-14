package scheduleconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/tokayops/tokayops/internal/rotation"
)

// Service is the only application entry point for schedule configuration
// commands. HTTP handlers bind and map errors; they never touch a repository.
type Service struct {
	repo  ScheduleConfigRepository
	now   func() time.Time
	newID func() string
	logf  func(format string, args ...any)
}

// Option customizes a Service. The clock and ID source are injectable so tests
// can pin both.
type Option func(*Service)

// WithClock overrides the wall clock. One transition uses one time value in
// every row it writes.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithIDSource overrides identifier generation.
func WithIDSource(newID func() string) Option {
	return func(s *Service) { s.newID = newID }
}

// WithLogger overrides the commit log sink.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(s *Service) { s.logf = logf }
}

// NewService builds a Service over a repository.
func NewService(repo ScheduleConfigRepository, opts ...Option) *Service {
	s := &Service{
		repo:  repo,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return uuid.New().String() },
		logf:  log.Printf,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SaveCommand is one edit of a schedule's configuration.
//
// It carries no timestamp on purpose. A moment supplied by a client cannot
// express when the change applies: the payload was composed before the request
// was sent, the request waited on a lock, and the only instant that means
// anything is the one captured after the schedule is locked.
type SaveCommand struct {
	ExpectedVersion int64
	Desired         rotation.ScheduleConfiguration
	ActorID         string
	Reason          *string
}

// SaveResult is everything the caller needs to answer, including when nothing
// was written: a no-op still has a version, a revision that is in force, and
// the instant at which "nothing changed" was decided.
type SaveResult struct {
	// Revision is the new revision, or the unchanged tail on a no-op.
	Revision *ScheduleRevision

	// Version is config_version after the command; unchanged on a no-op.
	Version int64

	// EffectiveAt is the captured effective instant. On a no-op it is the
	// moment the comparison was made.
	EffectiveAt time.Time

	Noop      bool
	Recreated bool
	Created   bool

	// commit is the line to write once the transaction is known to have
	// committed. Unexported: it is plumbing between the pipeline and the
	// caller inside this package, not part of the answer.
	commit *commitLog
}

// DeleteCommand deactivates a schedule.
type DeleteCommand struct {
	ExpectedVersion int64
	ActorID         string
	Reason          *string
}

// Save is the single write path for schedule configuration.
//
// It branches on the state of the root rather than exposing three endpoints:
// no root is a create, a soft-deleted root is a recreate, anything else is an
// edit. All three converge on the same validation, the same plan and the same
// commit guard, and differ only in what they write. Splitting them would mean
// two implementations of "initialize a schedule" and a recreate that a stale
// browser tab could trigger by accident - deleting bumps config_version, so a
// stale expected_version is a conflict here rather than a resurrection.
func (s *Service) Save(ctx context.Context, teamID string, cmd SaveCommand) (*SaveResult, error) {
	if teamID == "" {
		return nil, invalidField("team_id", "is required")
	}
	// Shape validation is pure, so it runs before the transaction: a payload
	// that could never be stored should not open one or wait for a lock.
	desired, err := NormalizeConfiguration(cmd.Desired)
	if err != nil {
		return nil, err
	}

	var result *SaveResult
	err = s.runCommand(ctx, func(tx ScheduleConfigTx) error {
		res, err := s.save(ctx, tx, teamID, desired, cmd)
		if err != nil {
			return err
		}
		result = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logCommit(result.commit)
	return result, nil
}

// runCommand is the one place a configuration command opens a transaction.
//
// It used to also time the span and count conflicts through a metrics sink of
// this package's own. Both are already visible in the HTTP metrics every
// command arrives through - duration by route, conflicts as the 409 - so the
// sink was a second, narrower copy of the same numbers.
func (s *Service) runCommand(ctx context.Context, fn func(ScheduleConfigTx) error) error {
	return s.repo.WithinTx(ctx, fn)
}

func (s *Service) save(ctx context.Context, tx ScheduleConfigTx, teamID string,
	desired rotation.ScheduleConfiguration, cmd SaveCommand) (*SaveResult, error) {

	// Users before schedules, always. Erasure locks the user it is erasing and
	// then scans schedules; a command that locked the schedule first and the
	// users second would be the other half of an AB-BA deadlock.
	if err := tx.LockUsers(ctx, commandUserIDs(cmd.ActorID, ConfigurationUserIDs(desired)...)); err != nil {
		return nil, err
	}
	if err := requireActiveActor(ctx, tx, cmd.ActorID); err != nil {
		return nil, err
	}

	root, err := tx.GetScheduleRootByTeam(ctx, teamID)
	if errors.Is(err, ErrScheduleNotFound) {
		// There is no row to lock yet, so the create branch cannot collide on
		// one: two concurrent creates are separated by the unique constraint
		// on team_id, which surfaces as ErrScheduleExists.
		if cmd.ExpectedVersion != 0 {
			return nil, &VersionConflictError{Expected: cmd.ExpectedVersion, Current: 0}
		}
		return s.initialize(ctx, tx, teamID, desired, cmd.ActorID, cmd.Reason)
	}
	if err != nil {
		return nil, err
	}

	target, err := s.lockForWrite(ctx, tx, root, cmd.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	return s.rewrite(ctx, tx, target, teamID, desired, cmd)
}

// writeTarget is the schedule a command has taken the lock on: the root as it
// was under that lock, the revision in force, and the instant the change takes
// effect. The three travel together because they are only valid together -
// each was read after the lock and against the same state.
type writeTarget struct {
	root        *ScheduleRoot
	tail        *ScheduleRevision
	effectiveAt time.Time
}

// lockForWrite is the preamble every write to an existing schedule shares:
// take the row lock, refuse a row that carries no revision history, check the
// caller is not stale, read the revision in force and check it agrees with the
// deleted projection.
//
// Effective time is captured last, and that ordering is the point of the
// function: the wait for the lock can be long, and an instant read before it
// would place the change in a slot that has already passed.
func (s *Service) lockForWrite(ctx context.Context, tx ScheduleConfigTx,
	root *ScheduleRoot, expectedVersion int64) (*writeTarget, error) {

	locked, err := tx.LockSchedule(ctx, root.ID)
	if err != nil {
		return nil, err
	}
	if locked.ConfigVersion != expectedVersion {
		return nil, &VersionConflictError{Expected: expectedVersion, Current: locked.ConfigVersion}
	}
	tail, err := tx.GetTailRevision(ctx, locked.ID)
	if err != nil {
		return nil, err
	}
	if err := checkDeletionConsistency(locked, tail); err != nil {
		return nil, err
	}
	return &writeTarget{
		root:        locked,
		tail:        tail,
		effectiveAt: NextEffectiveAt(&tail.EffectiveFrom, s.now().UTC()),
	}, nil
}

// rewrite is the edit and recreate branch: validate, plan, guard, and either
// report a no-op or append the revision.
func (s *Service) rewrite(ctx context.Context, tx ScheduleConfigTx, target *writeTarget,
	teamID string, desired rotation.ScheduleConfiguration, cmd SaveCommand) (*SaveResult, error) {

	// A deleted schedule is recreated, not edited. Its tail snapshot is a copy
	// kept so the column stays decodable - it is not a configuration in force,
	// so the planner is given no current state at all and starts the rotation
	// from the first group.
	recreate := target.root.DeletedAt != nil
	var current *rotation.ScheduleRevisionSnapshot
	if !recreate {
		current = &target.tail.Snapshot
	}

	// Deliberately re-read, and after the lock. The list LockUsers was built
	// from may be stale with respect to a concurrent RemoveTeamMember, which
	// serializes on the schedule lock rather than on the user rows.
	if err := ValidateMembership(ctx, tx, teamID, ConfigurationUserIDs(desired)); err != nil {
		return nil, err
	}

	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Current:     current,
		Desired:     desired,
		EffectiveAt: target.effectiveAt,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	if plan.Noop {
		// An early return, not an error: the empty transaction commits.
		// Rolling back would read as a failure in every log and metric that
		// watches this path, and nothing happened that needs undoing.
		return &SaveResult{
			Revision:    target.tail,
			Version:     target.root.ConfigVersion,
			EffectiveAt: target.effectiveAt,
			Noop:        true,
		}, nil
	}

	summary := plan.Change
	revision := &ScheduleRevision{
		ID:            s.newID(),
		ScheduleID:    target.root.ID,
		Version:       target.root.ConfigVersion + 1,
		Kind:          RevisionActive,
		Snapshot:      plan.Snapshot,
		EffectiveFrom: target.effectiveAt,
		RecordedAt:    target.effectiveAt,
		CreatedBy:     optionalString(cmd.ActorID),
		ChangeReason:  cmd.Reason,
		ChangeSummary: &summary,
	}

	trigger := TriggerSaved
	if recreate {
		trigger = TriggerRecreated
	}
	commit, err := s.appendRevision(ctx, tx, target, revision, recreate, trigger, cmd.ActorID)
	if err != nil {
		return nil, err
	}
	return &SaveResult{
		Revision:    revision,
		Version:     revision.Version,
		EffectiveAt: target.effectiveAt,
		Recreated:   recreate,
		commit:      commit,
	}, nil
}

// appendRevision is the write half, in the only order that keeps history
// intact: close the tail, insert its successor, move the deleted projection if
// the schedule is coming back, advance the version, record the event.
func (s *Service) appendRevision(ctx context.Context, tx ScheduleConfigTx, target *writeTarget,
	revision *ScheduleRevision, clearDeleted bool, trigger, actorID string) (*commitLog, error) {

	if err := tx.CloseRevision(ctx, target.root.ID, target.tail.ID, target.effectiveAt); err != nil {
		return nil, err
	}
	if err := tx.InsertRevision(ctx, revision); err != nil {
		return nil, err
	}
	if clearDeleted {
		if err := tx.SetScheduleDeleted(ctx, target.root.ID, nil); err != nil {
			return nil, err
		}
	}
	if err := tx.AdvanceVersion(ctx, target.root.ID, target.root.ConfigVersion, target.effectiveAt); err != nil {
		return nil, err
	}
	return s.recordChange(trigger, &target.tail.ID, revision,
		target.root.ConfigVersion, target.effectiveAt, actorID), nil
}

// initialize writes the root, revision 1 and the creation event.
func (s *Service) initialize(ctx context.Context, tx ScheduleConfigTx, teamID string,
	desired rotation.ScheduleConfiguration, actorID string, reason *string) (*SaveResult, error) {

	// The team row is the serialization point, and it is taken BEFORE
	// membership is read.
	//
	// There is no schedule row to lock yet - that is the whole shape of this
	// branch - but that does not mean there is nothing to lock. Without this,
	// RemoveTeamMember's own no-schedule branch (which deletes a membership and
	// takes no schedule lock either, because there is none) can commit between
	// the read below and this transaction's commit, leaving a schedule whose
	// rotation names somebody the team no longer contains.
	//
	// Lock order across this package is team -> schedule, the same order
	// DeleteTeam and RemoveTeamMember take, so no pair of them can deadlock.
	if err := tx.LockTeam(ctx, teamID); err != nil {
		return nil, err
	}

	// No tail revision to stay ahead of: effective time is simply now.
	effectiveAt := NormalizeTimestamp(s.now().UTC())

	if err := ValidateMembership(ctx, tx, teamID, ConfigurationUserIDs(desired)); err != nil {
		return nil, err
	}

	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Current:     nil,
		Desired:     desired,
		EffectiveAt: effectiveAt,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	// A no-op needs a current configuration to be equal to; the planner cannot
	// report one here. Guard rather than assume.
	if plan.Noop {
		return nil, fmt.Errorf("%w: initial transition reported as no-op", ErrInvariantViolation)
	}
	summary := plan.Change
	root := &ScheduleRoot{ID: s.newID(), TeamID: teamID}
	revision := &ScheduleRevision{
		ID:            s.newID(),
		ScheduleID:    root.ID,
		Version:       1,
		Kind:          RevisionActive,
		Snapshot:      plan.Snapshot,
		EffectiveFrom: effectiveAt,
		RecordedAt:    effectiveAt,
		CreatedBy:     optionalString(actorID),
		ChangeReason:  reason,
		ChangeSummary: &summary,
	}

	if err := tx.CreateInitialSchedule(ctx, root, revision); err != nil {
		return nil, err
	}
	commit := s.recordChange(TriggerCreated, nil, revision, 0, effectiveAt, actorID)

	return &SaveResult{
		Revision:    revision,
		Version:     revision.Version,
		EffectiveAt: effectiveAt,
		Created:     true,
		commit:      commit,
	}, nil
}

// Delete deactivates a schedule without erasing anything.
//
// The deleted period becomes a revision of its own, carrying a copy of the
// last valid snapshot, so the chain stays unbroken and a later recreate is
// distinguishable from a lost row. Every live override head is tombstoned in
// the same transaction: an override whose valid_to has not arrived yet would
// otherwise survive the delete and, after a recreate, quietly take over the
// rotation again.
func (s *Service) Delete(ctx context.Context, teamID string, cmd DeleteCommand) error {
	if teamID == "" {
		return invalidField("team_id", "is required")
	}

	var commit *commitLog
	err := s.runCommand(ctx, func(tx ScheduleConfigTx) error {
		var err error
		commit, err = s.delete(ctx, tx, teamID, cmd)
		return err
	})
	if err != nil {
		return err
	}
	s.logCommit(commit)
	return nil
}

func (s *Service) delete(ctx context.Context, tx ScheduleConfigTx, teamID string, cmd DeleteCommand) (*commitLog, error) {
	// The deleted revision records the actor and their reason, so the actor is
	// locked and checked here exactly as in a save.
	if err := tx.LockUsers(ctx, commandUserIDs(cmd.ActorID)); err != nil {
		return nil, err
	}
	if err := requireActiveActor(ctx, tx, cmd.ActorID); err != nil {
		return nil, err
	}

	root, err := tx.GetScheduleRootByTeam(ctx, teamID)
	if err != nil {
		return nil, err
	}
	target, err := s.lockForWrite(ctx, tx, root, cmd.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if target.root.DeletedAt != nil {
		return nil, ErrScheduleDeleted
	}

	deleted := &ScheduleRevision{
		ID:            s.newID(),
		ScheduleID:    target.root.ID,
		Version:       target.root.ConfigVersion + 1,
		Kind:          RevisionDeleted,
		Snapshot:      target.tail.Snapshot,
		EffectiveFrom: target.effectiveAt,
		RecordedAt:    target.effectiveAt,
		CreatedBy:     optionalString(cmd.ActorID),
		ChangeReason:  cmd.Reason,
	}

	if err := tx.CloseRevision(ctx, target.root.ID, target.tail.ID, target.effectiveAt); err != nil {
		return nil, err
	}
	if err := tx.InsertRevision(ctx, deleted); err != nil {
		return nil, err
	}
	if err := s.endLiveOverrides(ctx, tx, target.root.ID, target.effectiveAt, cmd.ActorID, cmd.Reason); err != nil {
		return nil, err
	}
	if err := tx.SetScheduleDeleted(ctx, target.root.ID, &target.effectiveAt); err != nil {
		return nil, err
	}
	if err := tx.AdvanceVersion(ctx, target.root.ID, target.root.ConfigVersion, target.effectiveAt); err != nil {
		return nil, err
	}
	return s.recordChange(TriggerDeleted, &target.tail.ID, deleted,
		target.root.ConfigVersion, target.effectiveAt, cmd.ActorID), nil
}

// endLiveOverrides ends every override the schedule still has, by the same
// rule a cancel follows. They share one recordedAt: the monotonicity rule
// applies to a command, not to each row it writes, and the next override
// command will still land above all of them.
func (s *Service) endLiveOverrides(ctx context.Context, tx ScheduleConfigTx,
	scheduleID string, at time.Time, actorID string, reason *string) error {

	heads, err := tx.ListOverrideHeads(ctx, scheduleID, false)
	if err != nil {
		return err
	}
	if len(heads) == 0 {
		return nil
	}
	recordedAt := NormalizeTimestamp(at)
	for _, head := range heads {
		next := head
		next.RevisionID = s.newID()
		next.Revision = head.Revision + 1
		next.RecordedAt = recordedAt
		next.RecordedBy = optionalString(actorID)
		// The deleter's reason, not the one whoever created the override
		// wrote: a revision's reason and its recorded_by are the same
		// person's, or the audit trail attributes words to someone who never
		// said them.
		next.Reason = reason
		// The schedule is going away, and so is every override on it - but by
		// the same rule a cancel follows: an override that has already covered
		// some hours keeps them, and only the rest goes. Tombstoning all of
		// them would take the served ones out of the projection and rewrite
		// who was on duty before the schedule was deleted.
		//
		// Already-ended overrides need nothing: they are outside the deletion
		// instant and the projection already answers them correctly.
		switch {
		case !recordedAt.After(head.ValidFrom):
			next.Deleted = true
		case recordedAt.Before(head.ValidTo):
			next.ValidTo = recordedAt
		default:
			continue
		}
		if err := tx.InsertOverrideRevision(ctx, &next); err != nil {
			return err
		}
	}
	return nil
}

// RemoveTeamMember takes someone out of a team, refusing while they hold a
// current assignment on that team's schedule.
//
// The same rule that blocks erasing a person applies here: a membership is
// what makes a rotation entry resolvable, so removing it out from under a live
// assignment leaves the schedule pointing at a non-member. Auto-editing the
// rotation instead was rejected for erasure - who is on call does not change
// without someone deciding it does - and would be inconsistent here.
//
// The check is team-scoped, unlike erasure's global sweep, and it serializes
// against Save on the schedule row lock, and - when the team has no schedule
// yet - on the team row lock. That is why Save re-reads membership after taking
// a lock, and why both of these paths take the team row before anything else:
// the schedule lock cannot serialize a schedule that does not exist.
func (s *Service) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	if teamID == "" || userID == "" {
		return invalidField("team_id", "and user_id are required")
	}
	return s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		// Taken first, and for both branches. The branch below that finds no
		// schedule used to take no lock at all, which made it the half of a
		// race the schedule lock could not cover: a concurrent first Save has
		// no schedule row to be blocked by either, so nothing serialized the
		// two and the save could commit a rotation naming a member this
		// transaction had already removed.
		//
		// Lock order is team -> schedule here and everywhere else in this
		// package.
		if err := tx.LockTeam(ctx, teamID); err != nil {
			return err
		}

		root, err := tx.GetScheduleRootByTeam(ctx, teamID)
		if errors.Is(err, ErrScheduleNotFound) {
			// No schedule means no assignment to protect.
			return tx.DeleteTeamMembership(ctx, teamID, userID)
		}
		if err != nil {
			return err
		}
		locked, err := tx.LockSchedule(ctx, root.ID)
		if err != nil {
			return err
		}
		// A deleted schedule has nobody on duty, so it blocks nothing -
		// otherwise a team could never clean up after deleting its schedule.
		if locked.DeletedAt == nil {
			blocked, err := s.holdsAssignment(ctx, tx, locked.ID, userID)
			if err != nil {
				return err
			}
			if blocked {
				return &MemberOnCallError{
					Schedules: []ScheduleRef{{ScheduleID: locked.ID, TeamID: teamID}},
				}
			}
		}
		return tx.DeleteTeamMembership(ctx, teamID, userID)
	})
}

// DeleteTeam removes a team, or explains what retains it.
//
// It lives here, in a package about schedules, because the thing that retains
// a team is schedule history and the transaction that has to establish that is
// this one. The honest home would be a team service, which does not exist;
// RemoveTeamMember is here for the same reason. If one is ever written, both
// move together.
//
// The protocol, in this order:
//
//  1. lock the team row. Inserting a child row takes FOR KEY SHARE on the
//     parent, and FOR UPDATE conflicts with it, so a concurrent "create the
//     first schedule" or "add a team-scoped webhook" either finishes before
//     this read or waits for this transaction to end. Both orders answer
//     deterministically; neither reaches the constraint by surprise.
//  2. read the schedule root AFTER the lock. READ COMMITTED is what makes
//     that read see a schedule that committed while this transaction waited.
//     Under REPEATABLE READ the snapshot would predate it, the row would be
//     invisible, and the refusal would come back as a raw constraint error.
//  3. delete, and let the integrations foreign key answer for itself.
//
// The schedule check is a read rather than a constraint because its answer is
// terminal: history cannot be removed, so there is no point reporting a
// removable blocker alongside it. Soft-deleted counts - the revisions are
// still there.
//
// Step 2 refuses on the EXISTENCE of a root, never on what the root contains.
// That is what makes it safe on its own: the ON DELETE RESTRICT from
// schedule_revisions defends a schedule that has a chain, but nothing proves a
// root has one, so a check that only refused "initialized" roots would be
// narrower than the cascade it guards against.
func (s *Service) DeleteTeam(ctx context.Context, teamID string) error {
	if teamID == "" {
		return invalidField("team_id", "is required")
	}
	return s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		if err := tx.LockTeam(ctx, teamID); err != nil {
			return err
		}

		root, err := tx.GetScheduleRootByTeam(ctx, teamID)
		switch {
		case errors.Is(err, ErrScheduleNotFound):
			// Never had a schedule, or never will have had one: nothing to
			// retain the team on this side.
		case err != nil:
			return err
		default:
			return &TeamHasScheduleHistoryError{ScheduleID: root.ID}
		}

		return tx.DeleteTeam(ctx, teamID)
	})
}

// holdsAssignment reports whether a user is currently assignable on a
// schedule, from both sources: the groups of the active tail revision, and any
// live override head aimed at them. An override can name someone who appears
// in no group at all, so checking the snapshot alone would miss them.
func (s *Service) holdsAssignment(ctx context.Context, tx ScheduleConfigTx,
	scheduleID, userID string) (bool, error) {

	tail, err := tx.GetTailRevision(ctx, scheduleID)
	if err != nil {
		return false, err
	}
	if tail.Kind == RevisionActive && snapshotNames(tail.Snapshot, userID) {
		return true, nil
	}

	heads, err := tx.ListOverrideHeads(ctx, scheduleID, false)
	if err != nil {
		return false, err
	}
	at := NormalizeTimestamp(s.now().UTC())
	for _, head := range heads {
		// A future override blocks too: it would otherwise become an
		// unresolvable assignment the moment it starts. An expired one does
		// not - its history stays explainable without the membership.
		if head.UserID == userID && head.ValidTo.After(at) {
			return true, nil
		}
	}
	return false, nil
}

// snapshotNames reports whether a user is in any group of any layer.
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

// checkDeletionConsistency compares the two statements of the same fact: the
// deleted_at projection on the root and the kind of the tail revision. They
// are written together, so disagreement is corruption rather than a state a
// caller could have produced.
func checkDeletionConsistency(root *ScheduleRoot, tail *ScheduleRevision) error {
	deletedChain := tail.Kind == RevisionDeleted
	deletedRoot := root.DeletedAt != nil
	if deletedChain != deletedRoot {
		return fmt.Errorf("%w: schedule %s has deleted_at=%v but a %s tail revision",
			ErrInvariantViolation, root.ID, root.DeletedAt, tail.Kind)
	}
	return nil
}

// commandUserIDs is the set a command locks: everyone it is about to put on
// call, plus whoever is issuing it.
//
// The actor belongs in the set because a command records their free text - a
// change reason, an override reason - and erasure promises to have cleared
// everything an erased person wrote. Without the lock a save can commit just
// after an erasure and leave behind text that nothing will ever clean again.
func commandUserIDs(actorID string, configured ...string) []string {
	if actorID == "" {
		return configured
	}
	for _, id := range configured {
		if id == actorID {
			return configured
		}
	}
	return append(append([]string(nil), configured...), actorID)
}

// requireActiveActor refuses a command whose author has been erased.
//
// It runs with the actor's row already locked, so the answer cannot change
// underneath. Being authorized when the request arrived is not the same as
// still existing when it writes.
func requireActiveActor(ctx context.Context, tx ScheduleConfigTx, actorID string) error {
	if actorID == "" {
		// A programmatic caller with no author. Nothing it writes carries a
		// person's name, so there is nobody for erasure to have missed.
		return nil
	}
	active, err := tx.ActiveUserIDs(ctx, []string{actorID})
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return ErrActorNotActive
	}
	return nil
}

// ConfigurationUserIDs is every user the configuration names, deduplicated in
// first-seen order. It is both the set to lock and the set to validate, and it
// is exported so the preview validates exactly the set a save would.
func ConfigurationUserIDs(c rotation.ScheduleConfiguration) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, groups := range [2][]rotation.RotationGroup{c.L1.Groups, c.L2.Groups} {
		for _, g := range groups {
			for _, m := range g.Members {
				if _, dup := seen[m]; dup {
					continue
				}
				seen[m] = struct{}{}
				out = append(out, m)
			}
		}
	}
	return out
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

// commitLog is one line about a committed change, assembled while the facts
// are at hand and written only once the transaction is known to have
// committed. Logging it inside the transaction, as this used to, claims a
// revision exists the moment before a failing COMMIT proves it does not.
type commitLog struct {
	trigger       string
	scheduleID    string
	oldRevisionID *string
	newRevisionID string
	oldVersion    int64
	newVersion    int64
	effectiveAt   time.Time
	actorID       string
	changeSummary string
}

// recordChange builds the line to log once the transaction commits.
//
// It used to also write a schedule_events row in the same transaction. That
// table had no readers: the revision it described already carries the version,
// the actor, the reason, the effective time, the snapshot and the change
// summary, and the event's payload only named ids derivable from the chain.
// A real consumer would need an outbox with a cursor, delivery and
// idempotency - not a write-only audit table kept warm in case.
func (s *Service) recordChange(trigger string,
	oldRevisionID *string, revision *ScheduleRevision, oldVersion int64,
	effectiveAt time.Time, actorID string) *commitLog {

	entry := &commitLog{
		trigger:       trigger,
		scheduleID:    revision.ScheduleID,
		oldRevisionID: oldRevisionID,
		newRevisionID: revision.ID,
		oldVersion:    oldVersion,
		newVersion:    revision.Version,
		effectiveAt:   effectiveAt,
		actorID:       actorID,
	}
	if revision.ChangeSummary != nil {
		if encoded, err := json.Marshal(revision.ChangeSummary); err == nil {
			entry.changeSummary = string(encoded)
		}
	}
	return entry
}

// logCommit writes one line per committed change.
//
// It names ids, versions and the diff summary. The snapshot is deliberately
// absent: it is already stored, it is large, and a log aggregator is not a
// place to keep a second copy of who is on call. A nil entry means the command
// wrote nothing, which is not an event.
func (s *Service) logCommit(entry *commitLog) {
	if entry == nil {
		return
	}
	s.logf("schedule_config: trigger=%s schedule_id=%s old_revision_id=%s new_revision_id=%s "+
		"old_version=%d new_version=%d effective_at=%s actor_id=%s change_summary=%s",
		entry.trigger, entry.scheduleID, derefOr(entry.oldRevisionID, "-"), entry.newRevisionID,
		entry.oldVersion, entry.newVersion, entry.effectiveAt.Format(time.RFC3339Nano),
		orDash(entry.actorID), entry.changeSummary)
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
