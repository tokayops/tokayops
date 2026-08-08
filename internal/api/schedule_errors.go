package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// scheduleErrorStatuses is the plain half of the error table: a sentinel, the
// status it answers with, and the message.
//
// It is data rather than a switch so it can be read against the contract in
// one glance. The order is the order it is scanned in, which only matters if
// two sentinels ever wrap each other - none of these do.
var scheduleErrorStatuses = []struct {
	err     error
	status  int
	message string
}{
	{scheduleconfig.ErrScheduleNotFound, http.StatusNotFound, "schedule not found"},
	{scheduleconfig.ErrRevisionNotFound, http.StatusNotFound, "revision not found"},
	{scheduleconfig.ErrOverrideNotFound, http.StatusNotFound, "override not found"},
	{scheduleconfig.ErrTeamNotFound, http.StatusNotFound, "team not found"},
	{erasure.ErrUserNotFound, http.StatusNotFound, "user not found"},

	{scheduleconfig.ErrScheduleExists, http.StatusConflict, "this team already has a schedule"},
	{scheduleconfig.ErrScheduleDeleted, http.StatusConflict, "schedule is deleted"},
	{scheduleconfig.ErrLegacySchedule, http.StatusConflict,
		"this schedule predates the revision model and must be reset before it can be edited"},
	{erasure.ErrLastAdmin, http.StatusConflict, "last active admin"},
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
			return c.JSON(entry.status, ErrorResponse{Error: entry.message})
		}
	}
	return a.mapScheduleFault(c, err)
}

// scheduleErrorDetails renders the errors that carry more than a class.
func scheduleErrorDetails(err error) (int, any, bool) {
	var validation *scheduleconfig.ValidationError
	if errors.As(err, &validation) {
		return http.StatusBadRequest, map[string]any{
			"error": validation.Msg,
			"field": validation.Field,
		}, true
	}

	var versionConflict *scheduleconfig.VersionConflictError
	if errors.As(err, &versionConflict) {
		return http.StatusConflict, map[string]any{
			"error":            "schedule was modified by someone else",
			"expected_version": versionConflict.Expected,
			"current_version":  versionConflict.Current,
		}, true
	}

	var revisionConflict *scheduleconfig.OverrideRevisionConflictError
	if errors.As(err, &revisionConflict) {
		return http.StatusConflict, map[string]any{
			"error":             "override was modified by someone else",
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
			"conflicting_overrides": conflicts,
		}, true
	}

	// 422 rather than 409: nothing collided, the payload names people who do
	// not belong to the team, and only editing it can fix that.
	var notMember *scheduleconfig.UserNotTeamMemberError
	if errors.As(err, &notMember) {
		return http.StatusUnprocessableEntity, map[string]any{
			"error":    "some users are not members of this team",
			"user_ids": notMember.UserIDs,
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
		a.scheduleMetrics().SnapshotDecodeError()
		log.Printf("ALERT schedule_config: stored snapshot could not be decoded: %v", err)
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "stored schedule data is corrupt"})

	case errors.Is(err, scheduleconfig.ErrInvariantViolation),
		errors.Is(err, scheduleconfig.ErrRevisionMismatch):
		log.Printf("ALERT schedule_config: invariant violation: %v", err)
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "schedule invariant violation"})
	}
	return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
}

func scheduleRef(scheduleID, teamID string) map[string]string {
	return map[string]string{"schedule_id": scheduleID, "team_id": teamID}
}

// onCallConflictBody is the 409 that lists the schedules blocking a removal or
// an erasure.
func onCallConflictBody(schedules []map[string]string) map[string]any {
	return map[string]any{
		"error":     "user holds a current on-call assignment",
		"schedules": schedules,
	}
}

// scheduleMetrics is the metrics sink, never nil: an unwired API still runs.
func (a *API) scheduleMetrics() scheduleconfig.Metrics {
	if a.scheduleMetricsSink == nil {
		return scheduleconfig.NopMetrics{}
	}
	return a.scheduleMetricsSink
}
