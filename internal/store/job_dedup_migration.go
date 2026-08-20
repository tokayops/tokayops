package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/tokayops/tokayops/internal/jobdedup"
)

// jobDedupSpecConstraint is added last by the one-shot migration below, inside
// the same transaction as everything else it does. Its presence is therefore
// exactly the statement "this database carries the job dedup model", and asking
// the catalog for it costs one lookup - unlike asking the jobs table whether any
// row still lacks a namespace, which scans it on every start forever.
const jobDedupSpecConstraint = "jobs_dedup_spec_complete"

// legacyJobDedupIndexes are the two rules the model replaces: a global one over
// dedup_key with no namespace, and a separate one for escalations over
// alert_group_id.
var legacyJobDedupIndexes = []string{"idx_active_jobs_dedup", "idx_one_escalation_per_ag"}

// ErrJobDedupActiveUnclassified refuses an upgrade that cannot express what a
// running job already claims.
//
// Stripping the key off such a row would leave it executing while holding
// nothing, and the producer that is gated on that claim would then be free to
// build a replacement - for an escalation, that is a second page for one
// incident. The upgrade stops instead, and the operator decides.
var ErrJobDedupActiveUnclassified = errors.New(
	"store: an active job holds a dedup key this version cannot express")

// jobDedupAdvisoryLock serializes the whole sequence below across instances.
//
// `CREATE TABLE IF NOT EXISTS` is not atomic against a concurrent creator:
// two instances starting together can both find the table missing and then
// collide on pg_type's unique index rather than on the table name, which is not
// an error either of them can read as "somebody else got there first". The
// contract this schema promises - start the instances in any order - has to be
// held by something, and a lock taken before the first statement is the
// cheapest something available.
//
// The number is arbitrary and only has to stay stable: "tokay" truncated, plus
// the epic that introduced the model.
const jobDedupAdvisoryLock int64 = 0x746F6B6111

// applyJobDedupModel brings a database onto the job dedup model, in one
// transaction, on every start.
//
// Three steps, and their order is part of the design: the policy table first,
// because the migration's foreign key needs its rows; the one-shot migration
// second; the sweep for legacy indexes last, since on a first run it would
// otherwise remove the guarantees the migration itself leans on.
func (s *Store) applyJobDedupModel() error {
	migrated, err := jobDedupModelApplied(s.db)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, jobDedupAdvisoryLock); err != nil {
		return fmt.Errorf("failed to take the job dedup migration lock: %w", err)
	}

	if err := syncJobDedupPoliciesTx(tx); err != nil {
		return err
	}

	var stripped int64
	if !migrated {
		// Asked again under the lock: the instance that waited here has to see
		// the finished work rather than repeat it.
		migrated, err = jobDedupModelApplied(tx)
		if err != nil {
			return err
		}
		if !migrated {
			if stripped, err = migrateJobDedupModelTx(tx); err != nil {
				return err
			}
		}
	}

	dropped, err := dropLegacyJobDedupIndexesTx(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Reported only now: before the commit none of it was true.
	if stripped > 0 {
		log.Printf("Job dedup migration: %d historical job(s) had a dedup key no family claims; "+
			"their key was cleared (the jobs and their outcomes were left alone)", stripped)
	}
	for _, name := range dropped {
		log.Printf("WARN: removed legacy job dedup index %s - an older version of TokayOps "+
			"was started against this database and recreated it", name)
	}
	return nil
}

// syncJobDedupPoliciesTx keeps the policy reference table in step with the code,
// and runs on EVERY start rather than inside the one-shot migration.
//
// A namespace introduced by a later release has to reach the table on the first
// start of the release that knows it. Seeding from inside the migration would
// mean the row never appears - the migration is long done - and the first job of
// that family would fail its foreign key.
func syncJobDedupPoliciesTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS job_dedup_policies (
			namespace TEXT PRIMARY KEY,
			scope     TEXT NOT NULL CHECK (scope IN ('while_active', 'forever')),
			-- The type a row of this family carries. It lives here, next to the
			-- scope, because the two are the same statement about the same
			-- family: a job free to name one family and carry another's type
			-- holds its claim under one name and answers queries under the
			-- other, and the alert group it belongs to is never paged.
			job_type  TEXT NOT NULL,
			-- Not redundant with the primary key: it is the target the
			-- composite foreign key from jobs needs.
			UNIQUE (namespace, scope, job_type)
		)`); err != nil {
		return fmt.Errorf("failed to create job_dedup_policies: %w", err)
	}

	// DO NOTHING, never DO UPDATE: a namespace does not change its policy, a
	// family whose policy changes takes a new name. The composite foreign key
	// carries ON UPDATE RESTRICT, so an UPDATE here would be refused by the
	// database the moment any job referenced the row.
	for _, p := range jobdedup.Policies() {
		if _, err := tx.Exec(`
			INSERT INTO job_dedup_policies (namespace, scope, job_type) VALUES ($1, $2, $3)
			ON CONFLICT (namespace) DO NOTHING`,
			string(p.Namespace), string(p.Scope), p.JobType); err != nil {
			return fmt.Errorf("failed to seed job dedup policy %s: %w", p.Namespace, err)
		}
	}

	return verifyJobDedupPoliciesTx(tx)
}

// verifyJobDedupPolicies compares the table with the code.
//
// The two disagree only when someone changes a namespace's scope instead of
// introducing a new namespace. Left to the foreign key, that mistake surfaces on
// the first insert - which, for an escalation, is the moment someone should have
// been paged. Refusing to start moves it to a moment nobody is waiting.
func verifyJobDedupPoliciesTx(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT namespace, scope, job_type FROM job_dedup_policies ORDER BY namespace`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var namespace, scope, jobType string
		if err := rows.Scan(&namespace, &scope, &jobType); err != nil {
			return err
		}
		want, known := jobdedup.PolicyOf(jobdedup.Namespace(namespace))
		if !known {
			// A database written by a newer release. Nothing here uses that
			// namespace, so refusing to start would turn a harmless trace into
			// an outage.
			log.Printf("WARN: job dedup policy %q is unknown to this version - "+
				"this database has been used by a newer release", namespace)
			continue
		}
		if string(want.Scope) != scope || want.JobType != jobType {
			return fmt.Errorf(
				"store: job dedup policy %q is (%s, %s) in this database and (%s, %s) in this "+
					"build; a namespace never changes its policy - introduce a new namespace instead",
				namespace, scope, jobType, want.Scope, want.JobType)
		}
	}
	return rows.Err()
}

// jobDedupModelApplied asks the catalog whether the migration has run.
//
// conrelid as well as conname: a constraint name is unique within its table,
// not within the database, so asking by name alone would one day find somebody
// else's constraint.
func jobDedupModelApplied(q interface {
	QueryRow(string, ...any) *sql.Row
}) (bool, error) {
	var applied bool
	err := q.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM pg_constraint
		 WHERE conrelid = 'jobs'::regclass AND conname = $1)`,
		jobDedupSpecConstraint).Scan(&applied)
	return applied, err
}

// migrateJobDedupModelTx is the migration itself, on a caller-owned
// transaction, so a test can run it and roll it back to prove that a failure
// half way leaves the old schema exactly as it was.
//
// Returns how many historical rows were left without a dedup key.
func migrateJobDedupModelTx(tx *sql.Tx) (int64, error) {
	// Taken before the first read, not left to the DDL below to acquire on its
	// own: between the pre-flight check and the backfill there must be no
	// window in which somebody writes a job. The advisory lock above serializes
	// instances running this migration; this one is about every other writer,
	// including an older instance the operator forgot to stop.
	if _, err := tx.Exec(`LOCK TABLE jobs IN ACCESS EXCLUSIVE MODE`); err != nil {
		return 0, fmt.Errorf("failed to lock jobs: %w", err)
	}

	// The escalation index goes first, and only this one: the alert_group_id
	// backfill immediately below temporarily produces duplicates, which is why
	// the pre-model migration dropped it around the same statements.
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_one_escalation_per_ag`); err != nil {
		return 0, err
	}

	if _, err := tx.Exec(`
		ALTER TABLE jobs ADD COLUMN IF NOT EXISTS dedup_namespace TEXT;
		ALTER TABLE jobs ADD COLUMN IF NOT EXISTS dedup_scope TEXT;`); err != nil {
		return 0, fmt.Errorf("failed to add dedup columns: %w", err)
	}

	// An escalation's identity is its alert group, so rows that never got the
	// column filled have to get it before they can be given a key.
	if _, err := tx.Exec(`
		UPDATE jobs SET alert_group_id = payload::jsonb->>'alert_group_id'
		 WHERE type = 'escalation' AND alert_group_id IS NULL
		   AND payload::jsonb->>'alert_group_id' IS NOT NULL`); err != nil {
		return 0, fmt.Errorf("failed to backfill alert_group_id: %w", err)
	}

	// One escalation row per group keeps the column; the others lose it, and
	// with it their identity. Active first, then newest: a job a worker is
	// running right now must not be the one that gets disowned.
	if _, err := tx.Exec(`
		WITH ranked AS (
			SELECT id, ROW_NUMBER() OVER (
				PARTITION BY alert_group_id
				ORDER BY (status IN ('pending', 'running')) DESC, created_at DESC, id
			) AS rn
			FROM jobs
			WHERE type = 'escalation' AND alert_group_id IS NOT NULL
		)
		UPDATE jobs SET alert_group_id = NULL
		  FROM ranked
		 WHERE jobs.id = ranked.id AND ranked.rn > 1`); err != nil {
		return 0, fmt.Errorf("failed to deduplicate escalation jobs: %w", err)
	}

	if err := refuseUnclassifiedActiveJobs(tx); err != nil {
		return 0, err
	}

	// Only the escalation key is rewritten. The others keep the string they
	// have, prefix and all: Sprint 3 decides those identities for real, and
	// rewriting them twice would mean reading the same history two ways in two
	// releases.
	//
	// starts_with, not LIKE: every prefix here contains an underscore, which
	// LIKE reads as a wildcard.
	backfills := []string{
		`UPDATE jobs SET dedup_namespace = 'escalation', dedup_key = alert_group_id, dedup_scope = 'forever'
		  WHERE type = 'escalation' AND alert_group_id IS NOT NULL`,
		`UPDATE jobs SET dedup_namespace = 'ack_update', dedup_scope = 'while_active'
		  WHERE type = 'update' AND dedup_key IS NOT NULL AND starts_with(dedup_key, 'update_ack_')`,
		`UPDATE jobs SET dedup_namespace = 'alert_update', dedup_scope = 'while_active'
		  WHERE type = 'update' AND dedup_key IS NOT NULL AND starts_with(dedup_key, 'update_alert_')`,
		`UPDATE jobs SET dedup_namespace = 'resolution', dedup_scope = 'while_active'
		  WHERE type = 'resolution' AND dedup_key IS NOT NULL AND starts_with(dedup_key, 'resolve_')`,
		`UPDATE jobs SET dedup_namespace = 'handoff', dedup_scope = 'while_active'
		  WHERE type = 'handoff_notify' AND dedup_key IS NOT NULL`,
	}
	for _, q := range backfills {
		if _, err := tx.Exec(q); err != nil {
			return 0, fmt.Errorf("failed to backfill dedup spec: %w", err)
		}
	}

	// What is left holds a key no family claims, and by now it is terminal -
	// the active ones were refused above. The key goes; the row and its outcome
	// stay.
	res, err := tx.Exec(`
		UPDATE jobs SET dedup_key = NULL
		 WHERE dedup_namespace IS NULL AND dedup_key IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("failed to clear unclassified dedup keys: %w", err)
	}
	stripped, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	// Two policies, two indexes. Without the status predicate the first one
	// would quietly be the second.
	if _, err := tx.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_dedup_active
			ON jobs (dedup_namespace, dedup_key)
			WHERE dedup_scope = 'while_active' AND status IN ('pending', 'running');
		CREATE UNIQUE INDEX IF NOT EXISTS idx_jobs_dedup_forever
			ON jobs (dedup_namespace, dedup_key)
			WHERE dedup_scope = 'forever';`); err != nil {
		return 0, fmt.Errorf("failed to create dedup indexes: %w", err)
	}

	// Dropped only now: until the index above existed, this one was what kept
	// two active jobs from sharing a key.
	if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_active_jobs_dedup`); err != nil {
		return 0, err
	}

	// The row points at a whole policy, not at parts of one: namespace, scope
	// AND type together have to be a combination the registry declares. That is
	// what makes "escalation namespace with another type" and "escalation type
	// under another namespace" equally impossible - both are harmful, because
	// one of the two names holds the claim while the other answers the engine's
	// queries, and a row that splits them belongs to a group nobody pages.
	//
	// MATCH SIMPLE is what lets history through: a row with no spec has NULLs
	// in two of the three columns, so the foreign key does not apply to it.
	if _, err := tx.Exec(`
		ALTER TABLE jobs ADD CONSTRAINT jobs_dedup_policy_fk
			FOREIGN KEY (dedup_namespace, dedup_scope, type)
			REFERENCES job_dedup_policies (namespace, scope, job_type)
			ON UPDATE RESTRICT ON DELETE RESTRICT`); err != nil {
		return 0, fmt.Errorf("failed to add dedup policy foreign key: %w", err)
	}

	// What a foreign key cannot say: an escalation answers for a particular
	// alert group, and the column that says which one is not part of any
	// policy. Required explicitly rather than merely compared - comparing a
	// NULL yields NULL, and a CHECK counts NULL as satisfied, which is exactly
	// the row that claims a group without answering for it.
	//
	// Written against the TYPE, not against one namespace: the type is what the
	// engine's queries read, and a second escalation namespace is not
	// hypothetical - it is how this model says a family changed its policy. A
	// row with no spec is exempt, which is what lets a disowned escalation from
	// before the upgrade keep its type without an alert group.
	if _, err := tx.Exec(`
		ALTER TABLE jobs ADD CONSTRAINT jobs_escalation_identity CHECK (
			dedup_namespace IS NULL
			OR type <> 'escalation'
			OR (alert_group_id IS NOT NULL AND alert_group_id = dedup_key))`); err != nil {
		return 0, fmt.Errorf("failed to add escalation identity check: %w", err)
	}

	// Last, and the marker of a finished migration. It is also the only thing
	// standing between a half-filled spec and the schema: the foreign key is
	// MATCH SIMPLE, so it passes any row with a NULL among its columns.
	if _, err := tx.Exec(`
		ALTER TABLE jobs ADD CONSTRAINT ` + jobDedupSpecConstraint + ` CHECK (
			(dedup_namespace IS NULL AND dedup_key IS NULL AND dedup_scope IS NULL)
			OR (dedup_namespace IS NOT NULL AND dedup_key IS NOT NULL AND dedup_scope IS NOT NULL))`); err != nil {
		return 0, fmt.Errorf("failed to add dedup spec check: %w", err)
	}

	return stripped, nil
}

// refuseUnclassifiedActiveJobs stops the upgrade on an active job whose family
// cannot be named. A row with no dedup key at all is not one of these: it holds
// no claim today either, so it loses nothing.
func refuseUnclassifiedActiveJobs(tx *sql.Tx) error {
	rows, err := tx.Query(`
		SELECT id, type, status FROM jobs
		 WHERE dedup_key IS NOT NULL
		   AND status IN ('pending', 'running')
		   AND NOT (
				(type = 'escalation' AND alert_group_id IS NOT NULL)
				OR (type = 'update' AND (starts_with(dedup_key, 'update_ack_')
					OR starts_with(dedup_key, 'update_alert_')))
				OR (type = 'resolution' AND starts_with(dedup_key, 'resolve_'))
				OR (type = 'handoff_notify')
		   )
		 ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var id, jobType, status string
		if err := rows.Scan(&id, &jobType, &status); err != nil {
			return err
		}
		if len(found) < 10 {
			found = append(found, fmt.Sprintf("%s (%s, %s)", id, jobType, status))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}

	// Written for the operator who has to act, not for a developer reading a
	// stack trace: the remedy is a decision about a live job, so the message
	// names the jobs and the two ways out.
	return fmt.Errorf("%w: %s. Let them finish, or cancel them "+
		"(UPDATE jobs SET status = 'canceled', canceled_at = NOW() WHERE id = ...), then start again",
		ErrJobDedupActiveUnclassified, strings.Join(found, ", "))
}

// dropLegacyJobDedupIndexesTx runs on every start, after the migration.
//
// An older image started against this database recreates both indexes from its
// own InitDB. It cannot write jobs any more - the spec check refuses its rows -
// but the indexes it leaves behind are enough to break this version: the global
// one over dedup_key knows nothing of namespaces, so it turns a deduplication
// that should be silent into a unique violation. Removing them again is cheaper
// than a runbook step; the names it removed go back to the caller, which logs
// them after the commit, so the repair is never silent.
func dropLegacyJobDedupIndexesTx(tx *sql.Tx) ([]string, error) {
	var dropped []string
	for _, name := range legacyJobDedupIndexes {
		var present bool
		if err := tx.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&present); err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		// nosemgrep: string-formatted-query - names come from the list above
		if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
			return nil, err
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}
