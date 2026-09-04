package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// What a producer is allowed to decide, and what it must refuse to decide.
//
// An admission is held forever: it is the moment an alert's one chance to page
// is spent. So the difference between "the policy is not there" and "the
// database did not answer" is not a matter of tidiness - the first is a fact
// about the escalation, and the second is a guess that would be frozen into
// every message of it and never revisited.

// routedTeam escalates by policy-1: the routing comes off the team now, so a
// fixture that means "this alert has a policy" says it here.
var routedTeam = &model.Team{ID: "team-1", DefaultPolicyID: "policy-1"}

// failingStore answers whatever the test says, including badly.
type failingStore struct {
	policy    *model.EscalationPolicy
	policyErr error
	team      *model.Team
	teamErr   error
	users     []*model.User
	usersErr  error

	// flaky: the first read of the team fails and the rest succeed, which is
	// what a database having a moment looks like.
	flaky     bool
	teamReads int

	// forgetful: the people are there the first time they are asked for and
	// gone afterwards, which is what an erasure landing mid-plan looks like.
	forgetful  bool
	usersReads int
}

func (f *failingStore) GetEscalationPolicyByID(string) (*model.EscalationPolicy, error) {
	return f.policy, f.policyErr
}

func (f *failingStore) GetUsersByIDs([]string) ([]*model.User, error) {
	f.usersReads++
	if f.forgetful && f.usersReads > 1 {
		return nil, f.usersErr
	}
	return f.users, f.usersErr
}

func (f *failingStore) GetTeamByID(string) (*model.Team, error) {
	f.teamReads++
	if f.flaky && f.teamReads == 1 {
		return nil, errors.New("connection reset")
	}
	return f.team, f.teamErr
}

func planFor(store planStore) *planner {
	return &planner{
		store:    store,
		oncall:   &fakeProjection{},
		settings: &fakeSettings{},
		cfg: &config.Config{Global: config.GlobalConfig{
			FirehoseCriticalChannel: "C_FIRE", SelfURL: "https://tokay.example",
		}},
	}
}

func criticalGroup() *model.AlertGroup {
	return &model.AlertGroup{
		ID: "ag-1", AlertKey: "dk-1", Status: model.AlertGroupStatusNew,
		TeamID: "team-1", Severity: "critical", Title: "Disk filling up",
	}
}

func TestADatabaseThatDidNotAnswerAdmitsNothing(t *testing.T) {
	cases := map[string]planStore{
		"the policy could not be read": &failingStore{
			policyErr: errors.New("connection reset"),
			team:      routedTeam,
		},
		"the team could not be read": &failingStore{
			policy:  &model.EscalationPolicy{ID: "policy-1"},
			teamErr: errors.New("connection reset"),
		},
	}

	for name, store := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
				schedulerender.TeamOnCallResult{})

			if !errors.Is(err, ErrOnCallResolutionUnavailable) {
				t.Fatalf("a database that did not answer produced %v, and the plan "+
					"would have been admitted on a guess", err)
			}
		})
	}
}

// TestWhatIsNotThereIsAnAnswer. The other half: a policy or a team that genuinely
// does not exist is a fact about this installation, and the alert still reaches
// the channel everybody watches.
func TestWhatIsNotThereIsAnAnswer(t *testing.T) {
	t.Run("no such policy", func(t *testing.T) {
		admission, err := planFor(&failingStore{
			policyErr: sql.ErrNoRows, team: routedTeam,
		}).buildPlan(context.Background(), criticalGroup(),
			schedulerender.TeamOnCallResult{})
		if err != nil {
			t.Fatalf("a missing policy stopped the escalation: %v", err)
		}
		if len(admission.Admission.Commitments) != 1 {
			t.Fatalf("expected the firehose alone, got %d commitments",
				len(admission.Admission.Commitments))
		}
		if escalationOf(t, admission).PolicyID != "" {
			t.Errorf("the group records policy %q, which does not exist", escalationOf(t, admission).PolicyID)
		}
	})

	t.Run("no such team", func(t *testing.T) {
		admission, err := planFor(&failingStore{
			policyErr: sql.ErrNoRows, teamErr: sql.ErrNoRows,
		}).buildPlan(context.Background(), criticalGroup(),
			schedulerender.TeamOnCallResult{})
		if err != nil {
			t.Fatalf("an unknown team stopped the escalation: %v", err)
		}

		// The card says so where its buttons would be. Recorded as onboarded,
		// it would offer an Acknowledge button that answers nobody.
		state := admission.Admission.Snapshot.Content()
		if state.TeamOnboarded {
			t.Error("a team TokayOps does not have was frozen as onboarded")
		}
	})
}

// TestAPolicyStepKeepsItsOwnIndex. The slot is part of a commitment's identity,
// and a policy's indices are not required to be dense: steps 5 and 9 admitted
// as 0 and 1 would be a different escalation than the one the policy describes.
func TestAPolicyStepKeepsItsOwnIndex(t *testing.T) {
	store := &failingStore{
		team: routedTeam,
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Sparse",
			Steps: []*model.EscalationStep{
				{StepIndex: 5, Provider: "slack", TargetKind: "dm",
					TargetType: "user", TargetID: "U_FIVE"},
				{StepIndex: 9, Provider: "slack", TargetKind: "channel",
					TargetType: "channel", TargetID: "C_NINE"},
			},
		},
	}

	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	slots := map[string]int{}
	for _, commitment := range admission.Admission.Commitments {
		if commitment.Slot.Kind == keys.SlotPolicy {
			slots[commitment.Target.Ref] = commitment.Slot.Index
		}
	}
	if slots["U_FIVE"] != 5 || slots["C_NINE"] != 9 {
		t.Fatalf("the commitments were admitted in slots %v, want the policy's own indices", slots)
	}
}

// TestTheAuditKeepsBothVocabularies. A commitment names an ADDRESS - a user or a
// channel - because that is what a delivery needs. The policy names the SHAPE of
// the message - a dm or a channel post - because that is what somebody
// configured. Recording one as the other makes every direct message read as a
// target kind nobody can configure.
func TestTheAuditKeepsBothVocabularies(t *testing.T) {
	store := &failingStore{
		team: routedTeam,
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Mixed",
			Steps: []*model.EscalationStep{{
				StepIndex: 0, Provider: "slack", TargetKind: "dm",
				TargetType: "user", TargetID: "U_ONE",
			}},
		},
	}

	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	var snapshot model.EscalationPolicySnapshot
	if err := json.Unmarshal(escalationOf(t, admission).PolicySnapshot, &snapshot); err != nil {
		t.Fatalf("read the policy snapshot: %v", err)
	}

	var dm *model.EscalationStepSnapshot
	for _, step := range snapshot.Steps {
		if step.TargetID == "U_ONE" {
			dm = step
		}
	}
	if dm == nil {
		t.Fatal("the direct message is missing from the audit")
	}
	if dm.TargetKind != "dm" {
		t.Errorf("the audit calls a direct message a %q", dm.TargetKind)
	}
	if dm.TargetType != string(keys.TargetUser) {
		t.Errorf("the audit says it was addressed to a %q", dm.TargetType)
	}
}

// TestASecondPromiseToTheSamePersonIsRecorded. Two steps sharing an index and a
// recipient are one commitment by identity, and the grammar refuses an admission
// holding the same key twice. Refused there it would be refused on every tick,
// forever, over a policy somebody can fix.
func TestASecondPromiseToTheSamePersonIsRecorded(t *testing.T) {
	store := &failingStore{
		team: routedTeam,
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Repeated",
			Steps: []*model.EscalationStep{
				{StepIndex: 0, Provider: "slack", TargetKind: "dm",
					TargetType: "user", TargetID: "U_ONE"},
				{StepIndex: 0, Provider: "slack", TargetKind: "dm",
					TargetType: "user", TargetID: "U_ONE"},
			},
		},
	}

	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("a policy that repeats itself stopped the escalation: %v", err)
	}

	promises := 0
	for _, commitment := range admission.Admission.Commitments {
		if commitment.Target.Ref == "U_ONE" {
			promises++
		}
	}
	if promises != 1 {
		t.Fatalf("the same person was promised %d times", promises)
	}
	if len(escalationOf(t, admission).Unpromised) != 1 ||
		escalationOf(t, admission).Unpromised[0].Reason != outbound.ReasonDuplicate {
		t.Fatalf("the repeat was recorded as %v", escalationOf(t, admission).Unpromised)
	}
}

// What a plan is built FROM, and the version it was built at.
//
// The snapshot frozen here is what every message of this escalation renders
// from, for as long as the escalation lives. That makes two things load
// bearing: it has to be a picture of one moment rather than a collage of
// several reads, and it has to say which moment, so the admission can refuse a
// plan built a moment too early.

// TestThePlanCarriesTheVersionItWasReadAt: without it the admission has nothing
// to compare under its lock, and a plan built from state that changed in the
// meantime would be held forever.
func TestThePlanCarriesTheVersionItWasReadAt(t *testing.T) {
	ag := criticalGroup()
	ag.RenderSourceVersion = 7

	admission, err := planFor(&failingStore{
		policyErr: sql.ErrNoRows, team: &model.Team{ID: "team-1"},
	}).buildPlan(context.Background(), ag, schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	if escalationOf(t, admission).SourceVersion != 7 {
		t.Fatalf("the plan says it was built from version %d, the group is at 7",
			escalationOf(t, admission).SourceVersion)
	}
}

// TestTheHistoryDoesNotReachTheFrozenState. The alert's history left the
// snapshot with tag 14 on 2026-08-25, and this is the assertion that keeps it
// out: two groups that differ ONLY in what has happened to them freeze to the
// same content identity.
//
// It is not tidiness. The digest answers "did the desired state change", and a
// field that reaches it without reaching the message makes that answer wrong -
// every line of history, including the ones a delivery writes about itself,
// would raise a revision and send a real edit nobody can see.
func TestTheHistoryDoesNotReachTheFrozenState(t *testing.T) {
	freeze := func(t *testing.T, events []*model.TimelineEvent) []byte {
		t.Helper()
		ag := criticalGroup()
		ag.TimelineEvents = events
		admission, err := planFor(&failingStore{
			policyErr: sql.ErrNoRows, team: &model.Team{ID: "team-1"},
		}).buildPlan(context.Background(), ag, schedulerender.TeamOnCallResult{})
		if err != nil {
			t.Fatalf("build the plan: %v", err)
		}
		if len(admission.Admission.Commitments) == 0 {
			t.Fatal("nothing was promised")
		}
		return admission.Admission.Snapshot.Digest()
	}

	bare := freeze(t, nil)

	busy := freeze(t, []*model.TimelineEvent{
		{
			ID: "ev-2", AlertGroupID: "ag-1", Type: model.TimelineEventAlertAdded,
			Message: "A second alert joined", CreatedAt: time.Unix(1700000200, 0),
		},
		{
			ID: "ev-1", AlertGroupID: "ag-1", Type: model.TimelineEventCreated,
			Message: "Alert group created", CreatedAt: time.Unix(1700000100, 0),
		},
	})
	if !bytes.Equal(bare, busy) {
		t.Fatal("the history changed the content identity of the state")
	}

	// The lines that used to need a filter of their own: written by a build
	// that knows more, or recorded without an id or a time. None of them can
	// cost the page any more, because none of them is read.
	unshowable := freeze(t, []*model.TimelineEvent{
		{
			ID: "ev-x", AlertGroupID: "ag-1", Type: model.TimelineEventType("teleported"),
			Message: "written by a build that knows more", CreatedAt: time.Unix(1700000100, 0),
		},
		{ID: "", AlertGroupID: "ag-1", Type: model.TimelineEventCreated, Message: "no id"},
	})
	if !bytes.Equal(bare, unshowable) {
		t.Fatal("a history line nobody can show still reached the state")
	}
}

// TestAFlakyTeamReadIsNotAFirehoseOnlyEscalation. The routing and "is this team
// set up here" used to be two reads of the same row. A blip that landed on the
// first one and cleared before the second produced an escalation with no policy
// at all - firehose only, held forever, over a database that was busy for a
// second. One read means the failure cannot answer half the question.
func TestAFlakyTeamReadIsNotAFirehoseOnlyEscalation(t *testing.T) {
	store := &failingStore{flaky: true, team: routedTeam,
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Critical",
			Steps: []*model.EscalationStep{{
				StepIndex: 0, Provider: "slack", TargetKind: "channel",
				TargetType: "channel", TargetID: "C_ONE",
			}},
		}}

	_, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallResult{})
	if !errors.Is(err, ErrOnCallResolutionUnavailable) {
		t.Fatalf("a team read that failed produced %v, and the alert would have been "+
			"admitted with no policy at all", err)
	}
	if store.teamReads != 1 {
		t.Errorf("the plan read the team %d times; a second read is what let a "+
			"failure answer one question and a success the other", store.teamReads)
	}
}

// TestTheTeamIsReadOnce is the same property stated on the path that works.
func TestTheTeamIsReadOnce(t *testing.T) {
	store := &failingStore{team: routedTeam,
		policy: &model.EscalationPolicy{ID: "policy-1", Name: "Critical"}}

	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}
	if store.teamReads != 1 {
		t.Fatalf("the plan read the team %d times, want 1", store.teamReads)
	}
	if escalationOf(t, admission).PolicyID != "policy-1" {
		t.Errorf("the alert escalates by %q", escalationOf(t, admission).PolicyID)
	}
	if !admission.Admission.Snapshot.Content().TeamOnboarded {
		t.Error("the card says the team is not set up here")
	}
}

// TestThePeoplePagedAreThePeopleRecorded. The commitments and the on-call
// snapshot on the group are one claim about one moment, and they used to be two
// reads of the people. Somebody erased between them - or a second answer that
// came back short - promised a message to Alice under a group that said Alice
// was not on call, which is the disagreement the whole read-once rule exists to
// prevent.
func TestThePeoplePagedAreThePeopleRecorded(t *testing.T) {
	alice := &model.User{ID: "u-alice", Name: "Alice"}
	store := &failingStore{
		team:      routedTeam,
		users:     []*model.User{alice},
		forgetful: true,
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Critical",
			Steps: []*model.EscalationStep{{
				StepIndex: 0, Provider: "slack", TargetKind: "dm",
				TargetType: "schedule", TargetID: "sched-1",
			}},
		},
	}

	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallRead(teamSchedule("sched-1", onDuty("g1", alice.ID)), nil))
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	if got := promisedUsers(admission); len(got) != 1 || got[0] != alice.ID {
		t.Fatalf("the plan pages %v", got)
	}

	var recorded model.OnCallResult
	if err := json.Unmarshal(escalationOf(t, admission).OnCallSnapshot, &recorded); err != nil {
		t.Fatalf("read the on-call snapshot: %v", err)
	}
	if len(recorded.L1Users) != 1 || recorded.L1Users[0].ID != alice.ID {
		t.Fatalf("the group records %+v as on call while the plan pages %s",
			recorded.L1Users, alice.ID)
	}

	if store.usersReads != 1 {
		t.Errorf("the plan asked who the people are %d times; a second answer is "+
			"what let the two disagree", store.usersReads)
	}
}

// TestTwoStepsOnOneScheduleAreToldApart. A policy may name the same schedule
// twice - a nudge now and a louder one in ten minutes - and when nobody is on
// it, both are recorded. Named only by their target the two lines are the same
// sentence about different work, and whoever reads the history cannot tell
// which step of the policy went unanswered.
func TestTwoStepsOnOneScheduleAreToldApart(t *testing.T) {
	store := &failingStore{
		team: routedTeam,
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Twice",
			Steps: []*model.EscalationStep{
				{StepIndex: 0, Provider: "slack", TargetKind: "dm",
					TargetType: "schedule", TargetID: "sched-1"},
				{StepIndex: 1, Provider: "slack", TargetKind: "dm",
					TargetType: "schedule", TargetID: "sched-1", DelaySeconds: 600},
			},
		},
	}

	// The team has a schedule and nobody is on it, which is an answer.
	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		schedulerender.TeamOnCallRead(teamSchedule("sched-1", schedulerender.OnCall{}), nil))
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	if len(escalationOf(t, admission).Unpromised) != 2 {
		t.Fatalf("the history records %d steps, want 2", len(escalationOf(t, admission).Unpromised))
	}
	first, second := escalationOf(t, admission).Unpromised[0], escalationOf(t, admission).Unpromised[1]
	if first.Step == second.Step {
		t.Fatalf("both lines read %q, so the two steps are indistinguishable", first.Step)
	}
	for i, step := range escalationOf(t, admission).Unpromised {
		if !strings.Contains(step.Step, fmt.Sprintf("%d", i)) {
			t.Errorf("line %d names the step as %q, without its index", i, step.Step)
		}
		if step.Reason != outbound.ReasonNobodyOnCall {
			t.Errorf("line %d blames %q", i, step.Reason)
		}
	}
}
