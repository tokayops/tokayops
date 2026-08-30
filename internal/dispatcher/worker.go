package dispatcher

import (
	"context"
	"log"

	"github.com/google/uuid"
)

// Dispatcher is what is left of the job engine: a catalogue of what the
// channels can do, and a Run that holds.
//
// It holds no store any more. Resolving a provider to make a call from a step
// was the only thing here that needed one, and the steps are gone.
type Dispatcher struct {
	providers *ProviderRegistry

	// WorkerID is a per-process identity used only for log correlation.
	WorkerID string
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		providers: NewProviderRegistry(),
		WorkerID:  uuid.New().String(),
	}
}

// RegisterProviderCapabilities is a thin pass-through used at startup to
// declare what a provider can do (target kinds, integration type). Read by
// API / UI for the policy editor; not used for runtime resolution.
func (d *Dispatcher) RegisterProviderCapabilities(c ProviderCapabilities) {
	d.providers.RegisterCapabilities(c)
}

// Providers exposes the registry so the API layer can read capabilities.
// Callers must treat the result as read-only - runtime resolution is the
// dispatcher's concern.
func (d *Dispatcher) Providers() *ProviderRegistry {
	return d.providers
}

// Run holds until the context is cancelled, and starts nothing.
//
// The step loop is not started any more because nothing writes a step for it to
// take: an escalation is a set of commitments in the outbound domain, and a
// shift change is now an announcement admitted the same way. What remains here
// is the loop itself, the executors and the store operations under them, and
// they are removed in that order - a reader deleted before its writer is a hole
// the compiler cannot see.
//
// This is the one state in which the claim can be watched from the outside: a
// tree that still HAS the loop, with nobody starting it, is the tree in which
// "no new job rows appear" is a fact about the product rather than about a
// package that no longer compiles.
func (d *Dispatcher) Run(ctx context.Context) {
	log.Printf("StepWorker (WorkerID: %s) started with no step loop: nothing writes steps", d.WorkerID)
	<-ctx.Done()
}
