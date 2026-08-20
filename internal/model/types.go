package model

import (
	"time"
)

// AlertStatus represents the state of an alert from Alertmanager.
type AlertStatus string

const (
	AlertStatusFiring   AlertStatus = "firing"
	AlertStatusResolved AlertStatus = "resolved"
)

// Alert represents a single alert received from Alertmanager.
type Alert struct {
	Fingerprint  string            `json:"fingerprint"`
	Status       AlertStatus       `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

// AlertGroupStatus represents the state of an alert group in our system.
type AlertGroupStatus string

const (
	AlertGroupStatusNew          AlertGroupStatus = "new"
	AlertGroupStatusProcessing   AlertGroupStatus = "processing"
	AlertGroupStatusTriggered    AlertGroupStatus = "triggered"
	AlertGroupStatusAcknowledged AlertGroupStatus = "acknowledged" // User acknowledged, waiting for resolution
	AlertGroupStatusResolved     AlertGroupStatus = "resolved"
	AlertGroupStatusClosed       AlertGroupStatus = "closed"
)

// AlertGroup represents aggregated alerts from Alertmanager (technical entity).
// This is the main working entity for Phase 2.
type AlertGroup struct {
	ID string `json:"id"` // UUID
	// AlertKey is what the alerting system calls the alert this group is
	// about: Alertmanager's group key, or a single alert's fingerprint. It
	// names the ALERT, not this incident - the same key comes back every time
	// the same thing breaks, and each time it opens a new group with a new ID.
	//
	// It is not the identity of any background work. Jobs declare that through
	// jobdedup, and an escalation that once keyed itself by this string paged
	// nobody for the second incident.
	//
	// The JSON name is the one this field has always had, and stays: it is
	// what the API, the webhooks and the UI already read.
	AlertKey string           `json:"dedup_key"`
	Status   AlertGroupStatus `json:"status"`
	Title    string           `json:"title"` // Derived from Alert Labels/Annotations

	// Classification
	TeamID           string `json:"team_id"`
	TeamNameSnapshot string `json:"team_name_snapshot,omitempty"`
	Severity         string `json:"severity"`

	// Escalation Logic
	PolicyID    string `json:"policy_id"`
	CurrentStep int    `json:"current_step"`

	// Policy Snapshot (stores the policy state at the time of creation/assignment)
	PolicySnapshot *EscalationPolicySnapshot `json:"policy_snapshot,omitempty"`

	// OnCall Snapshot (stores the on-call state at the time of alert group creation)
	OnCallSnapshot *OnCallResult `json:"oncall_snapshot,omitempty"`

	// Alerts Snapshot
	Alerts []Alert `json:"alerts"`

	// Acknowledgement
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
	ResolvedBy     string     `json:"resolved_by,omitempty"`
	AckProcessedAt *time.Time `json:"ack_processed_at,omitempty"` // When ack update job was processed

	// Slack Update Tracking
	SlackUpdatePending bool `json:"slack_update_pending,omitempty"` // Set when alerts change; cleared after Slack update job created

	// SlackUpdateGeneration counts the times the flag above was raised, and is
	// what lets the producer lower it without losing an alert that arrived
	// while it worked: it clears the flag only if the generation it read is
	// still the current one.
	//
	// Not part of the group as the API presents it - it is the token of an
	// internal gate, and a reader outside this repository can do nothing with
	// it but be confused.
	SlackUpdateGeneration int64 `json:"-"`

	// External Link to Alertmanager Source (for clickable titles)
	ExternalURL string `json:"external_url,omitempty"`

	// Link to future Incident (NULL in most cases for Phase 2)
	IncidentID *int `json:"incident_id,omitempty"`

	// Timeline (loaded on demand, not persisted here)
	TimelineEvents []*TimelineEvent `json:"timeline_events,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// AlertGroupSummary is a lightweight projection of AlertGroup for list views.
// It omits heavy fields (alerts, policy_snapshot, provider_data) and includes
// pre-computed alert counts instead.
type AlertGroupSummary struct {
	ID             string           `json:"id"`
	AlertKey       string           `json:"dedup_key"`
	Status         AlertGroupStatus `json:"status"`
	Title          string           `json:"title"`
	TeamID         string           `json:"team_id"`
	Severity       string           `json:"severity"`
	CurrentStep    int              `json:"current_step"`
	OnCallSnapshot *OnCallResult    `json:"oncall_snapshot,omitempty"`
	ExternalURL    string           `json:"external_url,omitempty"`
	AcknowledgedBy string           `json:"acknowledged_by,omitempty"`
	ResolvedBy     string           `json:"resolved_by,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	ResolvedAt     *time.Time       `json:"resolved_at,omitempty"`
	AlertsCount    int              `json:"alerts_count"`
	FiringCount    int              `json:"firing_count"`
}

// NotificationDelivery represents a single notification delivery attempt.
// It is tied to an AlertGroup and typically to a JobStep.
type NotificationDelivery struct {
	ID              string    `json:"id"`
	AlertGroupID    string    `json:"alert_group_id"`
	JobStepID       *string   `json:"job_step_id,omitempty"`
	Provider        string    `json:"provider"`
	Kind            string    `json:"kind"` // slack_channel, slack_dm, firehose
	TargetType      string    `json:"target_type,omitempty"`
	TargetID        string    `json:"target_id,omitempty"`
	ProviderPayload string    `json:"provider_payload,omitempty"`
	SupportsUpdate  bool      `json:"supports_update"`
	IsPrimary       bool      `json:"is_primary"`
	IsFirehose      bool      `json:"is_firehose"`
	Attempt         int       `json:"attempt"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IncidentStatus represents the lifecycle state of a business incident.
type IncidentStatus string

const (
	IncidentStatusInvestigating IncidentStatus = "investigating"
	IncidentStatusIdentified    IncidentStatus = "identified"
	IncidentStatusMonitoring    IncidentStatus = "monitoring"
	IncidentStatusResolved      IncidentStatus = "resolved"
)

// Incident represents a business-level event (stub for Phase 2, full implementation in Phase 3+).
// Created only when an AlertGroup is "promoted" to a significant business incident.
type Incident struct {
	ID             int            `json:"id"`                         // Human-readable ID: INC-1, INC-2, etc.
	Title          string         `json:"title"`                      // "High Latency in Payment Gateway"
	Status         IncidentStatus `json:"status"`                     // investigating, identified, monitoring, resolved
	Severity       string         `json:"severity,omitempty"`         // SEV1, SEV2 (may differ from alert)
	CommanderID    *string        `json:"commander_id,omitempty"`     // Reference to users (future)
	SlackChannelID *string        `json:"slack_channel_id,omitempty"` // War Room channel (future)
	CreatedAt      time.Time      `json:"created_at"`
}

// Team represents a group that owns alert groups and has on-call schedules.
type Team struct {
	ID              string            `json:"id"`                          // e.g., "devops"
	Name            string            `json:"name"`                        // Display name: "DevOps Team"
	Description     string            `json:"description"`                 // Optional description
	DefaultPolicyID string            `json:"default_policy_id,omitempty"` // Fallback escalation policy
	SeverityRoutes  map[string]string `json:"severity_routes,omitempty"`   // severity -> policy_id
	CreatedAt       time.Time         `json:"created_at"`
}

// UserRole defines the global role of a user in the system.
type UserRole string

const (
	UserRoleAdmin UserRole = "admin" // Full system access
	UserRoleUser  UserRole = "user"  // Standard authenticated user (default)
)

// User represents an individual who can be notified and assigned to incidents.
//
// External account links (Slack user IDs, Telegram chat IDs, ...) live in
// `external_identities` and are surfaced via Identities on demand by handlers —
// they are NOT loaded by the user-row scan (mirrors AlertGroup.TimelineEvents).
type User struct {
	ID           string              `json:"id"`                      // UUID
	Email        string              `json:"email"`                   // Unique email
	Name         string              `json:"name"`                    // Display name
	Role         UserRole            `json:"role"`                    // Global role: admin or user
	PasswordHash string              `json:"-"`                       // Hashed password
	AuthProvider string              `json:"auth_provider,omitempty"` // "password" or "oidc" - last login method
	CreatedAt    time.Time           `json:"created_at"`
	Identities   []*ExternalIdentity `json:"identities,omitempty"` // loaded on demand by handlers
}

// ExternalIdentity binds an TokayOps user to an external account (Slack, Telegram, ...).
// (provider, external_id) is globally unique; (user_id, provider) is unique per user.
type ExternalIdentity struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Provider    string    `json:"provider"`
	ExternalID  string    `json:"external_id"`
	ChatID      string    `json:"chat_id,omitempty"`      // provider-specific (Telegram DM chat)
	DisplayName string    `json:"display_name,omitempty"` // shown in timeline / UI
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// LinkToken is a short-lived, single-use token for linking an external account.
// For Slack OTP, Token is the 6-digit code DM'd to the user; for Telegram deep-link
// it is the opaque token in the /start <token> URL. The store persists only the
// SHA-256 hash — Token is never stored at rest.
type LinkToken struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	Provider   string    `json:"provider"`
	Token      string    `json:"-"` // plaintext, never serialized
	ExternalID string    `json:"external_id,omitempty"`
	Attempts   int       `json:"attempts"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// TeamMemberDetail represents a user with their role in a specific team.
type TeamMemberDetail struct {
	User
	TeamRole TeamMemberRole `json:"team_role"` // "team_admin" or "team_member"
}

// UserWithPermissions embeds User and adds permissions data.
type UserWithPermissions struct {
	User
	// Teams is a map of TeamID to Role (e.g. "devops" -> "team_admin")
	Teams map[string]TeamMemberRole `json:"teams"`
}

// APIToken represents an API token for automation access.
// Tokens allow API access without CSRF protection, suitable for scripts and integrations.
type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`                   // "My CI/CD Script"
	TokenHash  string     `json:"-"`                      // SHA256 hash, never exposed
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`   // nil = no expiration
	LastUsedAt *time.Time `json:"last_used_at,omitempty"` // Updated on each use
	CreatedAt  time.Time  `json:"created_at"`
}

// TeamMemberRole defines the role of a user within a team.
type TeamMemberRole string

const (
	TeamMemberRoleAdmin  TeamMemberRole = "team_admin"  // Team administrator
	TeamMemberRoleMember TeamMemberRole = "team_member" // Regular team member
)

// TeamMember represents the many-to-many relationship between Teams and Users.
type TeamMember struct {
	TeamID string         `json:"team_id"`
	UserID string         `json:"user_id"`
	Role   TeamMemberRole `json:"role"`
}

// TimelineEventType represents the type of timeline event.
type TimelineEventType string

const (
	TimelineEventCreated            TimelineEventType = "created"
	TimelineEventAlertAdded         TimelineEventType = "alert_added"
	TimelineEventAlertResolved      TimelineEventType = "alert_resolved"
	TimelineEventAcknowledged       TimelineEventType = "acknowledged"
	TimelineEventResolved           TimelineEventType = "resolved"
	TimelineEventNotificationSent   TimelineEventType = "notification_sent"
	TimelineEventNotificationFailed TimelineEventType = "notification_failed"
	TimelineEventNote               TimelineEventType = "note"
	TimelineEventStatusChange       TimelineEventType = "status_change"
)

// TimelineEvent represents a single event in an alert group's history.
type TimelineEvent struct {
	ID           string            `json:"id"`
	AlertGroupID string            `json:"alert_group_id"` // Changed from incident_id
	Type         TimelineEventType `json:"type"`
	Message      string            `json:"message"`            // Human-readable description
	Actor        string            `json:"actor"`              // User ID, "system", or alert name
	Metadata     map[string]string `json:"metadata,omitempty"` // Extra data (alert fingerprint, etc.)
	CreatedAt    time.Time         `json:"created_at"`
}

// ========================================
// Schedule Types (Phase 3)
// ========================================

// RotationType defines the rotation frequency
type RotationType string

const (
	RotationDaily  RotationType = "daily"
	RotationWeekly RotationType = "weekly"
)

// OnCallResult represents the current on-call user(s) for a schedule
type OnCallResult struct {
	L1Users []*User    `json:"l1_users"`
	L2User  *User      `json:"l2_user,omitempty"`
	L1Since *time.Time `json:"l1_since,omitempty"` // When L1 shift started
	L1Until *time.Time `json:"l1_until,omitempty"` // When L1 shift ends. nil = forever/unknown

	// Source is "rotation" or "override": what put L1Users on duty. The
	// projection overlays an override onto the layer, so L1Users already names
	// whoever is really on call - Source is what keeps the fact that they are
	// standing in from being lost, now that there is no legacy override row to
	// point at. Empty on snapshots written before it existed.
	Source string `json:"source,omitempty"`
}

// ========================================
// Escalation Policy Types (Phase 4)
// ========================================

// EscalationPolicy defines a multi-step escalation workflow
type EscalationPolicy struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	TeamID      *string           `json:"team_id,omitempty"` // nil = global policy
	Steps       []*EscalationStep `json:"steps"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// EscalationStep defines a single step in an escalation policy.
//
// Sprint 4 (Epic 7 L6) replaced the flat StepType enum with
// (Provider, TargetKind):
//   - Provider: provider key (e.g. "slack"); resolved at dispatch time.
//   - TargetKind: "dm" or "channel" — what kind of recipient the step targets.
//     The pair must be supported by the provider's capabilities.
//
// The legacy combined enum (`slack_dm` / `slack_channel`) was a one-provider
// shortcut; it doesn't model two providers cleanly.
type EscalationStep struct {
	ID                string `json:"id"`
	PolicyID          string `json:"policy_id"`
	StepIndex         int    `json:"step_index"`
	Provider          string `json:"provider"`    // "slack", "telegram", ...
	TargetKind        string `json:"target_kind"` // "dm" | "channel"
	TargetType        string `json:"target_type"` // user, channel, schedule
	TargetID          string `json:"target_id"`   // actual ID
	DelaySeconds      int    `json:"delay_seconds"`
	TimeoutSeconds    int    `json:"timeout_seconds"`
	MaxAttempts       int    `json:"max_attempts"`
	Message           string `json:"message,omitempty"`
	ContinueOnFailure bool   `json:"continue_on_failure"`
}
