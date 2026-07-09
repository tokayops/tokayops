package builders

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestBuildWithDedup_UsesDedupPrefix(t *testing.T) {
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

	// Test with "update_alert" prefix
	job, _, steps, err := builder.BuildWithDedup(ag, "update_alert")
	if err != nil {
		t.Fatalf("BuildWithDedup failed: %v", err)
	}

	if job == nil {
		t.Fatal("Job should not be nil")
	}
	if job.DedupKey == nil {
		t.Fatal("DedupKey should not be nil")
	}
	if !strings.HasPrefix(*job.DedupKey, "update_alert_") {
		t.Errorf("Expected dedup key prefix 'update_alert_', got %s", *job.DedupKey)
	}
	expectedDedup := "update_alert_" + ag.ID
	if *job.DedupKey != expectedDedup {
		t.Errorf("Expected dedup key %q, got %q", expectedDedup, *job.DedupKey)
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

	// Test that Build() uses "update_ack" prefix
	job2, _, _, err := builder.Build(ag)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}
	expectedAckDedup := "update_ack_" + ag.ID
	if *job2.DedupKey != expectedAckDedup {
		t.Errorf("Build() should use 'update_ack' prefix: expected %q, got %q", expectedAckDedup, *job2.DedupKey)
	}
}

func TestBuildWithDedup_NoUpdatableDeliveries(t *testing.T) {
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

	_, _, _, err = builder.BuildWithDedup(ag, "update_alert")
	if err != ErrNoUpdatableDeliveries {
		t.Errorf("Expected ErrNoUpdatableDeliveries, got %v", err)
	}
}
