//go:build integration

package dispatcher

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// handoffEnv bundles store + notifier + schedule for handoff integration tests.
type handoffEnv struct {
	s        *store.Store
	notifier *HandoffNotifier
	schedID  string
}

func setupHandoffEnv(t *testing.T) *handoffEnv {
	t.Helper()
	s := testutil.SetupDB(t)

	// Seed users and bind their Slack external identities. Epic 7 moved Slack IDs
	// out of the user row into external_identities; the handoff notifier resolves
	// each on-call user's Slack ID via that table. U_NOSLACK intentionally has none.
	users := []struct {
		u       *model.User
		slackID string
	}{
		{&model.User{ID: "U_A", Email: "ua@handoff.test", Name: "User A"}, "S_A"},
		{&model.User{ID: "U_B", Email: "ub@handoff.test", Name: "User B"}, "S_B"},
		{&model.User{ID: "U_C", Email: "uc@handoff.test", Name: "User C"}, "S_C"},
		{&model.User{ID: "U_NOSLACK", Email: "noslack@handoff.test", Name: "No Slack"}, ""},
	}
	for _, tc := range users {
		if err := s.CreateUser(tc.u); err != nil {
			t.Fatalf("CreateUser %s: %v", tc.u.ID, err)
		}
		if tc.slackID != "" {
			testutil.BindSlack(t, s, tc.u.ID, tc.slackID)
		}
	}

	// Seed team
	team := &model.Team{ID: "team-handoff", Name: "Handoff Team"}
	if err := s.CreateTeam(team); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	// Schedule with daily rotation, start in the past so the first group is currently on-call
	schedID := "sched-handoff"
	if err := s.CreateSchedule(&model.Schedule{
		ID:              schedID,
		TeamID:          team.ID,
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "09:00",
		L1RotationStart: time.Now().Add(-7 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	notifier := NewHandoffNotifier(s, staticDmProviders{"slack"}, time.Minute)
	return &handoffEnv{s: s, notifier: notifier, schedID: schedID}
}

// countHandoffJobs returns total handoff_notify jobs in DB for a given dedup prefix.
func countHandoffJobs(t *testing.T, s *store.Store, schedID string) int {
	t.Helper()
	var count int
	err := s.GetDB().QueryRow(`
		SELECT COUNT(*) FROM jobs
		WHERE type = 'handoff_notify' AND dedup_key LIKE 'handoff:' || $1 || ':%'
	`, schedID).Scan(&count)
	if err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return count
}

// fetchLatestHandoffJob returns the most recent handoff_notify job for a schedule.
func fetchLatestHandoffJob(t *testing.T, s *store.Store, schedID string) (jobID string, dedupKey string) {
	t.Helper()
	err := s.GetDB().QueryRow(`
		SELECT id, COALESCE(dedup_key, '') FROM jobs
		WHERE type = 'handoff_notify' AND dedup_key LIKE 'handoff:' || $1 || ':%'
		ORDER BY created_at DESC LIMIT 1
	`, schedID).Scan(&jobID, &dedupKey)
	if err != nil {
		t.Fatalf("fetch latest job: %v", err)
	}
	return
}

// fetchJobSteps returns all steps for a given job ordered by step_index.
func fetchJobSteps(t *testing.T, s *store.Store, jobID string) []*model.JobStep {
	t.Helper()
	rows, err := s.GetDB().Query(`
		SELECT id, job_id, stage_id, step_index, step_type, status, data, max_attempts, continue_on_failure
		FROM job_steps WHERE job_id = $1 ORDER BY step_index
	`, jobID)
	if err != nil {
		t.Fatalf("query steps: %v", err)
	}
	defer rows.Close()

	var steps []*model.JobStep
	for rows.Next() {
		st := &model.JobStep{}
		var dataStr string
		if err := rows.Scan(&st.ID, &st.JobID, &st.StageID, &st.StepIndex, &st.StepType, &st.Status, &dataStr, &st.MaxAttempts, &st.ContinueOnFailure); err != nil {
			t.Fatalf("scan step: %v", err)
		}
		st.Data = []byte(dataStr)
		steps = append(steps, st)
	}
	return steps
}

// TestHandoffNotifier_MultiUserGroup_CreatesJobWithFanOut verifies that a group
// of N users produces a job with one stage and N parallel handoff steps.
func TestHandoffNotifier_MultiUserGroup_CreatesJobWithFanOut(t *testing.T) {
	env := setupHandoffEnv(t)

	// Group [U_A, U_B] is currently on-call
	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_B"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	// Pre-warm with a different cached value to force change detection
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cache[env.schedID] = "previous-state"
	env.notifier.cacheMu.Unlock()

	if !env.notifier.checkAll() {
		t.Fatal("checkAll should succeed")
	}

	if got := countHandoffJobs(t, env.s, env.schedID); got != 1 {
		t.Fatalf("Expected 1 handoff job, got %d", got)
	}

	jobID, dedupKey := fetchLatestHandoffJob(t, env.s, env.schedID)
	// Dedup key must contain sorted joined user IDs
	expectedKey := "handoff:" + env.schedID + ":U_A,U_B"
	if dedupKey != expectedKey {
		t.Errorf("Dedup key mismatch: got %q, want %q", dedupKey, expectedKey)
	}

	// One stage
	var stageCount int
	env.s.GetDB().QueryRow(`SELECT COUNT(*) FROM job_stages WHERE job_id = $1`, jobID).Scan(&stageCount)
	if stageCount != 1 {
		t.Errorf("Expected 1 stage, got %d", stageCount)
	}

	// Two steps, both handoff_notify, both ContinueOnFailure=true
	steps := fetchJobSteps(t, env.s, jobID)
	if len(steps) != 2 {
		t.Fatalf("Expected 2 steps, got %d", len(steps))
	}
	gotTargets := make(map[string]bool)
	for _, st := range steps {
		if st.StepType != "handoff_notify" {
			t.Errorf("Step %s: expected type handoff_notify, got %s", st.ID, st.StepType)
		}
		if !st.ContinueOnFailure {
			t.Errorf("Step %s: expected ContinueOnFailure=true", st.ID)
		}
		var data model.HandoffStepData
		if err := json.Unmarshal(st.Data, &data); err != nil {
			t.Fatalf("Unmarshal step data: %v", err)
		}
		gotTargets[data.TargetID] = true
	}
	if !gotTargets["S_A"] || !gotTargets["S_B"] {
		t.Errorf("Expected steps targeting S_A and S_B, got %v", gotTargets)
	}

	// Cache must now hold sorted joined IDs
	env.notifier.cacheMu.RLock()
	cached := env.notifier.cache[env.schedID]
	env.notifier.cacheMu.RUnlock()
	if cached != "U_A,U_B" {
		t.Errorf("Expected cache 'U_A,U_B', got %q", cached)
	}
}

// TestHandoffNotifier_GroupChangeDetected verifies the critical [A,B]→[A,C] case:
// the first user did not change, but the group did, so a new handoff must fire.
func TestHandoffNotifier_GroupChangeDetected(t *testing.T) {
	env := setupHandoffEnv(t)

	// Initial group [A, B]
	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_B"}}); err != nil {
		t.Fatalf("SetScheduleGroups initial: %v", err)
	}

	// Warm up: cache will be populated, no job created
	if !env.notifier.checkAll() {
		t.Fatal("warmup checkAll failed")
	}
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cacheMu.Unlock()

	if got := countHandoffJobs(t, env.s, env.schedID); got != 0 {
		t.Fatalf("Expected 0 jobs after warmup, got %d", got)
	}

	// Change group to [A, C] — A stays, B replaced by C
	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_C"}}); err != nil {
		t.Fatalf("SetScheduleGroups change: %v", err)
	}

	if !env.notifier.checkAll() {
		t.Fatal("second checkAll failed")
	}

	// Exactly one new job must exist
	if got := countHandoffJobs(t, env.s, env.schedID); got != 1 {
		t.Errorf("Expected 1 handoff job after [A,B]→[A,C], got %d", got)
	}

	// Cache reflects new group
	env.notifier.cacheMu.RLock()
	cached := env.notifier.cache[env.schedID]
	env.notifier.cacheMu.RUnlock()
	if cached != "U_A,U_C" {
		t.Errorf("Expected cache 'U_A,U_C', got %q", cached)
	}
}

// TestHandoffNotifier_NoChangeNoJob verifies that a checkAll without any change
// does not produce additional handoff jobs.
func TestHandoffNotifier_NoChangeNoJob(t *testing.T) {
	env := setupHandoffEnv(t)

	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_B"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	// Warmup populates cache without creating jobs
	if !env.notifier.checkAll() {
		t.Fatal("warmup failed")
	}
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cacheMu.Unlock()

	// Second checkAll with no changes
	if !env.notifier.checkAll() {
		t.Fatal("second checkAll failed")
	}

	if got := countHandoffJobs(t, env.s, env.schedID); got != 0 {
		t.Errorf("Expected 0 handoff jobs (no change), got %d", got)
	}
}

// TestHandoffNotifier_PartialSlackIDs verifies that users without SlackUserID
// are skipped individually without blocking the rest of the group.
func TestHandoffNotifier_PartialSlackIDs(t *testing.T) {
	env := setupHandoffEnv(t)

	// Group [U_A, U_NOSLACK] — U_NOSLACK has no SlackUserID
	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A", "U_NOSLACK"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cache[env.schedID] = "previous-state"
	env.notifier.cacheMu.Unlock()

	if !env.notifier.checkAll() {
		t.Fatal("checkAll failed")
	}

	if got := countHandoffJobs(t, env.s, env.schedID); got != 1 {
		t.Fatalf("Expected 1 handoff job, got %d", got)
	}

	jobID, _ := fetchLatestHandoffJob(t, env.s, env.schedID)
	steps := fetchJobSteps(t, env.s, jobID)

	// Only 1 step — U_NOSLACK skipped
	if len(steps) != 1 {
		t.Fatalf("Expected 1 step (U_NOSLACK skipped), got %d", len(steps))
	}
	var data model.HandoffStepData
	if err := json.Unmarshal(steps[0].Data, &data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if data.TargetID != "S_A" {
		t.Errorf("Expected target S_A, got %s", data.TargetID)
	}
}

// TestHandoffNotifier_MultiProvider_FanOut proves the handoff fan-out is
// capability-driven, not Slack-specific: a single on-call user with identities
// on two dm-capable providers (slack + fake) gets one step per provider, each
// carrying the right HandoffStepData.ProviderName/TargetID. A third identity on
// a provider that is NOT dm-capable (email) is excluded.
func TestHandoffNotifier_MultiProvider_FanOut(t *testing.T) {
	env := setupHandoffEnv(t)

	// Treat both slack and a fake provider as dm-capable for this run.
	env.notifier = NewHandoffNotifier(env.s, staticDmProviders{"slack", "fake"}, time.Minute)

	// U_A already has slack -> S_A from setupHandoffEnv. Add a second dm-capable
	// identity (fake -> F_A) and a non-dm-capable one (email -> E_A) that must be skipped.
	testutil.BindIdentity(t, env.s, "U_A", "fake", "F_A")
	testutil.BindIdentity(t, env.s, "U_A", "email", "E_A")

	if err := env.s.SetScheduleGroups(env.schedID, [][]string{{"U_A"}}); err != nil {
		t.Fatalf("SetScheduleGroups: %v", err)
	}

	// Force change detection.
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cache[env.schedID] = "previous-state"
	env.notifier.cacheMu.Unlock()

	if !env.notifier.checkAll() {
		t.Fatal("checkAll should succeed")
	}

	jobID, _ := fetchLatestHandoffJob(t, env.s, env.schedID)
	steps := fetchJobSteps(t, env.s, jobID)

	// One step per dm-capable provider for U_A: slack + fake (email excluded).
	byProvider := make(map[string]string) // provider -> targetID
	for _, st := range steps {
		if st.StepType != "handoff_notify" {
			t.Errorf("step %s: type %q, want handoff_notify", st.ID, st.StepType)
		}
		if !st.ContinueOnFailure {
			t.Errorf("step %s: expected ContinueOnFailure=true", st.ID)
		}
		var data model.HandoffStepData
		if err := json.Unmarshal(st.Data, &data); err != nil {
			t.Fatalf("unmarshal step data: %v", err)
		}
		if _, dup := byProvider[data.ProviderName]; dup {
			t.Errorf("duplicate step for provider %q", data.ProviderName)
		}
		byProvider[data.ProviderName] = data.TargetID
	}

	if len(steps) != 2 {
		t.Fatalf("expected 2 fan-out steps (slack + fake), got %d: %+v", len(steps), byProvider)
	}
	if byProvider["slack"] != "S_A" {
		t.Errorf("slack step: got target %q, want S_A", byProvider["slack"])
	}
	if byProvider["fake"] != "F_A" {
		t.Errorf("fake step: got target %q, want F_A", byProvider["fake"])
	}
	if tgt, ok := byProvider["email"]; ok {
		t.Errorf("non-dm-capable email identity must be excluded, but got step targeting %q", tgt)
	}
}
