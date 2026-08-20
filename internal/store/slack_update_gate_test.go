package store

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// The gate that says "this group's message is out of date" is raised by the
// ingester and lowered by the update producer, and the two do not run in one
// transaction. Everything below is about the gap between them.
//
// One table, both implementations, for the same reason as the dedup rules: the
// producer is written against the double and runs against the database.
var slackUpdateGateCases = []struct {
	name string

	// raisesBeforeRead is what the producer sees when it lists the groups
	// waiting for an update; raisesAfterRead is what arrives while it works.
	raisesBeforeRead int
	raisesAfterRead  int

	wantCleared bool
	wantPending bool
}{
	{
		name:             "a clear for the version that was read lowers the gate",
		raisesBeforeRead: 1,
		wantCleared:      true,
		wantPending:      false,
	},
	{
		// The lost wake-up: this alert reached the group after the update job
		// was admitted, so nothing has rendered it yet. Lowering the gate here
		// is how it used to disappear.
		name:             "an alert arriving while the update is created keeps the gate up",
		raisesBeforeRead: 1,
		raisesAfterRead:  1,
		wantCleared:      false,
		wantPending:      true,
	},
	{
		name:             "several alerts arriving in that gap keep it up as well",
		raisesBeforeRead: 2,
		raisesAfterRead:  3,
		wantCleared:      false,
		wantPending:      true,
	},
}

// slackUpdateGate is the part of a store this asks about, so the same case can
// be put to the database and to the double.
type slackUpdateGate interface {
	CreateAlertGroup(ag *model.AlertGroup) error
	GetAlertGroupByID(id string) (*model.AlertGroup, error)
	UpdateAlertGroupAlertsAndRaiseSlackUpdate(id string, alerts []model.Alert) error
	ClearSlackUpdate(id string, observedGeneration int64) (bool, error)
}

// raise is what the ingester does when an alert changes the group: the alerts
// and the gate move in one write. The tests below raise the gate that way and
// not through a flag setter, because a flag setter is not a thing this store
// has - separating the two is what lost an alert.
func raise(t *testing.T, s slackUpdateGate, id string, alertNames ...string) {
	t.Helper()
	alerts := make([]model.Alert, 0, len(alertNames))
	for _, name := range alertNames {
		alerts = append(alerts, model.Alert{
			Fingerprint: name,
			Status:      model.AlertStatusFiring,
			Labels:      map[string]string{"alertname": name},
		})
	}
	if err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(id, alerts); err != nil {
		t.Fatalf("record alerts and raise the gate: %v", err)
	}
}

func runSlackUpdateGateCases(t *testing.T, newStore func(t *testing.T) slackUpdateGate) {
	t.Helper()

	for _, tc := range slackUpdateGateCases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)

			id := uuid.New().String()
			if err := s.CreateAlertGroup(&model.AlertGroup{
				ID:       id,
				AlertKey: "dk-" + id,
				Status:   model.AlertGroupStatusProcessing,
				Title:    "gate",
				Severity: "info",
			}); err != nil {
				t.Fatalf("CreateAlertGroup: %v", err)
			}

			for i := 0; i < tc.raisesBeforeRead; i++ {
				raise(t, s, id, "alert-a")
			}

			read, err := s.GetAlertGroupByID(id)
			if err != nil {
				t.Fatalf("GetAlertGroupByID: %v", err)
			}
			if !read.SlackUpdatePending {
				t.Fatal("the gate is down after an alert raised it")
			}

			for i := 0; i < tc.raisesAfterRead; i++ {
				raise(t, s, id, "alert-a", "alert-b")
			}

			cleared, err := s.ClearSlackUpdate(id, read.SlackUpdateGeneration)
			if err != nil {
				t.Fatalf("ClearSlackUpdate: %v", err)
			}
			if cleared != tc.wantCleared {
				t.Errorf("cleared = %v, want %v", cleared, tc.wantCleared)
			}

			after, err := s.GetAlertGroupByID(id)
			if err != nil {
				t.Fatalf("GetAlertGroupByID: %v", err)
			}
			if after.SlackUpdatePending != tc.wantPending {
				t.Errorf("gate up = %v, want %v", after.SlackUpdatePending, tc.wantPending)
			}
		})
	}
}

func TestSlackUpdateGate_Store(t *testing.T) {
	runSlackUpdateGateCases(t, func(t *testing.T) slackUpdateGate { return setupTestDB(t) })
}

func TestSlackUpdateGate_MockStore(t *testing.T) {
	runSlackUpdateGateCases(t, func(t *testing.T) slackUpdateGate { return NewMockStore() })
}

// TestSlackUpdateGateMovesWithTheAlerts: the alerts and the gate move in one
// write, so there is no moment in which the group holds a change its message is
// not marked as missing.
//
// Two writes had such a moment, and a process that stopped in it lost the alert
// for good: Alertmanager repeating the payload finds the alerts already
// recorded, merges nothing, and never raises the gate again.
func TestSlackUpdateGateMovesWithTheAlerts(t *testing.T) {
	for _, impl := range []struct {
		name  string
		build func(t *testing.T) slackUpdateGate
	}{
		{"Store", func(t *testing.T) slackUpdateGate { return setupTestDB(t) }},
		{"MockStore", func(t *testing.T) slackUpdateGate { return NewMockStore() }},
	} {
		t.Run(impl.name, func(t *testing.T) {
			s := impl.build(t)

			id := uuid.New().String()
			if err := s.CreateAlertGroup(&model.AlertGroup{
				ID: id, AlertKey: "dk-" + id, Status: model.AlertGroupStatusProcessing,
				Title: "gate", Severity: "info",
			}); err != nil {
				t.Fatalf("CreateAlertGroup: %v", err)
			}

			raise(t, s, id, "alert-a", "alert-b")

			ag, err := s.GetAlertGroupByID(id)
			if err != nil {
				t.Fatalf("GetAlertGroupByID: %v", err)
			}
			if len(ag.Alerts) != 2 {
				t.Fatalf("the group holds %d alerts, want the two just recorded", len(ag.Alerts))
			}
			if !ag.SlackUpdatePending {
				t.Error("the alerts were recorded with the gate down")
			}
			if ag.SlackUpdateGeneration == 0 {
				t.Error("the version did not move with the alerts")
			}
		})
	}
}

// TestSlackUpdateGateOnAGroupThatIsNotThere: the two halves answer a missing
// group differently, and each answer is the one its caller can act on.
//
// The write must land somewhere - a caller told nothing would answer its
// webhook with 200 for an alert it dropped - so it reports that it did not.
// The conditional clear has no such duty: nothing was lowered, which is the
// same answer it gives for a group that has moved on.
func TestSlackUpdateGateOnAGroupThatIsNotThere(t *testing.T) {
	for _, impl := range []struct {
		name  string
		build func(t *testing.T) slackUpdateGate
	}{
		{"Store", func(t *testing.T) slackUpdateGate { return setupTestDB(t) }},
		{"MockStore", func(t *testing.T) slackUpdateGate { return NewMockStore() }},
	} {
		t.Run(impl.name, func(t *testing.T) {
			s := impl.build(t)
			missing := uuid.New().String()

			err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(missing, []model.Alert{{Fingerprint: "fp"}})
			if !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("recording alerts for a group that is not there returned %v, want sql.ErrNoRows", err)
			}

			cleared, err := s.ClearSlackUpdate(missing, 1)
			if err != nil {
				t.Errorf("lowering the gate of a group that is not there returned %v, want no error", err)
			}
			if cleared {
				t.Error("a gate was lowered on a group that is not there")
			}
		})
	}
}

// The version only ever goes up, and it goes up on every alert. Were a raise to
// leave it where it was, two alerts in the same gap would look like one and the
// second would be cleared away with the first.
func TestSlackUpdateGateIsMonotonic(t *testing.T) {
	for _, impl := range []struct {
		name  string
		build func(t *testing.T) slackUpdateGate
	}{
		{"Store", func(t *testing.T) slackUpdateGate { return setupTestDB(t) }},
		{"MockStore", func(t *testing.T) slackUpdateGate { return NewMockStore() }},
	} {
		t.Run(impl.name, func(t *testing.T) {
			s := impl.build(t)

			id := uuid.New().String()
			if err := s.CreateAlertGroup(&model.AlertGroup{
				ID:       id,
				AlertKey: "dk-" + id,
				Status:   model.AlertGroupStatusProcessing,
				Title:    "gate",
				Severity: "info",
			}); err != nil {
				t.Fatalf("CreateAlertGroup: %v", err)
			}

			var seen []int64
			for i := 0; i < 3; i++ {
				raise(t, s, id, "alert-a")
				ag, err := s.GetAlertGroupByID(id)
				if err != nil {
					t.Fatalf("GetAlertGroupByID: %v", err)
				}
				seen = append(seen, ag.SlackUpdateGeneration)
			}

			for i := 1; i < len(seen); i++ {
				if seen[i] <= seen[i-1] {
					t.Fatalf("generations %v do not increase; two alerts share a version", seen)
				}
			}

			// Clearing the gate does not move it: the version belongs to the
			// alerts, not to the answer.
			if _, err := s.ClearSlackUpdate(id, seen[len(seen)-1]); err != nil {
				t.Fatalf("ClearSlackUpdate: %v", err)
			}
			ag, err := s.GetAlertGroupByID(id)
			if err != nil {
				t.Fatalf("GetAlertGroupByID: %v", err)
			}
			if ag.SlackUpdateGeneration != seen[len(seen)-1] {
				t.Errorf("generation = %d after a clear, want it untouched at %d",
					ag.SlackUpdateGeneration, seen[len(seen)-1])
			}
		})
	}
}
