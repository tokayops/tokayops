package metrics

import "github.com/prometheus/client_golang/prometheus"

// Tier 6 - the schedule runtime.
//
// Command-side counters used to live here too, reached through an interface
// scheduleconfig declared. They are gone: duration and conflicts are already
// visible in the HTTP metrics every command arrives through, and a corrupt
// snapshot is already visible to the runtime as a projection failure. What
// remains is what happens WITHOUT a request - the notifier and the syncer -
// which no HTTP metric can see.
var (

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
	// not project - a schedule whose duty roster can no longer be read at all.
	// A corrupt stored snapshot arrives here as reason="snapshot_decode": the
	// command side has no counter of its own, because a save that hits one
	// answers over HTTP, where the failure is already recorded.
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

func init() {
	prometheus.MustRegister(ScheduleOnCallNotificationsTotal)
	prometheus.MustRegister(ScheduleOnCallProjectionFailuresTotal)
	prometheus.MustRegister(ScheduleOnCallProjectionDuration)
}
