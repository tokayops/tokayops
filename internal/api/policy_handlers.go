package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
)

// ProviderCapabilitiesLookup is the read-only view of the channel catalogue
// that the API layer needs. What it answers is declared at start-up - which
// provider class carries which target kinds - so it never touches the database
// and never fails because an integration is disabled.
type ProviderCapabilitiesLookup interface {
	Capabilities(name string) (capabilities ProviderCapability, ok bool)
	AllCapabilities() []ProviderCapability
}

// ProviderCapability mirrors providers.Capability at the API boundary. The
// duplication is deliberate: this is what the HTTP response is shaped like, and
// a response shaped by another package's struct changes whenever that struct
// does.
type ProviderCapability struct {
	Name                 string                `json:"name"`
	IntegrationType      model.IntegrationType `json:"integration_type"`
	SupportedTargetKinds []string              `json:"supported_target_kinds"`
}

// ========================================
// Escalation Policy API (Phase 4)
// ========================================

// PolicyRequest represents a request to create/update an escalation policy
type PolicyRequest struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	TeamID      *string             `json:"team_id,omitempty"` // nil = global
	Steps       []PolicyStepRequest `json:"steps,omitempty"`
}

// PolicyStepRequest represents a step in a policy request.
//
// provider + target_kind, which replaced a flat step_type.
//   - provider: e.g. "slack", validated against the capability registry.
//   - target_kind: "dm" | "channel"; validated against the provider's
//     SupportedTargetKinds.
//
// target_type is the existing recipient resolver hint (user|channel|schedule)
// and must be compatible with target_kind ("dm" → user|schedule,
// "channel" → channel).
type PolicyStepRequest struct {
	Provider          string `json:"provider"`    // "slack", "telegram", ...
	TargetKind        string `json:"target_kind"` // "dm" | "channel"
	TargetType        string `json:"target_type"` // user, channel, schedule
	TargetID          string `json:"target_id"`
	DelaySeconds      int    `json:"delay_seconds"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`
	MaxAttempts       int    `json:"max_attempts,omitempty"`
	Message           string `json:"message,omitempty"`
	ContinueOnFailure *bool  `json:"continue_on_failure,omitempty"` // nil defaults to true
}

// ListPolicies godoc
// @Summary List all escalation policies
// @Description Get all escalation policies (filtered by team membership for non-admins)
// @Tags policies
// @Produce json
// @Success 200 {array} model.EscalationPolicy
// @Router /api/v1/policies [get]
func (a *API) ListPolicies(c echo.Context) error {
	userID, ok := c.Get("user_id").(string)
	if !ok {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
	}
	user, err := a.store.GetActiveUserByID(userID)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "user not found"})
	}

	var policies []*model.EscalationPolicy

	if user.Role == model.UserRoleAdmin {
		policies, err = a.store.GetAllEscalationPolicies()
	} else {
		policies, err = a.store.GetEscalationPoliciesForUser(user.ID)
	}

	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	return c.JSON(http.StatusOK, policies)
}

// GetPolicy godoc
// @Summary Get a specific escalation policy
// @Description Get escalation policy by ID
// @Tags policies
// @Produce json
// @Param id path string true "Policy ID"
// @Success 200 {object} model.EscalationPolicy
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/policies/{id} [get]
func (a *API) GetPolicy(c echo.Context) error {
	// Policy loaded by middleware
	policy, ok := c.Get("policy").(*model.EscalationPolicy)
	if !ok {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load policy from context"})
	}

	return c.JSON(http.StatusOK, policy)
}

// CreatePolicy godoc
// @Summary Create a new escalation policy
// @Description Create a new escalation policy (admin only for global, team_admin for team-scoped)
// @Tags policies
// @Accept json
// @Produce json
// @Param body body PolicyRequest true "Policy data"
// @Success 201 {object} model.EscalationPolicy
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Router /api/v1/policies [post]
func (a *API) CreatePolicy(c echo.Context) error {

	var req PolicyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Validation
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "name is required"})
	}
	if len(req.Steps) == 0 {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "at least one step is required"})
	}

	if req.TeamID != nil {
		if *req.TeamID == "" {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "team_id cannot be empty string (omit for global)"})
		}
		// Verify team exists
		_, err := a.store.GetTeamByID(*req.TeamID)
		if err != nil {
			if err == sql.ErrNoRows {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("team %s not found", *req.TeamID)})
			}
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to verify team existence"})
		}
	}

	now := time.Now()
	policy := &model.EscalationPolicy{
		ID:          uuid.New().String(),
		Name:        req.Name,
		Description: req.Description,
		TeamID:      req.TeamID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Convert steps
	steps, err := buildPolicySteps(policy.ID, req.Steps, a.providerCaps)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}
	policy.Steps = steps

	if err := a.store.CreateEscalationPolicy(policy); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusCreated, policy)
}

// UpdatePolicy godoc
// @Summary Update an escalation policy
// @Description Update escalation policy (admin for global, team_admin for team-scoped)
// @Tags policies
// @Accept json
// @Produce json
// @Param id path string true "Policy ID"
// @Param body body PolicyRequest true "Policy data"
// @Success 200 {object} model.EscalationPolicy
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/policies/{id} [put]
func (a *API) UpdatePolicy(c echo.Context) error {
	// Policy loaded by middleware
	existing, ok := c.Get("policy").(*model.EscalationPolicy)
	if !ok {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load policy from context"})
	}

	var req PolicyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	if len(req.Steps) == 0 {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "at least one step is required"})
	}

	// Update fields
	existing.Name = req.Name
	existing.Description = req.Description
	existing.UpdatedAt = time.Now()

	// Rebuild steps
	existing.Steps = nil
	steps, err := buildPolicySteps(existing.ID, req.Steps, a.providerCaps)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}
	existing.Steps = steps

	if err := a.store.UpdateEscalationPolicy(existing); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, existing)
}

// DeletePolicy godoc
// @Summary Delete an escalation policy
// @Description Delete escalation policy (admin for global, team_admin for team-scoped)
// @Tags policies
// @Param id path string true "Policy ID"
// @Success 204 "No Content"
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/policies/{id} [delete]
func (a *API) DeletePolicy(c echo.Context) error {
	// Policy loaded by middleware - RBAC Checked
	_, ok := c.Get("policy").(*model.EscalationPolicy)
	if !ok {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load policy from context"})
	}
	id := c.Param("id")

	// Check usage in Teams
	teams, err := a.store.GetAllTeams()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to check policy usage"})
	}
	for _, team := range teams {
		if team.DefaultPolicyID == id {
			return c.JSON(http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("policy is used as default policy by team %s", team.Name)})
		}
		for _, pid := range team.SeverityRoutes {
			if pid == id {
				return c.JSON(http.StatusConflict, ErrorResponse{Error: fmt.Sprintf("policy is used in severity routing by team %s", team.Name)})
			}
		}
	}

	if err := a.store.DeleteEscalationPolicy(id); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.NoContent(http.StatusNoContent)
}

// targetKindForTargetType is the semantic compatibility map between TargetKind
// and TargetType. These are invariants of the taxonomy itself, not provider-
// specific: a "channel" step never targets a user, a "dm" step never targets
// a channel. Schedule is a fan-out resolver that yields users, so it pairs
// with "dm".
var targetKindForTargetType = map[string]map[string]bool{
	"dm":      {"user": true, "schedule": true},
	"channel": {"channel": true},
}

// validatePolicyStep checks the (provider, target_kind, target_type) triple
// is consistent and that the provider supports target_kind. caps is the
// capability registry view (nil during tests that don't exercise the
// registry - guarded below).
func validatePolicyStep(step PolicyStepRequest, caps ProviderCapabilitiesLookup) error {
	if step.Provider == "" {
		return fmt.Errorf("provider is required")
	}
	if step.TargetKind == "" {
		return fmt.Errorf("target_kind is required")
	}

	// Capability check: provider registered, and target_kind supported.
	// caps == nil means this binary has no channel catalogue wired in (test setup);
	// fall back to validating the legacy {dm,channel} pair without provider
	// existence checks so unit tests that don't supply a registry still pass.
	if caps != nil {
		cap, ok := caps.Capabilities(step.Provider)
		if !ok {
			return fmt.Errorf("unknown provider: %s", step.Provider)
		}
		supported := false
		for _, k := range cap.SupportedTargetKinds {
			if k == step.TargetKind {
				supported = true
				break
			}
		}
		if !supported {
			return fmt.Errorf("provider %s does not support target_kind %s", step.Provider, step.TargetKind)
		}
	}

	// Taxonomy invariant: target_kind ↔ target_type compatibility.
	allowedTargetTypes, ok := targetKindForTargetType[step.TargetKind]
	if !ok {
		return fmt.Errorf("invalid target_kind: %s (must be dm or channel)", step.TargetKind)
	}
	if !allowedTargetTypes[step.TargetType] {
		return fmt.Errorf("%s step requires one of %v target types, got %s", step.TargetKind, keysOf(allowedTargetTypes), step.TargetType)
	}

	// target_id is required for concrete targets - schedules resolve at dispatch.
	if (step.TargetType == "user" || step.TargetType == "channel") && step.TargetID == "" {
		return fmt.Errorf("target_id is required for %s target type", step.TargetType)
	}

	// Timing field guards, which the step shape never touched.
	if step.DelaySeconds < 0 {
		return fmt.Errorf("delay_seconds must be >= 0")
	}
	if step.TimeoutSeconds < 0 {
		return fmt.Errorf("timeout_seconds must be > 0")
	}
	if step.MaxAttempts < 0 || step.MaxAttempts > 100 {
		return fmt.Errorf("max_attempts must be between 1 and 100")
	}

	return nil
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func buildPolicySteps(policyID string, reqSteps []PolicyStepRequest, caps ProviderCapabilitiesLookup) ([]*model.EscalationStep, error) {
	var steps []*model.EscalationStep
	for i, stepReq := range reqSteps {
		if err := validatePolicyStep(stepReq, caps); err != nil {
			return nil, err
		}

		timeout := stepReq.TimeoutSeconds
		if timeout == 0 {
			timeout = 30
		}
		maxAttempts := stepReq.MaxAttempts
		if maxAttempts == 0 {
			maxAttempts = 5
		}
		continueOnFailure := true
		if stepReq.ContinueOnFailure != nil {
			continueOnFailure = *stepReq.ContinueOnFailure
		}

		step := &model.EscalationStep{
			ID:                uuid.New().String(),
			PolicyID:          policyID,
			StepIndex:         i,
			Provider:          stepReq.Provider,
			TargetKind:        stepReq.TargetKind,
			TargetType:        stepReq.TargetType,
			TargetID:          stepReq.TargetID,
			DelaySeconds:      stepReq.DelaySeconds,
			TimeoutSeconds:    timeout,
			MaxAttempts:       maxAttempts,
			Message:           stepReq.Message,
			ContinueOnFailure: continueOnFailure,
		}
		steps = append(steps, step)
	}
	return steps, nil
}
