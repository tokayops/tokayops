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
// database is not hypothetical - it is what a review found once already, and a
// rule written out twice is a rule that drifts again.
//
// Both scopes are here. Escalation is absent because it left the job engine
// entirely - an escalation is a set of commitments in the outbound domain now,
// and the claim that holds a group forever is its admission batch. The handover
// occurrence carries the forever scope here: forever, and admitted through this
// door like anything else.
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
		// Unreachable until the index changed: one index over dedup_key alone
		// made two families sharing a string collide, whichever families they
		// were.
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
