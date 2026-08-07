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

	started := s.now()
	var result *SaveResult
	err = s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		res, err := s.save(ctx, tx, teamID, desired, cmd)
		if err != nil {
			return err
		}
		result = res
		return nil
	})
	s.metrics.TransitionDuration(s.now().Sub(started))
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			s.metrics.TransitionConflict()
		}
		return nil, err
	}
	if result.Noop {
		s.metrics.TransitionNoop()
	} else {
		s.metrics.RevisionCreated(triggerOf(result))
	}
	return result, nil
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
	if err := tx.LockUsers(ctx, ConfigurationUserIDs(desired)); err != nil {
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
	if err := s.refuseLegacyRoot(ctx, tx, root); err != nil {
		return nil, err
	}

	locked, err := tx.LockSchedule(ctx, root.ID)
	if err != nil {
		return nil, err
	}
	if locked.ConfigVersion != cmd.ExpectedVersion {
		return nil, &VersionConflictError{Expected: cmd.ExpectedVersion, Current: locked.ConfigVersion}
	}

	tail, err := tx.GetTailRevision(ctx, locked.ID)
	if err != nil {
		return nil, err
	}
	if err := checkDeletionConsistency(locked, tail); err != nil {
		return nil, err
	}
	// Only now: the wait for the lock may have been long, and an instant
	// captured before it would place the change in a slot that has passed.
	effectiveAt := NextEffectiveAt(&tail.EffectiveFrom, s.now().UTC())

	// A deleted schedule is recreated, not edited. Its tail snapshot is a copy
	// kept so the column stays decodable - it is not a configuration in force,
	// so the planner is given no current state at all and starts the rotation
	// from the first group.
	current := &tail.Snapshot
	res := &SaveResult{}
	if locked.DeletedAt != nil {
		current = nil
		res.Recreated = true
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
		EffectiveAt: effectiveAt,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	if plan.Noop {
		// An early return, not an error: the empty transaction commits.
		// Rolling back would read as a failure in every log and metric that
		// watches this path, and nothing happened that needs undoing.
		res.Noop = true
		res.Revision = tail
		res.Version = locked.ConfigVersion
		res.EffectiveAt = effectiveAt
		return res, nil
	}

	before, after, err := s.guardTransition(current, plan, effectiveAt)
	if err != nil {
		return nil, err
	}

	summary := plan.Change
	revision := &ScheduleRevision{
		ID:            s.newID(),
		ScheduleID:    locked.ID,
		Version:       locked.ConfigVersion + 1,
		Kind:          RevisionActive,
		Snapshot:      plan.Snapshot,
		EffectiveFrom: effectiveAt,
		RecordedAt:    effectiveAt,
		CreatedBy:     optionalString(cmd.ActorID),
		ChangeReason:  cmd.Reason,
		ChangeSummary: &summary,
	}

	if err := tx.CloseRevision(ctx, locked.ID, tail.ID, effectiveAt); err != nil {
		return nil, err
	}
	if err := tx.InsertRevision(ctx, revision); err != nil {
		return nil, err
	}
	if res.Recreated {
		if err := tx.SetScheduleDeleted(ctx, locked.ID, nil); err != nil {
			return nil, err
		}
	}
	if err := tx.AdvanceVersion(ctx, locked.ID, locked.ConfigVersion, effectiveAt); err != nil {
		return nil, err
	}
	trigger := TriggerSaved
	if res.Recreated {
		trigger = TriggerRecreated
	}
	if err := s.recordChange(ctx, tx, trigger, &tail.ID, revision,
		locked.ConfigVersion, effectiveAt, cmd.ActorID, before, after); err != nil {
		return nil, err
	}

	res.Revision = revision
	res.Version = revision.Version
	res.EffectiveAt = effectiveAt
	return res, nil
}

// CreateSchedule creates a schedule and its first revision in one transaction.
//
// It is the programmatic entry point - tests and future tooling - while the
// editor reaches the same code through Save. Both end up in initialize, so
// there is exactly one definition of what a freshly created schedule is.
//
// A concurrent create for the same team yields ErrScheduleExists.
func (s *Service) CreateSchedule(ctx context.Context, teamID string,
	config rotation.ScheduleConfiguration, actorID string, reason *string) (*ScheduleRevision, error) {

	res, err := s.Save(ctx, teamID, SaveCommand{
		ExpectedVersion: 0,
		Desired:         config,
		ActorID:         actorID,
		Reason:          reason,
	})
	// Save reports "the team already has a schedule at version N" as a version
	// conflict, which is right for an editor holding a stale form. A caller
	// that asked to create says nothing about versions, so the same fact is
	// reported as what it means to them.
	var conflict *VersionConflictError
	if errors.As(err, &conflict) && conflict.Expected == 0 {
		return nil, ErrScheduleExists
	}
	if err != nil {
		return nil, err
	}
	return res.Revision, nil
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
	before, after, err := s.guardTransition(nil, plan, effectiveAt)
	if err != nil {
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
	if err := s.recordChange(ctx, tx, TriggerCreated, nil, revision, 0, effectiveAt, actorID, before, after); err != nil {
		return nil, err
	}

	return &SaveResult{
		Revision:    revision,
		Version:     revision.Version,
		EffectiveAt: effectiveAt,
		Created:     true,
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

	started := s.now()
	err := s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		return s.delete(ctx, tx, teamID, cmd)
	})
	s.metrics.TransitionDuration(s.now().Sub(started))
	if err != nil {
		if errors.Is(err, ErrVersionConflict) {
			s.metrics.TransitionConflict()
		}
		return err
	}
	s.metrics.RevisionCreated(TriggerDeleted)
	return nil
}

func (s *Service) delete(ctx context.Context, tx ScheduleConfigTx, teamID string, cmd DeleteCommand) error {
	root, err := tx.GetScheduleRootByTeam(ctx, teamID)
	if err != nil {
		return err
	}
	if err := s.refuseLegacyRoot(ctx, tx, root); err != nil {
		return err
	}

	locked, err := tx.LockSchedule(ctx, root.ID)
	if err != nil {
		return err
	}
	if locked.ConfigVersion != cmd.ExpectedVersion {
		return &VersionConflictError{Expected: cmd.ExpectedVersion, Current: locked.ConfigVersion}
	}
	if locked.DeletedAt != nil {
		return ErrScheduleDeleted
	}

	tail, err := tx.GetTailRevision(ctx, locked.ID)
	if err != nil {
		return err
	}
	if err := checkDeletionConsistency(locked, tail); err != nil {
		return err
	}
	effectiveAt := NextEffectiveAt(&tail.EffectiveFrom, s.now().UTC())

	before, err := activeGroups(&tail.Snapshot, effectiveAt)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}

	deleted := &ScheduleRevision{
		ID:            s.newID(),
		ScheduleID:    locked.ID,
		Version:       locked.ConfigVersion + 1,
		Kind:          RevisionDeleted,
		Snapshot:      tail.Snapshot,
		EffectiveFrom: effectiveAt,
		RecordedAt:    effectiveAt,
		CreatedBy:     optionalString(cmd.ActorID),
		ChangeReason:  cmd.Reason,
	}

	if err := tx.CloseRevision(ctx, locked.ID, tail.ID, effectiveAt); err != nil {
		return err
	}
	if err := tx.InsertRevision(ctx, deleted); err != nil {
		return err
	}
	if err := s.tombstoneLiveOverrides(ctx, tx, locked.ID, effectiveAt, cmd.ActorID); err != nil {
		return err
	}
	if err := tx.SetScheduleDeleted(ctx, locked.ID, &effectiveAt); err != nil {
		return err
	}
	if err := tx.AdvanceVersion(ctx, locked.ID, locked.ConfigVersion, effectiveAt); err != nil {
		return err
	}
	return s.recordChange(ctx, tx, TriggerDeleted, &tail.ID, deleted,
		locked.ConfigVersion, effectiveAt, cmd.ActorID, before, activeGroupPair{})
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

// guardTransition is the commit post-condition: the rotation the planner
// promised has to be the rotation the snapshot it produced actually yields.
//
// The expected active group is checked ALWAYS. Equality of the old and the new
// active group is checked only when the plan claims to preserve it - a
// successor or first selection legitimately changes who is on duty, and
// demanding equality there would roll back valid edits.
func (s *Service) guardTransition(current *rotation.ScheduleRevisionSnapshot,
	plan rotation.TransitionPlan, at time.Time) (before, after activeGroupPair, err error) {

	if before, err = activeGroups(current, at); err != nil {
		return activeGroupPair{}, activeGroupPair{}, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	if after, err = activeGroups(&plan.Snapshot, at); err != nil {
		return activeGroupPair{}, activeGroupPair{}, fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}

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
			return activeGroupPair{}, activeGroupPair{}, fmt.Errorf(
				"%w: %s plan expects active group %q at %v, snapshot yields %q",
				ErrInvariantViolation, l.name, expected, at, l.after)
		}
		if l.transition.PreservesActiveGroup && current != nil && l.before != l.after {
			s.metrics.GuardViolation()
			return activeGroupPair{}, activeGroupPair{}, fmt.Errorf(
				"%w: %s claims to preserve the active group but %q became %q",
				ErrInvariantViolation, l.name, l.before, l.after)
		}
	}
	return before, after, nil
}

// recordChange writes the domain event in the same transaction as the revision
// it describes, then logs the commit.
//
// The log line names IDs, versions and the diff summary only. The snapshot
// itself is deliberately absent: it is already stored, it is large, and a log
// aggregator is not a place to keep a second copy of who is on call.
func (s *Service) recordChange(ctx context.Context, tx ScheduleConfigTx, trigger string,
	oldRevisionID *string, revision *ScheduleRevision, oldVersion int64,
	effectiveAt time.Time, actorID string, before, after activeGroupPair) error {

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
		return fmt.Errorf("%w: %v", ErrInvariantViolation, err)
	}
	if err := tx.InsertScheduleEvent(ctx, &ScheduleEvent{
		ScheduleID: revision.ScheduleID,
		EventType:  EventScheduleConfigurationChanged,
		Payload:    payload,
		RecordedAt: effectiveAt,
	}); err != nil {
		return err
	}

	summary := ""
	if revision.ChangeSummary != nil {
		if encoded, err := json.Marshal(revision.ChangeSummary); err == nil {
			summary = string(encoded)
		}
	}
	s.logf("schedule_config: trigger=%s schedule_id=%s old_revision_id=%s new_revision_id=%s "+
		"old_version=%d new_version=%d effective_at=%s actor_id=%s change_summary=%s "+
		"active_l1_before=%s active_l1_after=%s active_l2_before=%s active_l2_after=%s",
		trigger, revision.ScheduleID, derefOr(oldRevisionID, "-"), revision.ID,
		oldVersion, revision.Version, effectiveAt.Format(time.RFC3339Nano), orDash(actorID), summary,
		orDash(before.L1), orDash(after.L1), orDash(before.L2), orDash(after.L2))
	return nil
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
