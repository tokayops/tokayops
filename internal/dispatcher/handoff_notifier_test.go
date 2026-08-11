package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// counterValue reads a counter the way the API tests do, so the assertions can
// be about what was counted rather than about a metrics harness.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// mockNotifierStore implements the store methods HandoffNotifier still needs:
// team names, linked identities and job creation. On-call state does not come
// from here any more - it comes from the projection.
//
// It does NOT embed store.StoreInterface. While it did, the double compiled
// whether or not it implemented the methods under test, and every unimplemented
// call was a nil-pointer panic waiting rather than a compile error. Now the
// three methods below are the whole contract, and dropping one stops the build.
type mockNotifierStore struct {
	teams    []*model.Team
	slackIDs map[string]string // userID -> slack external id ("" means not linked)
	jobs     []*createdJob

	getTeamsErr  error
	createJobErr error

	// dedupHits are dedup keys an active job already holds, as a second
	// instance's write would find them.
	dedupHits map[string]bool
}

type createdJob struct {
	job   *model.Job
	steps []*model.JobStep
}

func (m *mockNotifierStore) GetAllTeams() ([]*model.Team, error) {
	if m.getTeamsErr != nil {
		return nil, m.getTeamsErr
	}
	return m.teams, nil
}

// GetIdentitiesForUsers reads the per-test slackIDs map. A user with an empty or
// missing entry has no dm-capable identity → the notifier skips them.
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

func (m *mockNotifierStore) CreateJobWithDedup(job *model.Job, _ []*model.JobStage, steps []*model.JobStep) (string, bool, error) {
	if m.createJobErr != nil {
		return "", false, m.createJobErr
	}
	if job.DedupKey != nil && m.dedupHits[*job.DedupKey] {
		return "existing-job", false, nil
	}
	m.jobs = append(m.jobs, &createdJob{job: job, steps: steps})
	return job.ID, true, nil
}

func (m *mockNotifierStore) dedupKeys() []string {
	var out []string
	for _, j := range m.jobs {
		if j.job.DedupKey != nil {
			out = append(out, *j.job.DedupKey)
		}
	}
	return out
}

// notifierEnv is a warmed-up notifier over a projection a test drives.
type notifierEnv struct {
	t        *testing.T
	store    *mockNotifierStore
	oncall   *fakeOnCall
	notifier *HandoffNotifier
}

func newNotifierEnv(t *testing.T, slackIDs map[string]string) *notifierEnv {
	t.Helper()
	st := &mockNotifierStore{
		teams:     []*model.Team{{ID: "team-1", Name: "Backend"}},
		slackIDs:  slackIDs,
		dedupHits: map[string]bool{},
	}
	oncall := &fakeOnCall{}
	return &notifierEnv{
		t:        t,
		store:    st,
		oncall:   oncall,
		notifier: NewHandoffNotifier(st, oncall, staticDmProviders{"slack"}, time.Minute),
	}
}

// tick observes one projection state. The first tick of a test is the warm-up
// pass unless warmUp() already ran.
func (e *notifierEnv) tick(schedules ...schedulerender.ScheduleOnCall) bool {
	e.t.Helper()
	e.oncall.set(schedules...)
	return e.notifier.checkAll(context.Background())
}

// warmUp runs the silent first pass and asserts it created nothing.
func (e *notifierEnv) warmUp(schedules ...schedulerender.ScheduleOnCall) {
	e.t.Helper()
	if !e.tick(schedules...) {
		e.t.Fatal("warm-up tick reported a call failure")
	}
	if len(e.store.jobs) != 0 {
		e.t.Fatalf("warm-up created %d jobs, want none", len(e.store.jobs))
	}
	e.notifier.cacheMu.Lock()
	e.notifier.warmedUp = true
	e.notifier.cacheMu.Unlock()
}

func (e *notifierEnv) jobs() []*createdJob { return e.store.jobs }

// targets lists the DM recipients of the one job the test expects.
func (e *notifierEnv) targets() []string {
	e.t.Helper()
	if len(e.store.jobs) != 1 {
		e.t.Fatalf("expected exactly 1 job, got %d", len(e.store.jobs))
	}
	var out []string
	for _, step := range e.store.jobs[0].steps {
		var data model.HandoffStepData
		if err := json.Unmarshal(step.Data, &data); err != nil {
			e.t.Fatalf("unmarshal step data: %v", err)
		}
		out = append(out, data.TargetID)
	}
	return out
}

func (e *notifierEnv) message() string {
	e.t.Helper()
	if len(e.store.jobs) != 1 || len(e.store.jobs[0].steps) == 0 {
		e.t.Fatalf("expected one job with steps, got %d jobs", len(e.store.jobs))
	}
	var data model.HandoffStepData
	if err := json.Unmarshal(e.store.jobs[0].steps[0].Data, &data); err != nil {
		e.t.Fatalf("unmarshal step data: %v", err)
	}
	return data.Message
}

func (e *notifierEnv) cached(scheduleID string) *composition {
	return e.notifier.cached(scheduleID)
}

func slackIDsFor(users ...string) map[string]string {
	out := make(map[string]string, len(users))
	for _, u := range users {
		out[u] = "U-" + strings.ToUpper(u)
	}
	return out
}

// TestNotifierNaturalHandoff: the incoming group hears about it, and only the
// people who were not on duty a moment ago.
func TestNotifierNaturalHandoff(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob", "carol"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice", "bob"))

	env.tick(rotationDuty("sched-1", "g-b", "bob", "carol"))

	if got := env.targets(); strings.Join(got, ",") != "U-CAROL" {
		t.Fatalf("notified %v, want carol alone - bob was already on call", got)
	}
	if key := env.jobs()[0].job.DedupKey; key == nil || !strings.HasPrefix(*key, kindHandoff+":sched-1:") {
		t.Fatalf("dedup key = %v, want a handoff key for sched-1", key)
	}
}

// TestNotifierAddedToActiveShift: same group, new member. It is an edit, not a
// rotation, and it says so.
func TestNotifierAddedToActiveShift(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("bob", "dave"))
	env.warmUp(rotationDuty("sched-1", "g-b", "bob"))

	env.tick(rotationDuty("sched-1", "g-b", "bob", "dave"))

	if got := env.targets(); strings.Join(got, ",") != "U-DAVE" {
		t.Fatalf("notified %v, want dave alone", got)
	}
	if key := env.jobs()[0].job.DedupKey; key == nil || !strings.HasPrefix(*key, kindAddedToActiveShift+":") {
		t.Fatalf("dedup key = %v, want an added_to_active_shift key", key)
	}
	if msg := env.message(); !strings.Contains(msg, "added to the on-call shift in progress") {
		t.Fatalf("message does not announce joining a shift in progress:\n%s", msg)
	}
}

// TestNotifierSilentTransitions collects every state change that must produce
// nothing at all.
func TestNotifierSilentTransitions(t *testing.T) {
	tests := []struct {
		name        string
		first, then schedulerender.ScheduleOnCall
	}{
		{
			name:  "somebody removed from the group on duty",
			first: rotationDuty("sched-1", "g-b", "bob", "dave"),
			then:  rotationDuty("sched-1", "g-b", "bob"),
		},
		{
			name:  "the same composition observed again",
			first: rotationDuty("sched-1", "g-a", "alice"),
			then:  rotationDuty("sched-1", "g-a", "alice"),
		},
		{
			name:  "the group emptied",
			first: rotationDuty("sched-1", "g-a", "alice"),
			then:  emptyDuty("sched-1"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newNotifierEnv(t, slackIDsFor("alice", "bob", "dave"))
			env.warmUp(tc.first)
			env.tick(tc.then)
			if len(env.jobs()) != 0 {
				t.Fatalf("created %d jobs, want none", len(env.jobs()))
			}
		})
	}
}

// TestNotifierOverrideBoundaries: a stand-in is told the same way anyone coming
// on call is, and so is the group the override hands duty back to.
func TestNotifierOverrideBoundaries(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "carol"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	env.tick(overrideDuty("sched-1", "ovr-1", "carol"))
	if got := env.targets(); strings.Join(got, ",") != "U-CAROL" {
		t.Fatalf("override start notified %v, want the stand-in", got)
	}

	env.store.jobs = nil
	env.tick(rotationDuty("sched-1", "g-a", "alice"))
	if got := env.targets(); strings.Join(got, ",") != "U-ALICE" {
		t.Fatalf("override end notified %v, want the returning group", got)
	}
}

// TestNotifierSingleGroupScheduleAcrossBoundaries: the case that forbids putting
// a slot boundary in the composition. The same person is on duty shift after
// shift; they are told once, when they first come on, and never again.
func TestNotifierSingleGroupScheduleAcrossBoundaries(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	for i := 1; i <= 3; i++ {
		slot := dutyBase.Add(time.Duration(i) * 24 * time.Hour)
		env.tick(duty(dutySpec{
			scheduleID: "sched-1",
			source:     schedulerender.SourceRotation,
			groupID:    "g-a",
			users:      []string{"alice"},
			slotStart:  slot,
		}))
	}
	if len(env.jobs()) != 0 {
		t.Fatalf("created %d jobs over three boundaries, want none", len(env.jobs()))
	}
}

// TestNotifierFirstObservationIsSilent: the cutover safeguard. The notifier
// warms up against an empty database, the operator recreates schedules one by
// one, and each one is a first observation - not twenty DM fan-outs in the
// middle of a maintenance window.
func TestNotifierFirstObservationIsSilent(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp() // nothing exists yet

	env.tick(rotationDuty("sched-1", "g-a", "alice"))

	if len(env.jobs()) != 0 {
		t.Fatalf("created %d jobs for a schedule seen for the first time, want none", len(env.jobs()))
	}
	if got := env.cached("sched-1"); got == nil || strings.Join(got.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v, want the observation recorded silently", got)
	}
}

// TestNotifierDeleteAndRecreateNotifies: the reason the projection reports
// deleted schedules. Filter them out and the cache keeps the old composition, so
// recreating with the same group passes in silence even though duty was
// interrupted.
func TestNotifierDeleteAndRecreateNotifies(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	deletedAt := dutyBase.Add(time.Hour)
	env.tick(duty(dutySpec{scheduleID: "sched-1", deletedAt: &deletedAt}))
	if len(env.jobs()) != 0 {
		t.Fatalf("the delete itself created %d jobs, want none", len(env.jobs()))
	}
	cached := env.cached("sched-1")
	if cached == nil || !cached.empty() {
		t.Fatalf("cache = %+v, want a recorded empty composition", cached)
	}

	env.tick(rotationDuty("sched-1", "g-a", "alice"))
	if got := env.targets(); strings.Join(got, ",") != "U-ALICE" {
		t.Fatalf("recreate notified %v, want alice - her duty was interrupted", got)
	}
}

// TestNotifierDamagedScheduleIsIsolated: one corrupt schedule costs exactly
// itself. The others are processed, its own cache is left alone - writing an
// empty composition would turn corruption into "duty ended" and, on repair, into
// a notification nobody earned - and the failure is counted.
func TestNotifierDamagedScheduleIsIsolated(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(
		rotationDuty("sched-broken", "g-a", "alice"),
		rotationDuty("sched-healthy", "g-a", "alice"),
	)

	before := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.WithLabelValues(
		metrics.ConsumerHandoffNotifier, string(schedulerender.FailureSnapshotDecode)))

	env.oncall.setBulk(schedulerender.BulkOnCall{
		Schedules: []schedulerender.ScheduleOnCall{rotationDuty("sched-healthy", "g-b", "bob")},
		Failures: []schedulerender.ProjectionFailure{{
			ScheduleID: "sched-broken",
			TeamID:     "team-1",
			Reason:     schedulerender.FailureSnapshotDecode,
			Err:        errors.New("snapshot could not be decoded"),
		}},
	})
	if !env.notifier.checkAll(context.Background()) {
		t.Fatal("a damaged schedule failed the whole tick")
	}

	if got := env.targets(); strings.Join(got, ",") != "U-BOB" {
		t.Fatalf("notified %v, want the healthy schedule's handoff to bob", got)
	}
	if cached := env.cached("sched-broken"); cached == nil || strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache of the damaged schedule = %+v, want it untouched", cached)
	}
	after := counterValue(t, metrics.ScheduleOnCallProjectionFailuresTotal.WithLabelValues(
		metrics.ConsumerHandoffNotifier, string(schedulerender.FailureSnapshotDecode)))
	if after-before != 1 {
		t.Errorf("projection failure counter moved by %v, want 1", after-before)
	}
}

// TestNotifierCallFailureTouchesNothing: a tick that could not read leaves every
// cache as it was, so the next tick sees the same transitions it would have.
func TestNotifierCallFailureTouchesNothing(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	env.oncall.fail(errors.New("could not begin transaction"))
	if env.notifier.checkAll(context.Background()) {
		t.Fatal("a read failure reported success")
	}
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v after a read failure, want it untouched", cached)
	}

	env.oncall.fail(nil)
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "U-BOB" {
		t.Fatalf("the retry notified %v, want the handoff it deferred", got)
	}
}

// TestNotifierWarmUpCompletesDespiteDamage is the regression against rebuilding
// the blast radius through the state machine. The old checkAll returned false on
// any error, so one damaged schedule meant warm-up never finished and NOBODY was
// ever notified. Only a failure of the call itself may hold warm-up up.
func TestNotifierWarmUpCompletesDespiteDamage(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.oncall.setBulk(schedulerender.BulkOnCall{
		Schedules: []schedulerender.ScheduleOnCall{rotationDuty("sched-healthy", "g-a", "alice")},
		Failures: []schedulerender.ProjectionFailure{{
			ScheduleID: "sched-broken", TeamID: "team-1",
			Reason: schedulerender.FailureRevisionGap, Err: errors.New("no revision in force"),
		}},
	})
	if !env.notifier.checkAll(context.Background()) {
		t.Fatal("warm-up was blocked by a damaged schedule")
	}
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cacheMu.Unlock()

	// The healthy schedule was seeded, so its next transition is a real one.
	env.tick(rotationDuty("sched-healthy", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "U-BOB" {
		t.Fatalf("notified %v, want the healthy schedule's handoff", got)
	}
}

// TestNotifierRepairedScheduleIsSilentOnce: the accepted cost of the rule above.
// A schedule damaged at start-up stays unknown, so its first transition after a
// repair passes silently - like any first observation. The alternative, calling
// it "observed with nobody on duty", would DM a group whose duty never changed.
func TestNotifierRepairedScheduleIsSilentOnce(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.oncall.setBulk(schedulerender.BulkOnCall{
		Failures: []schedulerender.ProjectionFailure{{
			ScheduleID: "sched-1", TeamID: "team-1",
			Reason: schedulerender.FailureSnapshotDecode, Err: errors.New("boom"),
		}},
	})
	if !env.notifier.checkAll(context.Background()) {
		t.Fatal("warm-up was blocked by a damaged schedule")
	}
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cacheMu.Unlock()

	if env.cached("sched-1") != nil {
		t.Fatal("the damaged schedule was cached; it must stay unknown")
	}

	// Repaired: first observation, silent, cache filled.
	env.tick(rotationDuty("sched-1", "g-a", "alice"))
	if len(env.jobs()) != 0 {
		t.Fatalf("the repair created %d jobs, want none", len(env.jobs()))
	}
	// And the next real transition is announced.
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "U-BOB" {
		t.Fatalf("notified %v, want the first transition after the repair", got)
	}
}

// TestNotifierWarmUpBlockedByCallFailure: nothing was read, so nothing was
// seeded, and finishing warm-up would make the next tick treat every schedule as
// a first observation of a state it never actually saw.
func TestNotifierWarmUpBlockedByCallFailure(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.oncall.fail(errors.New("no connection"))

	if env.notifier.checkAll(context.Background()) {
		t.Fatal("warm-up completed on a tick that read nothing")
	}
	if env.cached("sched-1") != nil {
		t.Fatal("something was cached by a failed tick")
	}
}

// TestNotifierWarmUpBlockedByTeamReadFailure: the team name is only how the
// message addresses the reader, but a tick that cannot read teams cannot be
// trusted to have read anything else either.
func TestNotifierWarmUpBlockedByTeamReadFailure(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.store.getTeamsErr = errors.New("no connection")

	if env.tick(rotationDuty("sched-1", "g-a", "alice")) {
		t.Fatal("warm-up completed although teams could not be read")
	}
	if env.cached("sched-1") != nil {
		t.Fatal("something was cached by a failed tick")
	}
}

// TestNotifierSecondInstanceCountsOneNotification: two processes detect the same
// transition, the dedup key lets one job through, and the metric counts what was
// sent rather than what was noticed.
func TestNotifierSecondInstanceCountsOneNotification(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	// The other instance got there first and its job is still pending.
	next := observe(rotationDuty("sched-1", "g-b", "bob"))
	env.store.dedupHits[occurrenceKey(kindHandoff, "sched-1", next)] = true

	before := counterValue(t, metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kindHandoff))
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	after := counterValue(t, metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kindHandoff))

	if len(env.jobs()) != 0 {
		t.Fatalf("created %d jobs although one was already pending", len(env.jobs()))
	}
	if after != before {
		t.Errorf("notification counter moved by %v for a job that was not created", after-before)
	}
	// The cache still advances: this instance has seen the transition, and
	// re-detecting it every minute would be noise, not safety.
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "bob" {
		t.Errorf("cache = %+v, want the observed composition", cached)
	}
}

// TestNotifierCountsCreatedJobs: one transition, one unit of the metric, however
// many identities the fan-out reaches.
func TestNotifierCountsCreatedJobs(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob", "carol"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	before := counterValue(t, metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kindHandoff))
	env.tick(rotationDuty("sched-1", "g-b", "bob", "carol"))
	after := counterValue(t, metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kindHandoff))

	if len(env.targets()) != 2 {
		t.Fatalf("fan-out reached %v, want both new members", env.targets())
	}
	if after-before != 1 {
		t.Errorf("notification counter moved by %v for one transition, want 1", after-before)
	}
}

// TestNotifierJobFailureRetriesNextTick: the cache is what makes the retry
// happen, so it must not advance past a job that was never created.
func TestNotifierJobFailureRetriesNextTick(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	env.store.createJobErr = errors.New("transient DB error")
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if len(env.jobs()) != 0 {
		t.Fatalf("created %d jobs despite the failure", len(env.jobs()))
	}
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v, want the old composition so the retry still sees a change", cached)
	}

	env.store.createJobErr = nil
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "U-BOB" {
		t.Fatalf("the retry notified %v, want bob", got)
	}
}

// TestNotifierSkipsUsersWithoutDmIdentity: someone with no dm-capable identity
// cannot be reached, and a step aimed at them would only fail.
func TestNotifierSkipsUsersWithoutDmIdentity(t *testing.T) {
	env := newNotifierEnv(t, map[string]string{"alice": "U-ALICE", "carol": "U-CAROL"})
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	// bob has no identity at all; carol does.
	env.tick(rotationDuty("sched-1", "g-b", "bob", "carol"))
	if got := env.targets(); strings.Join(got, ",") != "U-CAROL" {
		t.Fatalf("notified %v, want carol alone", got)
	}

	// Nobody reachable at all: no job, and the composition is still recorded so
	// the next transition is measured from it.
	env.store.jobs = nil
	env.tick(rotationDuty("sched-1", "g-c", "bob"))
	if len(env.jobs()) != 0 {
		t.Fatalf("created %d jobs for an unreachable group, want none", len(env.jobs()))
	}
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "bob" {
		t.Fatalf("cache = %+v, want the observed composition", cached)
	}
}

// TestNotifierKindsDoNotSuppressEachOther: the same composition, the same
// assignment boundary, two different events. Without the kind in the dedup key
// the second would be swallowed while the first was still pending.
func TestNotifierKindsDoNotSuppressEachOther(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("bob", "dave"))
	env.warmUp(rotationDuty("sched-1", "g-b", "bob"))

	added := duty(dutySpec{
		scheduleID: "sched-1", source: schedulerender.SourceRotation, groupID: "g-b",
		users: []string{"bob", "dave"}, slotStart: dutyBase, start: dutyBase,
	})
	env.tick(added)

	// A handoff arriving at the same composition and the same boundary - the
	// group that just gained dave is now serving a different slot of its own.
	env.notifier.cacheMu.Lock()
	env.notifier.cache["sched-1"] = composition{
		Source: schedulerender.SourceRotation, GroupID: "g-a", UserIDs: []string{"alice"},
	}
	env.notifier.cacheMu.Unlock()
	env.tick(added)

	keys := env.store.dedupKeys()
	if len(keys) != 2 {
		t.Fatalf("created %d jobs, want one per kind: %v", len(keys), keys)
	}
	if keys[0] == keys[1] {
		t.Fatalf("both kinds produced the dedup key %q", keys[0])
	}
	if !strings.HasPrefix(keys[0], kindAddedToActiveShift+":") || !strings.HasPrefix(keys[1], kindHandoff+":") {
		t.Fatalf("dedup keys do not carry their kinds: %v", keys)
	}
}

// TestNotifierMessagesCarryBothBoundaries: both texts print the three instants
// the reader needs, in the schedule's own timezone, and differ in their first
// line. The two pairs of boundaries diverge exactly where it matters - the shift
// began at 11:00, the stand-in's assignment at 14:00.
func TestNotifierMessagesCarryBothBoundaries(t *testing.T) {
	slotStart := time.Date(2026, 5, 4, 4, 0, 0, 0, time.UTC)   // 11:00 in Bangkok
	assignStart := time.Date(2026, 5, 4, 7, 0, 0, 0, time.UTC) // 14:00 in Bangkok
	assignEnd := time.Date(2026, 5, 5, 4, 0, 0, 0, time.UTC)   // 11:00 next day

	shift := dutySpec{
		scheduleID: "sched-1", timezone: "Asia/Bangkok",
		source: schedulerender.SourceRotation, groupID: "g-b", users: []string{"bob"},
		slotStart: slotStart, start: assignStart, end: assignEnd,
	}

	tests := []struct {
		name      string
		first     schedulerender.ScheduleOnCall
		then      schedulerender.ScheduleOnCall
		firstLine string
	}{
		{
			name:      kindHandoff,
			first:     rotationDuty("sched-1", "g-a", "alice"),
			then:      duty(shift),
			firstLine: ":mega: You are now on-call for team *Backend*.",
		},
		{
			name: kindAddedToActiveShift,
			first: duty(dutySpec{
				scheduleID: "sched-1", timezone: "Asia/Bangkok",
				source: schedulerender.SourceRotation, groupID: "g-b", users: []string{"alice"},
				slotStart: slotStart, start: slotStart, end: assignEnd,
			}),
			then: duty(dutySpec{
				scheduleID: "sched-1", timezone: "Asia/Bangkok",
				source: schedulerender.SourceRotation, groupID: "g-b", users: []string{"alice", "bob"},
				slotStart: slotStart, start: assignStart, end: assignEnd,
			}),
			firstLine: ":heavy_plus_sign: You have been added to the on-call shift in progress for team *Backend*.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
			env.warmUp(tc.first)
			env.tick(tc.then)

			msg := env.message()
			lines := strings.Split(msg, "\n")
			if lines[0] != tc.firstLine {
				t.Errorf("first line = %q, want %q", lines[0], tc.firstLine)
			}
			for _, want := range []string{
				"Rotation shift started:         Mon May 4, 11:00 (Asia/Bangkok)",
				"Your assignment effective from: Mon May 4, 14:00 (Asia/Bangkok)",
				"Assignment ends:                Tue May 5, 11:00 (Asia/Bangkok)",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("message is missing %q:\n%s", want, msg)
				}
			}
			if strings.Contains(msg, "indefinite") {
				t.Errorf("message still offers an indefinite end:\n%s", msg)
			}
		})
	}
}

// TestNotifierMessageUsesTheSnapshotTimezone: the zone comes from the
// configuration in force, not from the schedule row - which is the whole reason
// it travels with the projection.
func TestNotifierMessageUsesTheSnapshotTimezone(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(duty(dutySpec{
		scheduleID: "sched-1", timezone: "America/New_York",
		source: schedulerender.SourceRotation, groupID: "g-a", users: []string{"alice"},
	}))
	env.tick(duty(dutySpec{
		scheduleID: "sched-1", timezone: "America/New_York",
		source: schedulerender.SourceRotation, groupID: "g-b", users: []string{"bob"},
		slotStart: time.Date(2026, 5, 4, 15, 0, 0, 0, time.UTC),
	}))

	msg := env.message()
	if !strings.Contains(msg, "(America/New_York)") {
		t.Fatalf("message does not use the snapshot timezone:\n%s", msg)
	}
	if !strings.Contains(msg, "Mon May 4, 11:00") {
		t.Fatalf("message does not render 15:00 UTC as 11:00 local:\n%s", msg)
	}
}

// TestNotifierUnknownTimezoneFallsBackToUTC: a zone the runtime cannot load must
// not stop the notification; the message says which zone it is printing.
func TestNotifierUnknownTimezoneFallsBackToUTC(t *testing.T) {
	got := formatHandoffMessage(kindHandoff, "Backend", "Mars/Olympus", observation{
		GridSlotStart:   dutyBase,
		AssignmentStart: dutyBase,
		AssignmentEnd:   dutyBase.Add(24 * time.Hour),
	})
	if !strings.Contains(got, fmt.Sprintf("Mon May 4, 11:00 (%s)", "Mars/Olympus")) {
		t.Fatalf("message did not fall back to UTC times:\n%s", got)
	}
}
