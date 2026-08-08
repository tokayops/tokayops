package scheduleconfig

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The command side is what an HTTP handler answers with, and the response
// shapes are fixed: a 409 carries the version the caller collided with, a 422
// lists the users that are not team members, a 400 names the field that was
// rejected. A bare sentinel carries none of that, and the only alternative -
// the error mapper re-reading an error string - would turn prose into a
// contract.
//
// So every failure that has details is a type whose Unwrap returns the
// matching sentinel: errors.Is still classifies it, errors.As gets the details
// for the one caller that renders them.
var (
	// ErrValidation classifies a payload the caller can fix.
	ErrValidation = errors.New("scheduleconfig: invalid configuration")

	// ErrScheduleDeleted means the schedule is soft-deleted and the command
	// needs it active. Recreating is a Save, not a retry of this command.
	ErrScheduleDeleted = errors.New("scheduleconfig: schedule is deleted")

	// ErrLegacySchedule means the schedule row predates the revision model:
	// it has no tail revision and config_version 0. Revision commands refuse
	// it rather than grafting a chain onto it.
	ErrLegacySchedule = errors.New("scheduleconfig: schedule predates the revision model")

	// ErrOverrideNotFound means no live override revision answers to the ID.
	// A tombstoned override is not found either: it was deleted.
	ErrOverrideNotFound = errors.New("scheduleconfig: override not found")

	// ErrOverrideRevisionConflict means the caller edited a stale override.
	ErrOverrideRevisionConflict = errors.New("scheduleconfig: override revision conflict")

	// ErrOverrideOverlap means the override would cover an instant another
	// override of the same layer already covers.
	ErrOverrideOverlap = errors.New("scheduleconfig: override overlaps an existing override")

	// ErrUserNotTeamMember means the configuration names someone who is not a
	// member of the owning team.
	ErrUserNotTeamMember = errors.New("scheduleconfig: user is not a member of the team")

	// ErrMemberOnCall means a team member cannot be removed because they hold
	// a current assignment.
	ErrMemberOnCall = errors.New("scheduleconfig: user holds a current on-call assignment")

	// ErrActorNotActive means the user on whose behalf a command runs has been
	// erased. It is not a permission problem: they were authorized when the
	// request started and are gone by the time it writes.
	ErrActorNotActive = errors.New("scheduleconfig: the acting user has been erased")

	// ErrSnapshotDecode means a stored snapshot could not be decoded. It is
	// never softened into an empty rotation: empty is a valid configuration a
	// person chose, corruption is not.
	ErrSnapshotDecode = errors.New("scheduleconfig: snapshot could not be decoded")
)

// ValidationError is a rejected input. Field is the payload field when the
// service knows which one it is, and "configuration" for the shape errors the
// rotation validator returns as prose.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("scheduleconfig: invalid %s: %s", e.Field, e.Msg)
}

func (e *ValidationError) Unwrap() error { return ErrValidation }

// invalidField is the shorthand for a field the service itself rejects.
func invalidField(field, format string, args ...any) error {
	return &ValidationError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// invalidConfiguration wraps an error from the rotation validator. The wrap
// happens at the service boundary rather than inside rotation: the rotation
// package is pure math and has no opinion about HTTP status codes.
func invalidConfiguration(err error) error {
	return &ValidationError{Field: "configuration", Msg: err.Error()}
}

// VersionConflictError reports the optimistic-concurrency collision together
// with the version the caller has to reload.
type VersionConflictError struct {
	Expected int64
	Current  int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("scheduleconfig: expected config version %d, current is %d", e.Expected, e.Current)
}

func (e *VersionConflictError) Unwrap() error { return ErrVersionConflict }

// OverrideRevisionConflictError is the same collision for one override.
type OverrideRevisionConflictError struct {
	Expected int64
	Current  int64
}

func (e *OverrideRevisionConflictError) Error() string {
	return fmt.Sprintf("scheduleconfig: expected override revision %d, current is %d", e.Expected, e.Current)
}

func (e *OverrideRevisionConflictError) Unwrap() error { return ErrOverrideRevisionConflict }

// OverrideRef names one override in a conflict report. It is the identity plus
// the interval, which is all a caller needs to show what it collided with.
type OverrideRef struct {
	OverrideID string
	UserID     string
	ValidFrom  time.Time
	ValidTo    time.Time
}

// OverrideOverlapError lists the overrides the rejected one would have
// overlapped.
type OverrideOverlapError struct {
	Conflicts []OverrideRef
}

func (e *OverrideOverlapError) Error() string {
	ids := make([]string, len(e.Conflicts))
	for i, c := range e.Conflicts {
		ids[i] = c.OverrideID
	}
	return fmt.Sprintf("scheduleconfig: override overlaps %s", strings.Join(ids, ", "))
}

func (e *OverrideOverlapError) Unwrap() error { return ErrOverrideOverlap }

// UserNotTeamMemberError lists every offending user rather than the first one:
// the editor shows them all at once, and reporting one per round-trip would
// make fixing a group a sequence of failed saves.
type UserNotTeamMemberError struct {
	UserIDs []string
}

func (e *UserNotTeamMemberError) Error() string {
	return fmt.Sprintf("scheduleconfig: not members of the team: %s", strings.Join(e.UserIDs, ", "))
}

func (e *UserNotTeamMemberError) Unwrap() error { return ErrUserNotTeamMember }

// ScheduleRef names a schedule in a guard report.
type ScheduleRef struct {
	ScheduleID string
	TeamID     string
}

// MemberOnCallError reports the schedules that block removing a team member.
//
// erasure declares its own error of the same shape for the same guard applied
// globally. That is deliberate duplication: erasure knows nothing about
// schedule configuration, and one shared type would only exist to save six
// lines at the cost of an import that means nothing.
type MemberOnCallError struct {
	Schedules []ScheduleRef
}

func (e *MemberOnCallError) Error() string {
	ids := make([]string, len(e.Schedules))
	for i, s := range e.Schedules {
		ids[i] = s.ScheduleID
	}
	return fmt.Sprintf("scheduleconfig: user is on call in %s", strings.Join(ids, ", "))
}

func (e *MemberOnCallError) Unwrap() error { return ErrMemberOnCall }
