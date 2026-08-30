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

	// ScheduleOnCallNotificationsTotal counts announcements actually admitted,
	// not on-call changes detected. The two differ whenever more than one
	// instance is running: both observe the same handoff, the occurrence key
	// lets exactly one admission create the work, and counting detections would
	// report two announcements where one was made.
	ScheduleOnCallNotificationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "schedule_oncall_notifications_total",
		Help: "On-call announcements admitted, by kind of transition.",
	}, []string{"kind"})

	// HandoffRecipientsSkippedTotal counts people who came on duty and were not
	// told, by why.
	//
	// Nothing else shows this. The admission counter says the announcement was
	// accepted and schedule_oncall_notifications_total says it was made; both
	// are true of an announcement that reached one of the two people it was
	// about. This is the only place the other one appears.
	//
	// Counted once per person per occurrence, on the admission that CREATED it
	// and on no other: two instances detecting one shift change both see the
	// same unreachable person, and both commit - one as created, one as
	// existing. Counted on both, one person missed would read as two.
	HandoffRecipientsSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "handoff_recipients_skipped_total",
		Help: "People on an incoming shift who were not announced to, by reason.",
	}, []string{"reason"})

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
	prometheus.MustRegister(HandoffRecipientsSkippedTotal)
	prometheus.MustRegister(ScheduleOnCallProjectionFailuresTotal)
	prometheus.MustRegister(ScheduleOnCallProjectionDuration)
}
