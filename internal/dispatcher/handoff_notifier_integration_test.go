//go:build integration

package dispatcher

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// Rotation group identities. L1 group IDs must be canonical UUIDs.
const (
	handoffGroupA = "aaaaaaaa-2222-4222-8222-000000000001"
	handoffGroupB = "bbbbbbbb-2222-4222-8222-000000000002"
	handoffGroupC = "cccccccc-2222-4222-8222-000000000003"
)

// handoffEnv drives the notifier over the real projection against PostgreSQL.
//
// The clock is shared by the schedule service and the renderer, and it moves
// only when the test moves it: the notifier reads on-call state through the
// renderer's clock, so a test that advanced one and not the other would be
// asserting about two different moments.
type handoffEnv struct {
	t        *testing.T
	s        *store.Store
	config   *scheduleconfig.Service
	notifier *HandoffNotifier
	renderer *schedulerender.Service
	teamID   string
	schedID  string
	now      time.Time
	version  int64
}

func setupHandoffEnv(t *testing.T) *handoffEnv {
	t.Helper()
	s := testutil.SetupDB(t)

	env := &handoffEnv{
		t:      t,
		s:      s,
		teamID: "team-handoff",
		// A Monday, so a weekly policy is easy to reason about; the tests here
		// use a daily one.
		now: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
	}
	clock := func() time.Time { return env.now }

	// Epic 7 moved Slack IDs out of the user row into external_identities; the
	// notifier resolves each on-call user's dm target through that table.
	// U_NOSLACK intentionally has none.
	users := []struct {
		u       *model.User
		slackID string
	}{
		{&model.User{ID: "U_A", Email: "ua@handoff.test", Name: "User A"}, "S_A"},
		{&model.User{ID: "U_B", Email: "ub@handoff.test", Name: "User B"}, "S_B"},
		{&model.User{ID: "U_C", Email: "uc@handoff.test", Name: "User C"}, "S_C"},
		{&model.User{ID: "U_NOSLACK", Email: "noslack@handoff.test", Name: "No Slack"}, ""},
	}
	if err := s.CreateTeam(&model.Team{ID: env.teamID, Name: "Handoff Team"}); err != nil {
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
	env.renderer = schedulerender.New(s.ScheduleReadRepository(), schedulerender.WithClock(clock))
	env.notifier = NewHandoffNotifier(s, env.renderer, staticDmProviders{"slack"}, time.Minute)
	return env
}

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

// save writes a configuration through the real command service.
func (e *handoffEnv) save(cfg rotation.ScheduleConfiguration) {
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

// advance moves the shared clock and runs one notifier tick.
func (e *handoffEnv) tick(d time.Duration) {
	e.t.Helper()
	e.now = e.now.Add(d)
	if !e.notifier.checkAll(context.Background()) {
		e.t.Fatal("tick reported a call failure")
	}
}

// warmUp runs the silent first pass.
func (e *handoffEnv) warmUp() {
	e.t.Helper()
	e.tick(0)
	if got := e.handoffJobs(); len(got) != 0 {
		e.t.Fatalf("warm-up created %d jobs, want none", len(got))
	}
	e.notifier.cacheMu.Lock()
	e.notifier.warmedUp = true
	e.notifier.cacheMu.Unlock()
}

type notifyJob struct {
	id       string
	dedupKey string
	status   string
}

// handoffJobs lists the notification jobs in the database, oldest first.
func (e *handoffEnv) handoffJobs() []notifyJob {
	e.t.Helper()
	rows, err := e.s.GetDB().Query(`
		SELECT id, COALESCE(dedup_key, ''), status FROM jobs
		WHERE type = 'handoff_notify' ORDER BY created_at, id`)
	if err != nil {
		e.t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()

	var out []notifyJob
	for rows.Next() {
		var j notifyJob
		if err := rows.Scan(&j.id, &j.dedupKey, &j.status); err != nil {
			e.t.Fatalf("scan job: %v", err)
		}
		out = append(out, j)
	}
	return out
}

// stepTargets lists the dm targets of one job, by provider.
func (e *handoffEnv) stepTargets(jobID string) map[string]string {
	e.t.Helper()
	rows, err := e.s.GetDB().Query(`
		SELECT step_type, data, continue_on_failure FROM job_steps
		WHERE job_id = $1 ORDER BY step_index`, jobID)
	if err != nil {
		e.t.Fatalf("query steps: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var stepType, data string
		var continueOnFailure bool
		if err := rows.Scan(&stepType, &data, &continueOnFailure); err != nil {
			e.t.Fatalf("scan step: %v", err)
		}
		if stepType != "handoff_notify" {
			e.t.Errorf("step type = %q, want handoff_notify", stepType)
		}
		if !continueOnFailure {
			e.t.Error("a fan-out step must not block its siblings")
		}
		var parsed model.HandoffStepData
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			e.t.Fatalf("unmarshal step data: %v", err)
		}
		if _, dup := out[parsed.ProviderName]; dup {
			e.t.Errorf("duplicate step for provider %q", parsed.ProviderName)
		}
		out[parsed.ProviderName] = parsed.TargetID
	}
	return out
}

// TestHandoffNotifierOnRevisionModel is the end-to-end shape of the tick: the
// notifier reads a schedule created through the revision model, and the group
// coming on duty gets one step per dm-capable identity.
func TestHandoffNotifierOnRevisionModel(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B", "U_C"),
	))
	env.warmUp()

	env.tick(24 * time.Hour)

	jobs := env.handoffJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs after one handoff, want 1: %+v", len(jobs), jobs)
	}

	// Both members of the incoming group are notified, and nobody else. The
	// recipients are read off the step rows rather than through stepTargets,
	// which keys by provider and is for the multi-provider fan-out.
	rows, err := env.s.GetDB().Query(`SELECT data FROM job_steps WHERE job_id = $1`, jobs[0].id)
	if err != nil {
		t.Fatalf("query steps: %v", err)
	}
	defer rows.Close()
	targets := map[string]bool{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var parsed model.HandoffStepData
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		targets[parsed.TargetID] = true
		if parsed.ScheduleID != env.schedID {
			t.Errorf("step schedule = %q, want %q", parsed.ScheduleID, env.schedID)
		}
	}
	if !targets["S_B"] || !targets["S_C"] || targets["S_A"] {
		t.Errorf("notified %v, want the incoming group only", targets)
	}
}

// TestHandoffNotifierRepeatedCompositionIsNotDeduped is the reason the dedup key
// carries the moment of activation. A rotation returns to a group it has served
// before; if the key were the composition alone, the second - legitimate -
// notification would be swallowed by the unique index while the first job was
// still pending.
func TestHandoffNotifierRepeatedCompositionIsNotDeduped(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))
	env.warmUp()

	// A -> B -> A -> B over three days. Every job stays pending, so the unique
	// index on dedup_key is live for all of them.
	for i := 0; i < 3; i++ {
		env.tick(24 * time.Hour)
	}

	jobs := env.handoffJobs()
	if len(jobs) != 3 {
		t.Fatalf("%d jobs over three handoffs, want 3: %+v", len(jobs), jobs)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if j.status != string(model.JobStatusPending) {
			t.Errorf("job %s is %s; the test needs them pending for dedup to be live", j.id, j.status)
		}
		if seen[j.dedupKey] {
			t.Errorf("duplicate dedup key %q", j.dedupKey)
		}
		seen[j.dedupKey] = true
	}
	// The first and third handoffs put the same group on duty, so their keys
	// differ only in the moment of activation.
	if len(seen) != 3 {
		t.Fatalf("dedup keys = %v, want three distinct ones", seen)
	}
}

// TestHandoffNotifierEditedGroupIsNotDeduped is the same argument for an edit:
// [B] -> [B,D] -> [B] -> [B,D] returns to a composition it has already had, and
// the second addition must still be announced.
func TestHandoffNotifierEditedGroupIsNotDeduped(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.warmUp()

	// Add U_B to the group on duty, remove them, add them again. Each save is a
	// revision, so each addition takes effect at a different instant.
	for i := 0; i < 3; i++ {
		env.now = env.now.Add(time.Hour)
		if i%2 == 0 {
			env.save(dailyConfig(group(handoffGroupA, "U_A", "U_B")))
		} else {
			env.save(dailyConfig(group(handoffGroupA, "U_A")))
		}
		env.tick(time.Minute)
	}

	jobs := env.handoffJobs()
	// Two additions produce two notifications; the removal produces none.
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want one per addition: %+v", len(jobs), jobs)
	}
	if jobs[0].dedupKey == jobs[1].dedupKey {
		t.Fatalf("both additions produced the dedup key %q", jobs[0].dedupKey)
	}
	for _, j := range jobs {
		if got := env.stepTargets(j.id)["slack"]; got != "S_B" {
			t.Errorf("job %s notified %q, want the added member alone", j.id, got)
		}
	}
}

// TestHandoffNotifierTwoInstancesCreateOneJob: two processes observe the same
// transition, and the dedup key is what makes them agree. This is why the cache
// may live in memory per instance.
func TestHandoffNotifierTwoInstancesCreateOneJob(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))

	second := NewHandoffNotifier(env.s, env.renderer, staticDmProviders{"slack"}, time.Minute)

	// Both instances warm up on the same state.
	env.warmUp()
	if !second.checkAll(context.Background()) {
		t.Fatal("second instance failed to warm up")
	}
	second.cacheMu.Lock()
	second.warmedUp = true
	second.cacheMu.Unlock()

	// Both see the same handoff.
	env.now = env.now.Add(24 * time.Hour)
	if !env.notifier.checkAll(context.Background()) {
		t.Fatal("first instance tick failed")
	}
	if !second.checkAll(context.Background()) {
		t.Fatal("second instance tick failed")
	}

	if jobs := env.handoffJobs(); len(jobs) != 1 {
		t.Fatalf("%d jobs from two instances, want 1: %+v", len(jobs), jobs)
	}
}

// TestHandoffNotifierDeleteAndRecreate: the delete records an empty composition,
// so recreating with the same group is a transition again. Without the deleted
// schedule reaching the notifier this cycle would pass in silence.
func TestHandoffNotifierDeleteAndRecreate(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.warmUp()

	env.now = env.now.Add(time.Hour)
	if err := env.config.Delete(context.Background(), env.teamID, scheduleconfig.DeleteCommand{
		ExpectedVersion: env.version,
		ActorID:         "U_A",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	env.version++
	env.tick(time.Minute)
	if jobs := env.handoffJobs(); len(jobs) != 0 {
		t.Fatalf("the delete created %d jobs, want none: %+v", len(jobs), jobs)
	}

	env.now = env.now.Add(time.Hour)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.tick(time.Minute)

	jobs := env.handoffJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs after the recreate, want 1: %+v", len(jobs), jobs)
	}
	if got := env.stepTargets(jobs[0].id)["slack"]; got != "S_A" {
		t.Errorf("notified %q, want S_A - her duty was interrupted", got)
	}
}

// TestHandoffNotifierOverrideBoundaries: a stand-in is told the same way anyone
// coming on call is, and the group gets the same message when duty comes back.
func TestHandoffNotifierOverrideBoundaries(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.warmUp()

	from := env.now.Add(time.Hour)
	until := from.Add(2 * time.Hour)
	if _, err := env.config.CreateOverride(context.Background(), env.teamID, scheduleconfig.OverrideCommand{
		UserID:    "U_C",
		ValidFrom: from,
		ValidTo:   until,
		ActorID:   "U_A",
	}); err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	// Inside the override: the stand-in is on duty.
	env.tick(90 * time.Minute)
	jobs := env.handoffJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs when the override started, want 1: %+v", len(jobs), jobs)
	}
	if got := env.stepTargets(jobs[0].id)["slack"]; got != "S_C" {
		t.Errorf("notified %q, want the stand-in S_C", got)
	}

	// After it: the rotation is back.
	env.tick(2 * time.Hour)
	jobs = env.handoffJobs()
	if len(jobs) != 2 {
		t.Fatalf("%d jobs after the override ended, want 2: %+v", len(jobs), jobs)
	}
	if got := env.stepTargets(jobs[1].id)["slack"]; got != "S_A" {
		t.Errorf("notified %q, want the returning group S_A", got)
	}
}

// TestHandoffNotifierPartialIdentities: a user with no dm-capable identity is
// skipped individually, without blocking the rest of the group.
func TestHandoffNotifierPartialIdentities(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B", "U_NOSLACK"),
	))
	env.warmUp()

	env.tick(24 * time.Hour)

	jobs := env.handoffJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want 1: %+v", len(jobs), jobs)
	}
	var stepCount int
	if err := env.s.GetDB().QueryRow(`SELECT COUNT(*) FROM job_steps WHERE job_id = $1`,
		jobs[0].id).Scan(&stepCount); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	if stepCount != 1 {
		t.Fatalf("%d steps, want 1 - the user with no identity is skipped", stepCount)
	}
	if got := env.stepTargets(jobs[0].id)["slack"]; got != "S_B" {
		t.Errorf("notified %q, want S_B", got)
	}
}

// TestHandoffNotifierMultiProviderFanOut proves the fan-out is
// capability-driven, not Slack-specific: one on-call user with identities on two
// dm-capable providers gets one step per provider, and an identity on a provider
// that is not dm-capable is excluded.
func TestHandoffNotifierMultiProviderFanOut(t *testing.T) {
	env := setupHandoffEnv(t)
	env.notifier = NewHandoffNotifier(env.s, env.renderer, staticDmProviders{"slack", "fake"}, time.Minute)

	testutil.BindIdentity(t, env.s, "U_B", "fake", "F_B")
	testutil.BindIdentity(t, env.s, "U_B", "email", "E_B")

	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))
	env.warmUp()

	env.tick(24 * time.Hour)

	jobs := env.handoffJobs()
	if len(jobs) != 1 {
		t.Fatalf("%d jobs, want 1: %+v", len(jobs), jobs)
	}
	got := env.stepTargets(jobs[0].id)
	if len(got) != 2 {
		t.Fatalf("fan-out targets = %v, want slack + fake", got)
	}
	if got["slack"] != "S_B" || got["fake"] != "F_B" {
		t.Errorf("fan-out targets = %v, want S_B and F_B", got)
	}
	if target, ok := got["email"]; ok {
		t.Errorf("a provider that is not dm-capable produced a step targeting %q", target)
	}
}

// TestHandoffNotifierThreeGroupsNoRepeatWithinOneShift: consecutive ticks inside
// one shift observe the same composition and must stay silent - the notifier is
// polling, and a tick is not an event.
func TestHandoffNotifierThreeGroupsNoRepeatWithinOneShift(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
		group(handoffGroupC, "U_C"),
	))
	env.warmUp()

	for i := 0; i < 5; i++ {
		env.tick(time.Minute)
	}
	if jobs := env.handoffJobs(); len(jobs) != 0 {
		t.Fatalf("%d jobs from ticks inside one shift, want none: %+v", len(jobs), jobs)
	}
}

// TestHandoffNotifierEditedOverrideIsNotDeduped is the end-to-end form of the
// case AssignmentStart cannot carry.
//
// Editing an override leaves its valid_from alone, so the same person coming
// back onto the same override yields the same composition, the same kind AND the
// same instant. Only the override revision differs, and the second job must
// still be created while the first is pending.
func TestHandoffNotifierEditedOverrideIsNotDeduped(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.warmUp()

	from := env.now
	until := env.now.Add(8 * time.Hour)
	override, err := env.config.CreateOverride(context.Background(), env.teamID, scheduleconfig.OverrideCommand{
		UserID:    "U_B",
		ValidFrom: from,
		ValidTo:   until,
		ActorID:   "U_A",
	})
	if err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}
	env.tick(time.Minute)

	// U_C -> U_B -> U_C, all inside the one override interval. The first and
	// third edits are the same event twice over: same holder, same override,
	// same valid_from, same kind.
	revision := override.Revision
	for i, user := range []string{"U_C", "U_B", "U_C"} {
		env.now = env.now.Add(time.Hour)
		updated, err := env.config.UpdateOverride(context.Background(), env.schedID, override.OverrideID,
			revision, scheduleconfig.OverrideCommand{
				UserID:    user,
				ValidFrom: from,
				ValidTo:   until,
				ActorID:   "U_A",
			})
		if err != nil {
			t.Fatalf("UpdateOverride %d: %v", i, err)
		}
		revision = updated.Revision
		env.tick(time.Minute)
	}

	jobs := env.handoffJobs()
	// One per activation: U_B, then U_C, U_B, U_C.
	if len(jobs) != 4 {
		t.Fatalf("%d jobs over four activations, want 4: %+v", len(jobs), jobs)
	}
	if jobs[1].dedupKey == jobs[3].dedupKey {
		t.Fatalf("both activations of U_C share the dedup key %q; the second was suppressed", jobs[1].dedupKey)
	}
	for _, j := range jobs {
		if j.status != string(model.JobStatusPending) {
			t.Errorf("job %s is %s; the test needs them pending for dedup to be live", j.id, j.status)
		}
	}
	if got := env.stepTargets(jobs[3].id)["slack"]; got != "S_C" {
		t.Errorf("the last job notified %q, want S_C back on the override", got)
	}
}
