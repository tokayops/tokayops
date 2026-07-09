package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// mockSyncerStore implements the store interface methods required by syncer
type mockSyncerStore struct {
	store.StoreInterface
	schedules []*model.Schedule
	epochs    []*model.RotationEpoch
	overrides []*model.ScheduleOverride
	users     map[string]*model.User
	slackIDs  map[string]string // userID -> slack external id (empty = not linked → skipped)
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

func (m *mockSyncerStore) GetSchedulesWithUsergroup() ([]*model.Schedule, error) {
	return m.schedules, nil
}

func (m *mockSyncerStore) GetRotationEpochs(scheduleID, layer string, from, until time.Time) ([]*model.RotationEpoch, error) {
	return m.epochs, nil
}

func (m *mockSyncerStore) GetScheduleOverrides(scheduleID string, from, until time.Time) ([]*model.ScheduleOverride, error) {
	return m.overrides, nil
}

func (m *mockSyncerStore) GetUsersByIDs(ids []string) ([]*model.User, error) {
	var result []*model.User
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			result = append(result, u)
		}
	}
	return result, nil
}

func TestUsergroupSyncer_SyncAll_SkipsEmpty(t *testing.T) {
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{}, // No schedules with usergroup
	}

	syncer := &UsergroupSyncer{
		store:        mockStore,
		slackClient:  nil, // Not used when schedules is empty
		syncInterval: time.Minute,
	}

	ctx := context.Background()
	err := syncer.SyncAll(ctx)
	if err != nil {
		t.Errorf("SyncAll with no schedules should not error, got: %v", err)
	}
}

func TestUsergroupSyncer_SyncSchedule_NoL1User(t *testing.T) {
	// Schedule with usergroup but no L1 users configured
	schedule := &model.Schedule{
		ID:               "sched-1",
		TeamID:           "team-1",
		Timezone:         "UTC",
		SlackUsergroupID: "S12345678",
		L1RotationType:   model.RotationDaily,
		L1HandoffTime:    "11:00",
		L1RotationStart:  time.Now(),
	}

	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{schedule},
		epochs:    []*model.RotationEpoch{}, // No epochs = no users
		overrides: nil,
		users:     make(map[string]*model.User),
	}

	syncer := &UsergroupSyncer{
		store:        mockStore,
		slackClient:  nil, // API call shouldn't happen
		syncInterval: time.Minute,
	}

	ctx := context.Background()
	err := syncer.SyncSchedule(ctx, schedule)
	if err != nil {
		t.Errorf("SyncSchedule with no L1 epochs should skip without error, got: %v", err)
	}
}

func TestUsergroupSyncer_SyncSchedule_NoSlackUserID(t *testing.T) {
	// L1 user exists but has no SlackUserID
	user := &model.User{
		ID:    "user-1",
		Name:  "Test User",
		Email: "test@example.com",
	}

	schedule := &model.Schedule{
		ID:               "sched-1",
		TeamID:           "team-1",
		Timezone:         "UTC",
		SlackUsergroupID: "S12345678",
		L1RotationType:   model.RotationDaily,
		L1HandoffTime:    "11:00",
		L1RotationStart:  time.Now(),
	}

	epoch := &model.RotationEpoch{
		ID:         "epoch-1",
		ScheduleID: "sched-1",
		Layer:      "l1",
		Groups:     [][]string{{"user-1"}},
		StartTime:  time.Now().Add(-24 * time.Hour),
	}

	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{schedule},
		epochs:    []*model.RotationEpoch{epoch},
		overrides: nil,
		users:     map[string]*model.User{"user-1": user},
	}

	syncer := &UsergroupSyncer{
		store:        mockStore,
		slackClient:  nil, // API call shouldn't happen due to missing SlackUserID
		syncInterval: time.Minute,
	}

	ctx := context.Background()
	err := syncer.SyncSchedule(ctx, schedule)
	if err != nil {
		t.Errorf("SyncSchedule with user missing SlackUserID should skip without error, got: %v", err)
	}
}

func TestUsergroupSyncer_SyncSchedule_WithValidUser_SkipsWhenNoClient(t *testing.T) {
	// Note: Testing the full path to Slack API call requires a mock Slack client.
	// Since slack.Client doesn't have an interface, we'd need to refactor syncer to use one.
	// For now, we only test the edge cases that return early before the API call.
	// The integration test with a real DB will cover the full flow when TEST_DB_DSN is set.
	t.Skip("Full Slack API test requires mock client refactoring - covered by integration tests")
}

func TestUsergroupSyncerManager_StartWithSameToken_NoRestart(t *testing.T) {
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{},
	}

	manager := NewUsergroupSyncerManager(mockStore, 100*time.Millisecond)
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
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{},
	}

	manager := NewUsergroupSyncerManager(mockStore, 100*time.Millisecond)
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
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{},
	}

	manager := NewUsergroupSyncerManager(mockStore, 100*time.Millisecond)
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
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{},
	}

	manager := NewUsergroupSyncerManager(mockStore, 100*time.Millisecond)
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
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{},
	}

	manager := NewUsergroupSyncerManager(mockStore, 50*time.Millisecond)

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
	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{},
	}

	manager := NewUsergroupSyncerManager(mockStore, 50*time.Millisecond)
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

func TestUsergroupSyncer_SyncSchedule_OverrideTakesPrecedence(t *testing.T) {
	// Override user should take precedence over regular rotation
	regularUser := &model.User{
		ID:    "user-1",
		Name:  "Regular User",
		Email: "regular@example.com",
	}
	overrideUser := &model.User{
		ID:    "user-2",
		Name:  "Override User",
		Email: "override@example.com",
	}

	schedule := &model.Schedule{
		ID:               "sched-1",
		TeamID:           "team-1",
		Timezone:         "UTC",
		SlackUsergroupID: "S12345678",
		L1RotationType:   model.RotationDaily,
		L1HandoffTime:    "11:00",
		L1RotationStart:  time.Now().Add(-7 * 24 * time.Hour),
	}

	now := time.Now()
	epoch := &model.RotationEpoch{
		ID:         "epoch-1",
		ScheduleID: "sched-1",
		Layer:      "l1",
		Groups:     [][]string{{"user-1"}},
		StartTime:  time.Now().Add(-7 * 24 * time.Hour),
	}

	// Override covering current time
	override := &model.ScheduleOverride{
		ID:         "override-1",
		ScheduleID: "sched-1",
		UserID:     "user-2",
		StartTime:  now.Add(-1 * time.Hour),
		EndTime:    now.Add(1 * time.Hour),
	}

	mockStore := &mockSyncerStore{
		schedules: []*model.Schedule{schedule},
		epochs:    []*model.RotationEpoch{epoch},
		overrides: []*model.ScheduleOverride{override},
		users: map[string]*model.User{
			"user-1": regularUser,
			"user-2": overrideUser,
		},
	}

	syncer := &UsergroupSyncer{
		store:        mockStore,
		slackClient:  nil,
		syncInterval: time.Minute,
	}

	ctx := context.Background()
	err := syncer.SyncSchedule(ctx, schedule)
	// Should skip because override user has no SlackUserID
	if err != nil {
		t.Errorf("Expected skip for override user without SlackUserID, got error: %v", err)
	}
}
