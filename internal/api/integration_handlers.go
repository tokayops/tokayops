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

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/integrations"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbox"
	"github.com/tokayops/tokayops/internal/store"
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
		// The team was validated above and deleted before this write landed.
		// Deleting a team is a locked, deterministic command, so this side is
		// the one that loses the race - and it says so as a 404 rather than
		// handing the caller the name of a foreign key inside a 500.
		if errors.Is(err, store.ErrIntegrationTeamNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload cache if available
	a.reloadIntegrationCache()

	// Restart usergroup syncer if Slack integration was created
	if integration.Type == model.IntegrationTypeSlack {
		a.restartUsergroupSyncer()
	}

	if integration.Type == model.IntegrationTypeTelegram {
		a.reconcileTelegramWebhook(c.Request().Context(), integration.ID)
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
// @Failure 409 {object} ErrorResponse "the integration is being changed by another request; retry"
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations/{id} [put]
func (a *API) UpdateIntegration(c echo.Context) error {
	id := c.Param("id")

	// The middleware's copy serves the validation below and nothing else: the
	// type is immutable, and the URL check is about the request. The row itself
	// is re-read under a lock by the command, and the patch is applied there -
	// applying it to this copy and writing the result back whole would overwrite
	// whatever somebody else changed in between.
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

	var req UpdateIntegrationRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	patch := store.IntegrationPatch{Enabled: req.Enabled}
	if req.Name != "" {
		patch.Name = &req.Name
	}
	if req.Config != nil {
		// Validate config (allow empty/masked secrets on update)
		if err := validateIntegrationConfig(existing.Type, req.Config, true); err != nil {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		}
		patch.Config = req.Config
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

	actor, _ := c.Get("user_id").(string)
	change, err := a.store.UpdateIntegration(c.Request().Context(), id, patch, actor)
	if err != nil {
		return a.integrationCommandFailed(c, err)
	}
	updated := change.After

	// Reload cache
	a.reloadIntegrationCache()

	// Restart usergroup syncer if Slack integration was updated
	if updated.Type == model.IntegrationTypeSlack {
		a.restartUsergroupSyncer()
	}

	// The rows this command saw are candidates for deregistration and nothing
	// more; what is registered is decided by the row as it stands now.
	if updated.Type == model.IntegrationTypeTelegram {
		a.reconcileTelegramWebhook(c.Request().Context(), id, change.Before, change.After)
	}

	return c.JSON(http.StatusOK, integrations.MaskSecrets(updated))
}

// DeleteIntegration deletes an integration
// @Summary Delete integration
// @Description Delete an integration (admin only)
// @Tags integrations
// @Param id path string true "Integration ID"
// @Success 204
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse "the integration is being changed by another request; retry"
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/integrations/{id} [delete]
func (a *API) DeleteIntegration(c echo.Context) error {
	id := c.Param("id")

	actor, _ := c.Get("user_id").(string)
	change, err := a.store.DeleteIntegration(c.Request().Context(), id, actor)
	if err != nil {
		return a.integrationCommandFailed(c, err)
	}
	deleted := change.Before

	// Reload cache
	a.reloadIntegrationCache()

	// Stop usergroup syncer if Slack integration was deleted
	if deleted.Type == model.IntegrationTypeSlack {
		a.restartUsergroupSyncer()
	}

	if deleted.Type == model.IntegrationTypeTelegram {
		a.reconcileTelegramWebhook(c.Request().Context(), id, deleted)
	}

	return c.NoContent(http.StatusNoContent)
}

// integrationCommandFailed answers for a lifecycle command that did not run.
// Busy is a 409 and not a wait: the command already waited as long as a
// command waits, and the caller repeats it against whatever state the other
// command left.
func (a *API) integrationCommandFailed(c echo.Context, err error) error {
	switch {
	case errors.Is(err, store.ErrIntegrationNotFound):
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "integration not found"})
	case errors.Is(err, store.ErrIntegrationBusy):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "integration is being changed by another request, try again"})
	default:
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
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
		log.Printf("telegram: TOKAY_SELF_URL not configured - interactivity disabled: webhook not registered, Ack/Resolve buttons hidden")
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
		// ErrIntegrationNotFound (none configured / disabled) is normal - nothing to do.
		return
	}
	a.reconcileTelegramWebhook(ctx, integ.ID)
}

// reconcileTelegramWebhook brings the Bot API into line with the durable row.
//
// The effect of a lifecycle command is decided by the row as it stands AFTER
// the commit, read under an advisory lock - not by the command's arguments.
// Transactions are serialised; their effects are not. Two instances rotating
// the token T1 -> T2 and T2 -> T3 can run their effects in the reverse order,
// and an effect acting on its own (before, after) would register T2 again after
// T3 had been set; an edit racing a deletion would register the webhook of an
// integration that no longer exists. Under the lock each reconcile reads the
// current row and acts on that, one reconcile's read and calls finish before
// another's begin, and the last one to run sees the newest row.
//
// The rows a command saw contribute CANDIDATES for deregistration, nothing
// more: every token they carry that is not the current enabled one is removed.
// The current token itself is never removed, so T1 -> T2 -> T1 does not take
// the live webhook down. Best-effort throughout, like every Telegram effect
// here: a failure is logged and never fails the request.
func (a *API) reconcileTelegramWebhook(ctx context.Context, id string, seen ...*model.Integration) {
	if a.telegram == nil {
		return
	}
	candidates := telegramTokensOf(seen)
	err := a.store.WithIntegrationLocked(ctx, id, func(current *model.Integration) error {
		keep := ""
		if current != nil && current.Type == model.IntegrationTypeTelegram && current.Enabled {
			keep = telegramTokenOf(current)
		}
		for _, token := range candidates {
			if token == keep {
				continue
			}
			if err := a.telegram.DeleteWebhook(ctx, token); err != nil {
				log.Printf("telegram deleteWebhook failed: %v", err)
			}
		}
		if keep != "" {
			a.setTelegramWebhook(ctx, current)
		}
		return nil
	})
	if err != nil {
		log.Printf("telegram webhook reconcile for integration %s: %v", id, err)
	}
}

// telegramTokensOf is the distinct bot tokens the given rows carry, in order.
func telegramTokensOf(rows []*model.Integration) []string {
	var tokens []string
	seen := map[string]bool{}
	for _, row := range rows {
		token := telegramTokenOf(row)
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		tokens = append(tokens, token)
	}
	return tokens
}

func telegramTokenOf(row *model.Integration) string {
	if row == nil || row.Type != model.IntegrationTypeTelegram {
		return ""
	}
	var cfg model.TelegramConfig
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return ""
	}
	return cfg.BotToken
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

	// Bind request (mode is optional - inferred from integration type)
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
  description: TokayOps - incident management & alerting gateway
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
