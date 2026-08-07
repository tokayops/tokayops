package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/integrations"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbox"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// IntegrationCache interface for reloading cache after CRUD
type IntegrationCache interface {
	LoadAll(store store.StoreInterface) error
}

// IntegrationListResponse represents a list of integrations
type IntegrationListResponse struct {
	Integrations []*model.Integration `json:"integrations"`
	Total        int                  `json:"total"`
}

// CreateIntegrationRequest represents a request to create an integration
type CreateIntegrationRequest struct {
	Type    model.IntegrationType `json:"type"`
	Name    string                `json:"name"`
	Enabled *bool                 `json:"enabled"`           // Pointer to detect if provided (defaults to true)
	Scope   *model.WebhookScope   `json:"scope,omitempty"`   // Required for generic_webhook
	TeamID  *string               `json:"team_id,omitempty"` // Required when scope=team
	Config  json.RawMessage       `json:"config" swaggertype:"object"`
}

// UpdateIntegrationRequest represents a request to update an integration
type UpdateIntegrationRequest struct {
	Name    string          `json:"name"`
	Enabled *bool           `json:"enabled"`
	Config  json.RawMessage `json:"config" swaggertype:"object"`
}

// ListIntegrations returns integrations visible to the current user.
// Admin sees all; team_admin sees only team-scoped webhooks for their teams.
// @Summary List integrations
// @Description Get integrations visible to the current user
// @Tags integrations
// @Produce json
// @Success 200 {object} IntegrationListResponse
// @Failure 403 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations [get]
func (a *API) ListIntegrations(c echo.Context) error {
	userID, _ := c.Get("user_id").(string)

	isAdmin, err := a.rbac.IsAdmin(userID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	allIntegrations, err := a.store.GetAllIntegrations()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var result []*model.Integration
	if isAdmin {
		result = allIntegrations
	} else {
		memberships, err := a.store.GetTeamMembershipsForUser(userID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}

		adminTeams := make(map[string]bool)
		for teamID, role := range memberships {
			if role == model.TeamMemberRoleAdmin {
				adminTeams[teamID] = true
			}
		}

		if len(adminTeams) == 0 {
			return c.JSON(http.StatusForbidden, ErrorResponse{Error: "forbidden: insufficient permissions"})
		}

		for _, i := range allIntegrations {
			if i.Scope != nil && *i.Scope == model.WebhookScopeTeam && i.TeamID != nil && adminTeams[*i.TeamID] {
				result = append(result, i)
			}
		}
	}

	if result == nil {
		result = []*model.Integration{}
	}

	masked := make([]*model.Integration, len(result))
	for i, integ := range result {
		masked[i] = integrations.MaskSecrets(integ)
	}

	return c.JSON(http.StatusOK, IntegrationListResponse{
		Integrations: masked,
		Total:        len(masked),
	})
}

// GetIntegration returns a single integration with masked secrets
// @Summary Get integration
// @Description Get an integration by ID
// @Tags integrations
// @Produce json
// @Param id path string true "Integration ID"
// @Success 200 {object} model.Integration
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations/{id} [get]
func (a *API) GetIntegration(c echo.Context) error {
	// ScopeFromIntegration middleware already loaded and authorized
	if cached, ok := c.Get("integration").(*model.Integration); ok {
		return c.JSON(http.StatusOK, integrations.MaskSecrets(cached))
	}

	id := c.Param("id")
	integration, err := a.store.GetIntegrationByID(id)
	if err != nil {
		if errors.Is(err, store.ErrIntegrationNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "integration not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, integrations.MaskSecrets(integration))
}

// CreateIntegration creates a new integration
// @Summary Create integration
// @Description Create a new integration (admin only)
// @Tags integrations
// @Accept json
// @Produce json
// @Param integration body CreateIntegrationRequest true "Integration data"
// @Success 201 {object} model.Integration
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations [post]
func (a *API) CreateIntegration(c echo.Context) error {
	var req CreateIntegrationRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Validate type
	if !integrations.IsValidType(req.Type) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid integration type"})
	}

	// Validate name
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "name is required"})
	}

	// Normalize empty strings to nil to prevent DB constraint violations
	if req.Scope != nil && *req.Scope == "" {
		req.Scope = nil
	}
	if req.TeamID != nil && *req.TeamID == "" {
		req.TeamID = nil
	}

	// Validate scope and team_id
	if err := validateWebhookScope(req.Type, req.Scope, req.TeamID); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}

	// Validate config
	if err := validateIntegrationConfig(req.Type, req.Config, false); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}

	// Default enabled to true if not specified
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	integration := &model.Integration{
		Type:    req.Type,
		Name:    req.Name,
		Enabled: enabled,
		Scope:   req.Scope,
		TeamID:  req.TeamID,
		Config:  req.Config,
	}

	if err := a.store.CreateIntegration(integration); err != nil {
		if errors.Is(err, store.ErrDuplicateIntegration) {
			return c.JSON(http.StatusConflict, ErrorResponse{Error: "integration of this type already exists"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload cache if available
	a.reloadIntegrationCache()

	// Restart usergroup syncer if Slack integration was created
	if integration.Type == model.IntegrationTypeSlack {
		a.restartUsergroupSyncer()
	}

	// Register the Telegram webhook for an enabled bot (best-effort).
	if integration.Type == model.IntegrationTypeTelegram && integration.Enabled {
		a.setTelegramWebhook(c.Request().Context(), integration)
	}

	return c.JSON(http.StatusCreated, integrations.MaskSecrets(integration))
}

// UpdateIntegration updates an existing integration
// @Summary Update integration
// @Description Update an integration (admin only). Empty secrets keep existing values.
// @Tags integrations
// @Accept json
// @Produce json
// @Param id path string true "Integration ID"
// @Param integration body UpdateIntegrationRequest true "Integration data"
// @Success 200 {object} model.Integration
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations/{id} [put]
func (a *API) UpdateIntegration(c echo.Context) error {
	id := c.Param("id")

	// Use integration from middleware context if available
	var existing *model.Integration
	if cached, ok := c.Get("integration").(*model.Integration); ok {
		existing = cached
	} else {
		var err error
		existing, err = a.store.GetIntegrationByID(id)
		if err != nil {
			if errors.Is(err, store.ErrIntegrationNotFound) {
				return c.JSON(http.StatusNotFound, ErrorResponse{Error: "integration not found"})
			}
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
	}

	// Snapshot the OLD telegram bot token before the config is overwritten below.
	// reloadIntegrationCache() clears tokens, so deleting the old webhook after the
	// reload would have no token — we must capture it now and pass it explicitly.
	var oldTelegramCfgRaw json.RawMessage
	if existing.Type == model.IntegrationTypeTelegram {
		oldTelegramCfgRaw = existing.Config
	}

	var req UpdateIntegrationRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Update fields
	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.Config != nil {
		// Validate config (allow empty/masked secrets on update)
		if err := validateIntegrationConfig(existing.Type, req.Config, true); err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		}
		existing.Config = req.Config
	}

	// For generic_webhook, always validate the effective URL against HTTPS policy.
	// This catches pre-existing http:// URLs when the override is later disabled.
	if existing.Type == model.IntegrationTypeGenericWebhook {
		var effectiveURL string
		if req.Config != nil {
			var cfg model.GenericWebhookConfig
			json.Unmarshal(req.Config, &cfg)
			if cfg.URL != "" {
				effectiveURL = cfg.URL
			}
		}
		if effectiveURL == "" {
			var existingCfg model.GenericWebhookConfig
			json.Unmarshal(existing.Config, &existingCfg)
			effectiveURL = existingCfg.URL
		}
		if err := config.ValidateWebhookURL(effectiveURL); err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		}
	}

	if err := a.store.UpdateIntegration(existing); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload cache
	a.reloadIntegrationCache()

	// Restart usergroup syncer if Slack integration was updated
	if existing.Type == model.IntegrationTypeSlack {
		a.restartUsergroupSyncer()
	}

	// Reload to get updated data
	updated, _ := a.store.GetIntegrationByID(id)
	if updated != nil {
		existing = updated
	}

	// Telegram webhook lifecycle (best-effort, merged/decrypted `existing` config).
	// Delete the OLD webhook ONLY when it would otherwise be orphaned — the
	// integration is now disabled, or the bot token rotated to a different bot.
	// For a same-token edit we just re-affirm via the idempotent setWebhook, so a
	// harmless edit can't open a delete-then-failed-set gap that kills interactivity.
	if existing.Type == model.IntegrationTypeTelegram {
		ctx := c.Request().Context()
		var oldCfg, newCfg model.TelegramConfig
		_ = json.Unmarshal(oldTelegramCfgRaw, &oldCfg)
		_ = json.Unmarshal(existing.Config, &newCfg)

		tokenRotated := oldCfg.BotToken != "" && oldCfg.BotToken != newCfg.BotToken
		if !existing.Enabled || tokenRotated {
			a.deleteTelegramWebhookForConfig(ctx, oldTelegramCfgRaw)
		}
		if existing.Enabled {
			a.setTelegramWebhook(ctx, existing)
		}
	}

	return c.JSON(http.StatusOK, integrations.MaskSecrets(existing))
}

// DeleteIntegration deletes an integration
// @Summary Delete integration
// @Description Delete an integration (admin only)
// @Tags integrations
// @Param id path string true "Integration ID"
// @Success 204
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations/{id} [delete]
func (a *API) DeleteIntegration(c echo.Context) error {
	id := c.Param("id")

	// Use integration from middleware context if available
	var integration *model.Integration
	if cached, ok := c.Get("integration").(*model.Integration); ok {
		integration = cached
	} else {
		var err error
		integration, err = a.store.GetIntegrationByID(id)
		if err != nil {
			if errors.Is(err, store.ErrIntegrationNotFound) {
				return c.JSON(http.StatusNotFound, ErrorResponse{Error: "integration not found"})
			}
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
	}

	if err := a.store.DeleteIntegration(id); err != nil {
		if errors.Is(err, store.ErrIntegrationNotFound) {
			// Race condition: already deleted between GetIntegrationByID and DeleteIntegration
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "integration not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload cache
	a.reloadIntegrationCache()

	// Stop usergroup syncer if Slack integration was deleted
	if integration.Type == model.IntegrationTypeSlack {
		a.restartUsergroupSyncer()
	}

	// Clear the Telegram webhook using the deleted integration's bot token
	// (the cache is empty after the reload above, so use the snapshot).
	if integration.Type == model.IntegrationTypeTelegram {
		a.deleteTelegramWebhookForConfig(c.Request().Context(), integration.Config)
	}

	return c.NoContent(http.StatusNoContent)
}

// telegramWebhookURL returns the public webhook URL for setWebhook, or "" if
// SelfURL is unset (webhook cannot be registered → interactivity disabled).
func (a *API) telegramWebhookURL() string {
	if a.selfURL == "" {
		return ""
	}
	return strings.TrimRight(a.selfURL, "/") + "/telegram/webhook"
}

// setTelegramWebhook registers the Bot API webhook for a telegram integration,
// reading bot_token + secret_token from its (decrypted) config. Best-effort: a
// failure (e.g. a non-public SelfURL) is logged but never fails the API request.
func (a *API) setTelegramWebhook(ctx context.Context, integration *model.Integration) {
	if a.telegram == nil || integration.Type != model.IntegrationTypeTelegram {
		return
	}
	var cfg model.TelegramConfig
	if err := json.Unmarshal(integration.Config, &cfg); err != nil {
		log.Printf("telegram setWebhook: parse config: %v", err)
		return
	}
	url := a.telegramWebhookURL()
	if url == "" {
		log.Printf("telegram: TOKAY_SELF_URL not configured — interactivity disabled: webhook not registered, Ack/Resolve buttons hidden")
		return
	}
	if err := a.telegram.SetWebhook(ctx, cfg.BotToken, url, cfg.SecretToken); err != nil {
		log.Printf("telegram setWebhook failed (interactivity may be disabled): %v", err)
	}
}

// RegisterTelegramWebhookOnStartup registers the Bot API webhook for the enabled
// telegram integration at boot, so setting TOKAY_SELF_URL + restarting is enough
// (the create/update lifecycle hooks otherwise require re-saving the integration).
// Best-effort and idempotent; safe to call in a goroutine.
func (a *API) RegisterTelegramWebhookOnStartup(ctx context.Context) {
	if a.telegram == nil {
		return
	}
	integ, err := a.store.GetIntegrationByType(model.IntegrationTypeTelegram)
	if err != nil {
		// ErrIntegrationNotFound (none configured / disabled) is normal — nothing to do.
		return
	}
	a.setTelegramWebhook(ctx, integ)
}

// deleteTelegramWebhookForConfig clears the Bot API webhook using the bot token in
// the given (old) config. Best-effort; a missing token or parse error is a no-op.
func (a *API) deleteTelegramWebhookForConfig(ctx context.Context, cfgRaw json.RawMessage) {
	if a.telegram == nil || len(cfgRaw) == 0 {
		return
	}
	var cfg model.TelegramConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil || cfg.BotToken == "" {
		return
	}
	if err := a.telegram.DeleteWebhook(ctx, cfg.BotToken); err != nil {
		log.Printf("telegram deleteWebhook failed: %v", err)
	}
}

// TestIntegrationRequest represents a request to test an integration
type TestIntegrationRequest struct {
	Mode string `json:"mode"` // "dm" for direct message test
}

// TestIntegrationResponse represents the response from a test
type TestIntegrationResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// TestIntegration sends a test message via the integration
// @Summary Test integration
// @Description Send a test message via the integration (admin only)
// @Tags integrations
// @Accept json
// @Produce json
// @Param id path string true "Integration ID"
// @Param request body TestIntegrationRequest true "Test request"
// @Success 200 {object} TestIntegrationResponse
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 412 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations/{id}/test [post]
func (a *API) TestIntegration(c echo.Context) error {
	id := c.Param("id")

	// Bind request (mode is optional — inferred from integration type)
	var req TestIntegrationRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Get integration
	integration, err := a.store.GetIntegrationByID(id)
	if err != nil {
		if errors.Is(err, store.ErrIntegrationNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "integration not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if !integration.Enabled {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "integration is disabled"})
	}

	switch integration.Type {
	case model.IntegrationTypeSlack:
		return a.testSlackIntegration(c, integration)
	case model.IntegrationTypeGenericWebhook:
		return a.testWebhookIntegration(c, integration)
	default:
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "test is not supported for this integration type"})
	}
}

func (a *API) testSlackIntegration(c echo.Context, integration *model.Integration) error {
	userID, ok := c.Get("user_id").(string)
	if !ok || userID == "" {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "could not determine current user"})
	}

	if _, err := a.store.GetActiveUserByID(userID); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "could not fetch user"})
	}

	ident, err := a.store.GetExternalIdentity(userID, "slack")
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return c.JSON(http.StatusPreconditionFailed, ErrorResponse{Error: "link your Slack account first"})
	case err != nil:
		// A real DB error must not masquerade as user misconfiguration.
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to load slack identity"})
	case ident == nil || ident.ExternalID == "":
		// Defensive: some store implementations may signal "not found" with (nil, nil).
		return c.JSON(http.StatusPreconditionFailed, ErrorResponse{Error: "link your Slack account first"})
	}

	if a.slack == nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "Slack integration not configured"})
	}

	message := fmt.Sprintf("TokayOps Test: Slack integration \"%s\" is working", integration.Name)
	if err := a.slack.SendDM(c.Request().Context(), ident.ExternalID, message); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: fmt.Sprintf("Slack API error: %v", err)})
	}

	return c.JSON(http.StatusOK, TestIntegrationResponse{OK: true, Message: "Test DM sent"})
}

func (a *API) testWebhookIntegration(c echo.Context, integration *model.Integration) error {
	var cfg model.GenericWebhookConfig
	if err := json.Unmarshal(integration.Config, &cfg); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "invalid webhook config"})
	}

	now := time.Now().UTC()
	payload := model.WebhookEventPayload{
		Event:     string(model.OutboxEventFiring),
		Timestamp: now.Format(time.RFC3339),
		AlertGroup: model.WebhookAlertGroupPayload{
			ID:         "test-" + uuid.New().String()[:8],
			Title:      "Test delivery from Tokay",
			Status:     "firing",
			Severity:   "info",
			TeamID:     "test",
			TeamName:   "Test",
			AlertCount: 1,
			CreatedAt:  now.Format(time.RFC3339),
		},
		Actor: model.WebhookActorPayload{Name: "system"},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to build payload"})
	}

	eventID := "test-" + uuid.New().String()
	headers := outbox.BuildHeaders(eventID, model.OutboxEventFiring, body, cfg.Secret, cfg.CustomHeaders)

	timeout := 30 * time.Second
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}

	allowedCIDRs, _ := config.ParseAllowedPrivateCIDRs()
	sender := outbox.NewHTTPSender(allowedCIDRs)
	ctx, cancel := context.WithTimeout(c.Request().Context(), timeout)
	defer cancel()

	result := sender.Send(ctx, cfg.URL, body, headers)
	if result.Error != nil {
		return c.JSON(http.StatusOK, TestIntegrationResponse{
			OK:      false,
			Message: fmt.Sprintf("Delivery failed: %v", result.Error),
		})
	}

	ok := result.HTTPStatus >= 200 && result.HTTPStatus < 300
	return c.JSON(http.StatusOK, TestIntegrationResponse{
		OK:      ok,
		Message: fmt.Sprintf("Webhook responded with HTTP %d", result.HTTPStatus),
	})
}

// validateIntegrationConfig validates config JSON against the type schema
// declared by the integrations package. isUpdate=true allows empty/masked
// secrets (they merge with existing).
func validateIntegrationConfig(integrationType model.IntegrationType, configJSON json.RawMessage, isUpdate bool) error {
	if len(configJSON) == 0 {
		if isUpdate {
			return nil // No config change on update
		}
		return errors.New("config is required")
	}
	return integrations.ValidateConfig(integrationType, configJSON, isUpdate)
}

// validateWebhookScope validates scope and team_id for integration creation
func validateWebhookScope(integrationType model.IntegrationType, scope *model.WebhookScope, teamID *string) error {
	if integrationType == model.IntegrationTypeGenericWebhook {
		if scope == nil {
			return errors.New("scope is required for generic_webhook")
		}
		if *scope != model.WebhookScopeGlobal && *scope != model.WebhookScopeTeam {
			return fmt.Errorf("invalid scope %q, must be 'global' or 'team'", *scope)
		}
		if *scope == model.WebhookScopeTeam && (teamID == nil || *teamID == "") {
			return errors.New("team_id is required when scope is 'team'")
		}
		if *scope == model.WebhookScopeGlobal && teamID != nil && *teamID != "" {
			return errors.New("team_id must not be set when scope is 'global'")
		}
	} else {
		if scope != nil {
			return errors.New("scope is only valid for generic_webhook integrations")
		}
		if teamID != nil && *teamID != "" {
			return errors.New("team_id is only valid for generic_webhook integrations")
		}
	}
	return nil
}

// reloadIntegrationCache reloads the integration cache if available
func (a *API) reloadIntegrationCache() {
	if a.integrationCache != nil {
		_ = a.integrationCache.LoadAll(a.store)
	}
}

// SlackManifest returns a Slack App Manifest YAML with the correct callback URLs.
// The admin copies this manifest into api.slack.com → Create App → From Manifest.
func (a *API) SlackManifest(c echo.Context) error {
	baseURL := a.selfURL
	if baseURL == "" {
		baseURL = "<SET TOKAY_SELF_URL>"
	}

	manifest := fmt.Sprintf(`display_information:
  name: TokayOps
  description: TokayOps — incident management & alerting gateway
  background_color: "#2C2D30"
features:
  bot_user:
    display_name: Tokay
    always_online: true
oauth_config:
  scopes:
    bot:
      - chat:write
      - chat:write.customize
      - channels:read
      - groups:read
      - users:read
      - users:read.email
      - im:write
settings:
  interactivity:
    is_enabled: true
    request_url: %s/slack/interactive
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
`, baseURL)

	return c.Blob(http.StatusOK, "text/yaml; charset=utf-8", []byte(manifest))
}
