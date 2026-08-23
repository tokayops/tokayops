package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	slackprovider "github.com/tokayops/tokayops/internal/outbound/providers/slack"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// Reuse MockProvider from refactor_test.go (same package)

func TestExecutor_UserTarget_Success(t *testing.T) {
	s := store.NewMockStore()

	userID := "user-1"
	slackID := "U_TEST_1"
	s.CreateUser(&model.User{
		ID:   userID,
		Name: "Test User",
	})
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: userID, Provider: "slack", ExternalID: slackID})

	ag := &model.AlertGroup{
		ID:       "ag-1",
		Title:    "Test Alert",
		Severity: "critical",
		Status:   model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, uid, msg string) error {
			if uid != slackID {
				return fmt.Errorf("expected slackID %s, got %s", slackID, uid)
			}
			return nil
		},
	}
	exec := NewEscalationExecutor(s, staticProviders{"slack": mp}, &config.Config{})

	// Step pre-resolved to user (schedule resolution now happens in builder)
	stepData := model.EscalationStepData{
		AlertGroupID: ag.ID,
		TargetType:   "user",
		TargetID:     userID,
		ProviderName: "slack",
		Message:      "Wake up!",
	}
	data, _ := json.Marshal(stepData)
	step := &model.JobStep{StepType: "dm", Data: data}

	result, err := exec.Execute(context.Background(), &model.Job{}, step)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result != "dm_sent" {
		t.Errorf("Expected result 'dm_sent', got '%s'", result)
	}
}

// An unexpected step type reaching the escalation executor must be rejected, not
// silently sent as a channel card (regression guard for the send-switch default).
func TestExecutor_UnsupportedStepType_Rejected(t *testing.T) {
	s := store.NewMockStore()
	ag := &model.AlertGroup{ID: "ag-1", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	exec := NewEscalationExecutor(s, staticProviders{"slack": &MockProvider{}}, &config.Config{})

	stepData := model.EscalationStepData{
		AlertGroupID: ag.ID,
		ProviderName: "slack",
		TargetType:   "channel",
		TargetID:     "C1",
	}
	data, _ := json.Marshal(stepData)
	step := &model.JobStep{ID: "s1", StepType: "carrier_pigeon", Data: data}

	if _, err := exec.Execute(context.Background(), &model.Job{}, step); err == nil {
		t.Fatal("expected an unsupported step type to be rejected")
	}
}

func TestExecutor_UserTarget_NoSlackID(t *testing.T) {
	s := store.NewMockStore()
	userID := "user-no-slack"
	s.CreateUser(&model.User{ID: userID, Name: "No Slack"}) // No SlackID

	ag := &model.AlertGroup{ID: "ag-1", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	exec := NewEscalationExecutor(s, staticProviders{"slack": &MockProvider{}}, &config.Config{})

	stepData := model.EscalationStepData{
		AlertGroupID: ag.ID,
		TargetType:   "user",
		TargetID:     userID,
		ProviderName: "slack",
	}
	data, _ := json.Marshal(stepData)
	step := &model.JobStep{StepType: "dm", Data: data}

	_, err := exec.Execute(context.Background(), &model.Job{}, step)
	if err == nil {
		t.Fatal("Expected error for user without slack ID, got nil")
	}
}

// A dm step is fire-and-forget: it must not clobber the primary channel
// delivery (the one used for updates/resolution) and records a non-updatable row.
func TestExecutor_SlackDM_DoesNotClobberPrimaryDelivery(t *testing.T) {
	s := store.NewMockStore()

	ag := &model.AlertGroup{
		ID:     "ag-1",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	// Seed the primary, updatable channel delivery.
	primaryPayload := `{"channel_id":"C123","timestamp":"123.456"}`
	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		ID:              "primary-del",
		AlertGroupID:    ag.ID,
		Provider:        "slack",
		Kind:            "channel",
		ProviderPayload: primaryPayload,
		SupportsUpdate:  true,
		IsPrimary:       true,
		CreatedAt:       time.Now(),
	})

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, msg string) error {
			if userID == "" {
				t.Error("expected non-empty userID")
			}
			return nil
		},
	}
	exec := NewEscalationExecutor(s, staticProviders{"slack": mp}, &config.Config{})

	stepData := model.EscalationStepData{
		AlertGroupID: ag.ID,
		TargetID:     "U123",
		ProviderName: "slack",
		Message:      "Heads up!",
	}
	stepBytes, _ := json.Marshal(stepData)
	step := &model.JobStep{ID: "dm-step", StepType: "dm", Data: stepBytes}

	result, err := exec.Execute(context.Background(), &model.Job{}, step)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if result != "dm_sent" {
		t.Fatalf("Expected result 'dm_sent', got '%s'", result)
	}

	// Primary channel delivery is untouched.
	primary, err := s.GetPrimaryDelivery(ag.ID, "slack")
	if err != nil {
		t.Fatalf("GetPrimaryDelivery: %v", err)
	}
	if primary == nil || primary.ID != "primary-del" || primary.ProviderPayload != primaryPayload {
		t.Fatalf("primary delivery clobbered: %+v", primary)
	}

	// The DM produced a non-updatable delivery.
	dm, err := s.GetDeliveryByID("dm-step")
	if err != nil {
		t.Fatalf("GetDeliveryByID: %v", err)
	}
	if dm == nil {
		t.Fatal("expected DM delivery to be recorded")
	}
	if dm.SupportsUpdate {
		t.Fatal("DM delivery should not support update")
	}
}

func TestExecutor_SlackDM_AppendsPrimaryPermalink(t *testing.T) {
	s := store.NewMockStore()

	userID := "user-1"
	slackID := "U_TEST_1"
	s.CreateUser(&model.User{
		ID:   userID,
		Name: "Test User",
	})
	s.BindExternalIdentity(&model.ExternalIdentity{UserID: userID, Provider: "slack", ExternalID: slackID})

	ag := &model.AlertGroup{
		ID:     "ag-1",
		Title:  "Test Alert",
		Status: model.AlertGroupStatusProcessing,
	}
	s.CreateAlertGroup(ag)

	slackData := slackprovider.Data{
		ChannelID: "C123",
		Timestamp: "123.456",
		Permalink: "https://slack.example.com/archives/C123/p123456",
	}
	dataBytes, _ := json.Marshal(slackData)
	s.UpsertNotificationDelivery(&model.NotificationDelivery{
		AlertGroupID:    ag.ID,
		Provider:        "slack",
		Kind:            "channel",
		ProviderPayload: string(dataBytes),
		SupportsUpdate:  true,
		IsPrimary:       true,
		CreatedAt:       time.Now(),
	})

	var sentMessage string
	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, userID, msg string) error {
			sentMessage = msg
			return nil
		},
		PermalinkFunc: func(d *model.NotificationDelivery) string {
			var data slackprovider.Data
			if err := json.Unmarshal([]byte(d.ProviderPayload), &data); err != nil {
				return ""
			}
			return data.Permalink
		},
	}
	exec := NewEscalationExecutor(s, staticProviders{"slack": mp}, &config.Config{})

	stepData := model.EscalationStepData{
		AlertGroupID: ag.ID,
		TargetType:   "user",
		TargetID:     userID,
		ProviderName: "slack",
		Message:      "Please review",
	}
	stepBytes, _ := json.Marshal(stepData)
	step := &model.JobStep{StepType: "dm", Data: stepBytes}

	_, err := exec.Execute(context.Background(), &model.Job{}, step)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if sentMessage == "" {
		t.Fatalf("expected SendDM to be called")
	}
	if !strings.Contains(sentMessage, slackData.Permalink) {
		t.Fatalf("expected DM to include permalink, got: %s", sentMessage)
	}
}
