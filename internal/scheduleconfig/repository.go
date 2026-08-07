// Package scheduleconfig owns the application-level command side of schedule
// configuration: the persistence envelopes around a rotation snapshot, the
// narrow repository/unit-of-work interfaces a store must implement, and the
// service that turns user intent into revisions.
//
// It imports no persistence package. The store implements these interfaces;
// the dependency never points the other way.
package scheduleconfig

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
)

// TimestampResolution is the resolution PostgreSQL stores TIMESTAMPTZ at. Go
// clocks carry nanoseconds, so every effective/recorded timestamp is truncated
// to this resolution before it is compared or written: otherwise two values
// that differ only below a microsecond collapse into one after the database
// round-trip and the strict ordering of revisions breaks.
const TimestampResolution = time.Microsecond

// Typed errors. No SQL error ever escapes a repository implementation.
var (
	// ErrScheduleExists means the team already owns a schedule.
	ErrScheduleExists = errors.New("scheduleconfig: schedule already exists for team")

	// ErrScheduleNotFound means no schedule row matches the ID.
	ErrScheduleNotFound = errors.New("scheduleconfig: schedule not found")

	// ErrTeamNotFound means the schedule would belong to a team that does not
	// exist. Only the caller can fix that, so it is a contract error rather
	// than an invariant violation.
	ErrTeamNotFound = errors.New("scheduleconfig: team not found")

	// ErrVersionConflict means config_version did not match the expected value.
	ErrVersionConflict = errors.New("scheduleconfig: schedule config version conflict")

	// ErrRevisionMismatch means a revision-closing update did not affect
	// exactly the one revision it named. History must never be torn silently.
	ErrRevisionMismatch = errors.New("scheduleconfig: revision to close did not match")

	// ErrRevisionNotFound means no revision satisfies the query.
	ErrRevisionNotFound = errors.New("scheduleconfig: revision not found")

	// ErrInvariantViolation means the database rejected a write in a way that
	// cannot be explained by concurrent legitimate use. It is a bug signal,
	// not a condition callers can recover from.
	ErrInvariantViolation = errors.New("scheduleconfig: invariant violation")
)

// ScheduleRoot is the aggregate identity and concurrency root of a schedule.
// It carries no configuration: configuration lives only in revisions.
type ScheduleRoot struct {
	ID                  string
	TeamID              string
	ConfigVersion       int64
	HistoryCompleteFrom *time.Time
	DeletedAt           *time.Time
}

// Revision kinds. A schedule's history is one unbroken chain of revisions,
// so the period during which the schedule did not exist is a revision too.
//
// Modelling deletion as an absence instead - closing the tail and leaving a
// hole - would make a legitimate delete/recreate cycle indistinguishable from
// a lost row, because a recreate clears deleted_at and the hole is all that
// remains. A reader would then have to either flag every normal recreate as
// corruption or never detect real corruption at all.
const (
	// RevisionActive is a revision that configures the schedule.
	RevisionActive = "active"

	// RevisionDeleted covers the interval in which the schedule was deleted.
	// It carries the last valid snapshot so the column stays NOT NULL and
	// decodable, but no reader may derive assignments from it.
	RevisionDeleted = "deleted"
)

// ScheduleRevision is the persistence envelope around one configuration
// snapshot. EffectiveTo nil marks the tail revision - the one in force from
// EffectiveFrom onwards.
//
// The rotation math reads Snapshot only; ID, Version, the effective interval
// and the audit fields exist for diagnostics, events and history rendering.
type ScheduleRevision struct {
	ID            string
	ScheduleID    string
	Version       int64
	Kind          string
	Snapshot      rotation.ScheduleRevisionSnapshot
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	RecordedAt    time.Time
	CreatedBy     *string
	ChangeReason  *string
	ChangeSummary *rotation.ChangeSummary
}

// OverrideRevision is one append-only version of a logical override.
// OverrideID is the stable logical identity; RevisionID identifies this
// version. Delete appends a revision with Deleted set, it never removes rows.
type OverrideRevision struct {
	RevisionID string
	OverrideID string
	ScheduleID string
	Revision   int64
	Layer      string
	UserID     string
	ValidFrom  time.Time
	ValidTo    time.Time
	Reason     *string
	Deleted    bool
	RecordedAt time.Time
	RecordedBy *string
}

// Override layers. An override belongs to exactly one layer and the renderer
// applies it to that layer only.
const (
	LayerL1 = "l1"
	LayerL2 = "l2"
)

// ScheduleEvent is a domain event recorded in the same transaction as the
// change it describes.
type ScheduleEvent struct {
	ID         string
	ScheduleID string
	EventType  string
	Payload    json.RawMessage
	RecordedAt time.Time
}

// ScheduleConfigRepository hands out a unit of work. Everything a command does
// happens inside one WithinTx call; an error returned from fn rolls the whole
// transaction back.
type ScheduleConfigRepository interface {
	WithinTx(ctx context.Context, fn func(ScheduleConfigTx) error) error
}

// ScheduleConfigTx is the command-side unit of work over one schedule.
//
// It embeds the read view so that a command reads through the same contract
// the renderer uses. Two contracts over the same projection would be two
// chances to project it differently.
type ScheduleConfigTx interface {
	ScheduleReadView

	// CreateInitialSchedule is ATOMIC by construction: the root, revision 1,
	// config_version = 1 and history_complete_from are written by this single
	// operation. There is deliberately no "insert just the root" operation in
	// this contract - the state "schedule without a revision" is structurally
	// inexpressible rather than merely forbidden by test discipline.
	//
	// A concurrent create for the same team returns ErrScheduleExists.
	CreateInitialSchedule(ctx context.Context, root *ScheduleRoot, initial *ScheduleRevision) error

	// LockSchedule takes the row lock that serializes all writes to one
	// schedule. Effective time is captured only after it succeeds.
	LockSchedule(ctx context.Context, scheduleID string) (*ScheduleRoot, error)

	// GetTailRevision returns the open-ended revision, if any.
	GetTailRevision(ctx context.Context, scheduleID string) (*ScheduleRevision, error)

	// CloseRevision closes the revision named by expectedRevisionID and no
	// other. Zero or more than one affected row is ErrRevisionMismatch: a gap
	// in history must not pass unnoticed.
	CloseRevision(ctx context.Context, scheduleID, expectedRevisionID string, at time.Time) error

	InsertRevision(ctx context.Context, revision *ScheduleRevision) error

	// AdvanceVersion is a compare-and-set on config_version.
	AdvanceVersion(ctx context.Context, scheduleID string, expected int64, at time.Time) error

	InsertScheduleEvent(ctx context.Context, event *ScheduleEvent) error

	InsertOverrideRevision(ctx context.Context, rev *OverrideRevision) error
}

// NormalizeTimestamp truncates to the resolution the database stores, so a
// value compares the same before and after a round-trip. It also drops the
// monotonic clock reading that time.Now carries.
func NormalizeTimestamp(t time.Time) time.Time {
	return t.Truncate(TimestampResolution)
}

// NextEffectiveAt is the monotonicity rule for effective time:
//
//	max(now, tail.effective_from + 1 resolution unit)
//
// Two saves in the same instant, or a clock stepping backwards, would
// otherwise produce a zero-length or inverted revision interval and fail the
// effective_to > effective_from check on an otherwise valid operation.
//
// Both inputs are normalized first: a sub-resolution difference is not a
// difference at all once stored, so `now = tail + 500ns` must still advance to
// tail + 1µs. now must be read AFTER the schedule lock is held.
func NextEffectiveAt(tailEffectiveFrom *time.Time, now time.Time) time.Time {
	candidate := NormalizeTimestamp(now)
	if tailEffectiveFrom == nil {
		return candidate
	}
	floor := NormalizeTimestamp(*tailEffectiveFrom).Add(TimestampResolution)
	if candidate.Before(floor) {
		return floor
	}
	return candidate
}
