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
//
// All three families are driven through their real admission doors, with a
// different count each, because the rule is written against the notification
// family and a test that only ever produced webhook claims would stay green on
// a query that counted webhooks alone.

func noTargetsOf(t *testing.T, snap *model.MetricsSnapshot, family string) (int, bool) {
	t.Helper()
	for _, row := range snap.OutboundNoTargetsAdmissions {
		if row.Family == family {
			return row.Count, true
		}
	}
	return 0, false
}

func gatheredNoTargets(t *testing.T, s *Store) map[string]float64 {
	t.Helper()
	mf := families(t, s)["outbound_no_targets_admissions_total"]
	if mf == nil {
		t.Fatal("outbound_no_targets_admissions_total was not emitted")
	}
	if mf.GetType() != dto.MetricType_COUNTER {
		t.Fatalf("outbound_no_targets_admissions_total is a %s, not a counter", mf.GetType())
	}
	values := map[string]float64{}
	for _, m := range mf.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "family" {
				values[l.GetValue()] = m.GetCounter().GetValue()
			}
		}
	}
	return values
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
	for family, value := range gatheredNoTargets(t, s) {
		if value != 0 {
			t.Errorf("emitted %v for %s on an empty database", value, family)
		}
	}

	// notification, one: an escalation whose every step resolved to nobody.
	adm := outboundAdmission(t, outboundGroup(t, s), "first")
	adm = withEscalation(adm, func(about *outbound.EscalationContext) {
		about.Unpromised = []outbound.UnpromisedStep{{
			Step: "schedule sched-1", Reason: outbound.ReasonNobodyOnCall,
		}}
	})
	if result := mustSubmit(t, s, adm); result.Outcome != outbound.SubmitCreated || len(result.IntentIDs) != 0 {
		t.Fatalf("an escalation to nobody answered %q with %d commitments", result.Outcome, len(result.IntentIDs))
	}

	// handoff, two: announcements of shift changes with nobody to tell.
	for _, schedule := range []string{"sched-empty-1", "sched-empty-2"} {
		result, err := s.SubmitBatch(ctx, handoffBatch(t, schedule))
		if err != nil || result.Outcome != outbound.SubmitCreated || len(result.IntentIDs) != 0 {
			t.Fatalf("an announcement to nobody (%s): %+v (%v)", schedule, result, err)
		}
	}

	// webhook, three: events fanned out with no subscriber in scope.
	for i := 0; i < 3; i++ {
		alertEvent(t, s, "team-1", model.OutboxEventFiring)
		result, err := s.FanOutNextEvent(ctx)
		if err != nil || !result.Found || result.Commitments != 0 {
			t.Fatalf("fan-out with no subscribers: %+v (%v)", result, err)
		}
	}

	want := map[string]int{
		outbound.FamilyNotification: 1,
		outbound.FamilyHandoff:      2,
		outbound.FamilyWebhook:      3,
	}
	for family, expected := range want {
		var claims int
		if err := s.db.QueryRow(`SELECT count(*) FROM outbound_batches
			WHERE admission_outcome = 'no_targets' AND delivery_family = $1`, family).Scan(&claims); err != nil {
			t.Fatalf("count the %s claims: %v", family, err)
		}
		if claims != expected {
			t.Fatalf("%d no-targets claims for %s, the doors were used %d times", claims, family, expected)
		}
	}

	snap, err := s.GetMetricsSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for family, expected := range want {
		if n, ok := noTargetsOf(t, snap, family); !ok || n != expected {
			t.Errorf("the snapshot says %d no-targets admissions for %s (present: %t), want %d", n, family, ok, expected)
		}
	}

	// What the collector emits: a COUNTER, so increase() reads it as one, with
	// the exact value per family - and the same values from a second, fresh
	// collector, because nothing in the process carries them.
	for round := 0; round < 2; round++ {
		values := gatheredNoTargets(t, s)
		for family, expected := range want {
			if got, ok := values[family]; !ok || got != float64(expected) {
				t.Errorf("round %d: outbound_no_targets_admissions_total{family=%q} = %v (present: %t), want %d",
					round, family, got, ok, expected)
			}
		}
	}
}
