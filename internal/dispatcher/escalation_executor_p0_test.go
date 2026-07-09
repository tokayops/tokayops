package dispatcher

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// TestExecutor_PreResolvedUserTarget validates that the executor correctly
// handles steps that were pre-resolved by the builder (TargetType="user").
func TestExecutor_PreResolvedUserTarget(t *testing.T) {
	s := store.NewMockStore()

	user := &model.User{ID: "user-2", Name: "Override User"}
	s.CreateUser(user)
	slackID := "U_2"
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: user.ID, Provider: "slack", ExternalID: slackID})

	ag := &model.AlertGroup{
		ID:       "ag-1",
		Title:    "Test Alert",
		Severity: "critical",
		Status:   model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	var notifiedUserID string
	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, msg string) error {
			notifiedUserID = userID
			return nil
		},
	}
	exec := NewEscalationExecutor(s, staticProviders{"slack": mp}, &config.Config{})

	// Step pre-resolved to specific user by builder (schedule override already applied)
	stepData := model.EscalationStepData{
		AlertGroupID: ag.ID,
		TargetType:   "user",
		TargetID:     user.ID,
		ProviderName: "slack",
		Message:      "Urgent!",
	}
	data, _ := json.Marshal(stepData)
	step := &model.JobStep{StepType: "dm", Data: data}

	_, err := exec.Execute(context.Background(), &model.Job{}, step)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if notifiedUserID != slackID {
		t.Fatalf("Expected notification to %s, got %s", slackID, notifiedUserID)
	}
}
