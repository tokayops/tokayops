package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The cutover file is not run by the application, so nothing else would notice
// if it stopped applying - or if a start-up put back what it removes.
//
// It runs here against the shape it is written for, which this build no longer
// creates: a database upgraded from the version before it, with the job
// engine's tables still standing and rows in them. Run against a fresh schema
// instead, every statement in it is an IF EXISTS over nothing, and taking any
// of them out would still pass.

// legacyJobEngine builds the tables as the version before this one had them,
// foreign keys included - the keys are what make the order the file is written
// in the only order that works.
func legacyJobEngine(t *testing.T, s *Store) {
	t.Helper()

	// The suite shares one database and nothing truncates these any more, so a
	// run that fails halfway would leave them standing for everything after it.
	// The cutover takes them away when the test gets that far; this is for when
	// it does not.
	t.Cleanup(func() {
		if _, err := s.db.Exec(`
			DROP TABLE IF EXISTS notification_deliveries, job_steps, job_stages,
			                     jobs, job_dedup_policies;
			ALTER TABLE alert_groups
				DROP COLUMN IF EXISTS slack_update_pending,
				DROP COLUMN IF EXISTS ack_processed_at`); err != nil {
			t.Fatalf("clear the previous shape: %v", err)
		}
	})

	for _, ddl := range []string{
		`CREATE TABLE jobs (
			id TEXT PRIMARY KEY, type TEXT NOT NULL, status TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			dedup_namespace TEXT, dedup_key TEXT, dedup_scope TEXT,
			alert_group_id TEXT, current_stage INTEGER DEFAULT 0, error TEXT,
			created_at TIMESTAMPTZ DEFAULT NOW(), updated_at TIMESTAMPTZ DEFAULT NOW(),
			finished_at TIMESTAMPTZ, canceled_at TIMESTAMPTZ)`,
		`CREATE TABLE job_stages (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			stage_index INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'blocked',
			created_at TIMESTAMPTZ DEFAULT NOW(), updated_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(job_id, stage_index))`,
		`CREATE TABLE job_steps (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			stage_id TEXT NOT NULL REFERENCES job_stages(id),
			step_index INTEGER NOT NULL, step_type TEXT NOT NULL, status TEXT NOT NULL,
			data TEXT NOT NULL DEFAULT '{}', result TEXT, error TEXT,
			next_run_at TIMESTAMPTZ, locked_until TIMESTAMPTZ, locked_by TEXT,
			attempt_count INTEGER DEFAULT 0, timeout_seconds INTEGER,
			max_attempts INTEGER DEFAULT 5, continue_on_failure BOOLEAN DEFAULT FALSE,
			created_at TIMESTAMPTZ DEFAULT NOW(), updated_at TIMESTAMPTZ DEFAULT NOW())`,
		`CREATE TABLE notification_deliveries (
			id TEXT PRIMARY KEY,
			alert_group_id TEXT NOT NULL REFERENCES alert_groups(id),
			job_step_id TEXT UNIQUE REFERENCES job_steps(id) ON DELETE SET NULL,
			provider TEXT NOT NULL, kind TEXT NOT NULL,
			target_type TEXT, target_id TEXT, provider_payload TEXT,
			supports_update BOOLEAN DEFAULT FALSE,
			is_primary BOOLEAN DEFAULT FALSE, is_firehose BOOLEAN DEFAULT FALSE,
			attempt INTEGER DEFAULT 0,
			created_at TIMESTAMPTZ DEFAULT NOW(), updated_at TIMESTAMPTZ DEFAULT NOW())`,
		`CREATE TABLE job_dedup_policies (
			namespace TEXT PRIMARY KEY,
			scope     TEXT NOT NULL CHECK (scope IN ('while_active', 'forever')),
			job_type  TEXT NOT NULL,
			UNIQUE (namespace, scope, job_type))`,
		// The key that makes job_dedup_policies a parent of jobs rather than a
		// table beside it. Without it the drops could be written in any order
		// and this would still pass, while a real database refused them.
		`ALTER TABLE jobs ADD CONSTRAINT jobs_dedup_policy_fk
			FOREIGN KEY (dedup_namespace, dedup_scope, type)
			REFERENCES job_dedup_policies (namespace, scope, job_type)
			ON UPDATE RESTRICT ON DELETE RESTRICT`,
		`ALTER TABLE alert_groups
			ADD COLUMN slack_update_pending BOOLEAN NOT NULL DEFAULT FALSE,
			ADD COLUMN ack_processed_at TIMESTAMPTZ`,
	} {
		if _, err := s.db.Exec(ddl); err != nil {
			t.Fatalf("build the previous shape: %v", err)
		}
	}
}

func TestTheCutoverLeavesTheJobEngineGone(t *testing.T) {
	s := setupTestDB(t)
	legacyJobEngine(t, s)

	// Rows, because a drop over empty tables proves less than half of this: a
	// foreign key with nothing under it refuses nothing, and the order these
	// statements are written in would stop mattering.
	agID := outboundGroup(t, s)
	exec(t, s, `INSERT INTO jobs (id, type, status, alert_group_id)
	            VALUES ('job-1', 'escalation', 'succeeded', $1)`, agID)
	exec(t, s, `INSERT INTO job_stages (id, job_id, stage_index, status)
	            VALUES ('stage-1', 'job-1', 0, 'succeeded')`)
	exec(t, s, `INSERT INTO job_steps (id, job_id, stage_id, step_index, step_type, status)
	            VALUES ('step-1', 'job-1', 'stage-1', 0, 'dm', 'succeeded')`)
	exec(t, s, `INSERT INTO notification_deliveries (id, alert_group_id, job_step_id, provider, kind)
	            VALUES ('delivery-1', $1, 'step-1', 'slack', 'slack_dm')`, agID)
	path := filepath.Join("..", "..", "migrations", "drop-job-engine.sql")
	statements, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if _, err := s.db.Exec(string(statements)); err != nil {
		t.Fatalf("run the cutover: %v", err)
	}
	assertJobEngineGone(t, s, "the cutover")

	// A start on the build that ships this file - the whole of it, not the
	// outbound half: every one of these tables was created by the main schema
	// block, and a check that skipped it would pass while a start put them
	// straight back.
	if err := s.InitDB(); err != nil {
		t.Fatalf("start after the cutover: %v", err)
	}
	assertJobEngineGone(t, s, "a start afterwards")

	// And the product still works on the result: an alert group is read through
	// the same statement every reader uses, which is what would break if a
	// dropped column were still in its column list.
	group, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("read an alert group after the cutover: %v", err)
	}
	if group == nil {
		t.Fatal("the alert group is gone")
	}
}

func assertJobEngineGone(t *testing.T, s *Store, after string) {
	t.Helper()
	for _, table := range []string{"jobs", "job_stages", "job_steps",
		"notification_deliveries", "job_dedup_policies"} {
		var present bool
		if err := s.db.QueryRow(
			`SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil {
			t.Fatalf("look for %s: %v", table, err)
		}
		if present {
			t.Errorf("%s is still here after %s", table, after)
		}
	}
	for _, column := range []string{"slack_update_pending", "ack_processed_at"} {
		if hasColumn(t, s, "alert_groups", column) {
			t.Errorf("alert_groups.%s is still here after %s", column, after)
		}
	}
}
