package store

import (
	"context"
	"time"

	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// StoreInterface defines the interface for data persistence.
// This allows for mocking in tests.
type StoreInterface interface {
	// Alert Groups (renamed from Incidents)
	GetActiveAlertGroupByAlertKey(alertKey string) (*model.AlertGroup, error)
	CreateAlertGroup(ag *model.AlertGroup) error
	UpdateAlertGroupPolicy(id string, policyID string, snapshot *model.EscalationPolicySnapshot) error
	UpdateAlertGroupOnCall(id string, snapshot *model.OnCallResult) error
	// GetEscalationSources is the read a producer plans from: the groups
	// nobody has been paged for, with the alerts and the history their cards
	// are drawn from, and the version they were read at - all from one
	// consistent view of the database.
	GetEscalationSources(ctx context.Context) ([]*model.AlertGroup, error)
	GetProcessingAlertGroups() ([]*model.AlertGroup, error)
	GetAlertGroupByID(id string) (*model.AlertGroup, error)
	GetAllAlertGroups(status *model.AlertGroupStatus, limit, offset int) ([]*model.AlertGroup, int, error)
	GetAlertGroupsByTeam(teamID string, limit, offset int) ([]*model.AlertGroup, int, error)
	CountAlertGroupSummaries(statuses []model.AlertGroupStatus, severities []string, days int) (int, error)
	CountTeamAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days int) (int, error)
	ListAlertGroupSummaries(statuses []model.AlertGroupStatus, severities []string, days, limit, offset int, sortBy, sortDir string) ([]*model.AlertGroupSummary, error)
	ListTeamAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days, limit, offset int, sortBy, sortDir string) ([]*model.AlertGroupSummary, error)
	TouchAlertGroup(id string) error

	// Atomic create (AG + timeline + outbox in one transaction)
	CreateAlertGroupAtomic(ag *model.AlertGroup, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) error

	// Atomic ack/resolve (single-winner semantics, timeline + status + escalation cancel in one transaction)
	AckAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent) (changed bool, err error)
	ResolveAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent) (changed bool, err error)

	// Atomic resolve with alerts update (ingester auto-resolve: alerts + status + timeline + outbox in one transaction)
	ApplyAlertmanagerUpdateAtomic(ctx context.Context, alertKey string, incoming []model.Alert, actor string) (alertgroup.MergeResult, error)

	// notification_deliveries has no methods anywhere any more. It had one
	// reader and one writer, both in the job path that kept an alert group's
	// messages current, and what was delivered and where it landed is an
	// outbound commitment now. The table itself stands until the cutover under
	// migrations/, holding rows nothing reads.

	// Incidents (stub for future, business-level events)
	CreateIncident(i *model.Incident) error
	GetIncidentByID(id int) (*model.Incident, error)
	GetAllIncidents() ([]*model.Incident, error)

	// Teams
	CreateTeam(t *model.Team) error
	GetTeamByID(id string) (*model.Team, error)
	GetAllTeams() ([]*model.Team, error)
	UpdateTeam(t *model.Team) error

	// Users
	CreateUser(u *model.User) error

	// GetUserByID is the DISPLAY read: it returns erased users too, so history
	// that names an ID still resolves to "Deleted user". Authentication and
	// commands must use GetActiveUserByID instead.
	GetUserByID(id string) (*model.User, error)

	// GetActiveUserByID excludes erased users and answers ErrUserNotFound for
	// them, which is what makes a soft delete terminal.
	GetActiveUserByID(id string) (*model.User, error)
	GetUsersByIDs(ids []string) ([]*model.User, error)
	GetUserByEmail(email string) (*model.User, error)
	GetAllUsers() ([]*model.User, error)
	UpdateUser(u *model.User) error
	UpdateUserPassword(id, hash string) error
	UpdateUserAuthProvider(id, provider string) error
	DeleteUser(id string) error

	// External Identities (replaces User.SlackUserID + slack_otp_codes)
	BindExternalIdentity(ei *model.ExternalIdentity) error
	BindExternalIdentityIfAbsent(userID, provider, externalID, displayName string) (changed bool, err error)
	GetExternalIdentity(userID, provider string) (*model.ExternalIdentity, error)
	GetUserByExternalID(provider, externalID string) (*model.User, error)
	GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error)
	ListUserIdentities(userID string) ([]*model.ExternalIdentity, error)
	UnbindExternalIdentity(userID, provider string) error

	// Link Tokens (generic issue/consume for linking external accounts)
	IssueLinkToken(userID, provider, externalID, token string, expiresAt time.Time) error
	ConfirmIdentityLink(userID, provider, token string) (*model.ExternalIdentity, error)
	ConsumeLinkToken(provider, token, externalID, chatID, displayName string) (*model.ExternalIdentity, error)

	// Team Members
	AddTeamMember(teamID, userID string, role model.TeamMemberRole) error
	GetTeamMembers(teamID string) ([]*model.TeamMemberDetail, error)
	RemoveTeamMember(teamID, userID string) error
	GetTeamMembershipsForUser(userID string) (map[string]model.TeamMemberRole, error)

	// RBAC
	GetUserTeamRole(userID, teamID string) (model.TeamMemberRole, error)
	SetUserRole(userID string, role model.UserRole) error
	CountAdmins() (int, error)

	// Timeline
	AddTimelineEvent(e *model.TimelineEvent) error
	GetTimelineEvents(alertGroupID string) ([]*model.TimelineEvent, error)

	// API Tokens
	CreateAPIToken(token *model.APIToken) error
	GetAPITokenByID(id string) (*model.APIToken, error)
	GetAPITokenByHash(hash string) (*model.APIToken, error)
	GetUserAPITokens(userID string) ([]*model.APIToken, error)
	UpdateAPITokenLastUsed(id string) error
	DeleteAPIToken(id string) error

	// Escalation Policies (Phase 4)
	CreateEscalationPolicy(p *model.EscalationPolicy) error
	GetEscalationPolicyByID(id string) (*model.EscalationPolicy, error)
	GetAllEscalationPolicies() ([]*model.EscalationPolicy, error)
	UpdateEscalationPolicy(p *model.EscalationPolicy) error
	DeleteEscalationPolicy(id string) error
	GetPoliciesByTeamID(teamID string) ([]*model.EscalationPolicy, error)
	GetEscalationPoliciesForUser(userID string) ([]*model.EscalationPolicy, error)

	// Integrations (Phase 5)
	CreateIntegration(i *model.Integration) error
	GetIntegrationByID(id string) (*model.Integration, error)
	GetIntegrationByType(integrationType model.IntegrationType) (*model.Integration, error)
	GetIntegrationsByType(integrationType model.IntegrationType) ([]*model.Integration, error)
	GetAllIntegrations() ([]*model.Integration, error)
	// The lifecycle commands: one transaction each over the row and the
	// webhook commitments owed to it. See integration_lifecycle_store.go.
	UpdateIntegration(ctx context.Context, id string, patch IntegrationPatch, actor string) (IntegrationChange, error)
	DeleteIntegration(ctx context.Context, id, actor string) (IntegrationChange, error)
	WithIntegrationLocked(ctx context.Context, id string, fn func(current *model.Integration) error) error
	IntegrationTombstone(ctx context.Context, id string) (model.IntegrationTombstone, bool, error)

	// The alert outbox: the events the alert transactions write, and what the
	// fan-out turns into commitments. Written by the atomic transactions, read
	// by the fan-out; the pending read is how a test sees what a transaction
	// promised.
	CreateOutboxEvent(event *model.OutboxEvent) error
	GetOutboxEventByID(id string) (*model.OutboxEvent, error)
	GetPendingOutboxEvents(limit int) ([]*model.OutboxEvent, error)

	// Webhook deliveries: the delivery routes over the outbound domain's
	// commitments. See webhook_delivery_store.go.
	ListWebhookDeliveries(ctx context.Context, integrationID string, limit, offset int) ([]*model.OutboxDelivery, int, error)
	WebhookDelivery(ctx context.Context, integrationID, deliveryID string) (*model.OutboxDelivery, []*model.DeliveryAttempt, error)
	ReplayWebhookDelivery(ctx context.Context, req WebhookReplayRequest) (WebhookReplayResult, error)

	// Metrics
	GetMetricsSnapshot() (*model.MetricsSnapshot, error)

	// Lifecycle
	Close() error
}

// Ensure Store implements StoreInterface
var _ StoreInterface = (*Store)(nil)
