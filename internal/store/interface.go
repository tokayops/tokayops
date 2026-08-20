package store

import (
	"time"

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
	UpdateAlertGroupAlerts(id string, alerts []model.Alert) error
	GetNewAlertGroups() ([]*model.AlertGroup, error)
	GetProcessingAlertGroups() ([]*model.AlertGroup, error)
	GetAcknowledgedAlertGroups() ([]*model.AlertGroup, error)
	GetResolvedAlertGroups() ([]*model.AlertGroup, error)
	MarkAckProcessed(agID string) error
	// The two halves of one gate, and they are not symmetrical. It is raised by
	// the write that changes the group - in the same statement, so no crash can
	// separate the alert from the fact that the message is stale - and lowered
	// for one version, which is how a producer avoids clearing away a change
	// that arrived while it worked.
	UpdateAlertGroupAlertsAndRaiseSlackUpdate(id string, alerts []model.Alert) error
	ClearSlackUpdate(id string, observedGeneration int64) (bool, error)
	GetAlertGroupsPendingSlackUpdate() ([]*model.AlertGroup, error)
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
	ResolveAlertGroupWithAlertsAtomic(id string, alerts []model.Alert, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) (changed bool, err error)

	// Conditional status transition (CAS semantics)
	TransitionAlertGroupStatus(id string, fromStatus, toStatus model.AlertGroupStatus) (bool, error)

	// Notification Deliveries
	UpsertNotificationDelivery(d *model.NotificationDelivery) error
	SetPrimaryDeliveryIfNone(alertGroupID, deliveryID string) (bool, error)
	GetPrimaryDelivery(alertGroupID, provider string) (*model.NotificationDelivery, error)
	GetFirehoseDelivery(alertGroupID, provider string) (*model.NotificationDelivery, error)
	GetDeliveryByID(id string) (*model.NotificationDelivery, error)
	UpdateDeliveryPayload(deliveryID, payload string) error
	ListDeliveries(alertGroupID string) ([]*model.NotificationDelivery, error)
	HasPrimaryDelivery(alertGroupID, provider string) (bool, error)

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

	// External Identities (Epic 7 Sprint 3 — replaces User.SlackUserID + slack_otp_codes)
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

	// Jobs (Phase 2)
	//
	// CreateJobWithDedup reports whether the job was inserted: false means the
	// identity was already claimed under its own policy and nothing was
	// written. A caller that counts notifications actually sent needs that
	// answer. The existing job's ID is not returned, because no caller ever
	// read it.
	CreateJobWithDedup(job *model.Job, stages []*model.JobStage, steps []*model.JobStep) (created bool, err error)
	EnsureEscalationJob(agID string, job *model.Job, stages []*model.JobStage, steps []*model.JobStep, snapshot *model.EscalationPolicySnapshot) (bool, error)
	GetJobByID(id string) (*model.Job, error)
	GetJobStepByID(stepID string) (*model.JobStep, error)
	ClaimNextJobSteps(limit int, duration time.Duration) ([]*model.JobStep, error)
	UpdateJobStepIfOwned(step *model.JobStep, leaseToken string) (bool, error)
	FinishStepAndAdvance(stepID string, leaseToken string, outcome model.JobStepStatus, result string, stepError string) (model.AdvanceResult, error)
	CancelEscalationJobByAlertGroupID(alertGroupID string) error
	ExtendStepLease(stepID string, leaseToken string, duration time.Duration) error
	FailJob(jobID string, errorMsg string) error

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
	UpdateIntegration(i *model.Integration) error
	DeleteIntegration(id string) error

	// Outbox (Phase 6)
	CreateOutboxEvent(event *model.OutboxEvent) error
	GetOutboxEventByID(id string) (*model.OutboxEvent, error)
	GetPendingOutboxEvents(limit int) ([]*model.OutboxEvent, error)
	UpdateOutboxEvent(event *model.OutboxEvent) error
	UpdateOutboxEventIfOwned(event *model.OutboxEvent, workerID string) (bool, error)
	ClaimOutboxEvents(workerID string, limit int, leaseDuration time.Duration) ([]*model.OutboxEvent, error)
	ExtendOutboxEventLease(eventID, workerID string, until time.Time) (bool, error)

	CreateOutboxDelivery(delivery *model.OutboxDelivery) error
	GetOutboxDeliveryByID(id string) (*model.OutboxDelivery, error)
	GetOutboxDelivery(eventID, integrationID string) (*model.OutboxDelivery, error)
	GetDeliveriesByEventID(eventID string) ([]*model.OutboxDelivery, error)
	GetDeliveriesByIntegrationID(integrationID string, limit, offset int) ([]*model.OutboxDelivery, int, error)
	UpdateOutboxDelivery(delivery *model.OutboxDelivery) error
	ReplayOutboxDelivery(deliveryID string) error

	CreateDeliveryAttempt(attempt *model.DeliveryAttempt) error
	GetDeliveryAttempts(deliveryID string) ([]*model.DeliveryAttempt, error)

	// Metrics
	GetMetricsSnapshot() (*MetricsSnapshot, error)

	// Lifecycle
	Close() error
}

// AlertGroupCount holds the count of active alert groups for a team/severity pair.
type AlertGroupCount struct {
	TeamID   string
	Severity string
	Count    int
}

// AlertGroupStatusCount holds the count of alert groups for a team/severity/status triple.
type AlertGroupStatusCount struct {
	TeamID   string
	Severity string
	Status   string
	Count    int
}

// StatusCount holds a count for a single status value.
type StatusCount struct {
	Status string
	Count  int
}

// MetricsSnapshot holds all data needed by the Prometheus collector.
type MetricsSnapshot struct {
	ActiveAlertGroups        []AlertGroupCount
	AlertGroupsByStatus      []AlertGroupStatusCount
	TeamsWithoutOnCall       int
	TeamsWithPermanentOnCall int
	TeamsWithoutPolicy       int
	OutboxEventsByStatus     []StatusCount
	OutboxDeliveriesByStatus []StatusCount
}

// Ensure Store implements StoreInterface
var _ StoreInterface = (*Store)(nil)
