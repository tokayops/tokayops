package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/google/uuid"
	"github.com/lib/pq"
	_ "github.com/lib/pq"
)

type Store struct {
	db *sql.DB
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

	return &Store{db: db}, nil
}

func (s *Store) InitDB() error {
	// PostgreSQL Schema for Phase 2 - Create tables FIRST
	query := `
	-- Alert Groups table (renamed from incidents)
	CREATE TABLE IF NOT EXISTS alert_groups (
		id TEXT PRIMARY KEY,
		dedup_key TEXT NOT NULL,
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
	END $$;
	
	CREATE UNIQUE INDEX IF NOT EXISTS idx_active_alert_groups ON alert_groups(dedup_key) WHERE status NOT IN ('resolved', 'closed');
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
		dedup_key TEXT,
		alert_group_id TEXT,
		current_stage INTEGER DEFAULT 0,
		error TEXT,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		finished_at TIMESTAMPTZ,
		canceled_at TIMESTAMPTZ
	);

	DROP INDEX IF EXISTS idx_active_jobs_dedup;
	CREATE UNIQUE INDEX IF NOT EXISTS idx_active_jobs_dedup ON jobs(dedup_key)
	WHERE status IN ('pending', 'running');
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
	migrationQuery := `
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

		-- 9. Add alert_group_id column to jobs table
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns
		              WHERE table_name='jobs' AND column_name='alert_group_id') THEN
			ALTER TABLE jobs ADD COLUMN alert_group_id TEXT;
		END IF;

		-- 9b. Drop unique index before backfill (backfill creates temporary duplicates)
		DROP INDEX IF EXISTS idx_one_escalation_per_ag;

		-- 9c. Backfill alert_group_id from payload for all NULL rows
		UPDATE jobs SET alert_group_id = payload::jsonb->>'alert_group_id'
		WHERE type = 'escalation' AND alert_group_id IS NULL
		  AND payload::jsonb->>'alert_group_id' IS NOT NULL;

		-- 9d. Dedup: keep only latest escalation job per AG, nullify older dupes
		WITH ranked AS (
			SELECT id,
			       ROW_NUMBER() OVER (PARTITION BY alert_group_id ORDER BY created_at DESC) as rn
			FROM jobs
			WHERE type = 'escalation' AND alert_group_id IS NOT NULL
		)
		UPDATE jobs SET alert_group_id = NULL
		FROM ranked
		WHERE jobs.id = ranked.id AND ranked.rn > 1;

		-- 9e. Recreate unique index (after dedup guarantees no conflicts)
		CREATE UNIQUE INDEX idx_one_escalation_per_ag
		ON jobs(alert_group_id) WHERE type = 'escalation' AND alert_group_id IS NOT NULL;
	END $$;
	`
	if _, err := s.db.Exec(migrationQuery); err != nil {
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

	// Phase 3: Schedule tables
	scheduleQuery := `
	-- Schedules table (1:1 with teams)
	CREATE TABLE IF NOT EXISTS schedules (
		id TEXT PRIMARY KEY,
		team_id TEXT UNIQUE NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
		timezone TEXT DEFAULT 'UTC',
		
		-- L1 Layer Config
		l1_rotation_type TEXT NOT NULL DEFAULT 'weekly',
		l1_handoff_time TIME NOT NULL DEFAULT '11:00',
		l1_handoff_day INTEGER DEFAULT 1,
		l1_rotation_start TIMESTAMPTZ NOT NULL,
		
		-- L2 Layer Config (optional)
		l2_enabled BOOLEAN DEFAULT FALSE,
		l2_escalation_timeout_min INTEGER DEFAULT 5,
		l2_rotation_type TEXT DEFAULT 'weekly',
		l2_handoff_time TIME DEFAULT '11:00',
		l2_handoff_day INTEGER DEFAULT 1,
		l2_rotation_start TIMESTAMPTZ,
		
		-- Slack usergroup sync (optional)
		slack_usergroup_id TEXT,
		
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);

	-- Schedule users (ordered rotation)
	CREATE TABLE IF NOT EXISTS schedule_users (
		schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
		layer TEXT NOT NULL,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		position INTEGER NOT NULL,
		PRIMARY KEY (schedule_id, layer, user_id)
	);
	CREATE INDEX IF NOT EXISTS idx_schedule_users_layer ON schedule_users(schedule_id, layer, position);

	-- Schedule overrides (minute precision)
	CREATE TABLE IF NOT EXISTS schedule_overrides (
		id TEXT PRIMARY KEY,
		schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		start_time TIMESTAMPTZ NOT NULL,
		end_time TIMESTAMPTZ NOT NULL,
		reason TEXT,
		created_by TEXT REFERENCES users(id),
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		CHECK (end_time > start_time)
	);
	CREATE INDEX IF NOT EXISTS idx_overrides_range ON schedule_overrides(schedule_id, start_time, end_time);
	
	-- Prevent overlapping overrides (requires btree_gist extension)
	DO $$ BEGIN
		CREATE EXTENSION IF NOT EXISTS btree_gist;
	EXCEPTION WHEN insufficient_privilege THEN
		-- Extension creation may fail in restricted environments
		RAISE NOTICE 'btree_gist extension not created (needs superuser)';
	END $$;
	DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'no_overlapping_overrides') THEN
			ALTER TABLE schedule_overrides ADD CONSTRAINT no_overlapping_overrides
				EXCLUDE USING gist (schedule_id WITH =, tsrange(start_time, end_time) WITH &&);
		END IF;
	EXCEPTION WHEN undefined_function THEN
		-- Constraint creation may fail if btree_gist extension isn't available
		RAISE NOTICE 'Exclusion constraint not created (btree_gist not available)';
	END $$;

	-- Rotation epochs (history of rotation order changes)
	CREATE TABLE IF NOT EXISTS rotation_epochs (
		id TEXT PRIMARY KEY,
		schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
		layer TEXT NOT NULL, -- 'l1' or 'l2'
		user_ids TEXT NOT NULL, -- JSON array of user IDs in rotation order
		start_time TIMESTAMPTZ NOT NULL,
		end_time TIMESTAMPTZ, -- NULL = current epoch
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_rotation_epochs_schedule ON rotation_epochs(schedule_id, layer, start_time);
	-- Ensure only one open epoch per schedule/layer (P1-1 fix)
	CREATE UNIQUE INDEX IF NOT EXISTS idx_rotation_epochs_one_open ON rotation_epochs(schedule_id, layer) WHERE end_time IS NULL;

	-- Migration: convert flat user_ids ["a","b","c"] to groups [["a"],["b"],["c"]]
	DO $$ BEGIN
		UPDATE rotation_epochs
		SET user_ids = (
			SELECT json_agg(json_build_array(elem))::text
			FROM json_array_elements_text(user_ids::json) AS elem
		)
		WHERE user_ids IS NOT NULL
		  AND user_ids != '[]'
		  AND user_ids::json->0 IS NOT NULL
		  AND json_typeof(user_ids::json->0) = 'string';
	END $$;

	-- Migration: add slack_usergroup_id if not exists
	DO $$ BEGIN
		IF NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='schedules' AND column_name='slack_usergroup_id') THEN
			ALTER TABLE schedules ADD COLUMN slack_usergroup_id TEXT;
		END IF;
	END $$;
	`
	if _, err := s.db.Exec(scheduleQuery); err != nil {
		return err
	}

	// Schedule revision history: append-only configuration snapshots plus the
	// aggregate-root columns that carry version and history completeness.
	//
	// Nothing in the runtime reads these tables yet; they are created here so
	// the schema exists before the write path is switched over. All range
	// constraints use tstzrange — the legacy no_overlapping_overrides
	// constraint above uses naive tsrange over TIMESTAMPTZ columns and is not
	// a template to follow.
	revisionQuery := `
	ALTER TABLE schedules ADD COLUMN IF NOT EXISTS config_version BIGINT NOT NULL DEFAULT 0;
	ALTER TABLE schedules ADD COLUMN IF NOT EXISTS history_complete_from TIMESTAMPTZ;
	ALTER TABLE schedules ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

	-- Mutable config columns are no longer written when a schedule root is
	-- created; l1_rotation_start would otherwise reject the insert.
	ALTER TABLE schedules ALTER COLUMN l1_rotation_start DROP NOT NULL;

	ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

	-- ON DELETE RESTRICT: a schedule with history is never physically deleted
	-- (soft delete via schedules.deleted_at), so cascading the history away
	-- must be impossible at the schema level too.
	CREATE TABLE IF NOT EXISTS schedule_revisions (
		id             TEXT PRIMARY KEY,
		schedule_id    TEXT NOT NULL REFERENCES schedules(id) ON DELETE RESTRICT,
		version        BIGINT NOT NULL,
		snapshot       JSONB NOT NULL,
		effective_from TIMESTAMPTZ NOT NULL,
		effective_to   TIMESTAMPTZ,
		recorded_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		created_by     TEXT,
		change_reason  TEXT,
		change_summary JSONB,

		UNIQUE (schedule_id, version),
		CONSTRAINT schedule_revisions_version_positive CHECK (version >= 1),
		CHECK (effective_to IS NULL OR effective_to > effective_from)
	);
	DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'schedule_revisions_version_positive') THEN
			ALTER TABLE schedule_revisions ADD CONSTRAINT schedule_revisions_version_positive CHECK (version >= 1);
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
	DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'no_overlapping_schedule_revisions') THEN
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

	-- Schedule domain events, written in the same transaction as the revision
	-- they describe. Delivery columns are intentionally absent: consumers are
	-- internal, event_outbox stays bound to alert_groups.
	CREATE TABLE IF NOT EXISTS schedule_events (
		id          TEXT PRIMARY KEY,
		schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE RESTRICT,
		event_type  TEXT NOT NULL,
		payload     JSONB NOT NULL DEFAULT '{}',
		recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_schedule_events_schedule
		ON schedule_events(schedule_id, recorded_at);

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

	return nil
}

// ========================================
// Alert Group CRUD (renamed from Incident)
// ========================================

// alertGroupColumns is the standard SELECT clause for alert_groups.
// All query functions should use this to ensure consistent column ordering.
const alertGroupColumns = `id, dedup_key, status, title, team_id, team_name_snapshot, severity, policy_id, current_step,
	external_url, alerts_data, policy_snapshot, oncall_snapshot,
	created_at, updated_at, resolved_at, acknowledged_by, resolved_by, ack_processed_at, slack_update_pending`

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
		&ag.ID, &ag.DedupKey, &ag.Status, &ag.Title,
		&teamID, &teamNameSnapshot, &severity, &policyID, &ag.CurrentStep,
		&externalURL, &alertsData,
		&policySnapshot, &oncallSnapshot,
		&ag.CreatedAt, &ag.UpdatedAt, &resolvedAt, &acknowledgedBy, &resolvedBy, &ackProcessedAt,
		&ag.SlackUpdatePending,
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

	// Unmarshal JSON fields
	if alertsData.Valid && alertsData.String != "" {
		_ = json.Unmarshal([]byte(alertsData.String), &ag.Alerts)
	}
	if policySnapshot.Valid && policySnapshot.String != "" {
		_ = json.Unmarshal([]byte(policySnapshot.String), &ag.PolicySnapshot)
	}
	if oncallSnapshot.Valid && oncallSnapshot.String != "" {
		_ = json.Unmarshal([]byte(oncallSnapshot.String), &ag.OnCallSnapshot)
	}

	return &ag, nil
}

func (s *Store) GetActiveAlertGroup(dedupKey string) (*model.AlertGroup, error) {
	query := `SELECT ` + alertGroupColumns + `
			  FROM alert_groups
			  WHERE dedup_key = $1 AND status NOT IN ($2, $3)`

	row := s.db.QueryRow(query, dedupKey, model.AlertGroupStatusResolved, model.AlertGroupStatusClosed)
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

	query := `INSERT INTO alert_groups (id, dedup_key, status, title, team_id, team_name_snapshot, severity, policy_id, current_step, external_url, alerts_data, policy_snapshot, acknowledged_by, resolved_by, created_at, updated_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`

	var snapshotVal sql.NullString
	if len(snapshotJson) > 0 {
		snapshotVal = sql.NullString{String: string(snapshotJson), Valid: true}
	}

	_, err := s.db.Exec(query, ag.ID, ag.DedupKey, ag.Status, ag.Title, ag.TeamID, ag.TeamNameSnapshot, ag.Severity, ag.PolicyID, ag.CurrentStep, ag.ExternalURL, string(alertsJson), snapshotVal, ag.AcknowledgedBy, ag.ResolvedBy, ag.CreatedAt, ag.UpdatedAt)
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
		`INSERT INTO alert_groups (id, dedup_key, status, title, team_id, team_name_snapshot, severity, policy_id, current_step, external_url, alerts_data, policy_snapshot, acknowledged_by, resolved_by, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		ag.ID, ag.DedupKey, ag.Status, ag.Title, ag.TeamID, ag.TeamNameSnapshot, ag.Severity, ag.PolicyID, ag.CurrentStep,
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

func (s *Store) UpdateAlertGroupStatus(id string, status model.AlertGroupStatus) error {
	query := `UPDATE alert_groups SET status = $1, updated_at = $2 WHERE id = $3`

	if status == model.AlertGroupStatusResolved {
		query = `UPDATE alert_groups SET status = $1, updated_at = $2, resolved_at = $3 WHERE id = $4`
		_, err := s.db.Exec(query, status, time.Now(), time.Now(), id)
		return err
	}

	_, err := s.db.Exec(query, status, time.Now(), id)
	return err
}

// TransitionAlertGroupStatus conditionally updates status only if the current status
// matches fromStatus (CAS semantics). Returns (true, nil) if the row was updated,
// (false, nil) if the current status did not match, or (false, err) on DB error.
func (s *Store) TransitionAlertGroupStatus(id string, fromStatus, toStatus model.AlertGroupStatus) (bool, error) {
	now := time.Now()
	res, err := s.db.Exec(
		`UPDATE alert_groups SET status = $1, updated_at = $2 WHERE id = $3 AND status = $4`,
		toStatus, now, id, fromStatus,
	)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func (s *Store) UpdateAlertGroupAcknowledged(id string, acknowledgedBy string) error {
	// Clear ack_processed_at to allow re-processing on re-ack (retry scenario)
	query := `UPDATE alert_groups SET status = $1, acknowledged_by = $2, ack_processed_at = NULL, updated_at = $3 WHERE id = $4`
	_, err := s.db.Exec(query, model.AlertGroupStatusAcknowledged, acknowledgedBy, time.Now(), id)
	return err
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
func (s *Store) AckAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent, dedupKey string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now()

	// 1. Conditional UPDATE — from 'processing' or 'triggered' (single-winner semantics)
	res, err := tx.Exec(
		`UPDATE alert_groups
		 SET status = $1, acknowledged_by = $2, ack_processed_at = NULL, updated_at = $3
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

	// 4. Cancel escalation job (same TX)
	if dedupKey != "" {
		if err := cancelJobByDedupKeyTx(tx, dedupKey); err != nil {
			return false, err
		}
	}

	return true, tx.Commit()
}

// ResolveAlertGroupAtomic atomically resolves an alert group.
// Timeline event and status update happen in a single transaction.
// Returns (true, nil) if the resolve was applied, (false, nil) if the alert group
// was not in 'processing', 'triggered', or 'acknowledged' status, or (false, err) on failure.
func (s *Store) ResolveAlertGroupAtomic(id, actor string, meta map[string]string, outboxEvent *model.OutboxEvent, dedupKey string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now()

	// 1. Conditional UPDATE — from 'processing', 'triggered', or 'acknowledged'
	res, err := tx.Exec(
		`UPDATE alert_groups
		 SET status = $1, resolved_at = $2, resolved_by = $3, updated_at = $2
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

	// 4. Cancel escalation job (same TX)
	if dedupKey != "" {
		if err := cancelJobByDedupKeyTx(tx, dedupKey); err != nil {
			return false, err
		}
	}

	return true, tx.Commit()
}

// ResolveAlertGroupWithAlertsAtomic atomically resolves an alert group while updating its alerts data.
// Used by the ingester when all incoming alerts are resolved (auto-resolve).
// Allows transition from new/processing/triggered/acknowledged → resolved.
// Returns (true, nil) if applied, (false, nil) if already resolved (idempotent).
func (s *Store) ResolveAlertGroupWithAlertsAtomic(id string, alerts []model.Alert, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent, dedupKey string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now()
	alertsJSON, _ := json.Marshal(alerts)

	// 1. Conditional UPDATE — alerts_data + status + resolved fields
	res, err := tx.Exec(
		`UPDATE alert_groups
		 SET alerts_data = $1, status = $2, resolved_by = 'system', resolved_at = $3, updated_at = $3
		 WHERE id = $4 AND status IN ($5, $6, $7, $8)`,
		string(alertsJSON), model.AlertGroupStatusResolved, now, id,
		model.AlertGroupStatusNew, model.AlertGroupStatusProcessing,
		model.AlertGroupStatusTriggered, model.AlertGroupStatusAcknowledged,
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
			return false, err
		}
	}

	// 3. INSERT outbox event
	if err := insertOutboxEventTx(tx, outboxEvent); err != nil {
		return false, err
	}

	// 4. Cancel escalation job
	if dedupKey != "" {
		if err := cancelJobByDedupKeyTx(tx, dedupKey); err != nil {
			return false, err
		}
	}

	return true, tx.Commit()
}

// cancelJobByDedupKeyTx cancels an active escalation job and its pending steps within the given transaction.
func cancelJobByDedupKeyTx(tx *sql.Tx, dedupKey string) error {
	var jobID string
	err := tx.QueryRow(`
		UPDATE jobs SET status='canceled', canceled_at=NOW(), finished_at=NOW(), updated_at=NOW()
		WHERE dedup_key=$1 AND status IN ('pending','running')
		RETURNING id`, dedupKey).Scan(&jobID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE job_stages SET status='canceled', updated_at=NOW()
		WHERE job_id=$1 AND status IN ('active','blocked')`, jobID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE job_steps SET status='canceled', updated_at=NOW()
		WHERE job_id=$1 AND status IN ('pending','blocked','retry')`, jobID)
	return err
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

func (s *Store) UpdateAlertGroupAlerts(id string, alerts []model.Alert) error {
	data, _ := json.Marshal(alerts)
	query := `UPDATE alert_groups SET alerts_data = $1, updated_at = $2 WHERE id = $3`
	_, err := s.db.Exec(query, string(data), time.Now(), id)
	return err
}

func (s *Store) GetNewAlertGroups() ([]*model.AlertGroup, error) {
	// Also pick up stale "processing" AGs orphaned by a crash between
	// status update and job creation — but ONLY if no escalation job exists.
	// If any escalation job exists (succeeded, failed, canceled, etc.), the AG was
	// already processed and should not spawn a duplicate job.
	staleThreshold := time.Now().Add(-30 * time.Second)
	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups ag
	          WHERE ag.status = $1
	             OR (ag.status = $2 AND ag.updated_at < $3
	                 AND NOT EXISTS (
	                     SELECT 1 FROM jobs j
	                     WHERE j.alert_group_id = ag.id
	                       AND j.type = 'escalation'
	                 ))`

	rows, err := s.db.Query(query, model.AlertGroupStatusNew, model.AlertGroupStatusProcessing, staleThreshold)
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

func (s *Store) GetAcknowledgedAlertGroups() ([]*model.AlertGroup, error) {
	// Only return acknowledged alert groups that haven't been processed yet
	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE status = $1 AND ack_processed_at IS NULL`

	rows, err := s.db.Query(query, model.AlertGroupStatusAcknowledged)
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

func (s *Store) GetResolvedAlertGroups() ([]*model.AlertGroup, error) {
	alertGroups, err := s.getAlertGroupsByStatus(model.AlertGroupStatusResolved)
	if err != nil {
		return nil, err
	}
	if err := s.populateTimelineForAlertGroups(alertGroups); err != nil {
		return nil, err
	}
	return alertGroups, nil
}

func (s *Store) MarkAckProcessed(agID string) error {
	_, err := s.db.Exec(`
		UPDATE alert_groups
		SET ack_processed_at = NOW(), updated_at = NOW()
		WHERE id = $1
	`, agID)
	return err
}

func (s *Store) SetSlackUpdatePending(id string, pending bool) error {
	_, err := s.db.Exec(`
		UPDATE alert_groups SET slack_update_pending = $1, updated_at = $2 WHERE id = $3
	`, pending, time.Now(), id)
	return err
}

func (s *Store) GetAlertGroupsPendingSlackUpdate() ([]*model.AlertGroup, error) {
	query := `SELECT ` + alertGroupColumns + ` FROM alert_groups WHERE slack_update_pending = TRUE AND status IN ($1, $2, $3)`

	rows, err := s.db.Query(query, model.AlertGroupStatusProcessing, model.AlertGroupStatusAcknowledged, model.AlertGroupStatusTriggered)
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
const alertGroupSummaryColumns = `id, dedup_key, status, title, team_id, severity, current_step,
	oncall_snapshot, external_url, acknowledged_by, resolved_by,
	created_at, updated_at, resolved_at,
	jsonb_array_length(COALESCE(alerts_data::jsonb, '[]'::jsonb)),
	(SELECT count(*)::int FROM jsonb_array_elements(COALESCE(alerts_data::jsonb, '[]'::jsonb)) elem WHERE elem->>'status' = 'firing')`

func scanAlertGroupSummaryRow(scanner alertGroupScanner) (*model.AlertGroupSummary, error) {
	var ag model.AlertGroupSummary
	var resolvedAt sql.NullTime
	var teamID, severity, externalURL, oncallSnapshot, acknowledgedBy, resolvedBy sql.NullString

	err := scanner.Scan(
		&ag.ID, &ag.DedupKey, &ag.Status, &ag.Title,
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

func (s *Store) DeleteTeam(id string) error {
	if _, err := s.db.Exec(`DELETE FROM team_members WHERE team_id = $1`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM teams WHERE id = $1`, id)
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

func (s *Store) GetUserByID(id string) (*model.User, error) {
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at FROM users WHERE id = $1`
	row := s.db.QueryRow(query, id)

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

func (s *Store) GetAllUsers() ([]*model.User, error) {
	query := `SELECT id, email, name, role, password_hash, auth_provider, created_at FROM users ORDER BY name`
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

func (s *Store) AddTeamMember(teamID, userID string, role model.TeamMemberRole) error {
	query := `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)
			  ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`
	_, err := s.db.Exec(query, teamID, userID, role)
	return err
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

func (s *Store) UpdateUser(u *model.User) error {
	query := `UPDATE users SET email = $1, name = $2, role = $3 WHERE id = $4`
	_, err := s.db.Exec(query, u.Email, u.Name, u.Role, u.ID)
	return err
}

func (s *Store) UpdateUserPassword(id, hash string) error {
	query := `UPDATE users SET password_hash = $1 WHERE id = $2`
	_, err := s.db.Exec(query, hash, id)
	return err
}

func (s *Store) UpdateUserAuthProvider(id, provider string) error {
	query := `UPDATE users SET auth_provider = $1 WHERE id = $2`
	_, err := s.db.Exec(query, provider, id)
	return err
}

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

// SetUserRole updates a user's global role with transaction protection for last admin.
func (s *Store) SetUserRole(userID string, role model.UserRole) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Get current role of target user
	var currentRole string
	err = tx.QueryRow(`SELECT role FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&currentRole)
	if err != nil {
		return err
	}

	// If demoting from admin, lock ALL admin users and count them
	if currentRole == string(model.UserRoleAdmin) && role != model.UserRoleAdmin {
		// Lock all admin users to prevent parallel demotions
		rows, err := tx.Query(`SELECT id FROM users WHERE role = 'admin' FOR UPDATE`)
		if err != nil {
			return err
		}
		adminCount := 0
		for rows.Next() {
			var id string
			rows.Scan(&id)
			adminCount++
		}
		rows.Close()

		if adminCount <= 1 {
			return fmt.Errorf("cannot demote the last admin")
		}
	}

	_, err = tx.Exec(`UPDATE users SET role = $1 WHERE id = $2`, role, userID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// CountAdmins returns the number of users with admin role.
func (s *Store) CountAdmins() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&count)
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

// ========================================
// Schedule CRUD (Phase 3)
// ========================================

func (s *Store) CreateSchedule(sch *model.Schedule) error {
	if sch.CreatedAt.IsZero() {
		sch.CreatedAt = time.Now()
	}
	sch.UpdatedAt = sch.CreatedAt

	var l2RotationType interface{} = sch.L2RotationType
	if sch.L2RotationType == "" {
		l2RotationType = nil
	}
	var l2HandoffTime interface{} = sch.L2HandoffTime
	if sch.L2HandoffTime == "" {
		l2HandoffTime = nil
	}

	query := `INSERT INTO schedules (id, team_id, timezone, l1_rotation_type, l1_handoff_time, l1_handoff_day, l1_rotation_start, l2_enabled, l2_escalation_timeout_min, l2_rotation_type, l2_handoff_time, l2_handoff_day, l2_rotation_start, slack_usergroup_id, created_at, updated_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`
	_, err := s.db.Exec(query, sch.ID, sch.TeamID, sch.Timezone, sch.L1RotationType, sch.L1HandoffTime, sch.L1HandoffDay, sch.L1RotationStart, sch.L2Enabled, sch.L2EscalationTimeout, l2RotationType, l2HandoffTime, sch.L2HandoffDay, sch.L2RotationStart, nullIfEmpty(sch.SlackUsergroupID), sch.CreatedAt, sch.UpdatedAt)
	return err
}

func (s *Store) GetScheduleByTeamID(teamID string) (*model.Schedule, error) {
	query := `SELECT id, team_id, timezone, l1_rotation_type, to_char(l1_handoff_time, 'HH24:MI'), l1_handoff_day, l1_rotation_start, l2_enabled, l2_escalation_timeout_min, l2_rotation_type, to_char(l2_handoff_time, 'HH24:MI'), l2_handoff_day, l2_rotation_start, slack_usergroup_id, created_at, updated_at
			  FROM schedules WHERE team_id = $1`
	return s.scanSchedule(s.db.QueryRow(query, teamID))
}

func (s *Store) GetScheduleByID(id string) (*model.Schedule, error) {
	query := `SELECT id, team_id, timezone, l1_rotation_type, to_char(l1_handoff_time, 'HH24:MI'), l1_handoff_day, l1_rotation_start, l2_enabled, l2_escalation_timeout_min, l2_rotation_type, to_char(l2_handoff_time, 'HH24:MI'), l2_handoff_day, l2_rotation_start, slack_usergroup_id, created_at, updated_at
			  FROM schedules WHERE id = $1`
	return s.scanSchedule(s.db.QueryRow(query, id))
}

func (s *Store) scanSchedule(row *sql.Row) (*model.Schedule, error) {
	var sch model.Schedule
	var l1HandoffDay, l2HandoffDay sql.NullInt64
	var l2RotationType, l2HandoffTime, slackUsergroupID sql.NullString
	var l2RotationStart sql.NullTime

	err := row.Scan(&sch.ID, &sch.TeamID, &sch.Timezone, &sch.L1RotationType, &sch.L1HandoffTime, &l1HandoffDay, &sch.L1RotationStart, &sch.L2Enabled, &sch.L2EscalationTimeout, &l2RotationType, &l2HandoffTime, &l2HandoffDay, &l2RotationStart, &slackUsergroupID, &sch.CreatedAt, &sch.UpdatedAt)
	if err != nil {
		return nil, err
	}

	if l1HandoffDay.Valid {
		d := int(l1HandoffDay.Int64)
		sch.L1HandoffDay = &d
	}
	if l2HandoffDay.Valid {
		d := int(l2HandoffDay.Int64)
		sch.L2HandoffDay = &d
	}
	sch.L2RotationType = model.RotationType(l2RotationType.String)
	sch.L2HandoffTime = l2HandoffTime.String
	if l2RotationStart.Valid {
		sch.L2RotationStart = &l2RotationStart.Time
	}
	sch.SlackUsergroupID = slackUsergroupID.String

	return &sch, nil
}

func (s *Store) UpdateSchedule(sch *model.Schedule) error {
	sch.UpdatedAt = time.Now()
	query := `UPDATE schedules SET timezone = $1, l1_rotation_type = $2, l1_handoff_time = $3, l1_handoff_day = $4, l1_rotation_start = $5, l2_enabled = $6, l2_escalation_timeout_min = $7, l2_rotation_type = $8, l2_handoff_time = $9, l2_handoff_day = $10, l2_rotation_start = $11, slack_usergroup_id = $12, updated_at = $13 WHERE id = $14`
	_, err := s.db.Exec(query, sch.Timezone, sch.L1RotationType, sch.L1HandoffTime, sch.L1HandoffDay, sch.L1RotationStart, sch.L2Enabled, sch.L2EscalationTimeout, sch.L2RotationType, sch.L2HandoffTime, sch.L2HandoffDay, sch.L2RotationStart, nullIfEmpty(sch.SlackUsergroupID), sch.UpdatedAt, sch.ID)
	return err
}

func (s *Store) DeleteSchedule(id string) error {
	_, err := s.db.Exec(`DELETE FROM schedules WHERE id = $1`, id)
	return err
}

func (s *Store) SetScheduleUsers(scheduleID, layer string, userIDs []string) error {
	// Use transaction for atomic epoch transition
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	// Close current epoch if exists (with error checking)
	closeQuery := `UPDATE rotation_epochs SET end_time = $1 WHERE schedule_id = $2 AND layer = $3 AND end_time IS NULL`
	if _, err := tx.Exec(closeQuery, now, scheduleID, layer); err != nil {
		return err
	}

	// Create new epoch with the new user order (wrap each user as a singleton group)
	if len(userIDs) > 0 {
		groups := make([][]string, len(userIDs))
		for i, id := range userIDs {
			groups[i] = []string{id}
		}
		groupsJSON, _ := json.Marshal(groups)
		insertQuery := `INSERT INTO rotation_epochs (id, schedule_id, layer, user_ids, start_time, end_time, created_at)
					    VALUES ($1, $2, $3, $4, $5, $6, $7)`
		_, err := tx.Exec(insertQuery, generateUUID(), scheduleID, layer, string(groupsJSON), now, nil, now)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
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

// GetAllSchedules returns all schedules.
func (s *Store) GetAllSchedules() ([]*model.Schedule, error) {
	return s.querySchedules(`SELECT id, team_id, timezone, l1_rotation_type, to_char(l1_handoff_time, 'HH24:MI'), l1_handoff_day, l1_rotation_start, l2_enabled, l2_escalation_timeout_min, l2_rotation_type, to_char(l2_handoff_time, 'HH24:MI'), l2_handoff_day, l2_rotation_start, slack_usergroup_id, created_at, updated_at
			  FROM schedules`)
}

// GetSchedulesWithUsergroup returns all schedules that have slack_usergroup_id configured
func (s *Store) GetSchedulesWithUsergroup() ([]*model.Schedule, error) {
	return s.querySchedules(`SELECT id, team_id, timezone, l1_rotation_type, to_char(l1_handoff_time, 'HH24:MI'), l1_handoff_day, l1_rotation_start, l2_enabled, l2_escalation_timeout_min, l2_rotation_type, to_char(l2_handoff_time, 'HH24:MI'), l2_handoff_day, l2_rotation_start, slack_usergroup_id, created_at, updated_at
			  FROM schedules WHERE slack_usergroup_id IS NOT NULL AND slack_usergroup_id != ''`)
}

func (s *Store) querySchedules(query string) ([]*model.Schedule, error) {
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var schedules []*model.Schedule
	for rows.Next() {
		var sch model.Schedule
		var l1HandoffDay, l2HandoffDay sql.NullInt64
		var l2RotationType, l2HandoffTime, slackUsergroupID sql.NullString
		var l2RotationStart sql.NullTime

		if err := rows.Scan(&sch.ID, &sch.TeamID, &sch.Timezone, &sch.L1RotationType, &sch.L1HandoffTime, &l1HandoffDay, &sch.L1RotationStart, &sch.L2Enabled, &sch.L2EscalationTimeout, &l2RotationType, &l2HandoffTime, &l2HandoffDay, &l2RotationStart, &slackUsergroupID, &sch.CreatedAt, &sch.UpdatedAt); err != nil {
			return nil, err
		}

		if l1HandoffDay.Valid {
			d := int(l1HandoffDay.Int64)
			sch.L1HandoffDay = &d
		}
		if l2HandoffDay.Valid {
			d := int(l2HandoffDay.Int64)
			sch.L2HandoffDay = &d
		}
		sch.L2RotationType = model.RotationType(l2RotationType.String)
		sch.L2HandoffTime = l2HandoffTime.String
		if l2RotationStart.Valid {
			sch.L2RotationStart = &l2RotationStart.Time
		}
		sch.SlackUsergroupID = slackUsergroupID.String

		schedules = append(schedules, &sch)
	}
	return schedules, nil
}

func (s *Store) GetScheduleUsers(scheduleID, layer string) ([]*model.User, error) {
	// Get current epoch for this schedule/layer
	epoch, err := s.GetCurrentEpoch(scheduleID, layer)
	if err != nil {
		if err == sql.ErrNoRows {
			return []*model.User{}, nil // No epoch = no users
		}
		return nil, err
	}

	// Flatten groups to flat user list (for L2 which is always single-user groups)
	var users []*model.User
	for _, group := range epoch.Groups {
		for _, userID := range group {
			u, err := s.GetUserByID(userID)
			if err != nil {
				return nil, fmt.Errorf("user %s not found in rotation: %w", userID, err)
			}
			users = append(users, u)
		}
	}
	return users, nil
}

func (s *Store) SetScheduleGroups(scheduleID string, groups [][]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()

	closeQuery := `UPDATE rotation_epochs SET end_time = $1 WHERE schedule_id = $2 AND layer = $3 AND end_time IS NULL`
	if _, err := tx.Exec(closeQuery, now, scheduleID, "l1"); err != nil {
		return err
	}

	if len(groups) > 0 {
		groupsJSON, _ := json.Marshal(groups)
		insertQuery := `INSERT INTO rotation_epochs (id, schedule_id, layer, user_ids, start_time, end_time, created_at)
					    VALUES ($1, $2, $3, $4, $5, $6, $7)`
		_, err := tx.Exec(insertQuery, generateUUID(), scheduleID, "l1", string(groupsJSON), now, nil, now)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) GetScheduleGroups(scheduleID, layer string) ([][]*model.User, error) {
	epoch, err := s.GetCurrentEpoch(scheduleID, layer)
	if err != nil {
		if err == sql.ErrNoRows {
			return [][]*model.User{}, nil
		}
		return nil, err
	}

	var result [][]*model.User
	for _, group := range epoch.Groups {
		var users []*model.User
		for _, userID := range group {
			u, err := s.GetUserByID(userID)
			if err != nil {
				return nil, fmt.Errorf("user %s not found in rotation: %w", userID, err)
			}
			users = append(users, u)
		}
		result = append(result, users)
	}
	return result, nil
}

// ========================================
// Schedule Overrides
// ========================================

func (s *Store) CreateScheduleOverride(o *model.ScheduleOverride) error {
	if o.CreatedAt.IsZero() {
		o.CreatedAt = time.Now()
	}
	query := `INSERT INTO schedule_overrides (id, schedule_id, user_id, start_time, end_time, reason, created_by, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	_, err := s.db.Exec(query, o.ID, o.ScheduleID, o.UserID, o.StartTime, o.EndTime, o.Reason, o.CreatedBy, o.CreatedAt)
	return err
}

func (s *Store) GetScheduleOverrides(scheduleID string, from, until time.Time) ([]*model.ScheduleOverride, error) {
	query := `SELECT o.id, o.schedule_id, o.user_id, o.start_time, o.end_time, o.reason, o.created_by, o.created_at,
			         u.id, u.email, u.name
			  FROM schedule_overrides o
			  JOIN users u ON o.user_id = u.id
			  WHERE o.schedule_id = $1 AND o.start_time < $2 AND o.end_time > $3
			  ORDER BY o.start_time`
	rows, err := s.db.Query(query, scheduleID, until, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var overrides []*model.ScheduleOverride
	for rows.Next() {
		var o model.ScheduleOverride
		var reason, createdBy, email sql.NullString
		var u model.User

		if err := rows.Scan(&o.ID, &o.ScheduleID, &o.UserID, &o.StartTime, &o.EndTime, &reason, &createdBy, &o.CreatedAt,
			&u.ID, &email, &u.Name); err != nil {
			return nil, err
		}
		u.Email = email.String
		o.Reason = reason.String
		o.CreatedBy = createdBy.String
		o.User = &u
		overrides = append(overrides, &o)
	}
	return overrides, nil
}

// OverrideBelongsToSchedule checks if an override belongs to a schedule
func (s *Store) OverrideBelongsToSchedule(overrideID, scheduleID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM schedule_overrides WHERE id = $1 AND schedule_id = $2)`, overrideID, scheduleID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *Store) DeleteScheduleOverride(id string) error {
	_, err := s.db.Exec(`DELETE FROM schedule_overrides WHERE id = $1`, id)
	return err
}

func (s *Store) UpdateScheduleOverride(o *model.ScheduleOverride) error {
	query := `UPDATE schedule_overrides SET user_id = $1, start_time = $2, end_time = $3, reason = $4
			  WHERE id = $5`
	res, err := s.db.Exec(query, o.UserID, o.StartTime, o.EndTime, o.Reason, o.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ========================================
// Rotation Epochs
// ========================================

// CreateRotationEpoch creates a new rotation epoch
func (s *Store) CreateRotationEpoch(epoch *model.RotationEpoch) error {
	if epoch.CreatedAt.IsZero() {
		epoch.CreatedAt = time.Now()
	}
	groupsJSON, _ := json.Marshal(epoch.Groups)
	query := `INSERT INTO rotation_epochs (id, schedule_id, layer, user_ids, start_time, end_time, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := s.db.Exec(query, epoch.ID, epoch.ScheduleID, epoch.Layer, string(groupsJSON), epoch.StartTime, epoch.EndTime, epoch.CreatedAt)
	return err
}

// CloseCurrentEpoch closes the current epoch for a schedule/layer by setting end_time
func (s *Store) CloseCurrentEpoch(scheduleID, layer string, endTime time.Time) error {
	query := `UPDATE rotation_epochs SET end_time = $1 WHERE schedule_id = $2 AND layer = $3 AND end_time IS NULL`
	_, err := s.db.Exec(query, endTime, scheduleID, layer)
	return err
}

// GetRotationEpochs returns epochs that overlap with the given time range
func (s *Store) GetRotationEpochs(scheduleID, layer string, from, until time.Time) ([]*model.RotationEpoch, error) {
	query := `SELECT id, schedule_id, layer, user_ids, start_time, end_time, created_at
			  FROM rotation_epochs
			  WHERE schedule_id = $1 AND layer = $2
			    AND start_time < $3
			    AND (end_time IS NULL OR end_time > $4)
			  ORDER BY start_time`
	rows, err := s.db.Query(query, scheduleID, layer, until, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var epochs []*model.RotationEpoch
	for rows.Next() {
		var e model.RotationEpoch
		var userIDsJSON string
		var endTime sql.NullTime

		if err := rows.Scan(&e.ID, &e.ScheduleID, &e.Layer, &userIDsJSON, &e.StartTime, &endTime, &e.CreatedAt); err != nil {
			return nil, err
		}

		_ = json.Unmarshal([]byte(userIDsJSON), &e.Groups)
		if endTime.Valid {
			e.EndTime = &endTime.Time
		}
		epochs = append(epochs, &e)
	}
	return epochs, nil
}

// GetCurrentEpoch returns the current (open) epoch for a schedule/layer
func (s *Store) GetCurrentEpoch(scheduleID, layer string) (*model.RotationEpoch, error) {
	query := `SELECT id, schedule_id, layer, user_ids, start_time, end_time, created_at
			  FROM rotation_epochs
			  WHERE schedule_id = $1 AND layer = $2 AND end_time IS NULL
			  ORDER BY start_time DESC LIMIT 1`
	row := s.db.QueryRow(query, scheduleID, layer)

	var e model.RotationEpoch
	var userIDsJSON string
	var endTime sql.NullTime

	err := row.Scan(&e.ID, &e.ScheduleID, &e.Layer, &userIDsJSON, &e.StartTime, &endTime, &e.CreatedAt)
	if err != nil {
		return nil, err
	}

	_ = json.Unmarshal([]byte(userIDsJSON), &e.Groups)
	if endTime.Valid {
		e.EndTime = &endTime.Time
	}
	return &e, nil
}

// ========================================
// API Tokens CRUD
// ========================================

// CreateAPIToken creates a new API token
func (s *Store) CreateAPIToken(token *model.APIToken) error {
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}
	query := `INSERT INTO api_tokens (id, user_id, name, token_hash, expires_at, created_at) 
			  VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := s.db.Exec(query, token.ID, token.UserID, token.Name, token.TokenHash, token.ExpiresAt, token.CreatedAt)
	return err
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

func (s *Store) GetAPITokenByHash(hash string) (*model.APIToken, error) {
	query := `SELECT id, user_id, name, token_hash, expires_at, last_used_at, created_at 
			  FROM api_tokens WHERE token_hash = $1`
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
func (s *Store) GetMetricsSnapshot() (*MetricsSnapshot, error) {
	snap := &MetricsSnapshot{}

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
		var c AlertGroupCount
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
		var c AlertGroupStatusCount
		if err := rows2.Scan(&c.TeamID, &c.Severity, &c.Status, &c.Count); err != nil {
			return nil, err
		}
		snap.AlertGroupsByStatus = append(snap.AlertGroupsByStatus, c)
	}
	if err := rows2.Err(); err != nil {
		return nil, err
	}

	// 2. Teams without on-call (no schedule, no open L1 epoch, or empty L1 rotation)
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM teams t
		LEFT JOIN schedules s ON t.id = s.team_id
		LEFT JOIN rotation_epochs re ON s.id = re.schedule_id AND re.layer = 'l1' AND re.end_time IS NULL
			AND json_array_length(re.user_ids::json) > 0
		WHERE s.id IS NULL OR re.id IS NULL`).Scan(&snap.TeamsWithoutOnCall)
	if err != nil {
		return nil, fmt.Errorf("teams without oncall query: %w", err)
	}

	// 3. Teams with permanent on-call (single user in L1 rotation)
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM rotation_epochs re
		JOIN schedules s ON re.schedule_id = s.id
		WHERE re.layer = 'l1' AND re.end_time IS NULL
		AND json_array_length(re.user_ids::json) = 1`).Scan(&snap.TeamsWithPermanentOnCall)
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
		var c StatusCount
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
		var c StatusCount
		if err := rows4.Scan(&c.Status, &c.Count); err != nil {
			return nil, err
		}
		snap.OutboxDeliveriesByStatus = append(snap.OutboxDeliveriesByStatus, c)
	}
	if err := rows4.Err(); err != nil {
		return nil, err
	}

	return snap, nil
}
