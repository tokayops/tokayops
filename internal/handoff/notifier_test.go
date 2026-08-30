package handoff

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
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

// mockNotifierStore implements the store methods Notifier still needs:
// team names, linked identities and admission. On-call state does not come from
// here any more - it comes from the projection.
//
// It does NOT embed store.StoreInterface. While it did, the double compiled
// whether or not it implemented the methods under test, and every unimplemented
// call was a nil-pointer panic waiting rather than a compile error. Now the
// three methods below are the whole contract, and dropping one stops the build.
type mockNotifierStore struct {
	teams    []*model.Team
	slackIDs map[string]string // userID -> slack external id ("" means not linked)
	admitted []outbound.Batch

	getTeamsErr   error
	identitiesErr error
	submitErr     error
	identityCalls int

	// held are occurrences another instance already claimed, and the answer the
	// store gives about each: the same work comes back as existing, different
	// work under the same key as a conflict.
	held map[string]outbound.SubmitOutcome
}

func (m *mockNotifierStore) GetAllTeams() ([]*model.Team, error) {
	if m.getTeamsErr != nil {
		return nil, m.getTeamsErr
	}
	return m.teams, nil
}

// GetIdentitiesForUsers reads the per-test slackIDs map. A user with a missing
// entry has no identity at all; one with an empty entry has a Slack link with
// no address, which is what an unfinished link looks like.
func (m *mockNotifierStore) GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error) {
	m.identityCalls++
	if m.identitiesErr != nil {
		return nil, m.identitiesErr
	}
	out := make(map[string][]*model.ExternalIdentity)
	for _, id := range userIDs {
		slackID, ok := m.slackIDs[id]
		if !ok {
			continue
		}
		out[id] = []*model.ExternalIdentity{{UserID: id, Provider: "slack", ExternalID: slackID}}
	}
	return out, nil
}

func (m *mockNotifierStore) SubmitBatch(_ context.Context, batch outbound.Batch) (outbound.SubmitResult, error) {
	if m.submitErr != nil {
		return outbound.SubmitResult{}, m.submitErr
	}
	if outcome, taken := m.held[batch.Admission.BatchKey]; taken {
		return outbound.SubmitResult{Outcome: outcome, BatchID: "held"}, nil
	}
	m.admitted = append(m.admitted, batch)
	ids := make([]string, len(batch.Admission.Commitments))
	for i := range ids {
		ids[i] = fmt.Sprintf("intent-%d", i)
	}
	return outbound.SubmitResult{
		Outcome: outbound.SubmitCreated, BatchID: batch.Admission.BatchKey, IntentIDs: ids,
	}, nil
}

func (m *mockNotifierStore) batchKeys() []string {
	var out []string
	for _, b := range m.admitted {
		out = append(out, b.Admission.BatchKey)
	}
	return out
}

// notifierEnv is a warmed-up notifier over a projection a test drives.
type notifierEnv struct {
	t        *testing.T
	store    *mockNotifierStore
	oncall   *fakeOnCall
	notifier *Notifier
}

func newNotifierEnv(t *testing.T, slackIDs map[string]string) *notifierEnv {
	t.Helper()
	st := &mockNotifierStore{
		teams:    []*model.Team{{ID: "team-1", Name: "Backend"}},
		slackIDs: slackIDs,
		held:     map[string]outbound.SubmitOutcome{},
	}
	oncall := &fakeOnCall{}
	return &notifierEnv{
		t:        t,
		store:    st,
		oncall:   oncall,
		notifier: NewNotifier(st, oncall, staticDmProviders("slack"), time.Minute),
	}
}

// tick observes one projection state. The first tick of a test is the warm-up
// pass unless warmUp() already ran.
func (e *notifierEnv) tick(schedules ...schedulerender.ScheduleOnCall) bool {
	e.t.Helper()
	e.oncall.set(schedules...)
	return e.notifier.Tick(context.Background())
}

// warmUp runs the silent first pass and asserts it admitted nothing.
func (e *notifierEnv) warmUp(schedules ...schedulerender.ScheduleOnCall) {
	e.t.Helper()
	if !e.tick(schedules...) {
		e.t.Fatal("warm-up tick reported a call failure")
	}
	if len(e.store.admitted) != 0 {
		e.t.Fatalf("warm-up admitted %d announcements, want none", len(e.store.admitted))
	}
}

func (e *notifierEnv) announcements() []outbound.Batch { return e.store.admitted }

// targets lists who the one announcement the test expects is addressed to.
//
// Internal user ids, not provider addresses: a commitment names the PERSON, and
// the channel resolves how to reach them when it prepares the attempt. An
// identity relinked between the promise and the delivery then reaches the
// person rather than the account they used to have.
func (e *notifierEnv) targets() []string {
	e.t.Helper()
	if len(e.store.admitted) != 1 {
		e.t.Fatalf("expected exactly 1 announcement, got %d", len(e.store.admitted))
	}
	var out []string
	for _, c := range e.store.admitted[0].Admission.Commitments {
		out = append(out, c.Target.Ref)
	}
	sort.Strings(out)
	return out
}

// payload is what the one announcement tells its first recipient. It is the
// whole message as far as this side is concerned: what a channel writes is
// drawn from these fields, in the channel.
func (e *notifierEnv) payload() keys.HandoffPayloadV1 {
	e.t.Helper()
	if len(e.store.admitted) != 1 || len(e.store.admitted[0].Admission.Commitments) == 0 {
		e.t.Fatalf("expected one announcement with recipients, got %d", len(e.store.admitted))
	}
	payload, ok := e.store.admitted[0].Admission.Commitments[0].Payload.(keys.HandoffPayloadV1)
	if !ok {
		e.t.Fatalf("the commitment carries a %T, not an announcement",
			e.store.admitted[0].Admission.Commitments[0].Payload)
	}
	return payload
}

func (e *notifierEnv) cached(scheduleID string) *composition {
	return e.notifier.cached(scheduleID)
}

// occKey is the claim over one occurrence, as the grammar spells it.
func occKey(t *testing.T, kind, scheduleID string, next observation) string {
	t.Helper()
	announced, err := announcementKind(kind)
	if err != nil {
		t.Fatalf("kind: %v", err)
	}
	key, err := announcementOccurrence(announced, scheduleID, next).Key()
	if err != nil {
		t.Fatalf("occurrence key: %v", err)
	}
	return key
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

	if got := env.targets(); strings.Join(got, ",") != "carol" {
		t.Fatalf("notified %v, want carol alone - bob was already on call", got)
	}
	// The claim is over the occurrence, so it is compared against the
	// occurrence rather than read: the key is a digest, and asking what it
	// starts with would only pin the schedule it names.
	want := occKey(t, kindHandoff, "sched-1", observe(rotationDuty("sched-1", "g-b", "bob", "carol")))
	if got := env.announcements()[0].Admission.BatchKey; got != want {
		t.Fatalf("claimed %q, want the handoff occurrence of sched-1 (%s)", got, want)
	}
}

// TestNotifierAddedToActiveShift: same group, new member. It is an edit, not a
// rotation, and it says so.
func TestNotifierAddedToActiveShift(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("bob", "dave"))
	env.warmUp(rotationDuty("sched-1", "g-b", "bob"))

	env.tick(rotationDuty("sched-1", "g-b", "bob", "dave"))

	if got := env.targets(); strings.Join(got, ",") != "dave" {
		t.Fatalf("notified %v, want dave alone", got)
	}
	want := occKey(t, kindAddedToActiveShift, "sched-1",
		observe(rotationDuty("sched-1", "g-b", "bob", "dave")))
	if got := env.announcements()[0].Admission.BatchKey; got != want {
		t.Fatalf("claimed %q, want the added_to_active_shift occurrence (%s)", got, want)
	}
	// Which event this is travels in the payload, as a kind. Nothing here
	// writes a sentence about it: what a person reads is composed by the
	// channel that sends it.
	if kind := env.payload().Kind; kind != keys.HandoffAddedToActiveShift {
		t.Fatalf("the announcement is a %q, want one about joining a shift in progress", kind)
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
			if len(env.announcements()) != 0 {
				t.Fatalf("admitted %d announcements, want none", len(env.announcements()))
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
	if got := env.targets(); strings.Join(got, ",") != "carol" {
		t.Fatalf("override start notified %v, want the stand-in", got)
	}

	env.store.admitted = nil
	env.tick(rotationDuty("sched-1", "g-a", "alice"))
	if got := env.targets(); strings.Join(got, ",") != "alice" {
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
	if len(env.announcements()) != 0 {
		t.Fatalf("admitted %d announcements over three boundaries, want none", len(env.announcements()))
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

	if len(env.announcements()) != 0 {
		t.Fatalf("admitted %d announcements for a schedule seen for the first time, want none", len(env.announcements()))
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
	if len(env.announcements()) != 0 {
		t.Fatalf("the delete itself admitted %d announcements, want none", len(env.announcements()))
	}
	cached := env.cached("sched-1")
	if cached == nil || !cached.empty() {
		t.Fatalf("cache = %+v, want a recorded empty composition", cached)
	}

	env.tick(rotationDuty("sched-1", "g-a", "alice"))
	if got := env.targets(); strings.Join(got, ",") != "alice" {
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
	if !env.notifier.Tick(context.Background()) {
		t.Fatal("a damaged schedule failed the whole tick")
	}

	if got := env.targets(); strings.Join(got, ",") != "bob" {
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
	if env.notifier.Tick(context.Background()) {
		t.Fatal("a read failure reported success")
	}
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v after a read failure, want it untouched", cached)
	}

	env.oncall.fail(nil)
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "bob" {
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
	if !env.notifier.Tick(context.Background()) {
		t.Fatal("warm-up was blocked by a damaged schedule")
	}
	env.notifier.cacheMu.Lock()
	env.notifier.warmedUp = true
	env.notifier.cacheMu.Unlock()

	// The healthy schedule was seeded, so its next transition is a real one.
	env.tick(rotationDuty("sched-healthy", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "bob" {
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
	if !env.notifier.Tick(context.Background()) {
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
	if len(env.announcements()) != 0 {
		t.Fatalf("the repair admitted %d announcements, want none", len(env.announcements()))
	}
	// And the next real transition is announced.
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "bob" {
		t.Fatalf("notified %v, want the first transition after the repair", got)
	}
}

// TestNotifierWarmUpBlockedByCallFailure: nothing was read, so nothing was
// seeded, and finishing warm-up would make the next tick treat every schedule as
// a first observation of a state it never actually saw.
func TestNotifierWarmUpBlockedByCallFailure(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice"))
	env.oncall.fail(errors.New("no connection"))

	if env.notifier.Tick(context.Background()) {
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
// transition, the claim over the occurrence lets one admission create the work,
// and the metric counts what was promised rather than what was noticed.
func TestNotifierSecondInstanceCountsOneNotification(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	// The other instance got there first and holds the occurrence.
	next := observe(rotationDuty("sched-1", "g-b", "bob"))
	env.store.held[occKey(t, kindHandoff, "sched-1", next)] = outbound.SubmitExisting

	before := counterValue(t, metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kindHandoff))
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	after := counterValue(t, metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kindHandoff))

	if len(env.announcements()) != 0 {
		t.Fatalf("admitted %d announcements although the occurrence was held",
			len(env.announcements()))
	}
	if after != before {
		t.Errorf("notification counter moved by %v for work somebody else promised", after-before)
	}
	// The cache still advances: this instance has seen the transition, and
	// re-detecting it every minute would be noise, not safety.
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "bob" {
		t.Errorf("cache = %+v, want the observed composition", cached)
	}
}

// TestNotifierCountsAdmittedAnnouncements: one transition, one unit of the
// metric, however many identities the fan-out reaches.
func TestNotifierCountsAdmittedAnnouncements(t *testing.T) {
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

// TestNotifierAdmissionFailureRetriesNextTick: the cache is what makes the
// retry happen, so it must not advance past an announcement that was never
// admitted.
func TestNotifierAdmissionFailureRetriesNextTick(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	env.store.submitErr = errors.New("transient DB error")
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if len(env.announcements()) != 0 {
		t.Fatalf("admitted %d announcements despite the failure", len(env.announcements()))
	}
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v, want the old composition so the retry still sees a change", cached)
	}

	env.store.submitErr = nil
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "bob" {
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
	if got := env.targets(); strings.Join(got, ",") != "carol" {
		t.Fatalf("notified %v, want carol alone", got)
	}

	// A group nobody can reach at all. dave is new to this shift - so there IS
	// something to announce - and there is nowhere to announce it.
	//
	// That is still an answer about the occurrence, and it is admitted as one:
	// a claim with nothing promised under it. Staying silent instead would
	// leave the occurrence unclaimed, and an instance with a different view of
	// who is linked could promise something under it a minute later.
	env.store.admitted = nil
	env.tick(rotationDuty("sched-1", "g-c", "dave"))
	if len(env.announcements()) != 1 {
		t.Fatalf("admitted %d announcements for an unreachable group, want one",
			len(env.announcements()))
	}
	made := env.announcements()[0].Admission
	if len(made.Commitments) != 0 {
		t.Fatalf("promised %d messages to somebody nothing can reach", len(made.Commitments))
	}
	if made.Outcome != keys.OutcomeNoTargets {
		t.Fatalf("the announcement was admitted as %q, want no_targets", made.Outcome)
	}
	if cached := env.cached("sched-1"); cached == nil || strings.Join(cached.UserIDs, ",") != "dave" {
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

	claimed := env.store.batchKeys()
	if len(claimed) != 2 {
		t.Fatalf("admitted %d announcements, want one per kind: %v", len(claimed), claimed)
	}
	if claimed[0] == claimed[1] {
		t.Fatalf("both kinds claimed %q", claimed[0])
	}
	// Which key belongs to which kind, stated rather than inferred from the
	// spelling: the kind is inside the digest, not in front of it.
	if want := occKey(t, kindAddedToActiveShift, "sched-1", observe(added)); claimed[0] != want {
		t.Errorf("the first announcement claimed %q, want the added_to_active_shift occurrence %q",
			claimed[0], want)
	}
	if want := occKey(t, kindHandoff, "sched-1", observe(added)); claimed[1] != want {
		t.Errorf("the second announcement claimed %q, want the handoff occurrence %q",
			claimed[1], want)
	}
}

// TestTheAnnouncementCarriesBothBoundaries: the payload carries the three
// instants a reader needs and the zone to print them in, and the two events are
// told apart by a kind rather than by a sentence.
//
// The producer writes no prose at all now. What used to be asserted here - the
// first line, the labels, the formatting - is the channel's, and is asserted
// where the channel builds it. What has to be true HERE is that nothing a
// channel needs was dropped on the way: the two pairs of boundaries diverge
// exactly where it matters, the shift began at 11:00 and the stand-in's
// assignment at 14:00, and a payload carrying one of them could not say so.
func TestTheAnnouncementCarriesBothBoundaries(t *testing.T) {
	slotStart := time.Date(2026, 5, 4, 4, 0, 0, 0, time.UTC)   // 11:00 in Bangkok
	assignStart := time.Date(2026, 5, 4, 7, 0, 0, 0, time.UTC) // 14:00 in Bangkok
	assignEnd := time.Date(2026, 5, 5, 4, 0, 0, 0, time.UTC)   // 11:00 next day

	shift := dutySpec{
		scheduleID: "sched-1", timezone: "Asia/Bangkok",
		source: schedulerender.SourceRotation, groupID: "g-b", users: []string{"bob"},
		slotStart: slotStart, start: assignStart, end: assignEnd,
	}

	tests := []struct {
		name  string
		first schedulerender.ScheduleOnCall
		then  schedulerender.ScheduleOnCall
		kind  keys.HandoffKind
	}{
		{
			name:  kindHandoff,
			first: rotationDuty("sched-1", "g-a", "alice"),
			then:  duty(shift),
			kind:  keys.HandoffShiftChange,
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
			kind: keys.HandoffAddedToActiveShift,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
			env.warmUp(tc.first)
			env.tick(tc.then)

			got := env.payload()
			if got.Kind != tc.kind {
				t.Errorf("announced as %q, want %q", got.Kind, tc.kind)
			}
			if got.TeamName != "Backend" {
				t.Errorf("team name = %q, want the name and not the id", got.TeamName)
			}
			if got.Timezone != "Asia/Bangkok" {
				t.Errorf("timezone = %q, want the schedule's own", got.Timezone)
			}
			for _, moment := range []struct {
				what string
				got  time.Time
				want time.Time
			}{
				{"rotation shift start", got.GridSlotStart, slotStart},
				{"assignment start", got.AssignmentStart, assignStart},
				{"assignment end", got.AssignmentEnd, assignEnd},
			} {
				if !moment.got.Equal(moment.want) {
					t.Errorf("%s = %s, want %s", moment.what, moment.got, moment.want)
				}
			}
		})
	}
}

// TestTheAnnouncementCarriesTheSnapshotTimezone: the zone comes from the
// configuration in force, not from the schedule row - which is the whole reason
// it travels with the projection - and it travels on as a NAME.
//
// Not as an offset and not as formatted text: a channel prints these instants
// itself, and an announcement that arrives an hour late in a country that
// changed its clocks in between has to print the time that was true when it was
// read, which only a name can express.
func TestTheAnnouncementCarriesTheSnapshotTimezone(t *testing.T) {
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

	got := env.payload()
	if got.Timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want the snapshot's own", got.Timezone)
	}
	if !got.GridSlotStart.Equal(time.Date(2026, 5, 4, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("the instant was converted on the way out: %s", got.GridSlotStart)
	}
}

// TestAnAnnouncementTheGrammarRefusesLeavesTheScheduleAlone.
//
// What is on trial is the producer's answer to a refusal, not the refusal: an
// announcement that could not be built is not an announcement that was decided
// against, so the cache stays where it was and the next tick tries again.
//
// The trigger is a time zone nothing can load, which is the cheapest one to
// write down - and it is not reachable through the real projection: a schedule
// carrying one is refused when it is saved, and computing its rotation fails
// before any of this, so it arrives as a projection failure instead. Every
// other way the grammar refuses an announcement takes this same path.
func TestAnAnnouncementTheGrammarRefusesLeavesTheScheduleAlone(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(duty(dutySpec{
		scheduleID: "sched-1", timezone: "Mars/Olympus",
		source: schedulerender.SourceRotation, groupID: "g-a", users: []string{"alice"},
	}))
	env.tick(duty(dutySpec{
		scheduleID: "sched-1", timezone: "Mars/Olympus",
		source: schedulerender.SourceRotation, groupID: "g-b", users: []string{"bob"},
	}))

	if len(env.announcements()) != 0 {
		t.Fatalf("admitted %d announcements in a zone nothing can print",
			len(env.announcements()))
	}
	if cached := env.cached("sched-1"); cached == nil ||
		strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v, want the old composition so the repair is still a change", cached)
	}
}

// TestTheCacheMovesOnlyOnAnswersNothingCanImprove.
//
// The cache is the whole memory of what has been dealt with, so what moves it
// is a closed rule rather than "it worked". Three of the answers mean the
// occurrence is spoken for - by us, or by somebody else, or by somebody else
// with a different set - and repeating any of them would produce the same
// answer every minute until the shift ended. The rest leave it where it was, so
// the next tick sees the same transition and tries again.
func TestTheCacheMovesOnlyOnAnswersNothingCanImprove(t *testing.T) {
	cases := []struct {
		name    string
		outcome outbound.SubmitOutcome
		submit  error
		moves   bool
	}{
		{name: "we promised it", outcome: outbound.SubmitCreated, moves: true},
		{name: "somebody else promised it", outcome: outbound.SubmitExisting, moves: true},
		{
			// The loser of a race between two instances that built different
			// sets. Nothing here can overrule the winner and nothing can ask it
			// to try again, so re-detecting this forever would be noise.
			name: "somebody else promised something else", outcome: outbound.SubmitConflict, moves: true,
		},
		{
			// The people it was about are not the people who exist now.
			name: "a recipient was erased", outcome: outbound.SubmitRecipientErased, moves: false,
		},
		{
			// An answer about an alert group, which a shift change has none of.
			name: "an answer to somebody else's question", outcome: outbound.SubmitSourceChanged, moves: false,
		},
		{name: "nothing is known", submit: errors.New("connection reset"), moves: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
			env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

			next := observe(rotationDuty("sched-1", "g-b", "bob"))
			if tc.outcome != "" && tc.outcome != outbound.SubmitCreated {
				env.store.held[occKey(t, kindHandoff, "sched-1", next)] = tc.outcome
			}
			env.store.submitErr = tc.submit

			env.tick(rotationDuty("sched-1", "g-b", "bob"))

			cached := env.cached("sched-1")
			if cached == nil {
				t.Fatal("the schedule left the cache entirely")
			}
			moved := strings.Join(cached.UserIDs, ",") == "bob"
			if moved != tc.moves {
				t.Fatalf("cache holds %v; moved=%v, want %v", cached.UserIDs, moved, tc.moves)
			}
		})
	}
}

// TestAFailedIdentityReadLeavesTheScheduleAlone is the same boundary as the
// builder's, asserted where the consequence is: a tick that could not read who
// is linked admits nothing and remembers nothing, so the next one is still
// looking at a transition.
func TestAFailedIdentityReadLeavesTheScheduleAlone(t *testing.T) {
	env := newNotifierEnv(t, slackIDsFor("alice", "bob"))
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	env.store.identitiesErr = errors.New("connection reset")
	env.tick(rotationDuty("sched-1", "g-b", "bob"))

	if len(env.announcements()) != 0 {
		t.Fatalf("admitted %d announcements without knowing who is linked",
			len(env.announcements()))
	}
	if cached := env.cached("sched-1"); cached == nil ||
		strings.Join(cached.UserIDs, ",") != "alice" {
		t.Fatalf("cache = %+v, want the old composition so the retry still sees a change", cached)
	}

	// And the retry makes it.
	env.store.identitiesErr = nil
	env.tick(rotationDuty("sched-1", "g-b", "bob"))
	if got := env.targets(); strings.Join(got, ",") != "bob" {
		t.Fatalf("the retry notified %v, want bob", got)
	}
}

// TestSkippedRecipientsAreCountedOnceForTheOccurrence.
//
// Two instances see one shift change and both see the same unreachable person.
// Both commit - one creating the work, the other told it exists - so counting
// after every successful admission would report one person missed as two, and
// the same is true of a repeat after an answer this instance lost.
func TestSkippedRecipientsAreCountedOnceForTheOccurrence(t *testing.T) {
	skipped := func() float64 {
		return counterValue(t,
			metrics.HandoffRecipientsSkippedTotal.WithLabelValues(string(skipNoIdentity)))
	}

	// carol is linked, bob is not.
	env := newNotifierEnv(t, map[string]string{"alice": "U-ALICE", "carol": "U-CAROL"})
	env.warmUp(rotationDuty("sched-1", "g-a", "alice"))

	before := skipped()
	env.tick(rotationDuty("sched-1", "g-b", "bob", "carol"))
	if got := skipped() - before; got != 1 {
		t.Fatalf("counted %v people left out of the announcement it made, want 1", got)
	}

	// The other instance, arriving at the same occurrence second.
	loser := newNotifierEnv(t, map[string]string{"alice": "U-ALICE", "carol": "U-CAROL"})
	loser.store.held = env.store.held
	loser.warmUp(rotationDuty("sched-1", "g-a", "alice"))
	next := observe(rotationDuty("sched-1", "g-b", "bob", "carol"))
	loser.store.held[occKey(t, kindHandoff, "sched-1", next)] = outbound.SubmitExisting

	before = skipped()
	loser.tick(rotationDuty("sched-1", "g-b", "bob", "carol"))
	if got := skipped() - before; got != 0 {
		t.Fatalf("the instance that promised nothing counted %v people left out, want 0", got)
	}
}
