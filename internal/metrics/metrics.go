package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tokayops/tokayops/internal/model"
)

// Tier 1 — HTTP
var (
	HTTPRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})
)

// Tier 2 — Alert ingestion
var (
	AlertsReceivedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alerts_received_total",
		Help: "Total number of alerts received from webhooks.",
	}, []string{"team", "severity"})

	AlertGroupsCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alert_groups_created_total",
		Help: "Total number of new alert groups created.",
	}, []string{"team", "severity"})

	AlertGroupsResolvedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "alert_groups_resolved_total",
		Help: "Total number of alert groups auto-resolved (all alerts cleared).",
	}, []string{"team"})

	// Counted only for alert groups that were actually created, so retries and
	// merges into an existing group do not inflate it. The label is the raw
	// team label off the alert, which is the string an operator has to act on:
	// either onboard that team or fix the label.
	UnknownTeamAlertGroupsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "unknown_team_alert_groups_total",
		Help: "Alert groups created with a team label that matches no team in TokayOps.",
	}, []string{"team"})
)

// Tier 3 — Dispatcher / notification delivery
var (
	JobStepsProcessedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "job_steps_processed_total",
		Help: "Total number of job steps processed.",
	}, []string{"type", "status"})

	NotificationSentTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_sent_total",
		Help: "Total number of notifications sent successfully.",
	}, []string{"channel"})

	NotificationErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_errors_total",
		Help: "Total number of notification delivery errors.",
	}, []string{"channel", "reason"})
)

// Tier 4 — Engine
var (
	EngineRunsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "engine_runs_total",
		Help: "Total number of engine ticks.",
	})

	EngineAlertGroupsPickedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "engine_alert_groups_picked_total",
		Help: "Total number of alert groups picked up by the engine.",
	})

	// EngineEscalationBuildDeferralsTotal counts escalation builds abandoned
	// because the on-call recipients could not be resolved. The alert group
	// stays "new" and every following tick tries again.
	//
	// It counts deferrals, not alert groups, and the Help says so because the
	// difference is a factor of the tick rate: one group waiting through a
	// minute of trouble increments this sixty times. A rate over it answers
	// "did this happen recently", never "how many are waiting now" - that
	// question belongs to a query over alert_groups, not to a counter.
	EngineEscalationBuildDeferralsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "engine_escalation_build_deferrals_total",
		Help: "Escalation builds deferred because the on-call recipients could not be resolved. One alert group increments this on every retry, so this counts deferrals, not distinct alert groups.",
	})
)

// Tier 5 — Handoff notifier
var (
	HandoffWarmupNotComplete = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "handoff_warmup_not_complete_total",
		Help: "Total number of HandoffNotifier warm-up attempts that failed (incomplete cache).",
	})
)

// MTTR — resolution duration
var (
	AlertGroupResolutionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alert_group_resolution_duration_seconds",
		Help:    "Time from alert group creation to resolution, in seconds.",
		Buckets: []float64{60, 300, 600, 1800, 3600, 7200, 14400, 43200, 86400},
	}, []string{"team", "severity", "oncall_user"})
)

// MTTA — ack duration
var (
	AlertGroupAckDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alert_group_ack_duration_seconds",
		Help:    "Time from alert group creation to acknowledgment, in seconds.",
		Buckets: []float64{30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"team", "severity"})
)

// Tier 6 — Slack interactivity
var (
	SlackUserLinkedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_user_linked_total",
		Help: "Slack users auto-linked to TokayOps users.",
	}, []string{"method"})

	SlackInteractionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_interaction_total",
		Help: "Slack interactive button clicks.",
	}, []string{"action", "result"})

	SlackUnlinkedUserTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "slack_unlinked_user_total",
		Help: "Slack interaction attempts from users without a linked TokayOps account.",
	})

	SlackEscalationCancelErrorTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "slack_escalation_cancel_error_total",
		Help: "Errors when cancelling escalation jobs from Slack interactive handler.",
	})
)

// Tier 6b — Telegram interactivity (Epic 8 Sprint 3). Mirrors the Slack counters;
// no email_match label (Telegram has no email auto-link).
var (
	TelegramUserLinkedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "telegram_user_linked_total",
		Help: "Telegram users linked to Tokay users via deep link.",
	})

	TelegramInteractionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "telegram_interaction_total",
		Help: "Telegram callback button clicks.",
	}, []string{"action", "result"})

	TelegramUnlinkedUserTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "telegram_unlinked_user_total",
		Help: "Telegram interaction attempts from users without a linked Tokay account.",
	})
)

// Tier 7 — Outbox Delivery
var (
	OutboxEventsClaimedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbox_events_claimed_total",
		Help: "Total number of outbox events claimed by the worker.",
	})

	OutboxEventsCompletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_events_completed_total",
		Help: "Total number of outbox events completed.",
	}, []string{"result"})

	OutboxDeliveryAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_delivery_attempts_total",
		Help: "Total number of outbox delivery attempts.",
	}, []string{"status"})

	OutboxDeliveryDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "outbox_delivery_duration_seconds",
		Help:    "HTTP call duration for outbox webhook deliveries.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	})

	OutboxDeliveryBlockedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbox_delivery_blocked_total",
		Help: "Total number of outbox deliveries blocked by SSRF IP policy.",
	})
)

func init() {
	// Tier 1
	prometheus.MustRegister(HTTPRequestsTotal)
	prometheus.MustRegister(HTTPRequestDuration)

	// Tier 2
	prometheus.MustRegister(AlertsReceivedTotal)
	prometheus.MustRegister(AlertGroupsCreatedTotal)
	prometheus.MustRegister(UnknownTeamAlertGroupsTotal)
	prometheus.MustRegister(AlertGroupsResolvedTotal)

	// Tier 3
	prometheus.MustRegister(JobStepsProcessedTotal)
	prometheus.MustRegister(NotificationSentTotal)
	prometheus.MustRegister(NotificationErrorsTotal)

	// Tier 4
	prometheus.MustRegister(EngineRunsTotal)
	prometheus.MustRegister(EngineAlertGroupsPickedTotal)
	prometheus.MustRegister(EngineEscalationBuildDeferralsTotal)

	// Tier 5
	prometheus.MustRegister(HandoffWarmupNotComplete)

	// MTTR
	prometheus.MustRegister(AlertGroupResolutionDuration)

	// MTTA
	prometheus.MustRegister(AlertGroupAckDuration)

	// Tier 6
	prometheus.MustRegister(SlackUserLinkedTotal)
	prometheus.MustRegister(SlackInteractionTotal)
	prometheus.MustRegister(SlackUnlinkedUserTotal)
	prometheus.MustRegister(SlackEscalationCancelErrorTotal)

	prometheus.MustRegister(TelegramUserLinkedTotal)
	prometheus.MustRegister(TelegramInteractionTotal)
	prometheus.MustRegister(TelegramUnlinkedUserTotal)

	// Tier 7
	prometheus.MustRegister(OutboxEventsClaimedTotal)
	prometheus.MustRegister(OutboxEventsCompletedTotal)
	prometheus.MustRegister(OutboxDeliveryAttemptsTotal)
	prometheus.MustRegister(OutboxDeliveryDuration)
	prometheus.MustRegister(OutboxDeliveryBlockedTotal)
}

// ObserveAck records the ack duration (MTTA) for an alert group.
func ObserveAck(ag *model.AlertGroup) {
	duration := time.Since(ag.CreatedAt).Seconds()
	AlertGroupAckDuration.WithLabelValues(ag.TeamID, ag.Severity).Observe(duration)
}

// ObserveResolution records the resolution duration for an alert group.
func ObserveResolution(ag *model.AlertGroup) {
	if ag.ResolvedAt == nil {
		return
	}
	oncallUser := "none"
	if ag.OnCallSnapshot != nil && len(ag.OnCallSnapshot.L1Users) > 0 {
		oncallUser = ag.OnCallSnapshot.L1Users[0].Name
	}
	duration := ag.ResolvedAt.Sub(ag.CreatedAt).Seconds()
	AlertGroupResolutionDuration.WithLabelValues(ag.TeamID, ag.Severity, oncallUser).Observe(duration)
}
