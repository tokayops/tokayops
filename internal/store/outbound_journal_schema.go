package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The read side of the outbound domain needs one thing the tables did not
// have, and three indexes.
//
// The thing is the event a webhook claim is about. A fan-out claims the event
// under one key and a replay claims it under another - the event, the
// subscriber and the operator's idempotency key - so nothing derived from the
// event alone finds both, and the group's delivery history has to start from
// the events and reach every claim on them. The column is that link. It has no
// foreign key on purpose: a claim is never deleted and an event is, once no
// commitment is owed for it, so the claim outlives the event and keeps the
// event's id as a plain value.

const outboundBatchNamesEventConstraint = "outbound_batches_webhook_names_event"

// applyJournalSchema adds event_id to the claims, fills it on a database written
// before the column existed, and only then insists on it.
//
// The backfill reads the id out of the key - a webhook key is <event_id>:<hex>,
// and the grammar keeps ':' out of the id so the prefix is unambiguous - and it
// is CHECKED against the events table rather than trusted: a claim whose
// prefix names no event is identity data this build cannot read, and it stops
// the start naming the row, the way a payload that will not digest does. The
// check is possible exactly here, on the upgrade: retention has not run yet
// and no event has been removed from under its claims.
func applyJournalSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE outbound_batches ADD COLUMN IF NOT EXISTS event_id TEXT`); err != nil {
		return fmt.Errorf("add the event id to the claims: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_batches b
		SET event_id = split_part(b.batch_key, ':', 1)
		WHERE b.key_kind IN ('webhook_event', 'webhook_replay')
		  AND b.event_id IS NULL
		  AND EXISTS (SELECT 1 FROM event_outbox e WHERE e.id = split_part(b.batch_key, ':', 1))`); err != nil {
		return fmt.Errorf("fill the event id of the webhook claims: %w", err)
	}

	var unreadable, prefix string
	err := tx.QueryRowContext(ctx, `
		SELECT id, split_part(batch_key, ':', 1) FROM outbound_batches
		WHERE key_kind IN ('webhook_event', 'webhook_replay') AND event_id IS NULL
		ORDER BY id LIMIT 1`).Scan(&unreadable, &prefix)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("look for webhook claims that name no event: %w", err)
	}
	if err == nil {
		return fmt.Errorf(
			"webhook claim %s names event %q in its key, and no such event exists; "+
				"the claim cannot be read by this build and nothing can supply the "+
				"event after the fact. Repair or remove the row", unreadable, prefix)
	}

	for _, step := range []struct {
		what string
		sql  string
	}{
		{
			what: outboundBatchNamesEventConstraint,
			sql: guardedConstraint("outbound_batches", outboundBatchNamesEventConstraint,
				`CHECK ((key_kind IN ('webhook_event', 'webhook_replay')) = (event_id IS NOT NULL))`),
		},
		{
			// Every claim on an event, for the group's history and for the
			// retention that asks whether anything is still owed for it.
			what: "idx_outbound_batches_event",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_batches_event
			      ON outbound_batches (event_id) WHERE event_id IS NOT NULL`,
		},
		{
			// The commitments of one claim. A foreign-key column with no index,
			// which nothing looked up by until the journal did.
			what: "idx_outbound_intents_batch",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_intents_batch
			      ON outbound_intents (batch_id)`,
		},
		{
			// The operational journal: a period, newest first, with or without a
			// family or a status to narrow it. Without this a period alone reads
			// and sorts the whole window.
			what: "idx_outbound_intents_journal",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_intents_journal
			      ON outbound_intents (created_at DESC, id DESC)`,
		},
	} {
		if _, err := tx.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	return nil
}
