package ingester

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
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
	store           alertIntake
	cfg             *config.Config
	secretValidator WebhookSecretValidator
}

// alertIntake is the store as the ingester needs it: find the incident an alert
// belongs to, open one if there is none, and record what changed.
//
// Two of these are atomic on purpose. Creating a group writes the group, its
// timeline and the webhook event in one commit; recording changed alerts raises
// the "this message is out of date" mark in the same write as the alerts
// themselves, so no interruption can keep the alert and drop the mark.
type alertIntake interface {
	CreateAlertGroupAtomic(ag *model.AlertGroup, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) error

	// ApplyAlertmanagerUpdateAtomic applies a payload to the incident that is
	// open, and decides under its lock whether that is a merge or the end of
	// it. This layer does not decide: the read it would decide from is taken
	// before anything is held, and two webhooks for one alert would then act on
	// the same starting point and disagree.
	ApplyAlertmanagerUpdateAtomic(ctx context.Context, alertKey string,
		incoming []model.Alert, actor string) (alertgroup.MergeResult, error)

	GetTeamByID(id string) (*model.Team, error)
}

func NewIngester(s alertIntake, cfg *config.Config, secretValidator WebhookSecretValidator) *Ingester {
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

	// 3. Apply it to the incident that is open, if there is one. What that
	// means - a merge, the end of the incident, or nothing at all - is decided
	// under the lock on the row, not here.
	result, err := i.store.ApplyAlertmanagerUpdateAtomic(
		c.Request().Context(), alertKey, payload.Alerts, "system")
	if err != nil {
		log.Printf("Ingester: Failed to apply the payload for %s: %v", alertKey, err)
		return c.String(http.StatusInternalServerError, "Failed to persist")
	}
	switch result.Outcome {
	case alertgroup.MergeIgnored:
		return c.String(http.StatusOK, "Ignored Resolved")
	case alertgroup.MergeUnchanged:
		return c.String(http.StatusOK, "Unchanged")
	case alertgroup.MergeMerged:
		log.Printf("Ingester: Updated alert group %s", result.AlertGroupID)
		return c.String(http.StatusOK, "Updated")
	case alertgroup.MergeResolved:
		log.Printf("Ingester: All alerts cleared, resolved %s", result.AlertGroupID)
		return c.String(http.StatusOK, "Resolved")
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
			// Somebody else opened the incident between the answer above and
			// this insert. The partial unique index is what serialises that,
			// and the payload now belongs to their incident.
			log.Printf("Ingester: Duplicate key for %s, applying to the incident that won", alertKey)
			retry, retryErr := i.store.ApplyAlertmanagerUpdateAtomic(
				c.Request().Context(), alertKey, payload.Alerts, "system")
			if retryErr != nil {
				log.Printf("Ingester: Retry failed for %s: %v", alertKey, retryErr)
				return c.String(http.StatusInternalServerError, "Failed to persist")
			}
			// Anything but "there is no open incident" means it was applied. If
			// the winner has ALREADY resolved by now, the alert this payload
			// carries belongs to the next incident, and Alertmanager will send
			// it again - which is the same answer as any other lost race.
			if retry.Outcome != alertgroup.MergeNoActive {
				return c.String(http.StatusOK, "Updated")
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

func (i *Ingester) generateTitle(p *AMPayload) string {
	if name, ok := p.CommonLabels["alertname"]; ok {
		return name
	}
	if len(p.Alerts) > 0 {
		return p.Alerts[0].Labels["alertname"]
	}
	return "Unknown Alert Group"
}
