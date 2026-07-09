package builders

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

func TestResolutionJobBuilder_WithSnapshot(t *testing.T) {
	s := store.NewMockStore()

	// Create AG with snapshot
	ag := &model.AlertGroup{
		ID: "ag1",
		PolicySnapshot: &model.EscalationPolicySnapshot{
			Name: "snap_policy",
			Steps: []*model.EscalationStepSnapshot{
				{
					Provider:   "slack", // Supports resolution
					TargetKind: "dm",
				},
			},
		},
	}
	s.CreateAlertGroup(ag)

	// Seed primary delivery
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

	cfg := &config.Config{} // Empty config
	builder := NewResolutionJobBuilder(cfg, s)

	job, _, steps, err := builder.Build(ag)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if job == nil {
		t.Fatal("Job should not be nil")
	}

	if len(steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(steps))
	}

	step := steps[0]
	if step.StepType != "resolve" {
		t.Errorf("Expected step type 'resolve', got %s", step.StepType)
	}

	var data model.ResolutionStepData
	if err := json.Unmarshal(step.Data, &data); err != nil {
		t.Fatalf("Failed to unmarshal step data: %v", err)
	}

	if data.ProviderName != "slack" {
		t.Errorf("Expected provider 'slack', got %s", data.ProviderName)
	}
}

func TestResolutionJobBuilder_Nothing(t *testing.T) {
	s := store.NewMockStore()

	// AG without snapshot and no config
	ag := &model.AlertGroup{
		ID: "ag3",
	}

	cfg := &config.Config{}
	builder := NewResolutionJobBuilder(cfg, s)

	job, _, steps, err := builder.Build(ag)
	if err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	if job != nil || len(steps) > 0 {
		t.Error("Expected no job when no policy/snapshot available")
	}
}
