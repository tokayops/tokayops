package alertgroup

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func createAG(t *testing.T, s *store.MockStore, id string, status model.AlertGroupStatus) *model.AlertGroup {
	t.Helper()
	ag := &model.AlertGroup{
		ID:               id,
		AlertKey:         "dedup-" + id,
		Status:           status,
		Title:            "Test Alert",
		TeamID:           "devops",
		TeamNameSnapshot: "DevOps",
		Severity:         "critical",
		CreatedAt:        time.Now().Add(-5 * time.Minute),
		UpdatedAt:        time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup: %v", err)
	}
	return ag
}

func TestAck_Applied(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-1", model.AlertGroupStatusTriggered)

	result, err := svc.Ack("ag-1", Actor{Name: "Denis", Email: "denis@example.com"}, nil)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if result.Outcome != OutcomeApplied {
		t.Errorf("Expected applied, got %s", result.Outcome)
	}
	if result.AlertGroup.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected acknowledged, got %s", result.AlertGroup.Status)
	}
	if result.AlertGroup.AcknowledgedBy != "Denis" {
		t.Errorf("Expected acknowledged_by Denis, got %s", result.AlertGroup.AcknowledgedBy)
	}

	// Verify outbox event created
	events, _ := s.GetPendingOutboxEvents(10)
	var found *model.OutboxEvent
	for _, ev := range events {
		if ev.AlertGroupID == "ag-1" && ev.EventType == model.OutboxEventAcknowledged {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("Expected outbox event")
	}

	// Verify timeline event
	timeline, _ := s.GetTimelineEvents("ag-1")
	ackCount := 0
	for _, ev := range timeline {
		if ev.Type == model.TimelineEventAcknowledged {
			ackCount++
		}
	}
	if ackCount != 1 {
		t.Errorf("Expected 1 ack timeline event, got %d", ackCount)
	}
}

func TestAck_AlreadyDone(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-2", model.AlertGroupStatusAcknowledged)

	result, err := svc.Ack("ag-2", Actor{Name: "Denis"}, nil)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if result.Outcome != OutcomeAlreadyDone {
		t.Errorf("Expected already_done, got %s", result.Outcome)
	}
	if result.AlertGroup == nil {
		t.Fatal("Expected AlertGroup on already_done")
	}

	// Verify no outbox event
	events, _ := s.GetPendingOutboxEvents(10)
	for _, ev := range events {
		if ev.AlertGroupID == "ag-2" {
			t.Error("Expected no outbox event for already-done ack")
		}
	}
}

func TestAck_NotFound(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)

	result, err := svc.Ack("nonexistent", Actor{Name: "Denis"}, nil)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if result.Outcome != OutcomeNotFound {
		t.Errorf("Expected not_found, got %s", result.Outcome)
	}
	if result.AlertGroup != nil {
		t.Error("Expected nil AlertGroup on not_found")
	}
}

func TestAck_CASRaceLoser(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-race", model.AlertGroupStatusTriggered)

	// First ack wins
	result1, _ := svc.Ack("ag-race", Actor{Name: "Alice"}, nil)
	if result1.Outcome != OutcomeApplied {
		t.Fatalf("First ack should apply, got %s", result1.Outcome)
	}

	// Second ack is a race loser
	result2, err := svc.Ack("ag-race", Actor{Name: "Bob"}, nil)
	if err != nil {
		t.Fatalf("Second ack: %v", err)
	}
	if result2.Outcome != OutcomeAlreadyDone {
		t.Errorf("Expected already_done for race loser, got %s", result2.Outcome)
	}
}

func TestAck_UsesTeamNameSnapshot(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-snap", model.AlertGroupStatusTriggered)

	result, err := svc.Ack("ag-snap", Actor{Name: "Denis", Email: "denis@example.com"}, nil)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if result.Outcome != OutcomeApplied {
		t.Fatalf("Expected applied, got %s", result.Outcome)
	}

	// Verify outbox payload uses snapshot name
	events, _ := s.GetPendingOutboxEvents(10)
	for _, ev := range events {
		if ev.AlertGroupID == "ag-snap" && ev.EventType == model.OutboxEventAcknowledged {
			var payload model.WebhookEventPayload
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if payload.AlertGroup.TeamName != "DevOps" {
				t.Errorf("Expected team_name 'DevOps' from snapshot, got %q", payload.AlertGroup.TeamName)
			}
			return
		}
	}
	t.Fatal("Outbox event not found")
}

func TestAck_CancelsEscalation(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-cancel", model.AlertGroupStatusTriggered)

	// A pending escalation job shaped like the real one: cancellation addresses
	// it by alert group, so a fixture without alert_group_id would be a job the
	// escalation builder never produces.
	agID := "ag-cancel"
	if err := s.SeedEscalationJob(agID, &model.Job{
		ID:      "job-ag-cancel",
		Status:  model.JobStatusPending,
		Payload: json.RawMessage("{}"),
	}, nil, nil); err != nil {
		t.Fatalf("SeedEscalationJob: %v", err)
	}

	result, err := svc.Ack("ag-cancel", Actor{Name: "Denis"}, nil)
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if result.Outcome != OutcomeApplied {
		t.Fatalf("Expected applied, got %s", result.Outcome)
	}

	// Verify job was cancelled
	job, err := s.FindJobByIdentity(jobdedup.Escalation(agID))
	if err != nil || job == nil {
		t.Fatalf("escalation job not found after ack: %v", err)
	}
	if job.Status != model.JobStatusCanceled {
		t.Errorf("Expected job to be canceled, got %s", job.Status)
	}
}

func TestResolve_Applied(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-res", model.AlertGroupStatusTriggered)

	result, err := svc.Resolve("ag-res", Actor{Name: "Denis", Email: "denis@example.com"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Outcome != OutcomeApplied {
		t.Errorf("Expected applied, got %s", result.Outcome)
	}
	if result.AlertGroup.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected resolved, got %s", result.AlertGroup.Status)
	}
	if result.AlertGroup.ResolvedBy != "Denis" {
		t.Errorf("Expected resolved_by Denis, got %s", result.AlertGroup.ResolvedBy)
	}
	if result.AlertGroup.ResolvedAt == nil {
		t.Error("Expected resolved_at to be set")
	}

	// Verify outbox event
	events, _ := s.GetPendingOutboxEvents(10)
	var found *model.OutboxEvent
	for _, ev := range events {
		if ev.AlertGroupID == "ag-res" && ev.EventType == model.OutboxEventResolved {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("Expected outbox event for resolve")
	}
}

func TestResolve_FromAcknowledged(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-res-ack", model.AlertGroupStatusAcknowledged)

	result, err := svc.Resolve("ag-res-ack", Actor{Name: "Denis"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Outcome != OutcomeApplied {
		t.Errorf("Expected applied, got %s", result.Outcome)
	}
	if result.AlertGroup.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected resolved, got %s", result.AlertGroup.Status)
	}
}

func TestResolve_AlreadyDone(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)
	createAG(t, s, "ag-res-done", model.AlertGroupStatusResolved)

	result, err := svc.Resolve("ag-res-done", Actor{Name: "Denis"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Outcome != OutcomeAlreadyDone {
		t.Errorf("Expected already_done, got %s", result.Outcome)
	}
}

func TestResolve_NotFound(t *testing.T) {
	s := store.NewMockStore()
	svc := NewService(s)

	result, err := svc.Resolve("nonexistent", Actor{Name: "Denis"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if result.Outcome != OutcomeNotFound {
		t.Errorf("Expected not_found, got %s", result.Outcome)
	}
}
