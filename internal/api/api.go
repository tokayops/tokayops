package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rbac"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/slacksync"
	"github.com/tokayops/tokayops/internal/store"
)

// SlackMessenger defines the interface for Slack operations needed by the API layer.
type SlackMessenger interface {
	SendDM(ctx context.Context, userID, message string) error
	GetSlackUserIDByEmail(ctx context.Context, email string) (string, error)
	GetEmailBySlackID(ctx context.Context, slackUserID string) (string, error)
}

// TelegramAPI is the slice of the Telegram provider the API layer needs:
// answering callbacks, the /start link confirmation, webhook lifecycle, and the
// bot username for deep links. Satisfied by *telegram.Provider.
type TelegramAPI interface {
	AnswerCallback(ctx context.Context, callbackQueryID, text string) error
	SendText(ctx context.Context, chatID, text string) error
	BotUsername(ctx context.Context) (string, error)
	SetWebhook(ctx context.Context, token, webhookURL, secretToken string) error
	DeleteWebhook(ctx context.Context, token string) error
}

// API provides REST handlers for the TokayOps application.
type API struct {
	store            store.StoreInterface
	agService        *alertgroup.Service
	oidcProvider     *auth.OIDCProvider
	rbac             *rbac.Checker
	slack            SlackMessenger
	integrationCache *store.IntegrationCache
	syncerManager    *slacksync.UsergroupSyncerManager
	syncerCtx        context.Context
	respondEphemeral func(responseURL, text string)
	selfURL          string                     // TokayOps base URL for manifest generation
	providerCaps     ProviderCapabilitiesLookup // capability registry view (read-only)
	telegram         TelegramAPI                // optional, nil = telegram interactivity disabled

	// Schedule configuration is deliberately NOT reached through
	// store.StoreInterface. The revision model is not mirrored into MockStore,
	// and routing it through the same interface would put the configuration back
	// behind the door its own tables were taken out from.
	scheduleConfig   *scheduleconfig.Service
	scheduleRead     scheduleconfig.ScheduleReadRepository
	scheduleRenderer *schedulerender.Service
	userEraser       *erasure.Service
	// deliveryRetentionDays is how long delivery history is kept, for the
	// answer about a delivery that is not in the journal any more; 0 is never.
	deliveryRetentionDays int
}

// NewAPI creates a new API instance. Pass nil for oidc if not using OIDC.
// providerCaps is the channel catalogue as this layer reads it, used by policy
// validation and the GET /providers endpoint. nil is tolerated by individual
// handlers (policy validation falls back to taxonomy-only checks) but the
// production wiring in main.go always supplies it.
func NewAPI(s store.StoreInterface, oidc *auth.OIDCProvider, slack SlackMessenger, cache *store.IntegrationCache, selfURL string, providerCaps ProviderCapabilitiesLookup) *API {
	api := &API{
		store:            s,
		agService:        alertgroup.NewService(s),
		oidcProvider:     oidc,
		rbac:             rbac.NewChecker(s),
		slack:            slack,
		integrationCache: cache,
		selfURL:          selfURL,
		providerCaps:     providerCaps,
	}
	api.respondEphemeral = postResponseURL
	return api
}

// SetScheduleConfigService wires the schedule command side: save, delete,
// override commands and the team-member guard.
func (a *API) SetScheduleConfigService(svc *scheduleconfig.Service) {
	a.scheduleConfig = svc
}

// SetScheduleReadRepository wires the read side the config, revision and
// override endpoints answer from.
func (a *API) SetScheduleReadRepository(repo scheduleconfig.ScheduleReadRepository) {
	a.scheduleRead = repo
}

// SetScheduleRenderer wires the historical renderer, the on-call projection
// and the preview.
func (a *API) SetScheduleRenderer(svc *schedulerender.Service) {
	a.scheduleRenderer = svc
}

// SetDeliveryRetention tells the API the retention window, so that a journal
// that is not there can say why.
func (a *API) SetDeliveryRetention(days int) {
	a.deliveryRetentionDays = days
}

// SetUserEraser wires the user erasure command. Without it DeleteUser has no
// safe implementation and refuses rather than falling back to a hard delete.
func (a *API) SetUserEraser(svc *erasure.Service) {
	a.userEraser = svc
}

// SetTelegram wires the Telegram provider into the API layer so the webhook
// handler can answer callbacks and the integration lifecycle can manage the
// Bot API webhook. nil leaves telegram interactivity disabled.
func (a *API) SetTelegram(t TelegramAPI) {
	a.telegram = t
}

// SetUsergroupSyncerManager sets the usergroup syncer manager for dynamic start/stop.
func (a *API) SetUsergroupSyncerManager(ctx context.Context, manager *slacksync.UsergroupSyncerManager) {
	a.syncerCtx = ctx
	a.syncerManager = manager
}

// restartUsergroupSyncer restarts the usergroup syncer with the current token from cache.
func (a *API) restartUsergroupSyncer() {
	if a.syncerManager == nil || a.syncerCtx == nil || a.integrationCache == nil {
		return
	}

	// Get token from cache (prefer user token, fallback to bot token)
	token := a.integrationCache.GetSlackUserToken()
	if token == "" {
		token = a.integrationCache.GetSlackToken()
	}

	// Start syncer with the token (handles empty token by not starting)
	a.syncerManager.Start(a.syncerCtx, token)
}

// RegisterRoutes registers all API routes on the Echo instance.
func (a *API) RegisterRoutes(e *echo.Echo) {
	// Auth Routes (Public)
	authGroup := e.Group("/api/auth")
	authGroup.POST("/login", a.Login)
	authGroup.POST("/logout", a.Logout)
	authGroup.GET("/me", a.AuthMiddleware(a.Me))
	authGroup.POST("/logout", a.Logout)
	authGroup.GET("/me", a.AuthMiddleware(a.Me))
	authGroup.PATCH("/me", a.AuthMiddleware(a.UpdateMe))
	// Slack Binding Routes
	authGroup.POST("/me/slack/request-code", a.AuthMiddleware(a.RequestSlackCode))
	authGroup.POST("/me/slack/confirm-code", a.AuthMiddleware(a.ConfirmSlackCode))
	authGroup.DELETE("/me/slack", a.AuthMiddleware(a.UnbindSlack))
	// Telegram Binding Routes (deep-link, not OTP)
	authGroup.POST("/me/telegram/link", a.AuthMiddleware(a.RequestTelegramLink))
	authGroup.DELETE("/me/telegram", a.AuthMiddleware(a.UnbindTelegram))

	// OIDC Routes (Public)
	authGroup.GET("/oidc/config", a.OIDCConfig)
	authGroup.GET("/oidc/redirect", a.OIDCRedirect)
	authGroup.GET("/oidc/callback", a.OIDCCallback)

	// API V1 (Protected)
	v1 := e.Group("/api/v1")
	v1.Use(a.AuthMiddleware)

	// Alert Groups
	v1.GET("/alert-groups", a.ListAlertGroups, a.Require(rbac.ActionAlertView, ScopeGlobal()))
	v1.POST("/alert-groups", a.CreateManualAlertGroup, a.Require(rbac.ActionAlertCreate, ScopeFromBody("team_id")))
	v1.GET("/alert-groups/:id", a.GetAlertGroup, a.Require(rbac.ActionAlertView, ScopeGlobal()))
	v1.PATCH("/alert-groups/:id/ack", a.AckAlertGroup, a.Require(rbac.ActionAlertAck, ScopeFromResource("alert_group", "id")))
	v1.PATCH("/alert-groups/:id/resolve", a.ResolveAlertGroup, a.Require(rbac.ActionAlertResolve, ScopeFromResource("alert_group", "id")))
	v1.GET("/alert-groups/:id/timeline", a.GetAlertGroupTimeline, a.Require(rbac.ActionAlertView, ScopeFromResource("alert_group", "id")))
	v1.POST("/alert-groups/:id/notes", a.AddAlertGroupNote, a.Require(rbac.ActionAlertNoteAdd, ScopeFromResource("alert_group", "id")))
	// The group's deliveries go under the same action and scope as its
	// timeline: whoever may read "notification sent" may read to whom.
	v1.GET("/alert-groups/:id/deliveries", a.GetAlertGroupDeliveries, a.Require(rbac.ActionAlertView, ScopeFromResource("alert_group", "id")))

	// The delivery journal: every family, every team, one form. The journal of
	// one commitment carries the attempts and their addresses, which is why
	// it is the administrator's and not the group's.
	v1.GET("/deliveries", a.ListDeliveries, a.Require(rbac.ActionDeliveryView, ScopeGlobal()))
	v1.GET("/deliveries/:id", a.GetDeliveryJournal, a.Require(rbac.ActionDeliveryView, ScopeGlobal()))
	v1.POST("/deliveries/:id/decisions", a.DecideDelivery, a.Require(rbac.ActionDeliveryResolve, ScopeGlobal()))

	// Legacy Incidents (alias)
	v1.GET("/incidents", a.ListAlertGroups, a.Require(rbac.ActionAlertView, ScopeGlobal()))
	v1.GET("/incidents/:id", a.GetAlertGroup, a.Require(rbac.ActionAlertView, ScopeGlobal()))
	v1.PATCH("/incidents/:id/ack", a.AckAlertGroup, a.Require(rbac.ActionAlertAck, ScopeFromResource("alert_group", "id")))
	v1.PATCH("/incidents/:id/resolve", a.ResolveAlertGroup, a.Require(rbac.ActionAlertResolve, ScopeFromResource("alert_group", "id")))
	v1.GET("/incidents/:id/timeline", a.GetAlertGroupTimeline, a.Require(rbac.ActionAlertView, ScopeFromResource("alert_group", "id")))
	v1.POST("/incidents/:id/notes", a.AddAlertGroupNote, a.Require(rbac.ActionAlertNoteAdd, ScopeFromResource("alert_group", "id")))

	// Teams
	v1.GET("/teams", a.ListTeams, a.Require(rbac.ActionTeamList, ScopeGlobal()))
	v1.POST("/teams", a.CreateTeam, a.Require(rbac.ActionTeamCreate, ScopeGlobal()))
	v1.GET("/teams/:id", a.GetTeam, a.Require(rbac.ActionTeamView, ScopeGlobal())) // or TeamScope, but View is public
	v1.PUT("/teams/:id", a.UpdateTeam, a.Require(rbac.ActionTeamUpdate, ScopeFromResource("team", "id")))
	// requireScheduleStack because deleting a team is now a schedule-aware
	// command: what retains a team is its schedule history.
	v1.DELETE("/teams/:id", a.DeleteTeam, a.requireScheduleStack, a.Require(rbac.ActionTeamDelete, ScopeGlobal()))
	v1.GET("/teams/:id/alert-groups", a.GetTeamAlertGroups, a.Require(rbac.ActionAlertView, ScopeGlobal())) // List is global/public
	v1.GET("/teams/:id/incidents", a.GetTeamAlertGroups, a.Require(rbac.ActionAlertView, ScopeGlobal()))
	v1.GET("/teams/:id/members", a.GetTeamMembers, a.Require(rbac.ActionTeamView, ScopeFromResource("team", "id")))
	v1.POST("/teams/:id/members", a.AddTeamMember, a.Require(rbac.ActionTeamMemberAdd, ScopeFromResource("team", "id")))
	v1.DELETE("/teams/:id/members/:user_id", a.RemoveTeamMember, a.requireScheduleStack, a.Require(rbac.ActionTeamMemberRemove, ScopeFromResource("team", "id")))

	// Users
	v1.GET("/users", a.ListUsers, a.Require(rbac.ActionUserList, ScopeGlobal()))
	v1.GET("/users/:id", a.GetUser, a.Require(rbac.ActionUserView, ScopeGlobal()))
	// Display read for historical data: it answers for erased users, which
	// GET /users/:id deliberately does not. Same permission as viewing a user,
	// and it returns strictly less.
	v1.POST("/users/resolve", a.ResolveUsers, a.Require(rbac.ActionUserView, ScopeGlobal()))
	v1.POST("/users", a.CreateUser, a.Require(rbac.ActionUserCreate, ScopeGlobal()))
	v1.PUT("/users/:id", a.UpdateUser, a.Require(rbac.ActionUserUpdate, ScopeGlobal()))
	v1.PUT("/users/:id/password", a.UpdateUserPassword, a.Require(rbac.ActionUserPasswordUpdate, ScopeUserSelfOrAdmin("id")))
	v1.DELETE("/users/:id", a.DeleteUser, a.requireUserEraser, a.Require(rbac.ActionUserDelete, ScopeGlobal()))

	// Schedule configuration (revision model).
	//
	// preview and the revision endpoints require schedule.edit rather than
	// schedule.view: a preview takes the same payload as a save and is a
	// write-intent tool, and revisions carry created_by and change_reason.
	// Loosening a permission later is easy; tightening one is not.
	v1.GET("/teams/:id/schedule/config", a.GetScheduleConfig, a.requireScheduleStack, a.Require(rbac.ActionScheduleView, ScopeFromResource("team", "id")))
	v1.PUT("/teams/:id/schedule/config", a.PutScheduleConfig, a.requireScheduleStack, a.Require(rbac.ActionScheduleEdit, ScopeFromResource("team", "id")))
	v1.POST("/teams/:id/schedule/preview", a.PostSchedulePreview, a.requireScheduleStack, a.Require(rbac.ActionScheduleEdit, ScopeFromResource("team", "id")))
	v1.DELETE("/teams/:id/schedule", a.DeleteTeamSchedule, a.requireScheduleStack, a.Require(rbac.ActionScheduleEdit, ScopeFromResource("team", "id")))
	v1.GET("/teams/:id/schedule/revisions", a.ListScheduleRevisions, a.requireScheduleStack, a.Require(rbac.ActionScheduleEdit, ScopeFromResource("team", "id")))
	v1.GET("/teams/:id/schedule/revisions/:revision_id", a.GetScheduleRevision, a.requireScheduleStack, a.Require(rbac.ActionScheduleEdit, ScopeFromResource("team", "id")))
	v1.GET("/teams/:id/schedule/render", a.RenderSchedule, a.requireScheduleStack, a.Require(rbac.ActionScheduleView, ScopeFromResource("team", "id")))
	v1.GET("/teams/:id/schedule/on-call", a.GetScheduleOnCall, a.requireScheduleStack, a.Require(rbac.ActionScheduleView, ScopeFromResource("team", "id")))
	v1.GET("/teams/:id/schedule/overrides", a.ListScheduleOverrides, a.requireScheduleStack, a.Require(rbac.ActionScheduleView, ScopeFromResource("team", "id")))
	v1.POST("/teams/:id/schedule/overrides", a.CreateScheduleOverride, a.requireScheduleStack, a.Require(rbac.ActionOverrideCreate, ScopeFromResource("team", "id")))
	v1.PUT("/schedules/:schedule_id/overrides/:id", a.UpdateScheduleOverride, a.requireScheduleStack, a.Require(rbac.ActionOverrideUpdate, ScopeScheduleOverride("schedule_id", "id")))
	v1.DELETE("/schedules/:schedule_id/overrides/:id", a.DeleteScheduleOverride, a.requireScheduleStack, a.Require(rbac.ActionOverrideDelete, ScopeScheduleOverride("schedule_id", "id")))

	// API Tokens
	// These usually belong to user. Assuming self-management for now.
	// rbac.go didn't list explicit actions for tokens, but typically they are self-service.
	// For now, leaving as is (handled inside handler? or add implicit check).
	// api.go ListAPITokens usually filters by current user.
	// Let's check ListAPITokens implementation later.
	// API Tokens
	v1.GET("/tokens", a.ListAPITokens, a.Require(rbac.ActionTokenList, ScopeCurrentUser()))
	v1.POST("/tokens", a.CreateAPIToken, a.Require(rbac.ActionTokenCreate, ScopeCurrentUser()))
	v1.DELETE("/tokens/:id", a.DeleteAPIToken, a.Require(rbac.ActionTokenDelete, ScopeFromResource("token", "id")))

	// Providers: read-only capability discovery for the policy editor.
	v1.GET("/providers", a.ListProviders)

	// Escalation Policies (Phase 4)
	v1.GET("/policies", a.ListPolicies, a.Require(rbac.ActionPolicyList, ScopeGlobal()))
	v1.GET("/policies/:id", a.GetPolicy, a.Require(rbac.ActionPolicyView, ScopeFromResource("policy", "id")))
	v1.POST("/policies", a.CreatePolicy, a.Require(rbac.ActionPolicyCreate, ScopeFromBody("team_id")))
	v1.PUT("/policies/:id", a.UpdatePolicy, a.Require(rbac.ActionPolicyUpdate, ScopeFromResource("policy", "id")))
	v1.DELETE("/policies/:id", a.DeletePolicy, a.Require(rbac.ActionPolicyDelete, ScopeFromResource("policy", "id")))

	// Integrations (Admin + team_admin for team-scoped webhooks)
	v1.GET("/integrations/slack/manifest", a.SlackManifest, a.Require(rbac.ActionIntegrationCreate, ScopeGlobal()))
	v1.GET("/integrations", a.ListIntegrations) // handler-level auth: admin sees all, team_admin sees own team webhooks
	v1.GET("/integrations/:id", a.GetIntegration, a.Require(rbac.ActionIntegrationView, ScopeFromIntegration("id")))
	v1.POST("/integrations", a.CreateIntegration, a.Require(rbac.ActionIntegrationCreate, ScopeIntegrationCreate()))
	v1.PUT("/integrations/:id", a.UpdateIntegration, a.Require(rbac.ActionIntegrationUpdate, ScopeFromIntegration("id")))
	v1.DELETE("/integrations/:id", a.DeleteIntegration, a.Require(rbac.ActionIntegrationDelete, ScopeFromIntegration("id")))
	v1.POST("/integrations/:id/test", a.TestIntegration, a.Require(rbac.ActionIntegrationUpdate, ScopeFromIntegration("id")))

	// Delivery logs and replay
	// The two reading routes resolve through the tombstone once the integration
	// is gone; the replay does not - it makes a new delivery, and a deleted
	// subscriber gets none.
	v1.GET("/integrations/:id/deliveries", a.ListIntegrationDeliveries, a.Require(rbac.ActionIntegrationView, ScopeFromIntegrationHistory("id")))
	v1.GET("/integrations/:id/deliveries/:deliveryId", a.GetDeliveryDetail, a.Require(rbac.ActionIntegrationView, ScopeFromIntegrationHistory("id")))
	v1.POST("/integrations/:id/deliveries/:deliveryId/replay", a.ReplayDelivery, a.Require(rbac.ActionIntegrationUpdate, ScopeFromIntegration("id")))

	// Slack Interactive (Public - no AuthMiddleware, uses Slack signature verification)
	e.POST("/slack/interactive", a.HandleSlackInteractive, a.SlackSignatureMiddleware)

	// Telegram webhook (Public - no AuthMiddleware, uses X-Telegram-Bot-Api-Secret-Token verification)
	e.POST("/telegram/webhook", a.HandleTelegramWebhook, a.TelegramSecretMiddleware)
}

// AlertGroupListResponse represents a list of alert groups.
type AlertGroupListResponse struct {
	AlertGroups []*model.AlertGroup `json:"alert_groups"`
	Incidents   []*model.AlertGroup `json:"incidents"` // Legacy
	Total       int                 `json:"total"`
}

// AlertGroupSummaryListResponse represents a lightweight list of alert groups for list views.
type AlertGroupSummaryListResponse struct {
	AlertGroups []*model.AlertGroupSummary `json:"alert_groups"`
	Total       int                        `json:"total"`
	Page        int                        `json:"page"`
	TotalPages  int                        `json:"total_pages"`
	HasNext     bool                       `json:"has_next"`
	HasPrev     bool                       `json:"has_prev"`
}

// paginationMeta computes clamped page, offset, totalPages, hasNext, hasPrev.
// Limit is clamped to [1, 200], page to [1, totalPages].
// When total=0: page=1, totalPages=1, hasNext=false, hasPrev=false.
func paginationMeta(total, page, limit int) (clampedPage, offset, totalPages int, hasNext, hasPrev bool) {
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	totalPages = (total + limit - 1) / limit
	if totalPages < 1 {
		totalPages = 1
	}
	clampedPage = page
	if clampedPage < 1 {
		clampedPage = 1
	}
	if clampedPage > totalPages {
		clampedPage = totalPages
	}
	offset = (clampedPage - 1) * limit
	hasNext = clampedPage < totalPages
	hasPrev = clampedPage > 1
	return
}

type TeamMemberListResponse struct {
	Users []*model.TeamMemberDetail `json:"users"`
	Total int                       `json:"total"`
}

// ErrorResponse represents an error response.
//
// Code is the machine-readable half. A status alone is not a contract: the
// schedule editor answers 409 in six different senses, and each one needs a
// different reaction from the client - reload the form, reload the override
// list, show the conflicting intervals, switch to recreate. A client that had
// to tell them apart by parsing the message would break on the first
// rewording. Codes are defined in schedule_errors.go; the field is omitempty
// so responses that predate it are unchanged on the wire.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// ManualAlertGroupRequest represents a request to create a manual alert group.
type ManualAlertGroupRequest struct {
	TeamID   string `json:"team_id"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
}

// ListAlertGroups godoc
// @Summary List alert groups
// @Description Get a list of alert groups with optional status filter and pagination
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param status query string false "Filter by status (new, processing, triggered, resolved, closed)"
// @Param limit query int false "Limit (max 200)" default(50)
// @Param page query int false "Page number (1-indexed)" default(1)
// @Success 200 {object} AlertGroupListResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups [get]
func (a *API) ListAlertGroups(c echo.Context) error {
	// Parse query params
	limitStr := c.QueryParam("limit")
	pageStr := c.QueryParam("page")
	viewStr := c.QueryParam("view")
	daysStr := c.QueryParam("days")

	limit := 50
	page := 1
	days := 0

	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}
	if limit > 200 {
		limit = 200
	}
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}
	if daysStr != "" {
		if d, err := strconv.Atoi(daysStr); err == nil && d > 0 {
			days = d
		}
	}

	// Parse statuses (comma-separated, new param) with fallback to legacy "status"
	var statuses []model.AlertGroupStatus
	statusesStr := c.QueryParam("statuses")
	if statusesStr != "" {
		for _, s := range strings.Split(statusesStr, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				statuses = append(statuses, model.AlertGroupStatus(s))
			}
		}
	} else if statusStr := c.QueryParam("status"); statusStr != "" {
		statuses = []model.AlertGroupStatus{model.AlertGroupStatus(statusStr)}
	}

	// Parse severity (comma-separated)
	var severities []string
	severityStr := c.QueryParam("severity")
	if severityStr != "" {
		for _, s := range strings.Split(severityStr, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				severities = append(severities, s)
			}
		}
	}

	sortBy := c.QueryParam("sort")
	sortDir := c.QueryParam("sort_dir")

	// Summary view: lightweight response without heavy JSONB fields
	if viewStr == "summary" {
		total, err := a.store.CountAlertGroupSummaries(statuses, severities, days)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		clampedPage, clampedOffset, totalPages, hasNext, hasPrev := paginationMeta(total, page, limit)
		summaries, err := a.store.ListAlertGroupSummaries(statuses, severities, days, limit, clampedOffset, sortBy, sortDir)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		if summaries == nil {
			summaries = []*model.AlertGroupSummary{}
		}
		return c.JSON(http.StatusOK, AlertGroupSummaryListResponse{
			AlertGroups: summaries,
			Total:       total,
			Page:        clampedPage,
			TotalPages:  totalPages,
			HasNext:     hasNext,
			HasPrev:     hasPrev,
		})
	}

	// Legacy full view: use first status if provided
	offset := (page - 1) * limit
	var status *model.AlertGroupStatus
	if len(statuses) > 0 {
		status = &statuses[0]
	}

	alertGroups, total, err := a.store.GetAllAlertGroups(status, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if alertGroups == nil {
		alertGroups = []*model.AlertGroup{}
	}

	return c.JSON(http.StatusOK, AlertGroupListResponse{
		AlertGroups: alertGroups,
		Incidents:   alertGroups, // Legacy compatibility
		Total:       total,
	})
}

// CreateManualAlertGroup godoc
// @Summary Create a manual alert group
// @Description Create a manual alert group that routes via team+severity policy mapping
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param body body ManualAlertGroupRequest true "Manual alert group data"
// @Success 201 {object} model.AlertGroup
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups [post]
func (a *API) CreateManualAlertGroup(c echo.Context) error {
	var req ManualAlertGroupRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	teamID := strings.TrimSpace(req.TeamID)
	if teamID == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "team_id is required"})
	}

	team, err := a.store.GetTeamByID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	severity := strings.ToLower(strings.TrimSpace(req.Severity))
	if severity == "" {
		severity = "info"
	}
	if severity != "critical" && severity != "warning" && severity != "info" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid severity: must be critical, warning, or info"})
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Manual Alert"
	}
	const manualPrefix = "[MANUAL]"
	if strings.HasPrefix(strings.ToUpper(title), manualPrefix) {
		title = manualPrefix + strings.TrimSpace(title[len(manualPrefix):])
	} else {
		title = manualPrefix + " " + title
	}

	now := time.Now()
	alert := model.Alert{
		Fingerprint: uuid.New().String(),
		Status:      model.AlertStatusFiring,
		Labels: map[string]string{
			"alertname": title,
			"severity":  severity,
			"team":      teamID,
		},
		Annotations: map[string]string{
			"summary":     title,
			"description": "Manual alert group created from UI",
		},
		StartsAt: now,
		EndsAt:   now,
	}

	ag := &model.AlertGroup{
		ID:               uuid.New().String(),
		AlertKey:         "manual:" + uuid.New().String(),
		Status:           model.AlertGroupStatusNew,
		Title:            title,
		TeamID:           teamID,
		TeamNameSnapshot: team.Name,
		Severity:         severity,
		Alerts:           []model.Alert{alert},
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	if err := a.store.CreateAlertGroup(ag); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	actor := "user"
	if userID, ok := c.Get("user_id").(string); ok && userID != "" {
		// Display read on purpose: this names the actor in a timeline entry,
		// and an erased actor still has to render as something.
		if user, err := a.store.GetUserByID(userID); err == nil && user != nil {
			if user.Name != "" {
				actor = user.Name
			} else if user.Email != "" {
				actor = user.Email
			} else {
				actor = user.ID
			}
		} else {
			actor = userID
		}
	}

	a.addTimelineEvent(ag.ID, model.TimelineEventCreated, "Manual alert group created: "+title, actor)
	a.addTimelineEvent(ag.ID, model.TimelineEventAlertAdded, "Alert: "+title, actor)

	return c.JSON(http.StatusCreated, ag)
}

// GetAlertGroup godoc
// @Summary Get an alert group by ID
// @Description Get detailed information about a specific alert group
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param id path string true "Alert Group ID"
// @Success 200 {object} model.AlertGroup
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups/{id} [get]
func (a *API) GetAlertGroup(c echo.Context) error {
	id := c.Param("id")

	alertGroup, err := a.store.GetAlertGroupByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "alert group not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, alertGroup)
}

// AckAlertGroup godoc
// @Summary Acknowledge an alert group
// @Description Mark an alert group as acknowledged
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param id path string true "Alert Group ID"
// @Success 200 {object} model.AlertGroup
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups/{id}/ack [patch]
func (a *API) AckAlertGroup(c echo.Context) error {
	id := c.Param("id")
	actor := a.resolveRESTActor(c)

	result, err := a.agService.Ack(id, actor, nil)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	if result.Outcome == alertgroup.OutcomeNotFound {
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "alert group not found"})
	}
	return c.JSON(http.StatusOK, result.AlertGroup)
}

// ResolveAlertGroup godoc
// @Summary Resolve an alert group
// @Description Manually resolve an alert group
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param id path string true "Alert Group ID"
// @Success 200 {object} model.AlertGroup
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups/{id}/resolve [patch]
func (a *API) ResolveAlertGroup(c echo.Context) error {
	id := c.Param("id")
	actor := a.resolveRESTActor(c)

	result, err := a.agService.Resolve(id, actor, nil)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	if result.Outcome == alertgroup.OutcomeNotFound {
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "alert group not found"})
	}
	return c.JSON(http.StatusOK, result.AlertGroup)
}

// resolveRESTActor resolves the authenticated user from JWT context into an alertgroup.Actor.
func (a *API) resolveRESTActor(c echo.Context) alertgroup.Actor {
	userID, _ := c.Get("user_id").(string)
	actor := alertgroup.Actor{ID: userID, Name: "user"}
	if userID != "" {
		// Display read on purpose, as above: this is an audit label.
		if user, err := a.store.GetUserByID(userID); err == nil && user != nil {
			actor.Email = user.Email
			if user.Name != "" {
				actor.Name = user.Name
			} else if user.Email != "" {
				actor.Name = user.Email
			} else {
				actor.Name = user.ID
			}
		} else {
			actor.Name = userID
		}
	}
	return actor
}

// addTimelineEvent is a helper to add timeline events from API
func (a *API) addTimelineEvent(alertGroupID string, eventType model.TimelineEventType, message, actor string) {
	event := &model.TimelineEvent{
		ID:           uuid.New().String(),
		AlertGroupID: alertGroupID,
		Type:         eventType,
		Message:      message,
		Actor:        actor,
		CreatedAt:    time.Now(),
	}
	a.store.AddTimelineEvent(event)
}

// ========================================
// Timeline API
// ========================================

// TimelineResponse represents a list of timeline events.
type TimelineResponse struct {
	Events []*model.TimelineEvent `json:"events"`
	Total  int                    `json:"total"`
}

// AddNoteRequest represents a request to add a note to an alert group.
type AddNoteRequest struct {
	Message string `json:"message"` // Required
	Actor   string `json:"actor"`   // Optional, defaults to "user"
}

// GetAlertGroupTimeline godoc
// @Summary Get timeline for an alert group
// @Description Get all timeline events for a specific alert group
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param id path string true "Alert Group ID"
// @Success 200 {object} TimelineResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups/{id}/timeline [get]
func (a *API) GetAlertGroupTimeline(c echo.Context) error {
	id := c.Param("id")

	// Verify alert group exists
	_, err := a.store.GetAlertGroupByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "alert group not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	events, err := a.store.GetTimelineEvents(id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if events == nil {
		events = []*model.TimelineEvent{}
	}

	return c.JSON(http.StatusOK, TimelineResponse{
		Events: events,
		Total:  len(events),
	})
}

// AddAlertGroupNote godoc
// @Summary Add a note to an alert group
// @Description Add a user note to an alert group's timeline
// @Tags alert-groups
// @Accept json
// @Produce json
// @Param id path string true "Alert Group ID"
// @Param note body AddNoteRequest true "Note data"
// @Success 201 {object} model.TimelineEvent
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/alert-groups/{id}/notes [post]
func (a *API) AddAlertGroupNote(c echo.Context) error {
	id := c.Param("id")

	// Verify alert group exists
	_, err := a.store.GetAlertGroupByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "alert group not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req AddNoteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	if req.Message == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "message is required"})
	}

	actor := req.Actor
	if actor == "" {
		actor = "user"
	}

	event := &model.TimelineEvent{
		ID:           uuid.New().String(),
		AlertGroupID: id,
		Type:         model.TimelineEventNote,
		Message:      req.Message,
		Actor:        actor,
		CreatedAt:    time.Now(),
	}

	if err := a.store.AddTimelineEvent(event); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusCreated, event)
}

// ========================================
// Teams API
// ========================================

// TeamWithConfig extends Team with configuration status fields
type TeamWithConfig struct {
	*model.Team
	MemberCount      int  `json:"member_count"`
	OnCallConfigured bool `json:"on_call_configured"`
}

// TeamListResponse represents a list of teams.
type TeamListResponse struct {
	Teams []*TeamWithConfig `json:"teams"`
	Total int               `json:"total"`
}

// teamsWithSchedule answers which of these teams have a live schedule.
//
// Absent and soft-deleted schedules are "not configured", and any OTHER
// repository error fails the request. Swallowing errors is what produced an
// earlier defect here - a discarded error made every configured team report
// itself unconfigured - and it is not reproduced.
//
// A root with no history horizon is the one place this file answers quietly.
// It cannot be produced by any live path (the create flow writes the horizon in
// the same statement as the row), so it means an upgrade whose destructive
// schedule reset was skipped. Everywhere else that state is an invariant
// violation, but a list of teams must stay answerable: failing the whole page
// because one row is corrupt is the same blast radius that was taken out of the
// notifier tick. It is logged, and the schedule endpoints of that one team say
// plainly what is wrong.
//
// One snapshot for the whole page, not one per team: the list is a single
// answer, and reading each team in its own transaction would let a save
// between two of them show a state no instant ever had.
func (a *API) teamsWithSchedule(ctx context.Context, teams []*model.Team) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(teams))
	// The schedule stack is optional wiring; without it there is nothing to
	// report and an empty set is the honest answer.
	if a.scheduleRead == nil {
		return out, nil
	}

	// One query for the whole page, not one per team: the list is bounded by
	// the number of teams, and a read per row is how a page gets slower every
	// time somebody adds a team.
	wanted := make(map[string]struct{}, len(teams))
	for _, team := range teams {
		wanted[team.ID] = struct{}{}
	}

	err := a.scheduleRead.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		roots, err := view.ListScheduleRoots(ctx)
		if err != nil {
			return err
		}
		for i := range roots {
			root := &roots[i]
			if _, asked := wanted[root.TeamID]; !asked {
				continue
			}
			if root.DeletedAt != nil {
				continue
			}
			out[root.TeamID] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListTeams godoc
// @Summary List teams
// @Description Get all teams with configuration status
// @Tags teams
// @Accept json
// @Produce json
// @Success 200 {object} TeamListResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams [get]
func (a *API) ListTeams(c echo.Context) error {
	teams, err := a.store.GetAllTeams()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if teams == nil {
		teams = []*model.Team{}
	}

	configured, err := a.teamsWithSchedule(c.Request().Context(), teams)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error(), Code: CodeInternal})
	}

	// Enrich teams with configuration status
	enrichedTeams := make([]*TeamWithConfig, len(teams))
	for i, team := range teams {
		// Get member count
		members, _ := a.store.GetTeamMembers(team.ID)
		memberCount := 0
		if members != nil {
			memberCount = len(members)
		}

		_, onCallConfigured := configured[team.ID]

		enrichedTeams[i] = &TeamWithConfig{
			Team:             team,
			MemberCount:      memberCount,
			OnCallConfigured: onCallConfigured,
		}
	}

	return c.JSON(http.StatusOK, TeamListResponse{
		Teams: enrichedTeams,
		Total: len(enrichedTeams),
	})
}

// CreateTeamRequest represents a request to create a team.
type CreateTeamRequest struct {
	ID          string `json:"id"`          // Required
	Name        string `json:"name"`        // Required
	Description string `json:"description"` // Optional
}

// CreateTeam godoc
// @Summary Create a new team
// @Description Create a new team
// @Tags teams
// @Accept json
// @Produce json
// @Param team body CreateTeamRequest true "Team data"
// @Success 201 {object} model.Team
// @Failure 400 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams [post]
func (a *API) CreateTeam(c echo.Context) error {

	var req CreateTeamRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	if req.ID == "" || req.Name == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "id and name are required"})
	}

	// Check if team already exists
	_, err := a.store.GetTeamByID(req.ID)
	if err == nil {
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "team already exists"})
	}

	team := &model.Team{
		ID:          req.ID,
		Name:        req.Name,
		Description: req.Description,
	}

	if err := a.store.CreateTeam(team); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload to get CreatedAt
	created, _ := a.store.GetTeamByID(req.ID)
	if created != nil {
		team = created
	}

	return c.JSON(http.StatusCreated, team)
}

// GetTeam godoc
// @Summary Get a team by ID
// @Description Get detailed information about a specific team
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} model.Team
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id} [get]
func (a *API) GetTeam(c echo.Context) error {
	id := c.Param("id")

	team, err := a.store.GetTeamByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, team)
}

// UpdateTeamRequest represents a request to update a team.
type UpdateTeamRequest struct {
	Name            string            `json:"name"`
	Description     string            `json:"description"`
	DefaultPolicyID string            `json:"default_policy_id"`
	SeverityRoutes  map[string]string `json:"severity_routes"`
}

// UpdateTeam godoc
// @Summary Update a team
// @Description Update team details
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param team body UpdateTeamRequest true "Team data"
// @Success 200 {object} model.Team
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id} [put]
func (a *API) UpdateTeam(c echo.Context) error {
	id := c.Param("id")

	// Check if team exists
	team, err := a.store.GetTeamByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req UpdateTeamRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Validate Policies
	if req.DefaultPolicyID != "" {
		if _, err := a.store.GetEscalationPolicyByID(req.DefaultPolicyID); err != nil {
			if err == sql.ErrNoRows {
				return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("default policy %s not found", req.DefaultPolicyID)})
			}
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		team.DefaultPolicyID = req.DefaultPolicyID
	}

	if req.SeverityRoutes != nil {
		for sev, policyID := range req.SeverityRoutes {
			if _, err := a.store.GetEscalationPolicyByID(policyID); err != nil {
				if err == sql.ErrNoRows {
					return c.JSON(http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("policy %s for severity %s not found", policyID, sev)})
				}
				return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
			}
		}
		team.SeverityRoutes = req.SeverityRoutes
	}

	// Update fields
	if req.Name != "" {
		team.Name = req.Name
	}
	team.Description = req.Description

	if err := a.store.UpdateTeam(team); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload
	updated, _ := a.store.GetTeamByID(id)
	if updated != nil {
		team = updated
	}

	return c.JSON(http.StatusOK, team)
}

// DeleteTeam godoc
// @Summary Delete a team
// @Description Delete a team by ID
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Success 204
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id} [delete]
func (a *API) DeleteTeam(c echo.Context) error {
	// Existence is answered inside the transaction, by the row lock, rather
	// than by a read before it: a team can be deleted between the two, and
	// then a 404 would come back as a 500 from the write that followed.
	if err := a.scheduleConfig.DeleteTeam(c.Request().Context(), c.Param("id")); err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// GetTeamAlertGroups godoc
// @Summary Get alert groups for a team
// @Description Get all alert groups belonging to a specific team
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param limit query int false "Limit (max 200)" default(50)
// @Param page query int false "Page number (1-indexed)" default(1)
// @Success 200 {object} AlertGroupListResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id}/alert-groups [get]
func (a *API) GetTeamAlertGroups(c echo.Context) error {
	id := c.Param("id")

	// Verify team exists
	_, err := a.store.GetTeamByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	limit := 50
	page := 1
	days := 0

	if l, err := strconv.Atoi(c.QueryParam("limit")); err == nil && l > 0 {
		limit = l
	}
	if limit > 200 {
		limit = 200
	}
	if p, err := strconv.Atoi(c.QueryParam("page")); err == nil && p > 0 {
		page = p
	}
	if d, err := strconv.Atoi(c.QueryParam("days")); err == nil && d > 0 {
		days = d
	}

	// Parse statuses (comma-separated, new param) with fallback to legacy "status"
	var statuses []model.AlertGroupStatus
	statusesStr := c.QueryParam("statuses")
	if statusesStr != "" {
		for _, s := range strings.Split(statusesStr, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				statuses = append(statuses, model.AlertGroupStatus(s))
			}
		}
	} else if statusStr := c.QueryParam("status"); statusStr != "" {
		statuses = []model.AlertGroupStatus{model.AlertGroupStatus(statusStr)}
	}

	// Parse severity (comma-separated)
	var severities []string
	severityStr := c.QueryParam("severity")
	if severityStr != "" {
		for _, s := range strings.Split(severityStr, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				severities = append(severities, s)
			}
		}
	}

	viewStr := c.QueryParam("view")
	sortBy := c.QueryParam("sort")
	sortDir := c.QueryParam("sort_dir")

	// Summary view: lightweight response
	if viewStr == "summary" {
		total, err := a.store.CountTeamAlertGroupSummaries(id, statuses, severities, days)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		clampedPage, clampedOffset, totalPages, hasNext, hasPrev := paginationMeta(total, page, limit)
		summaries, err := a.store.ListTeamAlertGroupSummaries(id, statuses, severities, days, limit, clampedOffset, sortBy, sortDir)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		if summaries == nil {
			summaries = []*model.AlertGroupSummary{}
		}
		return c.JSON(http.StatusOK, AlertGroupSummaryListResponse{
			AlertGroups: summaries,
			Total:       total,
			Page:        clampedPage,
			TotalPages:  totalPages,
			HasNext:     hasNext,
			HasPrev:     hasPrev,
		})
	}

	offset := (page - 1) * limit
	alertGroups, total, err := a.store.GetAlertGroupsByTeam(id, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if alertGroups == nil {
		alertGroups = []*model.AlertGroup{}
	}

	return c.JSON(http.StatusOK, AlertGroupListResponse{
		AlertGroups: alertGroups,
		Incidents:   alertGroups, // Legacy compatibility
		Total:       total,
	})
}

// GetTeamMembers godoc
// @Summary Get team members
// @Description Get all users belonging to a specific team
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Success 200 {object} TeamMemberListResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id}/members [get]
func (a *API) GetTeamMembers(c echo.Context) error {
	id := c.Param("id")

	// Verify team exists
	_, err := a.store.GetTeamByID(id)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	members, err := a.store.GetTeamMembers(id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if members == nil {
		members = []*model.TeamMemberDetail{}
	}

	return c.JSON(http.StatusOK, TeamMemberListResponse{
		Users: members,
		Total: len(members),
	})
}

// ========================================
// Users API
// ========================================

// UserListResponse represents a list of users.
type UserListResponse struct {
	Users []*model.User `json:"users"`
	Total int           `json:"total"`
}

// ListUsers godoc
// @Summary List users
// @Description Get all users
// @Tags users
// @Accept json
// @Produce json
// @Success 200 {object} UserListResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/users [get]
func (a *API) ListUsers(c echo.Context) error {
	users, err := a.store.GetAllUsers()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if users == nil {
		users = []*model.User{}
	}

	// Batch-load identities to avoid N+1; tolerate failure (UI still works without identities).
	if len(users) > 0 {
		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		if byUser, err := a.store.GetIdentitiesForUsers(ids); err == nil {
			for _, u := range users {
				u.Identities = byUser[u.ID]
			}
		}
	}

	return c.JSON(http.StatusOK, UserListResponse{
		Users: users,
		Total: len(users),
	})
}

// GetUser godoc
// @Summary Get a user by ID
// @Description Get detailed information about a specific user
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Success 200 {object} model.User
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/users/{id} [get]
func (a *API) GetUser(c echo.Context) error {
	id := c.Param("id")

	// The active read: an erased user is gone as far as the admin API is
	// concerned. Their row survives only so history that names the ID still
	// resolves, and that hydration goes through the batch read, not here.
	user, err := a.store.GetActiveUserByID(id)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	user.Identities, _ = a.store.ListUserIdentities(user.ID)

	return c.JSON(http.StatusOK, user)
}

// CreateUserRequest represents a request to create a user.
//
// External account links (Slack, Telegram, ...) are NOT accepted here - they are
// established only via the link flow (POST /me/slack/request-code, and so on).
type CreateUserRequest struct {
	ID       string `json:"id"`                 // Optional, will be generated if not provided
	Email    string `json:"email"`              // Required
	Name     string `json:"name"`               // Required
	Password string `json:"password,omitempty"` // Optional, will be hashed if provided
	Role     string `json:"role,omitempty"`     // Optional: "admin" or "user"
}

// CreateUser godoc
// @Summary Create a new user
// @Description Create a new user with email, name, and optional password
// @Tags users
// @Accept json
// @Produce json
// @Param user body CreateUserRequest true "User data"
// @Success 201 {object} model.User
// @Failure 400 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/users [post]
func (a *API) CreateUser(c echo.Context) error {

	var req CreateUserRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	if req.Email == "" || req.Name == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "email and name are required"})
	}

	// Generate ID if not provided
	userID := req.ID
	if userID == "" {
		// Simple ID from email (before @)
		parts := strings.Split(req.Email, "@")
		userID = parts[0]
	}

	user := &model.User{
		ID:    userID,
		Email: req.Email,
		Name:  req.Name,
	}

	if req.Role != "" {
		if req.Role != string(model.UserRoleAdmin) && req.Role != string(model.UserRoleUser) {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid role"})
		}
		user.Role = model.UserRole(req.Role)
	}

	// Hash password if provided
	if req.Password != "" {
		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to hash password"})
		}
		user.PasswordHash = hash
		user.AuthProvider = "password"
	}

	if err := a.store.CreateUser(user); err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Reload to get CreatedAt
	created, _ := a.store.GetActiveUserByID(userID)
	if created != nil {
		user = created
	}

	return c.JSON(http.StatusCreated, user)
}

// UpdateUserRequest represents a request to update a user. External account links
// are not editable here - use the link flow (POST /me/slack/request-code, …).
type UpdateUserRequest struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"` // Optional: "admin" or "user"
}

// UpdateUser godoc
// @Summary Update a user
// @Description Update a user's information
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Param user body UpdateUserRequest true "User data"
// @Success 200 {object} model.User
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/users/{id} [put]
func (a *API) UpdateUser(c echo.Context) error {
	id := c.Param("id")

	// Verify user exists
	user, err := a.store.GetActiveUserByID(id)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req UpdateUserRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	// Apply updates
	if req.Email != "" {
		user.Email = req.Email
	}
	if req.Name != "" {
		user.Name = req.Name
	}
	if req.Role != "" {
		// Validate role
		if req.Role != string(model.UserRoleAdmin) && req.Role != string(model.UserRoleUser) {
			return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid role"})
		}

		// Role changes go through SetUserRole and nowhere else: it is what
		// serializes the last-admin invariant against erasure.
		if err := a.store.SetUserRole(user.ID, model.UserRole(req.Role)); err != nil {
			switch {
			case errors.Is(err, store.ErrLastAdmin):
				return c.JSON(http.StatusConflict, ErrorResponse{Error: "cannot demote the last admin"})
			case errors.Is(err, store.ErrUserNotFound):
				return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
			}
			return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		}
		user.Role = model.UserRole(req.Role)
	}

	if err := a.store.UpdateUser(user); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, user)
}

type UpdatePasswordRequest struct {
	Password string `json:"password"`
}

// UpdateUserPassword godoc
// @Summary Update a user's password
// @Description Update a user's password
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Param body body UpdatePasswordRequest true "New password"
// @Success 200 {object} model.User
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/users/{id}/password [put]
func (a *API) UpdateUserPassword(c echo.Context) error {
	id := c.Param("id")

	// Verify user exists first
	user, err := a.store.GetActiveUserByID(id)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req UpdatePasswordRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request"})
	}

	if req.Password == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "password cannot be empty"})
	}

	if err := auth.ValidatePassword(req.Password); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: "failed to hash password"})
	}

	if err := a.store.UpdateUserPassword(id, hash); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, user)
}

// DeleteUser godoc
// @Summary Delete a user
// @Description Delete a user and remove from all teams
// @Tags users
// @Accept json
// @Produce json
// @Param id path string true "User ID"
// @Success 204 "No Content"
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/users/{id} [delete]
func (a *API) DeleteUser(c echo.Context) error {
	// Erasure is the only entry point; the route's middleware has already
	// refused the request if it was not wired.
	if err := a.userEraser.Erase(c.Request().Context(), c.Param("id")); err != nil {
		return a.mapScheduleError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// ========================================
// Team Members API
// ========================================

// AddTeamMemberRequest represents a request to add a user to a team.
type AddTeamMemberRequest struct {
	UserID string `json:"user_id"` // Required
	Role   string `json:"role"`    // Optional, defaults to "member"
}

// AddTeamMember godoc
// @Summary Add a user to a team
// @Description Add a user as a member of a team
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param member body AddTeamMemberRequest true "Member data"
// @Success 201 {object} model.TeamMember
// @Failure 400 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id}/members [post]
func (a *API) AddTeamMember(c echo.Context) error {
	teamID := c.Param("id")

	// Verify team exists
	_, err := a.store.GetTeamByID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	var req AddTeamMemberRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
	}

	if req.UserID == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "user_id is required"})
	}

	// Validate role: only accept team_admin or team_member (no dual-accept)
	role := model.TeamMemberRole(req.Role)
	if role == "" {
		role = model.TeamMemberRoleMember
	}
	if role != model.TeamMemberRoleAdmin && role != model.TeamMemberRoleMember {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "invalid role: must be 'team_admin' or 'team_member'"})
	}

	// Verify user exists
	_, err = a.store.GetActiveUserByID(req.UserID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	if err := a.store.AddTeamMember(teamID, req.UserID, role); err != nil {
		// An erased user is not found here even though the row survives for
		// history: granting them a membership would put an erased identity
		// back into circulation.
		if errors.Is(err, store.ErrUserNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "user not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusCreated, model.TeamMember{
		TeamID: teamID,
		UserID: req.UserID,
		Role:   role,
	})
}

// RemoveTeamMember godoc
// @Summary Remove a user from a team
// @Description Remove a user from a team
// @Tags teams
// @Accept json
// @Produce json
// @Param id path string true "Team ID"
// @Param user_id path string true "User ID"
// @Success 204 "No Content"
// @Failure 404 {object} ErrorResponse
// @Failure 500 {object} ErrorResponse
// @Router /api/v1/teams/{id}/members/{user_id} [delete]
func (a *API) RemoveTeamMember(c echo.Context) error {
	teamID := c.Param("id")
	targetUserID := c.Param("user_id")

	// Verify team exists
	_, err := a.store.GetTeamByID(teamID)
	if err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "team not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// Through the service, which refuses while the member holds a current
	// assignment: a membership is what makes a rotation entry resolvable, so
	// removing one out from under a live assignment leaves the schedule
	// pointing at a non-member.
	if err := a.scheduleConfig.RemoveTeamMember(c.Request().Context(), teamID, targetUserID); err != nil {
		return a.mapScheduleError(c, err)
	}

	return c.NoContent(http.StatusNoContent)
}
