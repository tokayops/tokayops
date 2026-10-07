package outbound

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
)

// receivingChannel is a channel that can read a provider's events and be
// asked about a call.
type receivingChannel struct {
	*fakeChannel
	mu     sync.Mutex
	polled []string
	answer string
	fail   bool
}

func (c *receivingChannel) EffectStateOf(e ProviderEvent) (EffectState, string, bool) {
	if e.ProviderStatus == "completed" {
		return EffectHappened, e.ProviderStatus, true
	}
	return EffectInProgress, e.ProviderStatus, true
}

func (c *receivingChannel) Poll(_ context.Context, ref EffectRef) (ProviderEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polled = append(c.polled, ref.ExternalRef)
	if c.fail {
		return ProviderEvent{}, errors.New("the provider did not answer")
	}
	return ProviderEvent{Provider: "phone-test", AccountScope: "AC1", ProviderStatus: c.answer}, nil
}

// receiptFakeStore is the worker's store with the receipt half on top.
type receiptFakeStore struct {
	*fakeStore
	mu       sync.Mutex
	due      []AwaitingReceipt
	recorded []ProviderEvent
	applied  []string
	reviewed []string
}

func (f *receiptFakeStore) ClaimDueReceipts(context.Context, string, []string, int) ([]AwaitingReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	due := f.due
	f.due = nil
	return due, nil
}

func (f *receiptFakeStore) RecordProviderEvent(_ context.Context, e ProviderEvent) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, e)
	return e.EventID, nil
}

func (f *receiptFakeStore) ApplyProviderEvent(_ context.Context, id string,
	translators map[string]EffectTranslator) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := translators["phone"]; !ok {
		return ApplyResult{}, errors.New("no translator for the provider")
	}
	f.applied = append(f.applied, id)
	return ApplyResult{Outcome: ApplyApplied}, nil
}

func (f *receiptFakeStore) ReviewReceiptWait(_ context.Context, intentID string) (ApplyResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviewed = append(f.reviewed, intentID)
	return ApplyResult{}, nil
}

func (f *receiptFakeStore) UnmatchedProviderEvents(context.Context, int) ([]string, error) {
	return nil, nil
}

// A family that waits for the provider's word cannot start without what that
// needs: a store that keeps the word, and channels that can read it.
func TestAWaitingFamilyRefusesToStartWithoutWhatItNeeds(t *testing.T) {
	receiving := &receivingChannel{fakeChannel: newFakeChannel()}

	if _, err := NewLaneWorker(FamilyNotification, LanePhone, newFakeStore(), "w", map[string]Channel{"phone": receiving}); err == nil ||
		!strings.Contains(err.Error(), "store") {
		t.Fatalf("a store that cannot keep the provider's word: %v", err)
	}
	store := &receiptFakeStore{fakeStore: newFakeStore()}
	if _, err := NewLaneWorker(FamilyNotification, LanePhone, store, "w", map[string]Channel{"phone": newFakeChannel()}); err == nil ||
		!strings.Contains(err.Error(), "phone") {
		t.Fatalf("a channel that cannot read the provider's word: %v", err)
	}
	if _, err := NewLaneWorker(FamilyNotification, LanePhone, store, "w", map[string]Channel{"phone": receiving}); err != nil {
		t.Fatalf("a family with what it needs: %v", err)
	}
	// And the families that never wait are not asked for any of it.
	if _, err := NewWorkerFor(FamilyNotification, newFakeStore(), "w", map[string]Channel{"slack": newFakeChannel()}); err != nil {
		t.Fatalf("paging asked for the receipt half: %v", err)
	}
}

// A wait that came round is asked about object by object; each answer goes in
// as an event named by the object and the status, and then the wait itself is
// reviewed.
func TestAPollAsksAboutEveryObjectAndThenReviews(t *testing.T) {
	receiving := &receivingChannel{fakeChannel: newFakeChannel(), answer: "completed"}
	store := &receiptFakeStore{fakeStore: newFakeStore(), due: []AwaitingReceipt{{
		IntentID: "intent-1", Provider: "phone",
		Effects: []EffectRef{
			{IntentID: "intent-1", AttemptID: "a1", ExternalRef: "CA1", Provider: "phone"},
			{IntentID: "intent-1", AttemptID: "a2", ExternalRef: "CA2", Provider: "phone"},
		},
	}}}
	w, err := NewLaneWorker(FamilyNotification, LanePhone, store, "w", map[string]Channel{"phone": receiving})
	if err != nil {
		t.Fatal(err)
	}
	w.pollPass(context.Background())

	if len(receiving.polled) != 2 {
		t.Fatalf("polled %v", receiving.polled)
	}
	if len(store.recorded) != 2 || store.recorded[0].EventID != "poll:CA1:completed" ||
		store.recorded[0].AttemptID != "a1" || store.recorded[0].Sequence != nil {
		t.Fatalf("the answers were kept as %+v", store.recorded)
	}
	if len(store.applied) != 2 {
		t.Fatalf("the answers applied: %v", store.applied)
	}
	if len(store.reviewed) != 1 || store.reviewed[0] != "intent-1" {
		t.Fatalf("the wait reviewed: %v", store.reviewed)
	}
}

// A poll that fails keeps nothing and still lets the wait be reviewed: a
// deadline that has passed ends it whatever the provider is doing.
func TestAFailedPollStillLetsTheWaitEnd(t *testing.T) {
	receiving := &receivingChannel{fakeChannel: newFakeChannel(), fail: true}
	store := &receiptFakeStore{fakeStore: newFakeStore(), due: []AwaitingReceipt{{
		IntentID: "intent-1", Provider: "phone",
		Effects: []EffectRef{{IntentID: "intent-1", AttemptID: "a1", ExternalRef: "CA1"}},
	}}}
	w, err := NewLaneWorker(FamilyNotification, LanePhone, store, "w", map[string]Channel{"phone": receiving})
	if err != nil {
		t.Fatal(err)
	}
	w.pollPass(context.Background())

	if len(store.recorded) != 0 {
		t.Fatalf("a failed poll kept %+v", store.recorded)
	}
	if len(store.reviewed) != 1 {
		t.Fatal("a failed poll kept the wait from being reviewed")
	}
}

// calledWithin waits for a channel to have been asked to send, or gives up.
func calledWithin(c *fakeChannel, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.calls)
		c.mu.Unlock()
		if n > 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// A storm of calls whose requests hang until their deadline holds every call
// slot, and a direct message that comes after it still goes out at once: calls
// run on a lane of their own, with a pool of their own.
func TestHungCallsDoNotHoldADirectMessage(t *testing.T) {
	store := &receiptFakeStore{fakeStore: newFakeStore()}
	store.fakeStore.due = []ProviderDue{
		{Provider: "phone", ClaimableDue: 20, ClaimableFresh: 20},
		{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1},
	}
	store.available = map[string]*queues{"phone": {fresh: 20}, "slack": {fresh: 1}}

	hung := &receivingChannel{fakeChannel: newFakeChannel()}
	hung.block = make(chan struct{})
	defer close(hung.block)
	slack := newFakeChannel()

	calls, err := NewLaneWorker(FamilyNotification, LanePhone, store, "calls", map[string]Channel{"phone": hung})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := NewWorkerFor(FamilyNotification, store, "messages", map[string]Channel{"slack": slack})
	if err != nil {
		t.Fatal(err)
	}

	calls.Tick(context.Background())
	if !calledWithin(hung.fakeChannel, time.Second) {
		t.Fatal("no call was placed")
	}
	if calls.inflight.Load() != int64(PhonePoolSize) {
		t.Fatalf("%d calls in flight, the call lane holds %d", calls.inflight.Load(), PhonePoolSize)
	}

	messages.Tick(context.Background())
	if !calledWithin(slack, time.Second) {
		t.Fatal("a direct message waited behind calls that hang")
	}
}

// The same storm through ONE pool shows what the lane prevents: the calls take
// every slot, and the message waits for a deadline.
func TestOnePoolForBothWouldHoldTheMessage(t *testing.T) {
	store := &receiptFakeStore{fakeStore: newFakeStore()}
	store.fakeStore.due = []ProviderDue{{Provider: "phone", ClaimableDue: 20, ClaimableFresh: 20}}
	store.available = map[string]*queues{"phone": {fresh: 20}, "slack": {}}

	hung := &receivingChannel{fakeChannel: newFakeChannel()}
	hung.block = make(chan struct{})
	defer close(hung.block)
	slack := newFakeChannel()

	shared, err := NewWorkerFor(FamilyNotification, store, "shared",
		map[string]Channel{"phone": hung, "slack": slack})
	if err != nil {
		t.Fatal(err)
	}
	shared.Tick(context.Background())
	if !calledWithin(hung.fakeChannel, time.Second) {
		t.Fatal("no call was placed")
	}

	// The message arrives after the calls took the slots.
	store.fakeStore.mu.Lock()
	store.fakeStore.due = append(store.fakeStore.due, ProviderDue{Provider: "slack", ClaimableDue: 1, ClaimableFresh: 1})
	store.available["slack"].fresh = 1
	store.fakeStore.mu.Unlock()
	shared.Tick(context.Background())
	if calledWithin(slack, 200*time.Millisecond) {
		t.Fatal("a shared pool let the message through; the lane test proves nothing")
	}
}

// The call lane counts its ticks under its own lane: a stopped call worker is
// a series of its own going flat, not hidden by the paging worker's ticks.
func TestTheCallLaneTicksUnderItsOwnName(t *testing.T) {
	store := &receiptFakeStore{fakeStore: newFakeStore()}
	calls, err := NewLaneWorker(FamilyNotification, LanePhone, store, "calls",
		map[string]Channel{"phone": &receivingChannel{fakeChannel: newFakeChannel()}})
	if err != nil {
		t.Fatal(err)
	}
	before := counterValue(t, metrics.OutboundWorkerTicksTotal, FamilyNotification, LanePhone)
	paging := counterValue(t, metrics.OutboundWorkerTicksTotal, FamilyNotification, LaneDefault)
	calls.Tick(context.Background())
	if got := counterValue(t, metrics.OutboundWorkerTicksTotal, FamilyNotification, LanePhone) - before; got != 1 {
		t.Fatalf("the call lane ticked %v times", got)
	}
	if got := counterValue(t, metrics.OutboundWorkerTicksTotal, FamilyNotification, LaneDefault) - paging; got != 0 {
		t.Fatalf("the call lane's tick was counted as paging's: %v", got)
	}
	if _, err := NewLaneWorker(FamilyHandoff, LanePhone, store, "x", nil); err == nil {
		t.Fatal("a lane no family has was built")
	}
}
