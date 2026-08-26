package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/store"
)

func mustNewDispatcher(t *testing.T, s store.StoreInterface) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(s)
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

// TestTheEscalationStepTypesAreGone. Every other test in this package goes
// through mustNewDispatcher, which registers a stand-in for the deleted
// escalation executor - so none of them would notice production putting one
// back. This one asks the real constructor.
//
// A step type that resolves is a step type something can execute, and neither
// an escalation nor the state of its cards is the job engine's work any more. A
// leftover job from before the cutover has to FAIL on its first step, so that
// it stops without producing the effect it was written for - not so that its
// group is escalated again, which a destructive upgrade does not promise.
func TestTheEscalationStepTypesAreGone(t *testing.T) {
	d, err := NewDispatcher(store.NewMockStore())
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}

	for _, stepType := range []string{"dm", "channel", "firehose", "update", "resolve"} {
		if _, registered := d.executors[stepType]; registered {
			t.Errorf("%q still has an executor, so a job would run beside the "+
				"outbound domain", stepType)
		}
	}

	// And the one that is still the job engine's work is still there, so this
	// test fails for the right reason if the registry is emptied wholesale.
	if _, registered := d.executors["handoff_notify"]; !registered {
		t.Error("handoff_notify lost its executor")
	}
}
