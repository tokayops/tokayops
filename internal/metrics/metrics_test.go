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
