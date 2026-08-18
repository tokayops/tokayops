package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// maxRenderRange bounds one render request. Without it a client can make the
// renderer materialize years of daily slots for a single call. The limit is
// unchanged from the endpoint this replaced, so clients already respect it.
const maxRenderRange = 90 * 24 * time.Hour

// RenderSchedule godoc
// @Summary Render the schedule calendar of a team
// @Description Who was on duty across a range, derived from the revision chain. Range is capped at 90 days.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Param from query string true "Start of the range (RFC3339)"
// @Param until query string true "End of the range (RFC3339)"
// @Success 200 {object} ScheduleRenderResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/render [get]
func (a *API) RenderSchedule(c echo.Context) error {
	from, err := time.Parse(time.RFC3339, c.QueryParam("from"))
	if err != nil {
		return badRequest(c, CodeInvalidParameter, "invalid 'from' parameter")
	}
	until, err := time.Parse(time.RFC3339, c.QueryParam("until"))
	if err != nil {
		return badRequest(c, CodeInvalidParameter, "invalid 'until' parameter")
	}
	if !until.After(from) {
		return badRequest(c, CodeInvalidParameter, "until must be after from")
	}
	if until.Sub(from) > maxRenderRange {
		return badRequest(c, CodeRangeTooLarge, "range cannot exceed 90 days")
	}

	ctx := c.Request().Context()
	var scheduleID string
	err = a.scheduleRead.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := a.revisionRoot(ctx, view, c.Param("id"))
		if err != nil {
			return err
		}
		scheduleID = root.ID
		return nil
	})
	if err != nil {
		return a.mapScheduleError(c, err)
	}

	res, err := a.scheduleRenderer.RenderRange(ctx, scheduleID, from, until)
	if err != nil {
		return a.mapScheduleError(c, err)
	}

	return c.JSON(http.StatusOK, ScheduleRenderResponse{
		From:                res.From,
		Until:               res.Until,
		HistoryComplete:     res.HistoryComplete,
		HistoryCompleteFrom: res.HistoryCompleteFrom,
		DeletedAt:           res.DeletedAt,
		// Merged into natural shifts: the raw assignments are one grid slot
		// each, and a calendar that showed them unmerged would draw a boundary
		// at every handoff of an unchanged rotation.
		Entries:  shiftDTOs(schedulerender.MergeAdjacent(res.Assignments)),
		Warnings: warningDTOs(res.Warnings),
	})
}
