package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduler"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
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

// OverrideRequest represents a request to create an override
type OverrideRequest struct {
	UserID         string    `json:"user_id"`          // User to assign
	StartTime      time.Time `json:"start_time"`       // Legacy: UTC or offset-included time
	EndTime        time.Time `json:"end_time"`         // Legacy: UTC or offset-included time
	Reason         string    `json:"reason"`           // Optional reason
	Timezone       string    `json:"timezone"`         // Optional: Timezone for local times
	StartTimeLocal string    `json:"start_time_local"` // Optional: "2006-01-02T15:04"
	EndTimeLocal   string    `json:"end_time_local"`   // Optional: "2006-01-02T15:04"
}

// RenderEntry represents a single calendar entry
type RenderEntry struct {
	UserIDs   []string  `json:"user_ids"`
	UserNames []string  `json:"user_names"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	Layer     string    `json:"layer"` // "l1", "l2", "override"
	// Override-specific fields (only set when Layer == "override")
	OverrideID    string     `json:"override_id,omitempty"`
	ScheduleID    string     `json:"schedule_id,omitempty"`
	OverrideStart *time.Time `json:"override_start,omitempty"`
	OverrideEnd   *time.Time `json:"override_end,omitempty"`
	Reason        string     `json:"reason,omitempty"`
}

// RenderResponse wraps calendar entries with schedule metadata
type RenderResponse struct {
	Timezone string        `json:"timezone"`
	Entries  []RenderEntry `json:"entries"`
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

// DeleteTeamSchedule godoc
// @Summary Delete schedule for a team
// @Description Delete the on-call schedule configuration for a team
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Success 204
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule [delete]
func (a *API) DeleteTeamSchedule(c echo.Context) error {
	teamID := c.Param("id")

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "no schedule configured for this team"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if err := a.store.DeleteSchedule(schedule.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
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
					if _, err := a.store.GetUserByID(userID); err != nil {
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
			if _, err := a.store.GetUserByID(userID); err != nil {
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

// RenderSchedule godoc
// @Summary Render schedule calendar
// @Description Get schedule entries for a time range
// @Tags schedules
// @Produce json
// @Param id path string true "Team ID"
// @Param from query string true "Start time (RFC3339)"
// @Param until query string true "End time (RFC3339)"
// @Success 200 {array} RenderEntry
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/render [get]
func (a *API) RenderSchedule(c echo.Context) error {
	teamID := c.Param("id")

	fromStr := c.QueryParam("from")
	untilStr := c.QueryParam("until")

	from, err := time.Parse(time.RFC3339, fromStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid 'from' parameter"})
	}
	until, err := time.Parse(time.RFC3339, untilStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid 'until' parameter"})
	}

	// Validate time range
	if !until.After(from) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "until must be after from"})
	}
	maxRange := 90 * 24 * time.Hour
	if until.Sub(from) > maxRange {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "range cannot exceed 90 days"})
	}

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "no schedule configured for this team"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Load L1 epochs for the time range
	l1Epochs, err := a.store.GetRotationEpochs(schedule.ID, "l1", from, until)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load L1 epochs"})
	}

	// Load L2 epochs if enabled
	var l2Epochs []*model.RotationEpoch
	if schedule.L2Enabled {
		l2Epochs, err = a.store.GetRotationEpochs(schedule.ID, "l2", from, until)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load L2 epochs"})
		}
	}

	// Load overrides for the period
	schedule.Overrides, err = a.store.GetScheduleOverrides(schedule.ID, from, until)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load overrides"})
	}

	// Build user map for segment generator
	userIDs := make(map[string]bool)
	for _, epoch := range l1Epochs {
		for _, group := range epoch.Groups {
			for _, userID := range group {
				userIDs[userID] = true
			}
		}
	}
	for _, epoch := range l2Epochs {
		for _, group := range epoch.Groups {
			for _, userID := range group {
				userIDs[userID] = true
			}
		}
	}
	for _, o := range schedule.Overrides {
		if o.User != nil {
			// Already loaded? (Should not happen with current store implementation but safe to check)
			// Ensure it's in the map later if we decide to re-assign
		}
		userIDs[o.UserID] = true
	}

	uniqueUserIDs := make([]string, 0, len(userIDs))
	for id := range userIDs {
		uniqueUserIDs = append(uniqueUserIDs, id)
	}

	fetchedUsers, err := a.store.GetUsersByIDs(uniqueUserIDs)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to batch load users: " + err.Error()})
	}
	users := make(map[string]*model.User)
	for _, u := range fetchedUsers {
		users[u.ID] = u
	}

	// Ensure overrides have user objects attached if they came preloaded (store-dependent)
	// or from the batch fetch
	for _, o := range schedule.Overrides {
		if u, ok := users[o.UserID]; ok {
			o.User = u // Attach for segment generator usage
		}
	}

	// Generate L1 segments
	gen := scheduler.NewSegmentGenerator()
	segments := gen.GenerateSegments(schedule, l1Epochs, schedule.Overrides, users, from, until)

	// Generate L2 segments and append
	if schedule.L2Enabled && len(l2Epochs) > 0 {
		l2Schedule := &model.Schedule{
			ID:             schedule.ID,
			Timezone:       schedule.Timezone,
			L1RotationType: schedule.L2RotationType,
			L1HandoffTime:  schedule.L2HandoffTime,
			L1HandoffDay:   schedule.L2HandoffDay,
		}
		l2Segments := gen.GenerateSegments(l2Schedule, l2Epochs, nil, users, from, until)
		// Mark as L2 layer
		for i := range l2Segments {
			l2Segments[i].Layer = "l2"
		}
		segments = append(segments, l2Segments...)
	}

	// Load timezone for calendar rendering (view timezone)
	// Default to schedule's timezone if not specified in query
	viewTzName := c.QueryParam("timezone")
	if viewTzName == "" {
		viewTzName = schedule.Timezone
	}

	loc, err := time.LoadLocation(viewTzName)
	if err != nil {
		// Fallback to schedule timezone if invalid
		if l, err := time.LoadLocation(schedule.Timezone); err == nil {
			loc = l
		} else {
			loc = time.UTC
		}
	}

	// Prepare for calendar UI: split by days + merge adjacent same-user segments
	// This splits segments based on MIDNIGHT in the VIEW timezone
	segments = gen.RenderCalendarSchedule(segments, loc)

	// Convert to RenderEntry format (Times are UTC)
	entries := make([]RenderEntry, 0, len(segments))
	for _, seg := range segments {
		entry := RenderEntry{
			UserIDs:   seg.UserIDs,
			StartTime: seg.StartTime, // Return UTC
			EndTime:   seg.EndTime,   // Return UTC
			Layer:     seg.Layer,
		}
		for _, u := range seg.Users {
			entry.UserNames = append(entry.UserNames, u.Name)
		}
		if seg.Override != nil {
			entry.OverrideID = seg.Override.ID
			entry.ScheduleID = seg.Override.ScheduleID
			entry.OverrideStart = &seg.Override.StartTime
			entry.OverrideEnd = &seg.Override.EndTime
			entry.Reason = seg.Override.Reason
		}
		entries = append(entries, entry)
	}

	return c.JSON(http.StatusOK, RenderResponse{
		Timezone: schedule.Timezone,
		Entries:  entries,
	})
}

// CreateScheduleOverride godoc
// @Summary Create an override
// @Description Create a temporary on-call override (vacation, swap)
// @Tags schedules
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param override body OverrideRequest true "Override data"
// @Success 201 {object} model.ScheduleOverride
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/teams/{id}/schedule/overrides [post]
func (a *API) CreateScheduleOverride(c echo.Context) error {
	teamID := c.Param("id")

	schedule, err := a.store.GetScheduleByTeamID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "no schedule configured for this team"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req OverrideRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Handle local time inputs if provided
	if req.Timezone != "" {
		loc, err := time.LoadLocation(req.Timezone)
		if err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid timezone"})
		}

		// Parse StartTimeLocal if present
		if req.StartTimeLocal != "" {
			// Try parsing ISO format (from datetime-local input)
			// datetime-local format: "2006-01-02T15:04"
			t, err := time.ParseInLocation("2006-01-02T15:04", req.StartTimeLocal, loc)
			if err != nil {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid start_time_local format"})
			}
			req.StartTime = t.UTC()
		}

		// Parse EndTimeLocal if present
		if req.EndTimeLocal != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", req.EndTimeLocal, loc)
			if err != nil {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid end_time_local format"})
			}
			req.EndTime = t.UTC()
		}
	}

	if req.UserID == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "user_id is required"})
	}
	if req.StartTime.IsZero() || req.EndTime.IsZero() {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "start_time and end_time (or local equivalents) are required"})
	}
	if !req.EndTime.After(req.StartTime) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "end_time must be after start_time"})
	}
	if req.StartTime.Before(time.Now().UTC().Add(-5 * time.Minute)) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "start_time cannot be in the past"})
	}

	// Verify user is a member of the team
	members, err := a.store.GetTeamMembers(teamID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load team members: " + err.Error()})
	}
	isMember := false
	for _, m := range members {
		if m.ID == req.UserID {
			isMember = true
			break
		}
	}
	if !isMember {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("user %s is not a member of team %s", req.UserID, teamID)})
	}

	// Check for conflicting overrides
	existingOverrides, err := a.store.GetScheduleOverrides(schedule.ID, req.StartTime, req.EndTime)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to check existing overrides"})
	}
	if len(existingOverrides) > 0 {
		return c.JSON(http.StatusConflict, map[string]interface{}{
			"error":                 "override conflicts with existing override(s)",
			"conflicting_overrides": existingOverrides,
		})
	}

	// Get current user from context
	createdBy := ""
	if userID, ok := c.Get("user_id").(string); ok {
		createdBy = userID
	}

	override := &model.ScheduleOverride{
		ID:         uuid.New().String(),
		ScheduleID: schedule.ID,
		UserID:     req.UserID,
		StartTime:  req.StartTime,
		EndTime:    req.EndTime,
		Reason:     req.Reason,
		CreatedBy:  createdBy,
	}

	if err := a.store.CreateScheduleOverride(override); err != nil {
		// Check for exclusion constraint violation (Postgres error 23P01)
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23P01" {
			return c.JSON(http.StatusConflict, map[string]interface{}{
				"error": "override conflicts with existing override(s) (race detected)",
			})
		}
		// Fallback check
		if strings.Contains(err.Error(), "exclusion constraint") || strings.Contains(err.Error(), "no_overlapping_overrides") {
			return c.JSON(http.StatusConflict, map[string]interface{}{
				"error": "override conflicts with existing override(s) (race detected)",
			})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Load user for response
	override.User, _ = a.store.GetUserByID(req.UserID)

	return c.JSON(http.StatusCreated, override)
}

// UpdateScheduleOverride godoc
// @Summary Update an override
// @Description Update an existing schedule override (user, times, reason)
// @Tags schedules
// @Accept json
// @Produce json
// @Param schedule_id path string true "Schedule ID"
// @Param id path string true "Override ID"
// @Param override body OverrideRequest true "Override data"
// @Success 200 {object} model.ScheduleOverride
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/v1/schedules/{schedule_id}/overrides/{id} [put]
func (a *API) UpdateScheduleOverride(c echo.Context) error {
	scheduleID := c.Param("schedule_id")
	overrideID := c.Param("id")

	// ScopeScheduleOverride middleware already verified ownership and loaded schedule
	schedule, _ := c.Get("schedule").(*model.Schedule)
	if schedule == nil {
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "schedule not found"})
	}

	var req OverrideRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Handle local time inputs if provided
	if req.Timezone != "" {
		loc, err := time.LoadLocation(req.Timezone)
		if err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid timezone"})
		}

		if req.StartTimeLocal != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", req.StartTimeLocal, loc)
			if err != nil {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid start_time_local format"})
			}
			req.StartTime = t.UTC()
		}

		if req.EndTimeLocal != "" {
			t, err := time.ParseInLocation("2006-01-02T15:04", req.EndTimeLocal, loc)
			if err != nil {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid end_time_local format"})
			}
			req.EndTime = t.UTC()
		}
	}

	if req.UserID == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "user_id is required"})
	}
	if req.StartTime.IsZero() || req.EndTime.IsZero() {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "start_time and end_time (or local equivalents) are required"})
	}
	if !req.EndTime.After(req.StartTime) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "end_time must be after start_time"})
	}

	// Verify user is a member of the team
	members, err := a.store.GetTeamMembers(schedule.TeamID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load team members: " + err.Error()})
	}
	isMember := false
	for _, m := range members {
		if m.ID == req.UserID {
			isMember = true
			break
		}
	}
	if !isMember {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("user %s is not a member of team %s", req.UserID, schedule.TeamID)})
	}

	// Check for conflicting overrides (exclude self)
	existingOverrides, err := a.store.GetScheduleOverrides(scheduleID, req.StartTime, req.EndTime)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to check existing overrides"})
	}
	var conflicts []*model.ScheduleOverride
	for _, o := range existingOverrides {
		if o.ID != overrideID {
			conflicts = append(conflicts, o)
		}
	}
	if len(conflicts) > 0 {
		return c.JSON(http.StatusConflict, map[string]interface{}{
			"error":                 "override conflicts with existing override(s)",
			"conflicting_overrides": conflicts,
		})
	}

	override := &model.ScheduleOverride{
		ID:        overrideID,
		UserID:    req.UserID,
		StartTime: req.StartTime,
		EndTime:   req.EndTime,
		Reason:    req.Reason,
	}

	if err := a.store.UpdateScheduleOverride(override); err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "override not found"})
		}
		// Check for exclusion constraint violation (race condition)
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23P01" {
			return c.JSON(http.StatusConflict, map[string]interface{}{
				"error": "override conflicts with existing override(s) (race detected)",
			})
		}
		if strings.Contains(err.Error(), "exclusion constraint") || strings.Contains(err.Error(), "no_overlapping_overrides") {
			return c.JSON(http.StatusConflict, map[string]interface{}{
				"error": "override conflicts with existing override(s) (race detected)",
			})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Load full override data for response
	override.ScheduleID = scheduleID
	override.User, _ = a.store.GetUserByID(req.UserID)

	return c.JSON(http.StatusOK, override)
}

// DeleteScheduleOverride godoc
// @Summary Delete an override
// @Description Delete a schedule override
// @Tags schedules
// @Param schedule_id path string true "Schedule ID"
// @Param id path string true "Override ID"
// @Success 204
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/schedules/{schedule_id}/overrides/{id} [delete]
func (a *API) DeleteScheduleOverride(c echo.Context) error {

	overrideID := c.Param("id")

	if err := a.store.DeleteScheduleOverride(overrideID); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}
