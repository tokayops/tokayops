package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rbac"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// ScopeResolver resolves the RBAC scope for a request.
// It can optionally store data in the context (e.g., loaded models) to avoid double-fetching.
type ScopeResolver func(c echo.Context, api *API) (rbac.Scope, error)

// Require middleware enforces RBAC permissions.
func (a *API) Require(action rbac.Action, resolve ScopeResolver) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			userID, _ := c.Get("user_id").(string)

			// Resolve scope
			scope, err := resolve(c, a)
			if err != nil {
				// If resolver explicitly returns an error (e.g. 404), return it
				if he, ok := err.(*echo.HTTPError); ok {
					return c.JSON(he.Code, ErrorResponse{Error: fmt.Sprintf("%v", he.Message)})
				}
				// Default to 500 if generic error, or 404 if sql.ErrNoRows (simplification)
				if err == sql.ErrNoRows {
					return c.JSON(http.StatusNotFound, ErrorResponse{Error: "resource not found"})
				}
				return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			}

			// Check permission
			allowed, err := a.rbac.HasPermission(userID, action, scope)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			}
			if !allowed {
				return c.JSON(http.StatusForbidden, ErrorResponse{Error: "forbidden: insufficient permissions"})
			}

			return next(c)
		}
	}
}

// ScopeGlobal returns a global scope.
func ScopeGlobal() ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		return rbac.GlobalScope(), nil
	}
}

// ScopeUserSelfOrAdmin returns a UserScope for the given param.
// This matches "user_id" == current_user_id check in generic RBAC rules for "user" scope.
func ScopeUserSelfOrAdmin(param string) ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		targetUserID := c.Param(param)
		if targetUserID == "" {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "missing user id param")
		}
		// Return UserScope. The generic HasPermission rule 5 will check if (scope.UserID == userID)
		// Rule 1 will check if admin.
		return rbac.UserScope(targetUserID), nil
	}
}

// ScopeCurrentUser returns a UserScope for the authenticated user.
func ScopeCurrentUser() ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		userID, ok := c.Get("user_id").(string)
		if !ok {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusUnauthorized, "unauthorized")
		}
		return rbac.UserScope(userID), nil
	}
}

// ScopeFromResource returns a resolver that loads a resource by ID from the URL param.
// Supported kinds: "team", "alert_group", "token", "policy".
//
// There is deliberately no "schedule" kind. Scoping by schedule ID happens only
// on the override routes, and those go through ScopeScheduleOverride, which
// reads the revision contract; a resolver built on the legacy schedule reader
// would refuse every schedule the revision model governs.
//
// It assumes the read repository is wired, because requireScheduleStack runs
// ahead of it on every route that uses it.
func ScopeFromResource(kind, paramName string) ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		id := c.Param(paramName)
		if id == "" {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("missing %s id param", kind))
		}

		switch kind {
		case "team":
			// Just verify existence (optional, but good for 404s)
			_, err := api.store.GetTeamByID(id)
			if err != nil {
				if err == sql.ErrNoRows {
					return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "team not found")
				}
				return rbac.Scope{}, err
			}
			return rbac.TeamScope(id), nil

		case "alert_group":
			ag, err := api.store.GetAlertGroupByID(id)
			if err != nil {
				if err == sql.ErrNoRows {
					return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "alert group not found")
				}
				return rbac.Scope{}, err
			}
			// Store in context for handler optimization
			c.Set("alert_group", ag)
			return rbac.TeamScope(ag.TeamID), nil

		case "token":
			token, err := api.store.GetAPITokenByID(id)
			if err != nil {
				if err == sql.ErrNoRows {
					return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "token not found")
				}
				return rbac.Scope{}, err
			}
			return rbac.UserScope(token.UserID), nil

		case "policy":
			policy, err := api.store.GetEscalationPolicyByID(id)
			if err != nil {
				if err == sql.ErrNoRows {
					return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "policy not found")
				}
				return rbac.Scope{}, err
			}
			c.Set("policy", policy)
			if policy.TeamID != nil {
				return rbac.TeamScope(*policy.TeamID), nil
			}
			return rbac.GlobalScope(), nil

		default:
			return rbac.Scope{}, fmt.Errorf("unknown resource kind: %s", kind)
		}
	}
}

// ScopeScheduleOverride checks that the override exists AND belongs to the
// named schedule (IDOR protection), then scopes to the owning team.
//
// It reads through the revision contract rather than the legacy schedule
// reader. That reader now refuses any schedule governed by revisions, so
// leaving it here would turn every override request into a 500 - and the
// override tables it would be consulting are not the ones the commands write.
func ScopeScheduleOverride(scheduleIDParam, overrideIDParam string) ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		scheduleID := c.Param(scheduleIDParam)
		overrideID := c.Param(overrideIDParam)

		if scheduleID == "" || overrideID == "" {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "missing schedule or override id")
		}
		ctx := c.Request().Context()
		var teamID string
		err := api.scheduleRead.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
			root, err := view.GetScheduleRoot(ctx, scheduleID)
			if err != nil {
				return err
			}
			// A tombstoned head still proves ownership, so it passes here and
			// the command refuses it with "not found". Authorization decides
			// who may act on this schedule; whether the override is still
			// there is the command's question, asked under its lock.
			if _, err := view.GetOverrideHead(ctx, scheduleID, overrideID); err != nil {
				return err
			}
			teamID = root.TeamID
			return nil
		})
		switch {
		case errors.Is(err, scheduleconfig.ErrScheduleNotFound):
			return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "schedule not found")
		case errors.Is(err, scheduleconfig.ErrOverrideNotFound):
			return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "override not found in this schedule")
		case err != nil:
			return rbac.Scope{}, err
		}

		return rbac.TeamScope(teamID), nil
	}
}

// ScopeFromIntegration loads an integration by ID and determines the RBAC scope.
// For admin: returns GlobalScope (Rule 1 bypass).
// For non-admin: returns TeamScope only if it's a team-scoped webhook they admin; otherwise 404.
func ScopeFromIntegration(paramName string) ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		id := c.Param(paramName)
		if id == "" {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "missing integration id")
		}

		integration, err := api.store.GetIntegrationByID(id)
		if err != nil {
			if errors.Is(err, store.ErrIntegrationNotFound) {
				return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "integration not found")
			}
			return rbac.Scope{}, err
		}
		c.Set("integration", integration)

		// Admin: full access via GlobalScope (Rule 1 bypass)
		userID, _ := c.Get("user_id").(string)
		isAdmin, err := api.rbac.IsAdmin(userID)
		if err != nil {
			return rbac.Scope{}, err
		}
		if isAdmin {
			return rbac.GlobalScope(), nil
		}

		// Non-admin: only team-scoped generic_webhook they admin
		if integration.Scope == nil || *integration.Scope != model.WebhookScopeTeam || integration.TeamID == nil {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "integration not found")
		}

		isTeamAdmin, err := api.rbac.IsTeamAdmin(userID, *integration.TeamID)
		if err != nil {
			return rbac.Scope{}, err
		}
		if !isTeamAdmin {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusNotFound, "integration not found")
		}

		return rbac.TeamScope(*integration.TeamID), nil
	}
}

// ScopeIntegrationCreate resolves scope for POST /integrations.
// Admin → GlobalScope (bypass, handler validates).
// Non-admin + valid team_id → TeamScope.
// Non-admin + missing/empty team_id → 403.
func ScopeIntegrationCreate() ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		bodyBytes, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "failed to read request body")
		}
		c.Request().Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		userID, _ := c.Get("user_id").(string)
		isAdmin, err := api.rbac.IsAdmin(userID)
		if err != nil {
			return rbac.Scope{}, err
		}
		if isAdmin {
			return rbac.GlobalScope(), nil
		}

		var payload map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "invalid json body")
		}

		if val, ok := payload["team_id"]; ok {
			if strVal, ok := val.(string); ok && strVal != "" {
				return rbac.TeamScope(strVal), nil
			}
		}

		return rbac.Scope{}, echo.NewHTTPError(http.StatusForbidden, "forbidden: team_id is required")
	}
}

// ScopeFromBody inspects the request body for a specific field to determine scope.
// It reads the body, decodes it into a map to find the field, and then restores the body.
// If the field is present, it returns a TeamScope(value).
// If the field is missing/empty, it returns a GlobalScope().
func ScopeFromBody(field string) ScopeResolver {
	return func(c echo.Context, api *API) (rbac.Scope, error) {
		// Read body
		bodyBytes, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "failed to read request body")
		}

		// Restore body for subsequent handlers
		c.Request().Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		// Parse body
		var payload map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			// If JSON is invalid, we can returns BadRequest OR default to GlobalScope?
			// Better to fail because we can't determine scope.
			return rbac.Scope{}, echo.NewHTTPError(http.StatusBadRequest, "invalid json body")
		}

		// Lookup field
		if val, ok := payload[field]; ok {
			if strVal, ok := val.(string); ok && strVal != "" {
				return rbac.TeamScope(strVal), nil
			}
		}

		return rbac.GlobalScope(), nil
	}
}
