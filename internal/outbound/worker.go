package outbound

import (
	"context"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// The worker: one process's share of a family's delivery.
//
// Everything that matters for correctness is in the database. This holds a
// count of how many attempts it is running, and that count bounds THIS process
// rather than the work - two instances know nothing about each other and do not
// need to. The lease is the only thing that arbitrates, and it is a row.
//
// The cycle is deliberately in this order. Expiry and recovery first, because
// they are what returns abandoned work to the queue; then a claim for exactly
// the number of slots that are free. A claim for more than that is a lease
// held in a local queue while it expires - the failure that produces a
// duplicate page out of nothing but impatience.

// outboundStore is the persistence this worker needs, and nothing else. The
// concrete store is passed in at wiring time: a domain package that could reach
// the database directly would eventually reach past its own rules.
type outboundStore interface {
	ExpireDueIntents(ctx context.Context, family string, limit int) ([]Expired, error)
	RecoverStaleAttempts(ctx context.Context, family string, limit int) ([]Recovered, error)
	DueSnapshot(ctx context.Context, family string) ([]ProviderDue, error)
	ClaimDueIntents(ctx context.Context, req ClaimRequest) ([]Leased, error)
	BeginAttempt(ctx context.Context, req BeginAttemptRequest) (BeginAttemptResult, error)
	FinalizeDeliveryAttempt(ctx context.Context, req FinalizeRequest) (FinalizeResult, error)
}

// Channel is one provider as the worker sees it: whether a call may be made,
// how to make it, and what the answer means.
type Channel interface {
	Preparer
	Handler
}

// Worker runs one family's deliveries on one instance.
type Worker struct {
	family   string
	workerID string
	pool     int

	store    outboundStore
	channels map[string]Channel

	inflight atomic.Int64
	ticks    atomic.Uint64
	running  sync.WaitGroup
}

// NewWorker builds the worker for the notification family.
//
// The channels are keyed by provider. A provider missing from this map is one
// this instance cannot serve, and it is left alone rather than failed: what
// this process was configured with is not a property of the commitment, and an
// instance that ended work its neighbours can do would be the worst kind of
// deployment accident.
func NewWorker(st outboundStore, workerID string, channels map[string]Channel) *Worker {
	return &Worker{
		family:   FamilyNotification,
		workerID: workerID,
		pool:     NotificationPoolSize,
		store:    st,
		channels: channels,
	}
}

// Run works until the context is cancelled, then waits for what it holds.
func (w *Worker) Run(ctx context.Context) {
	log.Printf("outbound worker %s started: family=%s pool=%d providers=%d",
		w.workerID, w.family, w.pool, len(w.channels))

	ticker := time.NewTicker(NotificationClaimInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.drain()
			return
		case <-ticker.C:
			w.tick(ctx)
		}
	}
}

// drain waits for the attempts already in flight.
//
// They are not cancelled with the worker: a call that was made and whose answer
// is thrown away is the one outcome this whole domain exists to avoid, and
// waiting is cheap by comparison. What is not finished by the deadline is left
// to another instance's recovery, which closes it as ambiguous - correct, and
// expensive, which is why the deadline is longer than an attempt.
func (w *Worker) drain() {
	finished := make(chan struct{})
	go func() {
		w.running.Wait()
		close(finished)
	}()

	select {
	case <-finished:
		log.Printf("outbound worker %s stopped", w.workerID)
	case <-time.After(NotificationShutdownDeadline):
		log.Printf("outbound worker %s stopped with %d attempts unfinished; "+
			"recovery will close them as ambiguous", w.workerID, w.inflight.Load())
	}
}

func (w *Worker) tick(ctx context.Context) {
	tick := w.ticks.Add(1)

	// Housekeeping runs whether or not this instance has room. It is mostly
	// about work OTHER instances abandoned, and skipping it while busy is how a
	// fleet ends up with a queue nobody is looking after.
	if expired, err := w.store.ExpireDueIntents(ctx, w.family, w.pool); err != nil {
		log.Printf("outbound worker %s: expire: %v", w.workerID, err)
	} else if len(expired) > 0 {
		log.Printf("outbound worker %s: %d commitments passed their deadline unsent",
			w.workerID, len(expired))
	}
	if recovered, err := w.store.RecoverStaleAttempts(ctx, w.family, w.pool); err != nil {
		log.Printf("outbound worker %s: recover: %v", w.workerID, err)
	} else if len(recovered) > 0 {
		log.Printf("outbound worker %s: %d abandoned attempts closed", w.workerID, len(recovered))
	}

	free := w.pool - int(w.inflight.Load())
	if free <= 0 {
		return
	}

	for _, leased := range w.claim(ctx, free, tick) {
		w.inflight.Add(1)
		w.running.Add(1)
		go func(l Leased) {
			defer w.running.Done()
			defer w.inflight.Add(-1)
			w.serve(ctx, l)
		}(leased)
	}
}

// claim takes work for the free slots, and never more than that.
//
// Two mechanisms rather than one, because both stale numbers exist. The
// allocation is computed from demand a claim could take - not everything that
// is due - and what a claim still fails to get is returned to the remainder and
// offered to somebody else. The rows in between were taken by another instance
// in the milliseconds since the aggregate; that is normal, and a scheduler that
// treated its own aggregate as the truth would idle with work waiting.
func (w *Worker) claim(ctx context.Context, free int, tick uint64) []Leased {
	due, err := w.store.DueSnapshot(ctx, w.family)
	if err != nil {
		log.Printf("outbound worker %s: read the queue: %v", w.workerID, err)
		return nil
	}

	demand := make(map[string]int, len(due))
	fresh := make(map[string]int, len(due))
	for _, provider := range due {
		if _, served := w.channels[provider.Provider]; !served {
			// Not this instance's to take. Claiming it would hold a lease for
			// as long as it lasts and deliver nothing.
			continue
		}
		demand[provider.Provider] = provider.ClaimableDue
		fresh[provider.Provider] = provider.ClaimableFresh
	}

	var taken []Leased
	remaining := free
	for remaining > 0 {
		shares := shareSlots(remaining, demand, tick)
		if len(shares) == 0 {
			break
		}

		got := 0
		for _, provider := range sortedProviders(shares) {
			limit := shares[provider]
			first, any := splitPhases(limit, fresh[provider],
				demand[provider]-fresh[provider], tick)

			for _, phase := range []struct {
				kind  ClaimPhase
				limit int
			}{{ClaimFirstAttempts, first}, {ClaimAny, any}} {
				if phase.limit == 0 {
					continue
				}
				leased, err := w.store.ClaimDueIntents(ctx, ClaimRequest{
					Family: w.family, Provider: provider, Phase: phase.kind,
					Limit: phase.limit, Lease: NotificationLease, WorkerID: w.workerID,
				})
				if err != nil {
					log.Printf("outbound worker %s: claim %s: %v", w.workerID, provider, err)
					break
				}
				taken = append(taken, leased...)
				got += len(leased)
				remaining -= len(leased)

				// Offered is offered, whether or not it produced rows. A
				// first-attempt share that came back short means those rows
				// are gone - somebody else has them - and counting only what
				// arrived would leave the next pass splitting slots towards a
				// queue that is already empty, while the retries beside it
				// wait with the pool half idle.
				if phase.kind == ClaimFirstAttempts {
					if fresh[provider] -= phase.limit; fresh[provider] < 0 {
						fresh[provider] = 0
					}
				}
			}

			// What was offered is off the table either way: rows this instance
			// asked for and did not get belong to somebody else now.
			demand[provider] -= limit
			if demand[provider] < 0 {
				demand[provider] = 0
			}
			if fresh[provider] > demand[provider] {
				fresh[provider] = demand[provider]
			}
			if remaining <= 0 {
				break
			}
		}
		if got == 0 {
			break
		}
	}
	return taken
}

// sortedProviders keeps a pass reproducible. The shares are already decided;
// what the order settles is who is asked first when the last slots run out
// mid-pass, and a map's order would make that different every run.
func sortedProviders(shares map[string]int) []string {
	providers := make([]string, 0, len(shares))
	for provider := range shares {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

// serve carries one commitment through one attempt.
//
// Three contexts, and the split is the point. Preparation and the two record
// keeping calls get their own short deadlines; the attempt gets the family's.
// None of them is the worker's own context: a shutdown that cancelled the
// recording of a result the provider already accepted would turn a graceful
// stop into a duplicate page.
func (w *Worker) serve(parent context.Context, leased Leased) {
	channel, served := w.channels[leased.Intent.Provider]
	if !served {
		log.Printf("outbound worker %s: nothing here serves %s; leaving %s",
			w.workerID, leased.Intent.Provider, leased.Intent.ID)
		return
	}
	detached := context.WithoutCancel(parent)

	prepareCtx, cancelPrepare := context.WithTimeout(detached, NotificationPrepareDeadline)
	prepared := channel.Prepare(prepareCtx, leased.Intent)
	cancelPrepare()

	beginCtx, cancelBegin := recording(detached)
	begun, err := w.store.BeginAttempt(beginCtx,
		prepared.Request(leased.Intent.ID, leased.LeaseToken, w.workerID))
	cancelBegin()
	if err != nil {
		log.Printf("outbound worker %s: begin %s: %v", w.workerID, leased.Intent.ID, err)
		return
	}
	if begun.Outcome != BeginStarted {
		return
	}

	attemptCtx, cancelAttempt := context.WithTimeout(detached, NotificationAttemptDeadline)
	result, execErr := channel.ExecuteAttempt(attemptCtx, Call{
		IntentID:             leased.Intent.ID,
		AttemptID:            begun.AttemptID,
		Provider:             leased.Intent.Provider,
		AttemptKind:          begun.AttemptKind,
		Operation:            begun.Operation,
		Endpoint:             begun.BoundEndpoint,
		ProviderKey:          begun.ProviderKey,
		Revision:             begun.AppliedRevision,
		State:                begun.Snapshot,
		Payload:              begun.Payload,
		PayloadSchemaVersion: begun.PayloadSchemaVersion,
	})
	cancelAttempt()

	completion, breach := Conclude(channel, result)
	if breach != BreachNone {
		// Not fatal to the delivery - the conclusion above is already the safe
		// one - but a channel that does this is wrong, and silence here would
		// keep it wrong.
		log.Printf("outbound worker %s: %s broke its contract on attempt %s (%s); "+
			"recorded as %s", w.workerID, leased.Intent.Provider, begun.AttemptID,
			breach, completion.Outcome)
	}

	finalizeCtx, cancelFinalize := recording(detached)
	recorded, err := w.store.FinalizeDeliveryAttempt(finalizeCtx, FinalizeRequest{
		AttemptID:  begun.AttemptID,
		LeaseToken: leased.LeaseToken,
		Completion: completion,
		Receipt:    result.Receipt.Raw(),
		Summary:    Summarise(result.Summary, execErr),
	})
	cancelFinalize()
	if err != nil {
		// The attempt stays open and recovery will close it as ambiguous,
		// which is the honest outcome: the call was made and nobody can say
		// what it did.
		log.Printf("outbound worker %s: record the result of %s: %v",
			w.workerID, begun.AttemptID, err)
		return
	}

	// Anything but a plain finalisation means somebody else had already decided
	// this attempt, and that is worth saying out loud: a conflict in particular
	// is two contradicting accounts of one call, which no amount of retrying
	// resolves.
	switch recorded.Outcome {
	case FinalizeFinalized, FinalizeIdempotentRepeat:
	default:
		log.Printf("outbound worker %s: the result of %s was answered %q",
			w.workerID, begun.AttemptID, recorded.Outcome)
	}
}

// recording bounds one short database call.
//
// Its own deadline, longer than the lock timeout inside the store so that a row
// somebody else holds is refused by the store's own rule rather than by a
// context nobody can explain, and short enough that a database gone quiet does
// not hold a slot for the length of a lease.
func recording(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, NotificationRecordDeadline)
}
