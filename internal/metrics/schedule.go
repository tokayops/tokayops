package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Tier 6 - Schedule configuration commands.
//
// These are registered here, next to every other metric, but they are reached
// through a narrow interface that scheduleconfig declares. A direct import the
// other way would close the loop scheduleconfig -> metrics -> store ->
// scheduleconfig, since this package collects from the store and the store
// implements the scheduleconfig contracts. Every method below takes standard
// library types only, so nothing here has to import scheduleconfig either.
var (
	ScheduleRevisionCreatedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "schedule_revision_created_total",
		Help: "Schedule revisions committed, by what triggered them.",
	}, []string{"trigger"})

	ScheduleTransitionNoopTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schedule_transition_noop_total",
		Help: "Saves that found nothing to change and wrote nothing.",
	})

	ScheduleTransitionConflictTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schedule_transition_conflict_total",
		Help: "Saves rejected because the caller held a stale config version.",
	})

	ScheduleTransitionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "schedule_transition_duration_seconds",
		Help:    "Duration of one schedule configuration command, lock wait included.",
		Buckets: prometheus.DefBuckets,
	})

	// A rising count here is data corruption, not load: a stored snapshot no
	// longer decodes, and the affected schedule cannot be rendered at all.
	ScheduleSnapshotDecodeErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schedule_snapshot_decode_errors_total",
		Help: "Stored schedule snapshots that could not be decoded.",
	})

	// This one should be flat at zero. Anything else means a committed
	// revision would have produced a rotation the planner did not intend, and
	// the transaction was rolled back to stop it.
	SchedulePhaseGuardViolationsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schedule_phase_guard_violations_total",
		Help: "Commit-time post-condition failures in the schedule transition guard.",
	})

	// ScheduleOnCallNotificationsTotal counts notification jobs actually
	// created, not on-call changes detected. The two differ whenever more than
	// one instance is running: both observe the same handoff, the dedup key
	// lets exactly one job through, and counting detections would report two
	// notifications where one was sent.
	ScheduleOnCallNotificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "schedule_oncall_notifications_total",
		Help: "On-call notification jobs created, by kind of transition.",
	}, []string{"kind"})

	// ScheduleOnCallProjectionFailuresTotal counts schedules the runtime could
	// not project. It is deliberately separate from
	// schedule_snapshot_decode_errors_total: that one counts a save being
	// refused, this one counts a schedule whose duty roster can no longer be
	// read at all. The alerts they deserve are not the same.
	//
	// The consumer label is what makes a rate readable. Two consumers observe
	// the same schedules on different intervals, so without it one damaged
	// schedule is indistinguishable from two, and an alert can only say
	// "nonzero". With it, a rate over one consumer is proportional to the
	// number of damaged schedules, because that consumer's tick is periodic.
	// The exact count is not measured: nothing an operator does depends on it,
	// and the schedule ids are already in the log line next to this counter.
	ScheduleOnCallProjectionFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "schedule_oncall_projection_failures_total",
		Help: "Schedules that could not be projected by a runtime consumer, by consumer and reason.",
	}, []string{"consumer", "reason"})

	// ScheduleOnCallProjectionDuration is how long one bulk projection takes,
	// per consumer.
	//
	// It exists so the threshold for moving to batch reads is observable in
	// production rather than only in a benchmark: the plan names "a tick longer
	// than 5 seconds", and DefBuckets has a boundary at exactly 5, so a p99 of
	// this histogram answers that question without interpolating.
	//
	// It is observed by each consumer rather than inside the renderer, which
	// does not know who called it - the same reason the consumer label lives
	// here. Its rate doubles as the liveness signal for a consumer: a tick that
	// stopped happening shows up as no observations at all.
	ScheduleOnCallProjectionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "schedule_oncall_projection_duration_seconds",
		Help:    "Duration of one bulk on-call projection, by consumer.",
		Buckets: prometheus.DefBuckets,
	}, []string{"consumer"})
)

// Consumers of the bulk on-call projection, as a closed set: a label each
// caller spells out by hand drifts, and two spellings split one series in two.
const (
	ConsumerHandoffNotifier = "handoff_notifier"
	ConsumerUsergroupSyncer = "usergroup_syncer"
)

// ScheduleMetrics is the Prometheus implementation of the metrics sink the
// schedule configuration service and the API error mapper report through.
type ScheduleMetrics struct{}

func (ScheduleMetrics) RevisionCreated(trigger string) {
	ScheduleRevisionCreatedTotal.WithLabelValues(trigger).Inc()
}

func (ScheduleMetrics) TransitionNoop() { ScheduleTransitionNoopTotal.Inc() }

func (ScheduleMetrics) TransitionConflict() { ScheduleTransitionConflictTotal.Inc() }

func (ScheduleMetrics) TransitionDuration(d time.Duration) {
	ScheduleTransitionDuration.Observe(d.Seconds())
}

func (ScheduleMetrics) SnapshotDecodeError() { ScheduleSnapshotDecodeErrorsTotal.Inc() }

func (ScheduleMetrics) GuardViolation() { SchedulePhaseGuardViolationsTotal.Inc() }

func init() {
	prometheus.MustRegister(ScheduleRevisionCreatedTotal)
	prometheus.MustRegister(ScheduleTransitionNoopTotal)
	prometheus.MustRegister(ScheduleTransitionConflictTotal)
	prometheus.MustRegister(ScheduleTransitionDuration)
	prometheus.MustRegister(ScheduleSnapshotDecodeErrorsTotal)
	prometheus.MustRegister(SchedulePhaseGuardViolationsTotal)
	prometheus.MustRegister(ScheduleOnCallNotificationsTotal)
	prometheus.MustRegister(ScheduleOnCallProjectionFailuresTotal)
	prometheus.MustRegister(ScheduleOnCallProjectionDuration)
}
