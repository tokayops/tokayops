package store

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
)

func TestOutboxStore(t *testing.T) {
	if testStore == nil {
		t.Skip("TEST_DB_DSN not set")
	}

	// Setup encryption key for integration creation in delivery subtests
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	// t.Setenv, not os.Setenv + defer os.Unsetenv: the latter does not restore
	// the previous value, it deletes the variable. TestMain sets a default key
	// for the whole package, so unsetting it here left every later test that
	// needs one failing - invisibly in declaration order, and reproducibly
	// under -shuffle.
	t.Setenv(config.EncryptionKeyEnv, hex.EncodeToString(key))

	t.Run("create and get event", func(t *testing.T) {
		s := setupTestDB(t)

		// Need an alert group and team for FK
		s.CreateTeam(&model.Team{ID: "test-team", Name: "Test"})
		ag := &model.AlertGroup{
			ID:               "ag-outbox-1",
			AlertKey:         "dedup-outbox-1",
			Status:           model.AlertGroupStatusNew,
			Title:            "Test AG",
			TeamID:           "test-team",
			TeamNameSnapshot: "Test",
			Severity:         "critical",
		}
		s.CreateAlertGroup(ag)

		payload, _ := json.Marshal(map[string]string{"title": "Test AG"})
		event := &model.OutboxEvent{
			EventType:    model.OutboxEventFiring,
			AlertGroupID: "ag-outbox-1",
			TeamID:       "test-team",
			Actor:        "system",
			Payload:      payload,
		}

		if err := s.CreateOutboxEvent(event); err != nil {
			t.Fatalf("CreateOutboxEvent failed: %v", err)
		}
		if event.ID == "" {
			t.Error("Expected ID to be set")
		}
		if event.Status != model.OutboxEventStatusPending {
			t.Errorf("Expected pending status, got %s", event.Status)
		}

		fetched, err := s.GetOutboxEventByID(event.ID)
		if err != nil {
			t.Fatalf("GetOutboxEventByID failed: %v", err)
		}
		if fetched.EventType != model.OutboxEventFiring {
			t.Errorf("Expected firing event type, got %s", fetched.EventType)
		}
		if fetched.AlertGroupID != "ag-outbox-1" {
			t.Errorf("Expected ag-outbox-1, got %s", fetched.AlertGroupID)
		}
	})

	t.Run("get pending events", func(t *testing.T) {
		s := setupTestDB(t)

		s.CreateTeam(&model.Team{ID: "test-team", Name: "Test"})
		for i := 0; i < 3; i++ {
			ag := &model.AlertGroup{
				AlertKey:         "dedup-pend-" + string(rune('0'+i)),
				Status:           model.AlertGroupStatusNew,
				Title:            "AG",
				TeamID:           "test-team",
				TeamNameSnapshot: "Test",
				Severity:         "warning",
			}
			s.CreateAlertGroup(ag)

			event := &model.OutboxEvent{
				EventType:    model.OutboxEventFiring,
				AlertGroupID: ag.ID,
				TeamID:       "test-team",
				Payload:      json.RawMessage(`{}`),
			}
			s.CreateOutboxEvent(event)

			// The third is finished: fanned out by the domain.
			if i == 2 {
				if _, err := s.db.Exec(`UPDATE event_outbox SET status = 'fanned_out' WHERE id = $1`, event.ID); err != nil {
					t.Fatal(err)
				}
			}
		}

		pending, err := s.GetPendingOutboxEvents(10)
		if err != nil {
			t.Fatalf("GetPendingOutboxEvents failed: %v", err)
		}
		if len(pending) != 2 {
			t.Errorf("Expected 2 pending events, got %d", len(pending))
		}
	})

}

func TestCreateAlertGroupAtomic_Mock(t *testing.T) {
	m := NewMockStore()

	agID := "ag-atomic-mock"
	ag := &model.AlertGroup{
		ID:               agID,
		AlertKey:         "key-atomic",
		Status:           model.AlertGroupStatusNew,
		Title:            "Test AG",
		TeamID:           "team-1",
		TeamNameSnapshot: "Team 1",
		Severity:         "critical",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	now := time.Now()
	timelineEvents := []*model.TimelineEvent{
		{
			ID:           "tl-1",
			AlertGroupID: agID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created: Test AG",
			Actor:        "system",
			CreatedAt:    now,
		},
		{
			ID:           "tl-2",
			AlertGroupID: agID,
			Type:         model.TimelineEventAlertAdded,
			Message:      "Alert: TestAlert",
			Actor:        "system",
			Metadata:     map[string]string{"fingerprint": "fp1"},
			CreatedAt:    now,
		},
	}

	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventFiring,
		AlertGroupID: agID,
		TeamID:       "team-1",
		Actor:        "system",
	}

	if err := m.CreateAlertGroupAtomic(ag, timelineEvents, outboxEvent); err != nil {
		t.Fatalf("CreateAlertGroupAtomic: %v", err)
	}

	// Verify AG
	fetched, err := m.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if fetched.Status != model.AlertGroupStatusNew {
		t.Errorf("Expected status 'new', got '%s'", fetched.Status)
	}

	// Verify timeline events
	events, err := m.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("Expected 2 timeline events, got %d", len(events))
	}

	// Verify outbox event
	if outboxEvent.ID == "" {
		t.Fatal("Expected outbox event ID to be set")
	}
	outbox, err := m.GetOutboxEventByID(outboxEvent.ID)
	if err != nil {
		t.Fatalf("GetOutboxEventByID: %v", err)
	}
	if outbox.EventType != model.OutboxEventFiring {
		t.Errorf("Expected event type 'alert_group.firing', got '%s'", outbox.EventType)
	}
	if outbox.Status != model.OutboxEventStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", outbox.Status)
	}
	// Verify nil Payload normalized to {}
	if string(outbox.Payload) != "{}" {
		t.Errorf("Expected payload '{}', got '%s'", string(outbox.Payload))
	}
}

func TestOutboxStoreMock(t *testing.T) {
	t.Run("future next_attempt_at excluded", func(t *testing.T) {
		m := NewMockStore()

		future := time.Now().Add(1 * time.Hour)
		event := &model.OutboxEvent{
			EventType:     model.OutboxEventFiring,
			AlertGroupID:  "ag-1",
			TeamID:        "t-1",
			Payload:       json.RawMessage(`{}`),
			NextAttemptAt: &future,
		}
		if err := m.CreateOutboxEvent(event); err != nil {
			t.Fatalf("CreateOutboxEvent: %v", err)
		}

		pending, err := m.GetPendingOutboxEvents(10)
		if err != nil {
			t.Fatalf("GetPendingOutboxEvents: %v", err)
		}
		if len(pending) != 0 {
			t.Errorf("Expected 0 pending events, got %d", len(pending))
		}
	})

	t.Run("past next_attempt_at included", func(t *testing.T) {
		m := NewMockStore()

		past := time.Now().Add(-1 * time.Hour)
		event := &model.OutboxEvent{
			EventType:     model.OutboxEventFiring,
			AlertGroupID:  "ag-1",
			TeamID:        "t-1",
			Payload:       json.RawMessage(`{}`),
			NextAttemptAt: &past,
		}
		if err := m.CreateOutboxEvent(event); err != nil {
			t.Fatalf("CreateOutboxEvent: %v", err)
		}

		pending, err := m.GetPendingOutboxEvents(10)
		if err != nil {
			t.Fatalf("GetPendingOutboxEvents: %v", err)
		}
		if len(pending) != 1 {
			t.Errorf("Expected 1 pending event, got %d", len(pending))
		}
	})

	t.Run("pending events ordered nulls first", func(t *testing.T) {
		m := NewMockStore()

		twoAgo := time.Now().Add(-2 * time.Minute)
		oneAgo := time.Now().Add(-1 * time.Minute)

		e1 := &model.OutboxEvent{
			EventType:     model.OutboxEventFiring,
			AlertGroupID:  "ag-1",
			TeamID:        "t-1",
			Payload:       json.RawMessage(`{}`),
			NextAttemptAt: nil, // nil sorts first
		}
		e2 := &model.OutboxEvent{
			EventType:     model.OutboxEventFiring,
			AlertGroupID:  "ag-2",
			TeamID:        "t-1",
			Payload:       json.RawMessage(`{}`),
			NextAttemptAt: &twoAgo,
		}
		e3 := &model.OutboxEvent{
			EventType:     model.OutboxEventFiring,
			AlertGroupID:  "ag-3",
			TeamID:        "t-1",
			Payload:       json.RawMessage(`{}`),
			NextAttemptAt: &oneAgo,
		}

		for _, e := range []*model.OutboxEvent{e1, e2, e3} {
			if err := m.CreateOutboxEvent(e); err != nil {
				t.Fatalf("CreateOutboxEvent: %v", err)
			}
		}

		pending, err := m.GetPendingOutboxEvents(10)
		if err != nil {
			t.Fatalf("GetPendingOutboxEvents: %v", err)
		}
		if len(pending) != 3 {
			t.Fatalf("Expected 3 pending events, got %d", len(pending))
		}

		wantIDs := []string{e1.ID, e2.ID, e3.ID}
		for i, want := range wantIDs {
			if pending[i].ID != want {
				t.Errorf("pending[%d].ID = %s, want %s", i, pending[i].ID, want)
			}
		}
	})
}
