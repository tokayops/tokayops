package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/store"
)

func mustNewDispatcher(t *testing.T, s store.StoreInterface, cfg *config.Config) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(s, cfg)
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}
	return withLegacySteps(d)
}

// legacyStepExecutor stands in for the escalation executor these tests used to
// lean on.
//
// The escalation path left the job engine in Sprint 1: an escalation is a set
// of commitments in the outbound domain now, and no production executor takes a
// "dm", "channel" or "firehose" step any more. What the tests below are about
// is the ENGINE - retries, continue_on_failure, advancing a stage, unlocking a
// step - and they need a step that can be made to succeed or fail on demand.
//
// It is deliberately the thinnest thing that gives them that: read the step's
// data, find the provider it names, send. A test that wants a failure makes the
// provider fail, exactly as before. It carries no delivery recording, no
// timeline and no status transitions, because none of that is what is being
// tested here - and the whole job engine goes in Sprint 3 with these tests.
type legacyStepExecutor struct{ providers ProviderResolver }

func (e legacyStepExecutor) Execute(ctx context.Context, job *model.Job, step *model.JobStep) (string, error) {
	var data model.EscalationStepData
	if err := json.Unmarshal(step.Data, &data); err != nil {
		return "", fmt.Errorf("step %s carries unreadable data: %w", step.ID, err)
	}
	p, err := e.providers.Provider(data.ProviderName)
	if err != nil {
		return "", err
	}
	kind := "channel"
	if step.StepType == "dm" {
		kind = "user"
	}
	return p.Send(ctx, providers.NotificationRequest{
		Target:  providers.NotificationTarget{Kind: kind, ID: data.TargetID},
		Message: data.Message,
	})
}

// withLegacySteps registers that executor for the step types the escalation
// path used to own.
func withLegacySteps(d *Dispatcher) *Dispatcher {
	exec := legacyStepExecutor{providers: d.providers}
	for _, stepType := range []string{"dm", "channel", "firehose"} {
		d.RegisterExecutor(stepType, exec)
	}
	return d
}
