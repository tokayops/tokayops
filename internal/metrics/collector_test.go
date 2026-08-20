package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestBusinessCollector_ActiveAlertGroups(t *testing.T) {
	s := store.NewMockStore()

	// Create alert groups in various states
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag1", AlertKey: "k1", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusTriggered,
	})
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag2", AlertKey: "k2", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusNew,
	})
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag3", AlertKey: "k3", TeamID: "devops", Severity: "warning",
		Status: model.AlertGroupStatusAcknowledged,
	})
	// Resolved — should NOT count
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag4", AlertKey: "k4", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusResolved,
	})

	collected := collectMetrics(t, s)

	// active_alert_groups{team=devops, severity=critical} = 2
	val := findGaugeValue(t, collected, "active_alert_groups", map[string]string{
		"team": "devops", "severity": "critical",
	})
	if val != 2 {
		t.Errorf("active_alert_groups{devops,critical} = %v, want 2", val)
	}

	// active_alert_groups{team=devops, severity=warning} = 1
	val = findGaugeValue(t, collected, "active_alert_groups", map[string]string{
		"team": "devops", "severity": "warning",
	})
	if val != 1 {
		t.Errorf("active_alert_groups{devops,warning} = %v, want 1", val)
	}
}

// The two on-call gauges are NOT asserted here, and that is a decision rather
// than a gap. They are answers about the revision model, which the mock does
// not implement - it would have to grow a second projection to do so, and two
// implementations of "who is on duty" is exactly what was removed. The mock
// reports zero for both; a test over that would assert the double, not the
// query.
//
// They are covered against a real database in
// store.TestGetMetricsSnapshotOnCallGauges.

func TestBusinessCollector_TeamsWithoutPolicy(t *testing.T) {
	s := store.NewMockStore()
	// Both seed teams have no DefaultPolicyID
	collected := collectMetrics(t, s)

	val := findGaugeValue(t, collected, "teams_without_escalation_policy", nil)
	if val != 2 {
		t.Errorf("teams_without_escalation_policy = %v, want 2", val)
	}

	// Set a policy on devops
	team, _ := s.GetTeamByID("devops")
	team.DefaultPolicyID = "pol1"
	s.UpdateTeam(team)

	collected = collectMetrics(t, s)
	val = findGaugeValue(t, collected, "teams_without_escalation_policy", nil)
	if val != 1 {
		t.Errorf("teams_without_escalation_policy = %v, want 1", val)
	}
}

func TestBusinessCollector_AlertGroupsByStatus(t *testing.T) {
	s := store.NewMockStore()

	// Create alert groups in various statuses
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag1", AlertKey: "k1", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusTriggered,
	})
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag2", AlertKey: "k2", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusTriggered,
	})
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag3", AlertKey: "k3", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusAcknowledged,
	})
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag4", AlertKey: "k4", TeamID: "devops", Severity: "critical",
		Status: model.AlertGroupStatusResolved,
	})
	s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag5", AlertKey: "k5", TeamID: "devops", Severity: "warning",
		Status: model.AlertGroupStatusNew,
	})

	collected := collectMetrics(t, s)

	// alert_groups_by_status{team=devops, severity=critical, status=triggered} = 2
	val := findGaugeValue(t, collected, "alert_groups_by_status", map[string]string{
		"team": "devops", "severity": "critical", "status": "triggered",
	})
	if val != 2 {
		t.Errorf("alert_groups_by_status{devops,critical,triggered} = %v, want 2", val)
	}

	// alert_groups_by_status{team=devops, severity=critical, status=acknowledged} = 1
	val = findGaugeValue(t, collected, "alert_groups_by_status", map[string]string{
		"team": "devops", "severity": "critical", "status": "acknowledged",
	})
	if val != 1 {
		t.Errorf("alert_groups_by_status{devops,critical,acknowledged} = %v, want 1", val)
	}

	// Resolved status IS reported (unlike active_alert_groups which excludes it)
	val = findGaugeValue(t, collected, "alert_groups_by_status", map[string]string{
		"team": "devops", "severity": "critical", "status": "resolved",
	})
	if val != 1 {
		t.Errorf("alert_groups_by_status{devops,critical,resolved} = %v, want 1", val)
	}

	// alert_groups_by_status{team=devops, severity=warning, status=new} = 1
	val = findGaugeValue(t, collected, "alert_groups_by_status", map[string]string{
		"team": "devops", "severity": "warning", "status": "new",
	})
	if val != 1 {
		t.Errorf("alert_groups_by_status{devops,warning,new} = %v, want 1", val)
	}
}

func TestBusinessCollector_OutboxEventsByStatus(t *testing.T) {
	s := store.NewMockStore()

	s.CreateOutboxEvent(&model.OutboxEvent{
		ID: "evt-1", EventType: model.OutboxEventFiring, AlertGroupID: "ag-1",
		TeamID: "devops", Payload: []byte(`{}`), Status: model.OutboxEventStatusPending,
	})
	s.CreateOutboxEvent(&model.OutboxEvent{
		ID: "evt-2", EventType: model.OutboxEventFiring, AlertGroupID: "ag-2",
		TeamID: "devops", Payload: []byte(`{}`), Status: model.OutboxEventStatusPending,
	})
	s.CreateOutboxEvent(&model.OutboxEvent{
		ID: "evt-3", EventType: model.OutboxEventFiring, AlertGroupID: "ag-3",
		TeamID: "devops", Payload: []byte(`{}`), Status: model.OutboxEventStatusCompleted,
	})

	collected := collectMetrics(t, s)

	val := findGaugeValue(t, collected, "outbox_events_by_status", map[string]string{"status": "pending"})
	if val != 2 {
		t.Errorf("outbox_events_by_status{pending} = %v, want 2", val)
	}

	val = findGaugeValue(t, collected, "outbox_events_by_status", map[string]string{"status": "completed"})
	if val != 1 {
		t.Errorf("outbox_events_by_status{completed} = %v, want 1", val)
	}
}

func TestBusinessCollector_OutboxDeliveriesByStatus(t *testing.T) {
	s := store.NewMockStore()

	s.CreateOutboxEvent(&model.OutboxEvent{
		ID: "evt-1", EventType: model.OutboxEventFiring, AlertGroupID: "ag-1",
		TeamID: "devops", Payload: []byte(`{}`), Status: model.OutboxEventStatusCompleted,
	})

	s.CreateOutboxDelivery(&model.OutboxDelivery{
		ID: "del-1", EventID: "evt-1", IntegrationID: "integ-1", Status: model.OutboxDeliverySent,
	})
	s.CreateOutboxDelivery(&model.OutboxDelivery{
		ID: "del-2", EventID: "evt-1", IntegrationID: "integ-2", Status: model.OutboxDeliveryFailed,
	})
	s.CreateOutboxDelivery(&model.OutboxDelivery{
		ID: "del-3", EventID: "evt-1", IntegrationID: "integ-3", Status: model.OutboxDeliveryRetry,
	})

	collected := collectMetrics(t, s)

	val := findGaugeValue(t, collected, "outbox_deliveries_by_status", map[string]string{"status": "sent"})
	if val != 1 {
		t.Errorf("outbox_deliveries_by_status{sent} = %v, want 1", val)
	}

	val = findGaugeValue(t, collected, "outbox_deliveries_by_status", map[string]string{"status": "failed"})
	if val != 1 {
		t.Errorf("outbox_deliveries_by_status{failed} = %v, want 1", val)
	}

	val = findGaugeValue(t, collected, "outbox_deliveries_by_status", map[string]string{"status": "retry"})
	if val != 1 {
		t.Errorf("outbox_deliveries_by_status{retry} = %v, want 1", val)
	}
}

// --------------- helpers ---------------

// collectMetrics registers a fresh collector in a temporary registry, gathers, and returns families.
func collectMetrics(t *testing.T, s store.StoreInterface) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	metrics.RegisterCollectorWith(reg, s)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return families
}

// findGaugeValue finds a gauge metric by name and optional label set.
func findGaugeValue(t *testing.T, families []*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m, labels) {
				return m.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("metric %q with labels %v not found", name, labels)
	return 0
}

func matchLabels(m *dto.Metric, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	have := make(map[string]string)
	for _, lp := range m.GetLabel() {
		have[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
