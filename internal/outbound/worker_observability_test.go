package outbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What the worker says and counts about the work it finds abandoned, and about
// its own passes. None of this changes what is delivered; all of it is what an
// operator has instead of a debugger.

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var written bytes.Buffer
	log.SetOutput(&written)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &written
}

// workerOf builds a worker of the given family over the fake store; the
// paging family goes through NewWorker like production does.
func workerOf(t *testing.T, family string, store *fakeStore) *Worker {
	t.Helper()
	channels := map[string]Channel{"slack": newFakeChannel(), "webhook": newFakeChannel()}
	if family == FamilyNotification {
		return testWorker(store, channels)
	}
	w, err := NewWorkerFor(family, store, "worker-"+family, channels)
	if err != nil {
		t.Fatalf("build the %s worker: %v", family, err)
	}
	return w
}

// TestEveryTickIsCountedEvenAnEmptyOne: the tick counter is the liveness of
// the worker, not of the queue, so a pass that found nothing counts exactly
// like one that found work - and it counts under the family the worker runs,
// each of the three. A label that named one family for all three would make
// two running workers look stopped.
func TestEveryTickIsCountedEvenAnEmptyOne(t *testing.T) {
	for _, family := range Families() {
		t.Run(family, func(t *testing.T) {
			before := map[string]float64{}
			for _, f := range Families() {
				before[f] = counterValue(t, metrics.OutboundWorkerTicksTotal, f)
			}

			w := workerOf(t, family, newFakeStore())
			w.tick(context.Background())
			w.tick(context.Background())

			for _, f := range Families() {
				moved := counterValue(t, metrics.OutboundWorkerTicksTotal, f) - before[f]
				want := 0.0
				if f == family {
					want = 2
				}
				if moved != want {
					t.Errorf("two empty ticks of the %s worker moved outbound_worker_ticks_total{family=%q} by %v, want %v",
						family, f, moved, want)
				}
			}
		})
	}
}

// TestARecoveredAttemptIsCountedAndNamed: every attempt recovery closes is one
// line naming the commitment, the attempt and where it went, and one increment
// on the counter the alert reads - labelled by that destination, because
// "went back to pending" and "waiting for a person" are different news, and
// by the family of the worker that ran the pass.
func TestARecoveredAttemptIsCountedAndNamed(t *testing.T) {
	for _, family := range []string{FamilyNotification, FamilyHandoff} {
		t.Run(family, func(t *testing.T) {
			store := newFakeStore()
			store.recovered = []Recovered{
				{IntentID: "intent-a", AttemptID: "attempt-a1", To: StatusPending, Row: "T7"},
				{IntentID: "intent-b", AttemptID: "attempt-b1", To: StatusManualReview, Row: "T7"},
				{IntentID: "intent-c", AttemptID: "attempt-c1", To: StatusPending, Row: "T7"},
			}
			written := captureLog(t)

			pendingBefore := counterValue(t, metrics.OutboundLeasesExpiredTotal, family, string(StatusPending))
			reviewBefore := counterValue(t, metrics.OutboundLeasesExpiredTotal, family, string(StatusManualReview))

			w := workerOf(t, family, store)
			w.tick(context.Background())

			if got := counterValue(t, metrics.OutboundLeasesExpiredTotal, family, string(StatusPending)) - pendingBefore; got != 2 {
				t.Errorf("outbound_leases_expired_total{%s,pending} moved by %v, want 2", family, got)
			}
			if got := counterValue(t, metrics.OutboundLeasesExpiredTotal, family, string(StatusManualReview)) - reviewBefore; got != 1 {
				t.Errorf("outbound_leases_expired_total{%s,manual_review} moved by %v, want 1", family, got)
			}

			for _, want := range []string{
				"lease expired with an attempt open: intent=intent-a attempt=attempt-a1 -> pending (T7)",
				"lease expired with an attempt open: intent=intent-b attempt=attempt-b1 -> manual_review (T7)",
				"lease expired with an attempt open: intent=intent-c attempt=attempt-c1 -> pending (T7)",
			} {
				if !strings.Contains(written.String(), want) {
					t.Errorf("the log does not say %q:\n%s", want, written.String())
				}
			}
		})
	}
}

// TestAPassThatFailedHalfwayStillCountsWhatItClosed: recovery commits one
// attempt per transaction and answers with the ones it closed beside the
// error of the one it could not - a row another transaction held past
// lock_timeout is enough. The closed attempt is real whether or not the pass
// finished: it is counted and named, and the error is reported after it.
// Handling the rows only when there was no error threw every one of them away
// exactly on the passes that had trouble.
func TestAPassThatFailedHalfwayStillCountsWhatItClosed(t *testing.T) {
	store := newFakeStore()
	store.recovered = []Recovered{
		{IntentID: "intent-first", AttemptID: "attempt-first", To: StatusPending, Row: "T7"},
	}
	store.recoverErr = errors.New("recover intent-second: lock timeout")
	written := captureLog(t)

	before := counterValue(t, metrics.OutboundLeasesExpiredTotal, FamilyNotification, string(StatusPending))
	w := workerOf(t, FamilyNotification, store)
	w.tick(context.Background())

	if got := counterValue(t, metrics.OutboundLeasesExpiredTotal, FamilyNotification, string(StatusPending)) - before; got != 1 {
		t.Errorf("the attempt closed before the failure moved the counter by %v, want 1", got)
	}
	out := written.String()
	if !strings.Contains(out, "lease expired with an attempt open: intent=intent-first attempt=attempt-first -> pending (T7)") {
		t.Errorf("the attempt closed before the failure is not named:\n%s", out)
	}
	if !strings.Contains(out, "recover: recover intent-second: lock timeout") {
		t.Errorf("the failure of the pass is not reported:\n%s", out)
	}
}

// TestExpiredCommitmentsAreNamedUpToTen: the expiry line names the first ten
// commitments with their groups and counts the rest, the way the engine names
// the batch it picked. Eleven expiries is the boundary: ten names and "(and 1
// more)".
func TestExpiredCommitmentsAreNamedUpToTen(t *testing.T) {
	store := newFakeStore()
	for i := 1; i <= 11; i++ {
		e := Expired{IntentID: fmt.Sprintf("intent-%02d", i)}
		if i%2 == 1 {
			e.AlertGroupID = fmt.Sprintf("group-%02d", i)
		}
		store.expired = append(store.expired, e)
	}
	written := captureLog(t)

	w := testWorker(store, map[string]Channel{"slack": newFakeChannel()})
	w.tick(context.Background())

	line := written.String()
	if !strings.Contains(line, "11 commitments passed their deadline unsent: ") {
		t.Fatalf("no expiry line with the count:\n%s", line)
	}
	for i := 1; i <= 10; i++ {
		want := fmt.Sprintf("intent-%02d", i)
		if i%2 == 1 {
			want += fmt.Sprintf(" (group group-%02d)", i)
		}
		if !strings.Contains(line, want) {
			t.Errorf("the expiry line does not name %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "intent-11") {
		t.Errorf("the eleventh commitment is named instead of counted:\n%s", line)
	}
	if !strings.Contains(line, "(and 1 more)") {
		t.Errorf("the rest is not counted:\n%s", line)
	}
}

// TestALostBeginIsSaidOutLoud: a claimed commitment whose attempt the store
// refused to start - the lease taken by recovery, the commitment finalised by
// an acknowledgement - used to vanish from the log. The other side writes its
// own line; this is the one place the worker that lost says so.
func TestALostBeginIsSaidOutLoud(t *testing.T) {
	store := newFakeStore()
	store.beginOut = BeginAttemptResult{Outcome: BeginLeaseLost}
	channel := newFakeChannel()
	written := captureLog(t)

	w := testWorker(store, map[string]Channel{"slack": channel})
	w.serve(context.Background(), Leased{
		Intent: Intent{
			KeyKind: keys.KindEscalation,
			ID:      "intent-lost", Provider: "slack", Status: StatusPending,
			Family: FamilyNotification, Form: FormEditable,
		},
		LeaseToken: "token-1",
	})

	want := "begin intent-lost answered " + string(BeginLeaseLost)
	if !strings.Contains(written.String(), want) {
		t.Fatalf("the log does not say %q:\n%s", want, written.String())
	}
	if calls := channel.made(); len(calls) != 0 {
		t.Fatalf("a call was made after the store refused to start the attempt: %d", len(calls))
	}
}
