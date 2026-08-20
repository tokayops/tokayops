package store

import (
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
	RaiseSlackUpdate(id string) error
	ClearSlackUpdate(id string, observedGeneration int64) (bool, error)
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
				if err := s.RaiseSlackUpdate(id); err != nil {
					t.Fatalf("RaiseSlackUpdate: %v", err)
				}
			}

			read, err := s.GetAlertGroupByID(id)
			if err != nil {
				t.Fatalf("GetAlertGroupByID: %v", err)
			}
			if !read.SlackUpdatePending {
				t.Fatal("the gate is down after an alert raised it")
			}

			for i := 0; i < tc.raisesAfterRead; i++ {
				if err := s.RaiseSlackUpdate(id); err != nil {
					t.Fatalf("RaiseSlackUpdate: %v", err)
				}
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
				if err := s.RaiseSlackUpdate(id); err != nil {
					t.Fatalf("RaiseSlackUpdate: %v", err)
				}
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
