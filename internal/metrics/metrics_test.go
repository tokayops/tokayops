package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/model"
)

func TestAllMetricsRegistered(t *testing.T) {
	// Verify that all metric vars are not nil and can be described.
	// init() in metrics.go registers everything; a double-register would panic.
	checks := []struct {
		name string
		desc func() string
	}{
		{"http_requests_total", descFromCounterVec(HTTPRequestsTotal)},
		{"http_request_duration_seconds", descFromHistogramVec(HTTPRequestDuration)},
		{"alerts_received_total", descFromCounterVec(AlertsReceivedTotal)},
		{"alert_groups_created_total", descFromCounterVec(AlertGroupsCreatedTotal)},
		{"alert_groups_resolved_total", descFromCounterVec(AlertGroupsResolvedTotal)},
		{"unknown_team_alert_groups_total", descFromCounterVec(UnknownTeamAlertGroupsTotal)},
		{"engine_runs_total", descFromCounter(EngineRunsTotal)},
		{"engine_alert_groups_picked_total", descFromCounter(EngineAlertGroupsPickedTotal)},
		{"engine_escalation_build_deferrals_total", descFromCounter(EngineEscalationBuildDeferralsTotal)},
		{"alert_group_resolution_duration_seconds", descFromHistogramVec(AlertGroupResolutionDuration)},
		{"handoff_warmup_not_complete_total", descFromCounter(HandoffWarmupNotComplete)},
		{"alert_group_ack_duration_seconds", descFromHistogramVec(AlertGroupAckDuration)},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			got := c.desc()
			if got == "" {
				t.Errorf("metric %q has empty description", c.name)
			}
		})
	}
}

func descFromCounterVec(cv *prometheus.CounterVec) func() string {
	return func() string {
		ch := make(chan *prometheus.Desc, 1)
		cv.Describe(ch)
		d := <-ch
		return d.String()
	}
}

func descFromHistogramVec(hv *prometheus.HistogramVec) func() string {
	return func() string {
		ch := make(chan *prometheus.Desc, 1)
		hv.Describe(ch)
		d := <-ch
		return d.String()
	}
}

func descFromCounter(c prometheus.Counter) func() string {
	return func() string {
		ch := make(chan *prometheus.Desc, 1)
		c.Describe(ch)
		d := <-ch
		return d.String()
	}
}

func TestObserveResolution(t *testing.T) {
	t.Run("records duration with oncall user", func(t *testing.T) {
		created := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		resolved := created.Add(30 * time.Minute)
		ag := &model.AlertGroup{
			TeamID:   "platform",
			Severity: "critical",
			OnCallSnapshot: &model.OnCallResult{
				L1Users: []*model.User{{Name: "alice"}},
			},
			CreatedAt:  created,
			ResolvedAt: &resolved,
		}

		countBefore := getResolutionHistogramCount(t, "platform", "critical", "alice")
		ObserveResolution(ag)
		countAfter := getResolutionHistogramCount(t, "platform", "critical", "alice")

		if countAfter != countBefore+1 {
			t.Errorf("expected histogram count to increment by 1, got %d -> %d", countBefore, countAfter)
		}
	})

	t.Run("records with none when no oncall", func(t *testing.T) {
		created := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
		resolved := created.Add(1 * time.Hour)
		ag := &model.AlertGroup{
			TeamID:     "backend",
			Severity:   "warning",
			CreatedAt:  created,
			ResolvedAt: &resolved,
		}

		countBefore := getResolutionHistogramCount(t, "backend", "warning", "none")
		ObserveResolution(ag)
		countAfter := getResolutionHistogramCount(t, "backend", "warning", "none")

		if countAfter != countBefore+1 {
			t.Errorf("expected histogram count to increment by 1, got %d -> %d", countBefore, countAfter)
		}
	})

	t.Run("skips when ResolvedAt is nil", func(t *testing.T) {
		ag := &model.AlertGroup{
			TeamID:   "infra",
			Severity: "critical",
		}

		countBefore := getResolutionHistogramCount(t, "infra", "critical", "none")
		ObserveResolution(ag)
		countAfter := getResolutionHistogramCount(t, "infra", "critical", "none")

		if countAfter != countBefore {
			t.Errorf("expected no change when ResolvedAt is nil, got %d -> %d", countBefore, countAfter)
		}
	})
}

func TestObserveAck(t *testing.T) {
	t.Run("records ack duration", func(t *testing.T) {
		ag := &model.AlertGroup{
			TeamID:    "platform",
			Severity:  "critical",
			CreatedAt: time.Now().Add(-5 * time.Minute),
		}

		countBefore := getAckHistogramCount(t, "platform", "critical")
		ObserveAck(ag)
		countAfter := getAckHistogramCount(t, "platform", "critical")

		if countAfter != countBefore+1 {
			t.Errorf("expected histogram count to increment by 1, got %d -> %d", countBefore, countAfter)
		}
	})
}

func getAckHistogramCount(t *testing.T, team, severity string) uint64 {
	t.Helper()
	observer, err := AlertGroupAckDuration.GetMetricWithLabelValues(team, severity)
	if err != nil {
		t.Fatalf("failed to get metric: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func getResolutionHistogramCount(t *testing.T, team, severity, oncallUser string) uint64 {
	t.Helper()
	observer, err := AlertGroupResolutionDuration.GetMetricWithLabelValues(team, severity, oncallUser)
	if err != nil {
		t.Fatalf("failed to get metric: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestTheAdmissionHistogramCanSeeTheHandoverProfile.
//
// Asserted against the histogram the registry actually gathers, not against the
// slice the source declares. What the arithmetic of a worker does is one
// question; whether the instrument watching it has a boundary anywhere near the
// threshold is another, and only the second one decides whether an SLO can be
// read at all.
//
// The handover family lives at 240 to 300 seconds. With 60 as the last finite
// boundary - which is where this histogram was - every healthy observation
// lands in +Inf, histogram_quantile answers 60 for all of them, and the SLO
// reads green at exactly the moment it is broken.
func TestTheAdmissionHistogramCanSeeTheHandoverProfile(t *testing.T) {
	OutboundAdmissionLatencySeconds.WithLabelValues("handoff").Observe(1)

	gathered, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buckets []float64
	for _, family := range gathered {
		if family.GetName() != "outbound_admission_latency_seconds" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				buckets = append(buckets, b.GetUpperBound())
			}
		}
	}
	if len(buckets) == 0 {
		t.Fatal("the histogram is not in what the registry gathers")
	}

	has := func(bound float64) bool {
		for _, b := range buckets {
			if b == bound {
				return true
			}
		}
		return false
	}
	// The two thresholds this family promises, and the one paging promises -
	// because the boundaries past 60 were added for the second family and must
	// not have moved the first one's.
	for _, bound := range []float64{300, 360, 60} {
		if !has(bound) {
			t.Errorf("no boundary at %vs; the profile there cannot be measured: %v", bound, buckets)
		}
	}
}
