package outbound

import (
	"bytes"
	"context"
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

// TestEveryTickIsCountedEvenAnEmptyOne: the tick counter is the liveness of
// the worker, not of the queue, so a pass that found nothing counts exactly
// like one that found work.
func TestEveryTickIsCountedEvenAnEmptyOne(t *testing.T) {
	store := newFakeStore()
	w := testWorker(store, map[string]Channel{"slack": newFakeChannel()})

	before := counterValue(t, metrics.OutboundWorkerTicksTotal, FamilyNotification)
	w.tick(context.Background())
	w.tick(context.Background())
	after := counterValue(t, metrics.OutboundWorkerTicksTotal, FamilyNotification)

	if after-before != 2 {
		t.Fatalf("two empty ticks moved outbound_worker_ticks_total by %v, want 2", after-before)
	}
}

// TestARecoveredAttemptIsCountedAndNamed: every attempt recovery closes is one
// line naming the commitment, the attempt and where it went, and one increment
// on the counter the alert reads - labelled by that destination, because
// "went back to pending" and "waiting for a person" are different news.
func TestARecoveredAttemptIsCountedAndNamed(t *testing.T) {
	store := newFakeStore()
	store.recovered = []Recovered{
		{IntentID: "intent-a", AttemptID: "attempt-a1", To: StatusPending, Row: "T7"},
		{IntentID: "intent-b", AttemptID: "attempt-b1", To: StatusManualReview, Row: "T7"},
		{IntentID: "intent-c", AttemptID: "attempt-c1", To: StatusPending, Row: "T7"},
	}
	written := captureLog(t)

	pendingBefore := counterValue(t, metrics.OutboundLeasesExpiredTotal, FamilyNotification, string(StatusPending))
	reviewBefore := counterValue(t, metrics.OutboundLeasesExpiredTotal, FamilyNotification, string(StatusManualReview))

	w := testWorker(store, map[string]Channel{"slack": newFakeChannel()})
	w.tick(context.Background())

	if got := counterValue(t, metrics.OutboundLeasesExpiredTotal, FamilyNotification, string(StatusPending)) - pendingBefore; got != 2 {
		t.Errorf("outbound_leases_expired_total{to=pending} moved by %v, want 2", got)
	}
	if got := counterValue(t, metrics.OutboundLeasesExpiredTotal, FamilyNotification, string(StatusManualReview)) - reviewBefore; got != 1 {
		t.Errorf("outbound_leases_expired_total{to=manual_review} moved by %v, want 1", got)
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
