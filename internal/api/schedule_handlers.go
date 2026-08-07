package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduler"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// ========================================
// Schedule API (Phase 3)
// ========================================

// ScheduleRequest represents a request to create/update a schedule
type ScheduleRequest struct {
	Timezone            string     `json:"timezone"`              // IANA timezone
	L1RotationType      string     `json:"l1_rotation_type"`      // "daily" | "weekly"
	L1HandoffTime       string     `json:"l1_handoff_time"`       // "11:00"
	L1HandoffDay        *int       `json:"l1_handoff_day"`        // 0=Sun, 1=Mon
	L1RotationStart     time.Time  `json:"l1_rotation_start"`     // e.g., 2025-01-01T11:00:00Z
	L2Enabled           bool       `json:"l2_enabled"`            // Enable L2 layer
	L2EscalationTimeout int        `json:"l2_escalation_timeout"` // Minutes before escalating
	L2RotationType      string     `json:"l2_rotation_type"`
	L2HandoffTime       string     `json:"l2_handoff_time"`
	L2HandoffDay        *int       `json:"l2_handoff_day"`
	L2RotationStart     *time.Time `json:"l2_rotation_start"`
	SlackUsergroupID    string     `json:"slack_usergroup_id,omitempty"` // Optional Slack usergroup for on-call sync
}

// SetUsersRequest represents a request to set users for a layer
type SetUsersRequest struct {
	UserIDs []string `json:"user_ids"` // Ordered list of user IDs (L2 only)
}

// SetGroupsRequest represents a request to set L1 rotation groups
type SetGroupsRequest struct {
	Groups [][]string `json:"groups"` // Groups of user IDs, e.g. [["id1","id2"],["id3"]]
}

// GetTeamSchedule godoc
// @Summary Get schedule for a team
// @Description Get the on-call schedule configuration for a team
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} model.Schedule
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule [get]
func (a *API) GetTeamSchedule(c echo.Context) error {
	teamID := c.Param("id")

	// Verify team exists
	_, err := a.store.GetTeamByID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "no schedule configured for this team"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Load users for each layer
	schedule.L1Groups, err = a.store.GetScheduleGroups(schedule.ID, "l1")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load L1 groups"})
	}
	if schedule.L2Enabled {
		schedule.L2Users, err = a.store.GetScheduleUsers(schedule.ID, "l2")
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load L2 users"})
		}
	}

	// Load active overrides (next 30 days)
	now := time.Now()
	schedule.Overrides, err = a.store.GetScheduleOverrides(schedule.ID, now, now.Add(30*24*time.Hour))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load overrides"})
	}

	return c.JSON(http.StatusOK, schedule)
}

// UpsertTeamSchedule godoc
// @Summary Create or update schedule for a team
// @Description Create or update the on-call schedule configuration
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param schedule body ScheduleRequest true "Schedule configuration"
// @Success 200 {object} model.Schedule
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule [put]
func (a *API) UpsertTeamSchedule(c echo.Context) error {
	teamID := c.Param("id")

	// Verify team exists
	_, err := a.store.GetTeamByID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req ScheduleRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Validate handoff time formats (HH:MM)
	if req.L1HandoffTime != "" {
		if _, err := time.Parse("15:04", req.L1HandoffTime); err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "l1_handoff_time must be in HH:MM format (24h)"})
		}
	}
	if req.L2HandoffTime != "" {
		if _, err := time.Parse("15:04", req.L2HandoffTime); err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "l2_handoff_time must be in HH:MM format (24h)"})
		}
	}

	// Set defaults for missing fields
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if req.L1RotationType == "" {
		req.L1RotationType = "daily"
	}
	if req.L1HandoffTime == "" {
		req.L1HandoffTime = "11:00"
	}
	if req.L1RotationStart.IsZero() {
		req.L1RotationStart = time.Now()
	}
	if req.L2RotationType == "" {
		req.L2RotationType = "weekly"
	}
	if req.L2HandoffTime == "" {
		req.L2HandoffTime = "11:00"
	}

	// Validate timezone
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid timezone"})
	}

	// Validate handoff_day (0=Sun, 1=Mon, ..., 6=Sat)
	if req.L1HandoffDay != nil && (*req.L1HandoffDay < 0 || *req.L1HandoffDay > 6) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "l1_handoff_day must be 0-6 (0=Sunday, 6=Saturday)"})
	}
	if req.L2HandoffDay != nil && (*req.L2HandoffDay < 0 || *req.L2HandoffDay > 6) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "l2_handoff_day must be 0-6 (0=Sunday, 6=Saturday)"})
	}

	// Validate rotation types
	if req.L1RotationType != "daily" && req.L1RotationType != "weekly" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "l1_rotation_type must be 'daily' or 'weekly'"})
	}
	if req.L2Enabled && req.L2RotationType != "daily" && req.L2RotationType != "weekly" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "l2_rotation_type must be 'daily' or 'weekly'"})
	}

	// Default L2RotationStart to L1RotationStart if L2 enabled but not set
	if req.L2Enabled && req.L2RotationStart == nil {
		req.L2RotationStart = &req.L1RotationStart
	}

	// Check if schedule exists
	existing, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil && err != sql.ErrNoRows {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	schedule := &model.Schedule{
		TeamID:              teamID,
		Timezone:            req.Timezone,
		SlackUsergroupID:    req.SlackUsergroupID,
		L1RotationType:      model.RotationType(req.L1RotationType),
		L1HandoffTime:       req.L1HandoffTime,
		L1HandoffDay:        req.L1HandoffDay,
		L1RotationStart:     req.L1RotationStart,
		L2Enabled:           req.L2Enabled,
		L2EscalationTimeout: req.L2EscalationTimeout,
		L2RotationType:      model.RotationType(req.L2RotationType),
		L2HandoffTime:       req.L2HandoffTime,
		L2HandoffDay:        req.L2HandoffDay,
		L2RotationStart:     req.L2RotationStart,
	}

	if existing == nil {
		// Create new
		schedule.ID = uuid.New().String()
		if err := a.store.CreateSchedule(schedule); err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
	} else {
		// Update existing
		schedule.ID = existing.ID
		if err := a.store.UpdateSchedule(schedule); err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
	}

	// Reload
	schedule, _ = a.store.GetScheduleByTeamID(teamID)
	return c.JSON(http.StatusOK, schedule)
}

// SetScheduleL1Groups godoc
// @Summary Set L1 rotation groups
// @Description Set the rotation groups for L1 (primary) rotation
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param groups body SetGroupsRequest true "Groups of user IDs"
// @Success 200 {object} map[string]string
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/l1-groups [put]
func (a *API) SetScheduleL1Groups(c echo.Context) error {
	teamID := c.Param("id")

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "no schedule configured for this team"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req SetGroupsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Validate groups
	if len(req.Groups) > 0 {
		members, err := a.store.GetTeamMembers(teamID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load team members: " + err.Error()})
		}
		memberMap := make(map[string]bool)
		for _, m := range members {
			memberMap[m.ID] = true
		}

		for i, group := range req.Groups {
			if len(group) == 0 {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("group %d is empty", i)})
			}
			seen := make(map[string]bool)
			for _, userID := range group {
				if seen[userID] {
					return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("duplicate user %s in group %d", userID, i)})
				}
				seen[userID] = true
				if !memberMap[userID] {
					if _, err := a.store.GetActiveUserByID(userID); err != nil {
						return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("user %s not found", userID)})
					}
					return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("user %s is not a member of team %s", userID, teamID)})
				}
			}
		}
	}

	if err := a.store.SetScheduleGroups(schedule.ID, req.Groups); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// SetScheduleL2Users godoc
// @Summary Set L2 rotation users
// @Description Set the ordered list of users for L2 (secondary) rotation
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param users body SetUsersRequest true "User IDs in rotation order"
// @Success 200 {object} map[string]string
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/l2-users [put]
func (a *API) SetScheduleL2Users(c echo.Context) error {
	return a.setScheduleLayerUsers(c, "l2")
}

func (a *API) setScheduleLayerUsers(c echo.Context, layer string) error {
	teamID := c.Param("id")

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "no schedule configured for this team"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req SetUsersRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Validate that all users exist
	// Validate that all users exist AND are members of the team
	members, err := a.store.GetTeamMembers(teamID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load team members: " + err.Error()})
	}

	memberMap := make(map[string]bool)
	for _, m := range members {
		memberMap[m.ID] = true
	}

	for _, userID := range req.UserIDs {
		if !memberMap[userID] {
			// Check if user exists at all (for better error message)
			if _, err := a.store.GetActiveUserByID(userID); err != nil {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("user %s not found", userID)})
			}
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("user %s is not a member of team %s", userID, teamID)})
		}
	}

	if err := a.store.SetScheduleUsers(schedule.ID, layer, req.UserIDs); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// GetTeamOnCall godoc
// @Summary Get current on-call for a team
// @Description Get who is currently on-call for L1 and L2
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} model.OnCallResult
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/oncall [get]
func (a *API) GetTeamOnCall(c echo.Context) error {
	teamID := c.Param("id")

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			// Return empty result instead of 404 - allows integrations to work
			return c.JSON(http.StatusOK, &model.OnCallResult{})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	result, err := scheduler.FetchCurrentOnCall(a.store, schedule.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, result)
}

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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	var req ScheduleOverrideRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
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
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	var req ScheduleOverrideRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
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
// @Summary Delete an override
// @Description Appends a tombstone. The override history is kept and stays replayable as of any past instant.
// @Tags schedules
// @Produce json
// @Param schedule_id path string true "Schedule ID"
// @Param id path string true "Override ID"
// @Param expected_revision query int true "Revision the caller loaded"
// @Success 204
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/v1/schedules/{schedule_id}/overrides/{id} [delete]
func (a *API) DeleteScheduleOverride(c echo.Context) error {
	if !a.scheduleConfigReady() {
		return serviceUnavailable(c, "schedule configuration service")
	}
	// Query rather than body: a DELETE body is not carried reliably by every
	// client and proxy, and losing it here would silently skip the conflict
	// check the parameter exists to perform.
	expected, err := strconv.ParseInt(c.QueryParam("expected_revision"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest,
			ErrorResponse{Error: "expected_revision query parameter is required"})
	}

	if err := a.scheduleConfig.DeleteOverride(c.Request().Context(),
		c.Param("schedule_id"), c.Param("id"), expected, actorID(c)); err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
