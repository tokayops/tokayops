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

// UpdateOverride changes an override, and splits it when part of it has
// already been served.
//
// A future override is simply replaced: nothing has happened yet, so the next
// revision can say whatever the caller asked for, including a window that
// reaches into the past - recording that somebody covered yesterday is adding
// a fact, not erasing one.
//
// An override that is IN FORCE cannot be replaced that way. Revision N+1 would
// win the whole projection, so swapping the person at 14:00 would rewrite the
// four hours the first person had already covered. Instead:
//
//  1. the current override is truncated to now, exactly as a cancel would;
//  2. a NEW override carries the change from now to the requested end.
//
// The identity therefore changes, and that is the honest answer rather than an
// implementation leak: what carol covered until 14:00 and what dave covers
// after it are two assignments, and the audit reads as one ending where the
// other begins. The caller is told which one is live now - the returned
// revision names the new override.
//
// valid_from is not editable on the part that has been served: the new segment
// starts at now, and a valid_from the caller sent is ignored. valid_to at or
// before now is refused rather than obeyed - ending it now is what cancel does,
// and ending it earlier would erase duty somebody served.
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

		if !recordedAt.Before(head.ValidTo) {
			return &OverrideAlreadyEndedError{
				OverrideID: overrideID,
				ValidFrom:  head.ValidFrom,
				ValidTo:    head.ValidTo,
			}
		}

		inForce := recordedAt.After(head.ValidFrom)
		if !inForce {
			// Nothing served yet: one revision replaces another.
			rev := s.nextOverrideRevision(head, cmd, recordedAt)
			rev.ValidFrom = NormalizeTimestamp(cmd.ValidFrom)
			// Excluding itself is not an optimization: the head is part of the
			// current projection, so without it every update of an override
			// would collide with the version it replaces.
			if err := s.checkOverrideOverlap(ctx, tx, rev, overrideID); err != nil {
				return err
			}
			if err := tx.InsertOverrideRevision(ctx, rev); err != nil {
				return err
			}
			updated = rev
			return nil
		}

		validTo := NormalizeTimestamp(cmd.ValidTo)
		if !validTo.After(recordedAt) {
			return &OverrideEndsInThePastError{OverrideID: overrideID, ValidTo: validTo, Now: recordedAt}
		}

		// 1. Close the served part where it actually stops.
		truncated := *head
		truncated.RevisionID = s.newID()
		truncated.Revision = head.Revision + 1
		truncated.ValidTo = recordedAt
		truncated.RecordedAt = recordedAt
		truncated.RecordedBy = optionalString(cmd.ActorID)
		if err := tx.InsertOverrideRevision(ctx, &truncated); err != nil {
			return err
		}

		// 2. The change itself, as its own override starting now.
		rev := &OverrideRevision{
			RevisionID: s.newID(),
			OverrideID: s.newID(),
			ScheduleID: scheduleID,
			Revision:   1,
			Layer:      head.Layer,
			UserID:     cmd.UserID,
			ValidFrom:  recordedAt,
			ValidTo:    validTo,
			Reason:     cmd.Reason,
			RecordedAt: recordedAt,
			RecordedBy: optionalString(cmd.ActorID),
		}
		// Checked against the projection this transaction has already changed:
		// the truncation above is visible here, so the two halves do not
		// collide with each other. Nothing is excluded - the new override is
		// new, and the old one now ends where this one starts.
		if err := s.checkOverrideOverlap(ctx, tx, rev, ""); err != nil {
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

// nextOverrideRevision is the successor revision of a head, carrying the
// caller's values.
func (s *Service) nextOverrideRevision(head *OverrideRevision, cmd OverrideCommand,
	recordedAt time.Time) *OverrideRevision {

	return &OverrideRevision{
		RevisionID: s.newID(),
		OverrideID: head.OverrideID,
		ScheduleID: head.ScheduleID,
		Revision:   head.Revision + 1,
		Layer:      head.Layer,
		UserID:     cmd.UserID,
		ValidFrom:  NormalizeTimestamp(cmd.ValidFrom),
		ValidTo:    NormalizeTimestamp(cmd.ValidTo),
		Reason:     cmd.Reason,
		RecordedAt: recordedAt,
		RecordedBy: optionalString(cmd.ActorID),
	}
}

// CancelOverride ends an override, and what that means depends on whether any
// of it has happened yet.
//
// The old behaviour was one branch: append a tombstone, which drops the
// override out of the current projection - and the projection is what every
// render reads, so a stand-in who had already covered four hours vanished from
// last week's calendar too. Nothing was destroyed, the revisions are all still
// there, but the answer to "who was on duty at 11:00 last Tuesday" changed
// because somebody cancelled a shift today.
//
// The rule is that you can cancel the future and cannot un-happen the past:
//
//	T <= valid_from   the override never started    -> tombstone
//	in force          part of it has happened       -> truncate to T
//	T >= valid_to     it is over                    -> refuse
//
// The first boundary is <= rather than <: truncating an override to
// valid_to == valid_from would violate the CHECK on the table, so cancelling
// exactly at the start has to take the tombstone branch. It also means the same
// thing - nobody stood in for anybody.
//
// The reason is the canceller's own, and is not inherited from the revision
// being ended. Copying it forward put one person's words under another
// person's name, and the words are not lost: they are on the revisions that
// carried them, which is where history lives.
func (s *Service) CancelOverride(ctx context.Context, scheduleID, overrideID string,
	expectedRevision int64, actorID string, reason *string) error {

	return s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		// A cancel adds no assignment, but it does record who did it, so the
		// actor is still locked and checked.
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

		next := *head
		next.RevisionID = s.newID()
		next.Revision = head.Revision + 1
		next.RecordedAt = recordedAt
		next.RecordedBy = optionalString(actorID)
		next.Reason = reason

		switch {
		case !recordedAt.Before(head.ValidTo):
			return &OverrideAlreadyEndedError{
				OverrideID: overrideID,
				ValidFrom:  head.ValidFrom,
				ValidTo:    head.ValidTo,
			}
		case !recordedAt.After(head.ValidFrom):
			// Not started. It never happened, so there is nothing to keep.
			next.Deleted = true
		default:
			// In force. The part that has been served stays exactly as it was
			// served; only the rest goes.
			next.ValidTo = recordedAt
		}
		return tx.InsertOverrideRevision(ctx, &next)
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
	// An inactive schedule has nobody on duty, so there is nobody to stand in
	// for. Allowing it would also let an override outlive the delete that was
	// supposed to end it.
	if locked.DeletedAt != nil {
		return nil, time.Time{}, ErrScheduleDeleted
	}
	// Plain wall clock, normalized to what the database stores. The value is
	// audit only: which revision of an override is its head is decided by the
	// chain, not by comparing recorded_at. It used to be forced monotonic
	// against a MAX(recorded_at) query on every command - one extra round trip
	// per override write - so that as-of reads could resolve a head by time.
	// As-of reads are gone.
	return locked, NormalizeTimestamp(s.now().UTC()), nil
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

	existing, err := view.GetOverrideProjectionInRange(ctx, rev.ScheduleID, &rev.ValidFrom, &rev.ValidTo)
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
