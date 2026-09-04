package outbound

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tokayops/tokayops/internal/metrics"
)

// The window as the environment says it, and the loop over a store that
// answers what it is told: how many chunks a pass runs, what it records as a
// success, and what it does not.

func TestTheRetentionWindowIsReadStrictly(t *testing.T) {
	for _, tt := range []struct {
		value string
		days  int
		err   string
	}{
		{"", 30, ""},
		{"30", 30, ""},
		{"7", 7, ""},
		{"0", 0, ""},
		{"-1", 0, "cannot be negative"},
		{"abc", 0, "whole number of days"},
		{"1.5", 0, "whole number of days"},
	} {
		days, err := ParseRetentionWindow(tt.value)
		if tt.err == "" {
			if err != nil || days != tt.days {
				t.Errorf("%q: %d, %v; want %d", tt.value, days, err, tt.days)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.err) || !strings.Contains(err.Error(), RetentionEnv) {
			t.Errorf("%q: %v, want a refusal naming %s and saying %q", tt.value, err, RetentionEnv, tt.err)
		}
	}
	if _, err := NewRetention(nil, 0); err == nil {
		t.Error("a loop over a window of zero days was built; zero is off, not a loop")
	}
}

// sweeper is a store that answers a script of chunks.
type sweeper struct {
	answers []SweepResult
	err     error
	calls   int
	cutoffs []time.Time
}

func (s *sweeper) SweepDeliveryHistory(_ context.Context, cutoff time.Time, _ int) (SweepResult, error) {
	s.calls++
	s.cutoffs = append(s.cutoffs, cutoff)
	if s.err != nil {
		return SweepResult{}, s.err
	}
	if len(s.answers) == 0 {
		return SweepResult{}, nil
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	return answer, nil
}

func full(n int64) SweepResult {
	return SweepResult{Deleted: SweepCounts{Intents: n, Attempts: n, Events: n}}
}

func lastSuccessSeries() int {
	return testutil.CollectAndCount(metrics.OutboundRetentionLastSuccess)
}

func lastSuccess() float64 {
	return testutil.ToFloat64(metrics.OutboundRetentionLastSuccess.WithLabelValues())
}

func deletedOf(table string) float64 {
	return testutil.ToFloat64(metrics.OutboundRetentionDeletedTotal.WithLabelValues(table))
}

// TestAPassRunsChunksWhileTheyComeBackFull: the loop repeats the command
// while a chunk is full, stops at the first short one, and never runs more
// than its bound - the rest is the next hour's.
func TestAPassRunsChunksWhileTheyComeBackFull(t *testing.T) {
	st := &sweeper{answers: []SweepResult{full(2), full(2), full(1)}}
	r := &Retention{store: st, window: 24 * time.Hour, chunk: 2, maxChunks: 100, now: time.Now}
	outcome, total := r.Pass(context.Background())
	if outcome != PassSwept || st.calls != 3 || total.Intents != 5 {
		t.Fatalf("outcome %s after %d chunks, %d removed; want swept after 3 chunks, 5 removed", outcome, st.calls, total.Intents)
	}
	// Every chunk of one pass is measured from the same moment.
	for _, cutoff := range st.cutoffs[1:] {
		if !cutoff.Equal(st.cutoffs[0]) {
			t.Fatalf("the chunks of one pass used different cutoffs: %v", st.cutoffs)
		}
	}
	if age := time.Since(st.cutoffs[0]); age < 23*time.Hour || age > 25*time.Hour {
		t.Fatalf("the cutoff is %s ago, want the window", age)
	}

	// The bound: a store that always answers a full chunk is left after
	// maxChunks, and the pass still counts as a success - it committed every
	// chunk it ran.
	st = &sweeper{answers: []SweepResult{full(2), full(2), full(2), full(2), full(2)}}
	r = &Retention{store: st, window: 24 * time.Hour, chunk: 2, maxChunks: 3, now: time.Now}
	outcome, total = r.Pass(context.Background())
	if outcome != PassSwept || st.calls != 3 || total.Intents != 6 {
		t.Fatalf("outcome %s after %d chunks, %d removed; want swept after the bound of 3, 6 removed", outcome, st.calls, total.Intents)
	}
}

// TestOnlyAPassThatSweptIsASuccess: the success timestamp does not exist
// before the first pass that held the lock and committed; it moves for a
// pass that found nothing to remove, and not for a busy pass or a failed
// one. The counters move by what was removed, by table.
func TestOnlyAPassThatSweptIsASuccess(t *testing.T) {
	metrics.OutboundRetentionLastSuccess.Reset()
	metrics.OutboundRetentionDeletedTotal.Reset()
	for _, table := range RetentionTables() {
		metrics.OutboundRetentionDeletedTotal.WithLabelValues(table)
	}
	if lastSuccessSeries() != 0 {
		t.Fatal("the success series exists before any pass")
	}

	clock := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }

	busy := &sweeper{answers: []SweepResult{{Busy: true}}}
	r := &Retention{store: busy, window: 24 * time.Hour, chunk: 10, maxChunks: 3, now: now}
	if outcome, _ := r.Pass(context.Background()); outcome != PassBusy {
		t.Fatalf("a busy store gave %s", outcome)
	}
	if lastSuccessSeries() != 0 {
		t.Fatal("a busy pass recorded a success")
	}

	failing := &sweeper{err: errors.New("the database went away")}
	r = &Retention{store: failing, window: 24 * time.Hour, chunk: 10, maxChunks: 3, now: now}
	if outcome, _ := r.Pass(context.Background()); outcome != PassFailed {
		t.Fatalf("a failing store gave %s", outcome)
	}
	if lastSuccessSeries() != 0 {
		t.Fatal("a failed pass recorded a success")
	}

	nothing := &sweeper{}
	r = &Retention{store: nothing, window: 24 * time.Hour, chunk: 10, maxChunks: 3, now: now}
	if outcome, _ := r.Pass(context.Background()); outcome != PassSwept {
		t.Fatalf("a pass with nothing to remove gave %s", outcome)
	}
	if lastSuccessSeries() != 1 || lastSuccess() != float64(clock.Unix()) {
		t.Fatalf("after a pass with nothing to remove the success reads %v (%d series), want %d",
			lastSuccess(), lastSuccessSeries(), clock.Unix())
	}

	clock = clock.Add(time.Hour)
	swept := &sweeper{answers: []SweepResult{{Deleted: SweepCounts{Intents: 3, Attempts: 4, Observations: 1, Events: 9, Outbox: 2}}}}
	r = &Retention{store: swept, window: 24 * time.Hour, chunk: 10, maxChunks: 3, now: now}
	if outcome, _ := r.Pass(context.Background()); outcome != PassSwept {
		t.Fatalf("a sweeping pass gave %s", outcome)
	}
	if lastSuccess() != float64(clock.Unix()) {
		t.Fatalf("the success did not move to %d: %v", clock.Unix(), lastSuccess())
	}
	for table, want := range map[string]float64{
		"outbound_intents": 3, "outbound_attempts": 4, "outbound_attempt_observations": 1,
		"outbound_intent_events": 9, "event_outbox": 2,
	} {
		if got := deletedOf(table); got != want {
			t.Errorf("deleted_total{table=%q} = %v, want %v", table, got, want)
		}
	}

	// And a busy pass afterwards leaves the success where it was.
	clock = clock.Add(time.Hour)
	r = &Retention{store: &sweeper{answers: []SweepResult{{Busy: true}}}, window: 24 * time.Hour, chunk: 10, maxChunks: 3, now: now}
	r.Pass(context.Background())
	if lastSuccess() != float64(clock.Add(-time.Hour).Unix()) {
		t.Fatalf("a busy pass moved the success to %v", lastSuccess())
	}
}
