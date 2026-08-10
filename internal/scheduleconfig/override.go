package scheduleconfig

import (
	"context"
	"errors"
	"time"
)

// OverrideCommand is one create or update of a logical override.
type OverrideCommand struct {
	UserID    string
	ValidFrom time.Time
	ValidTo   time.Time
	Reason    *string
	ActorID   string
}

// CreateOverride appends revision 1 of a new logical override.
func (s *Service) CreateOverride(ctx context.Context, teamID string, cmd OverrideCommand) (*OverrideRevision, error) {
	if cmd.UserID == "" {
		return nil, invalidField("user_id", "is required")
	}
	var created *OverrideRevision
	err := s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		// Users before schedules, as in Save: the target of an override is an
		// assignment, and the actor's reason text is theirs to have erased.
		if err := tx.LockUsers(ctx, commandUserIDs(cmd.ActorID, cmd.UserID)); err != nil {
			return err
		}
		if err := requireActiveActor(ctx, tx, cmd.ActorID); err != nil {
			return err
		}
		root, err := tx.GetScheduleRootByTeam(ctx, teamID)
		if err != nil {
			return err
		}
		locked, recordedAt, err := s.lockForOverride(ctx, tx, root.ID)
		if err != nil {
			return err
		}
		if err := s.validateOverrideTarget(ctx, tx, locked.TeamID, cmd); err != nil {
			return err
		}
		// Layer l1 is hard-coded: the first release of the API creates primary
		// overrides only. The column and the renderer already carry the layer,
		// so admitting l2 later needs no migration.
		rev := &OverrideRevision{
			RevisionID: s.newID(),
			OverrideID: s.newID(),
			ScheduleID: locked.ID,
			Revision:   1,
			Layer:      LayerL1,
			UserID:     cmd.UserID,
			ValidFrom:  NormalizeTimestamp(cmd.ValidFrom),
			ValidTo:    NormalizeTimestamp(cmd.ValidTo),
			Reason:     cmd.Reason,
			RecordedAt: recordedAt,
			RecordedBy: optionalString(cmd.ActorID),
		}
		if err := s.checkOverrideOverlap(ctx, tx, rev, ""); err != nil {
			return err
		}
		if err := tx.InsertOverrideRevision(ctx, rev); err != nil {
			return err
		}
		created = rev
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateOverride appends the next revision of an existing override.
func (s *Service) UpdateOverride(ctx context.Context, scheduleID, overrideID string,
	expectedRevision int64, cmd OverrideCommand) (*OverrideRevision, error) {

	if cmd.UserID == "" {
		return nil, invalidField("user_id", "is required")
	}
	var updated *OverrideRevision
	err := s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		if err := tx.LockUsers(ctx, commandUserIDs(cmd.ActorID, cmd.UserID)); err != nil {
			return err
		}
		if err := requireActiveActor(ctx, tx, cmd.ActorID); err != nil {
			return err
		}
		locked, recordedAt, err := s.lockForOverride(ctx, tx, scheduleID)
		if err != nil {
			return err
		}
		head, err := liveOverrideHead(ctx, tx, scheduleID, overrideID, expectedRevision)
		if err != nil {
			return err
		}
		if err := s.validateOverrideTarget(ctx, tx, locked.TeamID, cmd); err != nil {
			return err
		}

		rev := &OverrideRevision{
			RevisionID: s.newID(),
			OverrideID: overrideID,
			ScheduleID: scheduleID,
			Revision:   head.Revision + 1,
			Layer:      head.Layer,
			UserID:     cmd.UserID,
			ValidFrom:  NormalizeTimestamp(cmd.ValidFrom),
			ValidTo:    NormalizeTimestamp(cmd.ValidTo),
			Reason:     cmd.Reason,
			RecordedAt: recordedAt,
			RecordedBy: optionalString(cmd.ActorID),
		}
		// Excluding itself is not an optimization: the head is part of the
		// current projection, so without it every update of an override would
		// collide with the version it replaces.
		if err := s.checkOverrideOverlap(ctx, tx, rev, overrideID); err != nil {
			return err
		}
		if err := tx.InsertOverrideRevision(ctx, rev); err != nil {
			return err
		}
		updated = rev
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// DeleteOverride appends a tombstone. Nothing is removed: the override's
// history stays readable, and an as-of query still shows it live up to this
// moment.
func (s *Service) DeleteOverride(ctx context.Context, scheduleID, overrideID string,
	expectedRevision int64, actorID string) error {

	return s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		// A delete adds no assignment, but it does record who did it and copies
		// the reason forward, so the actor is still locked and checked.
		if err := tx.LockUsers(ctx, commandUserIDs(actorID)); err != nil {
			return err
		}
		if err := requireActiveActor(ctx, tx, actorID); err != nil {
			return err
		}
		_, recordedAt, err := s.lockForOverride(ctx, tx, scheduleID)
		if err != nil {
			return err
		}
		head, err := liveOverrideHead(ctx, tx, scheduleID, overrideID, expectedRevision)
		if err != nil {
			return err
		}
		tombstone := *head
		tombstone.RevisionID = s.newID()
		tombstone.Revision = head.Revision + 1
		tombstone.Deleted = true
		tombstone.RecordedAt = recordedAt
		tombstone.RecordedBy = optionalString(actorID)
		return tx.InsertOverrideRevision(ctx, &tombstone)
	})
}

// lockForOverride takes the schedule row lock every override command shares
// with Save, refuses the states in which an override is meaningless, and only
// then captures the recorded time.
func (s *Service) lockForOverride(ctx context.Context, tx ScheduleConfigTx, scheduleID string) (*ScheduleRoot, time.Time, error) {
	locked, err := tx.LockSchedule(ctx, scheduleID)
	if err != nil {
		return nil, time.Time{}, err
	}
	// An override command never reads the revision chain, so without this the
	// one uninitialized row in the database would be the one place a write
	// succeeded: an override revision appended to a schedule that has none.
	if err := requireInitializedRoot(locked); err != nil {
		return nil, time.Time{}, err
	}
	// An inactive schedule has nobody on duty, so there is nobody to stand in
	// for. Allowing it would also let an override outlive the delete that was
	// supposed to end it.
	if locked.DeletedAt != nil {
		return nil, time.Time{}, ErrScheduleDeleted
	}
	recordedAt, err := s.nextOverrideRecordedAt(ctx, tx, scheduleID, s.now().UTC())
	if err != nil {
		return nil, time.Time{}, err
	}
	return locked, recordedAt, nil
}

// nextOverrideRecordedAt is the monotonicity rule for override system time:
//
//	max(now, max(recorded_at of every override revision) + 1 resolution unit)
//
// It is the same shape as NextEffectiveAt and for the same reason. As-of
// queries resolve an override by recorded_at, so two revisions sharing an
// instant - a clock stepping back, or two commands inside one microsecond -
// would make "what did we know then" ambiguous.
//
// It is computed here rather than inside the insert statement so that it sits
// next to the injected clock and can be tested with the clock wound backwards.
func (s *Service) nextOverrideRecordedAt(ctx context.Context, tx ScheduleConfigTx,
	scheduleID string, now time.Time) (time.Time, error) {

	candidate := NormalizeTimestamp(now)
	max, err := tx.MaxOverrideRecordedAt(ctx, scheduleID)
	if err != nil {
		return time.Time{}, err
	}
	if max != nil {
		floor := NormalizeTimestamp(*max).Add(TimestampResolution)
		if candidate.Before(floor) {
			candidate = floor
		}
	}
	return candidate, nil
}

// liveOverrideHead resolves the override an update or a delete names and
// checks the caller is not working from a stale version.
//
// The head is read INCLUDING tombstones. A deleted override has to answer "not
// found" rather than "no such override": read without tombstones, an update of
// a deleted ID would look like a create and start numbering at 1 again, which
// the unique constraint on (override_id, revision) would then reject with a
// message about nothing the user did.
func liveOverrideHead(ctx context.Context, view ScheduleReadView, scheduleID, overrideID string,
	expectedRevision int64) (*OverrideRevision, error) {

	head, err := view.GetOverrideHead(ctx, scheduleID, overrideID)
	if errors.Is(err, ErrOverrideNotFound) {
		return nil, ErrOverrideNotFound
	}
	if err != nil {
		return nil, err
	}
	if head.Deleted {
		return nil, ErrOverrideNotFound
	}
	if head.Revision != expectedRevision {
		return nil, &OverrideRevisionConflictError{Expected: expectedRevision, Current: head.Revision}
	}
	return head, nil
}

// validateOverrideTarget checks the interval and that the target is still an
// active member of the owning team, read fresh under the schedule lock.
func (s *Service) validateOverrideTarget(ctx context.Context, view ScheduleReadView,
	teamID string, cmd OverrideCommand) error {

	// Normalized first: a difference below database resolution is not a
	// difference once stored, so an interval that looks non-empty in
	// nanoseconds can still be rejected by the CHECK constraint.
	from := NormalizeTimestamp(cmd.ValidFrom)
	until := NormalizeTimestamp(cmd.ValidTo)
	if from.IsZero() {
		return invalidField("valid_from", "is required")
	}
	if !until.After(from) {
		return invalidField("valid_to", "must be after valid_from")
	}
	// A valid_from in the past is allowed. The record is append-only and
	// as-of queries preserve what was known when, so a retroactive correction
	// stays explainable instead of rewriting history.

	return ValidateMembership(ctx, view, teamID, []string{cmd.UserID})
}

// checkOverrideOverlap rejects an override that would cover an instant another
// override of the same layer already covers.
//
// The check runs against the CURRENT projection under the schedule lock, which
// is the only place it can run: overlap is a property of the latest revision
// per override_id with tombstones removed, and no per-row database constraint
// can express that.
func (s *Service) checkOverrideOverlap(ctx context.Context, view ScheduleReadView,
	rev *OverrideRevision, excludeOverrideID string) error {

	existing, err := view.GetOverrideProjectionInRange(ctx, rev.ScheduleID, &rev.ValidFrom, &rev.ValidTo, nil)
	if err != nil {
		return err
	}
	var conflicts []OverrideRef
	for _, other := range existing {
		if other.Layer != rev.Layer || other.OverrideID == excludeOverrideID {
			continue
		}
		conflicts = append(conflicts, OverrideRef{
			OverrideID: other.OverrideID,
			UserID:     other.UserID,
			ValidFrom:  other.ValidFrom,
			ValidTo:    other.ValidTo,
		})
	}
	if len(conflicts) > 0 {
		return &OverrideOverlapError{Conflicts: conflicts}
	}
	return nil
}
