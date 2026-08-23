package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// failingUpsertStore makes UpsertNotificationDelivery fail to exercise the
// recordDelivery error paths.
type failingUpsertStore struct {
	store.StoreInterface
}

func (f *failingUpsertStore) UpsertNotificationDelivery(d *model.NotificationDelivery) error {
	return errors.New("upsert failed")
}

// P1: an editable send whose delivery cannot be recorded must fail the step (so it
// retries) and must NOT advance the alert group — the delivery row is the only ref
// for ack/resolve.
func TestExecutor_EditableRecordFailure_FailsStep(t *testing.T) {
	real := store.NewMockStore()
	wrap := &failingUpsertStore{StoreInterface: real}

	ag := &model.AlertGroup{ID: "ag-1", Title: "T", Severity: "critical", Status: model.AlertGroupStatusProcessing}
	real.CreateAlertGroup(ag)

	mp := &MockProvider{
		SendFunc: func(ctx context.Context, targetID string, ag *model.AlertGroup) (string, error) {
			return `{"channel_id":"C123","timestamp":"1.2"}`, nil
		},
	}
	exec := NewEscalationExecutor(wrap, staticProviders{"slack": mp}, &config.Config{})

	stepData := model.EscalationStepData{AlertGroupID: ag.ID, TargetType: "channel", TargetID: "C123", ProviderName: "slack"}
	data, _ := json.Marshal(stepData)
	step := &model.JobStep{ID: "s1", StepType: "channel", Data: data}

	if _, err := exec.Execute(context.Background(), &model.Job{}, step); err == nil {
		t.Fatal("expected Execute to fail when an editable delivery cannot be recorded")
	}

	got, err := real.GetAlertGroupByID(ag.ID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if got.Status != model.AlertGroupStatusProcessing {
		t.Fatalf("AG must remain processing after record failure, got %s", got.Status)
	}
}

// P1: a fire-and-forget DM has no ref to lose, so a record failure stays best-effort
// and the step still succeeds.
func TestExecutor_DMRecordFailure_StillSucceeds(t *testing.T) {
	real := store.NewMockStore()
	wrap := &failingUpsertStore{StoreInterface: real}

	real.CreateUser(&model.User{ID: "u1", Name: "x"})
	real.BindExternalIdentity(&model.ExternalIdentity{UserID: "u1", Provider: "slack", ExternalID: "U1"})
	ag := &model.AlertGroup{ID: "ag-1", Status: model.AlertGroupStatusProcessing}
	real.CreateAlertGroup(ag)

	mp := &MockProvider{
		SendDMFunc: func(ctx context.Context, uid, msg string) error { return nil },
	}
	exec := NewEscalationExecutor(wrap, staticProviders{"slack": mp}, &config.Config{})

	stepData := model.EscalationStepData{AlertGroupID: ag.ID, TargetType: "user", TargetID: "u1", Message: "hi", ProviderName: "slack"}
	data, _ := json.Marshal(stepData)
	step := &model.JobStep{ID: "s1", StepType: "dm", Data: data}

	res, err := exec.Execute(context.Background(), &model.Job{}, step)
	if err != nil {
		t.Fatalf("DM step should succeed despite record failure: %v", err)
	}
	if res != "dm_sent" {
		t.Fatalf("expected 'dm_sent', got %q", res)
	}
}
