package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/model"
)

// A store that only answers when the scrape gives up: what a database that has
// stopped answering looks like from here.
type hangingSource struct{ entered chan struct{} }

func (h hangingSource) GetMetricsSnapshot(ctx context.Context) (*model.MetricsSnapshot, error) {
	close(h.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestTheCollectorGivesUpOnTheSnapshotAndSaysSo: the scrape's deadline is the
// collector's, not Prometheus's. A store that does not answer within it gets
// its context cancelled, the scrape reports no business series at all - not a
// partial set with zeroes in it - and the gather returns in about the
// timeout, not whenever the database feels like it.
func TestTheCollectorGivesUpOnTheSnapshotAndSaysSo(t *testing.T) {
	previous := snapshotTimeout
	snapshotTimeout = 150 * time.Millisecond
	t.Cleanup(func() { snapshotTimeout = previous })

	reg := prometheus.NewRegistry()
	source := hangingSource{entered: make(chan struct{})}
	RegisterCollectorWith(reg, source)

	started := time.Now()
	gathered, err := reg.Gather()
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	select {
	case <-source.entered:
	default:
		t.Fatal("the collector never asked the store")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the gather waited %s for a store that never answers", elapsed)
	}
	for _, mf := range gathered {
		t.Errorf("a series was reported from a snapshot that never came: %s", mf.GetName())
	}
}

// TestTheNoTargetsAdmissionsAreACounterReadFromTheSnapshot: the durable twin of
// outbound_admissions_total{outcome="no_targets"} is emitted as a COUNTER, so
// that increase() over it means what it means over any counter, and it is
// emitted for every family the snapshot lists, zero included.
func TestTheNoTargetsAdmissionsAreACounterReadFromTheSnapshot(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterCollectorWith(reg, snapshotOf{snap: &model.MetricsSnapshot{
		OutboundNoTargetsAdmissions: []model.OutboundFamilyCount{
			{Family: "notification", Count: 3},
			{Family: "handoff", Count: 0},
			{Family: "webhook", Count: 12},
		},
	}})

	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found *dto.MetricFamily
	for _, mf := range gathered {
		if mf.GetName() == "outbound_no_targets_admissions_total" {
			found = mf
		}
	}
	if found == nil {
		t.Fatal("outbound_no_targets_admissions_total was not emitted")
	}
	if found.GetType() != dto.MetricType_COUNTER {
		t.Fatalf("outbound_no_targets_admissions_total is a %s; increase() needs a counter", found.GetType())
	}
	want := map[string]float64{"notification": 3, "handoff": 0, "webhook": 12}
	for _, m := range found.GetMetric() {
		var family string
		for _, l := range m.GetLabel() {
			if l.GetName() == "family" {
				family = l.GetValue()
			}
		}
		value := m.GetCounter().GetValue()
		if expected, ok := want[family]; !ok || expected != value {
			t.Errorf("outbound_no_targets_admissions_total{family=%q} = %v, want %v", family, value, expected)
		}
		delete(want, family)
	}
	for family := range want {
		t.Errorf("no series for family %q", family)
	}
}

type snapshotOf struct{ snap *model.MetricsSnapshot }

func (s snapshotOf) GetMetricsSnapshot(context.Context) (*model.MetricsSnapshot, error) {
	return s.snap, nil
}
