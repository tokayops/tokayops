-- Dropping what the job engine left behind.
--
-- This file is NOT run by the application. TokayOps builds its schema on every
-- start and creates none of these any more, so a database that still has them
-- simply carries rows nothing reads. Removing them is a decision with a date on
-- it, and this is the statement of that decision rather than its execution.
--
-- Run it by hand, against a database that has already started at least once on
-- a build that no longer writes to any of it, and after you are sure you will
-- not roll back to a build that does.
--
-- There is no way back, and the way it fails is quiet. An older build creates
-- these tables itself on start-up, so its process may well come up - on an
-- empty job engine, with none of the work or the history it expects to find,
-- and without the two alert_groups columns below, which it reads. That is not
-- a working rollback: its alert API and its engine fail on the first alert,
-- and nothing says why. Restoring a backup taken before the upgrade is the
-- only way back.
--
-- Escalations and shift-change announcements are commitments in the outbound
-- tables now, and what they did is in outbound_attempts and
-- outbound_intent_events. Nothing here is the history of that work; it is the
-- history of the machinery that used to carry it.
--
--   psql "$DATABASE_URL" -1 -f migrations/drop-job-engine.sql
--
-- One transaction (-1), so a failure anywhere leaves the database as it was.

-- Leaves before roots: job_steps references job_stages and jobs, and
-- notification_deliveries references job_steps. Dropped in any other order,
-- these fail on a foreign key rather than on anything meaningful.
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS job_steps;
DROP TABLE IF EXISTS job_stages;
DROP TABLE IF EXISTS jobs;

-- The policy table the dedup model kept, and the rows in it. Nothing declares a
-- family any more because nothing declares work under one.
DROP TABLE IF EXISTS job_dedup_policies;

-- Two columns on alert_groups that belonged to a loop that no longer exists.
--
-- slack_update_pending was the flag that loop read to decide a card needed
-- redrawing; what a message has to show is a revision of the alert group now,
-- and the worker applies it. ack_processed_at recorded when that loop had dealt
-- with an acknowledgement.
ALTER TABLE alert_groups
    DROP COLUMN IF EXISTS slack_update_pending,
    DROP COLUMN IF EXISTS ack_processed_at;
