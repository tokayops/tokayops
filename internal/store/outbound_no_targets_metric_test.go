package store

import (
	"context"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The one alert that must not miss - "an alert had nobody to page" - reads a
// number counted from the claim rows, not from a process counter. A process
// counter loses an increment whenever the process dies between the commit and
// the Inc, and starts from zero on every restart; a claim row is never deleted,
// so the count of claims with that outcome only ever grows and is the same
// number from every process that asks.

func noTargetsOf(t *testing.T, snap *model.MetricsSnapshot, family string) (int, bool) {
	t.Helper()
	for _, row := range snap.OutboundNoTargetsAdmissions {
		if row.Family == family {
			return row.Count, true
		}
	}
	return 0, false
}

func TestNoTargetsAdmissionsAreCountedFromTheClaims(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Every family reports from the first scrape, zero included: increase()
	// over a series that appears on its first event has nothing to increase
	// from.
	empty, err := s.GetMetricsSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot on an empty database: %v", err)
	}
	for _, family := range outbound.Families() {
		if n, ok := noTargetsOf(t, empty, family); !ok || n != 0 {
			t.Errorf("no-targets admissions for %s on an empty database: %d (present: %t), want 0",
				family, n, ok)
		}
	}

	// No subscriber at all: the fan-out admits the event and promises nobody,
	// which is a claim with intent_count = 0.
	for i := 0; i < 2; i++ {
		alertEvent(t, s, "team-1", model.OutboxEventFiring)
		result, err := s.FanOutNextEvent(ctx)
		if err != nil || !result.Found || result.Commitments != 0 {
			t.Fatalf("fan-out with no subscribers: %+v (%v)", result, err)
		}
	}

	var claims int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_batches
		WHERE admission_outcome = 'no_targets' AND delivery_family = 'webhook'`).Scan(&claims); err != nil {
		t.Fatalf("count the claims: %v", err)
	}
	if claims != 2 {
		t.Fatalf("%d no-targets claims after two fan-outs with no subscribers, want 2", claims)
	}

	snap, err := s.GetMetricsSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if n, _ := noTargetsOf(t, snap, outbound.FamilyWebhook); n != claims {
		t.Errorf("the snapshot says %d no-targets admissions for webhook, the claims say %d", n, claims)
	}
	if n, _ := noTargetsOf(t, snap, outbound.FamilyNotification); n != 0 {
		t.Errorf("the snapshot says %d no-targets admissions for notification, want 0", n)
	}

	// What the collector emits: a COUNTER, so increase() reads it as one, and
	// the same value from a second, fresh collector - nothing in the process
	// carries it.
	for round := 0; round < 2; round++ {
		gathered := families(t, s)
		mf := gathered["outbound_no_targets_admissions_total"]
		if mf == nil {
			t.Fatalf("round %d: outbound_no_targets_admissions_total was not emitted", round)
		}
		if mf.GetType() != dto.MetricType_COUNTER {
			t.Fatalf("round %d: outbound_no_targets_admissions_total is a %s, not a counter", round, mf.GetType())
		}
		found := false
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "family" && l.GetValue() == outbound.FamilyWebhook {
					found = true
					if got := m.GetCounter().GetValue(); got != float64(claims) {
						t.Errorf("round %d: outbound_no_targets_admissions_total{family=webhook} = %v, want %d",
							round, got, claims)
					}
				}
			}
		}
		if !found {
			t.Fatalf("round %d: no series for the webhook family", round)
		}
	}
}
