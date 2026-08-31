//go:build integration

package handoff

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
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
	notifier *Notifier
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

	// Slack IDs live in external_identities rather than on the user row; the
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
	env.notifier = NewNotifier(s, env.renderer, staticDmProviders("slack"), time.Minute)
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
	if !e.notifier.Tick(context.Background()) {
		e.t.Fatal("tick reported a call failure")
	}
}

// warmUp runs the silent first pass.
func (e *handoffEnv) warmUp() {
	e.t.Helper()
	e.tick(0)
	if got := e.announcements(); len(got) != 0 {
		e.t.Fatalf("warm-up admitted %d announcements, want none", len(got))
	}
}

type announcement struct {
	id      string
	key     string
	outcome string
}

// announcements lists the shift-change claims in the database, oldest first.
func (e *handoffEnv) announcements() []announcement {
	e.t.Helper()
	rows, err := e.s.GetDB().Query(`
		SELECT id, batch_key, admission_outcome FROM outbound_batches
		WHERE key_kind = 'handoff' ORDER BY admitted_at, id`)
	if err != nil {
		e.t.Fatalf("query the claims: %v", err)
	}
	defer rows.Close()

	var out []announcement
	for rows.Next() {
		var a announcement
		if err := rows.Scan(&a.id, &a.key, &a.outcome); err != nil {
			e.t.Fatalf("scan a claim: %v", err)
		}
		out = append(out, a)
	}
	return out
}

// recipients lists who one announcement promises to reach, by provider.
//
// The value is the internal user id, not the account it will be delivered to:
// the commitment names the person and the channel resolves the address when it
// prepares the attempt.
func (e *handoffEnv) recipients(batchID string) map[string]string {
	e.t.Helper()
	rows, err := e.s.GetDB().Query(`
		SELECT provider, target_kind, target_ref FROM outbound_intents
		WHERE batch_id = $1 ORDER BY idempotency_key`, batchID)
	if err != nil {
		e.t.Fatalf("query the commitments: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var provider, targetKind, targetRef string
		if err := rows.Scan(&provider, &targetKind, &targetRef); err != nil {
			e.t.Fatalf("scan a commitment: %v", err)
		}
		if targetKind != "user" {
			e.t.Errorf("a shift is taken by a person, and this one names a %q", targetKind)
		}
		if _, dup := out[provider]; dup {
			e.t.Errorf("two commitments for provider %q", provider)
		}
		out[provider] = targetRef
	}
	return out
}

// TestNotifierOverPostgresOnRevisionModel is the end-to-end shape of the tick: the
// notifier reads a schedule created through the revision model, and the group
// coming on duty gets one step per dm-capable identity.
func TestNotifierOverPostgresOnRevisionModel(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B", "U_C"),
	))
	env.warmUp()

	env.tick(24 * time.Hour)

	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d announcements after one handoff, want 1: %+v", len(made), made)
	}

	// Both members of the incoming group are promised to, and nobody else. Read
	// off the commitments directly rather than through recipients(), which keys
	// by provider and is for the multi-provider fan-out.
	rows, err := env.s.GetDB().Query(`
		SELECT target_ref, payload->>'schedule_id' FROM outbound_intents WHERE batch_id = $1`,
		made[0].id)
	if err != nil {
		t.Fatalf("query the commitments: %v", err)
	}
	defer rows.Close()
	targets := map[string]bool{}
	for rows.Next() {
		var userID, scheduleID string
		if err := rows.Scan(&userID, &scheduleID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		targets[userID] = true
		if scheduleID != env.schedID {
			t.Errorf("the announcement is about schedule %q, want %q", scheduleID, env.schedID)
		}
	}
	if !targets["U_B"] || !targets["U_C"] || targets["U_A"] {
		t.Errorf("notified %v, want the incoming group only", targets)
	}
}

// TestNotifierOverPostgresRepeatedCompositionIsNotDeduped is the reason the dedup key
// carries the moment of activation. A rotation returns to a group it has served
// before; if the key were the composition alone, the second - legitimate -
// notification would be swallowed by the unique index while the first job was
// still pending.
func TestNotifierOverPostgresRepeatedCompositionIsNotDeduped(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))
	env.warmUp()

	// A -> B -> A -> B over three days. The claim over an occurrence is held
	// forever, so all three are live for all of them.
	for i := 0; i < 3; i++ {
		env.tick(24 * time.Hour)
	}

	made := env.announcements()
	if len(made) != 3 {
		t.Fatalf("%d announcements over three handoffs, want 3: %+v", len(made), made)
	}
	seen := map[string]bool{}
	for _, a := range made {
		if a.outcome != "admitted" {
			t.Errorf("announcement %s was admitted as %q, want admitted", a.id, a.outcome)
		}
		if seen[a.key] {
			t.Errorf("two announcements claimed %q", a.key)
		}
		seen[a.key] = true
	}
	// The first and third handoffs put the same group on duty, so their claims
	// differ only in the moment of activation.
	if len(seen) != 3 {
		t.Fatalf("claims = %v, want three distinct ones", seen)
	}
}

// TestAScheduleChangedAfterAnAnnouncementOwesBoth.
//
// There is no such thing as withdrawing an announcement. The schedule moving
// again does not make the first one wrong - somebody DID come on duty, and the
// message about it is still owed - and the second change is a different event
// about different people, so it gets a claim of its own.
//
// The alternative would be a producer deciding that a promise it made a minute
// ago is no longer interesting, which is the one thing a commitment exists to
// prevent.
func TestAScheduleChangedAfterAnAnnouncementOwesBoth(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.warmUp()

	env.now = env.now.Add(time.Hour)
	env.save(dailyConfig(group(handoffGroupA, "U_A", "U_B")))
	env.tick(time.Minute)

	first := env.announcements()
	if len(first) != 1 {
		t.Fatalf("%d announcements after the first edit, want 1: %+v", len(first), first)
	}

	// The schedule moves again before anything was delivered.
	env.now = env.now.Add(time.Hour)
	env.save(dailyConfig(group(handoffGroupA, "U_A", "U_B", "U_C")))
	env.tick(time.Minute)

	made := env.announcements()
	if len(made) != 2 {
		t.Fatalf("%d announcements after the second edit, want 2: %+v", len(made), made)
	}
	if made[0].key == made[1].key {
		t.Fatalf("both edits claimed %q; the second is a different event", made[0].key)
	}

	// The first is still owed, and still owed to the person it was about.
	var owed int
	if err := env.s.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_intents
		WHERE batch_id = $1 AND status = 'pending'`, made[0].id).Scan(&owed); err != nil {
		t.Fatalf("read the first announcement: %v", err)
	}
	if owed != 1 {
		t.Fatalf("the first announcement owes %d messages; nothing withdrew it", owed)
	}
	if got := env.recipients(made[0].id)["slack"]; got != "U_B" {
		t.Errorf("the first announcement is about %q, want U_B", got)
	}
	if got := env.recipients(made[1].id)["slack"]; got != "U_C" {
		t.Errorf("the second announcement is about %q, want U_C", got)
	}
}

// TestNotifierOverPostgresEditedGroupIsNotDeduped is the same argument for an edit:
// [B] -> [B,D] -> [B] -> [B,D] returns to a composition it has already had, and
// the second addition must still be announced.
func TestNotifierOverPostgresEditedGroupIsNotDeduped(t *testing.T) {
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

	made := env.announcements()
	// Two additions produce two notifications; the removal produces none.
	if len(made) != 2 {
		t.Fatalf("%d announcements, want one per addition: %+v", len(made), made)
	}
	if made[0].key == made[1].key {
		t.Fatalf("both additions claimed %q", made[0].key)
	}
	for _, a := range made {
		if got := env.recipients(a.id)["slack"]; got != "U_B" {
			t.Errorf("announcement %s notified %q, want the added member alone", a.id, got)
		}
	}
}

// TestNotifierOverPostgresTwoInstancesCreateOneJob: two processes observe the same
// transition, and the dedup key is what makes them agree. This is why the cache
// may live in memory per instance.
func TestNotifierOverPostgresTwoInstancesCreateOneJob(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))

	second := NewNotifier(env.s, env.renderer, staticDmProviders("slack"), time.Minute)

	// Both instances warm up on the same state.
	env.warmUp()
	if !second.Tick(context.Background()) {
		t.Fatal("second instance failed to warm up")
	}

	// Both see the same handoff.
	env.now = env.now.Add(24 * time.Hour)
	if !env.notifier.Tick(context.Background()) {
		t.Fatal("first instance tick failed")
	}
	if !second.Tick(context.Background()) {
		t.Fatal("second instance tick failed")
	}

	if made := env.announcements(); len(made) != 1 {
		t.Fatalf("%d announcements from two instances, want 1: %+v", len(made), made)
	}
}

// TestTwoInstancesRaceOnOneOccurrence.
//
// The test above ticks them one after the other, which is the easy half: the
// loser reads a row that is already committed. This is the other half - both
// inside the door at once, on a real database, which is the only place the
// answer is decided.
//
// What has to hold is one claim and one set of commitments. The loser is told
// the work exists, and told it against the SAME claim, because a second claim
// over one occurrence is a second announcement to the same person about the
// same shift.
//
// It also carries a person nobody can reach, for the counter: both instances
// see the same skipped recipient and both commit, so a count taken after every
// successful admission would report one person missed as two.
func TestTwoInstancesRaceOnOneOccurrence(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		// U_NOSLACK is on the incoming shift and has no linked account: one
		// commitment is promised and one person is skipped.
		group(handoffGroupB, "U_B", "U_NOSLACK"),
	))

	both := recording(env.s)
	env.notifier = NewNotifier(both, env.renderer, staticDmProviders("slack"), time.Minute)
	second := NewNotifier(both, env.renderer, staticDmProviders("slack"), time.Minute)
	env.warmUp()
	if !second.Tick(context.Background()) {
		t.Fatal("second instance failed to warm up")
	}

	skipped := func() float64 {
		t.Helper()
		counter, err := metrics.HandoffRecipientsSkippedTotal.GetMetricWithLabelValues("no_identity")
		if err != nil {
			t.Fatalf("read the counter: %v", err)
		}
		var m dto.Metric
		if err := counter.Write(&m); err != nil {
			t.Fatalf("read the counter: %v", err)
		}
		return m.GetCounter().GetValue()
	}
	before := skipped()

	// Both instances meet the same handover at the same moment, and the moment
	// is held open by the database rather than by the Go scheduler: the door
	// closes in front of the insert, so neither transaction can commit a claim
	// while the other is still deciding there is none.
	//
	// Installed after the warm-up, which admits nothing and would otherwise
	// stand at the door forever.
	door := holdEveryClaimAtTheInsert(t, env.s.GetDB())

	env.now = env.now.Add(24 * time.Hour)
	var wg sync.WaitGroup
	complete := make([]bool, 2)
	for i, notifier := range []*Notifier{env.notifier, second} {
		wg.Add(1)
		go func(i int, n *Notifier) {
			defer wg.Done()
			complete[i] = n.Tick(context.Background())
		}(i, notifier)
	}

	// Both of them there, each having read the batch key and found nothing.
	// This is the state the unique index exists for, and reaching it is the
	// whole difference between this test and a repeat.
	door.waitFor(2)
	door.open()

	wg.Wait()

	for i, ok := range complete {
		if !ok {
			t.Fatalf("instance %d did not complete its tick", i)
		}
	}

	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d claims over one occurrence, want 1: %+v", len(made), made)
	}
	if got := env.recipients(made[0].id); len(got) != 1 || got["slack"] != "U_B" {
		t.Fatalf("the claim promises %v, want one message to U_B", got)
	}

	// One claim in the table is only half of it. What each instance was TOLD
	// decides what it does next: the winner created the work, and the loser has
	// to be told the work exists - under the SAME claim, because a second claim
	// over one occurrence is a second message to the same person about the same
	// shift. A loser told anything else either announces again or leaves the
	// occurrence to be offered forever.
	offered := both.occurrences()
	if len(offered) != 1 {
		t.Fatalf("the instances offered %d occurrences, want 1: %v", len(offered), offered)
	}
	answers := both.answers(offered[0])
	if len(answers) != 2 {
		t.Fatalf("%d instances were answered about %s, want 2", len(answers), offered[0])
	}
	outcomes := map[outbound.SubmitOutcome]int{}
	for _, answer := range answers {
		outcomes[answer.Outcome]++
	}
	if outcomes[outbound.SubmitCreated] != 1 || outcomes[outbound.SubmitExisting] != 1 {
		t.Fatalf("the two instances were told %v, want one created and one existing",
			outcomes)
	}
	if answers[0].BatchID != answers[1].BatchID {
		t.Errorf("the instances were pointed at claims %q and %q",
			answers[0].BatchID, answers[1].BatchID)
	}
	if answers[0].BatchID != made[0].id {
		t.Errorf("they were pointed at %q and the claim in the table is %q",
			answers[0].BatchID, made[0].id)
	}

	// One person missed, counted once. Counted after every commit instead, the
	// loser would have added a second.
	if got := skipped() - before; got != 1 {
		t.Errorf("the skipped person was counted %v times", got)
	}
}

// TestNotifierOverPostgresSecondInstanceAfterTheJobFinished is bug 13.
//
// The test above ticks the two instances back to back, while the first job is
// still pending - which is the window the old rule covered, and the reason the
// bug survived a test suite. The instances of a real deployment tick on
// unrelated phases, a minute apart, and the job they race over lives for a
// second or two. By the time the second instance looks, the first job is done:
// under a while_active rule its claim is gone, the identity is free again, and
// whoever came on call is told twice.
//
// What makes the second DM wrong is not that it is a duplicate message but that
// the occurrence it announces already happened. That is the statement the
// forever policy makes, and this is where it is tested.
func TestNotifierOverPostgresSecondInstanceAfterTheJobFinished(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))

	second := NewNotifier(env.s, env.renderer, staticDmProviders("slack"), time.Minute)

	env.warmUp()
	if !second.Tick(context.Background()) {
		t.Fatal("second instance failed to warm up")
	}

	// The first instance detects the handover and its announcement is delivered
	// to the end.
	env.now = env.now.Add(24 * time.Hour)
	if !env.notifier.Tick(context.Background()) {
		t.Fatal("first instance tick failed")
	}
	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d announcements from the first instance, want 1: %+v", len(made), made)
	}
	if _, err := env.s.GetDB().Exec(
		`UPDATE outbound_intents SET status = 'succeeded' WHERE batch_id = $1`,
		made[0].id); err != nil {
		t.Fatalf("deliver the announcement: %v", err)
	}

	// Only now does the other instance tick. The DM has been delivered; this
	// instance has no way of knowing that except through the claim in the
	// table.
	if !second.Tick(context.Background()) {
		t.Fatal("second instance tick failed")
	}

	after := env.announcements()
	if len(after) != 1 {
		t.Fatalf("%d announcements after the second instance ticked, want 1 - the occurrence was announced already: %+v",
			len(after), after)
	}
	if after[0].id != made[0].id {
		t.Errorf("claim %s replaced by %s", made[0].id, after[0].id)
	}
}

// TestNotifierOverPostgresDeleteAndRecreate: the delete records an empty composition,
// so recreating with the same group is a transition again. Without the deleted
// schedule reaching the notifier this cycle would pass in silence.
func TestNotifierOverPostgresDeleteAndRecreate(t *testing.T) {
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
	if made := env.announcements(); len(made) != 0 {
		t.Fatalf("the delete admitted %d announcements, want none: %+v", len(made), made)
	}

	env.now = env.now.Add(time.Hour)
	env.save(dailyConfig(group(handoffGroupA, "U_A")))
	env.tick(time.Minute)

	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d announcements after the recreate, want 1: %+v", len(made), made)
	}
	if got := env.recipients(made[0].id)["slack"]; got != "U_A" {
		t.Errorf("notified %q, want U_A - her duty was interrupted", got)
	}
}

// TestNotifierOverPostgresOverrideBoundaries: a stand-in is told the same way anyone
// coming on call is, and the group gets the same message when duty comes back.
func TestNotifierOverPostgresOverrideBoundaries(t *testing.T) {
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
	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d announcements when the override started, want 1: %+v", len(made), made)
	}
	if got := env.recipients(made[0].id)["slack"]; got != "U_C" {
		t.Errorf("notified %q, want the stand-in U_C", got)
	}

	// After it: the rotation is back.
	env.tick(2 * time.Hour)
	made = env.announcements()
	if len(made) != 2 {
		t.Fatalf("%d announcements after the override ended, want 2: %+v", len(made), made)
	}
	if got := env.recipients(made[1].id)["slack"]; got != "U_A" {
		t.Errorf("notified %q, want the returning group U_A", got)
	}
}

// TestNotifierOverPostgresPartialIdentities: a user with no dm-capable identity is
// skipped individually, without blocking the rest of the group.
func TestNotifierOverPostgresPartialIdentities(t *testing.T) {
	env := setupHandoffEnv(t)
	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B", "U_NOSLACK"),
	))
	env.warmUp()

	env.tick(24 * time.Hour)

	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d announcements, want 1: %+v", len(made), made)
	}
	var promised int
	if err := env.s.GetDB().QueryRow(`SELECT COUNT(*) FROM outbound_intents WHERE batch_id = $1`,
		made[0].id).Scan(&promised); err != nil {
		t.Fatalf("count the commitments: %v", err)
	}
	if promised != 1 {
		t.Fatalf("%d commitments, want 1 - the user with no identity is skipped", promised)
	}
	if got := env.recipients(made[0].id)["slack"]; got != "U_B" {
		t.Errorf("notified %q, want U_B", got)
	}
}

// TestNotifierOverPostgresMultiProviderFanOut proves the fan-out is
// capability-driven, not Slack-specific: one on-call user with identities on
// two dm-capable providers is promised a message through each, and an identity
// on a provider that is not dm-capable is excluded.
//
// The second provider is a real one. A made-up name would be refused at
// admission by the rule that a commitment may only name a channel this build
// can deliver through - which is the rule working, and would leave this test
// asserting nothing about fan-out.
func TestNotifierOverPostgresMultiProviderFanOut(t *testing.T) {
	env := setupHandoffEnv(t)
	env.notifier = NewNotifier(env.s, env.renderer,
		staticDmProviders("slack", "telegram"), time.Minute)

	testutil.BindIdentity(t, env.s, "U_B", "telegram", "T_B")
	testutil.BindIdentity(t, env.s, "U_B", "email", "E_B")

	env.save(dailyConfig(
		group(handoffGroupA, "U_A"),
		group(handoffGroupB, "U_B"),
	))
	env.warmUp()

	env.tick(24 * time.Hour)

	made := env.announcements()
	if len(made) != 1 {
		t.Fatalf("%d announcements, want 1: %+v", len(made), made)
	}
	got := env.recipients(made[0].id)
	if len(got) != 2 {
		t.Fatalf("fan-out = %v, want one commitment each through slack and telegram", got)
	}
	// One person, two providers, and the SAME person named in both: which
	// account each provider delivers to is that provider's to resolve, and
	// resolving it here would freeze an account that may be relinked before the
	// message goes out.
	if got["slack"] != "U_B" || got["telegram"] != "U_B" {
		t.Errorf("fan-out = %v, want U_B through both", got)
	}
	if target, ok := got["email"]; ok {
		t.Errorf("a provider that is not dm-capable was promised a message to %q", target)
	}
}

// TestNotifierOverPostgresThreeGroupsNoRepeatWithinOneShift: consecutive ticks inside
// one shift observe the same composition and must stay silent - the notifier is
// polling, and a tick is not an event.
func TestNotifierOverPostgresThreeGroupsNoRepeatWithinOneShift(t *testing.T) {
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
	if made := env.announcements(); len(made) != 0 {
		t.Fatalf("%d announcements from ticks inside one shift, want none: %+v", len(made), made)
	}
}

// TestNotifierOverPostgresEditedOverrideIsNotDeduped: the same person coming back
// onto duty must page again, even while the first job is still pending.
//
// Editing an override that is IN FORCE splits it - the served part is closed
// where it stops and the change becomes a new override starting now - so U_C's
// two arrivals differ by override id and by valid_from as well as by revision.
// They used to differ by revision alone, which is what made this case worth an
// end-to-end test; it is worth keeping because the dedup key still has to tell
// them apart, and the assertion is unchanged either way.
func TestNotifierOverPostgresEditedOverrideIsNotDeduped(t *testing.T) {
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
	// Each edit lands on whatever the previous one produced: an in-force edit
	// returns the NEW override, so both the id and the revision move.
	overrideID, revision := override.OverrideID, override.Revision
	for i, user := range []string{"U_C", "U_B", "U_C"} {
		env.now = env.now.Add(time.Hour)
		updated, err := env.config.UpdateOverride(context.Background(), env.schedID, overrideID,
			revision, scheduleconfig.OverrideCommand{
				UserID:    user,
				ValidFrom: from,
				ValidTo:   until,
				ActorID:   "U_A",
			})
		if err != nil {
			t.Fatalf("UpdateOverride %d: %v", i, err)
		}
		overrideID, revision = updated.OverrideID, updated.Revision
		env.tick(time.Minute)
	}

	made := env.announcements()
	// One per activation: U_B, then U_C, U_B, U_C.
	if len(made) != 4 {
		t.Fatalf("%d announcements over four activations, want 4: %+v", len(made), made)
	}
	if made[1].key == made[3].key {
		t.Fatalf("both activations of U_C claimed %q; the second was suppressed", made[1].key)
	}
	if got := env.recipients(made[3].id)["slack"]; got != "U_C" {
		t.Errorf("the last announcement notified %q, want U_C back on the override", got)
	}
}

// recorder keeps what each instance was told, and is otherwise the store.
//
// It does no synchronising: two goroutines started together are not a race -
// one tick can finish entirely before the other begins - and a barrier in Go
// only lines up the CALLS. What has to be lined up is what the two transactions
// are doing inside PostgreSQL, and only PostgreSQL can hold them there.
type recorder struct {
	inner notifierStore

	mu   sync.Mutex
	told map[string][]outbound.SubmitResult
}

func recording(inner notifierStore) *recorder {
	return &recorder{inner: inner, told: map[string][]outbound.SubmitResult{}}
}

func (r *recorder) GetAllTeams() ([]*model.Team, error) { return r.inner.GetAllTeams() }

func (r *recorder) GetIdentitiesForUsers(ids []string) (map[string][]*model.ExternalIdentity, error) {
	return r.inner.GetIdentitiesForUsers(ids)
}

func (r *recorder) SubmitBatch(ctx context.Context,
	batch outbound.Batch) (outbound.SubmitResult, error) {

	result, err := r.inner.SubmitBatch(ctx, batch)
	if err == nil {
		r.mu.Lock()
		r.told[batch.Admission.BatchKey] = append(r.told[batch.Admission.BatchKey], result)
		r.mu.Unlock()
	}
	return result, err
}

// answers is what the instances were told about one occurrence.
func (r *recorder) answers(key string) []outbound.SubmitResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]outbound.SubmitResult(nil), r.told[key]...)
}

// occurrences is every occurrence that was offered at all, sorted.
func (r *recorder) occurrences() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	keys := make([]string, 0, len(r.told))
	for key := range r.told {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// claimBarrier is the door, and it is inside the database.
//
// A trigger makes every transaction inserting a claim wait for one advisory
// lock, and the test holds that lock. What arrives at the door is therefore a
// transaction that has already read the batch key and found nothing - that read
// is the statement before the insert - so counting the sessions waiting there
// counts the instances that are about to compete for the same key. Released
// together, they are arbitrated by the unique index and by nothing else.
//
// The alternative, letting the two goroutines run and hoping, tests a
// sequential repeat most of the time: the second transaction commits before the
// first one has begun, and the first then reads a claim that is already there.
const claimBarrierKey = 7710041

type claimBarrier struct {
	t      *testing.T
	db     *sql.DB
	holder *sql.Conn
}

func holdEveryClaimAtTheInsert(t *testing.T, db *sql.DB) *claimBarrier {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION tokay_test_hold_claim() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(`+fmt.Sprint(claimBarrierKey)+`);
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER tokay_test_hold_claim BEFORE INSERT ON outbound_batches
			FOR EACH ROW EXECUTE FUNCTION tokay_test_hold_claim()`); err != nil {
		t.Fatalf("install the door: %v", err)
	}

	// The lock is taken on a connection of its own and held for the session:
	// released with the statement, it would be gone before anybody arrived.
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("take a connection for the door: %v", err)
	}
	if _, err := holder.ExecContext(ctx,
		`SELECT pg_advisory_lock($1)`, claimBarrierKey); err != nil {
		t.Fatalf("close the door: %v", err)
	}

	b := &claimBarrier{t: t, db: db, holder: holder}
	t.Cleanup(func() {
		b.open()
		holder.Close()
		if _, err := db.ExecContext(ctx, `
			DROP TRIGGER IF EXISTS tokay_test_hold_claim ON outbound_batches;
			DROP FUNCTION IF EXISTS tokay_test_hold_claim()`); err != nil {
			t.Errorf("take the door away: %v", err)
		}
	})
	return b
}

// waitFor blocks until n transactions are held at the insert, and fails if they
// never are: fewer than n means the instances never met, and a test that went
// on regardless would be the sequential repeat wearing a race's name.
func (b *claimBarrier) waitFor(n int) {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := b.db.QueryRow(`
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			  AND classid = 0 AND objid = $1 AND objsubid = 1`,
			claimBarrierKey).Scan(&waiting); err != nil {
			b.t.Fatalf("look at the door: %v", err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%d transactions reached the claim, want %d: the instances "+
				"never competed for it", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (b *claimBarrier) open() {
	b.holder.ExecContext(context.Background(),
		`SELECT pg_advisory_unlock($1)`, claimBarrierKey)
}
