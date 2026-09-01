package outbound

import (
	"context"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
)

// FanOut is the webhook family's producer: it turns each event of the alert
// outbox into commitments to the subscribers that are enabled and in scope at
// that moment.
//
// A loop of its own, and not a step of the worker's tick. The worker is the
// executor of a family and knows nothing about where its work comes from - the
// escalation engine and the shift-change detector are producers beside it, and
// this one belongs beside them too. Its ticker is the family's claim interval
// because there is no reason for two clocks; the number of events per tick is
// the family's pool size for the same reason.
//
// Nothing here is waited for on the way out. The store operation makes no
// network calls and commits or rolls back whole, so a process that dies in the
// middle of a tick leaves the event pending for the next one.
type FanOut struct {
	store    fanOutStore
	interval time.Duration
	perTick  int
}

// FanOutResult is what one fan-out transaction did.
type FanOutResult struct {
	// Found says an event was there to fan out. False is the normal end of a
	// tick: nothing pending.
	Found bool
	// EventID is the event, when one was found - including when fanning it out
	// failed, so the caller can name it.
	EventID string
	// Refused says the EVENT was refused - it cannot be read by this build, or
	// its claim is already held - as opposed to the database failing. Set
	// beside the error, never instead of it: the caller counts it as a storage
	// contract failure and names the event, and the event stays where it is.
	Refused bool
	// Outcome and Commitments describe the admission, when it committed.
	Outcome     SubmitOutcome
	Commitments int
}

// fanOutStore is what the producer needs from the store, and nothing more.
type fanOutStore interface {
	// FanOutNextEvent claims one pending event under a row lock, resolves its
	// audience, admits the commitments and marks the event, in one
	// transaction. It returns Found=false when there is nothing pending.
	//
	// An event this build cannot read - an unknown event type, an empty body -
	// is a contract violation that leaves the event untouched, and the caller
	// sees it named in the result. That event stays at the head of the queue
	// until a person fixes it: the execution model does not build a way
	// around a damaged execution row, only a way to see it.
	FanOutNextEvent(ctx context.Context) (FanOutResult, error)
}

// NewFanOut builds the producer over the webhook family's policy.
func NewFanOut(st fanOutStore) (*FanOut, error) {
	policy, err := PolicyOf(FamilyWebhook)
	if err != nil {
		return nil, err
	}
	return &FanOut{store: st, interval: policy.ClaimInterval, perTick: policy.PoolSize}, nil
}

// Run fans out until the context is cancelled.
func (f *FanOut) Run(ctx context.Context) {
	log.Printf("outbound fan-out started: every %s, up to %d events per tick", f.interval, f.perTick)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("outbound fan-out stopped")
			return
		case <-ticker.C:
			f.Tick(ctx)
		}
	}
}

// Tick fans out up to perTick events, one transaction each, and stops early
// when the queue is empty or an event cannot be handled.
//
// Exported so that something other than the clock can drive it. It returns how
// many events were fanned out, which is what a test asks.
func (f *FanOut) Tick(ctx context.Context) int {
	done := 0
	for i := 0; i < f.perTick; i++ {
		if ctx.Err() != nil {
			return done
		}
		result, err := f.store.FanOutNextEvent(ctx)
		if err != nil {
			if result.Refused {
				// A row this build cannot read. Counted and named; not skipped,
				// not ended - see fanOutStore.
				metrics.StorageContractFailuresTotal.WithLabelValues("event_outbox").Inc()
				log.Printf("outbound fan-out: event %s cannot be fanned out and holds the queue: %v",
					result.EventID, err)
			} else {
				log.Printf("outbound fan-out: %v", err)
			}
			return done
		}
		if !result.Found {
			return done
		}
		// Counted here, after the store's commit: the store works inside the
		// transaction and has no business reporting an admission that may yet
		// roll back.
		metrics.OutboundAdmissionsTotal.WithLabelValues(FamilyWebhook,
			AdmissionLabel(result.Outcome, result.Commitments)).Inc()
		done++
	}
	return done
}
