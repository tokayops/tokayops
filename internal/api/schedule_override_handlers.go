package api

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Override endpoints. They live apart from the rest of the schedule
// configuration handlers because an override is a separate append-only history
// with its own revision counter: its conflict answers name an override
// revision, not the schedule's config version.

// CreateScheduleOverride godoc
// @Summary Create an override
// @Description Records a temporary stand-in as a new append-only override revision.
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param override body ScheduleOverrideRequest true "Override"
// @Success 201 {object} ScheduleOverrideDTO
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/overrides [post]
func (a *API) CreateScheduleOverride(c echo.Context) error {
	var req ScheduleOverrideRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, CodeInvalidRequestBody, "invalid request body")
	}

	rev, err := a.scheduleConfig.CreateOverride(c.Request().Context(), c.Param("id"),
		scheduleconfig.OverrideCommand{
			UserID:    req.UserID,
			ValidFrom: req.ValidFrom,
			ValidTo:   req.ValidTo,
			Reason:    req.Reason,
			ActorID:   actorID(c),
		})
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusCreated, scheduleOverrideDTO(*rev))
}

// UpdateScheduleOverride godoc
// @Summary Update an override
// @Description Appends the next revision of an override. expected_revision must match the current head.
// @Tags schedules
// @Accept json
// @Produce json
// @Param schedule_id path string true "Schedule ID"
// @Param id path string true "Override ID"
// @Param override body ScheduleOverrideRequest true "Override"
// @Success 200 {object} ScheduleOverrideDTO
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/v1/schedules/{schedule_id}/overrides/{id} [put]
func (a *API) UpdateScheduleOverride(c echo.Context) error {
	var req ScheduleOverrideRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, CodeInvalidRequestBody, "invalid request body")
	}

	rev, err := a.scheduleConfig.UpdateOverride(c.Request().Context(),
		c.Param("schedule_id"), c.Param("id"), req.ExpectedRevision,
		scheduleconfig.OverrideCommand{
			UserID:    req.UserID,
			ValidFrom: req.ValidFrom,
			ValidTo:   req.ValidTo,
			Reason:    req.Reason,
			ActorID:   actorID(c),
		})
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusOK, scheduleOverrideDTO(*rev))
}

// DeleteScheduleOverride godoc
// @Summary Cancel an override
// @Description Ends the override from this moment. An override that has not started is removed; one that is in force keeps the hours it has already covered and loses the rest; one that has already ended is refused, because cancelling it would rewrite who was on duty. History is append-only either way.
// @Tags schedules
// @Produce json
// @Param schedule_id path string true "Schedule ID"
// @Param id path string true "Override ID"
// @Param expected_revision query int true "Revision the caller loaded"
// @Param reason query string false "Why it is being cancelled"
// @Success 204
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Router /api/v1/schedules/{schedule_id}/overrides/{id} [delete]
func (a *API) DeleteScheduleOverride(c echo.Context) error {
	// Query rather than body: a DELETE body is not carried reliably by every
	// client and proxy, and losing it here would silently skip the conflict
	// check the parameter exists to perform. The reason rides along for the
	// same reason.
	expected, err := strconv.ParseInt(c.QueryParam("expected_revision"), 10, 64)
	if err != nil {
		return badRequest(c, CodeInvalidParameter, "expected_revision query parameter is required")
	}
	var reason *string
	if raw := c.QueryParam("reason"); raw != "" {
		reason = &raw
	}

	if err := a.scheduleConfig.CancelOverride(c.Request().Context(),
		c.Param("schedule_id"), c.Param("id"), expected, actorID(c), reason); err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
