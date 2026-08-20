package builders

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestUpdateBuilder_NamesItsFamily(t *testing.T) {
	s := store.NewMockStore()

	ag := &model.AlertGroup{
		ID:     "ag_dedup_test",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// Seed an updatable delivery
	delivery := &model.NotificationDelivery{
		AlertGroupID:   ag.ID,
		Provider:       "slack",
		Kind:           "slack_channel",
		SupportsUpdate: true,
		IsPrimary:      true,
		CreatedAt:      time.Now(),
	}
	if err := s.UpsertNotificationDelivery(delivery); err != nil {
		t.Fatalf("Failed to seed delivery: %v", err)
	}

	cfg := &config.Config{}
	builder, err := NewUpdateJobBuilder(cfg, s)
	if err != nil {
		t.Fatalf("NewUpdateJobBuilder failed: %v", err)
	}

	job, _, steps, err := builder.BuildAlertUpdate(ag)
	if err != nil {
		t.Fatalf("BuildAlertUpdate failed: %v", err)
	}

	if job == nil {
		t.Fatal("Job should not be nil")
	}
	// Both families share the job type "update", so the namespace is the only
	// thing that tells them apart.
	if job.Dedup == nil || *job.Dedup != *jobdedup.AlertUpdate(ag.ID) {
		t.Errorf("dedup spec = %+v, want the alert-update identity of %s", job.Dedup, ag.ID)
	}

	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}
	if steps[0].StepType != "update" {
		t.Errorf("Expected step type 'update', got %s", steps[0].StepType)
	}

	var data model.ResolutionStepData
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("Failed to unmarshal step data: %v", err)
	}
	if data.Operation != "update" {
		t.Errorf("Expected operation 'update', got %s", data.Operation)
	}

	job2, _, _, err := builder.Build(ag)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	if job2.Dedup == nil || *job2.Dedup != *jobdedup.AckUpdate(ag.ID) {
		t.Errorf("dedup spec = %+v, want the ack-update identity of %s", job2.Dedup, ag.ID)
	}
}

func TestUpdateBuilder_NoUpdatableDeliveries(t *testing.T) {
	s := store.NewMockStore()

	ag := &model.AlertGroup{
		ID:     "ag_no_deliveries",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	cfg := &config.Config{}
	builder, err := NewUpdateJobBuilder(cfg, s)
	if err != nil {
		t.Fatalf("NewUpdateJobBuilder failed: %v", err)
	}

	_, _, _, err = builder.BuildAlertUpdate(ag)
	if err != ErrNoUpdatableDeliveries {
		t.Errorf("Expected ErrNoUpdatableDeliveries, got %v", err)
	}
}
