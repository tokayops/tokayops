package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// mockSyncerStore serves the one store read the syncer still makes: the Slack
// identities of the people the projection put on duty.
type mockSyncerStore struct {
	slackIDs map[string]string // userID -> slack external id ("" = not linked → skipped)
}

func (m *mockSyncerStore) GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error) {
	out := make(map[string][]*model.ExternalIdentity)
	for _, id := range userIDs {
		slackID, ok := m.slackIDs[id]
		if !ok || slackID == "" {
			continue
		}
		out[id] = []*model.ExternalIdentity{{UserID: id, Provider: "slack", ExternalID: slackID}}
	}
	return out, nil
}

// slackStub is a Slack API double recording every usergroups.users.update it is
// asked to perform. The syncer talks to a real slack.Client, so the seam is the
// HTTP endpoint rather than an interface.
type slackStub struct {
	mu      sync.Mutex
	updates []slackUpdate
	fail    bool

	server *httptest.Server
}

type slackUpdate struct {
	usergroup string
	users     string
}

func newSlackStub(t *testing.T) *slackStub {
	t.Helper()
	stub := &slackStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.updates = append(stub.updates, slackUpdate{
			usergroup: r.FormValue("usergroup"),
			users:     r.FormValue("users"),
		})
		failing := stub.fail
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if failing {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"usergroup": map[string]any{"id": r.FormValue("usergroup")},
		})
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *slackStub) recorded() []slackUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slackUpdate(nil), s.updates...)
}

func (s *slackStub) setFail(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func newTestSyncer(t *testing.T, stub *slackStub, oncall onCallLister, slackIDs map[string]string) *UsergroupSyncer {
	t.Helper()
	return &UsergroupSyncer{
		store:        &mockSyncerStore{slackIDs: slackIDs},
		oncall:       oncall,
		slackClient:  slack.New("test-token", slack.OptionAPIURL(stub.server.URL+"/")),
		syncInterval: time.Minute,
		cache:        make(map[string]cacheEntry),
	}
}

// usergroupDuty is a schedule with a usergroup configured in its snapshot.
func usergroupDuty(scheduleID, usergroup, groupID string, users ...string) schedulerender.ScheduleOnCall {
	return duty(dutySpec{
		scheduleID: scheduleID,
		usergroup:  usergroup,
		source:     schedulerender.SourceRotation,
		groupID:    groupID,
		users:      users,
	})
}

// TestSyncerSyncsUsergroupFromSnapshot is the closing of the second defect: the
// usergroup is read from the configuration in force. The old filter looked at a
// mutable column nothing on the revision path writes, so the sync ran happily
// over zero schedules.
func TestSyncerSyncsUsergroupFromSnapshot(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(usergroupDuty("sched-1", "S12345", "g-a", "alice", "bob"))
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice", "bob"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	got := stub.recorded()
	if len(got) != 1 {
		t.Fatalf("%d Slack updates, want 1: %+v", len(got), got)
	}
	if got[0].usergroup != "S12345" {
		t.Errorf("usergroup = %q, want the one from the snapshot", got[0].usergroup)
	}
	if got[0].users != "U-ALICE,U-BOB" {
		t.Errorf("members = %q, want both on-call users sorted", got[0].users)
	}
}

// TestSyncerSkipsSchedulesWithoutUsergroup: the filter is in Go, over the
// projection, because the projection is the only place the usergroup is stated.
func TestSyncerSkipsSchedulesWithoutUsergroup(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(
		rotationDuty("sched-none", "g-a", "alice"),
		usergroupDuty("sched-1", "S12345", "g-a", "alice"),
	)
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	got := stub.recorded()
	if len(got) != 1 || got[0].usergroup != "S12345" {
		t.Fatalf("Slack updates = %+v, want only the schedule with a usergroup", got)
	}
}

// TestSyncerAppliesOverrideComposition: the projection already overlaid the
// override, so the group holds the stand-in with no override branch here.
func TestSyncerAppliesOverrideComposition(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	standIn := duty(dutySpec{
		scheduleID: "sched-1", usergroup: "S12345",
		source: schedulerender.SourceOverride, groupID: "ovr-1", users: []string{"carol"},
	})
	oncall.set(standIn)
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice", "carol"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	got := stub.recorded()
	if len(got) != 1 || got[0].users != "U-CAROL" {
		t.Fatalf("Slack updates = %+v, want the override holder", got)
	}
}

// TestSyncerIgnoresDeletedSchedules: deleting a schedule has never emptied its
// usergroup, and this is not where that changes. The projection reports the
// deleted row because the notifier needs it; the syncer drops it in one line.
func TestSyncerIgnoresDeletedSchedules(t *testing.T) {
	stub := newSlackStub(t)
	deletedAt := dutyBase.Add(time.Hour)
	oncall := &fakeOnCall{}
	oncall.set(duty(dutySpec{scheduleID: "sched-1", usergroup: "S12345", deletedAt: &deletedAt}))
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("Slack was called for a deleted schedule: %+v", got)
	}
}

// TestSyncerSkipsWhenNobodyIsOnCall: an empty group is not synced as an empty
// membership - between shifts the group keeps whoever it had.
func TestSyncerSkipsWhenNobodyIsOnCall(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(duty(dutySpec{scheduleID: "sched-1", usergroup: "S12345"}))
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("Slack was called with nobody on duty: %+v", got)
	}
}

// TestSyncerSkipsUsersWithoutSlackIdentity: someone with no Slack identity
// cannot be a member of a Slack group, and the rest of the group still syncs.
func TestSyncerSkipsUsersWithoutSlackIdentity(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(usergroupDuty("sched-1", "S12345", "g-a", "alice", "bob"))
	syncer := newTestSyncer(t, stub, oncall, map[string]string{"alice": "U-ALICE"})

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	got := stub.recorded()
	if len(got) != 1 || got[0].users != "U-ALICE" {
		t.Fatalf("Slack updates = %+v, want alice alone", got)
	}

	// Nobody linked at all: the group is left as it stands rather than emptied.
	stub2 := newSlackStub(t)
	syncer2 := newTestSyncer(t, stub2, oncall, map[string]string{})
	if err := syncer2.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := stub2.recorded(); len(got) != 0 {
		t.Fatalf("Slack was called with no linked users: %+v", got)
	}
}

// TestSyncerDamagedScheduleDoesNotStopTheRest: a schedule that could not be
// projected is logged and counted; its usergroup is left alone, because a group
// cannot be synced to a membership nobody could read.
func TestSyncerDamagedScheduleDoesNotStopTheRest(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.setBulk(schedulerender.BulkOnCall{
		Schedules: []schedulerender.ScheduleOnCall{usergroupDuty("sched-ok", "S-OK", "g-a", "alice")},
		Failures: []schedulerender.ProjectionFailure{{
			ScheduleID: "sched-broken", TeamID: "team-1",
			Reason: schedulerender.FailureRotation, Err: errors.New("unknown time zone"),
		}},
	})
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	before := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.WithLabelValues(
		metrics.ConsumerUsergroupSyncer, string(schedulerender.FailureRotation)))
	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	after := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.WithLabelValues(
		metrics.ConsumerUsergroupSyncer, string(schedulerender.FailureRotation)))

	got := stub.recorded()
	if len(got) != 1 || got[0].usergroup != "S-OK" {
		t.Fatalf("Slack updates = %+v, want the healthy schedule synced", got)
	}
	if after-before != 1 {
		t.Errorf("projection failure counter moved by %v, want 1", after-before)
	}
}

// TestSyncerOneSlackFailureDoesNotStopTheLoop: the sync is a loop over
// independent schedules and stays one.
func TestSyncerOneSlackFailureDoesNotStopTheLoop(t *testing.T) {
	stub := newSlackStub(t)
	stub.setFail(true)
	oncall := &fakeOnCall{}
	oncall.set(
		usergroupDuty("sched-1", "S-1", "g-a", "alice"),
		usergroupDuty("sched-2", "S-2", "g-a", "alice"),
	)
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll should not fail because one schedule did: %v", err)
	}
	if got := stub.recorded(); len(got) != 2 {
		t.Fatalf("%d Slack calls, want both schedules attempted: %+v", len(got), got)
	}
}

// TestSyncerCachesUnchangedMembership: an unchanged group is not written again.
// A failed write is not cached either, or the retry would never happen.
func TestSyncerCachesUnchangedMembership(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.set(usergroupDuty("sched-1", "S12345", "g-a", "alice"))
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice", "bob"))

	for i := 0; i < 3; i++ {
		if err := syncer.SyncAll(context.Background()); err != nil {
			t.Fatalf("SyncAll: %v", err)
		}
	}
	if got := stub.recorded(); len(got) != 1 {
		t.Fatalf("%d Slack calls for an unchanged membership, want 1: %+v", len(got), got)
	}

	// A change goes through the cache.
	oncall.set(usergroupDuty("sched-1", "S12345", "g-b", "bob"))
	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	got := stub.recorded()
	if len(got) != 2 || got[1].users != "U-BOB" {
		t.Fatalf("Slack updates = %+v, want the changed membership written", got)
	}
}

// TestSyncerFailedWriteIsNotCached: the cache records what Slack accepted, so a
// rejected write must be retried on the next tick.
func TestSyncerFailedWriteIsNotCached(t *testing.T) {
	stub := newSlackStub(t)
	stub.setFail(true)
	oncall := &fakeOnCall{}
	oncall.set(usergroupDuty("sched-1", "S12345", "g-a", "alice"))
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	stub.setFail(false)
	if err := syncer.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := stub.recorded(); len(got) != 2 {
		t.Fatalf("%d Slack calls, want the rejected write retried: %+v", len(got), got)
	}
}

// TestSyncerCallFailureIsReported: nothing was read, so the caller is told
// rather than being handed a silent no-op over zero schedules - the shape of the
// defect this replaces.
func TestSyncerCallFailureIsReported(t *testing.T) {
	stub := newSlackStub(t)
	oncall := &fakeOnCall{}
	oncall.fail(errors.New("could not begin transaction"))
	syncer := newTestSyncer(t, stub, oncall, slackIDsFor("alice"))

	if err := syncer.SyncAll(context.Background()); err == nil {
		t.Fatal("SyncAll reported success although it read nothing")
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("Slack was called after a read failure: %+v", got)
	}
}

func TestUsergroupSyncerManager_StartWithSameToken_NoRestart(t *testing.T) {
	manager := NewUsergroupSyncerManager(&mockSyncerStore{}, &fakeOnCall{}, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start with token
	manager.Start(ctx, "token1")
	if !manager.IsRunning() {
		t.Fatal("Manager should be running after Start")
	}

	// Start again with same token - should be idempotent
	manager.Start(ctx, "token1")

	// Still running
	if !manager.IsRunning() {
		t.Error("Manager should still be running after Start with same token")
	}

	// Verify token is tracked correctly
	manager.mu.Lock()
	token := manager.currentToken
	manager.mu.Unlock()

	if token != "token1" {
		t.Errorf("Expected currentToken to be 'token1', got '%s'", token)
	}
}

func TestUsergroupSyncerManager_StartWithDifferentToken_Restarts(t *testing.T) {
	manager := NewUsergroupSyncerManager(&mockSyncerStore{}, &fakeOnCall{}, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start with token1
	manager.Start(ctx, "token1")
	if !manager.IsRunning() {
		t.Fatal("Manager should be running after Start")
	}

	// Start with different token - should restart
	manager.Start(ctx, "token2")

	if !manager.IsRunning() {
		t.Error("Manager should still be running after restart with different token")
	}

	// Verify token was updated
	manager.mu.Lock()
	token := manager.currentToken
	manager.mu.Unlock()

	if token != "token2" {
		t.Errorf("Expected currentToken to be 'token2', got '%s'", token)
	}
}

func TestUsergroupSyncerManager_StartWithEmptyToken_Stops(t *testing.T) {
	manager := NewUsergroupSyncerManager(&mockSyncerStore{}, &fakeOnCall{}, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start with token
	manager.Start(ctx, "token1")
	if !manager.IsRunning() {
		t.Fatal("Manager should be running after Start")
	}

	// Start with empty token - should stop
	manager.Start(ctx, "")
	if manager.IsRunning() {
		t.Error("Manager should not be running after Start with empty token")
	}
}

func TestUsergroupSyncerManager_StopIsNonBlocking(t *testing.T) {
	manager := NewUsergroupSyncerManager(&mockSyncerStore{}, &fakeOnCall{}, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start
	manager.Start(ctx, "token1")

	// Stop should complete immediately (non-blocking)
	done := make(chan struct{})
	go func() {
		manager.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK - Stop returned immediately
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Stop should be non-blocking and return immediately")
	}

	if manager.IsRunning() {
		t.Error("Manager should not be running after Stop")
	}
}

func TestUsergroupSyncerManager_DeadSyncerRecovery(t *testing.T) {
	manager := NewUsergroupSyncerManager(&mockSyncerStore{}, &fakeOnCall{}, 50*time.Millisecond)

	// Create a cancellable context to simulate external cancellation
	syncerCtx, syncerCancel := context.WithCancel(context.Background())

	// Start syncer
	manager.Start(syncerCtx, "token1")
	if !manager.IsRunning() {
		t.Fatal("Manager should be running after Start")
	}

	// Simulate syncer death by cancelling its context externally
	syncerCancel()

	// Wait for cleanup goroutine to detect and clean up
	time.Sleep(200 * time.Millisecond)

	// Syncer should no longer be running (cleanup detected death)
	if manager.IsRunning() {
		t.Error("Manager should detect dead syncer and clear state")
	}

	// Start with same token should restart (not no-op)
	newCtx := context.Background()
	manager.Start(newCtx, "token1")

	if !manager.IsRunning() {
		t.Error("Manager should restart syncer after death recovery")
	}
}

func TestUsergroupSyncerManager_GenerationPreventsRace(t *testing.T) {
	manager := NewUsergroupSyncerManager(&mockSyncerStore{}, &fakeOnCall{}, 50*time.Millisecond)
	ctx1, cancel1 := context.WithCancel(context.Background())

	// Start with token1, gen=1
	manager.Start(ctx1, "token1")
	if !manager.IsRunning() {
		t.Fatal("Manager should be running")
	}

	// Start with token2 immediately, gen=2 (token1 syncer still running)
	ctx2 := context.Background()
	manager.Start(ctx2, "token2")

	// Verify token2 is now current
	manager.mu.Lock()
	token := manager.currentToken
	gen := manager.generation
	manager.mu.Unlock()

	if token != "token2" {
		t.Errorf("Expected token2, got %s", token)
	}
	if gen != 2 {
		t.Errorf("Expected generation 2, got %d", gen)
	}

	// Cancel old syncer's context to trigger its cleanup
	cancel1()
	time.Sleep(200 * time.Millisecond)

	// Manager should still be running (old syncer cleanup didn't affect new syncer)
	if !manager.IsRunning() {
		t.Error("Old syncer cleanup should not affect new syncer (generation mismatch)")
	}

	// Token should still be token2
	manager.mu.Lock()
	token = manager.currentToken
	manager.mu.Unlock()
	if token != "token2" {
		t.Errorf("Token should still be token2, got %s", token)
	}
}

// TestSyncerRunSurvivesAnEmptyProjection: a fresh installation has no schedules
// and the loop has to be fine with that.
func TestSyncerRunSurvivesAnEmptyProjection(t *testing.T) {
	stub := newSlackStub(t)
	syncer := newTestSyncer(t, stub, &fakeOnCall{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Fatalf("Slack was called with no schedules: %+v", got)
	}
}
