package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// failingStore answers whatever the test says, including badly.
type failingStore struct {
	policy    *model.EscalationPolicy
	policyErr error
	team      *model.Team
	teamErr   error
	users     []*model.User
	usersErr  error
}

func (f *failingStore) GetEscalationPolicyByID(string) (*model.EscalationPolicy, error) {
	return f.policy, f.policyErr
}

func (f *failingStore) GetUsersByIDs([]string) ([]*model.User, error) {
	return f.users, f.usersErr
}

func (f *failingStore) GetTeamByID(string) (*model.Team, error) {
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
			team:      &model.Team{ID: "team-1"},
		},
		"the team could not be read": &failingStore{
			policy:  &model.EscalationPolicy{ID: "policy-1"},
			teamErr: errors.New("connection reset"),
		},
	}

	for name, store := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
				"policy-1", schedulerender.TeamOnCallResult{})

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
			policyErr: sql.ErrNoRows, team: &model.Team{ID: "team-1"},
		}).buildPlan(context.Background(), criticalGroup(), "policy-1",
			schedulerender.TeamOnCallResult{})
		if err != nil {
			t.Fatalf("a missing policy stopped the escalation: %v", err)
		}
		if len(admission.Admission.Commitments) != 1 {
			t.Fatalf("expected the firehose alone, got %d commitments",
				len(admission.Admission.Commitments))
		}
		if admission.PolicyID != "" {
			t.Errorf("the group records policy %q, which does not exist", admission.PolicyID)
		}
	})

	t.Run("no such team", func(t *testing.T) {
		admission, err := planFor(&failingStore{
			policyErr: sql.ErrNoRows, teamErr: sql.ErrNoRows,
		}).buildPlan(context.Background(), criticalGroup(), "policy-1",
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
		team: &model.Team{ID: "team-1"},
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
		"policy-1", schedulerender.TeamOnCallResult{})
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
		team: &model.Team{ID: "team-1"},
		policy: &model.EscalationPolicy{
			ID: "policy-1", Name: "Mixed",
			Steps: []*model.EscalationStep{{
				StepIndex: 0, Provider: "slack", TargetKind: "dm",
				TargetType: "user", TargetID: "U_ONE",
			}},
		},
	}

	admission, err := planFor(store).buildPlan(context.Background(), criticalGroup(),
		"policy-1", schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	var snapshot model.EscalationPolicySnapshot
	if err := json.Unmarshal(admission.PolicySnapshot, &snapshot); err != nil {
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
		team: &model.Team{ID: "team-1"},
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
		"policy-1", schedulerender.TeamOnCallResult{})
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
	if len(admission.Unpromised) != 1 ||
		admission.Unpromised[0].Reason != outbound.ReasonDuplicate {
		t.Fatalf("the repeat was recorded as %v", admission.Unpromised)
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
	ag.SlackUpdateGeneration = 7

	admission, err := planFor(&failingStore{
		policyErr: sql.ErrNoRows, team: &model.Team{ID: "team-1"},
	}).buildPlan(context.Background(), ag, "", schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	if admission.SourceVersion != 7 {
		t.Fatalf("the plan says it was built from version %d, the group is at 7",
			admission.SourceVersion)
	}
}

// TestTheFrozenStateCarriesTheHistory. The card shows what has happened to the
// alert so far, and the snapshot is the only thing it may read. A producer that
// never loaded the history froze an empty one into every message of the
// escalation - permanently, because revision 0 is what the first cards render.
func TestTheFrozenStateCarriesTheHistory(t *testing.T) {
	ag := criticalGroup()
	ag.TimelineEvents = []*model.TimelineEvent{
		{
			ID: "ev-2", AlertGroupID: ag.ID, Type: model.TimelineEventAlertAdded,
			Message: "A second alert joined", CreatedAt: time.Unix(1700000200, 0),
		},
		{
			ID: "ev-1", AlertGroupID: ag.ID, Type: model.TimelineEventCreated,
			Message: "Alert group created", CreatedAt: time.Unix(1700000100, 0),
		},
	}

	admission, err := planFor(&failingStore{
		policyErr: sql.ErrNoRows, team: &model.Team{ID: "team-1"},
	}).buildPlan(context.Background(), ag, "", schedulerender.TeamOnCallResult{})
	if err != nil {
		t.Fatalf("build the plan: %v", err)
	}

	history := admission.Admission.Snapshot.Content().Timeline
	if len(history) != 2 {
		t.Fatalf("the frozen state holds %d lines of history, want 2", len(history))
	}
	// Oldest first: the snapshot settles the order once, and the stored, hashed
	// and rendered orders are the same order.
	if history[0].ID != "ev-1" || history[1].ID != "ev-2" {
		t.Fatalf("the history reads %s then %s", history[0].ID, history[1].ID)
	}
}

// TestAHistoryLineThisBuildCannotShowDoesNotCostThePage. Admission refuses an
// event it cannot name, and it is right to: rendering it as something else
// would put a digest behind a message nobody wrote. But refusing HERE costs the
// page - one line written by another build and the alert is unadmittable on
// every tick, forever, with nobody notified. The line stays in the audit and
// leaves the card.
func TestAHistoryLineThisBuildCannotShowDoesNotCostThePage(t *testing.T) {
	unshowable := []*model.TimelineEvent{
		{
			ID: "ev-x", AlertGroupID: "ag-1", Type: model.TimelineEventType("teleported"),
			Message: "written by a build that knows more", CreatedAt: time.Unix(1700000100, 0),
		},
		{
			ID: "", AlertGroupID: "ag-1", Type: model.TimelineEventCreated,
			Message: "no id", CreatedAt: time.Unix(1700000100, 0),
		},
		{
			ID: "ev-z", AlertGroupID: "ag-1", Type: model.TimelineEventCreated,
			Message: "no time",
		},
	}

	for _, event := range unshowable {
		t.Run(event.Message, func(t *testing.T) {
			ag := criticalGroup()
			ag.TimelineEvents = []*model.TimelineEvent{
				{
					ID: "ev-1", AlertGroupID: ag.ID, Type: model.TimelineEventCreated,
					Message: "Alert group created", CreatedAt: time.Unix(1700000000, 0),
				},
				event,
			}

			admission, err := planFor(&failingStore{
				policyErr: sql.ErrNoRows, team: &model.Team{ID: "team-1"},
			}).buildPlan(context.Background(), ag, "", schedulerender.TeamOnCallResult{})
			if err != nil {
				t.Fatalf("a history line nobody can show stopped the escalation: %v", err)
			}
			if len(admission.Admission.Commitments) == 0 {
				t.Fatal("nothing was promised")
			}

			history := admission.Admission.Snapshot.Content().Timeline
			if len(history) != 1 || history[0].ID != "ev-1" {
				t.Fatalf("the card would show %+v", history)
			}
		})
	}
}
