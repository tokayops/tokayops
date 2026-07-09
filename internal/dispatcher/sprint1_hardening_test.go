package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// P2.1: an editable channel send must produce valid coordinates; if Slack returns an
// empty timestamp the send fails rather than persisting a useless payload.
func TestSlackSend_EmptyTimestamp_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "channel": "C123", "ts": ""})
	}))
	defer server.Close()

	provider := newSlackProviderForTest("test-token", server.URL+"/", "")
	_, err := provider.Send(context.Background(), NotificationRequest{
		Kind:       "channel",
		Target:     NotificationTarget{Kind: "channel", ID: "C123"},
		AlertGroup: &model.AlertGroup{ID: "ag", Title: "T", Severity: "critical"},
		Editable:   true,
	})
	if err == nil {
		t.Fatal("expected error when postMessage returns an empty timestamp")
	}
}

// P2.3: Update/Resolve reject a non-empty but invalid payload (missing coordinates)
// before making any Slack call.
func TestSlackUpdateResolve_InvalidPayload_Errors(t *testing.T) {
	provider := newSlackProviderForTest("test-token", "http://invalid.local/", "")
	ag := &model.AlertGroup{ID: "ag", Title: "T", Severity: "critical"}
	bad := &model.NotificationDelivery{ID: "d1", ProviderPayload: `{"permalink":"x"}`}

	if _, err := provider.Update(context.Background(), bad, ag); err == nil {
		t.Error("Update should reject an invalid provider payload")
	}
	if err := provider.Resolve(context.Background(), bad, ag); err == nil {
		t.Error("Resolve should reject an invalid provider payload")
	}
}

// P3: Send validates the target shape and rejects unknown kinds instead of silently
// posting a channel card.
func TestSlackSend_KindValidation(t *testing.T) {
	provider := newSlackProviderForTest("test-token", "http://invalid.local/", "")
	ctx := context.Background()

	if _, err := provider.Send(ctx, NotificationRequest{Target: NotificationTarget{Kind: "carrier-pigeon", ID: "x"}}); err == nil {
		t.Error("unknown target kind should error")
	}
	if _, err := provider.Send(ctx, NotificationRequest{Target: NotificationTarget{Kind: "channel", ID: "C1"}, Editable: true}); err == nil {
		t.Error("channel send without an alert group should error")
	}
	if _, err := provider.Send(ctx, NotificationRequest{Target: NotificationTarget{Kind: "user", ID: "U1"}}); err == nil {
		t.Error("user send without a message should error")
	}
}
