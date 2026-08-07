package api

import (
	"context"
	"errors"
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

// scheduleConfigReady reports whether the command side is wired. Checking is
// not paranoia: the setters are optional by construction, since the API is
// built before the services it serves exist, and a nil dereference would
// answer with a stack trace rather than a diagnosis.
//
// It returns a bool rather than an error because c.JSON returns nil on
// success: an "err != nil" guard around it would never fire and the check
// would be decorative.
func (a *API) scheduleConfigReady() bool {
	return a.scheduleConfig != nil && a.scheduleRead != nil
}

func serviceUnavailable(c echo.Context, what string) error {
	return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: what + " is not available"})
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	teamID := c.Param("id")

	var out ScheduleConfigResponse
	err := a.scheduleRead.WithinSnapshot(c.Request().Context(), func(view scheduleconfig.ScheduleReadView) error {
		root, err := view.GetScheduleRootByTeam(c.Request().Context(), teamID)
		if err != nil {
			return err
		}
		// A row from before the revision model has no configuration in this
		// model, so it is not found here rather than half-answered.
		if scheduleconfig.IsLegacyRoot(root) {
			return scheduleconfig.ErrScheduleNotFound
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	teamID := c.Param("id")

	var req PutScheduleConfigRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	ctx := c.Request().Context()
	res, err := a.scheduleConfig.Save(ctx, teamID, scheduleconfig.SaveCommand{
		ExpectedVersion: req.ExpectedVersion,
		Desired:         req.config().toConfiguration(),
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
	if a.scheduleRenderer == nil {
		return serviceUnavailable(c, "schedule renderer")
	}
	teamID := c.Param("id")

	var req PutScheduleConfigRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}
	var until *time.Time
	if raw := c.QueryParam("until"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid 'until' parameter"})
		}
		until = &parsed
	}

	res, err := a.scheduleRenderer.Preview(c.Request().Context(), teamID,
		req.config().toConfiguration(), until)
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	// A query parameter rather than a body: a DELETE with a body is not
	// carried reliably by every client and proxy in the path.
	expected, err := strconv.ParseInt(c.QueryParam("expected_version"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest,
			ErrorResponse{Error: "expected_version query parameter is required"})
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	limit, err := revisionLimit(c.QueryParam("limit"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}
	var before *int64
	if raw := c.QueryParam("before_version"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid before_version"})
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
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

// revisionRoot resolves a team's schedule root for the read endpoints, giving
// a legacy row the same answer as a missing one: in the revision model that
// schedule does not exist.
func (a *API) revisionRoot(ctx context.Context, view scheduleconfig.ScheduleReadView, teamID string) (*scheduleconfig.ScheduleRoot, error) {
	root, err := view.GetScheduleRootByTeam(ctx, teamID)
	if err != nil {
		return nil, err
	}
	if scheduleconfig.IsLegacyRoot(root) {
		return nil, scheduleconfig.ErrScheduleNotFound
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
