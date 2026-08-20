package ingester

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

type AMPayload struct {
	Status       string            `json:"status"`
	GroupKey     string            `json:"groupKey"`
	ExternalURL  string            `json:"externalURL"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []model.Alert     `json:"alerts"`
}

// WebhookSecretValidator interface for validating webhook secrets
type WebhookSecretValidator interface {
	ValidateWebhookSecret(secret string) bool
}

type Ingester struct {
	store           store.StoreInterface
	cfg             *config.Config
	secretValidator WebhookSecretValidator
}

func NewIngester(s store.StoreInterface, cfg *config.Config, secretValidator WebhookSecretValidator) *Ingester {
	return &Ingester{store: s, cfg: cfg, secretValidator: secretValidator}
}

func (i *Ingester) RegisterRoutes(e *echo.Echo) {
	e.POST("/webhook/alertmanager", i.handleWebhook)
}

func (i *Ingester) handleWebhook(c echo.Context) error {
	// Authentication: validate webhook secret (rejects if no integrations configured)
	token := c.QueryParam("token")

	if !i.secretValidator.ValidateWebhookSecret(token) {
		log.Printf("Ingester: Unauthorized webhook request")
		return c.String(http.StatusUnauthorized, "Unauthorized")
	}

	var payload AMPayload
	if err := c.Bind(&payload); err != nil {
		log.Printf("Ingester: Failed to bind payload: %v", err)
		return c.String(http.StatusBadRequest, "Bad Request")
	}

	// 1. Deduplication (GroupKey)
	alertKey := payload.GroupKey
	if alertKey == "" {
		if len(payload.Alerts) > 0 {
			alertKey = payload.Alerts[0].Fingerprint
		} else {
			return c.String(http.StatusBadRequest, "No alerts or groupKey")
		}
	}
	if alertKey == "" {
		return c.String(http.StatusBadRequest, "Empty dedup key: groupKey and fingerprint both missing")
	}

	// 2. Classification
	teamID, ok := payload.CommonLabels["team"]
	if !ok || teamID == "" {
		teamID = "triage"
	}
	severity, ok := payload.CommonLabels["severity"]
	if !ok || severity == "" {
		severity = "info"
	}
	severity = strings.ToLower(severity)

	metrics.AlertsReceivedTotal.WithLabelValues(teamID, severity).Inc()
	log.Printf("Ingester: Group %s (Team: %s, Sev: %s, Alerts: %d)", alertKey, teamID, severity, len(payload.Alerts))

	// 3. Find Active Alert Group
	active, err := i.store.GetActiveAlertGroupByAlertKey(alertKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// Real DB error (not "not found") — return 500 so Alertmanager retries
		log.Printf("Ingester: DB error looking up group %s: %v", alertKey, err)
		return c.String(http.StatusInternalServerError, "DB lookup failed")
	}
	if active != nil {
		return i.mergeIntoGroup(c, active, payload.Alerts)
	}

	// 4. Create New Alert Group
	// Filter to firing alerts only — resolved alerts shouldn't appear in a new group.
	var firingAlerts []model.Alert
	for _, a := range payload.Alerts {
		if a.Status == model.AlertStatusFiring {
			firingAlerts = append(firingAlerts, a)
		}
	}
	if len(firingAlerts) == 0 {
		return c.String(http.StatusOK, "Ignored Resolved")
	}

	ag := &model.AlertGroup{
		ID:          uuid.New().String(),
		AlertKey:    alertKey,
		Status:      model.AlertGroupStatusNew,
		Title:       i.generateTitle(&payload),
		TeamID:      teamID,
		Severity:    severity,
		ExternalURL: payload.ExternalURL, // Link to Alertmanager source
		Alerts:      firingAlerts,        // Only firing alerts
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}

	// Build timeline events for atomic insert — µs offsets ensure deterministic ordering
	now := time.Now()
	timelineEvents := []*model.TimelineEvent{
		{
			ID:           uuid.New().String(),
			AlertGroupID: ag.ID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created: " + ag.Title,
			Actor:        "system",
			Metadata:     map[string]string{"team": teamID, "severity": severity},
			CreatedAt:    now,
		},
	}
	for i, a := range firingAlerts {
		timelineEvents = append(timelineEvents, &model.TimelineEvent{
			ID:           uuid.New().String(),
			AlertGroupID: ag.ID,
			Type:         model.TimelineEventAlertAdded,
			Message:      "Alert: " + a.Labels["alertname"],
			Actor:        "system",
			Metadata:     map[string]string{"fingerprint": a.Fingerprint},
			CreatedAt:    now.Add(time.Duration(i+1) * time.Microsecond),
		})
	}

	// Resolve team name for webhook payload snapshot.
	// Not-found is normal (unknown team label from Alertmanager) — use teamID as snapshot.
	// Any other DB error is transient and must fail the request so Alertmanager retries.
	teamName := teamID
	unknownTeam := true
	team, err := i.store.GetTeamByID(teamID)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("Ingester: Failed to resolve team %s: %v", teamID, err)
		return c.String(http.StatusInternalServerError, "Failed to persist")
	}
	if team != nil {
		teamName = team.Name
		unknownTeam = false
	}
	ag.TeamNameSnapshot = teamName

	// Build outbox event for webhook fan-out (including global subscriptions for unknown teams)
	eventPayload, err := model.BuildWebhookEventPayload(
		model.OutboxEventFiring, ag, teamName, "system", "", now,
	)
	if err != nil {
		log.Printf("Ingester: Failed to build event payload: %v", err)
		return c.String(http.StatusInternalServerError, "Failed to persist")
	}
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventFiring,
		AlertGroupID: ag.ID,
		TeamID:       teamID,
		Actor:        "system",
		Payload:      eventPayload,
	}

	// Atomic: AG + timeline + outbox in single transaction
	if err := i.store.CreateAlertGroupAtomic(ag, timelineEvents, outboxEvent); err != nil {
		// Handle duplicate key (race condition: another webhook created the group concurrently).
		// Primary: lib/pq unique_violation (23505). Fallback: string match for other drivers.
		var pqErr *pq.Error
		isDuplicateKey := (errors.As(err, &pqErr) && pqErr.Code == "23505") ||
			strings.Contains(err.Error(), "duplicate key")
		if isDuplicateKey {
			log.Printf("Ingester: Duplicate key for %s, retrying as merge", alertKey)
			active, retryErr := i.store.GetActiveAlertGroupByAlertKey(alertKey)
			if retryErr != nil {
				log.Printf("Ingester: Retry lookup failed for %s: %v", alertKey, retryErr)
				return c.String(http.StatusInternalServerError, "Failed to persist")
			}
			if active != nil {
				return i.mergeIntoGroup(c, active, payload.Alerts)
			}
		}
		log.Printf("Ingester: Failed to create alert group: %v", err)
		return c.String(http.StatusInternalServerError, "Failed to persist")
	}
	metrics.AlertGroupsCreatedTotal.WithLabelValues(teamID, severity).Inc()
	// Deliberately here and not at the lookup above: counting there would also
	// count Alertmanager retries, the duplicate-key path that merges into an
	// existing group, and requests that go on to fail.
	if unknownTeam {
		metrics.UnknownTeamAlertGroupsTotal.WithLabelValues(teamID).Inc()
	}
	log.Printf("Ingester: Created alert group %s", ag.ID)

	return c.String(http.StatusOK, "Created")
}

// mergeIntoGroup merges incoming alerts into an existing alert group and updates state.
func (i *Ingester) mergeIntoGroup(c echo.Context, active *model.AlertGroup, incomingAlerts []model.Alert) error {
	existingFingerprints := make(map[string]model.AlertStatus)
	for _, a := range active.Alerts {
		existingFingerprints[a.Fingerprint] = a.Status
	}

	relevant := filterMergeableAlerts(incomingAlerts, existingFingerprints)
	if len(relevant) == 0 {
		// Nothing in this payload belongs to the group. Skip the alerts_data
		// rewrite and the Slack re-render it would trigger.
		return c.String(http.StatusOK, "Ignored Resolved")
	}

	updatedAlerts := mergeAlerts(active.Alerts, relevant)
	active.Alerts = updatedAlerts

	// Check statuses
	allResolved := true
	for _, a := range active.Alerts {
		if a.Status == model.AlertStatusFiring {
			allResolved = false
			break
		}
	}

	if allResolved {
		log.Printf("Ingester: All alerts resolved. Resolving alert group %s", active.ID)

		now := time.Now()
		timelineEvents := buildMergeTimelineEvents(active.ID, relevant, existingFingerprints, now)
		timelineEvents = append(timelineEvents, &model.TimelineEvent{
			ID:           uuid.New().String(),
			AlertGroupID: active.ID,
			Type:         model.TimelineEventResolved,
			Message:      "Alert group resolved: all alerts cleared",
			Actor:        "system",
			CreatedAt:    now.Add(time.Duration(len(timelineEvents)+1) * time.Microsecond),
		})

		eventPayload, err := model.BuildWebhookEventPayload(
			model.OutboxEventResolved, active, active.TeamNameSnapshot, "system", "", now,
		)
		if err != nil {
			log.Printf("Ingester: Failed to build resolve event payload: %v", err)
			return c.String(http.StatusInternalServerError, "Failed to persist")
		}
		outboxEvent := &model.OutboxEvent{
			EventType:    model.OutboxEventResolved,
			AlertGroupID: active.ID,
			TeamID:       active.TeamID,
			Actor:        "system",
			Payload:      eventPayload,
		}

		changed, err := i.store.ResolveAlertGroupWithAlertsAtomic(
			active.ID, active.Alerts, timelineEvents, outboxEvent,
		)
		if err != nil {
			log.Printf("Ingester: Failed to resolve group %s: %v", active.ID, err)
			return c.String(http.StatusInternalServerError, "Failed to persist")
		}
		if !changed {
			// AG was resolved concurrently (manual/slack resolve won the CAS race).
			// Best-effort sync of alerts_data so resolved AG reflects latest Alertmanager state.
			log.Printf("Ingester: Group %s already resolved (syncing alerts)", active.ID)
			if err := i.store.UpdateAlertGroupAlerts(active.ID, active.Alerts); err != nil {
				log.Printf("Ingester: Failed to sync alerts for %s: %v", active.ID, err)
			}
		}

		return c.String(http.StatusOK, "Resolved")
	}

	log.Printf("Ingester: Updating alert group %s (Partial State)", active.ID)
	if err := i.store.UpdateAlertGroupAlerts(active.ID, active.Alerts); err != nil {
		log.Printf("Ingester: Failed to update alerts for %s: %v", active.ID, err)
		return c.String(http.StatusInternalServerError, "Failed to update alerts")
	}

	// Don't regress status — ingester only updates alerts data and flags Slack update.
	// Status transitions are owned by engine (new→processing) and user actions (ack/resolve).

	// Flag Slack message for update (picked up by alertUpdateProcessingLoop)
	if err := i.store.RaiseSlackUpdate(active.ID); err != nil {
		log.Printf("Ingester: Failed to set slack update pending for %s: %v", active.ID, err)
	}

	// Timeline events only after successful store writes (avoids duplicates on AM retry)
	now := time.Now()
	for _, e := range buildMergeTimelineEvents(active.ID, relevant, existingFingerprints, now) {
		if err := i.store.AddTimelineEvent(e); err != nil {
			log.Printf("Ingester: Failed to add timeline event: %v", err)
		}
	}

	return c.String(http.StatusOK, "Updated")
}

// buildMergeTimelineEvents builds timeline events for new/changed alerts during a merge.
// Pure function — no side effects. Microsecond offsets from baseTime ensure deterministic ordering.
func buildMergeTimelineEvents(alertGroupID string, incomingAlerts []model.Alert, existingFingerprints map[string]model.AlertStatus, baseTime time.Time) []*model.TimelineEvent {
	var events []*model.TimelineEvent
	for _, a := range incomingAlerts {
		prevStatus, existed := existingFingerprints[a.Fingerprint]
		var eventType model.TimelineEventType
		var message string
		if !existed && a.Status == model.AlertStatusFiring {
			eventType = model.TimelineEventAlertAdded
			message = "Alert added: " + a.Labels["alertname"]
		} else if existed && prevStatus == model.AlertStatusFiring && a.Status == model.AlertStatusResolved {
			eventType = model.TimelineEventAlertResolved
			message = "Alert resolved: " + a.Labels["alertname"]
		} else if existed && prevStatus == model.AlertStatusResolved && a.Status == model.AlertStatusFiring {
			eventType = model.TimelineEventAlertAdded
			message = "Alert re-fired: " + a.Labels["alertname"]
		} else {
			continue
		}
		events = append(events, &model.TimelineEvent{
			ID:           uuid.New().String(),
			AlertGroupID: alertGroupID,
			Type:         eventType,
			Message:      message,
			Actor:        "system",
			Metadata:     map[string]string{"fingerprint": a.Fingerprint},
			CreatedAt:    baseTime.Add(time.Duration(len(events)+1) * time.Microsecond),
		})
	}
	return events
}

func mergeAlerts(existing, incoming []model.Alert) []model.Alert {
	state := make(map[string]model.Alert)
	for _, a := range existing {
		state[a.Fingerprint] = a
	}
	for _, a := range incoming {
		state[a.Fingerprint] = a // Overwrite with latest status
	}

	result := make([]model.Alert, 0, len(state))
	for _, a := range state {
		result = append(result, a)
	}
	return result
}

func (i *Ingester) generateTitle(p *AMPayload) string {
	if name, ok := p.CommonLabels["alertname"]; ok {
		return name
	}
	if len(p.Alerts) > 0 {
		return p.Alerts[0].Labels["alertname"]
	}
	return "Unknown Alert Group"
}

// filterMergeableAlerts drops incoming alerts that do not belong to the group.
// Alertmanager re-sends alerts it resolved earlier for the same aggregation
// group; those were closed together with a previous alert group carrying the
// same dedup key, so only a firing alert may introduce a fingerprint the group
// has never seen. Mirrors the firing-only filter the create path applies.
func filterMergeableAlerts(incoming []model.Alert, existingFingerprints map[string]model.AlertStatus) []model.Alert {
	var relevant []model.Alert
	for _, a := range incoming {
		if _, known := existingFingerprints[a.Fingerprint]; !known && a.Status != model.AlertStatusFiring {
			continue
		}
		relevant = append(relevant, a)
	}
	return relevant
}
