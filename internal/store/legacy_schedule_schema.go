package store

// Pre-revision schedule schema.
//
// These tables and columns are no longer read or written by anything at
// runtime: the revision model owns the configuration, and the destructive
// upgrade reset empties the rows. They are NOT dropped - that is a separate
// cleanup once the release has settled - so the DDL has to keep running, and a
// fresh install still gets the same shape an upgraded one has.
//
// It lives here, apart from InitDB, for a reason that outlives the code: the
// check that the runtime has stopped touching legacy is a grep for the table
// and column names, and inside a multi-line CREATE TABLE there is nothing on
// the line of a column to distinguish it from live code. One file that is
// allowed to name them makes the check exact instead of advisory.
//
// The boundary is the era, not the table. `schedules` is very much alive - the
// revision model uses it as its aggregate root - but the statement that creates
// it is written in the old shape, with the mutable configuration columns
// inline, and splitting that statement would change the schema of a fresh
// install while changing nothing on an existing one. So the whole pre-revision
// block is here, and everything the revision model ADDED to `schedules`
// (config_version, history_complete_from, deleted_at) stays in InitDB.

const legacyScheduleDDL = `
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

// initLegacyScheduleSchema creates the pre-revision schedule schema and applies
// the migrations that reshaped it. InitDB calls it in the position the block
// used to occupy, so the execution order is unchanged.
func (s *Store) initLegacyScheduleSchema() error {
	if _, err := s.db.Exec(legacyScheduleDDL); err != nil {
		return err
	}

	// Written when the revision model stopped filling the mutable columns: the
	// NOT NULL on l1_rotation_start would otherwise reject a root insert. It
	// belongs with the column it is about rather than with the revision schema.
	if _, err := s.db.Exec(
		`ALTER TABLE schedules ALTER COLUMN l1_rotation_start DROP NOT NULL`); err != nil {
		return err
	}
	return nil
}
