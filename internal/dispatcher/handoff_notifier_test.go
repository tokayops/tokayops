package dispatcher

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// mockNotifierStore implements the store interface methods required by HandoffNotifier
type mockNotifierStore struct {
	store.StoreInterface
	schedules []*model.Schedule
	teams     []*model.Team
	epochs    map[string][]*model.RotationEpoch // scheduleID -> epochs
	overrides map[string][]*model.ScheduleOverride
	users     map[string]*model.User
	slackIDs  map[string]string // userID -> slack external id (empty means "not linked")
	jobs      []*createdJob

	getSchedulesErr error
	getEpochsErr    error
	createJobErr    error
}

type createdJob struct {
	job   *model.Job
	steps []*model.JobStep
}

func (m *mockNotifierStore) GetAllSchedules() ([]*model.Schedule, error) {
	if m.getSchedulesErr != nil {
		return nil, m.getSchedulesErr
	}
	return m.schedules, nil
}

func (m *mockNotifierStore) GetAllTeams() ([]*model.Team, error) {
	return m.teams, nil
}

func (m *mockNotifierStore) GetRotationEpochs(scheduleID, layer string, from, until time.Time) ([]*model.RotationEpoch, error) {
	if m.getEpochsErr != nil {
		return nil, m.getEpochsErr
	}
	if epochs, ok := m.epochs[scheduleID]; ok {
		return epochs, nil
	}
	return nil, nil
}

func (m *mockNotifierStore) GetScheduleOverrides(scheduleID string, from, until time.Time) ([]*model.ScheduleOverride, error) {
	overrides, ok := m.overrides[scheduleID]
	if !ok {
		return nil, nil
	}
	// Filter like the real SQL: start_time < until AND end_time > from
	var filtered []*model.ScheduleOverride
	for _, o := range overrides {
		if o.StartTime.Before(until) && o.EndTime.After(from) {
			filtered = append(filtered, o)
		}
	}
	return filtered, nil
}

func (m *mockNotifierStore) GetUsersByIDs(ids []string) ([]*model.User, error) {
	var result []*model.User
	for _, id := range ids {
		if u, ok := m.users[id]; ok {
			result = append(result, u)
		}
	}
	return result, nil
}

// GetIdentitiesForUsers reads the per-test slackIDs map. A user with an empty or
// missing entry mirrors the old "User.SlackUserID == ”" case → handoff skips them.
func (m *mockNotifierStore) GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error) {
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

func (m *mockNotifierStore) CreateJobWithDedup(job *model.Job, _ []*model.JobStage, steps []*model.JobStep) (string, error) {
	if m.createJobErr != nil {
		return "", m.createJobErr
	}
	m.jobs = append(m.jobs, &createdJob{job: job, steps: steps})
	return job.ID, nil
}

func newTestSchedule() *model.Schedule {
	return &model.Schedule{
		ID:              "sched-1",
		TeamID:          "team-1",
		Timezone:        "Asia/Bangkok",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Now().Add(-7 * 24 * time.Hour),
	}
}

func newTestUser(id, name, slackID string) *model.User {
	return &model.User{
		ID:    id,
		Name:  name,
		Email: name + "@example.com",
	}
}

func newTestEpoch(scheduleID string, userIDs []string) *model.RotationEpoch {
	// Wrap each user as a singleton group for the new Groups model
	groups := make([][]string, len(userIDs))
	for i, id := range userIDs {
		groups[i] = []string{id}
	}
	return &model.RotationEpoch{
		ID:         "epoch-1",
		ScheduleID: scheduleID,
		Layer:      "l1",
		Groups:     groups,
		StartTime:  time.Now().Add(-7 * 24 * time.Hour),
	}
}

func TestHandoffNotifier_WarmUp_NoJobsCreated(t *testing.T) {
	user := newTestUser("user-1", "Alice", "U11111")
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {newTestEpoch("sched-1", []string{"user-1"})},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-1": user},
		slackIDs:  map[string]string{"user-1": "U11111"},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	notifier.checkAll()

	if len(mockStore.jobs) != 0 {
		t.Errorf("Expected 0 jobs during warm-up, got: %d", len(mockStore.jobs))
	}

	notifier.cacheMu.RLock()
	cached, ok := notifier.cache["sched-1"]
	notifier.cacheMu.RUnlock()
	if !ok {
		t.Fatal("Expected cache entry for sched-1")
	}
	if cached != "user-1" {
		t.Errorf("Expected cached group key 'user-1', got: %s", cached)
	}
}

func TestHandoffNotifier_HandoffDetected_CreatesJob(t *testing.T) {
	user1 := newTestUser("user-1", "Alice", "U11111")
	user2 := newTestUser("user-2", "Bob", "U22222")
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {newTestEpoch("sched-1", []string{"user-1"})},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-1": user1, "user-2": user2},
		slackIDs:  map[string]string{"user-1": "U11111", "user-2": "U22222"},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	// Warm-up
	notifier.checkAll()
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	notifier.cacheMu.Unlock()

	if len(mockStore.jobs) != 0 {
		t.Fatalf("Expected 0 jobs after warm-up, got: %d", len(mockStore.jobs))
	}

	// Simulate user change
	mockStore.epochs["sched-1"] = []*model.RotationEpoch{newTestEpoch("sched-1", []string{"user-2"})}

	notifier.checkAll()

	if len(mockStore.jobs) != 1 {
		t.Fatalf("Expected 1 job after handoff, got: %d", len(mockStore.jobs))
	}

	job := mockStore.jobs[0]
	if job.job.Type != "handoff_notify" {
		t.Errorf("Expected job type 'handoff_notify', got: %s", job.job.Type)
	}
	if job.job.DedupKey == nil || *job.job.DedupKey != "handoff:sched-1:user-2" {
		t.Errorf("Expected dedup key 'handoff:sched-1:user-2', got: %s", *job.job.DedupKey)
	}
	if len(job.steps) != 1 {
		t.Fatalf("Expected 1 step, got: %d", len(job.steps))
	}
	if job.steps[0].StepType != "handoff_notify" {
		t.Errorf("Expected step type 'handoff_notify', got: %s", job.steps[0].StepType)
	}
	if job.steps[0].MaxAttempts != 3 {
		t.Errorf("Expected MaxAttempts 3, got: %d", job.steps[0].MaxAttempts)
	}

	var stepData model.HandoffStepData
	if err := json.Unmarshal(job.steps[0].Data, &stepData); err != nil {
		t.Fatalf("Failed to unmarshal step data: %v", err)
	}
	if stepData.TargetID != "U22222" {
		t.Errorf("Expected target 'U22222', got: %s", stepData.TargetID)
	}
	if stepData.ScheduleID != "sched-1" {
		t.Errorf("Expected schedule 'sched-1', got: %s", stepData.ScheduleID)
	}
	if stepData.TeamID != "team-1" {
		t.Errorf("Expected team 'team-1', got: %s", stepData.TeamID)
	}
}

func TestHandoffNotifier_NoChange_NoJob(t *testing.T) {
	user := newTestUser("user-1", "Alice", "U11111")
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {newTestEpoch("sched-1", []string{"user-1"})},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-1": user},
		slackIDs:  map[string]string{"user-1": "U11111"},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	// Warm-up
	notifier.checkAll()
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	notifier.cacheMu.Unlock()

	// Check again with same user
	notifier.checkAll()

	if len(mockStore.jobs) != 0 {
		t.Errorf("Expected 0 jobs when user unchanged, got: %d", len(mockStore.jobs))
	}
}

func TestHandoffNotifier_NoSlackUserID_Skipped(t *testing.T) {
	user1 := newTestUser("user-1", "Alice", "U11111")
	user2 := newTestUser("user-2", "Bob", "") // No Slack ID
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {newTestEpoch("sched-1", []string{"user-1"})},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-1": user1, "user-2": user2},
		slackIDs:  map[string]string{"user-1": "U11111"}, // user-2 intentionally has no slack identity
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	// Warm-up
	notifier.checkAll()
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	notifier.cacheMu.Unlock()

	// Simulate handoff to user without Slack ID
	mockStore.epochs["sched-1"] = []*model.RotationEpoch{newTestEpoch("sched-1", []string{"user-2"})}

	notifier.checkAll()

	if len(mockStore.jobs) != 0 {
		t.Errorf("Expected 0 jobs when user has no SlackUserID, got: %d", len(mockStore.jobs))
	}
}

func TestHandoffNotifier_NoL1User_Skipped(t *testing.T) {
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs:    map[string][]*model.RotationEpoch{},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)
	notifier.warmedUp = true

	notifier.checkAll()

	if len(mockStore.jobs) != 0 {
		t.Errorf("Expected 0 jobs when no L1 user, got: %d", len(mockStore.jobs))
	}
}

func TestFormatHandoffMessage_WithTimes(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Bangkok")
	start := time.Date(2025, 2, 12, 4, 0, 0, 0, time.UTC) // 11:00 ICT
	end := time.Date(2025, 2, 13, 4, 0, 0, 0, time.UTC)   // 11:00 ICT next day

	msg := formatHandoffMessage("Backend", "Asia/Bangkok", &start, &end)

	startFormatted := start.In(loc).Format("Mon Jan 2, 15:04")
	endFormatted := end.In(loc).Format("Mon Jan 2, 15:04")

	expected := ":mega: You are now on-call for team *Backend*.\n\n" +
		":clock1: *Shift start:* " + startFormatted + " (Asia/Bangkok)\n" +
		":clock4: *Shift end:* " + endFormatted + " (Asia/Bangkok)\n" +
		"\n_Shift end time is current as of now and may change if the schedule is modified._"

	if msg != expected {
		t.Errorf("Message mismatch.\nGot:\n%s\n\nExpected:\n%s", msg, expected)
	}
}

func TestFormatHandoffMessage_Indefinite(t *testing.T) {
	start := time.Date(2025, 2, 12, 4, 0, 0, 0, time.UTC)
	msg := formatHandoffMessage("Backend", "Asia/Bangkok", &start, nil)

	if !strings.Contains(msg, "indefinite") {
		t.Errorf("Expected message to contain 'indefinite', got: %s", msg)
	}
}

func TestHandoffNotifier_JobCreationError_RetriesNextTick(t *testing.T) {
	user1 := newTestUser("user-1", "Alice", "U11111")
	user2 := newTestUser("user-2", "Bob", "U22222")
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {newTestEpoch("sched-1", []string{"user-1"})},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-1": user1, "user-2": user2},
		slackIDs:  map[string]string{"user-1": "U11111", "user-2": "U22222"},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	// Warm-up
	notifier.checkAll()
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	notifier.cacheMu.Unlock()

	// Simulate handoff with DB error on job creation
	mockStore.epochs["sched-1"] = []*model.RotationEpoch{newTestEpoch("sched-1", []string{"user-2"})}
	mockStore.createJobErr = errors.New("transient DB error")
	notifier.checkAll()

	if len(mockStore.jobs) != 0 {
		t.Fatalf("Expected 0 jobs when CreateJobWithDedup fails, got: %d", len(mockStore.jobs))
	}

	// Cache should NOT be updated (still has the old group key)
	notifier.cacheMu.RLock()
	cached := notifier.cache["sched-1"]
	notifier.cacheMu.RUnlock()
	if cached != "user-1" {
		t.Errorf("Cache should still have 'user-1' after failed job creation, got: %s", cached)
	}

	// Fix DB error, retry
	mockStore.createJobErr = nil
	notifier.checkAll()

	if len(mockStore.jobs) != 1 {
		t.Fatalf("Expected 1 job after retry, got: %d", len(mockStore.jobs))
	}
}

func TestHandoffNotifier_MultiUserGroup_ChangeDetected(t *testing.T) {
	// Critical: [A,B] -> [A,C] must be detected as a change even though A stays.
	userA := newTestUser("user-a", "Alice", "UA")
	userB := newTestUser("user-b", "Bob", "UB")
	userC := newTestUser("user-c", "Carol", "UC")
	schedule := newTestSchedule()

	// Start with group [A, B]
	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {{
				ID:         "epoch-1",
				ScheduleID: "sched-1",
				Layer:      "l1",
				Groups:     [][]string{{"user-a", "user-b"}},
				StartTime:  time.Now().Add(-7 * 24 * time.Hour),
			}},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-a": userA, "user-b": userB, "user-c": userC},
		slackIDs:  map[string]string{"user-a": "UA", "user-b": "UB", "user-c": "UC"},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	// Warm-up: cache should store "user-a,user-b"
	notifier.checkAll()
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	notifier.cacheMu.Unlock()

	notifier.cacheMu.RLock()
	cachedKey := notifier.cache["sched-1"]
	notifier.cacheMu.RUnlock()
	if cachedKey != "user-a,user-b" {
		t.Fatalf("Expected cached key 'user-a,user-b', got: %s", cachedKey)
	}

	// Change group to [A, C] — A stays, B replaced by C
	mockStore.epochs["sched-1"] = []*model.RotationEpoch{{
		ID:         "epoch-2",
		ScheduleID: "sched-1",
		Layer:      "l1",
		Groups:     [][]string{{"user-a", "user-c"}},
		StartTime:  time.Now().Add(-1 * time.Hour),
	}}

	notifier.checkAll()

	if len(mockStore.jobs) != 1 {
		t.Fatalf("Expected 1 job for group change [A,B]->[A,C], got: %d", len(mockStore.jobs))
	}

	// Should create 2 steps (one for each user with SlackUserID)
	job := mockStore.jobs[0]
	if len(job.steps) != 2 {
		t.Fatalf("Expected 2 steps (one per group member), got: %d", len(job.steps))
	}

	// Verify both users get notified
	targets := make(map[string]bool)
	for _, step := range job.steps {
		var stepData model.HandoffStepData
		json.Unmarshal(step.Data, &stepData)
		targets[stepData.TargetID] = true
	}
	if !targets["UA"] || !targets["UC"] {
		t.Errorf("Expected targets UA and UC, got: %v", targets)
	}
}

func TestHandoffNotifier_MultiUserGroup_PartialSlackIDs(t *testing.T) {
	// User without SlackUserID is skipped, others get notified
	userA := newTestUser("user-a", "Alice", "UA")
	userB := newTestUser("user-b", "Bob", "") // No Slack ID
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {{
				ID:         "epoch-1",
				ScheduleID: "sched-1",
				Layer:      "l1",
				Groups:     [][]string{{"user-a", "user-b"}},
				StartTime:  time.Now().Add(-7 * 24 * time.Hour),
			}},
		},
		overrides: map[string][]*model.ScheduleOverride{},
		users:     map[string]*model.User{"user-a": userA, "user-b": userB},
		slackIDs:  map[string]string{"user-a": "UA"}, // user-b has no slack identity
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)
	notifier.checkAll()
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	// Simulate previous different group
	notifier.cache["sched-1"] = "other"
	notifier.cacheMu.Unlock()

	notifier.checkAll()

	if len(mockStore.jobs) != 1 {
		t.Fatalf("Expected 1 job, got: %d", len(mockStore.jobs))
	}
	// Only 1 step (user-b has no Slack ID)
	if len(mockStore.jobs[0].steps) != 1 {
		t.Fatalf("Expected 1 step (user without SlackUserID skipped), got: %d", len(mockStore.jobs[0].steps))
	}
	var stepData model.HandoffStepData
	json.Unmarshal(mockStore.jobs[0].steps[0].Data, &stepData)
	if stepData.TargetID != "UA" {
		t.Errorf("Expected target UA, got: %s", stepData.TargetID)
	}
}

func TestHandoffNotifier_AfterOverride_ShiftStartIsOverrideEnd(t *testing.T) {
	// Bug #3: After override ends, notification should show override end time
	// as shift start, not the rotation period start.
	//
	// Setup: daily rotation, user1 on-call.
	// Override by user3 from 14:00-18:00 (already ended).
	// When notifier detects user1 is back, L1Since must be 18:00 (override end).
	now := time.Now().UTC()
	handoffToday := time.Date(now.Year(), now.Month(), now.Day(), 11, 0, 0, 0, time.UTC)

	// Override ended 1 hour ago
	overrideEnd := now.Add(-1 * time.Hour)
	overrideStart := overrideEnd.Add(-4 * time.Hour)

	user1 := newTestUser("user-1", "Alice", "U11111")
	user3 := newTestUser("user-3", "Charlie", "U33333")

	schedule := &model.Schedule{
		ID:              "sched-1",
		TeamID:          "team-1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: handoffToday.Add(-7 * 24 * time.Hour),
	}

	epoch := &model.RotationEpoch{
		ID:         "epoch-1",
		ScheduleID: "sched-1",
		Layer:      "l1",
		Groups:     [][]string{{"user-1"}},
		StartTime:  handoffToday.Add(-7 * 24 * time.Hour),
	}

	override := &model.ScheduleOverride{
		ID:        "override-1",
		UserID:    "user-3",
		StartTime: overrideStart,
		EndTime:   overrideEnd,
	}

	mockStore := &mockNotifierStore{
		schedules: []*model.Schedule{schedule},
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		epochs: map[string][]*model.RotationEpoch{
			"sched-1": {epoch},
		},
		overrides: map[string][]*model.ScheduleOverride{
			"sched-1": {override},
		},
		users:    map[string]*model.User{"user-1": user1, "user-3": user3},
		slackIDs: map[string]string{"user-1": "U11111", "user-3": "U33333"},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	// Warm-up with user3 (override active)
	notifier.cacheMu.Lock()
	notifier.warmedUp = true
	notifier.cache["sched-1"] = "user-3" // Simulate: last seen user was override user
	notifier.cacheMu.Unlock()

	// Now check — override has ended, user1 is back
	notifier.checkAll()

	if len(mockStore.jobs) != 1 {
		t.Fatalf("Expected 1 job, got: %d", len(mockStore.jobs))
	}

	var stepData model.HandoffStepData
	if err := json.Unmarshal(mockStore.jobs[0].steps[0].Data, &stepData); err != nil {
		t.Fatalf("Failed to unmarshal step data: %v", err)
	}

	// The message must contain the override end time, not the handoff time (11:00)
	overrideEndFormatted := overrideEnd.Format("15:04")
	handoffFormatted := "11:00"

	if !strings.Contains(stepData.Message, overrideEndFormatted) {
		t.Errorf("Expected message to contain override end time %s, got:\n%s", overrideEndFormatted, stepData.Message)
	}
	if strings.Contains(stepData.Message, "Shift start:* "+handoffToday.Format("Mon Jan 2, ")+handoffFormatted) {
		t.Errorf("Message should NOT show rotation period start (11:00), got:\n%s", stepData.Message)
	}
}

func TestHandoffNotifier_WarmUp_FailsOnDBError(t *testing.T) {
	mockStore := &mockNotifierStore{
		getSchedulesErr: errors.New("DB connection error"),
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	ok := notifier.checkAll()
	if ok {
		t.Error("Expected checkAll to return false on GetAllSchedules error")
	}

	notifier.cacheMu.RLock()
	warmedUp := notifier.warmedUp
	notifier.cacheMu.RUnlock()
	if warmedUp {
		t.Error("warmedUp should remain false after failed checkAll")
	}
}

func TestHandoffNotifier_WarmUp_FailsOnPerScheduleError(t *testing.T) {
	schedule := newTestSchedule()

	mockStore := &mockNotifierStore{
		schedules:    []*model.Schedule{schedule},
		teams:        []*model.Team{{ID: "team-1", Name: "Backend"}},
		getEpochsErr: errors.New("per-schedule DB error"),
		overrides:    map[string][]*model.ScheduleOverride{},
		users:        map[string]*model.User{},
	}

	notifier := NewHandoffNotifier(mockStore, staticDmProviders{"slack"}, time.Minute)

	ok := notifier.checkAll()
	if ok {
		t.Error("Expected checkAll to return false on per-schedule epoch error")
	}

	notifier.cacheMu.RLock()
	_, hasCached := notifier.cache["sched-1"]
	notifier.cacheMu.RUnlock()
	if hasCached {
		t.Error("Expected no cache entry for schedule with epoch error")
	}

	// Fix error, warm-up should succeed
	mockStore.getEpochsErr = nil
	mockStore.epochs = map[string][]*model.RotationEpoch{
		"sched-1": {newTestEpoch("sched-1", []string{"user-1"})},
	}
	mockStore.users = map[string]*model.User{
		"user-1": newTestUser("user-1", "Alice", "U11111"),
	}
	mockStore.slackIDs = map[string]string{"user-1": "U11111"}

	ok = notifier.checkAll()
	if !ok {
		t.Error("Expected checkAll to return true after error is fixed")
	}
}
