package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// preModelJobsDDL puts the jobs table back the way it was before the dedup
// model, so the upgrade has something to upgrade.
//
// It used to be production DDL and is not any more. Only ever applied to a
// throwaway database: DROP COLUMN leaves its slot behind for good, and a shared
// database would collect them run after run.
const preModelJobsDDL = `
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_dedup_spec_complete;
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_dedup_policy_fk;
DROP INDEX IF EXISTS idx_jobs_dedup_active;
DROP INDEX IF EXISTS idx_jobs_dedup_forever;
ALTER TABLE jobs DROP COLUMN IF EXISTS dedup_namespace;
ALTER TABLE jobs DROP COLUMN IF EXISTS dedup_scope;

CREATE UNIQUE INDEX IF NOT EXISTS idx_active_jobs_dedup ON jobs(dedup_key)
	WHERE status IN ('pending', 'running');
CREATE UNIQUE INDEX IF NOT EXISTS idx_one_escalation_per_ag ON jobs(alert_group_id)
	WHERE type = 'escalation' AND alert_group_id IS NOT NULL;
`

func newPreModelDB(t *testing.T) *Store {
	t.Helper()
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	if _, err := s.db.Exec(preModelJobsDDL); err != nil {
		t.Fatalf("build the pre-model jobs table: %v", err)
	}
	return s
}

// seedPreModelJob writes a job the way the old code wrote them: a dedup key and
// nothing that says what the key means.
func seedPreModelJob(t *testing.T, s *Store, jobType, status, dedupKey string, alertGroupID *string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := s.db.Exec(`
		INSERT INTO jobs (id, type, status, payload, dedup_key, alert_group_id,
			current_stage, created_at, updated_at)
		VALUES ($1, $2, $3, '{}', $4, $5, 0, $6, $6)`,
		id, jobType, status, dedupKey, alertGroupID, time.Now()); err != nil {
		t.Fatalf("seed a pre-model %s job: %v", jobType, err)
	}
	return id
}

func jobSpecColumns(t *testing.T, s *Store, jobID string) (namespace, key, scope string) {
	t.Helper()
	var ns, k, sc *string
	if err := s.db.QueryRow(
		`SELECT dedup_namespace, dedup_key, dedup_scope FROM jobs WHERE id = $1`, jobID).
		Scan(&ns, &k, &sc); err != nil {
		t.Fatalf("read the spec of %s: %v", jobID, err)
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return deref(ns), deref(k), deref(sc)
}

func indexExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var present bool
	if err := s.db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&present); err != nil {
		t.Fatalf("look up index %s: %v", name, err)
	}
	return present
}

func columnExists(t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	var present bool
	if err := s.db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = $1 AND column_name = $2)`, table, column).Scan(&present); err != nil {
		t.Fatalf("look up column %s.%s: %v", table, column, err)
	}
	return present
}

func TestJobDedupMigration_ClassifiesEveryFamily(t *testing.T) {
	s := newPreModelDB(t)

	agID := "ag-classify"
	escalation := seedPreModelJob(t, s, "escalation", "succeeded", "alert-fingerprint-1", &agID)
	// An escalation whose alert group was taken away by the earlier migration
	// that made one escalation per group: nothing is left to derive a key from.
	orphan := seedPreModelJob(t, s, "escalation", "succeeded", "alert-fingerprint-2", nil)
	ackUpdate := seedPreModelJob(t, s, "update", "pending", "update_ack_"+agID, nil)
	alertUpdate := seedPreModelJob(t, s, "update", "succeeded", "update_alert_"+agID, nil)
	resolution := seedPreModelJob(t, s, "resolution", "pending", "resolve_"+agID, nil)
	handoff := seedPreModelJob(t, s, "handoff_notify", "pending", "handoff:sched-1:2026-01-05", nil)
	stray := seedPreModelJob(t, s, "update", "succeeded", "a_key_no_family_claims", nil)

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	for _, tc := range []struct {
		name                       string
		jobID                      string
		wantNS, wantKey, wantScope string
	}{
		// The escalation is the one key that is rewritten: its identity is the
		// group, not the alert whose fingerprint it used to carry.
		{"escalation", escalation, "escalation", agID, "forever"},
		{"ack update", ackUpdate, "ack_update", "update_ack_" + agID, "while_active"},
		{"alert update", alertUpdate, "alert_update", "update_alert_" + agID, "while_active"},
		{"resolution", resolution, "resolution", "resolve_" + agID, "while_active"},
		{"handoff", handoff, "handoff", "handoff:sched-1:2026-01-05", "while_active"},
		// Nothing can be said about these two, so nothing is said: the key goes
		// rather than being guessed at.
		{"orphaned escalation", orphan, "", "", ""},
		{"key no family claims", stray, "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, key, scope := jobSpecColumns(t, s, tc.jobID)
			if ns != tc.wantNS || key != tc.wantKey || scope != tc.wantScope {
				t.Errorf("spec = (%q, %q, %q), want (%q, %q, %q)",
					ns, key, scope, tc.wantNS, tc.wantKey, tc.wantScope)
			}
		})
	}

	if indexExists(t, s, "idx_active_jobs_dedup") || indexExists(t, s, "idx_one_escalation_per_ag") {
		t.Error("a legacy index survived the migration")
	}
	if !indexExists(t, s, "idx_jobs_dedup_active") || !indexExists(t, s, "idx_jobs_dedup_forever") {
		t.Error("the model's indexes are not both there")
	}
}

// An active job whose family cannot be named stops the upgrade. Clearing its
// key would leave it executing while holding nothing, and for an escalation the
// producer would then be free to build a second one - a second page for one
// incident.
func TestJobDedupMigration_RefusesAnActiveJobItCannotClassify(t *testing.T) {
	s := newPreModelDB(t)

	stuck := seedPreModelJob(t, s, "escalation", "running", "alert-fingerprint-stuck", nil)

	err := s.InitDB()
	if !errors.Is(err, ErrJobDedupActiveUnclassified) {
		t.Fatalf("InitDB error = %v, want it to refuse the unclassifiable job", err)
	}
	if !strings.Contains(err.Error(), stuck) {
		t.Errorf("the error does not name the job an operator has to deal with: %v", err)
	}

	if columnExists(t, s, "jobs", "dedup_namespace") {
		t.Error("the refused migration still added its columns")
	}
	if !indexExists(t, s, "idx_one_escalation_per_ag") {
		t.Error("the refused migration left the escalation index dropped")
	}
}

// Everything the migration does is one transaction. Half of it - the legacy
// index gone, the new one not yet built - is a database where two escalations
// for one alert group are possible.
func TestJobDedupMigration_RollsBackWholesale(t *testing.T) {
	s := newPreModelDB(t)

	agID := "ag-rollback"
	jobID := seedPreModelJob(t, s, "escalation", "succeeded", "alert-fingerprint-rollback", &agID)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := migrateJobDedupModelTx(tx); err != nil {
		t.Fatalf("the migration itself failed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if columnExists(t, s, "jobs", "dedup_namespace") || columnExists(t, s, "jobs", "dedup_scope") {
		t.Error("a rolled back migration left its columns behind")
	}
	if !indexExists(t, s, "idx_active_jobs_dedup") || !indexExists(t, s, "idx_one_escalation_per_ag") {
		t.Error("a rolled back migration left a legacy index dropped")
	}
	var key string
	if err := s.db.QueryRow(`SELECT dedup_key FROM jobs WHERE id = $1`, jobID).Scan(&key); err != nil {
		t.Fatalf("read the key back: %v", err)
	}
	if key != "alert-fingerprint-rollback" {
		t.Errorf("dedup_key = %q, want the value the rollback should have restored", key)
	}
}

func TestJobDedupModel_FreshDatabaseAndSecondRun(t *testing.T) {
	s := newThrowawayDB(t)

	for _, run := range []string{"first", "second"} {
		if err := s.InitDB(); err != nil {
			t.Fatalf("%s InitDB: %v", run, err)
		}
	}

	if !indexExists(t, s, "idx_jobs_dedup_active") || !indexExists(t, s, "idx_jobs_dedup_forever") {
		t.Error("a fresh database did not end up with both dedup indexes")
	}
	if indexExists(t, s, "idx_active_jobs_dedup") || indexExists(t, s, "idx_one_escalation_per_ag") {
		t.Error("a fresh database was given a legacy index")
	}
	var constraints int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM pg_constraint
		 WHERE conrelid = 'jobs'::regclass
		   AND conname IN ('jobs_dedup_spec_complete', 'jobs_dedup_policy_fk')`).Scan(&constraints); err != nil {
		t.Fatalf("count the constraints: %v", err)
	}
	if constraints != 2 {
		t.Errorf("jobs carries %d of its 2 dedup constraints", constraints)
	}
}

// The policy table is seeded on every start, not by the migration. A namespace
// a later release introduces has to reach a database whose migration is long
// finished - otherwise the first job of that family fails its foreign key.
func TestJobDedupPolicies_ReachADatabaseAlreadyMigrated(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	// Stand in for "this release knows a namespace the database has not seen".
	if _, err := s.db.Exec(`DELETE FROM job_dedup_policies WHERE namespace = 'handoff'`); err != nil {
		t.Fatalf("remove a policy: %v", err)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("second InitDB: %v", err)
	}

	var scope string
	if err := s.db.QueryRow(
		`SELECT scope FROM job_dedup_policies WHERE namespace = 'handoff'`).Scan(&scope); err != nil {
		t.Fatalf("the namespace never reached the table: %v", err)
	}
	if scope != "while_active" {
		t.Errorf("handoff seeded as %q", scope)
	}
}

// Changing a namespace's policy in place is the one disagreement worth refusing
// a start over: left alone it surfaces as a foreign key violation on the first
// insert, which for an escalation is the moment someone should be paged.
func TestJobDedupPolicies_ScopeMismatchRefusesToStart(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	if _, err := s.db.Exec(
		`UPDATE job_dedup_policies SET scope = 'forever' WHERE namespace = 'handoff'`); err != nil {
		t.Fatalf("change a policy: %v", err)
	}

	err := s.InitDB()
	if err == nil {
		t.Fatal("InitDB started against a database whose policy disagrees with the code")
	}
	if !strings.Contains(err.Error(), "handoff") {
		t.Errorf("the error does not name the namespace that disagrees: %v", err)
	}
}

// A namespace this build has never heard of means the database has been used by
// a newer release. Nothing here writes that family, so refusing to start would
// turn a harmless trace into an outage.
func TestJobDedupPolicies_UnknownNamespaceIsTolerated(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	if _, err := s.db.Exec(
		`INSERT INTO job_dedup_policies (namespace, scope) VALUES ('a_later_family', 'forever')`); err != nil {
		t.Fatalf("seed a future policy: %v", err)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB refused to start over a namespace it simply does not use: %v", err)
	}
}

// An older image started against this database recreates both legacy indexes
// from its own InitDB. It cannot write jobs any more, but the global index it
// leaves behind knows nothing of namespaces and would turn a silent
// deduplication into a unique violation.
func TestJobDedupModel_LegacyIndexesAreRemovedAgain(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	if _, err := s.db.Exec(`
		CREATE UNIQUE INDEX idx_active_jobs_dedup ON jobs(dedup_key)
			WHERE status IN ('pending', 'running');
		CREATE UNIQUE INDEX idx_one_escalation_per_ag ON jobs(alert_group_id)
			WHERE type = 'escalation' AND alert_group_id IS NOT NULL;`); err != nil {
		t.Fatalf("play the older image: %v", err)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	if indexExists(t, s, "idx_active_jobs_dedup") || indexExists(t, s, "idx_one_escalation_per_ag") {
		t.Error("a legacy index recreated by an older image survived a start")
	}
}
