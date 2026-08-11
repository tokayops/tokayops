package dispatcher

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/schedulerender"
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

// The threshold for moving to batch reads is stated as "a tick longer than five
// seconds". This is what makes that checkable in production rather than only in
// a benchmark - and it has to be per consumer, because the two tick at
// different intervals over the same schedules.
func TestProjectionDurationIsObservedPerConsumer(t *testing.T) {
	notifierBefore := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier))
	syncerBefore := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer))

	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	notifierMid := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier))
	if notifierMid == notifierBefore {
		t.Error("the notifier ticked without observing how long its projection took")
	}
	// A tick of one consumer must not land in the other's series, or a p99 is
	// an average of two different workloads on two different intervals.
	if got := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer)); got != syncerBefore {
		t.Error("the notifier's tick was counted against the syncer")
	}

	// And the other way round, or the assertion above only says that one of
	// the two constants is spelled correctly.
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(usergroupDuty("sched-1", "S12345", "g-a", "alice"))
	if err := newTestSyncer(t, stub, oncall, slackIDsFor("alice")).
		SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	if got := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer)); got == syncerBefore {
		t.Error("the syncer ticked without observing how long its projection took")
	}
	if got := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier)); got != notifierMid {
		t.Error("the syncer's tick was counted against the notifier")
	}
}

// One damaged schedule seen by both consumers is two observations, and they
// have to stay apart: summed, they say "two damaged schedules" on two different
// intervals, which is the reading that made the counter useless before.
func TestProjectionFailuresAreLabelledByConsumer(t *testing.T) {
	reason := schedulerender.FailureRevisionGap
	failure := schedulerender.BulkOnCall{
		Failures: []schedulerender.ProjectionFailure{{
			ScheduleID: "sched-broken",
			TeamID:     "team-1",
			Reason:     reason,
			Err:        errors.New("no revision in force"),
		}},
	}

	notifierBefore := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerHandoffNotifier, string(reason)))
	syncerBefore := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerUsergroupSyncer, string(reason)))

	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp()
	env.oncall.setBulk(failure)
	if !env.notifier.checkAll(context.Background()) {
		t.Fatal("a damaged schedule failed the whole tick")
	}

	notifierAfter := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerHandoffNotifier, string(reason)))
	syncerAfter := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerUsergroupSyncer, string(reason)))

	if notifierAfter-notifierBefore != 1 {
		t.Errorf("notifier series moved by %v, want 1", notifierAfter-notifierBefore)
	}
	if syncerAfter != syncerBefore {
		t.Errorf("the notifier's failure was counted against the syncer: %v", syncerAfter-syncerBefore)
	}
}
