package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	_ "github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// activeUserCTE is the head of every statement that creates something owned by
// a user: a token, an identity, a link, a membership.
//
// It does two jobs in one statement, and both are needed. The predicate makes
// "this user is alive" and "the row is inserted" atomic, so there is no window
// between checking and writing. The FOR SHARE makes erasure - which takes FOR
// UPDATE on the same row - wait for this transaction, so its sweep of the
// child tables runs after the insert rather than before it and misses nothing.
//
// Without the lock the predicate alone is not enough: an insert that was still
// uncommitted when erasure deleted the table would survive it.
//
// THE USER ID IS ALWAYS $1. That is the whole convention, and it is why this
// is a constant concatenated onto a query rather than a format string: a
// placeholder index chosen per call site is a silent coupling to the argument
// order, and getting it wrong locks the wrong person while every test that
// uses one user still passes.
const activeUserCTE = `WITH active AS (
	SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL FOR SHARE
)`

// lockActiveUserTx is activeUserCTE for a transaction that writes more than
// one statement: take the shared lock on the user first, and only then touch
// the rows that belong to them.
//
// Order matters as much as the lock. Erasure locks the user and then deletes
// the child rows; a transaction that locked a child row first and reached for
// the user second would be the other half of an AB-BA deadlock.
func lockActiveUserTx(tx *sql.Tx, userID string) error {
	var id string
	err := tx.QueryRow(
		`SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL FOR SHARE`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	return err
}

// ErrUserNotFound means no user matched, or the one that did has been erased.
// The two are the same answer on purpose: a command must not be able to tell
// "this person never existed" from "this person was deleted".
var ErrUserNotFound = errors.New("store: user not found")

// ErrLastAdmin means the change would leave the system with no active
// administrator. It is a sentinel because the API has to answer 409 for it,
// and the alternative - matching on the message text - is a contract nobody
// can see and the mock could never reproduce.
//
// erasure declares a sentinel of the same name for the same invariant seen
// from its side. The duplication is deliberate and is the second instance of
// this trade in the codebase, after UserOnCallError and MemberOnCallError: the
// erasure package knows nothing about the store, and one shared sentinel would
// buy a line of code at the price of an import that means nothing. If a third
// instance appears, that is the signal to give these invariants a home of
// their own rather than to keep paying.
var ErrLastAdmin = errors.New("store: cannot demote the last admin")

type Store struct {
	db *sql.DB

	// lockTimeout is how long a point mutation waits for a row somebody else
	// is holding. It is a field rather than the constant it is set from
	// because a test that measures the lock ORDER has to be able to raise it:
	// at three seconds, a loaded machine produces timeouts that say nothing
	// about who took which row first.
	lockTimeout time.Duration

	// render is what a message needs that an alert group does not carry: the
	// base URL of this installation and the zone times are printed in. Set once
	// at wiring - see SetRenderEnvironment.
	render renderEnvironment
}

func (s *Store) Close() error {
	return s.db.Close()
}

// GetDB returns the underlying sql.DB connection.
// STRICTLY FOR TESTING PURPOSES ONLY.
// Do not use this method in application code (api, engine, dispatcher).
// Always add new methods to Store interface instead of bypassing it.
func (s *Store) GetDB() *sql.DB {
	return s.db
}

func NewStore(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	return &Store{db: db, lockTimeout: outbound.NotificationLockTimeout}, nil
}

// legacyColumnMigrationsLock is what makes the block below safe for instances
// starting at the same moment.
//
// Every step in it is guarded, which is enough to run it twice in a row and not
// enough to run it twice at once: the guard and the ALTER it protects are two
// statements, and a rename committing between them leaves the other instance
// adding a column that now exists. One start fails, the container restarts, and
// on a bad day that is a crash loop over a schema that is already correct.
//
// The number is arbitrary and only has to stay stable and distinct from the
// other schema locks: "toka" plus the next sequence number.
const legacyColumnMigrationsLock int64 = 0x746F6B6113

// legacyColumnMigrationsDDL renames what older schemas called things.
//
// It runs after the tables are created, and every step is guarded, so a fresh
// database executes it and changes nothing. Never executed on its own - see
// applyLegacyColumnMigrations, which is what holds the lock.
const legacyColumnMigrationsDDL = `
	DO $$
	BEGIN
		-- 1. Rename incident_id to alert_group_id in timeline_events (legacy migration)
		IF EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='timeline_events' AND column_name='incident_id') THEN
			ALTER TABLE timeline_events RENAME COLUMN incident_id TO alert_group_id;
		END IF;
        
        -- 3. Add external_url column if not exists (for clickable Alertmanager links)
        IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='external_url') THEN
            ALTER TABLE alert_groups ADD COLUMN external_url TEXT;
        END IF;
        
        -- 4. Add notification_states column if not exists (for per-target retry tracking)
        IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='notification_states') THEN
            ALTER TABLE alert_groups ADD COLUMN notification_states TEXT;
        END IF;
        
        -- 5. Add auth_provider column to users table if not exists (OIDC support)
        IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='auth_provider') THEN
            ALTER TABLE users ADD COLUMN auth_provider TEXT;
        END IF;

		-- 6. Add FK for notification_deliveries.job_step_id if missing
		IF NOT EXISTS(
			SELECT 1 FROM information_schema.table_constraints
			WHERE table_name = 'notification_deliveries'
			  AND constraint_name = 'notification_deliveries_job_step_id_fkey'
		) THEN
			ALTER TABLE notification_deliveries
			ADD CONSTRAINT notification_deliveries_job_step_id_fkey
			FOREIGN KEY (job_step_id) REFERENCES job_steps(id) ON DELETE SET NULL;
		END IF;

		-- 7. Add resolved_by column
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='resolved_by') THEN
			ALTER TABLE alert_groups ADD COLUMN resolved_by TEXT;
		END IF;

		-- 8. Add ack_processed_at column for tracking when ack update was processed
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='ack_processed_at') THEN
			ALTER TABLE alert_groups ADD COLUMN ack_processed_at TIMESTAMPTZ;
		END IF;

		-- 8. Add slack_update_pending flag for tracking when Slack messages need updating
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='slack_update_pending') THEN
			ALTER TABLE alert_groups ADD COLUMN slack_update_pending BOOLEAN NOT NULL DEFAULT FALSE;
		END IF;

		-- 8. The version of the state a card is drawn from. Every write that
		-- changes what a message about this alert would say increments it.
		--
		-- Two things read it. A producer that froze a snapshot hands the
		-- version back when it admits the escalation, and the admission
		-- refuses a plan built from state that has moved since. And the
		-- slack_update_pending gate above clears the flag only for the version
		-- it read, so an alert arriving while the update job is created keeps
		-- the flag up instead of being cleared away with it.
		--
		-- It was called slack_update_generation, which was the second of those
		-- two jobs under the name of a loop that is on its way out.
		IF EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='slack_update_generation')
		   AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='render_source_version') THEN
			ALTER TABLE alert_groups RENAME COLUMN slack_update_generation TO render_source_version;
		END IF;
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='alert_groups' AND column_name='render_source_version') THEN
			ALTER TABLE alert_groups ADD COLUMN render_source_version BIGINT NOT NULL DEFAULT 0;
		END IF;

		-- 9. Add alert_group_id column to jobs table
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns
		              WHERE table_name='jobs' AND column_name='alert_group_id') THEN
			ALTER TABLE jobs ADD COLUMN alert_group_id TEXT;
		END IF;

		-- 9b. Filling alert_group_id and keeping one escalation row per group
		-- used to happen here, around a unique index this block dropped and
		-- recreated on every start. Both moved into the one-shot job dedup
		-- migration: recreating that index after the model exists would put
		-- back the rule the model replaced.
	END $$;
	`

// applyLegacyColumnMigrations brings an older schema onto the current names, in
// one transaction, on every start.
//
// The lock is taken first and the whole block runs inside it, so instances
// starting together queue rather than race: the second one re-reads the
// catalogue with the first one's work already committed, and its own statements
// become no-ops.
func (s *Store) applyLegacyColumnMigrations() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, legacyColumnMigrationsLock); err != nil {
		return fmt.Errorf("failed to take the legacy column migration lock: %w", err)
	}

	if _, err := tx.Exec(legacyColumnMigrationsDDL); err != nil {
		return fmt.Errorf("failed to migrate the legacy columns: %w", err)
	}

	return tx.Commit()
}

// schemaInitLock serialises the WHOLE start-up schema build, not only the
// guarded migrations inside it.
//
// `CREATE TABLE IF NOT EXISTS` is not a serialisation primitive: two backends
// running it at the same moment both find the table missing, and the loser
// fails on a duplicate key in `pg_type` rather than becoming a no-op. The same
// check-then-act gap sits in every guarded ALTER and every
// `CREATE INDEX IF NOT EXISTS` below. Every instance calls InitDB on start-up,
// so a rollout is exactly the moment several of them do this at once.
//
// The number is arbitrary and only has to stay stable and distinct from the
// other schema locks: "toka" plus the next sequence number.
const schemaInitLock int64 = 0x746F6B6114

// InitDB builds the schema this build expects, and holds the schema lock for as
// long as that takes.
//
// The lock lives in a transaction of its own that does nothing else: the build
// below spans many transactions - three of them take their own locks - and one
// statement cannot cover them all. An instance that waits here comes in after
// the winner has committed everything, and finds every statement a no-op.
//
// The inner locks stay. They are each function's own guarantee, and they cost
// nothing while this one is held: the order is always outer first, so there is
// no way round to invert.
func (s *Store) InitDB() error {
	guard, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer guard.Rollback()

	if _, err := guard.Exec(`SELECT pg_advisory_xact_lock($1)`, schemaInitLock); err != nil {
		return fmt.Errorf("failed to take the schema lock: %w", err)
	}

	if err := s.buildSchema(); err != nil {
		return err
	}

	return guard.Commit()
}

func (s *Store) buildSchema() error {
	// PostgreSQL Schema for Phase 2 - Create tables FIRST
	query := `
	-- Alert Groups table (renamed from incidents)
	CREATE TABLE IF NOT EXISTS alert_groups (
		id TEXT PRIMARY KEY,
		-- What the alerting system calls this alert: the group key Alertmanager
		-- sends, or the fingerprint of a single alert. It names the ALERT and
		-- not this row - the same key comes back every time the same thing
		-- breaks, and each time it is a new incident with a new id.
		--
		-- Not to be confused with jobs.dedup_key, which names a piece of
		-- background work. Confusing the two is how an escalation once came to
		-- be identified by the alert instead of by the incident.
		alert_key TEXT NOT NULL,
		status TEXT NOT NULL,
		title TEXT,
		
		team_id TEXT,
		severity TEXT,
		policy_id TEXT,
		current_step INTEGER DEFAULT 0,

		notification_states TEXT,
		external_url TEXT,
		alerts_data TEXT, 
		acknowledged_by TEXT,
		resolved_by TEXT,

		incident_id INTEGER,
		
		created_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ,
		resolved_at TIMESTAMPTZ,
		policy_snapshot JSONB,
		oncall_snapshot JSONB
	);

	-- Migration for existing tables: add policy_snapshot if not exists
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_groups' AND column_name = 'policy_snapshot') THEN
			ALTER TABLE alert_groups ADD COLUMN policy_snapshot JSONB;
		END IF;
		IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_groups' AND column_name = 'acknowledged_by') THEN
			ALTER TABLE alert_groups ADD COLUMN acknowledged_by TEXT;
		END IF;
		IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_groups' AND column_name = 'oncall_snapshot') THEN
			ALTER TABLE alert_groups ADD COLUMN oncall_snapshot JSONB;
		END IF;
		-- dedup_key -> alert_key. Two different identities were spelled the
		-- same way in one schema: the alert this group is about, and the work
		-- a job claims. The index below follows the column, so nothing else
		-- has to change here.
		IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_groups' AND column_name = 'dedup_key')
		   AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_groups' AND column_name = 'alert_key') THEN
			ALTER TABLE alert_groups RENAME COLUMN dedup_key TO alert_key;
		END IF;
	END $$;
	
	-- One live incident per alert: while a group is open, another one for the
	-- same alert key cannot be created, and once it is resolved or closed the
	-- key is free for the next time the same thing breaks. This index is that
	-- rule; nothing else states it.
	CREATE UNIQUE INDEX IF NOT EXISTS idx_active_alert_groups ON alert_groups(alert_key) WHERE status NOT IN ('resolved', 'closed');
	CREATE INDEX IF NOT EXISTS idx_alert_groups_status ON alert_groups(status);
	CREATE INDEX IF NOT EXISTS idx_alert_groups_team_id ON alert_groups(team_id);

	-- Teams table
	CREATE TABLE IF NOT EXISTS teams (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT,
		slack_channel TEXT,
		created_at TIMESTAMPTZ
	);

	-- Users table
	CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		email TEXT UNIQUE,
		name TEXT NOT NULL,
		password_hash TEXT,
		auth_provider TEXT,
		created_at TIMESTAMPTZ
	);

	-- External identities: user ↔ external account (Slack, Telegram, ...)
	CREATE TABLE IF NOT EXISTS external_identities (
		id           TEXT PRIMARY KEY,
		user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		provider     TEXT NOT NULL,
		external_id  TEXT NOT NULL,
		chat_id      TEXT,
		display_name TEXT,
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		updated_at   TIMESTAMPTZ DEFAULT NOW(),
		UNIQUE (user_id, provider)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_external_identities_provider_external
		ON external_identities(provider, external_id);

	-- Generic link tokens: issue/consume/TTL/single-use for linking external accounts.
	-- Used by Slack OTP today (token = 6-digit code) and Telegram deep-link in Epic 8.
	CREATE TABLE IF NOT EXISTS link_tokens (
		id           TEXT PRIMARY KEY,
		user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		provider     TEXT NOT NULL,
		token_hash   TEXT NOT NULL,
		external_id  TEXT,
		attempts     INTEGER DEFAULT 0,
		expires_at   TIMESTAMPTZ NOT NULL,
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		UNIQUE (user_id, provider),
		UNIQUE (provider, token_hash)
	);

	-- Team Members (many-to-many)
	CREATE TABLE IF NOT EXISTS team_members (
		team_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		role TEXT DEFAULT 'team_member',
		PRIMARY KEY (team_id, user_id),
		FOREIGN KEY (team_id) REFERENCES teams(id),
		FOREIGN KEY (user_id) REFERENCES users(id)
	);

	-- Timeline Events table
	CREATE TABLE IF NOT EXISTS timeline_events (
		id TEXT PRIMARY KEY,
		alert_group_id TEXT NOT NULL,
		type TEXT NOT NULL,
		message TEXT NOT NULL,
		actor TEXT DEFAULT 'system',
		metadata TEXT DEFAULT '{}',
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (alert_group_id) REFERENCES alert_groups(id)
	);
	CREATE INDEX IF NOT EXISTS idx_timeline_alert_group ON timeline_events(alert_group_id);

	-- Incidents table (stub for Phase 2, full implementation in Phase 3+)
	CREATE TABLE IF NOT EXISTS incidents (
		id SERIAL PRIMARY KEY,
		title TEXT NOT NULL,
		status TEXT DEFAULT 'investigating',
		severity TEXT,
		commander_id TEXT,
		slack_channel_id TEXT,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);

	-- Jobs (Phase 2)
	CREATE TABLE IF NOT EXISTS jobs (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		status TEXT NOT NULL,
		payload TEXT NOT NULL DEFAULT '{}',
		-- Identity and policy: (dedup_namespace, dedup_key) names the work,
		-- dedup_scope says how long that name is exclusive. All three or none;
		-- the uniqueness rules themselves live with the migration that
		-- introduces them (job_dedup_migration.go).
		dedup_namespace TEXT,
		dedup_key TEXT,
		dedup_scope TEXT,
		alert_group_id TEXT,
		current_stage INTEGER DEFAULT 0,
		error TEXT,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		finished_at TIMESTAMPTZ,
		canceled_at TIMESTAMPTZ
	);

	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

	-- Job Stages (Phase 2: sequential execution groups within a job)
	CREATE TABLE IF NOT EXISTS job_stages (
		id TEXT PRIMARY KEY,
		job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		stage_index INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'blocked',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		UNIQUE(job_id, stage_index)
	);
	CREATE INDEX IF NOT EXISTS idx_job_stages_job_status ON job_stages(job_id, status);

	-- Job Steps (Phase 2)
	CREATE TABLE IF NOT EXISTS job_steps (
		id TEXT PRIMARY KEY,
		job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
		stage_id TEXT NOT NULL REFERENCES job_stages(id),
		step_index INTEGER NOT NULL,
		step_type TEXT NOT NULL,
		status TEXT NOT NULL,
		data TEXT NOT NULL DEFAULT '{}',
		result TEXT,
		error TEXT,
		next_run_at TIMESTAMPTZ,
		locked_until TIMESTAMPTZ,
		locked_by TEXT,
		attempt_count INTEGER DEFAULT 0,
		timeout_seconds INTEGER,
		max_attempts INTEGER DEFAULT 5,
		continue_on_failure BOOLEAN DEFAULT FALSE,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);
	
	CREATE INDEX IF NOT EXISTS idx_job_steps_status_next_run ON job_steps(status, next_run_at) 
	WHERE status IN ('pending', 'retry');
	CREATE INDEX IF NOT EXISTS idx_job_steps_job_id_index ON job_steps(job_id, step_index);

	-- Notification Deliveries (Phase 3+)
	CREATE TABLE IF NOT EXISTS notification_deliveries (
		id TEXT PRIMARY KEY,
		alert_group_id TEXT NOT NULL REFERENCES alert_groups(id),
		job_step_id TEXT UNIQUE REFERENCES job_steps(id) ON DELETE SET NULL,
		provider TEXT NOT NULL,
		kind TEXT NOT NULL,
		target_type TEXT,
		target_id TEXT,
		provider_payload TEXT,
		supports_update BOOLEAN DEFAULT FALSE,
		is_primary BOOLEAN DEFAULT FALSE,
		is_firehose BOOLEAN DEFAULT FALSE,
		attempt INTEGER DEFAULT 0,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_notification_deliveries_alert_group ON notification_deliveries(alert_group_id);
	CREATE INDEX IF NOT EXISTS idx_notification_deliveries_primary ON notification_deliveries(alert_group_id, is_primary);
	CREATE INDEX IF NOT EXISTS idx_notification_deliveries_firehose ON notification_deliveries(alert_group_id, is_firehose);
	`
	if _, err := s.db.Exec(query); err != nil {
		return err
	}

	// Migration Phase: Rename legacy columns/tables if they exist
	// This runs AFTER table creation to handle upgrades from older schemas
	if err := s.applyLegacyColumnMigrations(); err != nil {
		return err
	}

	// Job dedup model: policy table, one-shot migration and the sweep for
	// legacy indexes, in that order and under one lock.
	if err := s.applyJobDedupModel(); err != nil {
		return err
	}

	// team_name_snapshot migration: add column, backfill from teams, fallback orphans, enforce NOT NULL
	snapshotMigration := `
	ALTER TABLE alert_groups ADD COLUMN IF NOT EXISTS team_name_snapshot TEXT;

	-- Backfill from teams for known team_ids
	UPDATE alert_groups SET team_name_snapshot = t.name
	  FROM teams t WHERE alert_groups.team_id = t.id
	  AND (alert_groups.team_name_snapshot IS NULL OR alert_groups.team_name_snapshot = '');

	-- Fallback for orphaned team_ids (deleted teams): use team_id as snapshot
	UPDATE alert_groups SET team_name_snapshot = team_id
	  WHERE team_name_snapshot IS NULL OR team_name_snapshot = '';

	ALTER TABLE alert_groups ALTER COLUMN team_name_snapshot SET NOT NULL;
	`
	if _, err := s.db.Exec(snapshotMigration); err != nil {
		return err
	}

	// Job stages migration: create table, backfill stage_id, rename current_step
	jobStagesMigration := `
	DO $$
	BEGIN
		-- 1. Create job_stages table if not exists (for upgrades; fresh installs already have it)
		CREATE TABLE IF NOT EXISTS job_stages (
			id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
			stage_index INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'blocked',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(job_id, stage_index)
		);
		CREATE INDEX IF NOT EXISTS idx_job_stages_job_status ON job_stages(job_id, status);

		-- 2. Add stage_id column to job_steps (nullable initially for migration)
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns
		              WHERE table_name='job_steps' AND column_name='stage_id') THEN
			ALTER TABLE job_steps ADD COLUMN stage_id TEXT REFERENCES job_stages(id);
		END IF;

		-- 3. Migrate existing steps: each step gets its own stage
		INSERT INTO job_stages (id, job_id, stage_index, status, created_at, updated_at)
		SELECT
			'stage-' || js.id,
			js.job_id,
			js.step_index,
			CASE
				WHEN js.status IN ('succeeded', 'failed', 'canceled') THEN js.status
				WHEN js.status = 'blocked' THEN 'blocked'
				ELSE 'active'
			END,
			js.created_at,
			js.updated_at
		FROM job_steps js
		ON CONFLICT (job_id, stage_index) DO NOTHING;

		-- 4. Backfill stage_id on job_steps
		UPDATE job_steps js SET stage_id = (
			SELECT jst.id FROM job_stages jst
			WHERE jst.job_id = js.job_id AND jst.stage_index = js.step_index
		)
		WHERE js.stage_id IS NULL;

		-- 5. Enforce NOT NULL if all rows migrated
		IF NOT EXISTS (SELECT 1 FROM job_steps WHERE stage_id IS NULL) THEN
			IF EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name='job_steps' AND column_name='stage_id' AND is_nullable='YES'
			) THEN
				ALTER TABLE job_steps ALTER COLUMN stage_id SET NOT NULL;
			END IF;
		END IF;

		-- 6. Rename jobs.current_step -> current_stage (upgrade path)
		IF EXISTS(SELECT 1 FROM information_schema.columns
		          WHERE table_name='jobs' AND column_name='current_step') THEN
			ALTER TABLE jobs RENAME COLUMN current_step TO current_stage;
		END IF;
	END $$;
	`
	if _, err := s.db.Exec(jobStagesMigration); err != nil {
		return err
	}

	// Schedule revision history: the aggregate root, its append-only
	// configuration snapshots, and the override history beside them.
	//
	// The root is created in its final shape, with the history horizon NOT NULL:
	// a row without one could only come from before the revision model, and the
	// destructive upgrade removes those rather than carrying them forward.
	//
	// The three ALTERs below add the columns to a database that predates them,
	// and are no-ops on one this statement created. They add NULLABLE and
	// tighten nothing: a row without a history horizon could only come from
	// before the revision model, and databases in that shape stopped being
	// upgradable when the cutover code was removed - such a database has to be
	// brought forward on an older release first.
	//
	// All range constraints use tstzrange over TIMESTAMPTZ columns.
	revisionQuery := `
	CREATE TABLE IF NOT EXISTS schedules (
		id                    TEXT PRIMARY KEY,
		team_id               TEXT UNIQUE NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
		config_version        BIGINT NOT NULL DEFAULT 0,
		history_complete_from TIMESTAMPTZ NOT NULL,
		deleted_at            TIMESTAMPTZ,
		created_at            TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		updated_at            TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);

	ALTER TABLE schedules ADD COLUMN IF NOT EXISTS config_version BIGINT NOT NULL DEFAULT 0;
	ALTER TABLE schedules ADD COLUMN IF NOT EXISTS history_complete_from TIMESTAMPTZ;
	ALTER TABLE schedules ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

	ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

	-- Required by the schedule_revisions exclusion constraint below. It used to
	-- be created by the pre-revision schema; that file is gone, and without this
	-- line the constraint would silently not be created on a fresh database -
	-- the constraint swallows a missing extension with a NOTICE.
	DO $$ BEGIN
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	EXCEPTION WHEN insufficient_privilege THEN
		RAISE NOTICE 'btree_gist extension not created (needs superuser)';
	END $$;

	-- ON DELETE RESTRICT: a schedule with history is never physically deleted
	-- (soft delete via schedules.deleted_at), so cascading the history away
	-- must be impossible at the schema level too.
	--
	-- kind makes the interval in which a schedule was deleted a record of its
	-- own rather than a hole. Recreating a schedule clears deleted_at, so a
	-- hole is all that would remain of a normal delete/recreate cycle and a
	-- reader could not tell it from a lost revision.
	CREATE TABLE IF NOT EXISTS schedule_revisions (
		id             TEXT PRIMARY KEY,
		schedule_id    TEXT NOT NULL REFERENCES schedules(id) ON DELETE RESTRICT,
		version        BIGINT NOT NULL,
		kind           TEXT NOT NULL DEFAULT 'active',
		snapshot       JSONB NOT NULL,
		effective_from TIMESTAMPTZ NOT NULL,
		effective_to   TIMESTAMPTZ,
		recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		created_by     TEXT,
		change_reason  TEXT,
		change_summary JSONB,

		UNIQUE (schedule_id, version),
		CONSTRAINT schedule_revisions_version_positive CHECK (version >= 1),
		CONSTRAINT schedule_revisions_kind_known CHECK (kind IN ('active', 'deleted')),
		CHECK (effective_to IS NULL OR effective_to > effective_from)
	);
	ALTER TABLE schedule_revisions ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'active';
	DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'schedule_revisions_version_positive') THEN
			ALTER TABLE schedule_revisions ADD CONSTRAINT schedule_revisions_version_positive CHECK (version >= 1);
		END IF;
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'schedule_revisions_kind_known') THEN
			ALTER TABLE schedule_revisions ADD CONSTRAINT schedule_revisions_kind_known
				CHECK (kind IN ('active', 'deleted'));
		END IF;
	END $$;
	CREATE UNIQUE INDEX IF NOT EXISTS idx_schedule_revisions_one_tail
		ON schedule_revisions(schedule_id)
		WHERE effective_to IS NULL;
	CREATE INDEX IF NOT EXISTS idx_schedule_revisions_range
		ON schedule_revisions(schedule_id, effective_from);

	-- Defence in depth only: non-overlap is guaranteed by the schedule row
	-- lock and a single transaction (an empty history has no revision row to
	-- lock). Skipped when btree_gist is unavailable.
	-- Qualified by table and kind, like the startup check that reports on it:
	-- constraint names are unique per table, so a same-named constraint on
	-- another table would make this skip creating the real one.
	DO $$ BEGIN
		IF NOT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conname = 'no_overlapping_schedule_revisions'
			  AND conrelid = to_regclass('schedule_revisions')
			  AND contype = 'x'
		) THEN
			ALTER TABLE schedule_revisions ADD CONSTRAINT no_overlapping_schedule_revisions
				EXCLUDE USING gist (schedule_id WITH =, tstzrange(effective_from, effective_to, '[)') WITH &&);
		END IF;
	EXCEPTION WHEN undefined_function OR undefined_object THEN
		RAISE NOTICE 'schedule_revisions exclusion constraint not created (btree_gist not available)';
	END $$;

	-- Append-only override history. Deliberately without an exclusion
	-- constraint: overlap is a property of the CURRENT projection (latest
	-- revision per override_id, tombstones excluded), which no per-row
	-- constraint can express. The service validates it under the schedule
	-- lock. user_id/recorded_by carry no FK so history survives user deletion.
	CREATE TABLE IF NOT EXISTS schedule_override_revisions (
		revision_id TEXT PRIMARY KEY,
		override_id TEXT NOT NULL,
		schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE RESTRICT,
		revision    BIGINT NOT NULL,
		layer       TEXT NOT NULL DEFAULT 'l1' CHECK (layer IN ('l1', 'l2')),
		user_id     TEXT NOT NULL,
		valid_from  TIMESTAMPTZ NOT NULL,
		valid_to    TIMESTAMPTZ NOT NULL,
		reason      TEXT,
		deleted     BOOLEAN NOT NULL DEFAULT FALSE,
		recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		recorded_by TEXT,

		UNIQUE (override_id, revision),
		CONSTRAINT schedule_override_revisions_revision_positive CHECK (revision >= 1),
		CHECK (valid_to > valid_from)
	);
	DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'schedule_override_revisions_revision_positive') THEN
			ALTER TABLE schedule_override_revisions
				ADD CONSTRAINT schedule_override_revisions_revision_positive CHECK (revision >= 1);
		END IF;
	END $$;
	CREATE INDEX IF NOT EXISTS idx_schedule_override_revisions_range
		ON schedule_override_revisions(schedule_id, valid_from);

	-- Markers for one-shot operational migrations that must never run twice.
	CREATE TABLE IF NOT EXISTS migration_markers (
		name       TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	`
	if _, err := s.db.Exec(revisionQuery); err != nil {
		return err
	}

	// API Tokens table (for automation access)
	apiTokensQuery := `
	CREATE TABLE IF NOT EXISTS api_tokens (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name TEXT NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		expires_at TIMESTAMPTZ,
		last_used_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id);
	CREATE INDEX IF NOT EXISTS idx_api_tokens_hash ON api_tokens(token_hash);
	`
	if _, err := s.db.Exec(apiTokensQuery); err != nil {
		return err
	}

	// (Slack OTP codes and users.slack_user_id index were removed in Epic 7 Sprint 3;
	// see external_identities + link_tokens above.)

	// RBAC migrations
	rbacQuery := `
	DO $$
	BEGIN
		-- 1. Add role column to users table
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='users' AND column_name='role') THEN
			ALTER TABLE users ADD COLUMN role TEXT DEFAULT 'user';
		END IF;

		-- 1b. Backfill NULL roles to 'user'
		UPDATE users SET role = 'user' WHERE role IS NULL;

		-- 2. Migrate team_members roles (admin -> team_admin, member -> team_member)
		UPDATE team_members SET role = 'team_admin' WHERE role = 'admin';
		UPDATE team_members SET role = 'team_member' WHERE role = 'member';
		
		-- 3. Update team_members default for existing tables
		ALTER TABLE team_members ALTER COLUMN role SET DEFAULT 'team_member';

		-- 4. Create triage team if not exists (for unassigned alerts)
		INSERT INTO teams (id, name, description, created_at)
		VALUES ('triage', 'Triage', 'Default team for unassigned alerts', NOW())
		ON CONFLICT (id) DO NOTHING;

		-- 5. Backfill alert_groups.team_id with triage team
		UPDATE alert_groups SET team_id = 'triage' WHERE team_id IS NULL OR team_id = '';

		-- 6. Bootstrap first user as admin if no admin exists
		UPDATE users SET role = 'admin' 
		WHERE id = (SELECT id FROM users ORDER BY created_at LIMIT 1)
		AND NOT EXISTS (SELECT 1 FROM users WHERE role = 'admin');
	END $$;
	
	-- 7. Add NOT NULL constraint to alert_groups.team_id (separate statement)
	DO $$
	BEGIN
		-- Check if column is already NOT NULL
		IF EXISTS (
			SELECT 1 FROM information_schema.columns 
			WHERE table_name = 'alert_groups' 
			AND column_name = 'team_id' 
			AND is_nullable = 'YES'
		) THEN
			ALTER TABLE alert_groups ALTER COLUMN team_id SET NOT NULL;
		END IF;
	END $$;
	`
	if _, err := s.db.Exec(rbacQuery); err != nil {
		return err
	}

	// Phase 4: Escalation Policies tables
	policyQuery := `
	-- Escalation Policies table
	CREATE TABLE IF NOT EXISTS escalation_policies (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		description TEXT,
		team_id TEXT REFERENCES teams(id) ON DELETE SET NULL,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);

	-- Escalation Steps table
	--
	-- Sprint 4 (Epic 7 L6): step shape is (provider, target_kind) instead of
	-- the old flat step_type. Provider validity is enforced at the API layer
	-- against the dispatcher's capability registry — keeping it as TEXT here
	-- avoids re-encoding the provider catalog in the DB.
	CREATE TABLE IF NOT EXISTS escalation_steps (
		id TEXT PRIMARY KEY,
		policy_id TEXT NOT NULL REFERENCES escalation_policies(id) ON DELETE CASCADE,
		step_index INTEGER NOT NULL CHECK (step_index >= 0),
		provider TEXT NOT NULL,
		target_kind TEXT NOT NULL CHECK (target_kind IN ('dm', 'channel')),
		target_type TEXT NOT NULL CHECK (target_type IN ('user', 'channel', 'schedule')),
		target_id TEXT NOT NULL,
		delay_seconds INTEGER DEFAULT 0 CHECK (delay_seconds >= 0),
		timeout_seconds INTEGER DEFAULT 30 CHECK (timeout_seconds > 0),
		max_attempts INTEGER DEFAULT 5 CHECK (max_attempts >= 1),
		UNIQUE(policy_id, step_index)
	);
	CREATE INDEX IF NOT EXISTS idx_escalation_steps_policy_id ON escalation_steps(policy_id);

	-- Team routing columns migration
	DO $$
	BEGIN
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='teams' AND column_name='default_policy_id') THEN
			ALTER TABLE teams ADD COLUMN default_policy_id TEXT;
		END IF;
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='teams' AND column_name='severity_routes') THEN
			ALTER TABLE teams ADD COLUMN severity_routes JSONB DEFAULT '{}';
		END IF;

		-- 3. Add message column to escalation_steps
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='escalation_steps' AND column_name='message') THEN
			ALTER TABLE escalation_steps ADD COLUMN message TEXT;
		END IF;

		-- 4. Add continue_on_failure column to escalation_steps (default TRUE for fail-forward behavior)
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='escalation_steps' AND column_name='continue_on_failure') THEN
			ALTER TABLE escalation_steps ADD COLUMN continue_on_failure BOOLEAN DEFAULT TRUE;
		END IF;

		-- 5. Epic 7 Sprint 4: replace step_type with (provider, target_kind).
		--    CREATE TABLE IF NOT EXISTS above is a no-op on existing DBs, so on
		--    an upgrade the old step_type column will still be there and the
		--    new selects/inserts will fail. Per project policy (no backward
		--    compat) we DROP the old column and ADD the two new ones in
		--    place; pre-existing rows are backfilled from the well-known
		--    legacy values (slack_dm / slack_channel -> slack + dm/channel).
		IF EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='escalation_steps' AND column_name='step_type')
		   AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='escalation_steps' AND column_name='provider') THEN
			ALTER TABLE escalation_steps ADD COLUMN provider TEXT;
			ALTER TABLE escalation_steps ADD COLUMN target_kind TEXT;
			UPDATE escalation_steps SET provider = 'slack',
				target_kind = CASE step_type
					WHEN 'slack_dm' THEN 'dm'
					WHEN 'slack_channel' THEN 'channel'
					ELSE 'dm'
				END;
			ALTER TABLE escalation_steps ALTER COLUMN provider SET NOT NULL;
			ALTER TABLE escalation_steps ALTER COLUMN target_kind SET NOT NULL;
			ALTER TABLE escalation_steps ADD CONSTRAINT escalation_steps_target_kind_check
				CHECK (target_kind IN ('dm', 'channel'));
			ALTER TABLE escalation_steps DROP COLUMN step_type;
		END IF;
	END $$;
	`
	if _, err := s.db.Exec(policyQuery); err != nil {
		return err
	}

	// Phase 5: Integrations table
	integrationsQuery := `
	CREATE TABLE IF NOT EXISTS integrations (
		id TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		direction TEXT NOT NULL,
		name TEXT NOT NULL,
		enabled BOOLEAN DEFAULT true,
		config TEXT NOT NULL,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW()
	);

	-- Partial unique: only one outbound integration per type (legacy, will be rebuilt below)
	CREATE UNIQUE INDEX IF NOT EXISTS idx_integrations_type_outbound
		ON integrations (type) WHERE direction = 'outbound';
	`
	if _, err := s.db.Exec(integrationsQuery); err != nil {
		return err
	}

	// Phase 5b: Add webhook scope columns to integrations
	webhookScopeQuery := `
	ALTER TABLE integrations ADD COLUMN IF NOT EXISTS scope TEXT;
	ALTER TABLE integrations ADD COLUMN IF NOT EXISTS team_id TEXT REFERENCES teams(id);

	-- Rebuild unique index: exclude generic_webhook from single-outbound constraint
	DROP INDEX IF EXISTS idx_integrations_type_outbound;
	CREATE UNIQUE INDEX IF NOT EXISTS idx_integrations_type_outbound
		ON integrations (type) WHERE direction = 'outbound' AND type <> 'generic_webhook';

	-- CHECK: generic_webhook requires scope; others must have scope=NULL
	ALTER TABLE integrations DROP CONSTRAINT IF EXISTS chk_webhook_scope;
	ALTER TABLE integrations ADD CONSTRAINT chk_webhook_scope CHECK (
		(type = 'generic_webhook' AND scope IN ('global', 'team'))
		OR (type <> 'generic_webhook' AND scope IS NULL)
	);

	-- CHECK: team scope requires team_id; non-team forbids team_id
	ALTER TABLE integrations DROP CONSTRAINT IF EXISTS chk_webhook_team_id;
	ALTER TABLE integrations ADD CONSTRAINT chk_webhook_team_id CHECK (
		(scope = 'team' AND team_id IS NOT NULL)
		OR (scope IS DISTINCT FROM 'team' AND team_id IS NULL)
	);
	`
	if _, err := s.db.Exec(webhookScopeQuery); err != nil {
		return err
	}

	// Phase 7: Outbox tables for transactional event delivery
	outboxQuery := `
	CREATE TABLE IF NOT EXISTS event_outbox (
		id TEXT PRIMARY KEY,
		event_type TEXT NOT NULL,
		alert_group_id TEXT NOT NULL REFERENCES alert_groups(id),
		team_id TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT 'system',
		payload JSONB NOT NULL DEFAULT '{}',
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INT NOT NULL DEFAULT 0,
		next_attempt_at TIMESTAMPTZ,
		locked_until TIMESTAMPTZ,
		locked_by TEXT,
		last_error TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		sent_at TIMESTAMPTZ
	);
	CREATE INDEX IF NOT EXISTS idx_outbox_claim
		ON event_outbox (next_attempt_at)
		WHERE status IN ('pending', 'processing');
	CREATE INDEX IF NOT EXISTS idx_outbox_alert_group
		ON event_outbox (alert_group_id);

	CREATE TABLE IF NOT EXISTS event_outbox_deliveries (
		id TEXT PRIMARY KEY,
		event_id TEXT NOT NULL REFERENCES event_outbox(id),
		integration_id TEXT NOT NULL REFERENCES integrations(id),
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INT NOT NULL DEFAULT 0,
		next_attempt_at TIMESTAMPTZ,
		last_http_status INT,
		last_error TEXT,
		request_payload TEXT,
		response_body_trunc TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		sent_at TIMESTAMPTZ,
		UNIQUE(event_id, integration_id)
	);
	CREATE INDEX IF NOT EXISTS idx_delivery_retry
		ON event_outbox_deliveries (next_attempt_at)
		WHERE status IN ('pending', 'retry');
	CREATE INDEX IF NOT EXISTS idx_delivery_event
		ON event_outbox_deliveries (event_id);
	`
	if _, err := s.db.Exec(outboxQuery); err != nil {
		return err
	}

	// Delivery attempts audit log (append-only)
	deliveryAttemptsQuery := `
	CREATE TABLE IF NOT EXISTS event_outbox_delivery_attempts (
		id TEXT PRIMARY KEY,
		delivery_id TEXT NOT NULL REFERENCES event_outbox_deliveries(id),
		attempt INT NOT NULL,
		http_status INT,
		error TEXT,
		response_body_trunc TEXT,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_delivery_attempts_delivery
		ON event_outbox_delivery_attempts (delivery_id);
	`
	if _, err := s.db.Exec(deliveryAttemptsQuery); err != nil {
		return err
	}

	// Drop FK on event_outbox.team_id — unknown teams are a valid runtime case
	// (firehose-only alerts) and global webhook subscriptions must still fan-out.
	dropOutboxTeamFK := `ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_team_id_fkey`
	if _, err := s.db.Exec(dropOutboxTeamFK); err != nil {
		return err
	}

	// Outbound delivery: the tables an outgoing commitment lives in. Last,
	// because two of them reference alert_groups, and under their own lock -
	// see outbound_schema.go.
	if err := s.applyOutboundSchema(); err != nil {
		return err
	}

	return nil
}

// ========================================
// Alert Group CRUD (renamed from Incident)
// ========================================

// alertGroupColumns is the standard SELECT clause for alert_groups.
// All query functions should use this to ensure consistent column ordering.
const alertGroupColumns = `id, alert_key, status, title, team_id, team_name_snapshot, severity, policy_id, current_step,
	external_url, alerts_data, policy_snapshot, oncall_snapshot,
	created_at, updated_at, resolved_at, acknowledged_by, resolved_by, ack_processed_at,
	slack_update_pending, render_source_version`

// alertGroupScanner is an interface for scanning rows (works with *sql.Row and *sql.Rows).
type alertGroupScanner interface {
	Scan(dest ...any) error
}

// scanAlertGroupRow scans a row into an AlertGroup struct.
// The row must contain columns in the order defined by alertGroupColumns.
func scanAlertGroupRow(scanner alertGroupScanner) (*model.AlertGroup, error) {
	var ag model.AlertGroup
	var resolvedAt, ackProcessedAt sql.NullTime
	var teamID, teamNameSnapshot, severity, policyID, externalURL sql.NullString
	var alertsData, policySnapshot, oncallSnapshot, acknowledgedBy, resolvedBy sql.NullString

	err := scanner.Scan(
		&ag.ID, &ag.AlertKey, &ag.Status, &ag.Title,
		&teamID, &teamNameSnapshot, &severity, &policyID, &ag.CurrentStep,
		&externalURL, &alertsData,
		&policySnapshot, &oncallSnapshot,
		&ag.CreatedAt, &ag.UpdatedAt, &resolvedAt, &acknowledgedBy, &resolvedBy, &ackProcessedAt,
		&ag.SlackUpdatePending, &ag.RenderSourceVersion,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	// Populate from nullable fields
	ag.TeamID = teamID.String
	ag.TeamNameSnapshot = teamNameSnapshot.String
	ag.Severity = severity.String
	ag.PolicyID = policyID.String
	ag.ExternalURL = externalURL.String
	ag.AcknowledgedBy = acknowledgedBy.String
	ag.ResolvedBy = resolvedBy.String

	if resolvedAt.Valid {
		ag.ResolvedAt = &resolvedAt.Time
	}
	if ackProcessedAt.Valid {
		ag.AckProcessedAt = &ackProcessedAt.Time
	}

	// The alerts ARE the alert group: they are what a message about it says,
	// and what an escalation is planned from. A row whose alerts cannot be read
	// is refused rather than returned as a group with none - swallowed, the
	// producer would freeze a snapshot describing an alert with nothing wrong
	// with it, once, for every message of that escalation.
	//
	// The two snapshots below are annotations about the group rather than the
	// group, and a damaged one must not hide the alert it is attached to.
	if alertsData.Valid && alertsData.String != "" {
		if err := json.Unmarshal([]byte(alertsData.String), &ag.Alerts); err != nil {
			// Counted as well as returned. The refusal can stop the admission
			// scan until somebody fixes the row, and a risk taken deliberately
			// has to be visible before an operator notices the silence.
			metrics.StorageContractFailuresTotal.WithLabelValues("alerts_data").Inc()
			return nil, fmt.Errorf("read the alerts of group %s: %w", ag.ID, err)
		}
	}
	if policySnapshot.Valid && policySnapshot.String != "" {
		_ = json.Unmarshal([]byte(policySnapshot.String), &ag.PolicySnapshot)
	}
	if oncallSnapshot.Valid && oncallSnapshot.String != "" {
		_ = json.Unmarshal([]byte(oncallSnapshot.String), &ag.OnCallSnapshot)
	}

	return &ag, nil
}

// GetActiveAlertGroupByAlertKey finds the incident currently open for an alert,
// if there is one. A resolved or closed group is not it: the same alert firing
// again is the next incident, not this one.
func (s *Store) GetActiveAlertGroupByAlertKey(alertKey string) (*model.AlertGroup, error) {
	query := `SELECT ` + alertGroupColumns + `
			  FROM alert_groups
			  WHERE alert_key = $1 AND status NOT IN ($2, $3)`

	row := s.db.QueryRow(query, alertKey, model.AlertGroupStatusResolved, model.AlertGroupStatusClosed)
	ag, err := scanAlertGroupRow(row)
	if err != nil {
		return nil, err
	}
	if ag == nil {
		return nil, sql.ErrNoRows
	}
	return ag, nil
}

func (s *Store) CreateAlertGroup(ag *model.AlertGroup) error {
	alertsJson, _ := json.Marshal(ag.Alerts)

	// Policy snapshot is likely nil on creation, but handle it anyway
	var snapshotJson []byte
	if ag.PolicySnapshot != nil {
		snapshotJson, _ = json.Marshal(ag.PolicySnapshot)
	}

	query := `INSERT INTO alert_groups (id, alert_key, status, title, team_id, team_name_snapshot, severity, policy_id, current_step, external_url, alerts_data, policy_snapshot, acknowledged_by, resolved_by, created_at, updated_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`

	var snapshotVal sql.NullString
	if len(snapshotJson) > 0 {
		snapshotVal = sql.NullString{String: string(snapshotJson), Valid: true}
	}

	_, err := s.db.Exec(query, ag.ID, ag.AlertKey, ag.Status, ag.Title, ag.TeamID, ag.TeamNameSnapshot, ag.Severity, ag.PolicyID, ag.CurrentStep, ag.ExternalURL, string(alertsJson), snapshotVal, ag.AcknowledgedBy, ag.ResolvedBy, ag.CreatedAt, ag.UpdatedAt)
	return err
}

// CreateAlertGroupAtomic atomically creates an alert group with timeline events and an outbox event.
// All inserts happen in a single transaction so no data is lost on crash.
func (s *Store) CreateAlertGroupAtomic(ag *model.AlertGroup, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. INSERT alert_group
	alertsJson, _ := json.Marshal(ag.Alerts)
	var snapshotJson []byte
	if ag.PolicySnapshot != nil {
		snapshotJson, _ = json.Marshal(ag.PolicySnapshot)
	}
	var snapshotVal sql.NullString
	if len(snapshotJson) > 0 {
		snapshotVal = sql.NullString{String: string(snapshotJson), Valid: true}
	}
	_, err = tx.Exec(
		`INSERT INTO alert_groups (id, alert_key, status, title, team_id, team_name_snapshot, severity, policy_id, current_step, external_url, alerts_data, policy_snapshot, acknowledged_by, resolved_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		ag.ID, ag.AlertKey, ag.Status, ag.Title, ag.TeamID, ag.TeamNameSnapshot, ag.Severity, ag.PolicyID, ag.CurrentStep,
		ag.ExternalURL, string(alertsJson), snapshotVal,
		ag.AcknowledgedBy, ag.ResolvedBy, ag.CreatedAt, ag.UpdatedAt,
	)
	if err != nil {
		return err
	}

	// 2. INSERT timeline events
	for _, e := range timelineEvents {
		metaJSON := "{}"
		if e.Metadata != nil {
			if b, err := json.Marshal(e.Metadata); err == nil {
				metaJSON = string(b)
			}
		}
		_, err = tx.Exec(
			`INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.ID, e.AlertGroupID, e.Type, e.Message, e.Actor, metaJSON, e.CreatedAt,
		)
		if err != nil {
			return err
		}
	}

	// 3. INSERT outbox event
	if err := insertOutboxEventTx(tx, outboxEvent); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) UpdateAlertGroupPolicy(id string, policyID string, snapshot *model.EscalationPolicySnapshot) error {
	var snapshotVal sql.NullString
	if snapshot != nil {
		data, _ := json.Marshal(snapshot)
		snapshotVal = sql.NullString{String: string(data), Valid: true}
	}
	query := `UPDATE alert_groups SET policy_id = $1, policy_snapshot = $2, updated_at = $3 WHERE id = $4`
	_, err := s.db.Exec(query, policyID, snapshotVal, time.Now(), id)
	return err
}

func (s *Store) UpdateAlertGroupOnCall(id string, snapshot *model.OnCallResult) error {
	var snapshotVal sql.NullString
	if snapshot != nil {
		data, _ := json.Marshal(snapshot)
		snapshotVal = sql.NullString{String: string(data), Valid: true}
	}
	query := `UPDATE alert_groups SET oncall_snapshot = $1, updated_at = $2 WHERE id = $3`
	_, err := s.db.Exec(query, snapshotVal, time.Now(), id)
	return err
}

func (s *Store) TouchAlertGroup(id string) error {
	query := `UPDATE alert_groups SET updated_at = $1 WHERE id = $2`
	_, err := s.db.Exec(query, time.Now(), id)
	return err
}

// insertOutboxEventTx inserts an outbox event within a transaction, normalizing defaults.
// If event is nil, it's a no-op.
func insertOutboxEventTx(tx *sql.Tx, event *model.OutboxEvent) error {
	if event == nil {
		return nil
	}
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.Status == "" {
		event.Status = model.OutboxEventStatusPending
	}
	if event.Actor == "" {
		event.Actor = "system"
	}
	if event.Payload == nil {
		event.Payload = json.RawMessage("{}")
	}
	event.CreatedAt = time.Now()

	_, err := tx.Exec(
		`INSERT INTO event_outbox (id, event_type, alert_group_id, team_id, actor, payload, status, attempts, next_attempt_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		event.ID, event.EventType, event.AlertGroupID, event.TeamID,
		event.Actor, event.Payload, event.Status, event.Attempts,
		event.NextAttemptAt, event.CreatedAt,
	)
	return err
}

// AckAlertGroupAtomic atomically acknowledges an alert group.
// Timeline event and status update happen in a single transaction.
// Returns (true, nil) if the ack was applied, (false, nil) if the alert group
// was not in 'processing' or 'triggered' status (idempotent/race loser), or (false, err) on failure.
func (s *Store) AckAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// One instant for everything this transaction writes, and it comes from the
	// database. The history is read in (created_at, id) order, so a line taken
	// from the process clock and another from the server's can be returned in
	// an order neither of them happened in.
	var now time.Time
	if err := tx.QueryRow(`SELECT now()`).Scan(&now); err != nil {
		return false, err
	}

	// 1. Conditional UPDATE — from 'processing' or 'triggered' (single-winner semantics)
	res, err := tx.Exec(
		`UPDATE alert_groups
		 SET status = $1, acknowledged_by = $2, ack_processed_at = NULL, updated_at = $3,
		     render_source_version = render_source_version + 1
		 WHERE id = $4 AND status IN ($5, $6)`,
		model.AlertGroupStatusAcknowledged, actor, now, id,
		model.AlertGroupStatusProcessing, model.AlertGroupStatusTriggered,
	)
	if err != nil {
		return false, err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}

	// 2. Timeline INSERT (within same transaction for atomicity)
	metaJSON := "{}"
	if meta != nil {
		if b, err := json.Marshal(meta); err == nil {
			metaJSON = string(b)
		}
	}
	_, err = tx.Exec(
		`INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New().String(), id, model.TimelineEventAcknowledged,
		"Alert group acknowledged", actor, metaJSON, now,
	)
	if err != nil {
		return false, err
	}

	// 3. INSERT outbox event (if provided)
	if err := insertOutboxEventTx(tx, outboxEvent); err != nil {
		return false, err
	}

	// 4. Withdraw what the group still owes. In the same commit as the status
	// change, because "acknowledged" and "nobody is being paged any more" are
	// one fact: split in two, a crash between them pages somebody for an alert
	// that is already being handled.
	// After the line above, and said so: the withdrawal happened because of the
	// acknowledgement, and a history that puts it first says the opposite.
	withdrawn, err := cancelIntentsAtTx(context.Background(), tx, id,
		"the alert was acknowledged", actor, now.Add(time.Microsecond))
	if err != nil {
		return false, err
	}

	// 5. And what the group still HAS out there is told that the alert moved.
	// The card is the domain's to keep current; this is where it learns there
	// is something new to show.
	if _, err := setDesiredStateTx(context.Background(), tx, s.render,
		outbound.DesiredStateRequest{
			AlertGroupID: id, Reason: outbound.DesiredAck, Actor: actor,
		}); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	countWithdrawn(withdrawn)
	return true, nil
}

// ResolveAlertGroupAtomic atomically resolves an alert group.
// Timeline event and status update happen in a single transaction.
// Returns (true, nil) if the resolve was applied, (false, nil) if the alert group
// was not in 'processing', 'triggered', or 'acknowledged' status, or (false, err) on failure.
func (s *Store) ResolveAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var now time.Time
	if err := tx.QueryRow(`SELECT now()`).Scan(&now); err != nil {
		return false, err
	}

	// 1. Conditional UPDATE — from 'processing', 'triggered', or 'acknowledged'
	res, err := tx.Exec(
		`UPDATE alert_groups
		 SET status = $1, resolved_at = $2, resolved_by = $3, updated_at = $2,
		     render_source_version = render_source_version + 1
		 WHERE id = $4 AND status IN ($5, $6, $7)`,
		model.AlertGroupStatusResolved, now, actor, id,
		model.AlertGroupStatusProcessing, model.AlertGroupStatusTriggered, model.AlertGroupStatusAcknowledged,
	)
	if err != nil {
		return false, err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		return false, nil
	}

	// 2. Timeline INSERT
	metaJSON := "{}"
	if meta != nil {
		if b, err := json.Marshal(meta); err == nil {
			metaJSON = string(b)
		}
	}
	_, err = tx.Exec(
		`INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New().String(), id, model.TimelineEventResolved,
		"Alert group manually resolved", actor, metaJSON, now,
	)
	if err != nil {
		return false, err
	}

	// 3. INSERT outbox event (if provided)
	if err := insertOutboxEventTx(tx, outboxEvent); err != nil {
		return false, err
	}

	// 4. Withdraw what the group still owes. In the same commit as the status
	// change, because "acknowledged" and "nobody is being paged any more" are
	// one fact: split in two, a crash between them pages somebody for an alert
	// that is already being handled.
	withdrawn, err := cancelIntentsAtTx(context.Background(), tx, id,
		"the alert was resolved", actor, now.Add(time.Microsecond))
	if err != nil {
		return false, err
	}

	// 5. The last revision a message will ever be brought to. It is raised even
	// when nothing else about the alert changed: a commitment parked at an
	// equal revision takes no further attempt, and the resolution would never
	// reach the card.
	if _, err := setDesiredStateTx(context.Background(), tx, s.render,
		outbound.DesiredStateRequest{
			AlertGroupID: id, Reason: outbound.DesiredResolve, Actor: actor,
		}); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	countWithdrawn(withdrawn)
	return true, nil
}

// countWithdrawn records commitments that ended because the alert did.
func countWithdrawn(n int) {
	for i := 0; i < n; i++ {
		countTerminal(outbound.FamilyNotification, outbound.StatusCanceled)
	}
}

// ========================================
// Notification Deliveries
// ========================================

func (s *Store) UpsertNotificationDelivery(d *model.NotificationDelivery) error {
	if d == nil {
		return errors.New("delivery is nil")
	}

	now := time.Now()
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	var jobStepID sql.NullString
	if d.JobStepID != nil && *d.JobStepID != "" {
		jobStepID = sql.NullString{String: *d.JobStepID, Valid: true}
	}

	// NOTE: is_primary is intentionally NOT updated on conflict to avoid clobbering
	// the chosen primary delivery when a step is retried or re-upserted.
	query := `
		INSERT INTO notification_deliveries (
			id, alert_group_id, job_step_id, provider, kind, target_type, target_id,
			provider_payload, supports_update,
			is_primary, is_firehose, attempt, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (job_step_id) DO UPDATE SET
			alert_group_id = EXCLUDED.alert_group_id,
			provider = EXCLUDED.provider,
			kind = EXCLUDED.kind,
			target_type = EXCLUDED.target_type,
			target_id = EXCLUDED.target_id,
			provider_payload = EXCLUDED.provider_payload,
			supports_update = EXCLUDED.supports_update,
			is_firehose = EXCLUDED.is_firehose,
			attempt = EXCLUDED.attempt,
			updated_at = EXCLUDED.updated_at
	`

	_, err := s.db.Exec(
		query,
		d.ID,
		d.AlertGroupID,
		jobStepID,
		d.Provider,
		d.Kind,
		d.TargetType,
		d.TargetID,
		d.ProviderPayload,
		d.SupportsUpdate,
		d.IsPrimary,
		d.IsFirehose,
		d.Attempt,
		d.CreatedAt,
		d.UpdatedAt,
	)
	return err
}

func (s *Store) SetPrimaryDeliveryIfNone(alertGroupID, deliveryID string) (bool, error) {
	query := `
		UPDATE notification_deliveries
		SET is_primary = TRUE, updated_at = $1
		WHERE id = $2
		AND NOT EXISTS (
			SELECT 1 FROM notification_deliveries WHERE alert_group_id = $3 AND is_primary = TRUE
		)
	`

	res, err := s.db.Exec(query, time.Now(), deliveryID, alertGroupID)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) GetPrimaryDelivery(alertGroupID, provider string) (*model.NotificationDelivery, error) {
	query := `
		SELECT id, alert_group_id, job_step_id, provider, kind, target_type, target_id,
		       provider_payload, supports_update,
		       is_primary, is_firehose, attempt, created_at, updated_at
		FROM notification_deliveries
		WHERE alert_group_id = $1 AND provider = $2 AND is_primary = TRUE
		ORDER BY created_at DESC
		LIMIT 1
	`

	row := s.db.QueryRow(query, alertGroupID, provider)
	return scanNotificationDelivery(row)
}

func (s *Store) GetFirehoseDelivery(alertGroupID, provider string) (*model.NotificationDelivery, error) {
	query := `
		SELECT id, alert_group_id, job_step_id, provider, kind, target_type, target_id,
		       provider_payload, supports_update,
		       is_primary, is_firehose, attempt, created_at, updated_at
		FROM notification_deliveries
		WHERE alert_group_id = $1 AND provider = $2 AND is_firehose = TRUE AND supports_update = TRUE
		ORDER BY created_at DESC
		LIMIT 1
	`

	row := s.db.QueryRow(query, alertGroupID, provider)
	return scanNotificationDelivery(row)
}

func (s *Store) GetDeliveryByID(id string) (*model.NotificationDelivery, error) {
	query := `
		SELECT id, alert_group_id, job_step_id, provider, kind, target_type, target_id,
		       provider_payload, supports_update,
		       is_primary, is_firehose, attempt, created_at, updated_at
		FROM notification_deliveries
		WHERE id = $1
	`

	row := s.db.QueryRow(query, id)
	return scanNotificationDelivery(row)
}

func (s *Store) UpdateDeliveryPayload(deliveryID, payload string) error {
	query := `UPDATE notification_deliveries SET provider_payload = $1, updated_at = $2 WHERE id = $3`
	_, err := s.db.Exec(query, payload, time.Now(), deliveryID)
	return err
}

func (s *Store) ListDeliveries(alertGroupID string) ([]*model.NotificationDelivery, error) {
	query := `
		SELECT id, alert_group_id, job_step_id, provider, kind, target_type, target_id,
		       provider_payload, supports_update,
		       is_primary, is_firehose, attempt, created_at, updated_at
		FROM notification_deliveries
		WHERE alert_group_id = $1
		ORDER BY created_at ASC
	`

	rows, err := s.db.Query(query, alertGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deliveries []*model.NotificationDelivery
	for rows.Next() {
		delivery, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		if delivery != nil {
			deliveries = append(deliveries, delivery)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deliveries, nil
}

func (s *Store) HasPrimaryDelivery(alertGroupID, provider string) (bool, error) {
	query := `
		SELECT 1 FROM notification_deliveries
		WHERE alert_group_id = $1 AND provider = $2 AND is_primary = TRUE
		LIMIT 1
	`

	var dummy int
	err := s.db.QueryRow(query, alertGroupID, provider).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

type notificationDeliveryScanner interface {
	Scan(dest ...any) error
}

func scanNotificationDelivery(scanner notificationDeliveryScanner) (*model.NotificationDelivery, error) {
	var d model.NotificationDelivery
	var jobStepID, targetType, targetID, providerPayload sql.NullString

	err := scanner.Scan(
		&d.ID,
		&d.AlertGroupID,
		&jobStepID,
		&d.Provider,
		&d.Kind,
		&targetType,
		&targetID,
		&providerPayload,
		&d.SupportsUpdate,
		&d.IsPrimary,
		&d.IsFirehose,
		&d.Attempt,
		&d.CreatedAt,
		&d.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if jobStepID.Valid {
		d.JobStepID = &jobStepID.String
	}
	d.TargetType = targetType.String
	d.TargetID = targetID.String
	d.ProviderPayload = providerPayload.String

	return &d, nil
}

func (s *Store) GetProcessingAlertGroups() ([]*model.AlertGroup, error) {
	// Include both processing and acknowledged (for Slack updates after Ack)
	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE status IN ($1, $2)`

	rows, err := s.db.Query(query, model.AlertGroupStatusProcessing, model.AlertGroupStatusAcknowledged)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alertGroups []*model.AlertGroup
	for rows.Next() {
		ag, err := scanAlertGroupRow(rows)
		if err != nil {
			return nil, err
		}
		alertGroups = append(alertGroups, ag)
	}

	if err := s.populateTimelineForAlertGroups(alertGroups); err != nil {
		return nil, err
	}
	return alertGroups, nil
}

func (s *Store) populateTimelineForAlertGroups(alertGroups []*model.AlertGroup) error {
	for _, ag := range alertGroups {
		events, err := s.GetTimelineEvents(ag.ID)
		if err != nil {
			return err
		}
		ag.TimelineEvents = events
	}
	return nil
}

func (s *Store) getAlertGroupsByStatus(status model.AlertGroupStatus) ([]*model.AlertGroup, error) {
	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE status = $1`

	rows, err := s.db.Query(query, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var alertGroups []*model.AlertGroup
	for rows.Next() {
		ag, err := scanAlertGroupRow(rows)
		if err != nil {
			return nil, err
		}
		alertGroups = append(alertGroups, ag)
	}
	return alertGroups, nil
}

func (s *Store) GetAlertGroupByID(id string) (*model.AlertGroup, error) {
	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE id = $1`

	row := s.db.QueryRow(query, id)
	ag, err := scanAlertGroupRow(row)
	if err != nil {
		return nil, err
	}
	if ag == nil {
		return nil, sql.ErrNoRows
	}

	// Populate timeline events for consistency with GetProcessingAlertGroups
	events, err := s.GetTimelineEvents(ag.ID)
	if err != nil {
		return nil, err
	}
	ag.TimelineEvents = events

	return ag, nil
}

func (s *Store) GetAllAlertGroups(status *model.AlertGroupStatus, limit, offset int) ([]*model.AlertGroup, int, error) {
	var query string
	var countQuery string
	var args []interface{}
	var countArgs []interface{}

	if status != nil {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query
		query = `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE status = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`
		countQuery = `SELECT COUNT(*) FROM alert_groups WHERE status = $1`
		args = []interface{}{*status, limit, offset}
		countArgs = []interface{}{*status}
	} else {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query
		query = `SELECT ` + alertGroupColumns + ` FROM alert_groups ORDER BY created_at DESC LIMIT $1 OFFSET $2`
		countQuery = `SELECT COUNT(*) FROM alert_groups`
		args = []interface{}{limit, offset}
	}

	var total int
	if err := s.db.QueryRow(countQuery, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var alertGroups []*model.AlertGroup
	for rows.Next() {
		ag, err := scanAlertGroupRow(rows)
		if err != nil {
			return nil, 0, err
		}
		alertGroups = append(alertGroups, ag)
	}
	return alertGroups, total, nil
}

// alertGroupSummaryColumns selects lightweight fields for list views,
// skipping heavy JSONB columns (alerts_data, policy_snapshot)
// and computing alert counts from alerts_data inline.
const alertGroupSummaryColumns = `id, alert_key, status, title, team_id, severity, current_step,
	oncall_snapshot, external_url, acknowledged_by, resolved_by,
	created_at, updated_at, resolved_at,
	jsonb_array_length(COALESCE(alerts_data::jsonb, '[]'::jsonb)),
	(SELECT count(*)::int FROM jsonb_array_elements(COALESCE(alerts_data::jsonb, '[]'::jsonb)) elem WHERE elem->>'status' = 'firing')`

func scanAlertGroupSummaryRow(scanner alertGroupScanner) (*model.AlertGroupSummary, error) {
	var ag model.AlertGroupSummary
	var resolvedAt sql.NullTime
	var teamID, severity, externalURL, oncallSnapshot, acknowledgedBy, resolvedBy sql.NullString

	err := scanner.Scan(
		&ag.ID, &ag.AlertKey, &ag.Status, &ag.Title,
		&teamID, &severity, &ag.CurrentStep,
		&oncallSnapshot, &externalURL, &acknowledgedBy, &resolvedBy,
		&ag.CreatedAt, &ag.UpdatedAt, &resolvedAt,
		&ag.AlertsCount, &ag.FiringCount,
	)
	if err != nil {
		return nil, err
	}

	ag.TeamID = teamID.String
	ag.Severity = severity.String
	ag.ExternalURL = externalURL.String
	ag.AcknowledgedBy = acknowledgedBy.String
	ag.ResolvedBy = resolvedBy.String

	if resolvedAt.Valid {
		ag.ResolvedAt = &resolvedAt.Time
	}
	if oncallSnapshot.Valid && oncallSnapshot.String != "" {
		_ = json.Unmarshal([]byte(oncallSnapshot.String), &ag.OnCallSnapshot)
	}

	return &ag, nil
}

// allowedSortColumns is a whitelist of columns/expressions that can be used for ORDER BY.
// severity and status use CASE expressions to sort by priority, not lexicographically.
var allowedSortColumns = map[string]string{
	"created_at":  "created_at",
	"severity":    "CASE severity WHEN 'critical' THEN 3 WHEN 'warning' THEN 2 WHEN 'info' THEN 1 ELSE 0 END",
	"status":      "CASE status WHEN 'triggered' THEN 5 WHEN 'processing' THEN 4 WHEN 'new' THEN 3 WHEN 'acknowledged' THEN 2 WHEN 'resolved' THEN 1 WHEN 'closed' THEN 0 ELSE 0 END",
	"title":       "title",
	"team_id":     "team_id",
	"resolved_at": "resolved_at",
}

func summaryOrderClause(sortBy, sortDir string) string {
	col, ok := allowedSortColumns[sortBy]
	if !ok {
		col = "created_at"
		sortBy = "created_at"
	}
	dir := "DESC"
	if strings.EqualFold(sortDir, "asc") {
		dir = "ASC"
	}
	primary := col + " " + dir
	if sortBy == "resolved_at" {
		nulls := "NULLS LAST"
		if dir == "ASC" {
			nulls = "NULLS FIRST"
		}
		primary = col + " " + dir + " " + nulls
	}
	if sortBy == "created_at" {
		return primary + ", id " + dir
	}
	return primary + ", created_at " + dir + ", id " + dir
}

// summaryWhereClause builds WHERE clause and args for alert group summary queries.
// initConditions/initArgs/startIdx allow pre-seeding (e.g. team_id = $1).
func summaryWhereClause(initConditions []string, initArgs []interface{}, startIdx int, statuses []model.AlertGroupStatus, severities []string, days int) (where string, args []interface{}, nextIdx int) {
	conditions := append([]string{}, initConditions...)
	args = append([]interface{}{}, initArgs...)
	argIdx := startIdx

	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, string(st))
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("status IN (%s)", strings.Join(placeholders, ",")))
	}
	if len(severities) > 0 {
		placeholders := make([]string, len(severities))
		for i, sev := range severities {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, sev)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("severity IN (%s)", strings.Join(placeholders, ",")))
	}
	if days > 0 {
		conditions = append(conditions, fmt.Sprintf("(updated_at >= NOW() - $%d * interval '1 day' OR created_at >= NOW() - $%d * interval '1 day')", argIdx, argIdx))
		args = append(args, days)
		argIdx++
	}

	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	nextIdx = argIdx
	return
}

func (s *Store) CountAlertGroupSummaries(statuses []model.AlertGroupStatus, severities []string, days int) (int, error) {
	where, args, _ := summaryWhereClause(nil, nil, 1, statuses, severities, days)
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM alert_groups"+where, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *Store) CountTeamAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days int) (int, error) {
	where, args, _ := summaryWhereClause([]string{"team_id = $1"}, []interface{}{teamID}, 2, statuses, severities, days)
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM alert_groups"+where, args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

func (s *Store) ListAlertGroupSummaries(statuses []model.AlertGroupStatus, severities []string, days, limit, offset int, sortBy, sortDir string) ([]*model.AlertGroupSummary, error) {
	where, args, argIdx := summaryWhereClause(nil, nil, 1, statuses, severities, days)

	orderBy := summaryOrderClause(sortBy, sortDir)
	query := buildAlertGroupSummarySelectQuery(where, orderBy, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*model.AlertGroupSummary
	for rows.Next() {
		ag, err := scanAlertGroupSummaryRow(rows)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, ag)
	}
	return summaries, nil
}

func (s *Store) ListTeamAlertGroupSummaries(teamID string, statuses []model.AlertGroupStatus, severities []string, days, limit, offset int, sortBy, sortDir string) ([]*model.AlertGroupSummary, error) {
	where, args, argIdx := summaryWhereClause([]string{"team_id = $1"}, []interface{}{teamID}, 2, statuses, severities, days)

	orderBy := summaryOrderClause(sortBy, sortDir)
	query := buildAlertGroupSummarySelectQuery(where, orderBy, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*model.AlertGroupSummary
	for rows.Next() {
		ag, err := scanAlertGroupSummaryRow(rows)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, ag)
	}
	return summaries, nil
}

func buildAlertGroupSummarySelectQuery(where, orderBy string, limitArg, offsetArg int) string {
	parts := []string{
		"SELECT ",
		alertGroupSummaryColumns,
		" FROM alert_groups",
		where,
		" ORDER BY ",
		orderBy,
		" LIMIT $",
		strconv.Itoa(limitArg),
		" OFFSET $",
		strconv.Itoa(offsetArg),
	}
	return strings.Join(parts, "")
}

func (s *Store) GetAlertGroupsByTeam(teamID string, limit, offset int) ([]*model.AlertGroup, int, error) {
	var total int
	countQuery := `SELECT COUNT(*) FROM alert_groups WHERE team_id = $1`
	if err := s.db.QueryRow(countQuery, teamID).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE team_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`

	rows, err := s.db.Query(query, teamID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var alertGroups []*model.AlertGroup
	for rows.Next() {
		ag, err := scanAlertGroupRow(rows)
		if err != nil {
			return nil, 0, err
		}
		alertGroups = append(alertGroups, ag)
	}
	return alertGroups, total, nil
}

// ========================================
// Incident CRUD (stub for future)
// ========================================

func (s *Store) CreateIncident(i *model.Incident) error {
	query := `INSERT INTO incidents (title, status, severity, commander_id, slack_channel_id, created_at) 
			  VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`
	return s.db.QueryRow(query, i.Title, i.Status, i.Severity, i.CommanderID, i.SlackChannelID, i.CreatedAt).Scan(&i.ID)
}

func (s *Store) GetIncidentByID(id int) (*model.Incident, error) {
	query := `SELECT id, title, status, severity, commander_id, slack_channel_id, created_at FROM incidents WHERE id = $1`
	row := s.db.QueryRow(query, id)

	var i model.Incident
	var severity, commanderID, slackChannelID sql.NullString
	err := row.Scan(&i.ID, &i.Title, &i.Status, &severity, &commanderID, &slackChannelID, &i.CreatedAt)
	if err != nil {
		return nil, err
	}
	i.Severity = severity.String
	if commanderID.Valid {
		i.CommanderID = &commanderID.String
	}
	if slackChannelID.Valid {
		i.SlackChannelID = &slackChannelID.String
	}
	return &i, nil
}

func (s *Store) GetAllIncidents() ([]*model.Incident, error) {
	query := `SELECT id, title, status, severity, commander_id, slack_channel_id, created_at FROM incidents ORDER BY created_at DESC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var incidents []*model.Incident
	for rows.Next() {
		var i model.Incident
		var severity, commanderID, slackChannelID sql.NullString
		if err := rows.Scan(&i.ID, &i.Title, &i.Status, &severity, &commanderID, &slackChannelID, &i.CreatedAt); err != nil {
			return nil, err
		}
		i.Severity = severity.String
		if commanderID.Valid {
			i.CommanderID = &commanderID.String
		}
		if slackChannelID.Valid {
			i.SlackChannelID = &slackChannelID.String
		}
		incidents = append(incidents, &i)
	}
	return incidents, nil
}

// ========================================
// Team CRUD
// ========================================

func (s *Store) CreateTeam(t *model.Team) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	var routesJSON interface{}
	if len(t.SeverityRoutes) > 0 {
		routesJSON, _ = json.Marshal(t.SeverityRoutes)
	}
	query := `INSERT INTO teams (id, name, description, default_policy_id, severity_routes, created_at)
	          VALUES ($1, $2, $3, $4, $5, $6)`
	defaultPolicyID := sql.NullString{String: t.DefaultPolicyID, Valid: t.DefaultPolicyID != ""}
	_, err := s.db.Exec(query, t.ID, t.Name, t.Description, defaultPolicyID, routesJSON, t.CreatedAt)
	return err
}

func (s *Store) GetTeamByID(id string) (*model.Team, error) {
	query := `SELECT id, name, description, default_policy_id, severity_routes, created_at FROM teams WHERE id = $1`
	row := s.db.QueryRow(query, id)

	var t model.Team
	var desc, defaultPolicyID sql.NullString
	var routesJSON []byte
	err := row.Scan(&t.ID, &t.Name, &desc, &defaultPolicyID, &routesJSON, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	t.Description = desc.String
	t.DefaultPolicyID = defaultPolicyID.String
	if routesJSON != nil {
		json.Unmarshal(routesJSON, &t.SeverityRoutes)
	}
	return &t, nil
}

func (s *Store) GetAllTeams() ([]*model.Team, error) {
	query := `SELECT id, name, description, default_policy_id, severity_routes, created_at FROM teams ORDER BY name`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var teams []*model.Team
	for rows.Next() {
		var t model.Team
		var desc, defaultPolicyID sql.NullString
		var routesJSON []byte
		if err := rows.Scan(&t.ID, &t.Name, &desc, &defaultPolicyID, &routesJSON, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.Description = desc.String
		t.DefaultPolicyID = defaultPolicyID.String
		if routesJSON != nil {
			json.Unmarshal(routesJSON, &t.SeverityRoutes)
		}
		teams = append(teams, &t)
	}
	return teams, nil
}

func (s *Store) UpdateTeam(t *model.Team) error {
	var routesJSON interface{}
	if len(t.SeverityRoutes) > 0 {
		routesJSON, _ = json.Marshal(t.SeverityRoutes)
	}

	query := `UPDATE teams SET name = $1, description = $2, default_policy_id = $3, severity_routes = $4 WHERE id = $5`
	defaultPolicyID := sql.NullString{String: t.DefaultPolicyID, Valid: t.DefaultPolicyID != ""}

	_, err := s.db.Exec(query, t.Name, t.Description, defaultPolicyID, routesJSON, t.ID)
	return err
}

func (s *Store) GetTeamMembershipsForUser(userID string) (map[string]model.TeamMemberRole, error) {
	query := `SELECT team_id, role FROM team_members WHERE user_id = $1`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	memberships := make(map[string]model.TeamMemberRole)
	for rows.Next() {
		var teamID, role string
		if err := rows.Scan(&teamID, &role); err != nil {
			return nil, err
		}
		memberships[teamID] = model.TeamMemberRole(role)
	}
	return memberships, nil
}

// ========================================
// User CRUD
// ========================================

func (s *Store) CreateUser(u *model.User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	if u.Role == "" {
		u.Role = model.UserRoleUser
	}

	// First user becomes admin (bootstrap rule)
	var adminCount int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&adminCount)
	if err == nil && adminCount == 0 {
		u.Role = model.UserRoleAdmin
	}

	query := `INSERT INTO users (id, email, name, role, password_hash, auth_provider, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err = s.db.Exec(query, u.ID, u.Email, u.Name, u.Role, u.PasswordHash, u.AuthProvider, u.CreatedAt)
	return err
}

// GetUserByID reads a user INCLUDING an erased one.
//
// It is the display read, and it exists so history stays legible: a revision
// or an override that names an erased ID has to resolve to something, and
// "Deleted user" is what anonymization left behind. Command paths and
// authentication must not use it - they use GetActiveUserByID, or an erased
// person keeps a working session and can be written back into the system.
func (s *Store) GetUserByID(id string) (*model.User, error) {
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at FROM users WHERE id = $1`
	return scanUser(s.db.QueryRow(query, id))
}

// GetActiveUserByID reads a user that has not been erased.
//
// Soft delete has to be terminal, and this is the read that makes it so:
// authentication and every command go through here, so an erased user's
// session stops working on their next request and no command can act on them.
func (s *Store) GetActiveUserByID(id string) (*model.User, error) {
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at
			  FROM users WHERE id = $1 AND deleted_at IS NULL`
	u, err := scanUser(s.db.QueryRow(query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	return u, err
}

func scanUser(row *sql.Row) (*model.User, error) {
	var u model.User
	var email, role, passwordHash, authProvider sql.NullString
	err := row.Scan(&u.ID, &email, &u.Name, &role, &passwordHash, &authProvider, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.Email = email.String
	u.Role = model.UserRole(role.String)
	u.PasswordHash = passwordHash.String
	u.AuthProvider = authProvider.String
	return &u, nil
}

// GetUsersByIDs is a DISPLAY read and deliberately does NOT filter erased
// users, unlike GetAllUsers directly above it.
//
// This is what hydrates names onto rendered history, and history names people
// who have since been erased: dropping them here would leave a shift with an
// ID and no name at all, which is worse than "Deleted user". The rule is by
// purpose, not by table - "align these two" is a change that silently breaks
// the calendar.
func (s *Store) GetUsersByIDs(ids []string) ([]*model.User, error) {
	if len(ids) == 0 {
		return []*model.User{}, nil
	}
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at FROM users WHERE id = ANY($1)`
	rows, err := s.db.Query(query, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*model.User
	for rows.Next() {
		var u model.User
		var email, role, passwordHash, authProvider sql.NullString
		if err := rows.Scan(&u.ID, &email, &u.Name, &role, &passwordHash, &authProvider, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Email = email.String
		u.Role = model.UserRole(role.String)
		u.PasswordHash = passwordHash.String
		u.AuthProvider = authProvider.String
		users = append(users, &u)
	}
	return users, nil
}

// GetAllUsers lists the people who exist. An erased user is a tombstone kept
// so history resolves, not a member of the directory - and the mock has always
// said so, which meant unit tests and production disagreed.
func (s *Store) GetAllUsers() ([]*model.User, error) {
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at
			  FROM users WHERE deleted_at IS NULL ORDER BY name`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*model.User
	for rows.Next() {
		var u model.User
		var email, role, passwordHash, authProvider sql.NullString
		if err := rows.Scan(&u.ID, &email, &u.Name, &role, &passwordHash, &authProvider, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Email = email.String
		u.Role = model.UserRole(role.String)
		u.PasswordHash = passwordHash.String
		u.AuthProvider = authProvider.String
		users = append(users, &u)
	}
	return users, nil
}

// AddTeamMember grants a membership, and only to a user who has not been
// erased: a bare INSERT would hand an erased identity a live grant back.
func (s *Store) AddTeamMember(teamID, userID string, role model.TeamMemberRole) error {
	query := activeUserCTE + `
		INSERT INTO team_members (team_id, user_id, role)
		SELECT $2, active.id, $3 FROM active
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`
	res, err := s.db.Exec(query, userID, teamID, role)
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrUserNotFound)
}

func (s *Store) GetTeamMembers(teamID string) ([]*model.TeamMemberDetail, error) {
	query := `SELECT u.id, u.email, u.name, u.created_at, tm.role
			  FROM users u
			  JOIN team_members tm ON u.id = tm.user_id
			  WHERE tm.team_id = $1`
	rows, err := s.db.Query(query, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var members []*model.TeamMemberDetail
	for rows.Next() {
		var tm model.TeamMemberDetail
		var email sql.NullString
		var role string
		if err := rows.Scan(&tm.ID, &email, &tm.Name, &tm.CreatedAt, &role); err != nil {
			return nil, err
		}
		tm.Email = email.String
		tm.TeamRole = model.TeamMemberRole(role)
		members = append(members, &tm)
	}
	return members, nil
}

func (s *Store) RemoveTeamMember(teamID, userID string) error {
	query := `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`
	_, err := s.db.Exec(query, teamID, userID)
	return err
}

// GetUserByEmail looks a user up by address. An anonymized user has a NULL
// email, so this returns sql.ErrNoRows for their former address - it can never
// resurface an erased identity.
func (s *Store) GetUserByEmail(email string) (*model.User, error) {
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at FROM users WHERE email = $1`
	row := s.db.QueryRow(query, email)

	var u model.User
	var storedEmail, role, passwordHash, authProvider sql.NullString
	err := row.Scan(&u.ID, &storedEmail, &u.Name, &role, &passwordHash, &authProvider, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.Email = storedEmail.String
	u.Role = model.UserRole(role.String)
	u.PasswordHash = passwordHash.String
	u.AuthProvider = authProvider.String
	return &u, nil
}

// The three user mutations below all carry `deleted_at IS NULL` and check that
// they affected a row.
//
// Without the filter each of them silently refills an erased row - name and
// email, a password hash, a federated identity - and the erasure that was
// supposed to be terminal becomes a stage a later request can undo. Without
// the row count, an update that matched nothing reports success, which reads
// as "the change was applied" to every caller.

// UpdateUser writes profile fields only. role is deliberately NOT among them.
//
// Role is an invariant, not a field: the system must keep one active
// administrator, and SetUserRole is what serializes that check against
// erasure. A profile update that also carried role could undo a promotion it
// never saw - read B as a user, let someone promote B and erase the last other
// admin, then write B back as a user and leave nobody in charge.
func (s *Store) UpdateUser(u *model.User) error {
	query := `UPDATE users SET email = $1, name = $2 WHERE id = $3 AND deleted_at IS NULL`
	res, err := s.db.Exec(query, u.Email, u.Name, u.ID)
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrUserNotFound)
}

func (s *Store) UpdateUserPassword(id, hash string) error {
	query := `UPDATE users SET password_hash = $1 WHERE id = $2 AND deleted_at IS NULL`
	res, err := s.db.Exec(query, hash, id)
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrUserNotFound)
}

func (s *Store) UpdateUserAuthProvider(id, provider string) error {
	query := `UPDATE users SET auth_provider = $1 WHERE id = $2 AND deleted_at IS NULL`
	res, err := s.db.Exec(query, provider, id)
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrUserNotFound)
}

// DeleteUser physically removes a user.
//
// Deprecated: user deletion goes through erasure.Service, which soft-deletes
// and anonymizes so the history that names the ID stays explainable, and which
// refuses to erase someone who still holds an assignment. This remains only
// for the tooling that has not moved yet and is removed with the rest of the
// legacy schedule path.
func (s *Store) DeleteUser(id string) error {
	if _, err := s.db.Exec(`DELETE FROM team_members WHERE user_id = $1`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users WHERE id = $1`, id)
	return err
}

// ========================================
// RBAC Methods
// ========================================

// GetUserTeamRole returns the team role for a user in a specific team.
// Returns sql.ErrNoRows if user is not a member of the team.
func (s *Store) GetUserTeamRole(userID, teamID string) (model.TeamMemberRole, error) {
	query := `SELECT role FROM team_members WHERE user_id = $1 AND team_id = $2`
	var role string
	err := s.db.QueryRow(query, userID, teamID).Scan(&role)
	if err != nil {
		return "", err
	}
	return model.TeamMemberRole(role), nil
}

// SetUserRole updates a user's global role, refusing to demote the last
// administrator the system has left.
//
// It takes the admin-lifecycle advisory lock first, the same one erasure
// takes, because these two commands change the same quantity from different
// directions: without a shared mutex, erasing one of two admins and demoting
// the other can both observe "there are two" and both commit.
//
// The count is of ACTIVE admins. Counting erased ones as living was the older
// bug: after the first erasure the guard believed there were still two, and
// the last real administrator could be demoted.
func (s *Store) SetUserRole(userID string, role model.UserRole) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, adminLifecycleLockKey); err != nil {
		return err
	}

	var currentRole string
	err = tx.QueryRow(`SELECT role FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, userID).Scan(&currentRole)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}

	if currentRole == string(model.UserRoleAdmin) && role != model.UserRoleAdmin {
		var adminCount int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).
			Scan(&adminCount); err != nil {
			return err
		}
		if adminCount <= 1 {
			return ErrLastAdmin
		}
	}

	if _, err = tx.Exec(`UPDATE users SET role = $1 WHERE id = $2 AND deleted_at IS NULL`, role, userID); err != nil {
		return err
	}

	return tx.Commit()
}

// CountAdmins returns the number of ACTIVE users with the admin role. An
// erased admin is not an administrator the system can fall back on.
func (s *Store) CountAdmins() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).Scan(&count)
	return count, err
}

// ========================================
// Timeline Events
// ========================================

func (s *Store) AddTimelineEvent(e *model.TimelineEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	metadata := "{}"
	if e.Metadata != nil {
		data, _ := json.Marshal(e.Metadata)
		metadata = string(data)
	}
	query := `INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at) 
			  VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := s.db.Exec(query, e.ID, e.AlertGroupID, e.Type, e.Message, e.Actor, metadata, e.CreatedAt)
	return err
}

func (s *Store) GetTimelineEvents(alertGroupID string) ([]*model.TimelineEvent, error) {
	query := `SELECT id, alert_group_id, type, message, actor, metadata, created_at 
			  FROM timeline_events 
			  WHERE alert_group_id = $1 
			  ORDER BY created_at ASC, id ASC`

	rows, err := s.db.Query(query, alertGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*model.TimelineEvent
	for rows.Next() {
		var e model.TimelineEvent
		var metadata sql.NullString

		if err := rows.Scan(&e.ID, &e.AlertGroupID, &e.Type, &e.Message, &e.Actor, &metadata, &e.CreatedAt); err != nil {
			return nil, err
		}

		if metadata.Valid && metadata.String != "" {
			_ = json.Unmarshal([]byte(metadata.String), &e.Metadata)
		}

		events = append(events, &e)
	}
	return events, nil
}

// generateUUID creates a new UUID using google/uuid
func generateUUID() string {
	return uuid.New().String()
}

// nullIfEmpty returns nil if s is empty, otherwise returns s
func nullIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ========================================
// Schedule Overrides
// ========================================

// ========================================
// Rotation Epochs
// ========================================

// ========================================
// API Tokens CRUD
// ========================================

// CreateAPIToken issues a token, and only to a user who has not been erased.
// A credential is the last thing an erased identity should get back.
func (s *Store) CreateAPIToken(token *model.APIToken) error {
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}
	query := activeUserCTE + `
		INSERT INTO api_tokens (id, user_id, name, token_hash, expires_at, created_at)
		SELECT $2, active.id, $3, $4, $5, $6 FROM active`
	res, err := s.db.Exec(query, token.UserID, token.ID, token.Name, token.TokenHash, token.ExpiresAt, token.CreatedAt)
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrUserNotFound)
}

// GetAPITokenByHash retrieves an API token by its hash
// GetAPITokenByID retrieves an API token by ID
func (s *Store) GetAPITokenByID(id string) (*model.APIToken, error) {
	query := `SELECT id, user_id, name, token_hash, expires_at, last_used_at, created_at 
			  FROM api_tokens WHERE id = $1`
	row := s.db.QueryRow(query, id)

	var token model.APIToken
	var expiresAt, lastUsedAt sql.NullTime

	err := row.Scan(&token.ID, &token.UserID, &token.Name, &token.TokenHash, &expiresAt, &lastUsedAt, &token.CreatedAt)
	if err != nil {
		return nil, err
	}

	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Time
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Time
	}
	return &token, nil
}

// GetAPITokenByHash resolves a bearer token, and a token belonging to an
// erased user does not resolve.
//
// The join is what makes soft delete terminal on this path. The cookie path
// checks the user; without this, a token that outlived its owner by a race
// would keep authenticating one, and Bearer requests never look at users at
// all.
func (s *Store) GetAPITokenByHash(hash string) (*model.APIToken, error) {
	query := `SELECT t.id, t.user_id, t.name, t.token_hash, t.expires_at, t.last_used_at, t.created_at
			  FROM api_tokens t
			  JOIN users u ON u.id = t.user_id AND u.deleted_at IS NULL
			  WHERE t.token_hash = $1`
	row := s.db.QueryRow(query, hash)

	var token model.APIToken
	var expiresAt, lastUsedAt sql.NullTime

	err := row.Scan(&token.ID, &token.UserID, &token.Name, &token.TokenHash, &expiresAt, &lastUsedAt, &token.CreatedAt)
	if err != nil {
		return nil, err
	}

	if expiresAt.Valid {
		token.ExpiresAt = &expiresAt.Time
	}
	if lastUsedAt.Valid {
		token.LastUsedAt = &lastUsedAt.Time
	}
	return &token, nil
}

// GetUserAPITokens retrieves all API tokens for a user
func (s *Store) GetUserAPITokens(userID string) ([]*model.APIToken, error) {
	query := `SELECT id, user_id, name, token_hash, expires_at, last_used_at, created_at 
			  FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []*model.APIToken
	for rows.Next() {
		var token model.APIToken
		var expiresAt, lastUsedAt sql.NullTime

		if err := rows.Scan(&token.ID, &token.UserID, &token.Name, &token.TokenHash, &expiresAt, &lastUsedAt, &token.CreatedAt); err != nil {
			return nil, err
		}

		if expiresAt.Valid {
			token.ExpiresAt = &expiresAt.Time
		}
		if lastUsedAt.Valid {
			token.LastUsedAt = &lastUsedAt.Time
		}
		tokens = append(tokens, &token)
	}
	return tokens, nil
}

// UpdateAPITokenLastUsed updates the last_used_at timestamp
func (s *Store) UpdateAPITokenLastUsed(id string) error {
	query := `UPDATE api_tokens SET last_used_at = $1 WHERE id = $2`
	_, err := s.db.Exec(query, time.Now(), id)
	return err
}

// DeleteAPIToken deletes an API token by ID
func (s *Store) DeleteAPIToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = $1`, id)
	return err
}

// (The old Slack OTP methods — SaveSlackOTP, ConfirmSlackOTP, UnbindSlackUser — were
// removed in Epic 7 Sprint 3. See external_identities + link_tokens in identity_store.go.)

// ========================================
// Escalation Policy CRUD (Phase 4)
// ========================================

func (s *Store) CreateEscalationPolicy(p *model.EscalationPolicy) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Insert policy
	_, err = tx.Exec(`INSERT INTO escalation_policies (id, name, description, team_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		p.ID, p.Name, p.Description, p.TeamID, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return err
	}

	// Insert steps
	for _, step := range p.Steps {
		_, err = tx.Exec(`INSERT INTO escalation_steps (id, policy_id, step_index, provider, target_kind, target_type, target_id, delay_seconds, timeout_seconds, max_attempts, message, continue_on_failure)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			step.ID, p.ID, step.StepIndex, step.Provider, step.TargetKind, step.TargetType, step.TargetID, step.DelaySeconds, step.TimeoutSeconds, step.MaxAttempts, step.Message, step.ContinueOnFailure)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) GetEscalationPolicyByID(id string) (*model.EscalationPolicy, error) {
	var p model.EscalationPolicy
	var teamID sql.NullString
	var description sql.NullString

	err := s.db.QueryRow(`SELECT id, name, description, team_id, created_at, updated_at FROM escalation_policies WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &description, &teamID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.Description = description.String
	if teamID.Valid {
		p.TeamID = &teamID.String
	}

	// Load steps
	rows, err := s.db.Query(`SELECT id, policy_id, step_index, provider, target_kind, target_type, target_id, delay_seconds, timeout_seconds, max_attempts, message, COALESCE(continue_on_failure, true)
		FROM escalation_steps WHERE policy_id = $1 ORDER BY step_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var step model.EscalationStep
		var msg sql.NullString
		if err := rows.Scan(&step.ID, &step.PolicyID, &step.StepIndex, &step.Provider, &step.TargetKind, &step.TargetType, &step.TargetID, &step.DelaySeconds, &step.TimeoutSeconds, &step.MaxAttempts, &msg, &step.ContinueOnFailure); err != nil {
			return nil, err
		}
		step.Message = msg.String
		p.Steps = append(p.Steps, &step)
	}
	// A read that stopped halfway is not a shorter policy: returned as one, an
	// escalation would quietly skip the steps that did not arrive, and nobody
	// would be paged by them.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the steps of policy %s: %w", id, err)
	}

	return &p, nil
}

func (s *Store) GetAllEscalationPolicies() ([]*model.EscalationPolicy, error) {
	rows, err := s.db.Query(`SELECT id, name, description, team_id, created_at, updated_at FROM escalation_policies ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []*model.EscalationPolicy
	for rows.Next() {
		var p model.EscalationPolicy
		var teamID, description sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &description, &teamID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Description = description.String
		if teamID.Valid {
			p.TeamID = &teamID.String
		}
		policies = append(policies, &p)
	}

	// Load steps for each policy
	for _, p := range policies {
		stepRows, err := s.db.Query(`SELECT id, policy_id, step_index, provider, target_kind, target_type, target_id, delay_seconds, timeout_seconds, max_attempts, COALESCE(message, ''), COALESCE(continue_on_failure, true)
			FROM escalation_steps WHERE policy_id = $1 ORDER BY step_index`, p.ID)
		if err != nil {
			return nil, err
		}
		for stepRows.Next() {
			var step model.EscalationStep
			if err := stepRows.Scan(&step.ID, &step.PolicyID, &step.StepIndex, &step.Provider, &step.TargetKind, &step.TargetType, &step.TargetID, &step.DelaySeconds, &step.TimeoutSeconds, &step.MaxAttempts, &step.Message, &step.ContinueOnFailure); err != nil {
				stepRows.Close()
				return nil, err
			}
			p.Steps = append(p.Steps, &step)
		}
		stepRows.Close()
	}

	return policies, nil
}

func (s *Store) UpdateEscalationPolicy(p *model.EscalationPolicy) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Update policy
	_, err = tx.Exec(`UPDATE escalation_policies SET name = $1, description = $2, team_id = $3, updated_at = $4 WHERE id = $5`,
		p.Name, p.Description, p.TeamID, time.Now(), p.ID)
	if err != nil {
		return err
	}

	// Delete existing steps
	_, err = tx.Exec(`DELETE FROM escalation_steps WHERE policy_id = $1`, p.ID)
	if err != nil {
		return err
	}

	// Insert new steps
	for _, step := range p.Steps {
		if step.ID == "" {
			step.ID = uuid.New().String()
		}
		_, err = tx.Exec(`INSERT INTO escalation_steps (id, policy_id, step_index, provider, target_kind, target_type, target_id, delay_seconds, timeout_seconds, max_attempts, message, continue_on_failure)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			step.ID, p.ID, step.StepIndex, step.Provider, step.TargetKind, step.TargetType, step.TargetID, step.DelaySeconds, step.TimeoutSeconds, step.MaxAttempts, step.Message, step.ContinueOnFailure)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) DeleteEscalationPolicy(id string) error {
	_, err := s.db.Exec(`DELETE FROM escalation_policies WHERE id = $1`, id)
	return err
}

func (s *Store) GetPoliciesByTeamID(teamID string) ([]*model.EscalationPolicy, error) {
	query := `SELECT id, name, description, team_id, created_at, updated_at 
		FROM escalation_policies 
		WHERE team_id = $1 OR team_id IS NULL 
		ORDER BY team_id NULLS LAST, name`
	return s.queryPolicies(query, teamID)
}

func (s *Store) GetEscalationPoliciesForUser(userID string) ([]*model.EscalationPolicy, error) {
	query := `
		SELECT id, name, description, team_id, created_at, updated_at 
		FROM escalation_policies 
		WHERE team_id IS NULL 
		   OR team_id IN (SELECT team_id FROM team_members WHERE user_id = $1)
		ORDER BY name`
	return s.queryPolicies(query, userID)
}

func (s *Store) queryPolicies(query string, args ...interface{}) ([]*model.EscalationPolicy, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []*model.EscalationPolicy
	for rows.Next() {
		var p model.EscalationPolicy
		var tid, description sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &description, &tid, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Description = description.String
		if tid.Valid {
			p.TeamID = &tid.String
		}
		policies = append(policies, &p)
	}

	// Load steps
	for _, p := range policies {
		stepRows, err := s.db.Query(`SELECT id, policy_id, step_index, provider, target_kind, target_type, target_id, delay_seconds, timeout_seconds, max_attempts, COALESCE(message, ''), COALESCE(continue_on_failure, true)
			FROM escalation_steps WHERE policy_id = $1 ORDER BY step_index`, p.ID)
		if err != nil {
			return nil, err
		}
		for stepRows.Next() {
			var step model.EscalationStep
			if err := stepRows.Scan(&step.ID, &step.PolicyID, &step.StepIndex, &step.Provider, &step.TargetKind, &step.TargetType, &step.TargetID, &step.DelaySeconds, &step.TimeoutSeconds, &step.MaxAttempts, &step.Message, &step.ContinueOnFailure); err != nil {
				stepRows.Close()
				return nil, err
			}
			p.Steps = append(p.Steps, &step)
		}
		stepRows.Close()
	}

	return policies, nil
}

// GetMetricsSnapshot returns all data needed by the Prometheus business metrics collector.
func (s *Store) GetMetricsSnapshot() (*model.MetricsSnapshot, error) {
	snap := &model.MetricsSnapshot{}

	// 1. Active alert groups by team/severity
	rows, err := s.db.Query(`
		SELECT COALESCE(team_id, ''), COALESCE(severity, ''), COUNT(*) FROM alert_groups
		WHERE status NOT IN ('resolved', 'closed')
		GROUP BY COALESCE(team_id, ''), COALESCE(severity, '')`)
	if err != nil {
		return nil, fmt.Errorf("active alert groups query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c model.AlertGroupCount
		if err := rows.Scan(&c.TeamID, &c.Severity, &c.Count); err != nil {
			return nil, err
		}
		snap.ActiveAlertGroups = append(snap.ActiveAlertGroups, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 1b. Alert groups by team/severity/status
	rows2, err := s.db.Query(`
		SELECT COALESCE(team_id, ''), COALESCE(severity, ''), status, COUNT(*) FROM alert_groups
		GROUP BY COALESCE(team_id, ''), COALESCE(severity, ''), status`)
	if err != nil {
		return nil, fmt.Errorf("alert groups by status query: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var c model.AlertGroupStatusCount
		if err := rows2.Scan(&c.TeamID, &c.Severity, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		snap.AlertGroupsByStatus = append(snap.AlertGroupsByStatus, c)
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}

	// 2. Teams without on-call: no schedule, or one whose configuration in force
	// puts nobody on duty.
	//
	// The state is read from the revision in force (the open-ended tail), which
	// is the only place the configuration lives. Two predicates are not
	// obvious and are both deliberate: deleted_at excludes soft-deleted
	// schedules, which the pre-revision version of this query could not have,
	// and l1.enabled is checked because a disabled layer keeps its groups in the
	// snapshot - only the phase pair is cleared - so groups alone would report a
	// switched-off rotation as covered.
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM teams t WHERE NOT EXISTS (
			SELECT 1 FROM schedules s
			JOIN schedule_revisions r ON r.schedule_id = s.id AND r.effective_to IS NULL
			WHERE s.team_id = t.id AND s.deleted_at IS NULL AND r.kind = 'active'
			  AND (r.snapshot->'l1'->>'enabled')::boolean
			  AND jsonb_array_length(r.snapshot->'l1'->'groups') > 0)`).Scan(&snap.TeamsWithoutOnCall)
	if err != nil {
		return nil, fmt.Errorf("teams without oncall query: %w", err)
	}

	// 3. Teams with permanent on-call: exactly one person carries L1, forever.
	//
	// "Exactly one person", not "exactly one group". The two were the same thing
	// when user_ids was a flat list of users, and the query said so; the
	// migration that turned that column into a list of GROUPS silently redefined
	// the same length check, and a single group of several people has counted as
	// a permanent on-call ever since - though those people are all paged
	// together and nobody is alone. This restores what the metric has always
	// claimed to measure (see its HELP text): bus factor one.
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM schedules s
		JOIN schedule_revisions r ON r.schedule_id = s.id AND r.effective_to IS NULL
		WHERE s.deleted_at IS NULL AND r.kind = 'active'
		  AND (r.snapshot->'l1'->>'enabled')::boolean
		  AND jsonb_array_length(r.snapshot->'l1'->'groups') = 1
		  AND jsonb_array_length(r.snapshot->'l1'->'groups'->0->'members') = 1`).
		Scan(&snap.TeamsWithPermanentOnCall)
	if err != nil {
		return nil, fmt.Errorf("teams with permanent oncall query: %w", err)
	}

	// 4. Teams without escalation policy
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM teams
		WHERE default_policy_id IS NULL OR default_policy_id = ''`).Scan(&snap.TeamsWithoutPolicy)
	if err != nil {
		return nil, fmt.Errorf("teams without policy query: %w", err)
	}

	// 5. Outbox events by status
	rows3, err := s.db.Query(`SELECT status, COUNT(*) FROM event_outbox GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("outbox events by status query: %w", err)
	}
	defer rows3.Close()
	for rows3.Next() {
		var c model.StatusCount
		if err := rows3.Scan(&c.Status, &c.Count); err != nil {
			return nil, err
		}
		snap.OutboxEventsByStatus = append(snap.OutboxEventsByStatus, c)
	}
	if err := rows3.Err(); err != nil {
		return nil, err
	}

	// 6. Outbox deliveries by status
	rows4, err := s.db.Query(`SELECT status, COUNT(*) FROM event_outbox_deliveries GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("outbox deliveries by status query: %w", err)
	}
	defer rows4.Close()
	for rows4.Next() {
		var c model.StatusCount
		if err := rows4.Scan(&c.Status, &c.Count); err != nil {
			return nil, err
		}
		snap.OutboxDeliveriesByStatus = append(snap.OutboxDeliveriesByStatus, c)
	}
	if err := rows4.Err(); err != nil {
		return nil, err
	}

	// 7. Outbound commitments by family and status.
	rows5, err := s.db.Query(`
		SELECT delivery_family, status, COUNT(*) FROM outbound_intents
		GROUP BY delivery_family, status`)
	if err != nil {
		return nil, fmt.Errorf("outbound intents by status query: %w", err)
	}
	defer rows5.Close()
	for rows5.Next() {
		var c model.OutboundStatusCount
		if err := rows5.Scan(&c.Family, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		snap.OutboundIntentsByStatus = append(snap.OutboundIntentsByStatus, c)
	}
	if err := rows5.Err(); err != nil {
		return nil, err
	}

	// 8. How far behind each family is.
	//
	// The subtraction is the database's, in the same statement that finds the
	// row: taken from the process's clock instead, this would report the drift
	// between two machines as a backlog.
	//
	// Two predicates carry the definition. `next_attempt_at <= now()` excludes
	// work that is SCHEDULED - a delayed policy step, a retry on backoff - which
	// is not lateness but the plan. And nothing is said about the lease: a
	// commitment claimed by a worker that then hung would otherwise disappear
	// from this gauge for the length of its lease, which is precisely the moment
	// somebody needs to be told.
	//
	// Every family that has rows at all reports a number, zero included, so a
	// backlog that has been worked off stops ringing instead of leaving its last
	// value behind forever.
	rows6, err := s.db.Query(`
		SELECT delivery_family,
		       COALESCE(EXTRACT(EPOCH FROM (now() - MIN(next_attempt_at)
		           FILTER (WHERE status = 'pending' AND next_attempt_at <= now()))), 0)::double precision
		FROM outbound_intents
		GROUP BY delivery_family`)
	if err != nil {
		return nil, fmt.Errorf("outbound queue lateness query: %w", err)
	}
	defer rows6.Close()
	for rows6.Next() {
		var l model.OutboundLateness
		if err := rows6.Scan(&l.Family, &l.Seconds); err != nil {
			return nil, err
		}
		snap.OutboundLatenessSeconds = append(snap.OutboundLatenessSeconds, l)
	}
	if err := rows6.Err(); err != nil {
		return nil, err
	}
	// And the paging family reports even when it has no rows at all, so the
	// series exists from the first scrape rather than appearing the first time
	// somebody is paged.
	if !hasFamily(snap.OutboundLatenessSeconds, outbound.FamilyNotification) {
		snap.OutboundLatenessSeconds = append(snap.OutboundLatenessSeconds,
			model.OutboundLateness{Family: outbound.FamilyNotification})
	}

	return snap, nil
}

func hasFamily(rows []model.OutboundLateness, family string) bool {
	for _, row := range rows {
		if row.Family == family {
			return true
		}
	}
	return false
}
