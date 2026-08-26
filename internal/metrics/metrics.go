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

	// EngineEscalationSourceChangedTotal counts plans refused because the alert
	// moved while they were being built - an alert joined the group, or a user
	// acknowledged it, between the read and the admission. The group is not
	// claimed and the next tick plans it again from what is now there.
	//
	// A few of these under a storm are the design working. A rate that stays
	// high is a group changing faster than a plan can be built, which is a page
	// that never goes out.
	EngineEscalationSourceChangedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "engine_escalation_source_changed_total",
		Help: "Escalation plans refused because the alert group changed between being read and being admitted. One alert group increments this on every retry, so this counts refusals, not distinct alert groups.",
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

// Tier 8 - outbound delivery
//
// What an operator needs from this domain is two questions answered: is
// anything owed that should already have gone out, and did anything end without
// being delivered. The counters below answer the second; the gauges in the
// collector answer the first, because "how much is late" is a fact about rows
// and only the database can be asked it.
//
// Deliberately absent: the age of the oldest pending commitment. A step of an
// escalation that is scheduled for ten minutes' time, and a retry waiting out
// its backoff, are planned work - counted as a backlog they would wake somebody
// for a system that is doing exactly what it was told.
var (
	// OutboundAttemptsTotal counts calls that were STARTED, by how they ended.
	//
	// error_class is empty for an accepted call and carries the channel's own
	// classification otherwise ("rate_limited", "channel_not_found", ...). It
	// is a closed vocabulary per provider, not a message: an error string in a
	// label is a cardinality bomb.
	OutboundAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_attempts_total",
		Help: "Outbound delivery attempts that reached the provider, by outcome.",
	}, []string{"family", "provider", "operation", "outcome", "error_class"})

	// OutboundIntentsTerminalTotal counts commitments that ended, by how.
	//
	// The terminal states are succeeded, permanent_failed, expired and
	// canceled. Everything except "succeeded" is a commitment that ended
	// WITHOUT A PROVEN SUCCESS, which is not the same as a message that was
	// never sent: a call whose fate is unknown can be withdrawn by an
	// acknowledgement or run out of time, and the effect may well have
	// happened. Whether it did is a question for the commitment's journal -
	// this counter says only that nothing proved it.
	//
	// The alert is written against those three for that reason: each one is an
	// escalation that stopped without anybody being able to say it worked.
	//
	// It is incremented after the transaction that ended the commitment
	// commits, never inside it: a rollback would otherwise report an ending
	// that never occurred.
	//
	// Best effort, like every counter in a process. A commit whose reply is
	// lost, or a process that dies between the two, loses the observation. An
	// alert that may not miss a permanent_failed has to be built over the
	// durable state - the commitment rows and their journal - and this counter
	// is for rate and for noticing, not for accounting.
	OutboundIntentsTerminalTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_intents_terminal_total",
		Help: "Outbound commitments that reached a terminal state, by state. Anything other than succeeded ended without a proven success, which is not the same as never sent - see the commitment journal.",
	}, []string{"family", "status"})

	// OutboundContractViolationsTotal counts what should not be able to happen:
	// a finalisation carrying a lease token that is not the one on the row, two
	// contradicting accounts of one call, an invariant the store refuses, a
	// channel reporting success with nothing to identify the message by.
	//
	// None of these is a delivery problem. Every one of them is a bug in this
	// system or in an assumption about a provider, and a nonzero rate is a
	// thing to go and read the log about.
	OutboundContractViolationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_contract_violations_total",
		Help: "Refusals that indicate a broken contract rather than a failed delivery.",
	}, []string{"op", "kind"})

	// OutboundAdmissionsTotal counts what happened to each escalation offered
	// for admission.
	//
	// "no_targets" is not one of the store's outcomes - it is an admission that
	// was accepted and promised nothing, and it is broken out because it is the
	// one success that means nobody was paged.
	OutboundAdmissionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_admissions_total",
		Help: "Escalation admissions by outcome. no_targets is an accepted admission that promised nothing.",
	}, []string{"outcome"})

	// OutboundDesiredRevisionsTotal counts proposals to move what an alert's
	// messages have to show, by what raised it and what came of it.
	//
	// reason is ack | resolve | merge - the transition that raised it. outcome
	// is applied | unchanged | stale_after_final | no_snapshot, and the two are
	// different questions: the first says who asked, the second whether a
	// revision now exists. Only "applied" means work was created.
	//
	// "unchanged" is the healthy majority for a busy alert - a payload that
	// repeats the same alerts changes nothing a message shows - and it is
	// counted rather than dropped so that a build that stopped noticing real
	// changes looks different from a quiet one.
	//
	// Incremented after the transaction commits, like every other counter here.
	OutboundDesiredRevisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_desired_revisions_total",
		Help: "Proposals to move what an alert group's messages have to show, by reason and outcome. Only applied created work.",
	}, []string{"reason", "outcome"})

	// OutboundAdmissionLatencySeconds measures the promise: from the commit
	// that admitted an escalation to the start of the FIRST attempt of a
	// commitment that was due immediately.
	//
	// Only immediately-due commitments are observed. A step with a ten-minute
	// delay would otherwise report ten minutes of latency for working exactly
	// as configured. Both ends of the interval are the database's own clock,
	// so nothing here depends on two processes agreeing about the time.
	OutboundAdmissionLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "outbound_admission_latency_seconds",
		Help:    "Seconds from admitting an escalation to starting the first attempt of an immediately due commitment.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"family"})
)

// Tier 8 - storage contract
//
// TokayOps trusts the physical integrity of PostgreSQL and assumes production
// rows are written only by this application's typed writers. Where that
// assumption is load bearing - the fields an obligation is created from - a row
// that cannot be decoded is refused rather than read as an empty one, and
// refusing it can stop the admission scan until an operator fixes the row.
//
// That is a deliberate risk, and a risk taken deliberately has to be visible.
// This counter is the visible half: any nonzero increment means a durable row
// no longer parses, and the log beside it names the group.
var (
	StorageContractFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "storage_contract_failures_total",
		Help: "Durable rows that could not be decoded into the type they are stored as. Any increment is a storage-contract violation, not a transient failure.",
	}, []string{"field"})
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
	prometheus.MustRegister(EngineEscalationSourceChangedTotal)

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

	prometheus.MustRegister(TelegramUserLinkedTotal)
	prometheus.MustRegister(TelegramInteractionTotal)
	prometheus.MustRegister(TelegramUnlinkedUserTotal)

	// Tier 7
	prometheus.MustRegister(OutboxEventsClaimedTotal)
	prometheus.MustRegister(OutboxEventsCompletedTotal)
	prometheus.MustRegister(OutboxDeliveryAttemptsTotal)
	prometheus.MustRegister(OutboxDeliveryDuration)
	prometheus.MustRegister(OutboxDeliveryBlockedTotal)

	// Tier 8
	prometheus.MustRegister(OutboundAttemptsTotal)
	prometheus.MustRegister(OutboundIntentsTerminalTotal)
	prometheus.MustRegister(OutboundContractViolationsTotal)
	prometheus.MustRegister(OutboundAdmissionsTotal)
	prometheus.MustRegister(OutboundDesiredRevisionsTotal)
	prometheus.MustRegister(OutboundAdmissionLatencySeconds)
	prometheus.MustRegister(StorageContractFailuresTotal)
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
