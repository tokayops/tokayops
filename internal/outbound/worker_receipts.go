package outbound

import (
	"context"
	"log"

	"github.com/tokayops/tokayops/internal/metrics"
)

// pollReceipts asks the provider about the calls whose wait came round, and
// tries again the events that came in before anything here had made what they
// are about.
//
// One pass at a time, off the tick: a poll is a network call, and a handful of
// slow ones must not stand between this family's queue and its claims. A pass
// still running when the next tick comes is simply not started again.
func (w *Worker) pollReceipts(ctx context.Context) {
	if !w.polling.CompareAndSwap(false, true) {
		return
	}
	w.running.Add(1)
	go func() {
		defer w.running.Done()
		defer w.polling.Store(false)
		w.pollPass(context.WithoutCancel(ctx))
	}()
}

func (w *Worker) pollPass(ctx context.Context) {
	providers := make([]string, 0, len(w.receiving))
	for provider := range w.receiving {
		providers = append(providers, provider)
	}
	due, err := w.receipts.ClaimDueReceipts(ctx, w.family, providers, w.pool)
	if err != nil {
		log.Printf("outbound worker %s: claim the waits: %v", w.workerID, err)
		return
	}
	for _, waiting := range due {
		channel, served := w.receiving[waiting.Provider]
		if !served {
			// Another instance serves it; the next look is already
			// scheduled, and that instance will take it then.
			continue
		}
		for _, ref := range waiting.Effects {
			w.pollOne(ctx, channel, ref)
		}
		// The answers are in through the same door as any callback. What is
		// left is the question only the poll asks: is the wait over.
		result, err := w.receipts.ReviewReceiptWait(ctx, waiting.IntentID)
		if err != nil {
			log.Printf("outbound worker %s: review the wait of %s: %v", w.workerID, waiting.IntentID, err)
			continue
		}
		if result.To != "" {
			log.Printf("outbound worker %s: the wait of %s ended: -> %s (%s)",
				w.workerID, waiting.IntentID, result.To, result.Row)
		}
	}

	unmatched, err := w.receipts.UnmatchedProviderEvents(ctx, w.pool)
	if err != nil {
		log.Printf("outbound worker %s: read the unmatched events: %v", w.workerID, err)
		return
	}
	for _, id := range unmatched {
		if _, err := w.receipts.ApplyProviderEvent(ctx, id, w.translators); err != nil {
			log.Printf("outbound worker %s: apply event %s: %v", w.workerID, id, err)
		}
	}
}

// pollOne asks about one object and applies the answer like a callback. The
// answer becomes an event of its own, named by the object and the status it
// reports: asking twice and hearing the same is the same event.
func (w *Worker) pollOne(ctx context.Context, channel ReceiptChannel, ref EffectRef) {
	policy, _ := ReceiptPolicyOf(ref.Provider)
	pollCtx, cancel := context.WithTimeout(ctx, policy.PollDeadline)
	event, err := channel.Poll(pollCtx, ref)
	cancel()
	if err != nil {
		metrics.OutboundReceiptPollsTotal.WithLabelValues(w.family, "failed").Inc()
		log.Printf("outbound worker %s: poll %s of %s: %v", w.workerID, ref.ExternalRef, ref.IntentID, err)
		return
	}
	metrics.OutboundReceiptPollsTotal.WithLabelValues(w.family, "answered").Inc()

	event.AttemptID, event.ExternalRef = ref.AttemptID, ref.ExternalRef
	event.EventID = "poll:" + ref.ExternalRef + ":" + event.ProviderStatus
	event.Sequence = nil
	id, err := w.receipts.RecordProviderEvent(ctx, event)
	if err != nil {
		log.Printf("outbound worker %s: keep the answer about %s: %v", w.workerID, ref.ExternalRef, err)
		return
	}
	if _, err := w.receipts.ApplyProviderEvent(ctx, id, w.translators); err != nil {
		log.Printf("outbound worker %s: apply the answer about %s: %v", w.workerID, ref.ExternalRef, err)
	}
}
