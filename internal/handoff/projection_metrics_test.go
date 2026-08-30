package handoff

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

// TestTheNotifiersTickIsObservedUnderItsOwnName.
//
// Two consumers project the same schedules on different intervals, so a series
// they shared would be a p99 over two workloads and a failure count that says
// "two damaged schedules" when one schedule is damaged. The other series is
// read here as well: without it this asserts only that one of the two constants
// is spelled correctly.
func TestTheNotifiersTickIsObservedUnderItsOwnName(t *testing.T) {
	before := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier))
	otherBefore := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer))

	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	if histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerHandoffNotifier)) == before {
		t.Error("the notifier ticked without observing how long its projection took")
	}
	if got := histogramCount(t,
		metrics.ScheduleOnCallProjectionDuration.WithLabelValues(metrics.ConsumerUsergroupSyncer)); got != otherBefore {
		t.Error("the notifier's tick was counted against the syncer")
	}
}

// TestADamagedScheduleIsCountedUnderTheConsumerThatMetIt. One damaged schedule
// seen by both consumers is two observations, and they have to stay apart:
// summed, they say "two damaged schedules" on two different intervals, which is
// the reading that made the counter useless before.
func TestADamagedScheduleIsCountedUnderTheConsumerThatMetIt(t *testing.T) {
	reason := schedulerender.FailureRevisionGap
	failure := schedulerender.BulkOnCall{
		Failures: []schedulerender.ProjectionFailure{{
			ScheduleID: "sched-broken",
			TeamID:     "team-1",
			Reason:     reason,
			Err:        errors.New("no revision in force"),
		}},
	}

	before := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerHandoffNotifier, string(reason)))
	otherBefore := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerUsergroupSyncer, string(reason)))

	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp()
	env.oncall.setBulk(failure)
	if !env.notifier.checkAll(context.Background()) {
		t.Fatal("a damaged schedule failed the whole tick")
	}

	after := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerHandoffNotifier, string(reason)))
	if after-before != 1 {
		t.Errorf("the notifier's series moved by %v, want 1", after-before)
	}
	if got := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.
		WithLabelValues(metrics.ConsumerUsergroupSyncer, string(reason))); got != otherBefore {
		t.Errorf("the notifier's failure was counted against the syncer: %v", got-otherBefore)
	}
}
