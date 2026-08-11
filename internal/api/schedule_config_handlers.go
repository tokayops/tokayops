package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Revision listing bounds. The default is a screenful of audit history; the
// cap keeps one request from asking for the whole life of a schedule.
const (
	revisionPageDefault = 100
	revisionPageMax     = 500
)

// requireScheduleStack refuses a request when the schedule services were not
// wired, before the handler can dereference one of them.
//
// It is middleware rather than a line in each handler because the check is a
// property of the route, not of the request: the API is constructed before the
// services it serves exist, so "wired" is a deployment fact and every schedule
// route shares it. The stack is checked whole - a partially wired API is a
// misconfiguration, not a mode worth serving.
//
// The earlier per-handler version was also silently broken: c.JSON returns nil
// on success, so an `if err := check(c); err != nil` guard around it never
// fired.
func (a *API) requireScheduleStack(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if a.scheduleConfig == nil || a.scheduleRead == nil || a.scheduleRenderer == nil {
			return serviceUnavailable(c, "schedule configuration service")
		}
		return next(c)
	}
}

// requireUserEraser is the same for the one route that deletes a user. There
// is no fallback to the old hard delete: it would break every revision that
// names the id.
func (a *API) requireUserEraser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if a.userEraser == nil {
			return serviceUnavailable(c, "user erasure service")
		}
		return next(c)
	}
}

func serviceUnavailable(c echo.Context, what string) error {
	return c.JSON(http.StatusServiceUnavailable,
		ErrorResponse{Error: what + " is not available", Code: CodeServiceUnavailable})
}

func actorID(c echo.Context) string {
	id, _ := c.Get("user_id").(string)
	return id
}

// GetScheduleConfig godoc
// @Summary Get the schedule configuration of a team
// @Description Returns the configuration in force, or the last valid one plus deleted_at for a deleted schedule.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} ScheduleConfigResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/config [get]
func (a *API) GetScheduleConfig(c echo.Context) error {
	teamID := c.Param("id")

	var out ScheduleConfigResponse
	err := a.scheduleRead.WithinSnapshot(c.Request().Context(), func(view scheduleconfig.ScheduleReadView) error {
		root, err := a.revisionRoot(c.Request().Context(), view, teamID)
		if err != nil {
			return err
		}

		// The tail is the highest version. For a deleted schedule that is the
		// deleted revision, which carries a copy of the last valid snapshot -
		// so the editor can prefill a recreate without a second request, and
		// a 410 with no body would have forced exactly that second request.
		tail, err := view.ListRevisions(c.Request().Context(), root.ID, 1, nil)
		if err != nil {
			return err
		}
		if len(tail) == 0 {
			return scheduleconfig.ErrRevisionNotFound
		}

		out = ScheduleConfigResponse{
			ScheduleID:    root.ID,
			Version:       root.ConfigVersion,
			RevisionID:    tail[0].ID,
			EffectiveFrom: tail[0].EffectiveFrom,
			DeletedAt:     root.DeletedAt,
			Config:        configDTOFromSnapshot(tail[0].Snapshot),
		}
		return nil
	})
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

// PutScheduleConfig godoc
// @Summary Save the schedule configuration of a team
// @Description Creates, edits or recreates the schedule depending on its current state. expected_version must be 0 when no schedule exists.
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param config body PutScheduleConfigRequest true "Desired configuration"
// @Success 200 {object} PutScheduleConfigResponse
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/config [put]
func (a *API) PutScheduleConfig(c echo.Context) error {
	teamID := c.Param("id")

	var req PutScheduleConfigRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, CodeInvalidRequestBody, "invalid request body")
	}

	ctx := c.Request().Context()
	res, err := a.scheduleConfig.Save(ctx, teamID, scheduleconfig.SaveCommand{
		ExpectedVersion: req.ExpectedVersion,
		Desired:         req.ScheduleConfigDTO.toConfiguration(),
		ActorID:         actorID(c),
		Reason:          req.Reason,
	})
	if err != nil {
		return a.mapScheduleError(c, err)
	}

	out := PutScheduleConfigResponse{
		Version:    res.Version,
		RevisionID: res.Revision.ID,
		Noop:       res.Noop,
		Created:    res.Created,
		Recreated:  res.Recreated,
	}
	// Rendered AFTER the commit and at the new revision's effective instant,
	// not at "now": a handoff can fall between the commit and this line, and
	// answering with the group that took over would misreport what the save
	// did. The chain is immutable, so this read is deterministic - only a
	// concurrent override can change it, which is honest.
	if a.scheduleRenderer != nil {
		onCall, err := a.scheduleRenderer.CurrentOnCall(ctx, res.Revision.ScheduleID, res.EffectiveAt)
		if err != nil {
			return a.mapScheduleError(c, err)
		}
		out.OnCallAfter = onCallDTO(onCall)
	}
	return c.JSON(http.StatusOK, out)
}

// PostSchedulePreview godoc
// @Summary Preview a schedule configuration
// @Description Renders what a save would do without writing anything.
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param until query string false "End of the previewed window (RFC3339); defaults to 14 days, capped at 90"
// @Param config body PutScheduleConfigRequest true "Desired configuration"
// @Success 200 {object} SchedulePreviewResponse
// @Failure 400 {object} ErrorResponse
// @Failure 422 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/preview [post]
func (a *API) PostSchedulePreview(c echo.Context) error {
	teamID := c.Param("id")

	var req PutScheduleConfigRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, CodeInvalidRequestBody, "invalid request body")
	}
	var until *time.Time
	if raw := c.QueryParam("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return badRequest(c, CodeInvalidParameter, "invalid 'until' parameter")
		}
		until = &parsed
	}

	res, err := a.scheduleRenderer.Preview(c.Request().Context(), teamID,
		req.ScheduleConfigDTO.toConfiguration(), until)
	if err != nil {
		return a.mapScheduleError(c, err)
	}

	return c.JSON(http.StatusOK, SchedulePreviewResponse{
		EvaluatedAt:   res.EvaluatedAt,
		BaseVersion:   res.BaseVersion,
		OnCallBefore:  onCallDTO(res.OnCallBefore),
		OnCallAfter:   onCallDTO(res.OnCallAfter),
		OnCallChanged: res.OnCallChanged,
		Entries:       shiftDTOs(res.Entries),
		Warnings:      warningDTOs(res.Warnings),
	})
}

// DeleteTeamSchedule godoc
// @Summary Delete the schedule of a team
// @Description Deactivates the schedule and tombstones its live overrides. History is kept.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Param expected_version query int true "config_version the caller loaded"
// @Success 204
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule [delete]
func (a *API) DeleteTeamSchedule(c echo.Context) error {
	// A query parameter rather than a body: a DELETE with a body is not
	// carried reliably by every client and proxy in the path.
	expected, err := strconv.ParseInt(c.QueryParam("expected_version"), 10, 64)
	if err != nil {
		return badRequest(c, CodeInvalidParameter, "expected_version query parameter is required")
	}

	if err := a.scheduleConfig.Delete(c.Request().Context(), c.Param("id"), scheduleconfig.DeleteCommand{
		ExpectedVersion: expected,
		ActorID:         actorID(c),
	}); err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ListScheduleRevisions godoc
// @Summary List the revisions of a team's schedule
// @Description Audit trail, newest first, paged by a version cursor. Read-only.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Param limit query int false "Page size (default 100, max 500)"
// @Param before_version query int false "Return revisions strictly below this version"
// @Success 200 {object} ScheduleRevisionListResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/revisions [get]
func (a *API) ListScheduleRevisions(c echo.Context) error {
	limit, err := revisionLimit(c.QueryParam("limit"))
	if err != nil {
		return badRequest(c, CodeInvalidParameter, err.Error())
	}
	var before *int64
	if raw := c.QueryParam("before_version"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return badRequest(c, CodeInvalidParameter, "invalid before_version")
		}
		before = &parsed
	}

	ctx := c.Request().Context()
	var out ScheduleRevisionListResponse
	err = a.scheduleRead.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := a.revisionRoot(ctx, view, c.Param("id"))
		if err != nil {
			return err
		}
		// One more than the page is fetched so "there is another page" is a
		// fact rather than an inference from a full page that happened to end
		// exactly at the boundary.
		revisions, err := view.ListRevisions(ctx, root.ID, limit+1, before)
		if err != nil {
			return err
		}
		if len(revisions) > limit {
			cursor := revisions[limit-1].Version
			out.NextBeforeVersion = &cursor
			revisions = revisions[:limit]
		}
		out.Revisions = make([]ScheduleRevisionDTO, len(revisions))
		for i, rev := range revisions {
			out.Revisions[i] = scheduleRevisionDTO(rev, false)
		}
		return nil
	})
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

// GetScheduleRevision godoc
// @Summary Get one revision of a team's schedule
// @Description Returns the revision with the configuration snapshot it carried. Read-only.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Param revision_id path string true "Revision ID"
// @Success 200 {object} ScheduleRevisionDTO
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/revisions/{revision_id} [get]
func (a *API) GetScheduleRevision(c echo.Context) error {
	ctx := c.Request().Context()

	var out ScheduleRevisionDTO
	err := a.scheduleRead.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := a.revisionRoot(ctx, view, c.Param("id"))
		if err != nil {
			return err
		}
		// Scoped by schedule: a revision ID belonging to another team must be
		// not found, not readable because the caller happened to guess it.
		rev, err := view.GetRevisionByID(ctx, root.ID, c.Param("revision_id"))
		if err != nil {
			return err
		}
		out = scheduleRevisionDTO(*rev, true)
		return nil
	})
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

// GetScheduleOnCall godoc
// @Summary Get who is on duty for a team right now
// @Description The current-assignment projection, derived from the revision chain. A team with no schedule and a team whose schedule is deleted both answer 200 with null layers: the question is who is on duty, and "nobody" is an answer. A schedule whose data cannot produce one - a broken revision chain, a missing history horizon - answers 500 rather than pretending nobody is on call.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} ScheduleOnCallResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/on-call [get]
func (a *API) GetScheduleOnCall(c echo.Context) error {
	ctx := c.Request().Context()

	// One call, one snapshot. This is the most frequently read endpoint here,
	// and fetching the root separately cost a second read-only transaction on
	// every request; the renderer also owns the rules about absent and deleted
	// schedules, which this handler used to keep its own copy of.
	//
	// A team with no schedule, or one that deleted it, is answered rather than
	// refused: this endpoint reports who is on duty, and "nobody" is a true
	// answer, not a missing resource. A client forced to read 404 as "nobody"
	// would swallow a real 404 from a mistyped team along with it.
	//
	// Damage is the opposite case and is not softened into "nobody": a schedule
	// the projection cannot read would otherwise look exactly like one with an
	// empty rotation, and nobody would be paged for it.
	res, err := a.scheduleRenderer.CurrentTeamOnCallNow(ctx, c.Param("id"))
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusOK, ScheduleOnCallResponse{
		ScheduleID: res.ScheduleID,
		DeletedAt:  res.DeletedAt,
		OnCall:     onCallDTO(res.OnCall),
	})
}

// ListScheduleOverrides godoc
// @Summary List the current overrides of a team's schedule
// @Description The head revision of every override that still exists - the only source of expected_revision for an edit.
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} ScheduleOverrideListResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/overrides [get]
func (a *API) ListScheduleOverrides(c echo.Context) error {
	ctx := c.Request().Context()

	out := ScheduleOverrideListResponse{Overrides: []ScheduleOverrideDTO{}}
	err := a.scheduleRead.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := a.revisionRoot(ctx, view, c.Param("id"))
		if err != nil {
			return err
		}
		heads, err := view.ListOverrideHeads(ctx, root.ID, false)
		if err != nil {
			return err
		}
		for _, head := range heads {
			out.Overrides = append(out.Overrides, scheduleOverrideDTO(head))
		}
		return nil
	})
	if err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.JSON(http.StatusOK, out)
}

// revisionRoot resolves a team's schedule root for the read endpoints.
//
// It is also where those endpoints refuse a row with no history horizon. Such a
// row cannot be created by this binary - the create flow writes the horizon in
// the same statement as the row - so it means the destructive upgrade reset was
// never run, and every answer built on it would be a guess about a schedule
// whose configuration this model cannot see. It is reported as the invariant
// violation it is, not as a missing schedule: a 404 here would tell an operator
// the team has no schedule when in fact it has an unreadable one.
func (a *API) revisionRoot(ctx context.Context, view scheduleconfig.ScheduleReadView, teamID string) (*scheduleconfig.ScheduleRoot, error) {
	root, err := view.GetScheduleRootByTeam(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if root.HistoryCompleteFrom == nil {
		return nil, fmt.Errorf("%w: schedule %s has no history horizon; the upgrade reset was not run",
			scheduleconfig.ErrInvariantViolation, root.ID)
	}
	return root, nil
}

func revisionLimit(raw string) (int, error) {
	if raw == "" {
		return revisionPageDefault, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, errors.New("limit must be a positive integer")
	}
	if limit > revisionPageMax {
		limit = revisionPageMax
	}
	return limit, nil
}
