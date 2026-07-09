//go:build integration

package dispatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
	"github.com/slack-go/slack"
)

// fakeSlack records each usergroups.users.update request body for verification.
type fakeSlack struct {
	server       *httptest.Server
	mu           sync.Mutex
	updateCalls  []string // captured `users` form parameter, one per call
	requestCount atomic.Int64
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	fs := &fakeSlack{}
	fs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/usergroups.users.update" {
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			fs.mu.Lock()
			fs.updateCalls = append(fs.updateCalls, r.PostForm.Get("users"))
			fs.mu.Unlock()
			fs.requestCount.Add(1)

			json.NewEncoder(w).Encode(map[string]interface{}{
				"ok": true,
				"usergroup": map[string]interface{}{
					"id":     r.PostForm.Get("usergroup"),
					"name":   "fake usergroup",
					"handle": "fake",
				},
			})
			return
		}

		t.Errorf("Unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	return fs
}

func (fs *fakeSlack) close() {
	fs.server.Close()
}

func (fs *fakeSlack) calls() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]string, len(fs.updateCalls))
	copy(out, fs.updateCalls)
	return out
}

// syncerEnv bundles store + syncer + schedule for syncer integration tests.
type syncerEnv struct {
	s        *store.Store
	syncer   *UsergroupSyncer
	schedID  string
	fake     *fakeSlack
	cleanup  func()
	schedule *model.Schedule
}

func setupSyncerEnv(t *testing.T) *syncerEnv {
	t.Helper()
	s := testutil.SetupDB(t)
	fake := newFakeSlack(t)

	// Seed users and bind their Slack external identities. Epic 7 moved Slack IDs
	// out of the user row into external_identities; the syncer resolves each
	// on-call user's Slack ID via that table. U_NOSLACK intentionally has none.
	users := []struct {
		u       *model.User
		slackID string
	}{
		{&model.User{ID: "U_A", Email: "ua@syncer.test", Name: "User A"}, "S_A"},
		{&model.User{ID: "U_B", Email: "ub@syncer.test", Name: "User B"}, "S_B"},
		{&model.User{ID: "U_C", Email: "uc@syncer.test", Name: "User C"}, "S_C"},
		{&model.User{ID: "U_NOSLACK", Email: "noslack@syncer.test", Name: "No Slack"}, ""},
	}
	for _, tc := range users {
		if err := s.CreateUser(tc.u); err != nil {
			t.Fatalf("CreateUser %s: %v", tc.u.ID, err)
		}
		if tc.slackID != "" {
			testutil.BindSlack(t, s, tc.u.ID, tc.slackID)
		}
	}

	team := &model.Team{ID: "team-syncer", Name: "Syncer Team"}
	if err := s.CreateTeam(team); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	schedID := "sched-syncer"
	schedule := &model.Schedule{
		ID:               schedID,
		TeamID:           team.ID,
		Timezone:         "UTC",
		SlackUsergroupID: "S_TEST_USERGROUP",
		L1RotationType:   model.RotationDaily,
		L1HandoffTime:    "09:00",
		L1RotationStart:  time.Now().Add(-7 * 24 * time.Hour),
	}
	if err := s.CreateSchedule(schedule); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// Construct syncer with test slack client pointing to fake server
	syncer := &UsergroupSyncer{
		store:        s,
		slackClient:  slack.New("test-token", slack.OptionAPIURL(fake.server.URL+"/")),
		syncInterval: time.Minute,
		cache:        make(map[string]cacheEntry),
	}

	return &syncerEnv{
		s:        s,
		syncer:   syncer,
		schedID:  schedID,
		fake:     fake,
		schedule: schedule,
		cleanup:  fake.close,
	}
}

// TestSyncer_MultiUserGroup_SendsCommaJoinedIDs verifies that all Slack IDs
// from a multi-user group are sent in a single comma-separated update.
func TestSyncer_MultiUserGroup_SendsCommaJoinedIDs(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_B"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	if err := env.syncer.SyncSchedule(context.Background(), env.schedule); err != nil {
		t.Fatalf("SyncSchedule: %v", err)
	}

	calls := env.fake.calls()
	if len(calls) != 1 {
		t.Fatalf("Expected exactly 1 Slack API call, got %d", len(calls))
	}

	got := strings.Split(calls[0], ",")
	sort.Strings(got)
	want := []string{"S_A", "S_B"}
	if !equalSlices(got, want) {
		t.Errorf("Expected users param %v, got %v", want, got)
	}

	// Cache must hold sorted joined IDs
	cached, ok := env.syncer.getCache("S_TEST_USERGROUP")
	if !ok {
		t.Fatal("Expected cache entry after sync")
	}
	if cached.slackIDs != "S_A,S_B" {
		t.Errorf("Expected cache 'S_A,S_B', got %q", cached.slackIDs)
	}
}

// TestSyncer_GroupChangeUpdatesUsergroup verifies the [A,B]→[A,C] case:
// the first user did not change, but the group did, so a second update must fire.
func TestSyncer_GroupChangeUpdatesUsergroup(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	// First sync with [A, B]
	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_B"}}); err != nil {
		t.Fatalf("SetScheduleGroups initial: %v", err)
	}
	if err := env.syncer.SyncSchedule(context.Background(), env.schedule); err != nil {
		t.Fatalf("First SyncSchedule: %v", err)
	}

	// Change group to [A, C]
	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_C"}}); err != nil {
		t.Fatalf("SetScheduleGroups change: %v", err)
	}
	if err := env.syncer.SyncSchedule(context.Background(), env.schedule); err != nil {
		t.Fatalf("Second SyncSchedule: %v", err)
	}

	calls := env.fake.calls()
	if len(calls) != 2 {
		t.Fatalf("Expected 2 Slack API calls (initial + change), got %d", len(calls))
	}

	// First call: S_A,S_B
	first := strings.Split(calls[0], ",")
	sort.Strings(first)
	if !equalSlices(first, []string{"S_A", "S_B"}) {
		t.Errorf("First call expected [S_A S_B], got %v", first)
	}

	// Second call: S_A,S_C
	second := strings.Split(calls[1], ",")
	sort.Strings(second)
	if !equalSlices(second, []string{"S_A", "S_C"}) {
		t.Errorf("Second call expected [S_A S_C], got %v", second)
	}
}

// TestSyncer_NoChangeSkipsAPICall verifies that a second sync without changes
// hits the cache and does not call Slack.
func TestSyncer_NoChangeSkipsAPICall(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_B"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	// First sync
	if err := env.syncer.SyncSchedule(context.Background(), env.schedule); err != nil {
		t.Fatalf("First SyncSchedule: %v", err)
	}

	// Second sync — same group, cache hit, no API call
	if err := env.syncer.SyncSchedule(context.Background(), env.schedule); err != nil {
		t.Fatalf("Second SyncSchedule: %v", err)
	}

	if got := env.fake.requestCount.Load(); got != 1 {
		t.Errorf("Expected 1 API call (second hit cache), got %d", got)
	}
}

// TestSyncer_SkipsMissingSlackIDs verifies that users without SlackUserID
// are excluded from the comma-joined Slack ID list.
func TestSyncer_SkipsMissingSlackIDs(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_NOSLACK"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	if err := env.syncer.SyncSchedule(context.Background(), env.schedule); err != nil {
		t.Fatalf("SyncSchedule: %v", err)
	}

	calls := env.fake.calls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 Slack API call, got %d", len(calls))
	}

	// Only S_A should be sent (U_NOSLACK skipped)
	if calls[0] != "S_A" {
		t.Errorf("Expected users=S_A, got %q", calls[0])
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
