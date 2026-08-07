package api

import (
	"errors"
	"log"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// mapScheduleError is the ONLY translation from a command-side error to an
// HTTP response. Handlers below never choose a status themselves.
//
// Concentrating it here is what keeps the contract stable: a 409 always
// carries the version or revision the caller has to reload, a 422 always
// carries the offending user IDs, and adding an error to the service cannot
// silently produce a 500 in one handler and a 400 in another.
func (a *API) mapScheduleError(c echo.Context, err error) error {
	// Structured errors first: they carry the details the response needs, and
	// each unwraps to the sentinel the classification below would have used.
	var validation *scheduleconfig.ValidationError
	if errors.As(err, &validation) {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": validation.Msg,
			"field": validation.Field,
		})
	}

	var versionConflict *scheduleconfig.VersionConflictError
	if errors.As(err, &versionConflict) {
		return c.JSON(http.StatusConflict, map[string]any{
			"error":            "schedule was modified by someone else",
			"expected_version": versionConflict.Expected,
			"current_version":  versionConflict.Current,
		})
	}

	var revisionConflict *scheduleconfig.OverrideRevisionConflictError
	if errors.As(err, &revisionConflict) {
		return c.JSON(http.StatusConflict, map[string]any{
			"error":             "override was modified by someone else",
			"expected_revision": revisionConflict.Expected,
			"current_revision":  revisionConflict.Current,
		})
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
		return c.JSON(http.StatusConflict, map[string]any{
			"error":                 "override conflicts with existing override(s)",
			"conflicting_overrides": conflicts,
		})
	}

	// 422 rather than 409: nothing collided, the payload names people who do
	// not belong to the team, and only editing it can fix that.
	var notMember *scheduleconfig.UserNotTeamMemberError
	if errors.As(err, &notMember) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{
			"error":    "some users are not members of this team",
			"user_ids": notMember.UserIDs,
		})
	}

	var memberOnCall *scheduleconfig.MemberOnCallError
	if errors.As(err, &memberOnCall) {
		return c.JSON(http.StatusConflict, map[string]any{
			"error":     "user holds a current on-call assignment",
			"schedules": scheduleRefs(memberOnCall.Schedules),
		})
	}

	var userOnCall *erasure.UserOnCallError
	if errors.As(err, &userOnCall) {
		refs := make([]scheduleconfig.ScheduleRef, len(userOnCall.Schedules))
		for i, s := range userOnCall.Schedules {
			refs[i] = scheduleconfig.ScheduleRef{ScheduleID: s.ScheduleID, TeamID: s.TeamID}
		}
		return c.JSON(http.StatusConflict, map[string]any{
			"error":     "user holds a current on-call assignment",
			"schedules": scheduleRefs(refs),
		})
	}

	switch {
	case errors.Is(err, scheduleconfig.ErrScheduleNotFound),
		errors.Is(err, scheduleconfig.ErrRevisionNotFound),
		errors.Is(err, scheduleconfig.ErrOverrideNotFound),
		errors.Is(err, scheduleconfig.ErrTeamNotFound),
		errors.Is(err, erasure.ErrUserNotFound):
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: err.Error()})

	case errors.Is(err, scheduleconfig.ErrScheduleExists):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "this team already has a schedule"})

	case errors.Is(err, scheduleconfig.ErrScheduleDeleted):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "schedule is deleted"})

	case errors.Is(err, scheduleconfig.ErrLegacySchedule):
		return c.JSON(http.StatusConflict, ErrorResponse{
			Error: "this schedule predates the revision model and must be reset before it can be edited",
		})

	case errors.Is(err, erasure.ErrLastAdmin):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "last active admin"})

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

func scheduleRefs(refs []scheduleconfig.ScheduleRef) []map[string]string {
	out := make([]map[string]string, len(refs))
	for i, ref := range refs {
		out[i] = map[string]string{"schedule_id": ref.ScheduleID, "team_id": ref.TeamID}
	}
	return out
}

// scheduleMetrics is the metrics sink, never nil: an unwired API still runs.
func (a *API) scheduleMetrics() scheduleconfig.Metrics {
	if a.scheduleMetricsSink == nil {
		return scheduleconfig.NopMetrics{}
	}
	return a.scheduleMetricsSink
}
