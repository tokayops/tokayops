package builders

import (
	"encoding/json"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// Every escalation step must carry the provider name (set by the builder) so the
// executor no longer derives it from the step type.
func TestEscalationJobBuilder_SetsProviderName(t *testing.T) {
	s := store.NewMockStore()
	builder := NewEscalationJobBuilder(s, nil)

	policy := &model.EscalationPolicy{
		ID:   "pol-1",
		Name: "P",
		Steps: []*model.EscalationStep{
			{ID: "s1", Provider: "slack", TargetKind: "channel", TargetType: "channel", TargetID: "C1"},
		},
	}
	s.CreateEscalationPolicy(policy)
	ag := &model.AlertGroup{ID: "ag-1", DedupKey: "d1", Status: model.AlertGroupStatusProcessing}
	s.CreateAlertGroup(ag)

	_, _, steps, _, err := builder.Build(ag, "pol-1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}

	var data model.EscalationStepData
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("unmarshal step data: %v", err)
	}
	if data.ProviderName != "slack" {
		t.Fatalf("expected ProviderName=slack, got %q", data.ProviderName)
	}
}
