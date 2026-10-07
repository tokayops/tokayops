package store

import (
	"context"
	"database/sql"
	"fmt"
)

// What a channel whose acceptance only means "queued" needs from the schema.
//
// A commitment that waits: the moment it is next asked about
// (receipt_timeout_at, reserved since the delivery tables were first built and
// read only now), and the moment the wait ends whatever was heard
// (receipt_deadline). The fact that what it was for no longer needs doing -
// an acknowledgement that arrived while its call was under way - kept apart
// from the status, because the call it made is not undone by it.
//
// outbound_effects is one row per external object an attempt made, keyed by
// the attempt: one attempt makes at most one, and a generation has as many as
// it had attempts that reached the provider. The name is the domain's - it
// does not know the object is a call.
//
// outbound_provider_events is the inbox: everything a provider said, written
// before it is understood, so an event is never lost to a failure between
// receiving it and applying it. An event id is unique only within a provider
// account, and a repeat of it is the same event.

const (
	outboundAwaitingHasReceipt = "outbound_intents_awaiting_has_receipt"
	outboundEffectStateKnown   = "outbound_effects_state_known"
	outboundEventStateKnown    = "outbound_provider_events_state_known"
)

func applyReceiptSchema(ctx context.Context, tx *sql.Tx) error {
	for _, step := range []struct{ what, sql string }{
		{
			what: "add the wait to the commitments",
			sql: `ALTER TABLE outbound_intents
				ADD COLUMN IF NOT EXISTS receipt_deadline TIMESTAMPTZ,
				ADD COLUMN IF NOT EXISTS obligation_withdrawn_at TIMESTAMPTZ,
				ADD COLUMN IF NOT EXISTS obligation_withdrawn_reason TEXT,
				ADD COLUMN IF NOT EXISTS obligation_withdrawn_actor TEXT`,
		},
		{
			// A commitment waits for the provider's word about something it
			// made, and knows when to ask next. Without either it is a wait for
			// nothing, at no time - a row nothing would ever pick up.
			what: "add " + outboundAwaitingHasReceipt,
			sql: guardedConstraint("outbound_intents", outboundAwaitingHasReceipt,
				`CHECK (status <> 'awaiting_receipt'
				        OR (receipt_recorded AND receipt_timeout_at IS NOT NULL
				            AND receipt_deadline IS NOT NULL))`),
		},
		{
			what: "add the poll's index",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_intents_awaiting
				ON outbound_intents (delivery_family, receipt_timeout_at)
				WHERE status = 'awaiting_receipt'`,
		},
		{
			what: "add the external objects",
			sql: `CREATE TABLE IF NOT EXISTS outbound_effects (
				attempt_id      TEXT PRIMARY KEY REFERENCES outbound_attempts(id),
				intent_id       TEXT NOT NULL REFERENCES outbound_intents(id),
				generation_no   INTEGER NOT NULL,
				external_ref    TEXT NOT NULL,
				state           TEXT NOT NULL,
				last_sequence   INTEGER,
				provider_status TEXT NOT NULL,
				updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
			)`,
		},
		{
			what: "add " + outboundEffectStateKnown,
			sql: guardedConstraint("outbound_effects", outboundEffectStateKnown,
				`CHECK (state IN ('in_progress', 'happened', 'not_placed', 'withdrawn'))`),
		},
		{
			what: "index the objects by commitment",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_effects_intent
				ON outbound_effects (intent_id)`,
		},
		{
			what: "add the inbox",
			sql: `CREATE TABLE IF NOT EXISTS outbound_provider_events (
				id              TEXT PRIMARY KEY,
				provider        TEXT NOT NULL,
				account_scope   TEXT NOT NULL,
				event_id        TEXT NOT NULL,
				attempt_id      TEXT,
				external_ref    TEXT,
				sequence        INTEGER,
				provider_status TEXT NOT NULL,
				summary         TEXT,
				occurred_at     TIMESTAMPTZ,
				received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
				state           TEXT NOT NULL DEFAULT 'unmatched',
				UNIQUE (provider, account_scope, event_id)
			)`,
		},
		{
			what: "add " + outboundEventStateKnown,
			sql: guardedConstraint("outbound_provider_events", outboundEventStateKnown,
				`CHECK (state IN ('unmatched', 'applied', 'ignored'))`),
		},
		{
			what: "index the inbox by what is left to apply",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_provider_events_unmatched
				ON outbound_provider_events (received_at) WHERE state = 'unmatched'`,
		},
		{
			what: "index the inbox by attempt",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_provider_events_attempt
				ON outbound_provider_events (attempt_id)`,
		},
	} {
		if _, err := tx.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	return nil
}
