package outbound

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The liveness counters have to exist before the thing they watch has ever
// happened. A counter that appears on its first increment gives a rule over
// rate() nothing to be zero about when the worker never started - which is the
// one case the rule is for. So the series are asserted on the DEFAULT registry,
// where the package's init put them, not on a registry a test built.
//
// The two families no test in this package ever ticks are also asserted at
// zero: the notification family's worker is what the worker tests drive, and
// its value depends on test order.

func gatherDefault(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	gathered, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather the default registry: %v", err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, mf := range gathered {
		byName[mf.GetName()] = mf
	}
	return byName
}

func seriesOf(mf *dto.MetricFamily, want map[string]string) (*dto.Metric, bool) {
	if mf == nil {
		return nil, false
	}
next:
	for _, m := range mf.GetMetric() {
		have := map[string]string{}
		for _, l := range m.GetLabel() {
			have[l.GetName()] = l.GetValue()
		}
		if len(have) != len(want) {
			continue
		}
		for k, v := range want {
			if have[k] != v {
				continue next
			}
		}
		return m, true
	}
	return nil, false
}

func TestTheLivenessSeriesExistBeforeAnythingTicked(t *testing.T) {
	byName := gatherDefault(t)

	ticks := byName["outbound_worker_ticks_total"]
	if ticks == nil {
		t.Fatal("outbound_worker_ticks_total is not on the default registry")
	}
	if ticks.GetType() != dto.MetricType_COUNTER {
		t.Errorf("outbound_worker_ticks_total is a %s, not a counter", ticks.GetType())
	}
	for _, family := range Families() {
		m, ok := seriesOf(ticks, map[string]string{"family": family})
		if !ok {
			t.Errorf("no outbound_worker_ticks_total series for %s before its worker ticked", family)
			continue
		}
		if family != FamilyNotification && m.GetCounter().GetValue() != 0 {
			t.Errorf("outbound_worker_ticks_total{family=%q} = %v before any tick, want 0",
				family, m.GetCounter().GetValue())
		}
	}

	leases := byName["outbound_leases_expired_total"]
	if leases == nil {
		t.Fatal("outbound_leases_expired_total is not on the default registry")
	}
	for _, family := range Families() {
		for _, to := range RecoveryTargets() {
			m, ok := seriesOf(leases, map[string]string{"family": family, "to": string(to)})
			if !ok {
				t.Errorf("no outbound_leases_expired_total series for %s -> %s", family, to)
				continue
			}
			if family != FamilyNotification && m.GetCounter().GetValue() != 0 {
				t.Errorf("outbound_leases_expired_total{%s,%s} = %v before any recovery, want 0",
					family, to, m.GetCounter().GetValue())
			}
		}
	}

	if fanout := byName["outbound_fanout_ticks_total"]; fanout == nil {
		t.Error("outbound_fanout_ticks_total is not on the default registry")
	} else if fanout.GetType() != dto.MetricType_COUNTER {
		t.Errorf("outbound_fanout_ticks_total is a %s, not a counter", fanout.GetType())
	}
}

// The label names are what the rules file is written against, so they are
// pinned exactly: a rule over {family="notification"} finds nothing on a series
// whose label was renamed to "partition", and promtool cannot tell.
func TestTheLivenessSeriesCarryExactlyTheLabelsTheRulesName(t *testing.T) {
	byName := gatherDefault(t)

	labelsOf := func(m *dto.Metric) []string {
		var names []string
		for _, l := range m.GetLabel() {
			names = append(names, l.GetName())
		}
		return names
	}
	want := map[string][]string{
		"outbound_worker_ticks_total":   {"family"},
		"outbound_leases_expired_total": {"family", "to"},
		"outbound_fanout_ticks_total":   nil,
	}
	for name, labels := range want {
		mf := byName[name]
		if mf == nil {
			t.Errorf("%s is missing", name)
			continue
		}
		for _, m := range mf.GetMetric() {
			got := labelsOf(m)
			if len(got) != len(labels) {
				t.Errorf("%s carries labels %v, want %v", name, got, labels)
				break
			}
			for i := range labels {
				if got[i] != labels[i] {
					t.Errorf("%s carries labels %v, want %v", name, got, labels)
					break
				}
			}
		}
	}
}
