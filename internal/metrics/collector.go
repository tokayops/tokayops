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
	outboxDeliveriesByStatusDesc = prometheus.NewDesc(
		"outbox_deliveries_by_status",
		"Current count of outbox deliveries by status.",
		[]string{"status"}, nil,
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
	ch <- outboxDeliveriesByStatusDesc
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

	for _, sc := range snap.OutboxDeliveriesByStatus {
		ch <- prometheus.MustNewConstMetric(
			outboxDeliveriesByStatusDesc, prometheus.GaugeValue,
			float64(sc.Count), sc.Status,
		)
	}
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
