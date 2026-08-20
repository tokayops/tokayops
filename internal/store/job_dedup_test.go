package store

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/jobdedup"
	"github.com/tokayops/tokayops/internal/model"
)

// The questions the dedup model answers, asked once and put to both
// implementations: the database below, the double under it.
//
// One table rather than two suites, because the double drifting from the
// database is not hypothetical - it is what the previous sprint found, and a
// rule written out twice is a rule that drifts again.
//
// Both policies are here now. Escalation is still absent - it holds forever but
// cannot be admitted through this door at all (see
// TestJobDedup_EscalationIsNotAdmittedByTheGeneralDoor), so its guarantee is
// exercised through EnsureEscalationJob - and the handover occurrence takes its
// place: forever, and admitted here like anything else.
//
// Every terminal status appears, not only success: what a while_active identity
// promises is that the work can happen again once the job is out of flight,
// however it left, and what a forever identity promises is that it cannot.
var jobDedupCases = []struct {
	name        string
	first       *jobdedup.Spec
	firstEnds   model.JobStatus
	second      *jobdedup.Spec
	wantCreated bool
}{
	{
		name:        "while_active refuses a second job while the first is running",
		first:       jobdedup.AckUpdate("ag-active"),
		firstEnds:   model.JobStatusPending,
		second:      jobdedup.AckUpdate("ag-active"),
		wantCreated: false,
	},
	{
		name:        "while_active admits the work again once the first job is done",
		first:       jobdedup.AckUpdate("ag-again"),
		firstEnds:   model.JobStatusSucceeded,
		second:      jobdedup.AckUpdate("ag-again"),
		wantCreated: true,
	},
	{
		name:        "another key in the same namespace is other work",
		first:       jobdedup.AckUpdate("ag-one"),
		firstEnds:   model.JobStatusPending,
		second:      jobdedup.AckUpdate("ag-two"),
		wantCreated: true,
	},
	{
		name:        "while_active admits the work again after a failed job",
		first:       jobdedup.AckUpdate("ag-failed"),
		firstEnds:   model.JobStatusFailed,
		second:      jobdedup.AckUpdate("ag-failed"),
		wantCreated: true,
	},
	{
		name:        "while_active admits the work again after a canceled job",
		first:       jobdedup.AckUpdate("ag-canceled"),
		firstEnds:   model.JobStatusCanceled,
		second:      jobdedup.AckUpdate("ag-canceled"),
		wantCreated: true,
	},
	{
		// Unreachable before this sprint: one index over dedup_key alone made
		// two families sharing a string collide, whichever families they were.
		name:        "the same key in another namespace is other work",
		first:       mustSpec("ack_update", "shared-key", "while_active"),
		firstEnds:   model.JobStatusPending,
		second:      mustSpec("resolution", "shared-key", "while_active"),
		wantCreated: true,
	},
	{
		name:        "forever refuses a second job while the first is running",
		first:       testOccurrence("sched-forever-active"),
		firstEnds:   model.JobStatusPending,
		second:      testOccurrence("sched-forever-active"),
		wantCreated: false,
	},
	{
		// The point of the policy, and of bug 13: the occurrence was announced
		// once, and a second instance noticing it later is not a second
		// occurrence.
		name:        "forever refuses the work again after the job succeeded",
		first:       testOccurrence("sched-forever-done"),
		firstEnds:   model.JobStatusSucceeded,
		second:      testOccurrence("sched-forever-done"),
		wantCreated: false,
	},
	{
		name:        "forever refuses the work again after the job failed",
		first:       testOccurrence("sched-forever-failed"),
		firstEnds:   model.JobStatusFailed,
		second:      testOccurrence("sched-forever-failed"),
		wantCreated: false,
	},
	{
		name:        "forever refuses the work again after the job was canceled",
		first:       testOccurrence("sched-forever-canceled"),
		firstEnds:   model.JobStatusCanceled,
		second:      testOccurrence("sched-forever-canceled"),
		wantCreated: false,
	},
	{
		name:        "another occurrence of the same schedule is other work",
		first:       testOccurrence("sched-two-turns"),
		firstEnds:   model.JobStatusSucceeded,
		second:      testOccurrenceAt("sched-two-turns", 24*time.Hour),
		wantCreated: true,
	},
}

// testOccurrence is one handover as the notifier would name it. The parts are
// fixed except the schedule, so two calls with the same schedule are the same
// occurrence and two with different ones are not.
func testOccurrence(scheduleID string) *jobdedup.Spec {
	return testOccurrenceAt(scheduleID, 0)
}

func testOccurrenceAt(scheduleID string, after time.Duration) *jobdedup.Spec {
	return jobdedup.HandoffOccurrence(jobdedup.Occurrence{
		Kind:            "handoff",
		ScheduleID:      scheduleID,
		Source:          "rotation",
		GroupID:         "g-a",
		UserIDs:         []string{"u-alice"},
		AssignmentStart: time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC).Add(after),
		RevisionID:      "rev-1",
	})
}

func strPtr(s string) *string { return &s }

func mustSpec(namespace, key, scope string) *jobdedup.Spec {
	spec, err := jobdedup.New(jobdedup.Namespace(namespace), key, jobdedup.Scope(scope))
	if err != nil {
		panic(err)
	}
	return spec
}

func dedupTestJob(spec *jobdedup.Spec) *model.Job {
	now := time.Now()
	return &model.Job{
		ID:        uuid.New().String(),
		Type:      "test",
		Status:    model.JobStatusPending,
		Dedup:     spec,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestJobDedupRules_Store(t *testing.T) {
	s := setupTestDB(t)

	for _, tc := range jobDedupCases {
		t.Run(tc.name, func(t *testing.T) {
			first := dedupTestJob(tc.first)
			if created, err := s.CreateJobWithDedup(first, nil, nil); err != nil || !created {
				t.Fatalf("first insert: created=%v err=%v", created, err)
			}
			if tc.firstEnds != model.JobStatusPending {
				if _, err := s.db.Exec(`UPDATE jobs SET status = $1 WHERE id = $2`,
					tc.firstEnds, first.ID); err != nil {
					t.Fatalf("age the first job: %v", err)
				}
			}

			created, err := s.CreateJobWithDedup(dedupTestJob(tc.second), nil, nil)
			if err != nil {
				t.Fatalf("second insert: %v", err)
			}
			if created != tc.wantCreated {
				t.Errorf("second insert created=%v, want %v", created, tc.wantCreated)
			}
		})
	}
}

func TestJobDedupRules_MockStore(t *testing.T) {
	for _, tc := range jobDedupCases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewMockStore()

			first := dedupTestJob(tc.first)
			if created, err := s.CreateJobWithDedup(first, nil, nil); err != nil || !created {
				t.Fatalf("first insert: created=%v err=%v", created, err)
			}
			if tc.firstEnds != model.JobStatusPending {
				s.MarkJobFinished(tc.first, tc.firstEnds)
			}

			created, err := s.CreateJobWithDedup(dedupTestJob(tc.second), nil, nil)
			if err != nil {
				t.Fatalf("second insert: %v", err)
			}
			if created != tc.wantCreated {
				t.Errorf("second insert created=%v, want %v", created, tc.wantCreated)
			}
		})
	}
}

// A job without an identity is refused rather than admitted unclaimed.
func TestJobDedup_SpecIsRequired(t *testing.T) {
	s := setupTestDB(t)
	job := dedupTestJob(nil)

	if _, err := s.CreateJobWithDedup(job, nil, nil); err == nil {
		t.Fatal("a job with no dedup spec was accepted")
	}

	mock := NewMockStore()
	if _, err := mock.CreateJobWithDedup(dedupTestJob(nil), nil, nil); err == nil {
		t.Fatal("the double accepted a job the store refuses")
	}
}

// A conflict on the primary key is a bug, not a deduplication. The targeted
// ON CONFLICT is what keeps the two apart: a clause naming the table's dedup
// index cannot swallow a collision on any other constraint.
func TestJobDedup_PrimaryKeyConflictIsAnError(t *testing.T) {
	s := setupTestDB(t)

	first := dedupTestJob(jobdedup.AckUpdate("ag-pkey"))
	if created, err := s.CreateJobWithDedup(first, nil, nil); err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}

	clash := dedupTestJob(jobdedup.AckUpdate("ag-pkey-other"))
	clash.ID = first.ID

	created, err := s.CreateJobWithDedup(clash, nil, nil)
	if err == nil {
		t.Fatalf("duplicate job id reported created=%v instead of failing", created)
	}
	if !strings.Contains(err.Error(), "jobs_pkey") {
		t.Errorf("error does not name the constraint that failed: %v", err)
	}
}

// The schema, not only the constructors, refuses a namespace nobody declared.
func TestJobDedup_SchemaRefusesAnUndeclaredNamespace(t *testing.T) {
	s := setupTestDB(t)

	_, err := s.db.Exec(`
		INSERT INTO jobs (id, type, status, payload, dedup_namespace, dedup_key, dedup_scope,
			current_stage, created_at, updated_at)
		VALUES ($1, 'test', 'pending', '{}', 'not_a_family', 'k', 'forever', 0, NOW(), NOW())`,
		uuid.New().String())
	if err == nil {
		t.Fatal("the schema accepted a job in a namespace no policy declares")
	}

	// And half a spec is not a spec, which the foreign key alone would allow:
	// it is MATCH SIMPLE, so a NULL in either column passes it.
	_, err = s.db.Exec(`
		INSERT INTO jobs (id, type, status, payload, dedup_namespace, dedup_key,
			current_stage, created_at, updated_at)
		VALUES ($1, 'test', 'pending', '{}', 'ack_update', 'k', 0, NOW(), NOW())`,
		uuid.New().String())
	if err == nil {
		t.Fatal("the schema accepted a job carrying half a dedup spec")
	}
}

// Two incidents that share an alert fingerprint are two incidents. The
// escalation of each is its own work, and a forever claim on the ALERT's key
// would have swallowed the second one - nobody would be paged.
func TestEscalationIdentityIsTheGroupNotTheAlert(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-repeat-incident"
	firstAG := createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusNew)
	ag, err := s.GetAlertGroupByID(firstAG)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}

	job, stages, steps, snapshot := makeTestJob(firstAG)
	if created, err := s.EnsureEscalationJob(firstAG, job, stages, steps, snapshot); err != nil || !created {
		t.Fatalf("first escalation: created=%v err=%v", created, err)
	}

	// The incident is resolved, and the same alert fires again: a new group
	// under the very same dedup key.
	forceAlertGroupStatus(t, s, firstAG, model.AlertGroupStatusResolved)
	secondAG := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID:               secondAG,
		AlertKey:         ag.AlertKey,
		Status:           model.AlertGroupStatusNew,
		TeamID:           teamID,
		TeamNameSnapshot: teamID,
		Severity:         "critical",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("create the second group: %v", err)
	}

	job2, stages2, steps2, snapshot2 := makeTestJob(secondAG)
	created, err := s.EnsureEscalationJob(secondAG, job2, stages2, steps2, snapshot2)
	if err != nil {
		t.Fatalf("second escalation: %v", err)
	}
	if !created {
		t.Fatal("the repeat incident got no escalation of its own - its alert dedup key was treated as the identity")
	}
}

// The general insert door does not admit escalations, and the schema says the
// same thing about rows that reach it any other way.
//
// The scenario this closes loses a page outright: a job carrying the escalation
// claim but not its type or alert group takes the claim without moving the
// alert group or storing a snapshot. The real EnsureEscalationJob then finds
// the claim held, reports created=false and commits the group into processing,
// where the query that finds unescalated groups cannot see it - it reads type
// and alert_group_id, which the impostor need not carry.
func TestJobDedup_EscalationIsNotAdmittedByTheGeneralDoor(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-one-door", model.AlertGroupStatusNew)

	impostor := dedupTestJob(jobdedup.Escalation(agID))
	impostor.Type = "test"
	if _, err := s.CreateJobWithDedup(impostor, nil, nil); err == nil {
		t.Fatal("the general door admitted an escalation claim")
	}
	mock := NewMockStore()
	mockImpostor := dedupTestJob(jobdedup.Escalation(agID))
	mockImpostor.Type = "test"
	if _, err := mock.CreateJobWithDedup(mockImpostor, nil, nil); err == nil {
		t.Error("the double admitted an escalation claim the store refuses")
	}

	// The type is the other name for the same thing, and a caller does not get
	// to choose it: whatever it asks for, the row carries the type its family
	// declares. A job typed 'escalation' under another namespace would answer
	// "has this group been escalated" while holding no claim on it.
	typed := dedupTestJob(jobdedup.AckUpdate(agID))
	typed.Type = "escalation"
	if created, err := s.CreateJobWithDedup(typed, nil, nil); err != nil || !created {
		t.Fatalf("insert: created=%v err=%v", created, err)
	}
	stored, err := s.GetJobByID(typed.ID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if stored.Type != "update" {
		t.Errorf("stored type = %q, want the type ack_update declares", stored.Type)
	}
	mockTyped := dedupTestJob(jobdedup.AckUpdate(agID))
	mockTyped.Type = "escalation"
	if _, err := mock.CreateJobWithDedup(mockTyped, nil, nil); err != nil {
		t.Fatalf("the double refused what the store accepted: %v", err)
	}
	if mockTyped.Type != "update" {
		t.Errorf("the double stored type %q, want the type ack_update declares", mockTyped.Type)
	}

	// Raw SQL is not a way round it either: the three ways of saying
	// "escalation of this group" may not disagree, in either direction.
	for _, tc := range []struct {
		name      string
		jobType   string
		namespace string
		key       string
		scope     string
		groupID   *string
	}{
		{"another type", "resolution", "escalation", agID, "forever", &agID},
		{"another group", "escalation", "escalation", agID, "forever", strPtr("some-other-group")},
		// The one a comparison alone would have let through: comparing a NULL
		// yields NULL, and a CHECK reads NULL as satisfied.
		{"no group at all", "escalation", "escalation", agID, "forever", nil},
		// And the reverse contradiction: the claim is elsewhere, the type still
		// answers for the group.
		{"escalation type under another namespace", "escalation", "ack_update",
			"update_ack_" + agID, "while_active", &agID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.db.Exec(`
				INSERT INTO jobs (id, type, status, payload, dedup_namespace, dedup_key, dedup_scope,
					alert_group_id, current_stage, created_at, updated_at)
				VALUES ($1, $2, 'pending', '{}', $3, $4, $5, $6, 0, NOW(), NOW())`,
				uuid.New().String(), tc.jobType, tc.namespace, tc.key, tc.scope, tc.groupID)
			if err == nil {
				t.Fatal("the schema accepted an escalation row that contradicts itself")
			}
		})
	}

	// And the group still gets its escalation, which is the whole point.
	job, stages, steps, snapshot := makeTestJob(agID)
	created, err := s.EnsureEscalationJob(agID, job, stages, steps, snapshot)
	if err != nil || !created {
		t.Fatalf("the real escalation was not created afterwards: created=%v err=%v", created, err)
	}
}

// The escalation claim outlives the job that holds it: a group whose escalation
// finished is not escalated again, however stale it looks.
func TestJobDedup_EscalationClaimOutlivesItsJob(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-forever", model.AlertGroupStatusNew)
	job, stages, steps, snapshot := makeTestJob(agID)
	if created, err := s.EnsureEscalationJob(agID, job, stages, steps, snapshot); err != nil || !created {
		t.Fatalf("first escalation: created=%v err=%v", created, err)
	}
	if _, err := s.db.Exec(`UPDATE jobs SET status = 'succeeded' WHERE id = $1`, job.ID); err != nil {
		t.Fatalf("age the escalation: %v", err)
	}

	again, _, _, againSnapshot := makeTestJob(agID)
	created, err := s.EnsureEscalationJob(agID, again, nil, nil, againSnapshot)
	if err != nil {
		t.Fatalf("second escalation: %v", err)
	}
	if created {
		t.Error("a finished escalation stopped holding its group - the incident would be paged twice")
	}
}

// A group the user has already acted on keeps its status: admitting an
// escalation for it is refused, and refused without side effects.
//
// The check that used to live here - a job whose type contradicts its identity -
// is gone because the contradiction is: EnsureEscalationJob derives the type,
// the identity and the alert group from the group it locks, so a caller has
// nothing left to disagree with.
func TestJobDedup_EscalationRefusedForAnActedOnGroupChangesNothing(t *testing.T) {
	check := func(t *testing.T, agID string,
		ensure func(*model.Job) (bool, error), statusOf func() model.AlertGroupStatus) {
		t.Helper()
		job, _, _, _ := makeTestJob(agID)
		created, err := ensure(job)
		if err != nil {
			t.Fatalf("EnsureEscalationJob: %v", err)
		}
		if created {
			t.Error("an acknowledged group was escalated - the user's decision was overridden")
		}
		if got := statusOf(); got != model.AlertGroupStatusAcknowledged {
			t.Errorf("alert group = %s after a refused escalation, want acknowledged", got)
		}
		// The job the caller handed in is untouched too: nothing was admitted,
		// so nothing stamped it with an identity.
		if job.Dedup != nil || job.AlertGroupID != nil || job.Type != "" {
			t.Errorf("the refused job came back stamped: dedup=%v group=%v type=%q",
				job.Dedup, job.AlertGroupID, job.Type)
		}
	}

	t.Run("store", func(t *testing.T) {
		s := setupTestDB(t)
		agID := createTestTeamAndAG(t, s, "team-acted-on", model.AlertGroupStatusAcknowledged)
		check(t, agID,
			func(job *model.Job) (bool, error) {
				return s.EnsureEscalationJob(agID, job, nil, nil, nil)
			},
			func() model.AlertGroupStatus {
				ag, err := s.GetAlertGroupByID(agID)
				if err != nil {
					t.Fatalf("GetAlertGroupByID: %v", err)
				}
				return ag.Status
			})
	})

	t.Run("mock", func(t *testing.T) {
		agID := "ag-acted-on"
		m := NewMockStore()
		if err := m.CreateAlertGroup(&model.AlertGroup{
			ID: agID, AlertKey: "k-" + agID, Status: model.AlertGroupStatusAcknowledged,
		}); err != nil {
			t.Fatalf("CreateAlertGroup: %v", err)
		}
		check(t, agID,
			func(job *model.Job) (bool, error) {
				return m.EnsureEscalationJob(agID, job, nil, nil, nil)
			},
			func() model.AlertGroupStatus {
				ag, err := m.GetAlertGroupByID(agID)
				if err != nil {
					t.Fatalf("GetAlertGroupByID: %v", err)
				}
				return ag.Status
			})
	})
}

// The fixture door is held to what the real one enforces, or the double drifts
// from the database again - which is the failure this whole model exists to
// stop repeating.
//
// A spec with the wrong scope is not among the cases: it cannot be written any
// more. The registry fills scope and type in from the namespace and the fields
// are not assignable, so the compiler refuses what this used to check at run
// time.
func TestJobDedup_SeedEscalationJobHoldsTheSameInvariant(t *testing.T) {
	agID := "ag-seed-invariant"
	newJob := func() *model.Job {
		job, _, _, _ := makeTestJob(agID)
		return job
	}

	t.Run("a claim already held", func(t *testing.T) {
		m := NewMockStore()
		if err := m.SeedEscalationJob(agID, newJob(), nil, nil); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		if err := m.SeedEscalationJob(agID, newJob(), nil, nil); err == nil {
			t.Error("the fixture built two escalations for one group, which the database cannot hold")
		}
	})
}
