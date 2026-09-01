-- Dropping what the webhook outbox worker left behind.
--
-- This file is NOT run by the application. TokayOps builds its schema on every
-- start and creates neither of these tables any more; a database upgraded from
-- an earlier release simply carries them, and the rows in them, with nothing
-- reading either. Removing them is a decision with a date on it, and this is
-- the statement of that decision rather than its execution.
--
-- Run it by hand, against a database that has already started at least once on
-- a build that delivers webhooks through the outbound domain, and after you are
-- sure you will not roll back to a build that does not.
--
-- There is no way back, and the way it fails is quiet. An older build creates
-- these tables itself on start-up, so it comes up perfectly well - with no
-- delivery history and, after the DELETE below, without the events it had
-- finished. It will not tell you anything is missing. Restoring a backup taken
-- before this ran is the only way back.
--
-- Webhook deliveries are commitments in the outbound tables now, and what they
-- did is in outbound_attempts and outbound_intent_events. The delivery history
-- from before the cutover is not carried over: after this file the delivery
-- list of every subscriber starts from the cutover.
--
--   psql "$DATABASE_URL" -1 -f migrations/drop-webhook-outbox.sql
--
-- One transaction (-1), so a failure anywhere leaves the database as it was.

-- Leaves before roots: the attempts reference the deliveries, and the
-- deliveries reference event_outbox and integrations (the start already removed
-- the key to integrations). Their indexes go with them.
DROP TABLE IF EXISTS event_outbox_delivery_attempts;
DROP TABLE IF EXISTS event_outbox_deliveries;

-- event_outbox itself stays: the alert transactions write it and the fan-out
-- reads it. What goes are the events the OLD worker finished. Their deliveries
-- were in the table above, and the new path neither reads these rows nor
-- writes their statuses - it marks an event fanned_out. Events still pending,
-- and events the old worker had claimed (processing) when it was stopped, are
-- kept: the fan-out picks both up.
DELETE FROM event_outbox WHERE status IN ('completed', 'failed');
