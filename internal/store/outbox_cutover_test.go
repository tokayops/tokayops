package store

import (
	"context"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
)

// The old webhook outbox is gone from the start-up schema, its two tables are
// removed by a file a person runs, and the metrics snapshot no longer depends
// on a table that is not there.

func relation(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var exists bool
	if err := s.db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("look up %s: %v", name, err)
	}
	return exists
}

// families gathers the business collector over the real store once.
func families(t *testing.T, s *Store) map[string]*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewRegistry()
	metrics.RegisterCollectorWith(reg, s)
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, mf := range gathered {
		byName[mf.GetName()] = mf
	}
	return byName
}

func hasLabel(mf *dto.MetricFamily, name, value string) bool {
	if mf == nil {
		return false
	}
	for _, m := range mf.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == name && l.GetValue() == value {
				return true
			}
		}
	}
	return false
}

// TestAFreshDatabaseHasNoDeliveryTablesAndTheSnapshotStillAnswers: the start
// creates the event table and neither of the old worker's; and the collector,
// which answers a failed snapshot with nothing at all, has every series it
// promises on a database with no rows - and the event and commitment series as
// soon as there is an event.
func TestAFreshDatabaseHasNoDeliveryTablesAndTheSnapshotStillAnswers(t *testing.T) {
	s := setupTestDB(t)
	if !relation(t, s, "event_outbox") {
		t.Fatal("the event table is gone")
	}
	for _, gone := range []string{"event_outbox_deliveries", "event_outbox_delivery_attempts",
		"idx_delivery_retry", "idx_delivery_event", "idx_delivery_attempts_delivery"} {
		if relation(t, s, gone) {
			t.Errorf("the start still creates %s", gone)
		}
	}

	if _, err := s.GetMetricsSnapshot(context.Background()); err != nil {
		t.Fatalf("the snapshot fails on a fresh database: %v", err)
	}
	empty := families(t, s)
	lateness := empty["outbound_queue_lateness_seconds"]
	for _, family := range []string{"notification", "handoff", "webhook"} {
		if !hasLabel(lateness, "family", family) {
			t.Errorf("no lateness series for %s on a fresh database", family)
		}
	}

	subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	after := families(t, s)
	if !hasLabel(after["outbox_events_by_status"], "status", "fanned_out") {
		t.Errorf("no outbox_events_by_status{fanned_out}: %v", after["outbox_events_by_status"])
	}
	if !hasLabel(after["outbound_intents_by_status"], "family", "webhook") {
		t.Errorf("no outbound_intents_by_status for the webhook family: %v", after["outbound_intents_by_status"])
	}
	if _, present := after["outbox_deliveries_by_status"]; present {
		t.Error("the collector still emits the old delivery gauge")
	}
}

// TestTheCutoverFileRemovesWhatTheStartNoLongerCreates: against the exact shape
// of the previous release - both tables, both keys, the indexes and rows in
// them - the file removes the tables and the events the old worker finished,
// keeps the events still owed, and a full start afterwards brings nothing back.
func TestTheCutoverFileRemovesWhatTheStartNoLongerCreates(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	pending := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	completed := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	failed := alertEvent(t, s, "team-1", model.OutboxEventResolved)
	claimed := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	for eventID, status := range map[string]string{completed: "completed", failed: "failed", claimed: "processing"} {
		if _, err := s.db.Exec(`UPDATE event_outbox SET status = $2 WHERE id = $1`, eventID, status); err != nil {
			t.Fatal(err)
		}
	}
	sprint3OutboundShape(t, s)
	if _, err := s.db.Exec(`INSERT INTO event_outbox_deliveries (id, event_id, integration_id, status, attempts)
		VALUES ('legacy-d', $1, $2, 'sent', 1)`, completed, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO event_outbox_delivery_attempts (id, delivery_id, attempt, http_status)
		VALUES ('legacy-a', 'legacy-d', 0, 200)`); err != nil {
		t.Fatal(err)
	}
	for _, present := range []string{"event_outbox_deliveries", "event_outbox_delivery_attempts",
		"idx_delivery_retry", "idx_delivery_event", "idx_delivery_attempts_delivery"} {
		if !relation(t, s, present) {
			t.Fatalf("the previous shape lacks %s: the file would be applied to nothing", present)
		}
	}
	if !hasNamedConstraint(t, s, "event_outbox_deliveries", "event_outbox_deliveries_integration_id_fkey") {
		t.Fatal("the previous shape lacks the key to integrations")
	}

	file, err := os.ReadFile("../../migrations/drop-webhook-outbox.sql")
	if err != nil {
		t.Fatalf("read the cutover file: %v", err)
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(string(file)); err != nil {
		tx.Rollback()
		t.Fatalf("apply the cutover file: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, gone := range []string{"event_outbox_deliveries", "event_outbox_delivery_attempts",
		"idx_delivery_retry", "idx_delivery_event", "idx_delivery_attempts_delivery"} {
		if relation(t, s, gone) {
			t.Errorf("%s survived the cutover file", gone)
		}
	}
	statuses := map[string]string{}
	rows, err := s.db.Query(`SELECT id, status FROM event_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		statuses[id] = status
	}
	rows.Close()
	if len(statuses) != 2 || statuses[pending] != "pending" || statuses[claimed] != "processing" {
		t.Fatalf("after the cutover the events are %v: want the pending and the claimed one kept, the finished ones gone", statuses)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("start after the cutover: %v", err)
	}
	for _, gone := range []string{"event_outbox_deliveries", "event_outbox_delivery_attempts"} {
		if relation(t, s, gone) {
			t.Errorf("the start brought %s back", gone)
		}
	}
	// And the events the file kept are the fan-out's to take.
	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found {
		t.Fatalf("the fan-out after the cutover: %+v (%v)", result, err)
	}
}
