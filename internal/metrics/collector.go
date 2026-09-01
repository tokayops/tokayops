package metrics

import (
	"log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tokayops/tokayops/internal/model"
)

var (
	activeAlertGroupsDesc = prometheus.NewDesc(
		"active_alert_groups",
		"Current count of non-resolved/closed alert groups.",
		[]string{"team", "severity"}, nil,
	)
	teamsWithoutOnCallDesc = prometheus.NewDesc(
		"teams_without_oncall",
		"Count of teams with no schedule or empty L1 rotation.",
		nil, nil,
	)
	teamsWithPermanentOnCallDesc = prometheus.NewDesc(
		"teams_with_permanent_oncall",
		"Count of teams with only 1 user in L1 rotation.",
		nil, nil,
	)
	teamsWithoutPolicyDesc = prometheus.NewDesc(
		"teams_without_escalation_policy",
		"Count of teams with no default escalation policy.",
		nil, nil,
	)
	alertGroupsByStatusDesc = prometheus.NewDesc(
		"alert_groups_by_status",
		"Current count of alert groups by status.",
		[]string{"team", "severity", "status"}, nil,
	)
	outboxEventsByStatusDesc = prometheus.NewDesc(
		"outbox_events_by_status",
		"Current count of outbox events by status.",
		[]string{"status"}, nil,
	)
	outboundIntentsByStatusDesc = prometheus.NewDesc(
		"outbound_intents_by_status",
		"Current count of outbound delivery commitments by status.",
		[]string{"family", "status"}, nil,
	)
	outboundQueueLatenessDesc = prometheus.NewDesc(
		"outbound_queue_lateness_seconds",
		"Age of the oldest outbound commitment that is due now and has not started. "+
			"Work scheduled for later is not late; leased work is included, because a worker that hung is what this must not hide.",
		[]string{"family"}, nil,
	)
	outboundCardsBehindDesc = prometheus.NewDesc(
		"outbound_cards_behind",
		"Editable messages showing something older than the alert they are about. "+
			"queued will be caught up by a worker, stuck needs a person, abandoned is a person having decided not to - which is a normal end, not a fault.",
		[]string{"state"}, nil,
	)
	outboundCardStalenessDesc = prometheus.NewDesc(
		"outbound_card_staleness_seconds",
		"How long the oldest message still due to be caught up has been behind its alert. "+
			"Abandoned messages are excluded: nobody is going to catch those up, and counting them would leave this high forever.",
		nil, nil,
	)
)

// BusinessCollector queries the store on each Prometheus scrape.
type BusinessCollector struct {
	store metricsSource
}

func (c *BusinessCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- activeAlertGroupsDesc
	ch <- teamsWithoutOnCallDesc
	ch <- teamsWithPermanentOnCallDesc
	ch <- teamsWithoutPolicyDesc
	ch <- alertGroupsByStatusDesc
	ch <- outboxEventsByStatusDesc
	ch <- outboundIntentsByStatusDesc
	ch <- outboundQueueLatenessDesc
	ch <- outboundCardsBehindDesc
	ch <- outboundCardStalenessDesc
}

func (c *BusinessCollector) Collect(ch chan<- prometheus.Metric) {
	snap, err := c.store.GetMetricsSnapshot()
	if err != nil {
		log.Printf("metrics collector: %v", err)
		return
	}

	for _, ag := range snap.ActiveAlertGroups {
		ch <- prometheus.MustNewConstMetric(
			activeAlertGroupsDesc, prometheus.GaugeValue,
			float64(ag.Count), ag.TeamID, ag.Severity,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		teamsWithoutOnCallDesc, prometheus.GaugeValue,
		float64(snap.TeamsWithoutOnCall),
	)
	ch <- prometheus.MustNewConstMetric(
		teamsWithPermanentOnCallDesc, prometheus.GaugeValue,
		float64(snap.TeamsWithPermanentOnCall),
	)
	ch <- prometheus.MustNewConstMetric(
		teamsWithoutPolicyDesc, prometheus.GaugeValue,
		float64(snap.TeamsWithoutPolicy),
	)

	for _, ag := range snap.AlertGroupsByStatus {
		ch <- prometheus.MustNewConstMetric(
			alertGroupsByStatusDesc, prometheus.GaugeValue,
			float64(ag.Count), ag.TeamID, ag.Severity, ag.Status,
		)
	}

	for _, sc := range snap.OutboxEventsByStatus {
		ch <- prometheus.MustNewConstMetric(
			outboxEventsByStatusDesc, prometheus.GaugeValue,
			float64(sc.Count), sc.Status,
		)
	}

	for _, sc := range snap.OutboundIntentsByStatus {
		ch <- prometheus.MustNewConstMetric(
			outboundIntentsByStatusDesc, prometheus.GaugeValue,
			float64(sc.Count), sc.Family, sc.Status,
		)
	}

	for _, l := range snap.OutboundLatenessSeconds {
		ch <- prometheus.MustNewConstMetric(
			outboundQueueLatenessDesc, prometheus.GaugeValue,
			l.Seconds, l.Family,
		)
	}

	for _, b := range snap.OutboundCardsBehind {
		ch <- prometheus.MustNewConstMetric(
			outboundCardsBehindDesc, prometheus.GaugeValue,
			float64(b.Count), b.State,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		outboundCardStalenessDesc, prometheus.GaugeValue,
		snap.OutboundCardStalenessSeconds,
	)
}

// RegisterCollector registers the business metrics collector with the default Prometheus registry.
// metricsSource is the store as this collector needs it: one snapshot of the
// counts a scrape reports. Declared here rather than taken whole, so that what
// the collector may read is the whole of what is written here.
type metricsSource interface {
	GetMetricsSnapshot() (*model.MetricsSnapshot, error)
}

func RegisterCollector(s metricsSource) {
	prometheus.MustRegister(&BusinessCollector{store: s})
}

// RegisterCollectorWith registers the business metrics collector with the given registry.
func RegisterCollectorWith(reg prometheus.Registerer, s metricsSource) {
	reg.MustRegister(&BusinessCollector{store: s})
}
