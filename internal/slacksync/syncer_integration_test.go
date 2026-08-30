//go:build integration

package slacksync

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

	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// syncerUsergroup is the usergroup the fixture configures - in the snapshot,
// which is the only place the revision model states it.
const syncerUsergroup = "S_TEST_USERGROUP"

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

// syncerEnv drives the syncer over the real projection against PostgreSQL. The
// schedule service and the renderer share one clock, so a test that moves it
// moves both the configuration's effective instant and the projected duty.
type syncerEnv struct {
	t       *testing.T
	s       *store.Store
	config  *scheduleconfig.Service
	syncer  *UsergroupSyncer
	fake    *fakeSlack
	cleanup func()
	teamID  string
	schedID string
	now     time.Time
	version int64
}

func setupSyncerEnv(t *testing.T) *syncerEnv {
	t.Helper()
	s := testutil.SetupDB(t)
	fake := newFakeSlack(t)

	env := &syncerEnv{
		t:       t,
		s:       s,
		fake:    fake,
		cleanup: fake.close,
		teamID:  "team-syncer",
		now:     time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
	}
	clock := func() time.Time { return env.now }

	// Slack IDs live in external_identities rather than on the user row; the
	// syncer resolves each on-call user's Slack ID via that table. U_NOSLACK
	// intentionally has none.
	users := []struct {
		u       *model.User
		slackID string
	}{
		{&model.User{ID: "U_A", Email: "ua@syncer.test", Name: "User A"}, "S_A"},
		{&model.User{ID: "U_B", Email: "ub@syncer.test", Name: "User B"}, "S_B"},
		{&model.User{ID: "U_C", Email: "uc@syncer.test", Name: "User C"}, "S_C"},
		{&model.User{ID: "U_NOSLACK", Email: "noslack@syncer.test", Name: "No Slack"}, ""},
	}
	if err := s.CreateTeam(&model.Team{ID: env.teamID, Name: "Syncer Team"}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	for _, tc := range users {
		if err := s.CreateUser(tc.u); err != nil {
			t.Fatalf("CreateUser %s: %v", tc.u.ID, err)
		}
		if err := s.AddTeamMember(env.teamID, tc.u.ID, model.TeamMemberRoleMember); err != nil {
			t.Fatalf("AddTeamMember %s: %v", tc.u.ID, err)
		}
		if tc.slackID != "" {
			testutil.BindSlack(t, s, tc.u.ID, tc.slackID)
		}
	}

	env.config = scheduleconfig.NewService(s.ScheduleConfigRepository(),
		scheduleconfig.WithClock(clock))
	env.syncer = &UsergroupSyncer{
		store:        s,
		oncall:       schedulerender.New(s.ScheduleReadRepository(), schedulerender.WithClock(clock)),
		slackClient:  slack.New("test-token", slack.OptionAPIURL(fake.server.URL+"/")),
		syncInterval: time.Minute,
		cache:        make(map[string]cacheEntry),
	}
	return env
}

// Rotation group identities. L1 group IDs must be canonical UUIDs.
const (
	handoffGroupA = "aaaaaaaa-2222-4222-8222-000000000001"
	handoffGroupB = "bbbbbbbb-2222-4222-8222-000000000002"
	handoffGroupC = "cccccccc-2222-4222-8222-000000000003"
)

// dailyConfig is a daily rotation handing over at 12:00 UTC, so a tick a day
// later is a different group.
func dailyConfig(groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	policy := rotation.RotationPolicy{
		Cadence:     model.RotationDaily,
		HandoffTime: "12:00",
	}
	return rotation.ScheduleConfiguration{
		Timezone: "UTC",
		L1: rotation.LayerConfiguration{
			Enabled: true,
			Policy:  policy,
			Groups:  groups,
		},
		L2:                      rotation.LayerConfiguration{Enabled: false, Policy: policy},
		L2EscalationTimeoutMins: 5,
	}
}

func group(id string, members ...string) rotation.RotationGroup {
	return rotation.RotationGroup{ID: id, Members: members}
}

// usergroupConfig is the daily rotation the syncer fixture uses, carrying the
// Slack usergroup in the configuration where the editor writes it.
func usergroupConfig(usergroup string, groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	cfg := dailyConfig(groups...)
	cfg.SlackUsergroupID = usergroup
	return cfg
}

func (e *syncerEnv) save(cfg rotation.ScheduleConfiguration) {
	e.t.Helper()
	res, err := e.config.Save(context.Background(), e.teamID, scheduleconfig.SaveCommand{
		ExpectedVersion: e.version,
		Desired:         cfg,
		ActorID:         "U_A",
	})
	if err != nil {
		e.t.Fatalf("Save: %v", err)
	}
	e.version = res.Version
	e.schedID = res.Revision.ScheduleID
}

func (e *syncerEnv) syncAll() {
	e.t.Helper()
	if err := e.syncer.SyncAll(context.Background()); err != nil {
		e.t.Fatalf("SyncAll: %v", err)
	}
}

// TestSyncer_UsergroupComesFromTheSnapshot is the closing of the second defect:
// the schedule is found and its usergroup read from the configuration in force.
// The old query filtered on a mutable column the revision path never writes, so
// it synced nothing and reported success.
func TestSyncer_UsergroupComesFromTheSnapshot(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A", "U_B")))
	env.syncAll()

	calls := env.fake.calls()
	if len(calls) != 1 {
		t.Fatalf("Expected exactly 1 Slack API call, got %d", len(calls))
	}
	got := strings.Split(calls[0], ",")
	sort.Strings(got)
	if !equalSlices(got, []string{"S_A", "S_B"}) {
		t.Errorf("Expected users param [S_A S_B], got %v", got)
	}

	// Cache must hold sorted joined IDs
	cached, ok := env.syncer.getCache(syncerUsergroup)
	if !ok {
		t.Fatal("Expected cache entry after sync")
	}
	if cached.slackIDs != "S_A,S_B" {
		t.Errorf("Expected cache 'S_A,S_B', got %q", cached.slackIDs)
	}
}

// TestSyncer_GroupChangeUpdatesUsergroup verifies the [A,B]→[A,C] case: the
// first user did not change, but the group did, so a second update must fire.
func TestSyncer_GroupChangeUpdatesUsergroup(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A", "U_B")))
	env.syncAll()

	env.now = env.now.Add(time.Hour)
	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A", "U_C")))
	env.syncAll()

	calls := env.fake.calls()
	if len(calls) != 2 {
		t.Fatalf("Expected 2 Slack API calls (initial + change), got %d", len(calls))
	}

	first := strings.Split(calls[0], ",")
	sort.Strings(first)
	if !equalSlices(first, []string{"S_A", "S_B"}) {
		t.Errorf("First call expected [S_A S_B], got %v", first)
	}
	second := strings.Split(calls[1], ",")
	sort.Strings(second)
	if !equalSlices(second, []string{"S_A", "S_C"}) {
		t.Errorf("Second call expected [S_A S_C], got %v", second)
	}
}

// TestSyncer_ActiveOverrideChangesTheGroup: the projection overlays the
// override, so the usergroup holds whoever is really on duty.
func TestSyncer_ActiveOverrideChangesTheGroup(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A")))
	env.syncAll()

	from := env.now.Add(time.Hour)
	if _, err := env.config.CreateOverride(context.Background(), env.teamID, scheduleconfig.OverrideCommand{
		UserID:    "U_C",
		ValidFrom: from,
		ValidTo:   from.Add(2 * time.Hour),
		ActorID:   "U_A",
	}); err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	env.now = from.Add(30 * time.Minute)
	env.syncAll()

	calls := env.fake.calls()
	if len(calls) != 2 {
		t.Fatalf("Expected 2 Slack API calls, got %d: %v", len(calls), calls)
	}
	if calls[1] != "S_C" {
		t.Errorf("Expected the stand-in S_C in the group, got %q", calls[1])
	}
}

// TestSyncer_NoChangeSkipsAPICall verifies that a second sync without changes
// hits the cache and does not call Slack.
func TestSyncer_NoChangeSkipsAPICall(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A", "U_B")))
	env.syncAll()
	env.syncAll()

	if got := env.fake.requestCount.Load(); got != 1 {
		t.Errorf("Expected 1 API call (second hit cache), got %d", got)
	}
}

// TestSyncer_SkipsMissingSlackIDs verifies that users with no Slack identity are
// excluded from the comma-joined list.
func TestSyncer_SkipsMissingSlackIDs(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A", "U_NOSLACK")))
	env.syncAll()

	calls := env.fake.calls()
	if len(calls) != 1 {
		t.Fatalf("Expected 1 Slack API call, got %d", len(calls))
	}
	if calls[0] != "S_A" {
		t.Errorf("Expected users=S_A, got %q", calls[0])
	}
}

// TestSyncer_SchedulesWithoutUsergroupAreSkipped: the filter is in Go over the
// projection, because the projection is where the usergroup is stated.
func TestSyncer_SchedulesWithoutUsergroupAreSkipped(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.syncAll()

	if got := env.fake.requestCount.Load(); got != 0 {
		t.Errorf("Slack was called for a schedule with no usergroup (%d calls)", got)
	}
}

// TestSyncer_DeletedScheduleIsNotSynced: deleting a schedule has never emptied
// its usergroup, and it still does not.
func TestSyncer_DeletedScheduleIsNotSynced(t *testing.T) {
	env := setupSyncerEnv(t)
	defer env.cleanup()

	env.save(usergroupConfig(syncerUsergroup, group(handoffGroupA, "U_A")))
	env.syncAll()
	if got := env.fake.requestCount.Load(); got != 1 {
		t.Fatalf("Expected 1 API call before the delete, got %d", got)
	}

	env.now = env.now.Add(time.Hour)
	if err := env.config.Delete(context.Background(), env.teamID, scheduleconfig.DeleteCommand{
		ExpectedVersion: env.version,
		ActorID:         "U_A",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	env.now = env.now.Add(time.Hour)
	env.syncAll()

	if got := env.fake.requestCount.Load(); got != 1 {
		t.Errorf("Slack was called for a deleted schedule (%d calls total)", got)
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
