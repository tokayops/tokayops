package store

import (
	"context"
	"database/sql"
	"fmt"
)

// What retention needs of the schema: the moment an event was fanned out, and
// the two indexes the chunks walk.

// applyRetentionSchema adds fanned_out_at to the alert outbox, fills it for
// every event that is already finished, and builds the retention indexes.
//
// The age of an event is the moment of its fan-out, not of its creation: the
// fan-out takes pending events of any age, and an event that waited a month
// behind a stopped fan-out gets commitments with today's deadline. For an
// event this build fanned out, that moment is known exactly - the fan-out and
// the claim it admits are one transaction, so it is the claim's admitted_at,
// and the claim is found by the event_id the journal schema filled in. For
// the events the old worker finished, before there were claims, it is the
// best the row has: when it was sent, else when it was created. An event
// marked fanned out with no claim to date it from keeps no date, and the
// sweep never takes it.
func applyRetentionSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE event_outbox ADD COLUMN IF NOT EXISTS fanned_out_at TIMESTAMPTZ`); err != nil {
		return fmt.Errorf("add fanned_out_at to the outbox: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE event_outbox e
		SET fanned_out_at = b.admitted_at
		FROM outbound_batches b
		WHERE b.event_id = e.id AND b.key_kind = 'webhook_event'
		  AND e.status = 'fanned_out' AND e.fanned_out_at IS NULL`); err != nil {
		return fmt.Errorf("date the events this build fanned out: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE event_outbox
		SET fanned_out_at = COALESCE(sent_at, created_at)
		WHERE status IN ('completed', 'failed') AND fanned_out_at IS NULL`); err != nil {
		return fmt.Errorf("date the events the old worker finished: %w", err)
	}
	for _, index := range []struct{ what, sql string }{
		{
			what: "idx_outbound_intents_retention",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_intents_retention
				ON outbound_intents (updated_at, id)
				WHERE status IN (` + terminalStatusList + `)`,
		},
		{
			what: "idx_event_outbox_retention",
			sql: `CREATE INDEX IF NOT EXISTS idx_event_outbox_retention
				ON event_outbox (fanned_out_at, id)
				WHERE status IN (` + finishedOutboxStatusList + `)`,
		},
	} {
		if _, err := tx.ExecContext(ctx, index.sql); err != nil {
			return fmt.Errorf("create %s: %w", index.what, err)
		}
	}
	return nil
}
