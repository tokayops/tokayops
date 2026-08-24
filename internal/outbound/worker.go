package outbound

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
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

// Tick is one pass of the cycle: housekeeping, then a claim for the slots that
// are free.
//
// Exported so that something other than the clock can drive it - a test that
// waited whole seconds per delivery would spend its life asleep - and because
// what a pass DOES is worth naming rather than hiding inside a ticker.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

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
			first, retries := splitPhases(limit, fresh[provider],
				demand[provider]-fresh[provider], tick)

			for _, phase := range []struct {
				kind  ClaimPhase
				limit int
			}{{ClaimFirstAttempts, first}, {ClaimRetriesFirst, retries}} {
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

	// Observed HERE, before the call, because the fact it measures is already
	// durable: the attempt exists and its started_at is committed. Observed
	// after the call instead - as this first did - the distribution would only
	// ever contain attempts that came back, so a provider that hangs and a
	// process that dies would quietly remove exactly the worst measurements
	// from a metric about how long a page takes to go out.
	if begun.FirstAttemptLatency != nil {
		metrics.OutboundAdmissionLatencySeconds.WithLabelValues(w.family).
			Observe(*begun.FirstAttemptLatency)
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

	concluded, breach := Conclude(channel, result, execErr)
	if breach != BreachNone {
		// Not fatal to the delivery - the conclusion above is already the safe
		// one - but a channel that does this is wrong, and silence here would
		// keep it wrong.
		metrics.OutboundContractViolationsTotal.WithLabelValues("execute_attempt", string(breach)).Inc()
		log.Printf("outbound worker %s: %s broke its contract on attempt %s (%s); "+
			"recorded as %s", w.workerID, leased.Intent.Provider, begun.AttemptID,
			breach, concluded.Outcome())
	}

	// Counted here rather than after the record below, because this is where
	// the fact is: the call was made and it ended this way. Whether writing it
	// down succeeds is a different question, and the log line at the bottom is
	// what answers it.
	metrics.OutboundAttemptsTotal.WithLabelValues(w.family, leased.Intent.Provider,
		string(begun.Operation), string(concluded.Outcome()), errorClass(concluded)).Inc()

	finalizeCtx, cancelFinalize := recording(detached)
	recorded, err := w.store.FinalizeDeliveryAttempt(finalizeCtx, FinalizeRequest{
		AttemptID:  begun.AttemptID,
		LeaseToken: leased.LeaseToken,
		Conclusion: concluded,
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
	// this attempt, and that is worth saying out loud. Whether it is a BUG is a
	// separate question - see brokenContract.
	if kind, broken := brokenContract(recorded); broken {
		metrics.OutboundContractViolationsTotal.WithLabelValues("finalize", kind).Inc()
	}
	switch recorded.Outcome {
	case FinalizeFinalized, FinalizeIdempotentRepeat:
	default:
		log.Printf("outbound worker %s: the result of %s was answered %q",
			w.workerID, begun.AttemptID, recorded.Outcome)
	}

	// One line per attempt: this is where an operator sees that TokayOps is
	// still trying, and which delivery of which alert it is trying.
	//
	// What is NOT here is as deliberate as what is. No payload, no token, no
	// provider response body - the summary is the channel's own short
	// classification, already truncated where it was built (NFR-7), and the
	// receipt reference is coordinates rather than content.
	log.Printf("outbound worker %s: intent=%s provider=%s generation=%d attempt=%s "+
		"outcome=%s status=%s%s", w.workerID, leased.Intent.ID, leased.Intent.Provider,
		leased.Intent.GenerationNo, begun.AttemptID, concluded.Outcome(),
		recorded.To, detail(concluded))
}

// errorClass is the channel's own classification of a failure, and empty for a
// call that was accepted. It is a label, so it is a closed vocabulary rather
// than a message: an error string here would make a new time series out of
// every distinct failure text.
func errorClass(c Conclusion) string {
	if class := c.Completion().ErrorClass; class != nil {
		return *class
	}
	return ""
}

// detail is what the log line adds beyond the outcome, and only when there is
// something to add.
//
// Never the response body: what a provider says about a failure can carry
// anything, including the message that was being sent.
//
// And never the receipt REFERENCE, which is where this was wrong. A receipt ref
// is coordinates - a Slack channel and timestamp, a Telegram chat id - and for
// a direct message the channel IS a working address for the person. Written to
// a log, it outlives the erasure that removes the same value from every table,
// because a log is not something this system can go back and edit. What a
// reader actually needs from the line is whether the provider gave us anything
// to identify the message by, and that is a boolean.
func detail(c Conclusion) string {
	completion := c.Completion()
	out := ""
	if completion.ErrorClass != nil && *completion.ErrorClass != "" {
		out += " error_class=" + *completion.ErrorClass
	}
	out += fmt.Sprintf(" receipt_recorded=%t",
		completion.ReceiptRef != nil && *completion.ReceiptRef != "")
	if summary := c.Summary(); summary != "" {
		out += " response_summary=" + strconv.Quote(summary)
	}
	return out
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

// brokenContract says whether a finalisation that was not accepted indicates a
// BUG, as opposed to a race the design expects.
//
// The distinction was got wrong once and it matters, because this counter is
// meant to be zero: an alert on it says "go and read the log", and an alert
// that fires on ordinary operation stops being read.
//
// A lost lease is the ordinary case. Recovery reclaims an attempt whose lease
// ran out, the original worker finishes a moment later and its result arrives
// late - which is exactly what fencing is for, and the store keeps that result
// as a durable observation rather than throwing it away. A slow provider is
// enough to produce it.
//
// A lost lease with NOTHING recorded is the other thing entirely: the caller
// could not prove the result was theirs, so it was refused outright. That is a
// stranger finalising somebody else's attempt, and there is no benign way for
// it to happen.
func brokenContract(recorded FinalizeResult) (string, bool) {
	switch recorded.Outcome {
	case FinalizeFinalized, FinalizeIdempotentRepeat:
		return "", false
	case FinalizeLeaseLost:
		if recorded.ObservationRecorded {
			return "", false
		}
		return "foreign_token", true
	default:
		// A conflict is two contradicting accounts of one call; not_found is a
		// result for an attempt that does not exist. Neither is a race.
		return string(recorded.Outcome), true
	}
}
