package slacksync

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tokayops/tokayops/internal/metrics"
)

// histogramCount is how many observations a labelled histogram has taken. The
// count is the whole assertion here: the point is that the consumer observes
// its own tick, not how long the tick was.
func histogramCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	m, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T is not a Metric", o)
	}
	var got dto.Metric
	if err := m.Write(&got); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return got.GetHistogram().GetSampleCount()
}

// TestTheSyncersTickIsObservedUnderItsOwnName is the counterpart of the
// notifier's. Two consumers project the same schedules on different intervals,
// and a series they shared would be a p99 over two workloads.
func TestTheSyncersTickIsObservedUnderItsOwnName(t *testing.T) {
	before := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer))
	otherBefore := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier))

	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(usergroupDuty("sched-1", "S12345", "g-a", "alice"))
	if err := newTestSyncer(t, stub, oncall, slackIDsFor("alice")).
		SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	if histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer)) == before {
		t.Error("the syncer ticked without observing how long its projection took")
	}
	if got := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier)); got != otherBefore {
		t.Error("the syncer's tick was counted against the notifier")
	}
}

// counterValue reads a counter the way the API tests do, so the assertions can
// be about what was counted rather than about a metrics harness.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
