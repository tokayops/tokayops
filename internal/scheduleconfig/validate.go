package scheduleconfig

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Every writer of the revision model - the PostgreSQL repository and the
// in-memory fake alike - runs its input through the helpers below before
// storing anything. That is what keeps the two from drifting: a value one
// accepts can never be one the other rejects, so a service test that passes
// against the fake cannot fail against the database for a reason the fake
// never modelled.
//
// They are named Prepare rather than Validate because they also fill in
// defaults and normalize timestamps to database resolution; calling that
// "validation" would understate what they do to their argument.

// normalizeRecordedAt keeps recorded time at database resolution and fills in
// a value for callers that left it zero.
func normalizeRecordedAt(t time.Time) time.Time {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return NormalizeTimestamp(t)
}

// PrepareRevision normalizes a revision's timestamps and checks the structural
// preconditions of storing it.
func PrepareRevision(revision *ScheduleRevision) error {
	if revision == nil {
		return fmt.Errorf("%w: nil revision", ErrInvariantViolation)
	}
	// An empty string is a perfectly good TEXT primary key, so an unset ID
	// would be stored rather than rejected.
	if revision.ID == "" || revision.ScheduleID == "" {
		return fmt.Errorf("%w: revision needs an id and a schedule id", ErrInvariantViolation)
	}
	if revision.Version < 1 {
		return fmt.Errorf("%w: revision version must start at 1, got %d",
			ErrInvariantViolation, revision.Version)
	}

	revision.EffectiveFrom = NormalizeTimestamp(revision.EffectiveFrom)
	if revision.EffectiveFrom.IsZero() {
		return fmt.Errorf("%w: revision needs an effective_from", ErrInvariantViolation)
	}
	if revision.EffectiveTo != nil {
		to := NormalizeTimestamp(*revision.EffectiveTo)
		if !to.After(revision.EffectiveFrom) {
			return fmt.Errorf("%w: revision interval %v..%v is empty or inverted",
				ErrInvariantViolation, revision.EffectiveFrom, to)
		}
		revision.EffectiveTo = &to
	}
	revision.RecordedAt = normalizeRecordedAt(revision.RecordedAt)
	return nil
}

// PrepareInitialSchedule is PrepareRevision plus the create-flow preconditions
// that bind the first revision to its root, and it stamps the root fields that
// are derived from that revision rather than supplied by the caller.
//
// Deriving config_version and history_complete_from here rather than in each
// writer is what makes the create flow atomic in substance and not only in
// transaction scope: there is one definition of what a freshly created
// schedule looks like.
func PrepareInitialSchedule(root *ScheduleRoot, revision *ScheduleRevision) error {
	if root == nil {
		return fmt.Errorf("%w: nil schedule root", ErrInvariantViolation)
	}
	if root.ID == "" || root.TeamID == "" {
		return fmt.Errorf("%w: schedule root needs an id and a team id", ErrInvariantViolation)
	}
	if err := PrepareRevision(revision); err != nil {
		return err
	}
	if revision.ScheduleID != root.ID {
		return fmt.Errorf("%w: initial revision belongs to schedule %q, not %q",
			ErrInvariantViolation, revision.ScheduleID, root.ID)
	}
	if revision.Version != 1 {
		return fmt.Errorf("%w: initial revision must be version 1, got %d",
			ErrInvariantViolation, revision.Version)
	}
	if revision.EffectiveTo != nil {
		return fmt.Errorf("%w: initial revision must be open-ended", ErrInvariantViolation)
	}

	from := revision.EffectiveFrom
	root.ConfigVersion = 1
	root.HistoryCompleteFrom = &from
	root.DeletedAt = nil
	return nil
}

// PrepareOverrideRevision fills in the defaults a caller may omit - a
// generated revision identifier and the l1 layer - normalizes the validity
// interval and checks the rest.
func PrepareOverrideRevision(rev *OverrideRevision) error {
	if rev == nil {
		return fmt.Errorf("%w: nil override revision", ErrInvariantViolation)
	}
	if rev.RevisionID == "" {
		rev.RevisionID = uuid.New().String()
	}
	if rev.OverrideID == "" {
		return fmt.Errorf("%w: override revision needs a logical override id", ErrInvariantViolation)
	}
	if rev.ScheduleID == "" {
		return fmt.Errorf("%w: override revision needs a schedule id", ErrInvariantViolation)
	}
	if rev.UserID == "" {
		return fmt.Errorf("%w: override revision needs a user id", ErrInvariantViolation)
	}
	if rev.Revision < 1 {
		return fmt.Errorf("%w: override revision number must start at 1, got %d",
			ErrInvariantViolation, rev.Revision)
	}
	if rev.Layer == "" {
		rev.Layer = LayerL1
	}
	if rev.Layer != LayerL1 && rev.Layer != LayerL2 {
		return fmt.Errorf("%w: unknown override layer %q", ErrInvariantViolation, rev.Layer)
	}

	rev.ValidFrom = NormalizeTimestamp(rev.ValidFrom)
	rev.ValidTo = NormalizeTimestamp(rev.ValidTo)
	if rev.ValidFrom.IsZero() {
		return fmt.Errorf("%w: override revision needs a valid_from", ErrInvariantViolation)
	}
	if !rev.ValidTo.After(rev.ValidFrom) {
		return fmt.Errorf("%w: override interval %v..%v is empty or inverted",
			ErrInvariantViolation, rev.ValidFrom, rev.ValidTo)
	}
	rev.RecordedAt = normalizeRecordedAt(rev.RecordedAt)
	return nil
}

// PrepareScheduleEvent fills in the identifier, the empty payload and the
// recorded time, then checks the rest.
//
// A nil event is an error, not a no-op: an event that has to accompany a
// configuration change would otherwise let a failure to assemble it commit as
// a change with no audit trail.
func PrepareScheduleEvent(event *ScheduleEvent) error {
	if event == nil {
		return fmt.Errorf("%w: nil schedule event", ErrInvariantViolation)
	}
	if event.ScheduleID == "" {
		return fmt.Errorf("%w: schedule event needs a schedule id", ErrInvariantViolation)
	}
	if event.EventType == "" {
		return fmt.Errorf("%w: schedule event needs a type", ErrInvariantViolation)
	}
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage("{}")
	}
	// Checked here so a malformed payload reads as a contract violation rather
	// than as a raw database JSON parse error.
	if !json.Valid(event.Payload) {
		return fmt.Errorf("%w: schedule event payload is not valid JSON", ErrInvariantViolation)
	}
	event.RecordedAt = normalizeRecordedAt(event.RecordedAt)
	return nil
}
