package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tokayops/tokayops/internal/model"
)

// Tier 1 - HTTP
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

// Tier 2 - Alert ingestion
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

// Tier 4 - Engine
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

// Tier 5 - Handoff notifier
var (
	HandoffWarmupNotComplete = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "handoff_warmup_not_complete_total",
		Help: "Total number of HandoffNotifier warm-up attempts that failed (incomplete cache).",
	})
)

// MTTR - resolution duration
var (
	AlertGroupResolutionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alert_group_resolution_duration_seconds",
		Help:    "Time from alert group creation to resolution, in seconds.",
		Buckets: []float64{60, 300, 600, 1800, 3600, 7200, 14400, 43200, 86400},
	}, []string{"team", "severity", "oncall_user"})
)

// MTTA - ack duration
var (
	AlertGroupAckDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alert_group_ack_duration_seconds",
		Help:    "Time from alert group creation to acknowledgment, in seconds.",
		Buckets: []float64{30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"team", "severity"})
)

// Tier 6 - Slack interactivity
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

// Tier 6b - Telegram interactivity. Mirrors the Slack counters;
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

	// OutboundAdmissionsTotal counts what happened to each admission offered to
	// the domain.
	//
	// "no_targets" is not one of the store's outcomes - it is an admission that
	// was accepted and promised nothing, and it is broken out because it is the
	// one success that means nobody was told.
	//
	// The family is what makes the series readable at all. Two partitions offer
	// work here and their answers mean different things: a paging admission
	// that promised nobody is an alert nobody will see, and a handover one is a
	// shift change nobody will hear about. Counted together they are one number
	// that no alert can be written against, because neither rate can be
	// separated from the other's noise. It is a closed set - derived from the
	// kind of claim, never taken from a caller - so a producer cannot invent a
	// partition by mislabelling.
	OutboundAdmissionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_admissions_total",
		Help: "Admissions by execution family and outcome. no_targets is an accepted admission that promised nothing.",
	}, []string{"family", "outcome"})

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
	// that admitted work to the start of the FIRST attempt of a commitment that
	// was due immediately.
	//
	// Only immediately-due commitments are observed. A step with a ten-minute
	// delay would otherwise report ten minutes of latency for working exactly
	// as configured. Both ends of the interval are the database's own clock,
	// so nothing here depends on two processes agreeing about the time.
	//
	// The boundaries past 60 are what makes the second family measurable at
	// all. Paging lives in the seconds and its nine original boundaries are
	// kept exactly, so its thresholds are measured as they always were; a
	// hundred announcements draining through a small pool live at 240 to 300,
	// and with 60 as the last finite bucket every healthy one of those would
	// land in +Inf. histogram_quantile would then answer 60 for all of them -
	// the SLO would read green precisely when it was broken.
	OutboundAdmissionLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "outbound_admission_latency_seconds",
		Help: "Seconds from admitting work to starting the first attempt of an immediately due commitment.",
		Buckets: []float64{
			0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
			120, 180, 240, 300, 360, 600, 900,
		},
	}, []string{"family"})

	// OutboundWorkerTicksTotal counts passes of each family's worker, empty
	// ones included.
	//
	// It is the liveness signal, and the only one: a worker whose goroutine
	// died, or that hangs inside housekeeping on a lock, leaves the queue
	// gauges exactly as they were - and an empty queue looks the same whether
	// somebody is watching it or not. The series is initialised to zero for
	// every family at start-up (see outbound.init), because a counter that only
	// appears on its first increment gives a rule over rate() nothing to be
	// zero ABOUT when the worker never started.
	OutboundWorkerTicksTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_worker_ticks_total",
		Help: "Passes of the delivery worker of each family, including passes that found nothing to do. A stopped rate is a stopped worker.",
	}, []string{"family"})

	// OutboundFanOutTicksTotal is the same signal for the webhook family's
	// producer, which runs in a loop of its own.
	OutboundFanOutTicksTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "outbound_fanout_ticks_total",
		Help: "Passes of the webhook fan-out loop, including passes that found no event. A stopped rate is a stopped fan-out.",
	})

	// OutboundRetentionWindowDays is the configured window; zero when the
	// sweep is off. The rules read it to know whether to expect a success.
	OutboundRetentionWindowDays = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "outbound_retention_window_days",
		Help: "The delivery history retention window in days, as configured; 0 when retention is off.",
	})

	// OutboundRetentionLastSuccess is when a pass last held the lock and
	// reached its commit - including a pass that found nothing to remove. A
	// vector with no labels so that the series does not exist until the first
	// success: "enabled and never succeeded" is a state the rules alert on,
	// and a zero would read as a success in 1970.
	OutboundRetentionLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "outbound_retention_last_success_timestamp_seconds",
		Help: "Unix time of the last retention pass that held the lock and committed. Absent until the first; a busy pass or a failed one does not move it.",
	}, []string{})

	// OutboundRetentionDeletedTotal counts what retention removed, by table.
	OutboundRetentionDeletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_retention_deleted_total",
		Help: "Rows the delivery history retention removed, by table.",
	}, []string{"table"})

	// OutboundLeasesExpiredTotal counts attempts that recovery closed because
	// the worker holding them never came back, by where the commitment went.
	//
	// One of these is a process that died with a call in flight, and it is
	// already visible as a restart. Five in a quarter of an hour is either a
	// provider answering slower than the lease or a fleet being restarted in a
	// circle, and that is what the alert is written against. "to" is the
	// status the commitment was moved to: pending under the retry policy,
	// manual_review under manual review, and the rest as the machine decides.
	// Initialised to zero for every family and target at start-up, like the
	// tick counters.
	OutboundLeasesExpiredTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "outbound_leases_expired_total",
		Help: "Attempts closed by recovery because their worker's lease ran out, by the status the commitment went to.",
	}, []string{"family", "to"})
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

	// Tier 8
	prometheus.MustRegister(OutboundAttemptsTotal)
	prometheus.MustRegister(OutboundIntentsTerminalTotal)
	prometheus.MustRegister(OutboundContractViolationsTotal)
	prometheus.MustRegister(OutboundAdmissionsTotal)
	prometheus.MustRegister(OutboundDesiredRevisionsTotal)
	prometheus.MustRegister(OutboundAdmissionLatencySeconds)
	prometheus.MustRegister(OutboundWorkerTicksTotal)
	prometheus.MustRegister(OutboundFanOutTicksTotal)
	prometheus.MustRegister(OutboundLeasesExpiredTotal)
	prometheus.MustRegister(OutboundRetentionWindowDays)
	prometheus.MustRegister(OutboundRetentionLastSuccess)
	prometheus.MustRegister(OutboundRetentionDeletedTotal)
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
