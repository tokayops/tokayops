package outbound

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
)

// The fan-out loop, driven by hand over a store that answers what it is told
// to. What the loop owns is small and exact: how many events a tick takes,
// where it stops, and what it counts after the store has committed.

type scriptedFanOut struct {
	answers []func() (FanOutResult, error)
	calls   int
}

func (s *scriptedFanOut) FanOutNextEvent(context.Context) (FanOutResult, error) {
	if s.calls >= len(s.answers) {
		s.calls++
		return FanOutResult{}, nil
	}
	answer := s.answers[s.calls]
	s.calls++
	return answer()
}

func fannedOut(commitments int) func() (FanOutResult, error) {
	return func() (FanOutResult, error) {
		return FanOutResult{Found: true, EventID: "evt", Outcome: SubmitCreated,
			Commitments: commitments}, nil
	}
}

func nothingPending() (FanOutResult, error) { return FanOutResult{}, nil }

func newFanOutFor(t *testing.T, st fanOutStore) *FanOut {
	t.Helper()
	f, err := NewFanOut(st)
	if err != nil {
		t.Fatalf("build the fan-out: %v", err)
	}
	return f
}

// TestTheFanOutTakesTheFamilysNumbers: the ticker is the family's claim
// interval and a tick takes at most the family's pool size, for the reason
// there are no separate numbers - there is no second clock to be fair to.
func TestTheFanOutTakesTheFamilysNumbers(t *testing.T) {
	policy, _ := PolicyOf(FamilyWebhook)
	f := newFanOutFor(t, &scriptedFanOut{})
	if f.interval != policy.ClaimInterval || f.perTick != policy.PoolSize {
		t.Fatalf("fan-out every %s taking %d, the family says %s and %d",
			f.interval, f.perTick, policy.ClaimInterval, policy.PoolSize)
	}
	if f.interval != 2*time.Second || f.perTick != 8 {
		t.Fatalf("the family's numbers moved: %s, %d", f.interval, f.perTick)
	}
}

// TestATickStopsWhenTheQueueIsEmpty: three events pending, three fanned out, and
// the fourth call - the one that says nothing is pending - ends the tick without
// asking again.
func TestATickStopsWhenTheQueueIsEmpty(t *testing.T) {
	st := &scriptedFanOut{answers: []func() (FanOutResult, error){
		fannedOut(2), fannedOut(0), fannedOut(1), nothingPending,
	}}
	if done := newFanOutFor(t, st).Tick(context.Background()); done != 3 {
		t.Fatalf("%d events fanned out, there were 3", done)
	}
	if st.calls != 4 {
		t.Fatalf("the store was asked %d times; three events and one empty answer is 4", st.calls)
	}
}

// TestATickTakesNoMoreThanThePool: with a queue that never empties, one tick
// takes exactly the pool size and leaves the rest for the next.
func TestATickTakesNoMoreThanThePool(t *testing.T) {
	answers := make([]func() (FanOutResult, error), 0, 20)
	for i := 0; i < 20; i++ {
		answers = append(answers, fannedOut(1))
	}
	st := &scriptedFanOut{answers: answers}
	f := newFanOutFor(t, st)
	if done := f.Tick(context.Background()); done != f.perTick {
		t.Fatalf("one tick fanned out %d events, the pool is %d", done, f.perTick)
	}
	if st.calls != f.perTick {
		t.Fatalf("the store was asked %d times for a pool of %d", st.calls, f.perTick)
	}
}

// TestAdmissionsAreCountedAfterTheCommit: the store works inside the
// transaction and counts nothing; the loop counts what the store reports as
// committed, by family and by what it promised - and an event nobody subscribed
// to is counted as exactly that, not as an admission like any other.
func TestAdmissionsAreCountedAfterTheCommit(t *testing.T) {
	created := counterValue(t, metrics.OutboundAdmissionsTotal, FamilyWebhook, "created")
	nobody := counterValue(t, metrics.OutboundAdmissionsTotal, FamilyWebhook, "no_targets")

	st := &scriptedFanOut{answers: []func() (FanOutResult, error){
		fannedOut(2), fannedOut(0), fannedOut(3), nothingPending,
	}}
	newFanOutFor(t, st).Tick(context.Background())

	if got := counterValue(t, metrics.OutboundAdmissionsTotal, FamilyWebhook, "created"); got != created+2 {
		t.Errorf("created admissions counted %v, want %v", got-created, 2)
	}
	if got := counterValue(t, metrics.OutboundAdmissionsTotal, FamilyWebhook, "no_targets"); got != nobody+1 {
		t.Errorf("no_targets admissions counted %v, want %v", got-nobody, 1)
	}
}

// TestARefusedEventHoldsTheQueue: an event this build cannot read is counted as
// a storage contract failure, named, and NOT stepped over - the tick ends there
// and the next one will meet the same event. Nothing after it is touched.
func TestARefusedEventHoldsTheQueue(t *testing.T) {
	before := counterValue(t, metrics.StorageContractFailuresTotal, "event_outbox")
	created := counterValue(t, metrics.OutboundAdmissionsTotal, FamilyWebhook, "created")

	refused := func() (FanOutResult, error) {
		return FanOutResult{Found: true, EventID: "evt-bad", Refused: true},
			errors.New("event evt-bad: store: outbound contract violation: unknown webhook event type")
	}
	st := &scriptedFanOut{answers: []func() (FanOutResult, error){
		fannedOut(1), refused, fannedOut(1), fannedOut(1),
	}}
	if done := newFanOutFor(t, st).Tick(context.Background()); done != 1 {
		t.Fatalf("%d events fanned out around a refused one, want 1 - the one before it", done)
	}
	if st.calls != 2 {
		t.Fatalf("the store was asked %d times; a refused event ends the tick at the second", st.calls)
	}
	if got := counterValue(t, metrics.StorageContractFailuresTotal, "event_outbox"); got != before+1 {
		t.Errorf("storage contract failures counted %v, want 1", got-before)
	}
	if got := counterValue(t, metrics.OutboundAdmissionsTotal, FamilyWebhook, "created"); got != created+1 {
		t.Errorf("created admissions counted %v, want 1", got-created)
	}
}

// TestADatabaseErrorIsNotAStorageContractFailure: the store failing is logged
// and ends the tick, and it is not counted as a row this build cannot read.
func TestADatabaseErrorIsNotAStorageContractFailure(t *testing.T) {
	before := counterValue(t, metrics.StorageContractFailuresTotal, "event_outbox")
	st := &scriptedFanOut{answers: []func() (FanOutResult, error){
		func() (FanOutResult, error) { return FanOutResult{}, errors.New("connection reset") },
		fannedOut(1),
	}}
	if done := newFanOutFor(t, st).Tick(context.Background()); done != 0 {
		t.Fatalf("%d events fanned out after the database failed", done)
	}
	if got := counterValue(t, metrics.StorageContractFailuresTotal, "event_outbox"); got != before {
		t.Errorf("a database error was counted as a storage contract failure")
	}
}

// TestACancelledFanOutStopsAsking: once the context is done the loop asks the
// store nothing more, whatever is pending.
func TestACancelledFanOutStopsAsking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := &scriptedFanOut{answers: []func() (FanOutResult, error){fannedOut(1)}}
	if done := newFanOutFor(t, st).Tick(ctx); done != 0 || st.calls != 0 {
		t.Fatalf("a cancelled fan-out did %d and asked %d times", done, st.calls)
	}
}

// TestEveryFanOutTickIsCountedEvenAnEmptyOne: the tick counter is the liveness
// of the loop, so a tick that found no event counts exactly like one that
// fanned out eight. A counter that only moved on work would be silent for the
// same reason on a stopped loop and on an empty queue.
func TestEveryFanOutTickIsCountedEvenAnEmptyOne(t *testing.T) {
	f := newFanOutFor(t, &scriptedFanOut{answers: []func() (FanOutResult, error){nothingPending}})

	before := plainCounterValue(t, metrics.OutboundFanOutTicksTotal)
	if got := f.Tick(context.Background()); got != 0 {
		t.Fatalf("an empty tick fanned out %d", got)
	}
	f.Tick(context.Background())
	after := plainCounterValue(t, metrics.OutboundFanOutTicksTotal)

	if after-before != 2 {
		t.Fatalf("two empty ticks moved outbound_fanout_ticks_total by %v, want 2", after-before)
	}
}

func plainCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
