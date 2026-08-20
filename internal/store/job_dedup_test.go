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
		name:        "forever refuses a second job even after the first is done",
		first:       jobdedup.Escalation("ag-forever"),
		firstEnds:   model.JobStatusSucceeded,
		second:      jobdedup.Escalation("ag-forever"),
		wantCreated: false,
	},
	{
		name:        "another key in the same namespace is other work",
		first:       jobdedup.AckUpdate("ag-one"),
		firstEnds:   model.JobStatusPending,
		second:      jobdedup.AckUpdate("ag-two"),
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
}

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
			if tc.firstEnds == model.JobStatusSucceeded {
				s.MarkJobSucceeded(tc.first)
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
	if err := s.UpdateAlertGroupStatus(firstAG, model.AlertGroupStatusResolved); err != nil {
		t.Fatalf("resolve the first group: %v", err)
	}
	secondAG := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID:               secondAG,
		DedupKey:         ag.DedupKey,
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
