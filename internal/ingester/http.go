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
	Alerts       []AMAlert         `json:"alerts"`
	// TruncatedAlerts is how many alerts Alertmanager cut off the notification
	// (max_alerts). It does not say which.
	TruncatedAlerts uint64 `json:"truncatedAlerts"`
}

// AMAlert is an alert as Alertmanager sends it, and only that.
//
// It is not model.Alert, which also carries what this system observed about
// the alert - since when Alertmanager stopped reporting it. Reading a payload
// into the stored type would let whoever holds the webhook secret set that
// observation, and an alert would claim to have been silent since a moment
// nobody watched.
type AMAlert struct {
	Fingerprint  string            `json:"fingerprint"`
	Status       model.AlertStatus `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
}

func (a AMAlert) alert() model.Alert {
	return model.Alert{
		Fingerprint:  a.Fingerprint,
		Status:       a.Status,
		Labels:       a.Labels,
		Annotations:  a.Annotations,
		StartsAt:     a.StartsAt,
		EndsAt:       a.EndsAt,
		GeneratorURL: a.GeneratorURL,
	}
}

// alerts is the payload as the rest of the system holds alerts.
func (p AMPayload) alerts() []model.Alert {
	out := make([]model.Alert, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		out = append(out, a.alert())
	}
	return out
}

// notification says what the payload is worth as a statement about the group.
//
// It is a snapshot - the whole set of alerts Alertmanager still reports - only
// if it named the group, carried alerts, and had nothing cut off by
// max_alerts. Without a group key the alert key is one alert's fingerprint and
// the payload was never about a group; with alerts cut off, what is missing is
// missing for a reason nobody can read.
func (p AMPayload) notification(integrationID string, staleAfterSeconds int) alertgroup.Notification {
	return alertgroup.Notification{
		Alerts:            p.alerts(),
		Snapshot:          p.GroupKey != "" && p.TruncatedAlerts == 0 && len(p.Alerts) > 0,
		IntegrationID:     integrationID,
		StaleAfterSeconds: staleAfterSeconds,
	}
}

// WebhookSource names the integration a secret belongs to. It is the fast
// filter and the way to learn an id; whether the payload is taken is settled
// against the database, because this answer comes from a cache only the
// instance that handled a change has reloaded.
type WebhookSource interface {
	WebhookIntegrationID(secret string) (string, bool)
}

type Ingester struct {
	store  alertIntake
	cfg    *config.Config
	source WebhookSource
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
		notification alertgroup.Notification, actor string) (alertgroup.MergeResult, error)

	// VerifyIntake says whether the integration the secret belongs to still
	// exists, is enabled and still carries that secret, and what it declares
	// about silence. The cache cannot answer the first three: it is reloaded
	// by one instance at a time.
	VerifyIntake(ctx context.Context, integrationID, secret string) (int, bool, error)

	GetTeamByID(id string) (*model.Team, error)
}

func NewIngester(s alertIntake, cfg *config.Config, source WebhookSource) *Ingester {
	return &Ingester{store: s, cfg: cfg, source: source}
}

func (i *Ingester) RegisterRoutes(e *echo.Echo) {
	e.POST("/webhook/alertmanager", i.handleWebhook)
}

func (i *Ingester) handleWebhook(c echo.Context) error {
	// Authentication, in two steps. The cache says which integration the token
	// belongs to, and the database says whether that integration may still
	// send: an instance that has not reloaded the cache since the integration
	// was disabled or its secret rotated would otherwise go on taking its
	// payloads.
	token := c.QueryParam("token")

	integrationID, known := i.source.WebhookIntegrationID(token)
	if !known {
		log.Printf("Ingester: Unauthorized webhook request")
		return c.String(http.StatusUnauthorized, "Unauthorized")
	}
	staleAfterSeconds, allowed, err := i.store.VerifyIntake(c.Request().Context(), integrationID, token)
	if err != nil {
		// The database is the same database the payload would be stored in, so
		// there is nothing to be gained by turning Alertmanager away: it is
		// asked to come back.
		log.Printf("Ingester: Failed to verify integration %s: %v", integrationID, err)
		return c.String(http.StatusInternalServerError, "Failed to persist")
	}
	if !allowed {
		log.Printf("Ingester: Integration %s no longer accepts this token", integrationID)
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
	// Severity is one of three words from here on: routing, the firehose, the
	// UI and the metrics read it, and none of them has an answer for a fourth.
	// A missing label is info, and so is any other word - said once, below,
	// when the incident it opens is created, not on every repeat of the payload.
	rawSeverity := strings.ToLower(payload.CommonLabels["severity"])
	severity, knownSeverity := normalSeverity(rawSeverity)

	metrics.AlertsReceivedTotal.WithLabelValues(teamID, severity).Inc()
	firingInPayload := 0
	for _, a := range payload.Alerts {
		if a.Status == model.AlertStatusFiring {
			firingInPayload++
		}
	}
	log.Printf("Ingester: Group %s (Team: %s, Sev: %s, Alerts: %d firing, %d resolved, payload %s)",
		alertKey, teamID, severity, firingInPayload, len(payload.Alerts)-firingInPayload, payload.Status)
	if payload.TruncatedAlerts > 0 {
		// Outside the contract: the receiver has max_alerts set. What the
		// group still holds cannot be read from a list with an unknown part
		// missing, so this is said every time rather than once.
		log.Printf("Ingester: %s: Alertmanager cut %d alerts off the notification (max_alerts); "+
			"the alert group can resolve while one of them still fires - set max_alerts to 0",
			alertKey, payload.TruncatedAlerts)
		metrics.AlertmanagerTruncatedNotificationsTotal.Inc()
	}

	// 3. Apply it to the incident that is open, if there is one. What that
	// means - a merge, the end of the incident, or nothing at all - is decided
	// under the lock on the row, not here.
	result, err := i.store.ApplyAlertmanagerUpdateAtomic(
		c.Request().Context(), alertKey, payload.notification(integrationID, staleAfterSeconds), "system")
	if err != nil {
		log.Printf("Ingester: Failed to apply the payload for %s: %v", alertKey, err)
		return c.String(http.StatusInternalServerError, "Failed to persist")
	}
	// Every outcome leaves a line, the quiet ones included: a payload that
	// changed nothing is the one a person asks about afterwards.
	switch result.Outcome {
	case alertgroup.MergeIgnored:
		log.Printf("Ingester: %s: nothing in the payload belongs to the open incident %s, ignored",
			alertKey, result.AlertGroupID)
		return c.String(http.StatusOK, "Ignored Resolved")
	case alertgroup.MergeUnchanged:
		log.Printf("Ingester: %s: the open incident %s already says this, unchanged",
			alertKey, result.AlertGroupID)
		return c.String(http.StatusOK, "Unchanged")
	case alertgroup.MergeMerged:
		log.Printf("Ingester: Updated alert group %s", result.AlertGroupID)
		return c.String(http.StatusOK, "Updated")
	case alertgroup.MergeResolved:
		log.Printf("Ingester: All alerts cleared, resolved %s", result.AlertGroupID)
		return c.String(http.StatusOK, "Resolved")
	}

	// 4. Create New Alert Group
	// Filter to firing alerts only - resolved alerts shouldn't appear in a new group.
	var firingAlerts []model.Alert
	for _, a := range payload.alerts() {
		if a.Status == model.AlertStatusFiring {
			firingAlerts = append(firingAlerts, a)
		}
	}
	if len(firingAlerts) == 0 {
		log.Printf("Ingester: %s: no open incident and nothing firing in the payload, ignored", alertKey)
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
	ag.IntakeIntegrationID = integrationID
	if staleAfterSeconds > 0 {
		seconds := staleAfterSeconds
		ag.StaleAfterSeconds = &seconds
	}

	// Build timeline events for atomic insert - µs offsets ensure deterministic ordering
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
	// Not-found is normal (unknown team label from Alertmanager) - use teamID as snapshot.
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
				c.Request().Context(), alertKey, payload.notification(integrationID, staleAfterSeconds), "system")
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
	if !knownSeverity {
		log.Printf("Ingester: severity %q of %s is none of critical, warning, info; alert group %s counts as info",
			rawSeverity, alertKey, ag.ID)
	}
	// Deliberately here and not at the lookup above: counting there would also
	// count Alertmanager retries, the duplicate-key path that merges into an
	// existing group, and requests that go on to fail.
	if unknownTeam {
		metrics.UnknownTeamAlertGroupsTotal.WithLabelValues(teamID).Inc()
	}
	log.Printf("Ingester: Created alert group %s", ag.ID)

	return c.String(http.StatusOK, "Created")
}

// normalSeverity folds the label into the three severities the rest of the
// system knows: an empty label is info, and so is a word that is none of
// them. The second answer says whether the label was one of the three.
func normalSeverity(label string) (severity string, known bool) {
	switch label {
	case "critical", "warning", "info":
		return label, true
	case "":
		return "info", true
	default:
		return "info", false
	}
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
