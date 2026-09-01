package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The digest of what a commitment was admitted with, and the rules that keep a
// claim and its commitments describing one thing.
//
// All of it lands in the same transaction as the rest of the start-up schema.
// Half of it applied would be worse than none: the next start would find a
// column with no rule on it and no way to tell whether the values in it were
// ever checked.

const (
	outboundPayloadDigestLenConstraint = "outbound_intents_payload_digest_len"

	// The pair of columns that must agree inside one row, on both tables.
	//
	// The name carries the version of the vocabulary, and that is the whole
	// upgrade mechanism: a guarded constraint is added only when its NAME is
	// absent, so extending the CHECK under the old name would reach a fresh
	// database and never an existing one - which would keep the old rule and
	// refuse the first row of any family added since. A new vocabulary is a new
	// name plus the removal of the old.
	outboundBatchFamilyConstraint  = "outbound_batches_family_matches_kind_v2"
	outboundIntentFamilyConstraint = "outbound_intents_family_matches_kind_v2"

	// The names the previous vocabulary was held under. Dropped on every start;
	// a no-op once they are gone.
	outboundBatchFamilyConstraintV1  = "outbound_batches_family_matches_kind"
	outboundIntentFamilyConstraintV1 = "outbound_intents_family_matches_kind"

	// A webhook delivery's own three rules. The first two are the handover's
	// rules again, for the same reason: the business key names the event and the
	// subscriber and NOT the target kind or the provider, so a row could be
	// consistent with itself and still be aimed at a person or executed by a
	// worker with no channel for it.
	outboundWebhookSubscriberTarget   = "outbound_intents_webhook_targets_a_subscriber"
	outboundWebhookTargetAgreement    = "outbound_intents_webhook_payload_addresses_the_target"
	outboundWebhookProviderConstraint = "outbound_intents_webhook_provider"

	// The claim's identity, and the commitment that has to point at all of it.
	outboundBatchIdentityUnique = "outbound_batches_identity"
	outboundIntentBatchIdentity = "outbound_intents_batch_identity_fk"

	// A handover's own two rules.
	outboundHandoffTargetAgreement = "outbound_intents_handoff_payload_addresses_the_target"
	outboundHandoffPersonTarget    = "outbound_intents_handoff_targets_a_person"
)

// familyOfKindSQL is the pairing as the database states it, and it is the same
// pairing keys.FamilyOf makes. Written out rather than derived, because a CHECK
// cannot call Go - and stated in the database at all because the alternative is
// a row whose family and kind disagree, executed by the wrong worker, counted
// in the wrong series and alerted on by the wrong rule.
const familyOfKindSQL = `(
	(key_kind IN ('escalation', 'escalation_replay') AND delivery_family = 'notification')
	OR (key_kind = 'handoff' AND delivery_family = 'handoff')
	OR (key_kind IN ('webhook_event', 'webhook_replay') AND delivery_family = 'webhook')
)`

// applyPayloadDigest brings a database to the shape this build needs, in the
// order a populated table allows.
//
// A BYTEA NOT NULL cannot be added to a table with rows in one statement, so
// the column arrives nullable, is filled by this build's own codec, is checked
// for gaps and only then made NOT NULL. The rules follow, last, so that nothing
// is validated against half-filled data.
func applyPayloadDigest(ctx context.Context, tx *sql.Tx) error {
	// 1. The column, nullable for now.
	if _, err := tx.ExecContext(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns
			               WHERE table_name = 'outbound_intents'
			                 AND column_name = 'payload_digest') THEN
				ALTER TABLE outbound_intents ADD COLUMN payload_digest BYTEA;
			END IF;
		END $$;`); err != nil {
		return fmt.Errorf("add the payload digest: %w", err)
	}

	// 2. The backfill, in Go rather than in SQL. Canonicalising a payload is
	//    this domain's codec, and repeating it as an expression would be a
	//    second implementation of the thing that must have exactly one.
	if err := backfillPayloadDigests(ctx, tx); err != nil {
		return err
	}

	// 3. No gaps. A row the backfill could not read has already stopped the
	//    start above; this asks the database rather than trusting the loop.
	var missing int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM outbound_intents WHERE payload_digest IS NULL`).
		Scan(&missing); err != nil {
		return fmt.Errorf("look for commitments with no payload digest: %w", err)
	}
	if missing > 0 {
		return fmt.Errorf(
			"%d commitment(s) still have no payload digest after the backfill", missing)
	}

	// 4-11. Required, then the seven rules. The rules go last so that none of
	//       them is checked against a column that is still being filled.
	for _, step := range []struct {
		what string
		sql  string
	}{
		{
			what: "make the payload digest required",
			sql: `ALTER TABLE outbound_intents
			      ALTER COLUMN payload_digest SET NOT NULL`,
		},
		{
			what: outboundPayloadDigestLenConstraint,
			sql: guardedConstraint("outbound_intents", outboundPayloadDigestLenConstraint,
				`CHECK (octet_length(payload_digest) = 32)`),
		},
		{
			// The old vocabulary goes first, then the new one arrives under its
			// own name. Both statements are no-ops on a database that has been
			// through this once, and on one that has not the old rule is gone
			// before a webhook row could meet it.
			what: "drop " + outboundBatchFamilyConstraintV1,
			sql: `ALTER TABLE outbound_batches DROP CONSTRAINT IF EXISTS ` +
				outboundBatchFamilyConstraintV1,
		},
		{
			what: "drop " + outboundIntentFamilyConstraintV1,
			sql: `ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
				outboundIntentFamilyConstraintV1,
		},
		{
			// Validating, not NOT VALID: the new vocabulary is a superset of the
			// old, so no existing row can contradict it and the check is cheap.
			what: outboundBatchFamilyConstraint,
			sql: guardedConstraint("outbound_batches", outboundBatchFamilyConstraint,
				`CHECK `+familyOfKindSQL),
		},
		{
			what: outboundIntentFamilyConstraint,
			sql: guardedConstraint("outbound_intents", outboundIntentFamilyConstraint,
				`CHECK `+familyOfKindSQL),
		},
		{
			// The parent side of the composite key. Redundant as data - id is
			// already the primary key - and needed so the foreign key below has
			// something to reference.
			what: outboundBatchIdentityUnique,
			sql: guardedConstraint("outbound_batches", outboundBatchIdentityUnique,
				`UNIQUE (id, key_kind, delivery_family, grammar_version)`),
		},
		{
			// A commitment belongs to a claim of the SAME kind, family and
			// grammar. The two CHECKs above keep each row consistent with
			// itself and say nothing about the pair; with two incompatible
			// content forms in the tree that became an execution contract, not
			// tidiness - a handover commitment under an escalation's claim
			// would go looking for a frozen state it never had.
			what: outboundIntentBatchIdentity,
			sql: guardedConstraint("outbound_intents", outboundIntentBatchIdentity,
				`FOREIGN KEY (batch_id, key_kind, delivery_family, grammar_version)
				 REFERENCES outbound_batches (id, key_kind, delivery_family, grammar_version)`),
		},
		{
			// A handover names its recipient in the columns and again in the
			// payload, and IS NOT DISTINCT FROM is what makes that total: with
			// plain equality a payload carrying no target at all compares NULL
			// against the column, and a CHECK that evaluates to unknown passes.
			// The escalation rule of the same shape is a separate
			// constraint under its own name on purpose: extending that one
			// would be a guarded block finding the old name already there and
			// changing nothing, so an upgraded database would keep the old rule
			// while a fresh one got the new.
			what: outboundHandoffTargetAgreement,
			sql: guardedConstraint("outbound_intents", outboundHandoffTargetAgreement,
				`CHECK (
					key_kind <> 'handoff'
					OR (payload->'target'->>'kind' IS NOT DISTINCT FROM target_kind
						AND payload->'target'->>'ref' IS NOT DISTINCT FROM target_ref)
				)`),
		},
		{
			// A shift is taken by a person. The business key of a handover
			// carries the occurrence, the provider and the user id and NOT the
			// target kind, so one aimed at a channel would share its key with
			// one aimed at the person of that id: two promises, deduplicated
			// as one.
			what: outboundHandoffPersonTarget,
			sql: guardedConstraint("outbound_intents", outboundHandoffPersonTarget,
				`CHECK (key_kind <> 'handoff' OR target_kind = 'user')`),
		},
		{
			// A webhook is delivered to a subscriber. Its business key carries
			// the event and the integration id and not the target kind, so a
			// row aimed at a person of the same id would share the key with the
			// one aimed at the subscriber.
			what: outboundWebhookSubscriberTarget,
			sql: guardedConstraint("outbound_intents", outboundWebhookSubscriberTarget,
				`CHECK (key_kind NOT IN ('webhook_event', 'webhook_replay')
					OR target_kind = 'subscriber')`),
		},
		{
			// The handover rule again, for the webhook kinds: the payload names
			// the subscriber it was composed for, the columns name the one it is
			// delivered to, and the two must be one.
			what: outboundWebhookTargetAgreement,
			sql: guardedConstraint("outbound_intents", outboundWebhookTargetAgreement,
				`CHECK (
					key_kind NOT IN ('webhook_event', 'webhook_replay')
					OR (payload->'target'->>'kind' IS NOT DISTINCT FROM target_kind
						AND payload->'target'->>'ref' IS NOT DISTINCT FROM target_ref)
				)`),
		},
		{
			// The family has one provider, and it is not in the key. A row in
			// the webhook partition naming any other would sit in a queue
			// whose worker has no channel for it and no other worker sees.
			what: outboundWebhookProviderConstraint,
			sql: guardedConstraint("outbound_intents", outboundWebhookProviderConstraint,
				`CHECK (key_kind NOT IN ('webhook_event', 'webhook_replay')
					OR provider = 'webhook')`),
		},
	} {
		if _, err := tx.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("failed to %s: %w", step.what, err)
		}
	}
	return nil
}

// guardedConstraint adds a constraint only if it is not there, so a rerun of
// the start-up is a no-op rather than an error.
func guardedConstraint(table, name, body string) string {
	return `
DO $$
BEGIN
	IF NOT EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conname = '` + name + `' AND conrelid = '` + table + `'::regclass
	) THEN
		ALTER TABLE ` + table + ` ADD CONSTRAINT ` + name + ` ` + body + `;
	END IF;
END $$;`
}

// backfillPayloadDigests computes the digest for every commitment written
// before the column existed.
//
// A payload that will not canonicalise stops the start and names the row. It is
// damaged execution data: guessing a digest for it would make every later
// attempt compare against a value nobody derived from anything.
func backfillPayloadDigests(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, key_kind, payload_schema_version, payload
		FROM outbound_intents WHERE payload_digest IS NULL`)
	if err != nil {
		return fmt.Errorf("read the commitments with no payload digest: %w", err)
	}

	type pending struct {
		id     string
		digest []byte
	}
	var filled []pending
	for rows.Next() {
		var id, kind string
		var schemaVersion int
		var payload []byte
		if err := rows.Scan(&id, &kind, &schemaVersion, &payload); err != nil {
			rows.Close()
			return err
		}
		digest, err := keys.PayloadDigest(keys.Kind(kind), schemaVersion, payload)
		if err != nil {
			rows.Close()
			return fmt.Errorf(
				"the payload of commitment %s cannot be canonicalised, so no digest "+
					"can be computed for it: %w", id, err)
		}
		filled = append(filled, pending{id: id, digest: digest})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, row := range filled {
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbound_intents SET payload_digest = $2 WHERE id = $1`,
			row.id, row.digest); err != nil {
			return fmt.Errorf("record the payload digest of %s: %w", row.id, err)
		}
	}
	return nil
}
