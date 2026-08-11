package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/integrations"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/google/uuid"
)

// MockStore is an in-memory implementation of StoreInterface for testing.
type MockStore struct {
	mu sync.RWMutex

	alertGroups            map[string]*model.AlertGroup
	incidents              map[int]*model.Incident
	incidentSeq            int
	teams                  map[string]*model.Team
	users                  map[string]*model.User
	erasedUsers            map[string]bool
	teamMembers            map[string]map[string]model.TeamMemberRole // teamID -> userID -> role
	timelineEvents         map[string][]*model.TimelineEvent          // alertGroupID -> events
	apiTokens              map[string]*model.APIToken                 // tokenID -> token
	externalIdentities     map[string]*model.ExternalIdentity         // "userID|provider" -> identity
	linkTokens             map[string]mockLinkToken                   // "userID|provider" -> link token
	jobs                   map[string]*model.Job                      // jobID -> job
	jobStages              map[string]*model.JobStage                 // stageID -> stage
	jobSteps               map[string]*model.JobStep                  // stepID -> step
	escalationPolicies     map[string]*model.EscalationPolicy         // policyID -> policy
	integrations           map[string]*model.Integration              // integrationID -> integration
	notificationDeliveries map[string]*model.NotificationDelivery     // deliveryID -> delivery
	outboxEvents           map[string]*model.OutboxEvent              // eventID -> event
	outboxDeliveries       map[string]*model.OutboxDelivery           // deliveryID -> delivery
	deliveryAttempts       map[string][]*model.DeliveryAttempt        // deliveryID -> attempts

	// Error injection for testing. When set, the corresponding method returns this error.
	GetIntegrationByIDError       error
	GetUserByIDError              error
	CreateOutboxDeliveryError     error // returned by CreateOutboxDelivery (non-duplicate calls)
	ExtendOutboxEventLeaseError   error // returned by ExtendOutboxEventLease
	ExtendOutboxEventLeaseResult  *bool // if non-nil, overrides the ownership check result
	UpdateOutboxEventIfOwnedError error // returned by UpdateOutboxEventIfOwned
	ReplayOutboxDeliveryError     error // returned by ReplayOutboxDelivery
}

// NewMockStore creates a new MockStore with seed data.
func NewMockStore() *MockStore {
	m := &MockStore{
		alertGroups:    make(map[string]*model.AlertGroup),
		incidents:      make(map[int]*model.Incident),
		incidentSeq:    0,
		teams:          make(map[string]*model.Team),
		users:          make(map[string]*model.User),
		teamMembers:    make(map[string]map[string]model.TeamMemberRole),
		timelineEvents: make(map[string][]*model.TimelineEvent),
		apiTokens:      make(map[string]*model.APIToken),

		externalIdentities:     make(map[string]*model.ExternalIdentity),
		linkTokens:             make(map[string]mockLinkToken),
		jobs:                   make(map[string]*model.Job),
		jobStages:              make(map[string]*model.JobStage),
		jobSteps:               make(map[string]*model.JobStep),
		escalationPolicies:     make(map[string]*model.EscalationPolicy),
		integrations:           make(map[string]*model.Integration),
		notificationDeliveries: make(map[string]*model.NotificationDelivery),
		outboxEvents:           make(map[string]*model.OutboxEvent),
		outboxDeliveries:       make(map[string]*model.OutboxDelivery),
		deliveryAttempts:       make(map[string][]*model.DeliveryAttempt),
	}

	// Seed data matching what InitDB would create
	m.seedData()
	return m
}

func (m *MockStore) seedData() {
	// Seed teams
	m.teams["devops"] = &model.Team{
		ID:          "devops",
		Name:        "DevOps",
		Description: "DevOps Team",
		CreatedAt:   time.Now(),
	}
	m.teams["triage"] = &model.Team{
		ID:          "triage",
		Name:        "Triage",
		Description: "Default team for unassigned alerts",
		CreatedAt:   time.Now(),
	}

	// Seed users (first user is admin per RBAC bootstrap rule)
	m.users["denis"] = &model.User{
		ID:        "denis",
		Email:     "denis@example.com",
		Name:      "Denis",
		Role:      model.UserRoleAdmin, // First user becomes admin
		CreatedAt: time.Now(),
	}
	m.users["alex"] = &model.User{
		ID:        "alex",
		Email:     "alex@example.com",
		Name:      "Alex",
		Role:      model.UserRoleUser,
		CreatedAt: time.Now(),
	}

	// Add users to devops team (using new role names)
	m.teamMembers["devops"] = map[string]model.TeamMemberRole{
		"denis": model.TeamMemberRoleAdmin,  // team_admin
		"alex":  model.TeamMemberRoleMember, // team_member
	}
	m.teamMembers["triage"] = map[string]model.TeamMemberRole{}
}

// Close is a no-op for MockStore.
func (m *MockStore) Close() error {
	return nil
}

// ========================================
// Alert Groups (renamed from Incidents)
// ========================================

func (m *MockStore) GetActiveAlertGroup(dedupKey string) (*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, ag := range m.alertGroups {
		if ag.DedupKey == dedupKey &&
			ag.Status != model.AlertGroupStatusResolved &&
			ag.Status != model.AlertGroupStatusClosed {
			return m.copyAlertGroup(ag), nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) CreateAlertGroup(ag *model.AlertGroup) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.alertGroups[ag.ID] = m.copyAlertGroup(ag)
	return nil
}

func (m *MockStore) CreateAlertGroupAtomic(ag *model.AlertGroup, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.alertGroups[ag.ID] = m.copyAlertGroup(ag)

	for _, e := range timelineEvents {
		eventCopy := *e
		if eventCopy.CreatedAt.IsZero() {
			eventCopy.CreatedAt = time.Now()
		}
		m.timelineEvents[e.AlertGroupID] = append(m.timelineEvents[e.AlertGroupID], &eventCopy)
	}

	m.insertOutboxEvent(outboxEvent)

	return nil
}

// insertOutboxEvent normalizes defaults and stores an outbox event. Caller must hold m.mu.
func (m *MockStore) insertOutboxEvent(event *model.OutboxEvent) {
	if event == nil {
		return
	}
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.Status == "" {
		event.Status = model.OutboxEventStatusPending
	}
	if event.Actor == "" {
		event.Actor = "system"
	}
	if event.Payload == nil {
		event.Payload = json.RawMessage("{}")
	}
	event.CreatedAt = time.Now()
	m.outboxEvents[event.ID] = event
}

func (m *MockStore) UpdateAlertGroupStatus(id string, status model.AlertGroupStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		ag.Status = status
		ag.UpdatedAt = time.Now()
		if status == model.AlertGroupStatusResolved {
			now := time.Now()
			ag.ResolvedAt = &now
		}
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) UpdateAlertGroupAcknowledged(id string, acknowledgedBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		ag.Status = model.AlertGroupStatusAcknowledged
		ag.AcknowledgedBy = acknowledgedBy
		ag.AckProcessedAt = nil // Clear to allow re-processing on re-ack
		ag.UpdatedAt = time.Now()
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) UpdateAlertGroupPolicy(id string, policyID string, snapshot *model.EscalationPolicySnapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		ag.PolicyID = policyID

		if snapshot != nil {
			snapCopy := *snapshot
			snapCopy.Steps = make([]*model.EscalationStepSnapshot, len(snapshot.Steps))
			for i, s := range snapshot.Steps {
				stepCopy := *s
				snapCopy.Steps[i] = &stepCopy
			}
			ag.PolicySnapshot = &snapCopy
		} else {
			ag.PolicySnapshot = nil
		}

		ag.UpdatedAt = time.Now()
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) UpdateAlertGroupOnCall(id string, snapshot *model.OnCallResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		if snapshot != nil {
			snapCopy := *snapshot
			ag.OnCallSnapshot = &snapCopy
		} else {
			ag.OnCallSnapshot = nil
		}
		ag.UpdatedAt = time.Now()
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) UpdateAlertGroupAlerts(id string, alerts []model.Alert) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		ag.Alerts = make([]model.Alert, len(alerts))
		copy(ag.Alerts, alerts)
		ag.UpdatedAt = time.Now()
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) GetNewAlertGroups() ([]*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	staleThreshold := time.Now().Add(-30 * time.Second)
	var result []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if ag.Status == model.AlertGroupStatusNew {
			result = append(result, m.copyAlertGroup(ag))
		} else if ag.Status == model.AlertGroupStatusProcessing && ag.UpdatedAt.Before(staleThreshold) {
			// Only include stale processing AGs that have NO escalation job at all (true orphan).
			// If any escalation job exists (succeeded, failed, canceled, etc.), the AG was already
			// processed and should not spawn a duplicate job.
			hasAnyJob := false
			for _, j := range m.jobs {
				if j.AlertGroupID != nil && *j.AlertGroupID == ag.ID && j.Type == "escalation" {
					hasAnyJob = true
					break
				}
			}
			if !hasAnyJob {
				result = append(result, m.copyAlertGroup(ag))
			}
		}
	}
	return result, nil
}

func (m *MockStore) GetProcessingAlertGroups() ([]*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Include both processing and acknowledged (for Slack updates after Ack)
	var alertGroups []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if ag.Status == model.AlertGroupStatusProcessing || ag.Status == model.AlertGroupStatusAcknowledged {
			alertGroups = append(alertGroups, m.copyAlertGroup(ag))
		}
	}

	// Populate timeline
	for _, ag := range alertGroups {
		events, _ := m.GetTimelineEvents(ag.ID)
		ag.TimelineEvents = events
	}
	return alertGroups, nil
}

func (m *MockStore) GetAcknowledgedAlertGroups() ([]*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Only return acknowledged alert groups that haven't been processed yet
	var alertGroups []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if ag.Status == model.AlertGroupStatusAcknowledged && ag.AckProcessedAt == nil {
			alertGroups = append(alertGroups, m.copyAlertGroup(ag))
		}
	}

	for _, ag := range alertGroups {
		events, _ := m.GetTimelineEvents(ag.ID)
		ag.TimelineEvents = events
	}
	return alertGroups, nil
}

func (m *MockStore) GetResolvedAlertGroups() ([]*model.AlertGroup, error) {
	alertGroups, err := m.getAlertGroupsByStatus(model.AlertGroupStatusResolved)
	if err != nil {
		return nil, err
	}
	// Populate timeline
	for _, ag := range alertGroups {
		events, _ := m.GetTimelineEvents(ag.ID)
		ag.TimelineEvents = events
	}
	return alertGroups, nil
}

func (m *MockStore) getAlertGroupsByStatus(status model.AlertGroupStatus) ([]*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if ag.Status == status {
			result = append(result, m.copyAlertGroup(ag))
		}
	}
	return result, nil
}

func (m *MockStore) GetAlertGroupByID(id string) (*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if ag, ok := m.alertGroups[id]; ok {
		return m.copyAlertGroup(ag), nil
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetAllAlertGroups(status *model.AlertGroupStatus, limit, offset int) ([]*model.AlertGroup, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var filtered []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if status == nil || ag.Status == *status {
			filtered = append(filtered, m.copyAlertGroup(ag))
		}
	}

	total := len(filtered)

	// Apply pagination
	if offset >= len(filtered) {
		return []*model.AlertGroup{}, total, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return filtered[offset:end], total, nil
}

func (m *MockStore) GetAlertGroupsByTeam(teamID string, limit, offset int) ([]*model.AlertGroup, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var filtered []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if ag.TeamID == teamID {
			filtered = append(filtered, m.copyAlertGroup(ag))
		}
	}

	total := len(filtered)

	if offset >= len(filtered) {
		return []*model.AlertGroup{}, total, nil
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return filtered[offset:end], total, nil
}

func mockSevPriority(s string) int {
	switch s {
	case "critical":
		return 3
	case "warning":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

func mockStatusPriority(s model.AlertGroupStatus) int {
	switch s {
	case model.AlertGroupStatusTriggered:
		return 5
	case model.AlertGroupStatusProcessing:
		return 4
	case model.AlertGroupStatusNew:
		return 3
	case model.AlertGroupStatusAcknowledged:
		return 2
	case model.AlertGroupStatusResolved:
		return 1
	case model.AlertGroupStatusClosed:
		return 0
	default:
		return 0
	}
}

func intCmp(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func stringCmp(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func timeCmp(a, b time.Time) int {
	if a.Before(b) {
		return -1
	}
	if a.After(b) {
		return 1
	}
	return 0
}

// cmpTimePtr compares two nullable times. nils sort before non-nil values.
func cmpTimePtr(a, b *time.Time) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return -1
	}
	if b == nil {
		return 1
	}
	return a.Compare(*b)
}

func compareSummaryField(a, b *model.AlertGroupSummary, field string) int {
	switch field {
	case "severity":
		return intCmp(mockSevPriority(a.Severity), mockSevPriority(b.Severity))
	case "status":
		return intCmp(mockStatusPriority(a.Status), mockStatusPriority(b.Status))
	case "team_id":
		return stringCmp(a.TeamID, b.TeamID)
	case "title":
		return stringCmp(a.Title, b.Title)
	case "resolved_at":
		ta, tb := time.Time{}, time.Time{}
		if a.ResolvedAt != nil {
			ta = *a.ResolvedAt
		}
		if b.ResolvedAt != nil {
			tb = *b.ResolvedAt
		}
		// NULLs (zero time) sort last in DESC, first in ASC — handled by caller via direction
		return timeCmp(ta, tb)
	default: // "created_at" or empty
		return timeCmp(a.CreatedAt, b.CreatedAt)
	}
}

func sortSummaries(items []*model.AlertGroupSummary, sortBy, sortDir string) {
	dir := 1
	if sortDir != "asc" {
		dir = -1
	}
	sort.SliceStable(items, func(i, j int) bool {
		cmp := compareSummaryField(items[i], items[j], sortBy) * dir
		if cmp != 0 {
			return cmp < 0
		}
		// tie-breaker: created_at, then id
		if sortBy != "created_at" {
			cmp = timeCmp(items[i].CreatedAt, items[j].CreatedAt) * dir
			if cmp != 0 {
				return cmp < 0
			}
		}
		return (stringCmp(items[i].ID, items[j].ID) * dir) < 0
	})
}

// filterAlertGroupSummaries returns summaries matching the given filters.
// If teamID is non-empty, only alert groups for that team are included.
// Caller must hold m.mu.RLock.
func (m *MockStore) filterAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days int) []*model.AlertGroupSummary {
	var cutoff time.Time
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}

	statusSet := make(map[model.AlertGroupStatus]bool, len(statuses))
	for _, s := range statuses {
		statusSet[s] = true
	}
	sevSet := make(map[string]bool, len(severities))
	for _, s := range severities {
		sevSet[s] = true
	}

	var filtered []*model.AlertGroupSummary
	for _, ag := range m.alertGroups {
		if teamID != "" && ag.TeamID != teamID {
			continue
		}
		if len(statusSet) > 0 && !statusSet[ag.Status] {
			continue
		}
		if len(sevSet) > 0 && !sevSet[ag.Severity] {
			continue
		}
		if days > 0 && ag.UpdatedAt.Before(cutoff) && ag.CreatedAt.Before(cutoff) {
			continue
		}
		firingCount := 0
		for _, a := range ag.Alerts {
			if a.Status == "firing" {
				firingCount++
			}
		}
		filtered = append(filtered, &model.AlertGroupSummary{
			ID: ag.ID, DedupKey: ag.DedupKey, Status: ag.Status, Title: ag.Title,
			TeamID: ag.TeamID, Severity: ag.Severity, CurrentStep: ag.CurrentStep,
			OnCallSnapshot: ag.OnCallSnapshot, ExternalURL: ag.ExternalURL,
			AcknowledgedBy: ag.AcknowledgedBy, ResolvedBy: ag.ResolvedBy,
			CreatedAt: ag.CreatedAt, UpdatedAt: ag.UpdatedAt, ResolvedAt: ag.ResolvedAt,
			AlertsCount: len(ag.Alerts), FiringCount: firingCount,
		})
	}
	return filtered
}

func (m *MockStore) CountAlertGroupSummaries(statuses []model.AlertGroupStatus, severities []string, days int) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.filterAlertGroupSummaries("", statuses, severities, days)), nil
}

func (m *MockStore) CountTeamAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days int) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.filterAlertGroupSummaries(teamID, statuses, severities, days)), nil
}

func (m *MockStore) ListAlertGroupSummaries(statuses []model.AlertGroupStatus, severities []string, days, limit, offset int, sortBy, sortDir string) ([]*model.AlertGroupSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	filtered := m.filterAlertGroupSummaries("", statuses, severities, days)
	sortSummaries(filtered, sortBy, sortDir)
	total := len(filtered)
	if offset >= total {
		return []*model.AlertGroupSummary{}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return filtered[offset:end], nil
}

func (m *MockStore) ListTeamAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days, limit, offset int, sortBy, sortDir string) ([]*model.AlertGroupSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	filtered := m.filterAlertGroupSummaries(teamID, statuses, severities, days)
	sortSummaries(filtered, sortBy, sortDir)
	total := len(filtered)
	if offset >= total {
		return []*model.AlertGroupSummary{}, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return filtered[offset:end], nil
}

func (m *MockStore) TouchAlertGroup(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		ag.UpdatedAt = time.Now()
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) AckAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent, dedupKey string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ag, ok := m.alertGroups[id]
	if !ok {
		// Match real store: UPDATE WHERE id=$1 AND status=$2 returns 0 rows for non-existent ID
		return false, nil
	}
	if ag.Status != model.AlertGroupStatusProcessing && ag.Status != model.AlertGroupStatusTriggered {
		return false, nil
	}

	now := time.Now()
	ag.Status = model.AlertGroupStatusAcknowledged
	ag.AcknowledgedBy = actor
	ag.AckProcessedAt = nil
	ag.UpdatedAt = now

	// Add timeline event atomically (under same lock)
	event := &model.TimelineEvent{
		ID:           uuid.New().String(),
		AlertGroupID: id,
		Type:         model.TimelineEventAcknowledged,
		Message:      "Alert group acknowledged",
		Actor:        actor,
		Metadata:     meta,
		CreatedAt:    now,
	}
	m.timelineEvents[id] = append(m.timelineEvents[id], event)

	m.insertOutboxEvent(outboxEvent)

	m.cancelJobByDedupKeyLocked(dedupKey)

	return true, nil
}

func (m *MockStore) ResolveAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent, dedupKey string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ag, ok := m.alertGroups[id]
	if !ok {
		// Match real store: UPDATE WHERE id=$1 AND status IN (...) returns 0 rows for non-existent ID
		return false, nil
	}
	if ag.Status != model.AlertGroupStatusProcessing && ag.Status != model.AlertGroupStatusTriggered && ag.Status != model.AlertGroupStatusAcknowledged {
		return false, nil
	}

	now := time.Now()
	ag.Status = model.AlertGroupStatusResolved
	ag.ResolvedBy = actor
	ag.ResolvedAt = &now
	ag.UpdatedAt = now

	// Add timeline event atomically (under same lock)
	event := &model.TimelineEvent{
		ID:           uuid.New().String(),
		AlertGroupID: id,
		Type:         model.TimelineEventResolved,
		Message:      "Alert group manually resolved",
		Actor:        actor,
		Metadata:     meta,
		CreatedAt:    now,
	}
	m.timelineEvents[id] = append(m.timelineEvents[id], event)

	m.insertOutboxEvent(outboxEvent)

	m.cancelJobByDedupKeyLocked(dedupKey)

	return true, nil
}

func (m *MockStore) ResolveAlertGroupWithAlertsAtomic(id string, alerts []model.Alert, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent, dedupKey string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ag, ok := m.alertGroups[id]
	if !ok {
		return false, nil
	}
	if ag.Status != model.AlertGroupStatusNew &&
		ag.Status != model.AlertGroupStatusProcessing &&
		ag.Status != model.AlertGroupStatusTriggered &&
		ag.Status != model.AlertGroupStatusAcknowledged {
		return false, nil
	}

	now := time.Now()
	ag.Alerts = make([]model.Alert, len(alerts))
	copy(ag.Alerts, alerts)
	ag.Status = model.AlertGroupStatusResolved
	ag.ResolvedBy = "system"
	ag.ResolvedAt = &now
	ag.UpdatedAt = now

	for _, e := range timelineEvents {
		eventCopy := *e
		if eventCopy.CreatedAt.IsZero() {
			eventCopy.CreatedAt = now
		}
		m.timelineEvents[id] = append(m.timelineEvents[id], &eventCopy)
	}

	m.insertOutboxEvent(outboxEvent)
	m.cancelJobByDedupKeyLocked(dedupKey)

	return true, nil
}

// cancelJobByDedupKeyLocked cancels jobs matching dedupKey. Caller must hold m.mu.
func (m *MockStore) cancelJobByDedupKeyLocked(dedupKey string) {
	if dedupKey == "" {
		return
	}
	for _, job := range m.jobs {
		if job.DedupKey != nil && *job.DedupKey == dedupKey {
			if job.Status == model.JobStatusPending || job.Status == model.JobStatusRunning {
				job.Status = model.JobStatusCanceled
			}
		}
	}
}

func (m *MockStore) TransitionAlertGroupStatus(id string, fromStatus, toStatus model.AlertGroupStatus) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ag, ok := m.alertGroups[id]
	if !ok {
		return false, nil
	}
	if ag.Status != fromStatus {
		return false, nil
	}

	ag.Status = toStatus
	ag.UpdatedAt = time.Now()
	return true, nil
}

func (m *MockStore) MarkAckProcessed(agID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[agID]; ok {
		now := time.Now()
		ag.AckProcessedAt = &now
		ag.UpdatedAt = now
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) SetSlackUpdatePending(id string, pending bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ag, ok := m.alertGroups[id]; ok {
		ag.SlackUpdatePending = pending
		ag.UpdatedAt = time.Now()
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) GetAlertGroupsPendingSlackUpdate() ([]*model.AlertGroup, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var alertGroups []*model.AlertGroup
	for _, ag := range m.alertGroups {
		if ag.SlackUpdatePending &&
			(ag.Status == model.AlertGroupStatusProcessing ||
				ag.Status == model.AlertGroupStatusAcknowledged ||
				ag.Status == model.AlertGroupStatusTriggered) {
			alertGroups = append(alertGroups, m.copyAlertGroup(ag))
		}
	}
	return alertGroups, nil
}

// ========================================
// Notification Deliveries
// ========================================

func (m *MockStore) UpsertNotificationDelivery(d *model.NotificationDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if d == nil {
		return fmt.Errorf("delivery is nil")
	}

	now := time.Now()
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	if d.JobStepID != nil && *d.JobStepID != "" {
		for _, existing := range m.notificationDeliveries {
			if existing.JobStepID != nil && *existing.JobStepID == *d.JobStepID {
				existing.AlertGroupID = d.AlertGroupID
				existing.Provider = d.Provider
				existing.Kind = d.Kind
				existing.TargetType = d.TargetType
				existing.TargetID = d.TargetID
				existing.ProviderPayload = d.ProviderPayload
				existing.SupportsUpdate = d.SupportsUpdate
				existing.IsFirehose = d.IsFirehose
				// Intentionally keep existing.IsPrimary to avoid clobbering primary on retries.
				existing.Attempt = d.Attempt
				existing.UpdatedAt = d.UpdatedAt
				return nil
			}
		}
	}

	copy := *d
	if d.JobStepID != nil {
		jobStepID := *d.JobStepID
		copy.JobStepID = &jobStepID
	}
	m.notificationDeliveries[copy.ID] = &copy
	return nil
}

func (m *MockStore) SetPrimaryDeliveryIfNone(alertGroupID, deliveryID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, d := range m.notificationDeliveries {
		if d.AlertGroupID == alertGroupID && d.IsPrimary {
			return false, nil
		}
	}

	if d, ok := m.notificationDeliveries[deliveryID]; ok {
		d.IsPrimary = true
		d.UpdatedAt = time.Now()
		return true, nil
	}

	return false, sql.ErrNoRows
}

func (m *MockStore) GetPrimaryDelivery(alertGroupID, provider string) (*model.NotificationDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var selected *model.NotificationDelivery
	for _, d := range m.notificationDeliveries {
		if d.AlertGroupID != alertGroupID || d.Provider != provider || !d.IsPrimary {
			continue
		}
		if selected == nil || d.CreatedAt.After(selected.CreatedAt) {
			selected = d
		}
	}
	if selected == nil {
		return nil, nil
	}
	copy := *selected
	if selected.JobStepID != nil {
		jobStepID := *selected.JobStepID
		copy.JobStepID = &jobStepID
	}
	return &copy, nil
}

func (m *MockStore) GetFirehoseDelivery(alertGroupID, provider string) (*model.NotificationDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var selected *model.NotificationDelivery
	for _, d := range m.notificationDeliveries {
		if d.AlertGroupID != alertGroupID || d.Provider != provider || !d.IsFirehose || !d.SupportsUpdate {
			continue
		}
		if selected == nil || d.CreatedAt.After(selected.CreatedAt) {
			selected = d
		}
	}
	if selected == nil {
		return nil, nil
	}
	copy := *selected
	if selected.JobStepID != nil {
		jobStepID := *selected.JobStepID
		copy.JobStepID = &jobStepID
	}
	return &copy, nil
}

func (m *MockStore) GetDeliveryByID(id string) (*model.NotificationDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, d := range m.notificationDeliveries {
		if d.ID == id {
			copy := *d
			if d.JobStepID != nil {
				jobStepID := *d.JobStepID
				copy.JobStepID = &jobStepID
			}
			return &copy, nil
		}
	}
	return nil, nil
}

func (m *MockStore) ListDeliveries(alertGroupID string) ([]*model.NotificationDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var deliveries []*model.NotificationDelivery
	for _, d := range m.notificationDeliveries {
		if d.AlertGroupID != alertGroupID {
			continue
		}
		copy := *d
		if d.JobStepID != nil {
			jobStepID := *d.JobStepID
			copy.JobStepID = &jobStepID
		}
		deliveries = append(deliveries, &copy)
	}

	sort.Slice(deliveries, func(i, j int) bool {
		return deliveries[i].CreatedAt.Before(deliveries[j].CreatedAt)
	})

	return deliveries, nil
}

func (m *MockStore) HasPrimaryDelivery(alertGroupID, provider string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, d := range m.notificationDeliveries {
		if d.AlertGroupID == alertGroupID && d.Provider == provider && d.IsPrimary {
			return true, nil
		}
	}
	return false, nil
}

func (m *MockStore) UpdateDeliveryPayload(deliveryID, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, d := range m.notificationDeliveries {
		if d.ID == deliveryID {
			d.ProviderPayload = payload
			return nil
		}
	}
	return nil
}

// ========================================
// Incidents (stub for future)
// ========================================

func (m *MockStore) CreateIncident(i *model.Incident) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.incidentSeq++
	i.ID = m.incidentSeq
	incCopy := *i
	m.incidents[i.ID] = &incCopy
	return nil
}

func (m *MockStore) GetIncidentByID(id int) (*model.Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if inc, ok := m.incidents[id]; ok {
		incCopy := *inc
		return &incCopy, nil
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetAllIncidents() ([]*model.Incident, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.Incident
	for _, inc := range m.incidents {
		incCopy := *inc
		result = append(result, &incCopy)
	}
	return result, nil
}

// ========================================
// Teams
// ========================================

func (m *MockStore) CreateTeam(t *model.Team) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	teamCopy := *t
	m.teams[t.ID] = &teamCopy
	m.teamMembers[t.ID] = make(map[string]model.TeamMemberRole)
	return nil
}

func (m *MockStore) GetTeamByID(id string) (*model.Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if t, ok := m.teams[id]; ok {
		teamCopy := *t
		return &teamCopy, nil
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetAllTeams() ([]*model.Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.Team
	for _, t := range m.teams {
		teamCopy := *t
		result = append(result, &teamCopy)
	}
	return result, nil
}

func (m *MockStore) UpdateTeam(t *model.Team) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.teams[t.ID]; ok {
		existing.Name = t.Name
		existing.Description = t.Description
		existing.DefaultPolicyID = t.DefaultPolicyID
		existing.SeverityRoutes = t.SeverityRoutes
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) DeleteTeam(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.teamMembers, id)
	delete(m.teams, id)
	return nil
}

// ========================================
// Users
// ========================================

func (m *MockStore) CreateUser(u *model.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	userCopy := *u
	m.users[u.ID] = &userCopy
	return nil
}

func (m *MockStore) GetUserByID(id string) (*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.GetUserByIDError != nil {
		return nil, m.GetUserByIDError
	}
	if u, ok := m.users[id]; ok {
		userCopy := *u
		return &userCopy, nil
	}
	return nil, sql.ErrNoRows
}

// GetActiveUserByID mirrors the store: an erased user is not found.
func (m *MockStore) GetActiveUserByID(id string) (*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.GetUserByIDError != nil {
		return nil, m.GetUserByIDError
	}
	if m.erasedUsers[id] {
		return nil, ErrUserNotFound
	}
	if u, ok := m.users[id]; ok {
		userCopy := *u
		return &userCopy, nil
	}
	return nil, ErrUserNotFound
}

// EraseUser marks a mock user as soft-deleted: GetUserByID keeps returning
// them for history, GetActiveUserByID stops.
func (m *MockStore) EraseUser(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.erasedUsers == nil {
		m.erasedUsers = map[string]bool{}
	}
	m.erasedUsers[id] = true
}

func (m *MockStore) GetUsersByIDs(ids []string) ([]*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.User
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			userCopy := *u
			result = append(result, &userCopy)
		}
	}
	return result, nil
}

func (m *MockStore) GetUserByEmail(email string) (*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, u := range m.users {
		if u.Email == email {
			userCopy := *u
			return &userCopy, nil
		}
	}
	return nil, sql.ErrNoRows
}

// GetAllUsers excludes erased users, like the store: the operator's user list
// is a list of people who exist, not a log of who ever did.
func (m *MockStore) GetAllUsers() ([]*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.User
	for id, u := range m.users {
		if m.erasedUsers[id] {
			continue
		}
		userCopy := *u
		result = append(result, &userCopy)
	}
	return result, nil
}

// AnonymizeUser strips the identifying fields, mirroring the erasure
// primitive. id and role survive: the ID is what history joins on.
func (m *MockStore) AnonymizeUser(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[id]; ok {
		u.Name = AnonymizedUserName
		u.Email = ""
		u.PasswordHash = ""
		u.AuthProvider = ""
	}
}

func (m *MockStore) UpdateUser(u *model.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Profile fields only, as in the store: role changes go through SetUserRole,
	// which is the one place the last-admin invariant is serialized.
	if existing, ok := m.users[u.ID]; ok && !m.erasedUsers[u.ID] {
		existing.Email = u.Email
		existing.Name = u.Name
		return nil
	}
	return ErrUserNotFound
}

func (m *MockStore) UpdateUserPassword(id, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if u, ok := m.users[id]; ok && !m.erasedUsers[id] {
		u.PasswordHash = hash
		return nil
	}
	return ErrUserNotFound
}

func (m *MockStore) UpdateUserAuthProvider(id, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if u, ok := m.users[id]; ok && !m.erasedUsers[id] {
		u.AuthProvider = provider
		return nil
	}
	return ErrUserNotFound
}

func (m *MockStore) DeleteUser(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove from all teams
	for _, members := range m.teamMembers {
		delete(members, id)
	}
	delete(m.users, id)
	return nil
}

// ========================================
// Team Members
// ========================================

func (m *MockStore) AddTeamMember(teamID, userID string, role model.TeamMemberRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.teams[teamID]; !ok {
		return sql.ErrNoRows
	}
	if _, ok := m.users[userID]; !ok || m.erasedUsers[userID] {
		return ErrUserNotFound
	}

	if m.teamMembers[teamID] == nil {
		m.teamMembers[teamID] = make(map[string]model.TeamMemberRole)
	}
	m.teamMembers[teamID][userID] = role
	return nil
}

func (m *MockStore) GetTeamMembers(teamID string) ([]*model.TeamMemberDetail, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	members, ok := m.teamMembers[teamID]
	if !ok {
		return []*model.TeamMemberDetail{}, nil
	}

	var result []*model.TeamMemberDetail
	for userID, role := range members {
		if u, ok := m.users[userID]; ok && !m.erasedUsers[userID] {
			tm := &model.TeamMemberDetail{
				User:     *u,
				TeamRole: role,
			}
			result = append(result, tm)
		}
	}
	return result, nil
}

func (m *MockStore) RemoveTeamMember(teamID, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if members, ok := m.teamMembers[teamID]; ok {
		delete(members, userID)
	}
	return nil
}

func (m *MockStore) GetTeamMembershipsForUser(userID string) (map[string]model.TeamMemberRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	memberships := make(map[string]model.TeamMemberRole)
	for teamID, members := range m.teamMembers {
		if role, ok := members[userID]; ok {
			memberships[teamID] = role
		}
	}
	return memberships, nil
}

// ========================================
// Timeline Events
// ========================================

func (m *MockStore) AddTimelineEvent(e *model.TimelineEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	eventCopy := *e
	m.timelineEvents[e.AlertGroupID] = append(m.timelineEvents[e.AlertGroupID], &eventCopy)
	return nil
}

func (m *MockStore) GetTimelineEvents(alertGroupID string) ([]*model.TimelineEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	events := m.timelineEvents[alertGroupID]
	var result []*model.TimelineEvent
	for _, e := range events {
		eventCopy := *e
		result = append(result, &eventCopy)
	}
	return result, nil
}

// ========================================
// Helpers
// ========================================

func (m *MockStore) copyAlertGroup(ag *model.AlertGroup) *model.AlertGroup {
	agCopy := *ag
	agCopy.Alerts = make([]model.Alert, len(ag.Alerts))
	copy(agCopy.Alerts, ag.Alerts)

	if ag.PolicySnapshot != nil {
		snapCopy := *ag.PolicySnapshot
		snapCopy.Steps = make([]*model.EscalationStepSnapshot, len(ag.PolicySnapshot.Steps))
		for i, s := range ag.PolicySnapshot.Steps {
			stepCopy := *s
			snapCopy.Steps[i] = &stepCopy
		}
		agCopy.PolicySnapshot = &snapCopy
	}

	return &agCopy
}



// ========================================
// Rotation Epochs (schedule history stubs)
// ========================================


// ========================================
// API Tokens
// ========================================

// activeUser mirrors the lifecycle rule the store enforces in SQL: nothing
// owned by a user may be created for, or resolved to, someone who has been
// erased. The mock has to agree, or API tests keep passing against states
// production has made impossible.
//
// Callers hold the lock.
func (m *MockStore) activeUser(userID string) bool {
	if m.erasedUsers[userID] {
		return false
	}
	_, ok := m.users[userID]
	return ok
}

func (m *MockStore) CreateAPIToken(token *model.APIToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.activeUser(token.UserID) {
		return ErrUserNotFound
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}
	tokenCopy := *token
	m.apiTokens[token.ID] = &tokenCopy
	return nil
}

func (m *MockStore) GetAPITokenByHash(hash string) (*model.APIToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, t := range m.apiTokens {
		if t.TokenHash == hash && m.activeUser(t.UserID) {
			tokenCopy := *t
			return &tokenCopy, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetUserAPITokens(userID string) ([]*model.APIToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.APIToken
	for _, t := range m.apiTokens {
		if t.UserID == userID {
			tokenCopy := *t
			result = append(result, &tokenCopy)
		}
	}
	return result, nil
}

func (m *MockStore) UpdateAPITokenLastUsed(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if t, ok := m.apiTokens[id]; ok {
		now := time.Now()
		t.LastUsedAt = &now
		return nil
	}
	return sql.ErrNoRows
}

func (m *MockStore) DeleteAPIToken(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.apiTokens, id)
	return nil
}

// ========================================
// External Identities + Link Tokens (Epic 7 Sprint 3)
// ========================================

type mockIdentity = model.ExternalIdentity

type mockLinkToken struct {
	UserID     string
	Provider   string
	TokenHash  string
	ExternalID string
	Attempts   int
	ExpiresAt  time.Time
}

func (m *MockStore) BindExternalIdentity(ei *model.ExternalIdentity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bindIdentityLocked(ei)
}

func (m *MockStore) bindIdentityLocked(ei *model.ExternalIdentity) error {
	if !m.activeUser(ei.UserID) {
		return ErrUserNotFound
	}
	// (provider, external_id) must be globally unique across users
	for _, other := range m.externalIdentities {
		if other.Provider == ei.Provider && other.ExternalID == ei.ExternalID && other.UserID != ei.UserID {
			return ErrExternalIdentityAlreadyLinked
		}
	}
	key := ei.UserID + "|" + ei.Provider
	if ei.ID == "" {
		if existing, ok := m.externalIdentities[key]; ok {
			ei.ID = existing.ID
			ei.CreatedAt = existing.CreatedAt
		} else {
			ei.ID = uuid.New().String()
		}
	}
	now := time.Now()
	if ei.CreatedAt.IsZero() {
		ei.CreatedAt = now
	}
	ei.UpdatedAt = now
	copy := *ei
	m.externalIdentities[key] = &copy
	return nil
}

func (m *MockStore) BindExternalIdentityIfAbsent(userID, provider, externalID, displayName string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.externalIdentities[userID+"|"+provider]; ok {
		return false, nil
	}
	// An erased user is the same answer as a conflict here: nothing changed.
	if !m.activeUser(userID) {
		return false, nil
	}
	ei := &model.ExternalIdentity{
		UserID: userID, Provider: provider, ExternalID: externalID, DisplayName: displayName,
	}
	if err := m.bindIdentityLocked(ei); err != nil {
		return false, err
	}
	return true, nil
}

func (m *MockStore) GetExternalIdentity(userID, provider string) (*model.ExternalIdentity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if ei, ok := m.externalIdentities[userID+"|"+provider]; ok {
		copy := *ei
		return &copy, nil
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetUserByExternalID(provider, externalID string) (*model.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ei := range m.externalIdentities {
		if ei.Provider == provider && ei.ExternalID == externalID && m.activeUser(ei.UserID) {
			copy := *m.users[ei.UserID]
			return &copy, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]*model.ExternalIdentity)
	want := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		want[id] = true
	}
	for _, ei := range m.externalIdentities {
		if want[ei.UserID] {
			copy := *ei
			out[ei.UserID] = append(out[ei.UserID], &copy)
		}
	}
	return out, nil
}

func (m *MockStore) ListUserIdentities(userID string) ([]*model.ExternalIdentity, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*model.ExternalIdentity
	for _, ei := range m.externalIdentities {
		if ei.UserID == userID {
			copy := *ei
			out = append(out, &copy)
		}
	}
	return out, nil
}

func (m *MockStore) UnbindExternalIdentity(userID, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.externalIdentities, userID+"|"+provider)
	return nil
}

func (m *MockStore) IssueLinkToken(userID, provider, externalID, token string, expiresAt time.Time) error {
	if token == "" {
		return errors.New("token is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeUser(userID) {
		return ErrUserNotFound
	}
	hash := mockHashToken(token)
	// (provider, token_hash) global uniqueness — collisions retry at the caller.
	for k, lt := range m.linkTokens {
		if lt.Provider == provider && lt.TokenHash == hash && k != userID+"|"+provider {
			return errors.New("link token collision")
		}
	}
	m.linkTokens[userID+"|"+provider] = mockLinkToken{
		UserID: userID, Provider: provider, TokenHash: hash,
		ExternalID: externalID, Attempts: 0, ExpiresAt: expiresAt,
	}
	return nil
}

func (m *MockStore) ConfirmIdentityLink(userID, provider, token string) (*model.ExternalIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.activeUser(userID) {
		return nil, ErrUserNotFound
	}
	key := userID + "|" + provider
	lt, ok := m.linkTokens[key]
	if !ok {
		return nil, ErrLinkTokenInvalid
	}
	if time.Now().After(lt.ExpiresAt) {
		delete(m.linkTokens, key)
		return nil, ErrLinkTokenExpired
	}
	if lt.Attempts >= 3 {
		delete(m.linkTokens, key)
		return nil, ErrLinkTokenExpired
	}
	if mockHashToken(token) != lt.TokenHash {
		lt.Attempts++
		if lt.Attempts >= 3 {
			delete(m.linkTokens, key)
			return nil, ErrLinkTokenExpired
		}
		m.linkTokens[key] = lt
		return nil, ErrLinkTokenInvalid
	}
	if lt.ExternalID == "" {
		return nil, errors.New("link token has no external_id; nothing to bind")
	}
	ei := &model.ExternalIdentity{UserID: userID, Provider: provider, ExternalID: lt.ExternalID}
	if err := m.bindIdentityLocked(ei); err != nil {
		return nil, err
	}
	delete(m.linkTokens, key)
	copy := *ei
	return &copy, nil
}

func (m *MockStore) ConsumeLinkToken(provider, token, externalID, chatID, displayName string) (*model.ExternalIdentity, error) {
	if token == "" || externalID == "" {
		return nil, ErrLinkTokenInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	hash := mockHashToken(token)

	var key string
	var found mockLinkToken
	ok := false
	for k, lt := range m.linkTokens {
		if lt.Provider == provider && lt.TokenHash == hash {
			key, found, ok = k, lt, true
			break
		}
	}
	if !ok {
		return nil, ErrLinkTokenInvalid
	}
	if !m.activeUser(found.UserID) {
		return nil, ErrUserNotFound
	}
	if time.Now().After(found.ExpiresAt) {
		delete(m.linkTokens, key)
		return nil, ErrLinkTokenExpired
	}

	ei := &model.ExternalIdentity{
		UserID:      found.UserID,
		Provider:    provider,
		ExternalID:  externalID,
		ChatID:      chatID,
		DisplayName: displayName,
	}
	if err := m.bindIdentityLocked(ei); err != nil {
		return nil, err
	}
	delete(m.linkTokens, key)
	copy := *ei
	return &copy, nil
}

func mockHashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ========================================
// RBAC Methods
// ========================================

// GetUserTeamRole returns the team role for a user in a specific team.
// Returns sql.ErrNoRows if user is not a member of the team.
func (m *MockStore) GetUserTeamRole(userID, teamID string) (model.TeamMemberRole, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if members, ok := m.teamMembers[teamID]; ok {
		if role, ok := members[userID]; ok {
			return role, nil
		}
	}
	return "", sql.ErrNoRows
}

// SetUserRole updates a user's global role with last admin protection.
func (m *MockStore) SetUserRole(userID string, role model.UserRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Role is part of the lifecycle, like everything else a user owns: an
	// erased user has no role to change, and does not count as one of the
	// administrators the system is required to keep.
	if !m.activeUser(userID) {
		return ErrUserNotFound
	}
	user := m.users[userID]

	if user.Role == model.UserRoleAdmin && role != model.UserRoleAdmin {
		adminCount := 0
		for id, u := range m.users {
			if u.Role == model.UserRoleAdmin && m.activeUser(id) {
				adminCount++
			}
		}
		if adminCount <= 1 {
			return ErrLastAdmin
		}
	}

	user.Role = role
	return nil
}

// CountAdmins returns the number of users with admin role.
// CountAdmins counts ACTIVE admins. An erased one is not an administrator the
// system can fall back on.
func (m *MockStore) CountAdmins() (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for id, u := range m.users {
		if u.Role == model.UserRoleAdmin && m.activeUser(id) {
			count++
		}
	}
	return count, nil
}

// Jobs Mocks (Phase 2)
// NOTE: We add a map to store jobs for testing
func (m *MockStore) CreateJobWithDedup(job *model.Job, stages []*model.JobStage, steps []*model.JobStep) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Initialize maps if needed (hack for existing tests)
	if m.jobs == nil {
		m.jobs = make(map[string]*model.Job)
	}
	if m.jobStages == nil {
		m.jobStages = make(map[string]*model.JobStage)
	}
	if m.jobSteps == nil {
		m.jobSteps = make(map[string]*model.JobStep)
	}

	// Implement dedup: check if job with same dedup_key exists and is pending/running
	if job.DedupKey != nil {
		for _, existing := range m.jobs {
			if existing.DedupKey != nil && *existing.DedupKey == *job.DedupKey {
				if existing.Status == model.JobStatusPending || existing.Status == model.JobStatusRunning {
					// Dedup: return existing job ID, nothing was created
					return existing.ID, false, nil
				}
			}
		}
	}

	jobCopy := *job
	m.jobs[job.ID] = &jobCopy

	for _, stage := range stages {
		stageCopy := *stage
		m.jobStages[stage.ID] = &stageCopy
	}
	for _, step := range steps {
		stepCopy := *step
		m.jobSteps[step.ID] = &stepCopy
	}
	return job.ID, true, nil
}

// EnsureEscalationJob atomically transitions an AG from new/processing → processing
// and creates the escalation job + snapshot.
func (m *MockStore) EnsureEscalationJob(agID string, job *model.Job, stages []*model.JobStage, steps []*model.JobStep, snapshot *model.EscalationPolicySnapshot) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.jobs == nil {
		m.jobs = make(map[string]*model.Job)
	}
	if m.jobStages == nil {
		m.jobStages = make(map[string]*model.JobStage)
	}
	if m.jobSteps == nil {
		m.jobSteps = make(map[string]*model.JobStep)
	}

	ag, ok := m.alertGroups[agID]
	if !ok {
		return false, fmt.Errorf("alert group %s not found", agID)
	}

	// Only new or processing are eligible
	if ag.Status != model.AlertGroupStatusNew && ag.Status != model.AlertGroupStatusProcessing {
		return false, nil
	}

	// Transition to processing + touch updated_at
	ag.Status = model.AlertGroupStatusProcessing
	ag.UpdatedAt = time.Now()

	// Dedup: check if ANY escalation job exists for this AG (any status)
	for _, existing := range m.jobs {
		if existing.AlertGroupID != nil && *existing.AlertGroupID == agID && existing.Type == "escalation" {
			// Escalation already exists (any status) — don't create duplicate
			return false, nil
		}
	}

	// Create job + stages + steps
	jobCopy := *job
	m.jobs[job.ID] = &jobCopy
	for _, stage := range stages {
		stageCopy := *stage
		m.jobStages[stage.ID] = &stageCopy
	}
	for _, step := range steps {
		stepCopy := *step
		m.jobSteps[step.ID] = &stepCopy
	}

	// Save snapshot
	if snapshot != nil {
		ag.PolicyID = snapshot.PolicyID
		snapCopy := *snapshot
		snapCopy.Steps = make([]*model.EscalationStepSnapshot, len(snapshot.Steps))
		for i, s := range snapshot.Steps {
			sc := *s
			snapCopy.Steps[i] = &sc
		}
		ag.PolicySnapshot = &snapCopy
	}

	return true, nil
}

func (m *MockStore) GetJobByID(id string) (*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.jobs == nil {
		return nil, sql.ErrNoRows
	}
	if job, ok := m.jobs[id]; ok {
		jobCopy := *job
		return &jobCopy, nil
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetJobByDedupKey(dedupKey string) (*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var latest *model.Job
	for _, j := range m.jobs {
		if j.DedupKey != nil && *j.DedupKey == dedupKey {
			if latest == nil || j.CreatedAt.After(latest.CreatedAt) {
				latest = j
			}
		}
	}
	if latest != nil {
		jobCopy := *latest
		return &jobCopy, nil
	}
	return nil, sql.ErrNoRows
}

// MarkJobSucceeded is a test helper to mark a job as succeeded by dedup key
func (m *MockStore) MarkJobSucceeded(dedupKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, j := range m.jobs {
		if j.DedupKey != nil && *j.DedupKey == dedupKey {
			j.Status = model.JobStatusSucceeded
			return
		}
	}
}

func (m *MockStore) GetJobStepByID(stepID string) (*model.JobStep, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if step, ok := m.jobSteps[stepID]; ok {
		stepCopy := *step
		return &stepCopy, nil
	}
	return nil, sql.ErrNoRows
}

// GetJobStepsByJobID returns all steps for a job sorted by StepIndex (test helper, not on interface)
func (m *MockStore) GetJobStepsByJobID(jobID string) []*model.JobStep {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*model.JobStep
	for _, step := range m.jobSteps {
		if step.JobID == jobID {
			stepCopy := *step
			result = append(result, &stepCopy)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].StepIndex < result[j].StepIndex })
	return result
}
func (m *MockStore) ClaimNextJobSteps(limit int, duration time.Duration) ([]*model.JobStep, error) {
	return []*model.JobStep{}, nil // TODO: Implement if needed for loop testing
}
func (m *MockStore) UpdateJobStepIfOwned(step *model.JobStep, leaseToken string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.jobSteps[step.ID]
	if !ok {
		return false, nil
	}
	if existing.LockedBy == nil || *existing.LockedBy != leaseToken {
		return false, nil
	}
	stepCopy := *step
	stepCopy.UpdatedAt = time.Now()
	m.jobSteps[step.ID] = &stepCopy
	return true, nil
}

func (m *MockStore) FinishStepAndAdvance(stepID string, leaseToken string, outcome model.JobStepStatus, result string, stepError string) (model.AdvanceResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	step, ok := m.jobSteps[stepID]
	if !ok {
		return 0, fmt.Errorf("step %s not found", stepID)
	}

	// Find stage and job
	stage, ok := m.jobStages[step.StageID]
	if !ok {
		return 0, fmt.Errorf("stage %s not found", step.StageID)
	}
	job, ok := m.jobs[step.JobID]
	if !ok {
		return 0, fmt.Errorf("job %s not found", step.JobID)
	}

	// 1. Terminal guard
	if job.Status != model.JobStatusPending && job.Status != model.JobStatusRunning {
		if step.LockedBy != nil && *step.LockedBy == leaseToken {
			step.Status = model.JobStepStatusCanceled
			step.LockedUntil = nil
		}
		return model.AdvanceJobAlreadyTerminal, nil
	}

	// 2. Already-advanced guard
	if stage.Status == model.JobStageStatusSucceeded || stage.Status == model.JobStageStatusFailed || stage.Status == model.JobStageStatusCanceled {
		return model.AdvanceAlreadyAdvanced, nil
	}

	// 3. Lease check
	if step.LockedBy == nil || *step.LockedBy != leaseToken || step.Status != model.JobStepStatusRunning {
		return model.AdvanceLeaseLost, nil
	}

	// Finalize step
	now := time.Now()
	step.Status = outcome
	if result != "" {
		step.Result = json.RawMessage(fmt.Sprintf("%q", result))
	}
	if stepError != "" {
		step.Error = &stepError
	}
	step.LockedUntil = nil
	step.UpdatedAt = now

	// 4. Hard-fail
	if outcome == model.JobStepStatusFailed && !step.ContinueOnFailure {
		stage.Status = model.JobStageStatusFailed
		stage.UpdatedAt = now
		job.Status = model.JobStatusFailed
		if stepError != "" {
			job.Error = &stepError
		}
		job.FinishedAt = &now
		job.UpdatedAt = now
		return model.AdvanceJobFinished, nil
	}

	// 5. Pending siblings
	pendingCount := 0
	for _, s := range m.jobSteps {
		if s.StageID == step.StageID && s.Status != model.JobStepStatusSucceeded && s.Status != model.JobStepStatusFailed && s.Status != model.JobStepStatusCanceled {
			pendingCount++
		}
	}
	if pendingCount > 0 {
		return model.AdvanceWaitingSiblings, nil
	}

	// 6. Hard-fail siblings
	for _, s := range m.jobSteps {
		if s.StageID == step.StageID && s.Status == model.JobStepStatusFailed && !s.ContinueOnFailure {
			stage.Status = model.JobStageStatusFailed
			stage.UpdatedAt = now
			job.Status = model.JobStatusFailed
			errMsg := "step failed"
			job.Error = &errMsg
			job.FinishedAt = &now
			job.UpdatedAt = now
			return model.AdvanceJobFinished, nil
		}
	}

	// 7. Stage completed — determine status
	hasAnyFailed := false
	for _, s := range m.jobSteps {
		if s.StageID == step.StageID && s.Status == model.JobStepStatusFailed {
			hasAnyFailed = true
			break
		}
	}
	if hasAnyFailed {
		stage.Status = model.JobStageStatusFailed
	} else {
		stage.Status = model.JobStageStatusSucceeded
	}
	stage.UpdatedAt = now

	// 8. Find next blocked stage
	var nextStage *model.JobStage
	for _, st := range m.jobStages {
		if st.JobID == job.ID && st.StageIndex == stage.StageIndex+1 && st.Status == model.JobStageStatusBlocked {
			nextStage = st
			break
		}
	}

	if nextStage == nil {
		// Last stage — finish job
		jobHasFailed := false
		for _, s := range m.jobSteps {
			if s.JobID == job.ID && s.Status == model.JobStepStatusFailed {
				jobHasFailed = true
				break
			}
		}
		if jobHasFailed {
			job.Status = model.JobStatusFailed
		} else {
			job.Status = model.JobStatusSucceeded
		}
		job.FinishedAt = &now
		job.UpdatedAt = now
		return model.AdvanceJobFinished, nil
	}

	// 9. Unlock next stage + steps
	nextStage.Status = model.JobStageStatusActive
	nextStage.UpdatedAt = now
	for _, s := range m.jobSteps {
		if s.StageID == nextStage.ID && s.Status == model.JobStepStatusBlocked {
			s.Status = model.JobStepStatusPending
			s.NextRunAt = &now
			s.UpdatedAt = now
		}
	}
	job.CurrentStage = stage.StageIndex + 1
	job.UpdatedAt = now

	return model.AdvanceUnlockedNextStage, nil
}
func (m *MockStore) CancelJobByDedupKey(dedupKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, job := range m.jobs {
		if job.DedupKey != nil && *job.DedupKey == dedupKey {
			if job.Status == model.JobStatusPending || job.Status == model.JobStatusRunning {
				job.Status = model.JobStatusCanceled
				// Cancel active/blocked stages
				for _, stage := range m.jobStages {
					if stage.JobID == job.ID {
						if stage.Status == model.JobStageStatusActive || stage.Status == model.JobStageStatusBlocked {
							stage.Status = model.JobStageStatusCanceled
						}
					}
				}
			}
		}
	}
	return nil
}
func (m *MockStore) ExtendStepLease(stepID string, leaseToken string, duration time.Duration) error {
	return nil
}
func (m *MockStore) GetJobsByIDs(ids []string) (map[string]*model.Job, error) {
	return make(map[string]*model.Job), nil
}

func (m *MockStore) FailJob(jobID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[jobID]; ok {
		job.Status = model.JobStatusFailed
		job.Error = &reason
		now := time.Now()
		job.FinishedAt = &now
		job.UpdatedAt = now
		return nil
	}
	return sql.ErrNoRows
}

// ========================================
// Escalation Policies (Phase 4)
// ========================================

func (m *MockStore) CreateEscalationPolicy(p *model.EscalationPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	policyCopy := *p
	// Deep copy steps
	policyCopy.Steps = make([]*model.EscalationStep, len(p.Steps))
	for i, step := range p.Steps {
		stepCopy := *step
		policyCopy.Steps[i] = &stepCopy
	}
	m.escalationPolicies[p.ID] = &policyCopy
	return nil
}

func (m *MockStore) GetEscalationPolicyByID(id string) (*model.EscalationPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.escalationPolicies[id]; ok {
		policyCopy := *p
		policyCopy.Steps = make([]*model.EscalationStep, len(p.Steps))
		for i, step := range p.Steps {
			stepCopy := *step
			policyCopy.Steps[i] = &stepCopy
		}
		return &policyCopy, nil
	}
	return nil, sql.ErrNoRows
}

func (m *MockStore) GetAllEscalationPolicies() ([]*model.EscalationPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*model.EscalationPolicy
	for _, p := range m.escalationPolicies {
		policyCopy := *p
		result = append(result, &policyCopy)
	}
	return result, nil
}

func (m *MockStore) UpdateEscalationPolicy(p *model.EscalationPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.escalationPolicies[p.ID]; !ok {
		return sql.ErrNoRows
	}
	policyCopy := *p
	policyCopy.Steps = make([]*model.EscalationStep, len(p.Steps))
	for i, step := range p.Steps {
		stepCopy := *step
		policyCopy.Steps[i] = &stepCopy
	}
	m.escalationPolicies[p.ID] = &policyCopy
	return nil
}

func (m *MockStore) DeleteEscalationPolicy(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.escalationPolicies[id]; !ok {
		return sql.ErrNoRows
	}
	delete(m.escalationPolicies, id)
	return nil
}

func (m *MockStore) GetPoliciesByTeamID(teamID string) ([]*model.EscalationPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*model.EscalationPolicy
	for _, p := range m.escalationPolicies {
		if p.TeamID != nil && *p.TeamID == teamID {
			policyCopy := *p
			result = append(result, &policyCopy)
		}
	}
	return result, nil
}

func (m *MockStore) GetEscalationPoliciesForUser(userID string) ([]*model.EscalationPolicy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	userTeams := make(map[string]bool)
	for teamID, members := range m.teamMembers {
		if _, ok := members[userID]; ok {
			userTeams[teamID] = true
		}
	}

	var result []*model.EscalationPolicy
	for _, p := range m.escalationPolicies {
		// Include Global OR User's Team
		if p.TeamID == nil {
			pCopy := *p
			result = append(result, &pCopy)
		} else if userTeams[*p.TeamID] {
			pCopy := *p
			result = append(result, &pCopy)
		}
	}
	return result, nil
}

// ========================================
// Integrations (Phase 5)
// ========================================

func (m *MockStore) CreateIntegration(i *model.Integration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Set direction and ID. Same invariant as Store.CreateIntegration —
	// type is validated upstream; an unknown one here is a programming error.
	dir, ok := integrations.DirectionFor(i.Type)
	if !ok {
		return fmt.Errorf("unknown integration type %s", i.Type)
	}
	i.Direction = dir
	i.ID = fmt.Sprintf("int-%d", len(m.integrations)+1)
	i.CreatedAt = time.Now()
	i.UpdatedAt = time.Now()

	// Check unique constraint for outbound (generic_webhook allows multiple)
	if i.Direction == model.IntegrationDirectionOutbound && i.Type != model.IntegrationTypeGenericWebhook {
		for _, existing := range m.integrations {
			if existing.Type == i.Type && existing.Direction == model.IntegrationDirectionOutbound {
				return ErrDuplicateIntegration
			}
		}
	}

	copy := *i
	m.integrations[i.ID] = &copy
	return nil
}

func (m *MockStore) GetIntegrationByID(id string) (*model.Integration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.GetIntegrationByIDError != nil {
		return nil, m.GetIntegrationByIDError
	}
	if i, ok := m.integrations[id]; ok {
		copy := *i
		return &copy, nil
	}
	return nil, ErrIntegrationNotFound
}

func (m *MockStore) GetIntegrationByType(integrationType model.IntegrationType) (*model.Integration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, i := range m.integrations {
		// Mirror Store.GetIntegrationByType, which filters enabled = true.
		if i.Type == integrationType && i.Enabled {
			copy := *i
			return &copy, nil
		}
	}
	return nil, ErrIntegrationNotFound
}

func (m *MockStore) GetIntegrationsByType(integrationType model.IntegrationType) ([]*model.Integration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.Integration
	for _, i := range m.integrations {
		if i.Type == integrationType {
			copy := *i
			result = append(result, &copy)
		}
	}
	return result, nil
}

func (m *MockStore) GetAllIntegrations() ([]*model.Integration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.Integration
	for _, i := range m.integrations {
		copy := *i
		result = append(result, &copy)
	}
	return result, nil
}

func (m *MockStore) UpdateIntegration(i *model.Integration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.integrations[i.ID]; !ok {
		return ErrIntegrationNotFound
	}

	i.UpdatedAt = time.Now()
	copy := *i
	m.integrations[i.ID] = &copy
	return nil
}

func (m *MockStore) DeleteIntegration(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.integrations[id]; !ok {
		return ErrIntegrationNotFound
	}
	delete(m.integrations, id)
	return nil
}

// GetAPITokenByID retrieves an API token by ID from MockStore
func (m *MockStore) GetAPITokenByID(id string) (*model.APIToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if token, ok := m.apiTokens[id]; ok {
		// Return copy
		tokenCopy := *token
		return &tokenCopy, nil
	}
	return nil, sql.ErrNoRows
}

// GetMetricsSnapshot returns mock metrics data.
func (m *MockStore) GetMetricsSnapshot() (*MetricsSnapshot, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	snap := &MetricsSnapshot{}

	// Active alert groups
	counts := make(map[string]map[string]int) // team -> severity -> count
	for _, ag := range m.alertGroups {
		if ag.Status == model.AlertGroupStatusResolved || ag.Status == model.AlertGroupStatusClosed {
			continue
		}
		if counts[ag.TeamID] == nil {
			counts[ag.TeamID] = make(map[string]int)
		}
		counts[ag.TeamID][ag.Severity]++
	}
	for team, sevMap := range counts {
		for sev, count := range sevMap {
			snap.ActiveAlertGroups = append(snap.ActiveAlertGroups, AlertGroupCount{
				TeamID: team, Severity: sev, Count: count,
			})
		}
	}

	// Alert groups by status
	statusCounts := make(map[string]map[string]map[string]int) // team -> severity -> status -> count
	for _, ag := range m.alertGroups {
		if statusCounts[ag.TeamID] == nil {
			statusCounts[ag.TeamID] = make(map[string]map[string]int)
		}
		if statusCounts[ag.TeamID][ag.Severity] == nil {
			statusCounts[ag.TeamID][ag.Severity] = make(map[string]int)
		}
		statusCounts[ag.TeamID][ag.Severity][string(ag.Status)]++
	}
	for team, sevMap := range statusCounts {
		for sev, statMap := range sevMap {
			for status, count := range statMap {
				snap.AlertGroupsByStatus = append(snap.AlertGroupsByStatus, AlertGroupStatusCount{
					TeamID: team, Severity: sev, Status: status, Count: count,
				})
			}
		}
	}

	// Teams without on-call and teams with permanent on-call are reported as
	// zero, deliberately. Both are answers about the revision model, which this
	// mock does not implement - deliberately, so that there is one projection
	// rather than two - and computing them from something else here would be a
	// second definition of "has a schedule" that could disagree with the real one.
	//
	// Their coverage is the integration test against the real query.

	// Teams without escalation policy
	for _, team := range m.teams {
		if team.DefaultPolicyID == "" {
			snap.TeamsWithoutPolicy++
		}
	}

	// Outbox events by status
	evtCounts := make(map[string]int)
	for _, e := range m.outboxEvents {
		evtCounts[string(e.Status)]++
	}
	for status, count := range evtCounts {
		snap.OutboxEventsByStatus = append(snap.OutboxEventsByStatus, StatusCount{Status: status, Count: count})
	}

	// Outbox deliveries by status
	delCounts := make(map[string]int)
	for _, d := range m.outboxDeliveries {
		delCounts[string(d.Status)]++
	}
	for status, count := range delCounts {
		snap.OutboxDeliveriesByStatus = append(snap.OutboxDeliveriesByStatus, StatusCount{Status: status, Count: count})
	}

	return snap, nil
}

// ========================================
// Outbox
// ========================================

func (m *MockStore) CreateOutboxEvent(event *model.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.Status == "" {
		event.Status = model.OutboxEventStatusPending
	}
	if event.Actor == "" {
		event.Actor = "system"
	}
	event.CreatedAt = time.Now()
	// Store a copy so the caller's pointer and the mock's pointer are independent
	// (mirrors real DB semantics where in-memory mutations don't affect stored rows).
	cp := *event
	if event.LockedBy != nil {
		v := *event.LockedBy
		cp.LockedBy = &v
	}
	if event.LockedUntil != nil {
		v := *event.LockedUntil
		cp.LockedUntil = &v
	}
	m.outboxEvents[event.ID] = &cp
	return nil
}

func (m *MockStore) GetOutboxEventByID(id string) (*model.OutboxEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	event, ok := m.outboxEvents[id]
	if !ok {
		return nil, ErrOutboxEventNotFound
	}
	return event, nil
}

func (m *MockStore) GetPendingOutboxEvents(limit int) ([]*model.OutboxEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now()
	var result []*model.OutboxEvent
	for _, e := range m.outboxEvents {
		if e.Status != model.OutboxEventStatusPending && e.Status != model.OutboxEventStatusProcessing {
			continue
		}
		if e.LockedUntil != nil && e.LockedUntil.After(now) {
			continue
		}
		if e.NextAttemptAt != nil && e.NextAttemptAt.After(now) {
			continue
		}
		result = append(result, e)
	}
	// Sort by NextAttemptAt NULLS FIRST to match Postgres
	slices.SortFunc(result, func(a, b *model.OutboxEvent) int {
		return cmpTimePtr(a.NextAttemptAt, b.NextAttemptAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *MockStore) UpdateOutboxEvent(event *model.OutboxEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.outboxEvents[event.ID]; !ok {
		return ErrOutboxEventNotFound
	}
	// Store a copy (mirrors real DB: in-memory mutations don't affect stored rows).
	cp := *event
	if event.LockedBy != nil {
		v := *event.LockedBy
		cp.LockedBy = &v
	}
	if event.LockedUntil != nil {
		v := *event.LockedUntil
		cp.LockedUntil = &v
	}
	m.outboxEvents[event.ID] = &cp
	return nil
}

func (m *MockStore) UpdateOutboxEventIfOwned(event *model.OutboxEvent, workerID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.UpdateOutboxEventIfOwnedError != nil {
		return false, m.UpdateOutboxEventIfOwnedError
	}

	existing, ok := m.outboxEvents[event.ID]
	if !ok {
		return false, nil
	}
	if existing.LockedBy == nil || *existing.LockedBy != workerID {
		return false, nil
	}
	// Store a copy (mirrors real DB: in-memory mutations don't affect stored rows).
	cp := *event
	if event.LockedBy != nil {
		v := *event.LockedBy
		cp.LockedBy = &v
	}
	if event.LockedUntil != nil {
		v := *event.LockedUntil
		cp.LockedUntil = &v
	}
	m.outboxEvents[event.ID] = &cp
	return true, nil
}

func (m *MockStore) CreateOutboxDelivery(delivery *model.OutboxDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if delivery.ID == "" {
		delivery.ID = uuid.New().String()
	}
	if delivery.Status == "" {
		delivery.Status = model.OutboxDeliveryPending
	}
	delivery.CreatedAt = time.Now()

	// Check unique(event_id, integration_id)
	for _, d := range m.outboxDeliveries {
		if d.EventID == delivery.EventID && d.IntegrationID == delivery.IntegrationID {
			return fmt.Errorf("duplicate delivery for event %s and integration %s", delivery.EventID, delivery.IntegrationID)
		}
	}

	if m.CreateOutboxDeliveryError != nil {
		return m.CreateOutboxDeliveryError
	}

	m.outboxDeliveries[delivery.ID] = delivery
	return nil
}

func (m *MockStore) GetOutboxDeliveryByID(id string) (*model.OutboxDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	d, ok := m.outboxDeliveries[id]
	if !ok {
		return nil, ErrOutboxDeliveryNotFound
	}
	return d, nil
}

func (m *MockStore) GetOutboxDelivery(eventID, integrationID string) (*model.OutboxDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, d := range m.outboxDeliveries {
		if d.EventID == eventID && d.IntegrationID == integrationID {
			return d, nil
		}
	}
	return nil, ErrOutboxDeliveryNotFound
}

func (m *MockStore) GetDeliveriesByEventID(eventID string) ([]*model.OutboxDelivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*model.OutboxDelivery
	for _, d := range m.outboxDeliveries {
		if d.EventID == eventID {
			result = append(result, d)
		}
	}
	slices.SortFunc(result, func(a, b *model.OutboxDelivery) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return result, nil
}

func (m *MockStore) GetDeliveriesByIntegrationID(integrationID string, limit, offset int) ([]*model.OutboxDelivery, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []*model.OutboxDelivery
	for _, d := range m.outboxDeliveries {
		if d.IntegrationID == integrationID {
			all = append(all, d)
		}
	}
	slices.SortFunc(all, func(a, b *model.OutboxDelivery) int {
		return b.CreatedAt.Compare(a.CreatedAt) // DESC
	})

	total := len(all)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return all[offset:end], total, nil
}

func (m *MockStore) UpdateOutboxDelivery(delivery *model.OutboxDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.outboxDeliveries[delivery.ID]; !ok {
		return ErrOutboxDeliveryNotFound
	}
	m.outboxDeliveries[delivery.ID] = delivery
	return nil
}

func (m *MockStore) ReplayOutboxDelivery(deliveryID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ReplayOutboxDeliveryError != nil {
		return m.ReplayOutboxDeliveryError
	}

	d, ok := m.outboxDeliveries[deliveryID]
	if !ok {
		return ErrOutboxDeliveryNotFound
	}

	// CAS: only terminal deliveries can be replayed
	if d.Status != model.OutboxDeliverySent && d.Status != model.OutboxDeliveryFailed {
		return ErrOutboxDeliveryNotTerminal
	}

	// Reset delivery
	d.Status = model.OutboxDeliveryPending
	d.Attempts = 0
	d.NextAttemptAt = nil
	d.LastHTTPStatus = nil
	d.LastError = nil
	d.ResponseBodyTrunc = nil
	d.SentAt = nil

	// Re-open parent event if terminal
	if evt, ok := m.outboxEvents[d.EventID]; ok {
		if evt.Status == model.OutboxEventStatusCompleted || evt.Status == model.OutboxEventStatusFailed {
			evt.Status = model.OutboxEventStatusProcessing
			evt.NextAttemptAt = nil
			evt.LockedUntil = nil
			evt.LockedBy = nil
			evt.LastError = nil
			evt.SentAt = nil
		}
	}
	return nil
}

func (m *MockStore) ClaimOutboxEvents(workerID string, limit int, leaseDuration time.Duration) ([]*model.OutboxEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var result []*model.OutboxEvent
	for _, e := range m.outboxEvents {
		if e.Status != model.OutboxEventStatusPending && e.Status != model.OutboxEventStatusProcessing {
			continue
		}
		if e.LockedUntil != nil && e.LockedUntil.After(now) {
			continue
		}
		if e.NextAttemptAt != nil && e.NextAttemptAt.After(now) {
			continue
		}
		result = append(result, e)
	}
	slices.SortFunc(result, func(a, b *model.OutboxEvent) int {
		return cmpTimePtr(a.NextAttemptAt, b.NextAttemptAt)
	})
	if len(result) > limit {
		result = result[:limit]
	}
	// Apply lock
	lockedUntil := now.Add(leaseDuration)
	for _, e := range result {
		e.Status = model.OutboxEventStatusProcessing
		e.LockedBy = &workerID
		e.LockedUntil = &lockedUntil
	}
	return result, nil
}

func (m *MockStore) ExtendOutboxEventLease(eventID, workerID string, until time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.ExtendOutboxEventLeaseError != nil {
		return false, m.ExtendOutboxEventLeaseError
	}
	if m.ExtendOutboxEventLeaseResult != nil {
		return *m.ExtendOutboxEventLeaseResult, nil
	}

	e, ok := m.outboxEvents[eventID]
	if !ok {
		return false, nil
	}
	if e.Status != model.OutboxEventStatusProcessing || e.LockedBy == nil || *e.LockedBy != workerID {
		return false, nil
	}
	e.LockedUntil = &until
	return true, nil
}

func (m *MockStore) CreateDeliveryAttempt(attempt *model.DeliveryAttempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if attempt.ID == "" {
		attempt.ID = uuid.New().String()
	}
	attempt.CreatedAt = time.Now()
	m.deliveryAttempts[attempt.DeliveryID] = append(m.deliveryAttempts[attempt.DeliveryID], attempt)
	return nil
}

func (m *MockStore) GetDeliveryAttempts(deliveryID string) ([]*model.DeliveryAttempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	attempts := m.deliveryAttempts[deliveryID]
	result := make([]*model.DeliveryAttempt, len(attempts))
	copy(result, attempts)
	return result, nil
}

// Ensure Store implements StoreInterface
var _ StoreInterface = (*Store)(nil)
var _ StoreInterface = (*MockStore)(nil)
