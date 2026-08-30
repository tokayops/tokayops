package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

type mockHandoffProvider struct {
	sendDMCalled bool
	lastUserID   string
	lastMessage  string
	sendDMErr    error
}

func (m *mockHandoffProvider) Send(ctx context.Context, req providers.NotificationRequest) (string, error) {
	if req.Target.Kind == "user" {
		m.sendDMCalled = true
		m.lastUserID = req.Target.ID
		m.lastMessage = req.Message
		return "", m.sendDMErr
	}
	return "", nil
}
func (m *mockHandoffProvider) Update(ctx context.Context, _ *model.NotificationDelivery, _ *model.AlertGroup) (string, error) {
	return "", nil
}
func (m *mockHandoffProvider) Resolve(ctx context.Context, _ *model.NotificationDelivery, _ *model.AlertGroup) error {
	return nil
}
func (m *mockHandoffProvider) Permalink(_ *model.NotificationDelivery) string {
	return ""
}

func TestHandoffExecutor_Execute(t *testing.T) {
	provider := &mockHandoffProvider{}
	exec := NewHandoffExecutor(staticProviders{"slack": provider})

	// Handoff steps carry an explicit ProviderName, populated by the notifier
	// per linked identity; the executor does not hardcode "slack".
	stepData := model.HandoffStepData{
		ProviderName: "slack",
		TargetID:     "U12345",
		Message:      "You are on-call",
		ScheduleID:   "sched-1",
		TeamID:       "team-1",
	}
	data, _ := json.Marshal(stepData)

	job := &model.Job{ID: "job-1", Type: "handoff_notify"}
	step := &model.JobStep{ID: "step-1", StepType: "handoff_notify", Data: data}

	result, err := exec.Execute(context.Background(), job, step)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	if result != "handoff_dm_sent_to_U12345" {
		t.Errorf("Expected result 'handoff_dm_sent_to_U12345', got: %s", result)
	}
	if !provider.sendDMCalled {
		t.Error("Expected SendDM to be called")
	}
	if provider.lastUserID != "U12345" {
		t.Errorf("Expected userID 'U12345', got: %s", provider.lastUserID)
	}
	if provider.lastMessage != "You are on-call" {
		t.Errorf("Expected message 'You are on-call', got: %s", provider.lastMessage)
	}
}

// TestHandoffExecutor_NonSlackProvider_Routed asserts the executor honors
// ProviderName instead of hardcoding "slack".
func TestHandoffExecutor_NonSlackProvider_Routed(t *testing.T) {
	telegramProvider := &mockHandoffProvider{}
	slackProvider := &mockHandoffProvider{}
	exec := NewHandoffExecutor(staticProviders{
		"slack":    slackProvider,
		"telegram": telegramProvider,
	})

	stepData := model.HandoffStepData{
		ProviderName: "telegram",
		TargetID:     "TG_55",
		Message:      "shift",
	}
	data, _ := json.Marshal(stepData)

	job := &model.Job{ID: "job-tg"}
	step := &model.JobStep{ID: "step-tg", StepType: "handoff_notify", Data: data}

	if _, err := exec.Execute(context.Background(), job, step); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slackProvider.sendDMCalled {
		t.Error("slack provider must not be touched when ProviderName is telegram")
	}
	if !telegramProvider.sendDMCalled || telegramProvider.lastUserID != "TG_55" {
		t.Errorf("telegram provider was not invoked correctly: %+v", telegramProvider)
	}
}

func TestHandoffExecutor_MissingProvider(t *testing.T) {
	exec := NewHandoffExecutor(staticProviders{})

	stepData := model.HandoffStepData{ProviderName: "slack", TargetID: "U12345", Message: "test"}
	data, _ := json.Marshal(stepData)

	job := &model.Job{ID: "job-1"}
	step := &model.JobStep{ID: "step-1", Data: data}

	_, err := exec.Execute(context.Background(), job, step)
	if err == nil {
		t.Fatal("Expected error for missing provider")
	}
	if !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("Expected ErrUnknownProvider, got: %v", err)
	}
}

// TestHandoffExecutor_EmptyProviderName guards the defensive check - the
// handoff_notifier always sets ProviderName; a missing value is a build-invariant
// violation that must fail permanently with ErrMissingProvider, never retry.
func TestHandoffExecutor_EmptyProviderName(t *testing.T) {
	exec := NewHandoffExecutor(staticProviders{"slack": &mockHandoffProvider{}})

	stepData := model.HandoffStepData{TargetID: "U", Message: "x"} // no ProviderName
	data, _ := json.Marshal(stepData)

	job := &model.Job{ID: "job-1"}
	step := &model.JobStep{ID: "step-1", Data: data}

	_, err := exec.Execute(context.Background(), job, step)
	if !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("expected ErrMissingProvider on empty provider_name, got %v", err)
	}
	if !isPermanentError(err) {
		t.Fatal("ErrMissingProvider must be a permanent error")
	}
}

func TestHandoffExecutor_SendDMFailure(t *testing.T) {
	provider := &mockHandoffProvider{sendDMErr: errors.New("slack API error")}
	exec := NewHandoffExecutor(staticProviders{"slack": provider})

	stepData := model.HandoffStepData{ProviderName: "slack", TargetID: "U12345", Message: "test"}
	data, _ := json.Marshal(stepData)

	job := &model.Job{ID: "job-1"}
	step := &model.JobStep{ID: "step-1", Data: data}

	_, err := exec.Execute(context.Background(), job, step)
	if err == nil {
		t.Fatal("Expected error from SendDM failure")
	}
	if err.Error() != "slack API error" {
		t.Errorf("Expected 'slack API error', got: %s", err.Error())
	}
}

func TestHandoffExecutor_InvalidData(t *testing.T) {
	provider := &mockHandoffProvider{}
	exec := NewHandoffExecutor(staticProviders{"slack": provider})

	job := &model.Job{ID: "job-1"}
	step := &model.JobStep{ID: "step-1", Data: []byte("invalid json")}

	_, err := exec.Execute(context.Background(), job, step)
	if err == nil {
		t.Fatal("Expected error for invalid JSON data")
	}
}
