package outbound

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
)

// Retention: the history of finished deliveries has a term.
//
// What goes is a terminal commitment - succeeded, permanent_failed, expired,
// canceled - older than the window, with its attempts, observations and
// lifecycle events; and an alert event the fan-out finished longer ago than the
// window, once no claim on it holds a commitment any more. What stays is
// everything that is still work or still a claim: live commitments of any
// age, the claims themselves (a deleted claim would admit the same work
// twice), the group's snapshot, the alert and its timeline, and events the
// fan-out has not taken yet.
//
// One store command does one chunk in one transaction under an advisory lock;
// the loop repeats it while chunks come back full, up to a bound, once an
// hour - and once a minute after the start, so that setting the window has a
// visible effect before the hour is out.

const (
	// RetentionDefaultDays is the window when the environment says nothing:
	// a month, which is how long a question about a delivery takes to arrive.
	RetentionDefaultDays = 30
	// RetentionEnv is the variable the window is read from. Zero turns the
	// sweep off; a negative or non-numeric value refuses the start.
	RetentionEnv = "TOKAY_DELIVERY_RETENTION_DAYS"

	// RetentionChunk is how many commitments one transaction removes.
	RetentionChunk = 1000
	// RetentionMaxChunks bounds one pass: a database with a year of history
	// loses a hundred thousand commitments an hour rather than all of them in
	// one transaction.
	RetentionMaxChunks = 100
	// RetentionInterval is how often a pass runs.
	RetentionInterval = time.Hour
	// RetentionFirstPassAfter is when the first pass runs after the start.
	RetentionFirstPassAfter = time.Minute
)

// ParseRetentionWindow reads the window out of the environment's value: an
// empty value is the default, zero is off, and anything else that is not a
// non-negative integer is refused with the text the operator needs.
func ParseRetentionWindow(value string) (days int, err error) {
	if value == "" {
		return RetentionDefaultDays, nil
	}
	days, err = strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: the retention window is a whole number of days (0 turns retention off)",
			RetentionEnv, value)
	}
	if days < 0 {
		return 0, fmt.Errorf("%s=%d: the retention window cannot be negative (0 turns retention off)",
			RetentionEnv, days)
	}
	return days, nil
}

// SweepCounts is what one chunk removed, by table.
type SweepCounts struct {
	Intents      int64
	Attempts     int64
	Observations int64
	Events       int64
	Outbox       int64
}

// Add is the running total of a pass.
func (c *SweepCounts) Add(other SweepCounts) {
	c.Intents += other.Intents
	c.Attempts += other.Attempts
	c.Observations += other.Observations
	c.Events += other.Events
	c.Outbox += other.Outbox
}

// SweepResult is one chunk's answer: busy when another instance holds the
// sweep, otherwise what was removed.
type SweepResult struct {
	Busy    bool
	Deleted SweepCounts
}

// retentionStore is the one command the loop needs.
type retentionStore interface {
	SweepDeliveryHistory(ctx context.Context, cutoff time.Time, chunk int) (SweepResult, error)
}

// Retention is the loop.
type Retention struct {
	store     retentionStore
	window    time.Duration
	interval  time.Duration
	first     time.Duration
	chunk     int
	maxChunks int
	now       func() time.Time
}

// NewRetention is a loop over a window of days. Zero days is not a loop: the
// caller does not start one.
func NewRetention(st retentionStore, days int) (*Retention, error) {
	if days <= 0 {
		return nil, fmt.Errorf("a retention loop needs a window of at least one day, got %d", days)
	}
	return &Retention{
		store:     st,
		window:    time.Duration(days) * 24 * time.Hour,
		interval:  RetentionInterval,
		first:     RetentionFirstPassAfter,
		chunk:     RetentionChunk,
		maxChunks: RetentionMaxChunks,
		now:       time.Now,
	}, nil
}

// Run passes once a minute after the start, then once an hour, until the
// context ends. It is not waited for on the way out: a chunk cut off rolls
// back whole.
func (r *Retention) Run(ctx context.Context) {
	log.Printf("outbound retention started: window %s, every %s, first pass in %s",
		r.window, r.interval, r.first)
	first := time.NewTimer(r.first)
	defer first.Stop()
	select {
	case <-ctx.Done():
		log.Printf("outbound retention stopped")
		return
	case <-first.C:
		r.Pass(ctx)
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("outbound retention stopped")
			return
		case <-ticker.C:
			r.Pass(ctx)
		}
	}
}

// PassOutcome is what a pass came to.
type PassOutcome string

const (
	// PassSwept is a pass that held the lock and committed every chunk it ran,
	// including one that found nothing to remove.
	PassSwept PassOutcome = "swept"
	// PassBusy is a pass that found another instance sweeping and left.
	PassBusy PassOutcome = "busy"
	// PassFailed is a pass a chunk of which failed; what earlier chunks
	// committed stays committed, and no success is recorded.
	PassFailed PassOutcome = "failed"
)

// Pass sweeps up to maxChunks chunks older than the window, and records the
// success - the timestamp the alerting rules watch - only for a pass that
// held the lock and reached every commit.
func (r *Retention) Pass(ctx context.Context) (PassOutcome, SweepCounts) {
	started := r.now()
	cutoff := started.Add(-r.window)
	var total SweepCounts
	for i := 0; i < r.maxChunks; i++ {
		if ctx.Err() != nil {
			return PassFailed, total
		}
		result, err := r.store.SweepDeliveryHistory(ctx, cutoff, r.chunk)
		if err != nil {
			log.Printf("outbound retention: chunk %d failed: %v", i+1, err)
			return PassFailed, total
		}
		if result.Busy {
			return PassBusy, total
		}
		total.Add(result.Deleted)
		countRetention(result.Deleted)
		if int(result.Deleted.Intents) < r.chunk && int(result.Deleted.Outbox) < r.chunk {
			break
		}
	}
	metrics.OutboundRetentionLastSuccess.WithLabelValues().Set(float64(r.now().Unix()))
	log.Printf("outbound retention: removed %d commitments, %d attempts, %d observations, %d events, %d outbox events older than %s in %s",
		total.Intents, total.Attempts, total.Observations, total.Events, total.Outbox,
		cutoff.UTC().Format(time.RFC3339), r.now().Sub(started).Round(time.Millisecond))
	return PassSwept, total
}

func countRetention(deleted SweepCounts) {
	for table, n := range map[string]int64{
		"outbound_intents":              deleted.Intents,
		"outbound_attempts":             deleted.Attempts,
		"outbound_attempt_observations": deleted.Observations,
		"outbound_intent_events":        deleted.Events,
		"event_outbox":                  deleted.Outbox,
	} {
		if n > 0 {
			metrics.OutboundRetentionDeletedTotal.WithLabelValues(table).Add(float64(n))
		}
	}
}

// RetentionTables is the closed set of tables the sweep removes rows from,
// for the counter's zero series.
func RetentionTables() []string {
	return []string{
		"outbound_intents", "outbound_attempts", "outbound_attempt_observations",
		"outbound_intent_events", "event_outbox",
	}
}
