package outbound

import (
	"context"
	"errors"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
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
		// The largest window a duration holds, and the first that does not: a
		// day more wraps negative and puts the cutoff in the future.
		{strconv.Itoa(RetentionMaxDays), RetentionMaxDays, ""},
		{strconv.Itoa(RetentionMaxDays + 1), 0, "at most"},
		{"9223372036854775807", 0, "at most"},
		{"99999999999999999999", 0, "whole number of days"},
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
	if _, err := NewRetention(nil, RetentionMaxDays+1); err == nil {
		t.Error("a loop over a window that does not fit a duration was built")
	}
	if _, err := NewRetention(nil, math.MaxInt); err == nil {
		t.Error("a loop over the largest int was built")
	}
	// The largest window that fits still measures from the past.
	st := &sweeper{}
	r, err := NewRetention(st, RetentionMaxDays)
	if err != nil {
		t.Fatalf("the largest window was refused: %v", err)
	}
	if r.window <= 0 {
		t.Fatalf("the largest window is %s", r.window)
	}
	r.Pass(context.Background())
	if len(st.cutoffs) != 1 || !st.cutoffs[0].Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("the largest window measured from %v, want the distant past", st.cutoffs)
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

// ticking is a store that reports each call on a channel, for a loop running
// in its own goroutine.
type ticking struct{ calls chan time.Time }

func (s *ticking) SweepDeliveryHistory(context.Context, time.Time, int) (SweepResult, error) {
	s.calls <- time.Now()
	return SweepResult{}, nil
}

// lockedLog is a log sink a test can read while the loop writes it.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestTheFirstPassWaitsForTheStartDelay: the loop's first pass is at the
// start delay and not at the start - an instance coming up is building its
// schema and starting its workers, and a sweep in the same second would
// compete with them for the database - and not at the first tick of the
// hourly interval either. After it, the interval; and every pass writes the
// line an operator reads the first pass by.
func TestTheFirstPassWaitsForTheStartDelay(t *testing.T) {
	sink := &lockedLog{}
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	st := &ticking{calls: make(chan time.Time, 8)}
	r := &Retention{store: st, window: 24 * time.Hour, interval: time.Hour,
		first: 200 * time.Millisecond, chunk: 10, maxChunks: 3, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	select {
	case at := <-st.calls:
		t.Fatalf("the first pass ran %s after the start, before the delay", at.Sub(started))
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case at := <-st.calls:
		if since := at.Sub(started); since < r.first {
			t.Fatalf("the first pass ran %s after the start, before the delay of %s", since, r.first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first pass within five seconds; the loop is waiting for the interval")
	}
	cancel()
	<-done
	for _, line := range []string{
		"outbound retention started: window 24h0m0s, every 1h0m0s, first pass in 200ms",
		"outbound retention: removed 0 commitments, 0 attempts, 0 observations, 0 events, 0 outbox events older than",
		"outbound retention stopped",
	} {
		if !strings.Contains(sink.String(), line) {
			t.Errorf("the log does not say %q:\n%s", line, sink.String())
		}
	}

	// And after the first pass, the interval.
	st = &ticking{calls: make(chan time.Time, 8)}
	r = &Retention{store: st, window: 24 * time.Hour, interval: 100 * time.Millisecond,
		first: time.Millisecond, chunk: 10, maxChunks: 3, now: time.Now}
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		select {
		case <-st.calls:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d passes within five seconds; the loop does not keep ticking", i)
		}
	}
	cancel()
	<-done
}

// TestTheLoopIsBuiltWithTheProductionCadence: a loop from the constructor
// passes a minute after the start and hourly after that, in those units -
// the checklist an operator reads the first pass by says a minute, and the
// tests above build their loops by hand.
func TestTheLoopIsBuiltWithTheProductionCadence(t *testing.T) {
	r, err := NewRetention(&sweeper{}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if r.first != time.Minute {
		t.Errorf("the first pass is %s after the start, want a minute", r.first)
	}
	if r.interval != time.Hour {
		t.Errorf("a pass every %s, want hourly", r.interval)
	}
	if r.window != 30*24*time.Hour {
		t.Errorf("the window is %s, want 30 days", r.window)
	}
	if r.chunk != RetentionChunk || r.maxChunks != RetentionMaxChunks {
		t.Errorf("chunks of %d, at most %d per pass", r.chunk, r.maxChunks)
	}
}
