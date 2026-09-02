package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What the worker is responsible for is arithmetic and order: it takes exactly
// the work it can run, in an order that serves everybody, and it records what
// happened even while it is being shut down. The tests drive tick() and serve()
// directly rather than the ticker - a test that waits for a real second is a
// test that eventually flakes, and none of these are about the clock.

type fakeStore struct {
	mu sync.Mutex

	due      []ProviderDue
	claimErr error
	beginOut BeginAttemptResult
	beginErr error

	// available is what each provider's two queues really hold, which is what
	// makes an undershoot possible: the aggregate says one thing and the claim
	// finds another. They are separate because the store's two phases are:
	// asking for first attempts cannot return a retry, and a claim for
	// anything takes the oldest first, which is the retries.
	available map[string]*queues

	claims    []ClaimRequest
	begins    []BeginAttemptRequest
	finalized []FinalizeRequest
	expiries  int
	recovers  int
	nextID    int

	// expired and recovered are what housekeeping answers, for the tests about
	// what the worker says and counts when it finds abandoned work.
	expired   []Expired
	recovered []Recovered
}

// queues is one provider's work, split the way the store splits it.
type queues struct {
	fresh   int
	retries int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		available: map[string]*queues{},
		beginOut: BeginAttemptResult{
			Outcome:              BeginStarted,
			AttemptKind:          AttemptCreate,
			Operation:            OperationSend,
			BoundEndpoint:        "C-bound",
			ProviderKey:          "create-key",
			Content:              mustSnapshotContent(3),
			PayloadSchemaVersion: 1,
			Payload:              json.RawMessage(`{"text":"disk will fill"}`),
		},
	}
}

func (f *fakeStore) ExpireDueIntents(context.Context, string, int) ([]Expired, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expiries++
	return f.expired, nil
}

func (f *fakeStore) RecoverStaleAttempts(context.Context, string, int) ([]Recovered, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recovers++
	return f.recovered, nil
}

func (f *fakeStore) DueSnapshot(context.Context, string) ([]ProviderDue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.due, nil
}

func (f *fakeStore) ClaimDueIntents(_ context.Context, req ClaimRequest) ([]Leased, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.claims = append(f.claims, req)
	if f.claimErr != nil {
		return nil, f.claimErr
	}

	give := req.Limit
	if have, tracked := f.available[req.Provider]; tracked {
		switch req.Phase {
		case ClaimFirstAttempts:
			if give > have.fresh {
				give = have.fresh
			}
			have.fresh -= give
		default:
			// Oldest first, which is the retries, and whatever is left of the
			// share comes from the untried work.
			taken := give
			if taken > have.retries {
				taken = have.retries
			}
			have.retries -= taken
			rest := give - taken
			if rest > have.fresh {
				rest = have.fresh
			}
			have.fresh -= rest
			give = taken + rest
		}
	}
	if give < 0 {
		give = 0
	}

	leased := make([]Leased, 0, give)
	for i := 0; i < give; i++ {
		f.nextID++
		leased = append(leased, Leased{
			Intent: Intent{
				KeyKind: keys.KindEscalation,
				ID:      fmt.Sprintf("intent-%d", f.nextID), Provider: req.Provider,
				Status: StatusPending, AlertGroupID: "ag-1",
			},
			LeaseToken:  fmt.Sprintf("token-%d", f.nextID),
			LockedUntil: time.Now().Add(req.Lease),
		})
	}
	return leased, nil
}

func (f *fakeStore) BeginAttempt(_ context.Context,
	req BeginAttemptRequest) (BeginAttemptResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins = append(f.begins, req)
	if f.beginErr != nil {
		return BeginAttemptResult{}, f.beginErr
	}
	if req.Preparation != PreparationReady {
		return BeginAttemptResult{Outcome: BeginPreparedRetry}, nil
	}
	out := f.beginOut
	out.AttemptID = "attempt-for-" + req.IntentID
	return out, nil
}

func (f *fakeStore) FinalizeDeliveryAttempt(ctx context.Context,
	req FinalizeRequest) (FinalizeResult, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return FinalizeResult{}, err
	}
	f.finalized = append(f.finalized, req)
	return FinalizeResult{Outcome: FinalizeFinalized}, nil
}

func (f *fakeStore) counts() (claimed, begun, finalized int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.claims), len(f.begins), len(f.finalized)
}

// fakeChannel is one provider that does what the test tells it to.
type fakeChannel struct {
	mu sync.Mutex

	preparation Preparation
	result      Result
	execErr     error
	outcome     Outcome
	class       string
	known       bool

	// block holds ExecuteAttempt until it is closed, which is how the tests
	// look at a worker with attempts genuinely in flight.
	block chan struct{}

	calls []Call
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{
		preparation: Ready("C-proposed"),
		result: Result{
			Evidence: ProviderResponse, Status: "ok",
			Receipt: mustReceipt("C-bound/1700000000.000100",
				`{"channel":"C-bound","ts":"1700000000.000100"}`),
		},
		outcome: OutcomeAccepted,
		known:   true,
	}
}

func mustReceipt(ref, raw string) Receipt {
	receipt, err := NewReceipt(ref, json.RawMessage(raw))
	if err != nil {
		panic(err)
	}
	return receipt
}

func (c *fakeChannel) Prepare(context.Context, Intent) Preparation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.preparation
}

func (c *fakeChannel) ExecuteAttempt(ctx context.Context, call Call) (Result, error) {
	c.mu.Lock()
	block := c.block
	c.calls = append(c.calls, call)
	result, err := c.result, c.execErr
	c.mu.Unlock()

	if block != nil {
		<-block
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return Result{}, errors.New("an attempt was made with no deadline")
	}
	return result, err
}

func (c *fakeChannel) ClassifyResponse(Result) (Classification, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Classification{Outcome: c.outcome, Class: c.class}, c.known
}

func (c *fakeChannel) made() []Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Call(nil), c.calls...)
}

func testWorker(store outboundStore, channels map[string]Channel) *Worker {
	w := NewWorker(store, "worker-1", channels)
	return w
}

// TestATickTakesOnlyWhatItCanRun is the rule the first design got wrong. A
// claim for more than there are slots is a lease sitting in a local queue while
// it expires - and an expired lease means somebody else redoes a call that may
// already have gone out.
func TestATickTakesOnlyWhatItCanRun(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 100, ClaimableFresh: 100}}
	store.available["slack"] = &queues{fresh: 100}

	channel := newFakeChannel()
	channel.block = make(chan struct{})
	w := testWorker(store, map[string]Channel{"slack": channel})

	w.tick(context.Background())

	if got := int(w.inflight.Load()); got != NotificationPoolSize {
		t.Fatalf("the tick started %d attempts for a pool of %d", got, NotificationPoolSize)
	}
	claimed, _, _ := store.counts()
	if claimed == 0 {
		t.Fatal("nothing was claimed")
	}
	total := 0
	store.mu.Lock()
	for _, c := range store.claims {
		total += c.Limit
	}
	store.mu.Unlock()
	if total > NotificationPoolSize {
		t.Fatalf("the tick asked for %d rows with %d slots", total, NotificationPoolSize)
	}

	// A second tick while they are all still running takes nothing at all.
	before := len(store.claims)
	w.tick(context.Background())
	if len(store.claims) != before {
		t.Fatalf("a full pool still went to the queue: %d claims", len(store.claims)-before)
	}

	// And the housekeeping ran on both ticks: it is mostly about work other
	// instances abandoned, and a busy pool is no reason to stop looking.
	if store.expiries != 2 || store.recovers != 2 {
		t.Fatalf("housekeeping ran %d/%d times in two ticks", store.expiries, store.recovers)
	}

	close(channel.block)
	w.running.Wait()
}

// TestWorkNobodyHereCanDoIsLeftAlone. What this instance was configured with is
// not a property of the commitment: taking a Slack page on an instance with no
// Slack channel would hold the lease until it expired and deliver nothing.
func TestWorkNobodyHereCanDoIsLeftAlone(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{
		{Provider: "slack", ClaimableDue: 5, ClaimableFresh: 5},
		{Provider: "telegram", ClaimableDue: 5, ClaimableFresh: 5},
	}
	store.available["telegram"] = &queues{fresh: 5}

	w := testWorker(store, map[string]Channel{"telegram": newFakeChannel()})
	w.tick(context.Background())
	w.running.Wait()

	store.mu.Lock()
	defer store.mu.Unlock()
	for _, claim := range store.claims {
		if claim.Provider != "telegram" {
			t.Fatalf("claimed %s work on an instance that cannot send it", claim.Provider)
		}
	}
	if len(store.claims) == 0 {
		t.Fatal("the provider this instance does serve was not claimed either")
	}
}

// TestAnAttemptIsMadeFromWhatTheStoreSaid: the address, the key and the
// revision come back from BeginAttempt, and the handler is told those - not the
// address it proposed a moment earlier.
func TestAnAttemptIsMadeFromWhatTheStoreSaid(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1}}
	store.available["slack"] = &queues{fresh: 1}

	channel := newFakeChannel()
	w := testWorker(store, map[string]Channel{"slack": channel})
	w.tick(context.Background())
	w.running.Wait()

	calls := channel.made()
	if len(calls) != 1 {
		t.Fatalf("%d calls were made", len(calls))
	}
	call := calls[0]
	if call.Endpoint != "C-bound" || call.ProviderKey != "create-key" {
		t.Fatalf("the call went to %q under %q", call.Endpoint, call.ProviderKey)
	}
	if revision, has := call.Content.Revision(); !has || revision != 3 {
		t.Fatalf("the call carried revision %d (present: %v)", revision, has)
	}
	if call.AttemptKind != AttemptCreate || call.Operation != OperationSend {
		t.Fatalf("the call is a %s/%s", call.AttemptKind, call.Operation)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.begins) != 1 || store.begins[0].BoundEndpoint != "C-proposed" {
		t.Fatalf("the store was asked with %+v", store.begins)
	}
	if len(store.finalized) != 1 {
		t.Fatalf("%d results were recorded", len(store.finalized))
	}
	recorded := store.finalized[0]
	if recorded.Conclusion.Outcome() != OutcomeAccepted {
		t.Fatalf("the result was recorded as %q", recorded.Conclusion.Outcome())
	}
	if recorded.Conclusion.Completion().AppliedRevision != nil {
		t.Fatal("the worker told the store which revision the attempt applied")
	}
	if len(recorded.Conclusion.Receipt()) == 0 {
		t.Fatal("the coordinates the provider returned were not recorded")
	}
}

// TestARefusedPreparationNeverReachesTheProvider. The refusal is recorded - it
// is the proof that no call happened - and the provider is not touched.
func TestARefusedPreparationNeverReachesTheProvider(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1}}
	store.available["slack"] = &queues{fresh: 1}

	channel := newFakeChannel()
	channel.preparation = Impossible("identity_not_linked", "nobody has linked this user")
	w := testWorker(store, map[string]Channel{"slack": channel})
	w.tick(context.Background())
	w.running.Wait()

	if calls := channel.made(); len(calls) != 0 {
		t.Fatalf("%d calls were made after a preparation that refused", len(calls))
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.begins) != 1 {
		t.Fatalf("%d attempts were opened", len(store.begins))
	}
	asked := store.begins[0]
	if asked.Preparation != PreparationPermanent || asked.ErrorClass != "identity_not_linked" {
		t.Fatalf("the refusal was reported as %+v", asked)
	}
	if len(store.finalized) != 0 {
		t.Fatal("a call that never happened was finalised")
	}
}

// TestTheWorkerDoesNotLetAChannelClassifyASilence is the fold seen from
// outside. The channel here is one that would call a request that may have gone
// out a clean success - the mistake that turns a page nobody received into a
// delivery - and the worker never gives it the chance.
func TestTheWorkerDoesNotLetAChannelClassifyASilence(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1}}
	store.available["slack"] = &queues{fresh: 1}

	channel := newFakeChannel()
	channel.result = Result{Evidence: PossiblySent}
	channel.execErr = errors.New("read tcp: i/o timeout")
	channel.outcome, channel.known = OutcomeAccepted, true

	w := testWorker(store, map[string]Channel{"slack": channel})
	w.tick(context.Background())
	w.running.Wait()

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.finalized) != 1 {
		t.Fatalf("%d results were recorded", len(store.finalized))
	}
	recorded := store.finalized[0]
	if recorded.Conclusion.Outcome() != OutcomeAmbiguous {
		t.Fatalf("a request that may have gone out was recorded as %q",
			recorded.Conclusion.Outcome())
	}
	// And the account of it survives, which is all the journal will have.
	if recorded.Conclusion.Summary() == "" {
		t.Fatal("nothing was recorded about why the call ended")
	}
}

// TestAStoppingWorkerStillRecordsWhatHappened is the whole reason the recording
// runs on its own context. A shutdown that cancelled it would throw away a
// result the provider had already accepted - and the retry that follows is the
// duplicate page this domain exists to avoid.
func TestAStoppingWorkerStillRecordsWhatHappened(t *testing.T) {
	store := newFakeStore()
	channel := newFakeChannel()
	channel.block = make(chan struct{})

	w := testWorker(store, map[string]Channel{"slack": channel})
	ctx, stop := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.serve(ctx, Leased{
			Intent:     Intent{ID: "intent-1", KeyKind: keys.KindEscalation, Provider: "slack", Status: StatusPending},
			LeaseToken: "token-1",
		})
	}()

	// The worker is told to stop while the call is in flight.
	for {
		if len(channel.made()) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	close(channel.block)
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.finalized) != 1 {
		t.Fatal("the result of a call made before the shutdown was thrown away")
	}
	if store.finalized[0].Conclusion.Outcome() != OutcomeAccepted {
		t.Fatalf("it was recorded as %q", store.finalized[0].Conclusion.Outcome())
	}
}

// TestAnUndershotClaimIsOfferedToSomebodyElse. The aggregate is a moment old by
// the time the claim runs, and another instance may have taken the rows in
// between. The slots it could not fill go back into the pool rather than idling
// until the next tick.
func TestAnUndershotClaimIsOfferedToSomebodyElse(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{
		{Provider: "slack", ClaimableDue: 50, ClaimableFresh: 50},
		{Provider: "telegram", ClaimableDue: 50, ClaimableFresh: 50},
	}
	// Slack's backlog is gone by the time the claim arrives; Telegram's is real.
	store.available["slack"] = &queues{}
	store.available["telegram"] = &queues{fresh: 50}

	channel := newFakeChannel()
	channel.block = make(chan struct{})
	w := testWorker(store, map[string]Channel{"slack": channel, "telegram": channel})
	w.tick(context.Background())

	// An equal split would have run four. The redistribution is what fills the
	// rest; the last slot may still be spent asking the empty provider, and a
	// pass that comes back with nothing is where the loop is meant to stop.
	if got := int(w.inflight.Load()); got < NotificationPoolSize-1 {
		t.Fatalf("the tick ran %d attempts of a pool of %d while one provider had "+
			"nothing and the other had plenty", got, NotificationPoolSize)
	}
	for _, claim := range store.claims {
		if claim.Provider == "telegram" {
			continue
		}
		if claim.Limit > NotificationPoolSize/2 {
			t.Fatalf("the empty provider was offered %d slots", claim.Limit)
		}
	}

	close(channel.block)
	w.running.Wait()
}

// TestAFreshShareThatCameBackEmptyDoesNotMaskTheRetries.
//
// The aggregate said five untried and five waiting. Between the aggregate and
// the claim another instance took every untried one - so the share reserved for
// them comes back with nothing, and the question is what the next pass believes.
// Counting only the rows that arrived leaves the tick convinced there are still
// five untried, so it splits the remaining slots towards an empty queue again
// and again, while the retries sit there and the pool runs at a third of its
// capacity.
func TestAFreshShareThatCameBackEmptyDoesNotMaskTheRetries(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 10, ClaimableFresh: 5}}
	store.available["slack"] = &queues{fresh: 0, retries: 5}

	channel := newFakeChannel()
	channel.block = make(chan struct{})
	w := testWorker(store, map[string]Channel{"slack": channel})
	w.tick(context.Background())

	if got := int(w.inflight.Load()); got != 5 {
		t.Fatalf("the tick took %d of the 5 commitments that were actually there, "+
			"with %d slots free", got, NotificationPoolSize)
	}

	close(channel.block)
	w.running.Wait()
}

// TestAQueueThatCannotGiveAnythingEndsTheTick: when nothing can be claimed at
// all, the pass stops instead of asking again forever.
func TestAQueueThatCannotGiveAnythingEndsTheTick(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 50, ClaimableFresh: 50}}
	store.available["slack"] = &queues{}

	w := testWorker(store, map[string]Channel{"slack": newFakeChannel()})

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w.tick(context.Background())
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the tick kept asking a queue that had nothing to give")
	}
	if got := int(w.inflight.Load()); got != 0 {
		t.Fatalf("%d attempts started from an empty queue", got)
	}
}

// TestAWorkerStopsWhenItIsTold. Nothing in flight, so the only thing being
// proved is that stopping is not a wait for the shutdown deadline.
func TestAWorkerStopsWhenItIsTold(t *testing.T) {
	w := testWorker(newFakeStore(), map[string]Channel{"slack": newFakeChannel()})

	ctx, stop := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		w.Run(ctx)
	}()

	stop()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a worker with nothing in flight waited to be told twice")
	}
}

// TestRunHoldsUntilTheAnswerIsRecorded. Everything above drives tick() and
// serve(); this one drives Run, because the property is about Run RETURNING.
//
// Whoever starts the worker has to be able to wait for it, and waiting only
// means something if Run does not come back while a call it has already made is
// still being written down. A process that exits there leaves the delivery
// ambiguous: the message may have gone out, nothing says so, and somebody has
// to decide. This is what the join in cmd/tokayops guards, and the container's
// stop grace period has to outlast it.
func TestRunHoldsUntilTheAnswerIsRecorded(t *testing.T) {
	store := newFakeStore()
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1}}
	store.available["slack"] = &queues{fresh: 1}

	channel := newFakeChannel()
	channel.block = make(chan struct{})

	w := testWorker(store, map[string]Channel{"slack": channel})
	ctx, stop := context.WithCancel(context.Background())

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		w.Run(ctx)
	}()

	// Wait until a call is genuinely out at the provider.
	deadline := time.Now().Add(5 * time.Second)
	for len(channel.made()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no attempt was ever started")
		}
		time.Sleep(time.Millisecond)
	}

	// SIGTERM, as far as the worker is concerned.
	stop()

	select {
	case <-returned:
		t.Fatal("Run returned while a call it had made was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	if _, _, finalized := store.counts(); finalized != 0 {
		t.Fatalf("%d results were recorded before the provider answered", finalized)
	}

	// The provider answers, a second after the shutdown began.
	close(channel.block)

	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the call it was holding finished")
	}

	if _, _, finalized := store.counts(); finalized != 1 {
		t.Fatalf("%d results were recorded, so an answer that arrived during the "+
			"shutdown was thrown away", finalized)
	}
	if got := store.finalized[0].Conclusion.Outcome(); got != OutcomeAccepted {
		t.Fatalf("the answer was recorded as %q", got)
	}
}

// TestTheAttemptLogSaysWhatHappenedWithoutSayingWhatWasSent.
//
// One line per attempt is where an operator sees that TokayOps is still trying,
// and which delivery of which alert it is trying. It is also the easiest place
// in the system to leak: the payload is right there, and so is whatever the
// provider chose to say back.
//
// So the line is pinned by what it must contain AND by what it must not, and
// the second list grew after review: the provider's own summary reads "accepted
// with channel=D0123" on some paths, which is an address in prose, and a
// receipt reference is coordinates outright. Both live in the attempt's record,
// where erasure can remove them. A log cannot be edited afterwards.
func TestTheAttemptLogSaysWhatHappenedWithoutSayingWhatWasSent(t *testing.T) {
	store := newFakeStore()
	store.beginOut.Payload = json.RawMessage(`{"text":"disk will fill on db-primary"}`)

	longTail := "TAIL-THAT-MUST-BE-CUT"
	channel := newFakeChannel()
	channel.result = Result{
		Evidence: ProviderResponse,
		Status:   "ok",
		Summary:  strings.Repeat("a", SummaryLimit) + longTail,
		Receipt:  mustReceipt("C-bound/1700000000.000100", `{"channel":"C-bound"}`),
	}

	var written bytes.Buffer
	log.SetOutput(&written)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	w := testWorker(store, map[string]Channel{"slack": channel})
	w.serve(context.Background(), Leased{
		Intent: Intent{
			KeyKind: keys.KindEscalation,
			ID:      "intent-7", Provider: "slack", Status: StatusPending,
			AlertGroupID: "ag-1", GenerationNo: 2,
		},
		LeaseToken: "token-7",
	})

	line := written.String()
	for _, want := range []string{
		"intent=intent-7", "provider=slack", "generation=2",
		"attempt=attempt-for-intent-7", "outcome=accepted",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the attempt line does not say %s:\n%s", want, line)
		}
	}

	for _, forbidden := range []string{
		"disk will fill", // the payload
		"token-7",        // the lease
		"aaaa",           // the provider's own words, which can name a channel
		longTail,
		"C-bound", // the coordinates of the message it made
	} {
		if strings.Contains(line, forbidden) {
			t.Errorf("the attempt line leaked %q:\n%s", forbidden, line)
		}
	}
}

func counterValue(t *testing.T, c *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	counter, err := c.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func histogramCount(t *testing.T, h *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	observer, err := h.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("read the histogram: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("read the histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestALateResultIsNotABug.
//
// A lost lease is what fencing is FOR: recovery reclaims an attempt whose lease
// ran out, the original worker finishes a moment later, and the store keeps its
// answer as a durable observation. A slow provider is enough to produce it.
//
// Counted as a contract violation - which it was - a busy afternoon would raise
// an alert that says "there is a bug here", and an alert that fires on ordinary
// operation is an alert nobody reads. A lost lease with NOTHING recorded is the
// other thing entirely: somebody finalising an attempt they cannot prove is
// theirs.
func TestALateResultIsNotABug(t *testing.T) {
	cases := []struct {
		name      string
		result    FinalizeResult
		wantKind  string
		violation bool
	}{
		{
			name:   "the result arrived late and was kept",
			result: FinalizeResult{Outcome: FinalizeLeaseLost, ObservationRecorded: true},
		},
		{
			name:      "a stranger finalised somebody else's attempt",
			result:    FinalizeResult{Outcome: FinalizeLeaseLost},
			wantKind:  "foreign_token",
			violation: true,
		},
		{
			name:      "two contradicting accounts of one call",
			result:    FinalizeResult{Outcome: FinalizeConflict},
			wantKind:  "conflict",
			violation: true,
		},
		{
			name:      "a result for an attempt that does not exist",
			result:    FinalizeResult{Outcome: FinalizeNotFound},
			wantKind:  "not_found",
			violation: true,
		},
		{name: "the ordinary case", result: FinalizeResult{Outcome: FinalizeFinalized}},
		{name: "a repeat after a lost reply", result: FinalizeResult{Outcome: FinalizeIdempotentRepeat}},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			kind, broken := brokenContract(tt.result)
			if broken != tt.violation {
				t.Fatalf("%q counted as a contract violation: %v, want %v",
					tt.result.Outcome, broken, tt.violation)
			}
			if kind != tt.wantKind {
				t.Errorf("counted under kind %q, want %q", kind, tt.wantKind)
			}
		})
	}
}

// TestTheLatencyOfACallThatNeverCameBackIsStillMeasured.
//
// The histogram is defined as admitted_at to started_at, and both are committed
// before the provider is even called. Observed after the call - as this first
// did - a provider that hangs and a process that dies would remove exactly the
// worst measurements from a distribution about how long a page takes to go out.
// The metric would then look healthiest at the moment it stopped being true.
func TestTheLatencyOfACallThatNeverCameBackIsStillMeasured(t *testing.T) {
	latency := 3.5
	store := newFakeStore()
	store.beginOut.FirstAttemptLatency = &latency
	store.due = []ProviderDue{{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1}}
	store.available["slack"] = &queues{fresh: 1}

	channel := newFakeChannel()
	channel.block = make(chan struct{})

	before := histogramCount(t, metrics.OutboundAdmissionLatencySeconds, FamilyNotification)

	w := testWorker(store, map[string]Channel{"slack": channel})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	w.tick(ctx)

	// The call is in flight and will never come back on its own.
	deadline := time.Now().Add(5 * time.Second)
	for len(channel.made()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no attempt was started")
		}
		time.Sleep(time.Millisecond)
	}

	if got := histogramCount(t, metrics.OutboundAdmissionLatencySeconds, FamilyNotification); got != before+1 {
		t.Fatalf("the histogram holds %d observations, want %d: a call that has not "+
			"answered yet is exactly the one worth measuring", got, before+1)
	}

	close(channel.block)
	w.running.Wait()
}

// mustSnapshotContent is the content a real BeginAttempt hands back for a
// commitment drawn from a frozen state.
func mustSnapshotContent(revision int64) AttemptContent {
	state, err := keys.NewRenderSnapshot(keys.SnapshotInput{
		AlertGroupID: "ag-1", Status: keys.GroupTriggered, Title: "Disk filling up",
		Severity: "critical", Revision: revision, DisplayTimezone: "UTC",
		Alerts: []keys.AlertSnapshot{{
			Fingerprint: "fp-1", Status: keys.AlertFiring,
			StartsAt: time.Unix(1700000000, 0).UTC(), AlertName: "DiskWillFill",
		}},
	})
	if err != nil {
		panic(err)
	}
	content, err := NewSnapshotContent(state, false)
	if err != nil {
		panic(err)
	}
	return content
}
