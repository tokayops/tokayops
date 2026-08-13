package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// Error codes. Every schedule-editor error carries one, including the ones a
// handler answers directly before the service is reached - otherwise a client
// would still need a prose fallback for half the contract, and the promise
// "branch on the code" would be false exactly where it is least obvious.
//
// They are values rather than free strings so that adding a branch to the
// editor and forgetting to emit the code is a compile error here, not a
// silent default in the browser.
const (
	CodeScheduleNotFound  = "schedule_not_found"
	CodeRevisionNotFound  = "revision_not_found"
	CodeOverrideNotFound  = "override_not_found"
	CodeTeamNotFound      = "team_not_found"
	CodeUserNotFound      = "user_not_found"
	CodeScheduleExists    = "schedule_exists"
	CodeScheduleDeleted   = "schedule_deleted"
	CodeLastAdmin         = "last_admin"
	CodeActorNotActive    = "actor_not_active"
	CodeVersionConflict   = "schedule_version_conflict"
	CodeRevisionConflict  = "override_revision_conflict"
	CodeOverrideOverlap   = "override_overlap"
	CodeUserNotTeamMember = "user_not_team_member"
	CodeMemberOnCall      = "member_on_call"

	// Two codes, because the two refusals are about different halves of the
	// same rule and a caller acts on them differently: an override that has
	// ended cannot be touched at all, while an update that would end one in
	// the past should be reissued as a cancel.
	CodeOverrideAlreadyEnded  = "override_already_ended"
	CodeOverrideEndsInThePast = "override_ends_in_the_past"

	// Deleting a team is refused by two different things, and the codes are
	// separate because the remedies are: history cannot be removed at all, an
	// integration is removed by the caller.
	CodeTeamHasScheduleHistory = "team_has_schedule_history"
	CodeTeamHasIntegrations    = "team_has_integrations"

	CodeValidationFailed   = "validation_failed"
	CodeInvalidRequestBody = "invalid_request_body"
	CodeInvalidParameter   = "invalid_parameter"
	CodeRangeTooLarge      = "range_too_large"
	CodeSnapshotCorrupt    = "snapshot_corrupt"
	CodeInvariantViolation = "invariant_violation"
	CodeServiceUnavailable = "service_unavailable"
	CodeInternal           = "internal_error"
)

// scheduleErrorStatuses is the plain half of the error table: a sentinel, the
// status it answers with, the machine code and the message.
//
// It is data rather than a switch so it can be read against the contract in
// one glance. The order is the order it is scanned in, which only matters if
// two sentinels ever wrap each other - none of these do.
var scheduleErrorStatuses = []struct {
	err     error
	status  int
	code    string
	message string
}{
	{scheduleconfig.ErrScheduleNotFound, http.StatusNotFound, CodeScheduleNotFound, "schedule not found"},
	{scheduleconfig.ErrRevisionNotFound, http.StatusNotFound, CodeRevisionNotFound, "revision not found"},
	{scheduleconfig.ErrOverrideNotFound, http.StatusNotFound, CodeOverrideNotFound, "override not found"},
	{scheduleconfig.ErrTeamNotFound, http.StatusNotFound, CodeTeamNotFound, "team not found"},
	{erasure.ErrUserNotFound, http.StatusNotFound, CodeUserNotFound, "user not found"},

	{scheduleconfig.ErrScheduleExists, http.StatusConflict, CodeScheduleExists,
		"this team already has a schedule"},
	{scheduleconfig.ErrScheduleDeleted, http.StatusConflict, CodeScheduleDeleted, "schedule is deleted"},

	// 422 rather than 409: a conflict invites a retry after re-reading, and
	// re-reading changes nothing here. The override is over; no revision the
	// caller could load would make cancelling it meaningful.
	{scheduleconfig.ErrOverrideAlreadyEnded, http.StatusUnprocessableEntity,
		CodeOverrideAlreadyEnded, "override has already ended"},
	{scheduleconfig.ErrOverrideEndsInThePast, http.StatusUnprocessableEntity,
		CodeOverrideEndsInThePast, "an update cannot end an override in the past"},
	{erasure.ErrLastAdmin, http.StatusConflict, CodeLastAdmin, "last active admin"},

	// Two codes rather than one "team is retained", because the two are acted
	// on differently: schedule history cannot be removed at all, a team-scoped
	// integration is deleted by the caller who then retries.
	{scheduleconfig.ErrTeamHasIntegrations, http.StatusConflict, CodeTeamHasIntegrations,
		"team still has integrations"},

	// 401, not 403: the caller was authorized when the request arrived and has
	// been erased since. There is no permission they could be granted.
	{scheduleconfig.ErrActorNotActive, http.StatusUnauthorized, CodeActorNotActive, "user not found"},
}

// mapScheduleError is the ONLY translation from a command-side error to an
// HTTP response. Handlers below never choose a status themselves.
//
// Concentrating it here is what keeps the contract stable: a 409 always
// carries the version or revision the caller has to reload, a 422 always
// carries the offending user IDs, and adding an error to the service cannot
// silently produce a 500 in one handler and a 400 in another.
func (a *API) mapScheduleError(c echo.Context, err error) error {
	// Structured errors first: they carry the details the response needs, and
	// each unwraps to a sentinel the table below would have matched instead.
	if status, body, ok := scheduleErrorDetails(err); ok {
		return c.JSON(status, body)
	}
	for _, entry := range scheduleErrorStatuses {
		if errors.Is(err, entry.err) {
			return c.JSON(entry.status, ErrorResponse{Error: entry.message, Code: entry.code})
		}
	}
	return a.mapScheduleFault(c, err)
}

// badRequest is the direct-answer half of the contract: the checks a handler
// makes before the service is reached still have to name themselves.
func badRequest(c echo.Context, code, message string) error {
	return c.JSON(http.StatusBadRequest, ErrorResponse{Error: message, Code: code})
}

// scheduleErrorDetails renders the errors that carry more than a class.
func scheduleErrorDetails(err error) (int, any, bool) {
	var validation *scheduleconfig.ValidationError
	if errors.As(err, &validation) {
		return http.StatusBadRequest, map[string]any{
			"error": validation.Msg,
			"code":  CodeValidationFailed,
			"field": validation.Field,
		}, true
	}

	var versionConflict *scheduleconfig.VersionConflictError
	if errors.As(err, &versionConflict) {
		return http.StatusConflict, map[string]any{
			"error":            "schedule was modified by someone else",
			"code":             CodeVersionConflict,
			"expected_version": versionConflict.Expected,
			"current_version":  versionConflict.Current,
		}, true
	}

	var revisionConflict *scheduleconfig.OverrideRevisionConflictError
	if errors.As(err, &revisionConflict) {
		return http.StatusConflict, map[string]any{
			"error":             "override was modified by someone else",
			"code":              CodeRevisionConflict,
			"expected_revision": revisionConflict.Expected,
			"current_revision":  revisionConflict.Current,
		}, true
	}

	var overlap *scheduleconfig.OverrideOverlapError
	if errors.As(err, &overlap) {
		conflicts := make([]map[string]any, len(overlap.Conflicts))
		for i, ref := range overlap.Conflicts {
			conflicts[i] = map[string]any{
				"override_id": ref.OverrideID,
				"user_id":     ref.UserID,
				"valid_from":  ref.ValidFrom,
				"valid_to":    ref.ValidTo,
			}
		}
		return http.StatusConflict, map[string]any{
			"error":                 "override conflicts with existing override(s)",
			"code":                  CodeOverrideOverlap,
			"conflicting_overrides": conflicts,
		}, true
	}

	// 422 rather than 409: nothing collided, the payload names people who do
	// not belong to the team, and only editing it can fix that.
	var notMember *scheduleconfig.UserNotTeamMemberError
	if errors.As(err, &notMember) {
		return http.StatusUnprocessableEntity, map[string]any{
			"error":    "some users are not members of this team",
			"code":     CodeUserNotTeamMember,
			"user_ids": notMember.UserIDs,
		}, true
	}

	// The refusal names the schedule, so an operator reading the response
	// knows which row retains the team without going to look for it.
	var scheduleHistory *scheduleconfig.TeamHasScheduleHistoryError
	if errors.As(err, &scheduleHistory) {
		return http.StatusConflict, map[string]any{
			"error":       "team has schedule history and cannot be deleted",
			"code":        CodeTeamHasScheduleHistory,
			"schedule_id": scheduleHistory.ScheduleID,
		}, true
	}

	// The same 409 body from two packages. erasure reports the conflict
	// globally and scheduleconfig per team; they stay separate types because
	// erasure knows nothing about schedule configuration, and the price is
	// exactly these two branches building the same shape.
	var memberOnCall *scheduleconfig.MemberOnCallError
	if errors.As(err, &memberOnCall) {
		refs := make([]map[string]string, 0, len(memberOnCall.Schedules))
		for _, ref := range memberOnCall.Schedules {
			refs = append(refs, scheduleRef(ref.ScheduleID, ref.TeamID))
		}
		return http.StatusConflict, onCallConflictBody(refs), true
	}
	var userOnCall *erasure.UserOnCallError
	if errors.As(err, &userOnCall) {
		refs := make([]map[string]string, 0, len(userOnCall.Schedules))
		for _, ref := range userOnCall.Schedules {
			refs = append(refs, scheduleRef(ref.ScheduleID, ref.TeamID))
		}
		return http.StatusConflict, onCallConflictBody(refs), true
	}

	return 0, nil, false
}

// mapScheduleFault answers the errors that are nobody's fault but ours.
func (a *API) mapScheduleFault(c echo.Context, err error) error {
	switch {
	// A snapshot that will not decode is corruption, and the one thing that
	// must never happen here is answering with an empty rotation: an empty
	// rotation is a valid state someone chose, so a caller cannot tell the two
	// apart. It is logged loudly and counted.
	case errors.Is(err, scheduleconfig.ErrSnapshotDecode):
		log.Printf("ALERT schedule_config: stored snapshot could not be decoded: %v", err)
		return c.JSON(http.StatusInternalServerError,
			ErrorResponse{Error: "stored schedule data is corrupt", Code: CodeSnapshotCorrupt})

	// The renderer's damage sentinel answers here too. It describes a row that
	// no live write path could have produced - a chain with a hole in it - so
	// it is the same class of answer as the command side's invariant violation,
	// and leaving it to the default below would have given half the schedule
	// contract a machine code and the other half a prose fallback.
	//
	// Its other consumer is untouched: the bulk projection still classifies the
	// same sentinels into per-schedule failure reasons. That is what having
	// sentinels instead of error text buys.
	case errors.Is(err, scheduleconfig.ErrInvariantViolation),
		errors.Is(err, scheduleconfig.ErrRevisionMismatch),
		errors.Is(err, schedulerender.ErrRevisionGap):
		log.Printf("ALERT schedule_config: invariant violation: %v", err)
		return c.JSON(http.StatusInternalServerError,
			ErrorResponse{Error: "schedule invariant violation", Code: CodeInvariantViolation})
	}
	return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error(), Code: CodeInternal})
}

func scheduleRef(scheduleID, teamID string) map[string]string {
	return map[string]string{"schedule_id": scheduleID, "team_id": teamID}
}

// onCallConflictBody is the 409 that lists the schedules blocking a removal or
// an erasure.
func onCallConflictBody(schedules []map[string]string) map[string]any {
	return map[string]any{
		"error":     "user holds a current on-call assignment",
		"code":      CodeMemberOnCall,
		"schedules": schedules,
	}
}
