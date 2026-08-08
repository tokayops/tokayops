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
	repo    ScheduleConfigRepository
	now     func() time.Time
	newID   func() string
	metrics Metrics
	logf    func(format string, args ...any)

	// planTransition is the pure planner, injectable so a test can feed the
	// commit guard a plan that contradicts its own snapshot. A guard no test
	// can trip is a comment, not a guard.
	planTransition func(rotation.TransitionInput) (rotation.TransitionPlan, error)
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

// WithMetrics sends the command-side counters somewhere. Without it they go
// nowhere, which is what keeps unit tests free of a metrics registry.
func WithMetrics(m Metrics) Option {
	return func(s *Service) {
		if m != nil {
			s.metrics = m
		}
	}
}

// WithLogger overrides the commit log sink.
func WithLogger(logf func(format string, args ...any)) Option {
	return func(s *Service) { s.logf = logf }
}

// NewService builds a Service over a repository.
func NewService(repo ScheduleConfigRepository, opts ...Option) *Service {
	s := &Service{
		repo:           repo,
		now:            func() time.Time { return time.Now().UTC() },
		newID:          func() string { return uuid.New().String() },
		metrics:        NopMetrics{},
		logf:           log.Printf,
		planTransition: rotation.PlanTransition,
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
	if result.Noop {
		s.metrics.TransitionNoop()
	} else {
		s.metrics.RevisionCreated(triggerOf(result))
	}
	return result, nil
}

// runCommand is the one place a configuration command opens a transaction and
// reports on it. Every command times the same span - lock wait included - and
// counts a conflict the same way, so there is no path where one of them
// silently stops being measured.
func (s *Service) runCommand(ctx context.Context, fn func(ScheduleConfigTx) error) error {
	started := s.now()
	err := s.repo.WithinTx(ctx, fn)
	s.metrics.TransitionDuration(s.now().Sub(started))
	if errors.Is(err, ErrVersionConflict) {
		s.metrics.TransitionConflict()
	}
	return err
}

func triggerOf(res *SaveResult) string {
	switch {
	case res.Created:
		return TriggerCreated
	case res.Recreated:
		return TriggerRecreated
	default:
		return TriggerSaved
	}
}

func (s *Service) save(ctx context.Context, tx ScheduleConfigTx, teamID string,
	desired rotation.ScheduleConfiguration, cmd SaveCommand) (*SaveResult, error) {

	// Users before schedules, always. Erasure locks the user it is erasing and
	// then scans schedules; a command that locked the schedule first and the
	// users second would be the other half of an AB-BA deadlock.
	if err := tx.LockUsers(ctx, commandUserIDs(cmd.ActorID, ConfigurationUserIDs(desired))); err != nil {
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
// refuse a pre-revision row, take the row lock, check the caller is not stale,
// read the revision in force and check it agrees with the deleted projection.
//
// Effective time is captured last, and that ordering is the point of the
// function: the wait for the lock can be long, and an instant read before it
// would place the change in a slot that has already passed.
func (s *Service) lockForWrite(ctx context.Context, tx ScheduleConfigTx,
	root *ScheduleRoot, expectedVersion int64) (*writeTarget, error) {

	if err := s.refuseLegacyRoot(ctx, tx, root); err != nil {
		return nil, err
	}
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

	plan, err := s.planTransition(rotation.TransitionInput{
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

	before, after, err := s.activeGroupsAround(current, plan, target.effectiveAt)
	if err != nil {
		return nil, err
	}
	if err := s.guardTransition(plan, current != nil, before, after, target.effectiveAt); err != nil {
		return nil, err
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
	commit, err := s.appendRevision(ctx, tx, target, revision, recreate, trigger, cmd.ActorID, before, after)
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
	revision *ScheduleRevision, clearDeleted bool, trigger, actorID string,
	before, after activeGroupPair) (*commitLog, error) {

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
	return s.recordChange(ctx, tx, trigger, &target.tail.ID, revision,
		target.root.ConfigVersion, target.effectiveAt, actorID, before, after)
}

// CreateSchedule creates a schedule and its first revision in one transaction.
//
// It is the programmatic entry point - tests and tooling - while the editor
// reaches the same code through Save. Both end in initialize, so there is
// exactly one definition of what a freshly created schedule is.
//
// It does NOT delegate to Save. A caller that asks to create says nothing
// about versions, so "the team already has a schedule" is the answer it needs;
// Save reports the same fact as a version conflict carrying the current
// version, which is what an editor holding a stale form needs. Deriving one
// from the other would mean reading a version number back out of an error to
// guess which situation it described.
//
// A concurrent create for the same team yields ErrScheduleExists too, from the
// unique constraint on team_id.
func (s *Service) CreateSchedule(ctx context.Context, teamID string,
	config rotation.ScheduleConfiguration, actorID string, reason *string) (*ScheduleRevision, error) {

	if teamID == "" {
		return nil, invalidField("team_id", "is required")
	}
	desired, err := NormalizeConfiguration(config)
	if err != nil {
		return nil, err
	}

	var created *SaveResult
	err = s.runCommand(ctx, func(tx ScheduleConfigTx) error {
		if err := tx.LockUsers(ctx, commandUserIDs(actorID, ConfigurationUserIDs(desired))); err != nil {
			return err
		}
		if err := requireActiveActor(ctx, tx, actorID); err != nil {
			return err
		}
		switch _, err := tx.GetScheduleRootByTeam(ctx, teamID); {
		case err == nil:
			return ErrScheduleExists
		case !errors.Is(err, ErrScheduleNotFound):
			return err
		}
		created, err = s.initialize(ctx, tx, teamID, desired, actorID, reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.logCommit(created.commit)
	s.metrics.RevisionCreated(TriggerCreated)
	return created.Revision, nil
}

// initialize writes the root, revision 1 and the creation event.
func (s *Service) initialize(ctx context.Context, tx ScheduleConfigTx, teamID string,
	desired rotation.ScheduleConfiguration, actorID string, reason *string) (*SaveResult, error) {

	// No schedule row exists yet, so there is nothing to lock and no tail
	// revision to stay ahead of: effective time is simply now.
	effectiveAt := NormalizeTimestamp(s.now().UTC())

	if err := ValidateMembership(ctx, tx, teamID, ConfigurationUserIDs(desired)); err != nil {
		return nil, err
	}

	plan, err := s.planTransition(rotation.TransitionInput{
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
	before, after, err := s.activeGroupsAround(nil, plan, effectiveAt)
	if err != nil {
		return nil, err
	}
	if err := s.guardTransition(plan, false, before, after, effectiveAt); err != nil {
		return nil, err
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
	commit, err := s.recordChange(ctx, tx, TriggerCreated, nil, revision, 0, effectiveAt, actorID, before, after)
	if err != nil {
		return nil, err
	}

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
	s.metrics.RevisionCreated(TriggerDeleted)
	return nil
}

func (s *Service) delete(ctx context.Context, tx ScheduleConfigTx, teamID string, cmd DeleteCommand) (*commitLog, error) {
	// The deleted revision records the actor and their reason, so the actor is
	// locked and checked here exactly as in a save.
	if err := tx.LockUsers(ctx, commandUserIDs(cmd.ActorID, nil)); err != nil {
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

	before, err := activeGroups(&target.tail.Snapshot, target.effectiveAt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
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
	if err := s.tombstoneLiveOverrides(ctx, tx, target.root.ID, target.effectiveAt, cmd.ActorID); err != nil {
		return nil, err
	}
	if err := tx.SetScheduleDeleted(ctx, target.root.ID, &target.effectiveAt); err != nil {
		return nil, err
	}
	if err := tx.AdvanceVersion(ctx, target.root.ID, target.root.ConfigVersion, target.effectiveAt); err != nil {
		return nil, err
	}
	// Nobody is on duty after a delete, so the "after" pair is empty by
	// construction rather than computed.
	return s.recordChange(ctx, tx, TriggerDeleted, &target.tail.ID, deleted,
		target.root.ConfigVersion, target.effectiveAt, cmd.ActorID, before, activeGroupPair{})
}

// tombstoneLiveOverrides appends a delete revision to every override that is
// still alive. They share one recordedAt: the monotonicity rule applies to a
// command, not to each row it writes, and the next override command will still
// land above all of them.
func (s *Service) tombstoneLiveOverrides(ctx context.Context, tx ScheduleConfigTx,
	scheduleID string, at time.Time, actorID string) error {

	heads, err := tx.ListOverrideHeads(ctx, scheduleID, false)
	if err != nil {
		return err
	}
	if len(heads) == 0 {
		return nil
	}
	recordedAt, err := s.nextOverrideRecordedAt(ctx, tx, scheduleID, at)
	if err != nil {
		return err
	}
	for _, head := range heads {
		tombstone := head
		tombstone.RevisionID = s.newID()
		tombstone.Revision = head.Revision + 1
		tombstone.Deleted = true
		tombstone.RecordedAt = recordedAt
		tombstone.RecordedBy = optionalString(actorID)
		if err := tx.InsertOverrideRevision(ctx, &tombstone); err != nil {
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
// against Save on the schedule row lock. That is why Save re-reads membership
// after taking that lock.
func (s *Service) RemoveTeamMember(ctx context.Context, teamID, userID string) error {
	if teamID == "" || userID == "" {
		return invalidField("team_id", "and user_id are required")
	}
	return s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		root, err := tx.GetScheduleRootByTeam(ctx, teamID)
		if errors.Is(err, ErrScheduleNotFound) {
			// No schedule means no assignment to protect.
			return tx.DeleteTeamMembership(ctx, teamID, userID)
		}
		if err != nil {
			return err
		}
		// A legacy row keeps legacy semantics until the cutover removes it:
		// its rotation lives in tables this package does not read, so a guard
		// here could only be a guess.
		if IsLegacyRoot(root) {
			return tx.DeleteTeamMembership(ctx, teamID, userID)
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

// refuseLegacyRoot rejects a schedule row left over from before the revision
// model: config_version 0 and no revision chain.
//
// Grafting revisions onto it would produce a hybrid the legacy readers still
// present as if it were theirs. The symmetry is deliberate: the new path
// refuses old data, and the old path refuses new data.
func (s *Service) refuseLegacyRoot(ctx context.Context, tx ScheduleConfigTx, root *ScheduleRoot) error {
	if !IsLegacyRoot(root) {
		return nil
	}
	_, err := tx.GetTailRevision(ctx, root.ID)
	if errors.Is(err, ErrRevisionNotFound) {
		return ErrLegacySchedule
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: schedule %s has revisions at config_version 0", ErrInvariantViolation, root.ID)
}

// IsLegacyRoot reports whether a schedule row predates the revision model.
//
// config_version is the discriminator because it is monotonic: the create flow
// writes 1 with the first revision and nothing ever lowers it, so zero can
// only mean "no revision was ever written for this row". Read-side callers
// that have no transaction - the preview - use this directly; the command side
// additionally proves the chain really is empty before saying so.
func IsLegacyRoot(root *ScheduleRoot) bool {
	return root != nil && root.ConfigVersion == 0
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
func commandUserIDs(actorID string, configured []string) []string {
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
func requireActiveActor(ctx context.Context, view ScheduleReadView, actorID string) error {
	if actorID == "" {
		// A programmatic caller with no author. Nothing it writes carries a
		// person's name, so there is nobody for erasure to have missed.
		return nil
	}
	active, err := view.ActiveUserIDs(ctx, []string{actorID})
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

// activeGroupPair is the group on duty in each layer at one instant; an empty
// string means the layer produces no assignment.
type activeGroupPair struct {
	L1 string
	L2 string
}

func activeGroups(snap *rotation.ScheduleRevisionSnapshot, at time.Time) (activeGroupPair, error) {
	var out activeGroupPair
	if snap == nil {
		return out, nil
	}
	var err error
	if out.L1, err = activeGroupID(snap.Timezone, snap.L1, at); err != nil {
		return activeGroupPair{}, fmt.Errorf("l1: %w", err)
	}
	if out.L2, err = activeGroupID(snap.Timezone, snap.L2, at); err != nil {
		return activeGroupPair{}, fmt.Errorf("l2: %w", err)
	}
	return out, nil
}

func activeGroupID(timezone string, layer rotation.RotationLayerSnapshot, at time.Time) (string, error) {
	grid, err := rotation.NewGrid(timezone, layer.Policy)
	if err != nil {
		return "", err
	}
	group, _, ok, err := rotation.ActiveGroupAt(grid, layer, at)
	if err != nil || !ok {
		return "", err
	}
	return group.ID, nil
}

// activeGroupsAround resolves who the old and the new snapshot each put on
// duty at the transition instant.
//
// It is separate from the guard because the answer has two consumers: the
// guard checks it, and the commit log records it. Folding the computation into
// the guard made a validator that also returned data, which is how a "guard"
// ends up being called for its side effects.
func (s *Service) activeGroupsAround(current *rotation.ScheduleRevisionSnapshot,
	plan rotation.TransitionPlan, at time.Time) (before, after activeGroupPair, err error) {

	if before, err = activeGroups(current, at); err != nil {
		return activeGroupPair{}, activeGroupPair{}, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	if after, err = activeGroups(&plan.Snapshot, at); err != nil {
		return activeGroupPair{}, activeGroupPair{}, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	return before, after, nil
}

// guardTransition is the commit post-condition: the rotation the planner
// promised has to be the rotation the snapshot it produced actually yields.
//
// The expected active group is checked ALWAYS. Equality of the old and the new
// active group is checked only when the plan claims to preserve it - a
// successor or first selection legitimately changes who is on duty, and
// demanding equality there would roll back valid edits.
//
// It reads nothing and computes nothing: given a plan and the two answers, it
// either agrees or it does not.
func (s *Service) guardTransition(plan rotation.TransitionPlan, hadCurrent bool,
	before, after activeGroupPair, at time.Time) error {

	layers := [2]struct {
		name       string
		transition rotation.LayerTransition
		before     string
		after      string
	}{
		{"l1", plan.L1, before.L1, after.L1},
		{"l2", plan.L2, before.L2, after.L2},
	}
	for _, l := range layers {
		expected := ""
		if l.transition.ExpectedActiveGroupID != nil {
			expected = *l.transition.ExpectedActiveGroupID
		}
		if l.after != expected {
			s.metrics.GuardViolation()
			return fmt.Errorf("%w: %s plan expects active group %q at %v, snapshot yields %q",
				ErrInvariantViolation, l.name, expected, at, l.after)
		}
		if l.transition.PreservesActiveGroup && hadCurrent && l.before != l.after {
			s.metrics.GuardViolation()
			return fmt.Errorf("%w: %s claims to preserve the active group but %q became %q",
				ErrInvariantViolation, l.name, l.before, l.after)
		}
	}
	return nil
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
	before        activeGroupPair
	after         activeGroupPair
}

// recordChange writes the domain event in the same transaction as the revision
// it describes, and returns the line to log after the commit.
func (s *Service) recordChange(ctx context.Context, tx ScheduleConfigTx, trigger string,
	oldRevisionID *string, revision *ScheduleRevision, oldVersion int64,
	effectiveAt time.Time, actorID string, before, after activeGroupPair) (*commitLog, error) {

	payload, err := json.Marshal(ConfigurationChangedPayload{
		Trigger:       trigger,
		OldRevisionID: oldRevisionID,
		NewRevisionID: revision.ID,
		OldVersion:    oldVersion,
		NewVersion:    revision.Version,
		EffectiveAt:   effectiveAt,
		ActorID:       optionalString(actorID),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	if err := tx.InsertScheduleEvent(ctx, &ScheduleEvent{
		ScheduleID: revision.ScheduleID,
		EventType:  EventScheduleConfigurationChanged,
		Payload:    payload,
		RecordedAt: effectiveAt,
	}); err != nil {
		return nil, err
	}

	entry := &commitLog{
		trigger:       trigger,
		scheduleID:    revision.ScheduleID,
		oldRevisionID: oldRevisionID,
		newRevisionID: revision.ID,
		oldVersion:    oldVersion,
		newVersion:    revision.Version,
		effectiveAt:   effectiveAt,
		actorID:       actorID,
		before:        before,
		after:         after,
	}
	if revision.ChangeSummary != nil {
		if encoded, err := json.Marshal(revision.ChangeSummary); err == nil {
			entry.changeSummary = string(encoded)
		}
	}
	return entry, nil
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
		"old_version=%d new_version=%d effective_at=%s actor_id=%s change_summary=%s "+
		"active_l1_before=%s active_l1_after=%s active_l2_before=%s active_l2_after=%s",
		entry.trigger, entry.scheduleID, derefOr(entry.oldRevisionID, "-"), entry.newRevisionID,
		entry.oldVersion, entry.newVersion, entry.effectiveAt.Format(time.RFC3339Nano),
		orDash(entry.actorID), entry.changeSummary,
		orDash(entry.before.L1), orDash(entry.after.L1),
		orDash(entry.before.L2), orDash(entry.after.L2))
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
