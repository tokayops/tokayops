package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tokayops/tokayops/internal/outbound"
)

// The form of an actor in the journal: a kind beside the reference.

const outboundEventActorKindConstraint = "outbound_intent_events_actor_kind"

// applyActorSchema adds actor_kind to the journal and classifies every line
// written before it existed.
//
// The classification goes by the WRITE PATH - the kind of the line, the kind
// of the claim, the reason - and never by the text of the actor. The text
// cannot be trusted to say what wrote it: an acknowledgement signed lines with
// the display name of the person, and a person may be called "system". A line
// no pair below attributes stays legacy, which is the right answer and not a
// gap: it says "a build before this one, path not established", and is read
// as text.
//
// Two references are made uniform where the path is certain: an expiry, which
// signed nothing, becomes the worker's; the fan-out's old spelling becomes the
// component's name. Everything else keeps the text it was written with.
func applyActorSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE outbound_intent_events ADD COLUMN IF NOT EXISTS actor_kind TEXT`); err != nil {
		return fmt.Errorf("add actor_kind to the journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, backfillActorKind); err != nil {
		return fmt.Errorf("classify the journal's actors: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE outbound_intent_events ALTER COLUMN actor_kind SET NOT NULL`); err != nil {
		return fmt.Errorf("require actor_kind: %w", err)
	}
	kinds := ""
	for i, kind := range outbound.ActorKinds() {
		if i > 0 {
			kinds += ", "
		}
		kinds += "'" + string(kind) + "'"
	}
	if _, err := tx.ExecContext(ctx, guardedConstraint("outbound_intent_events",
		outboundEventActorKindConstraint, `CHECK (actor_kind IN (`+kinds+`))`)); err != nil {
		return fmt.Errorf("constrain actor_kind: %w", err)
	}
	return nil
}

// backfillActorKind is the table of write paths of the build before this one,
// applied to every line that has no kind yet. The reason literals are the
// ones that build wrote, quoted from its code.
const backfillActorKind = `
UPDATE outbound_intent_events e
SET actor_kind = CASE
	WHEN e.kind = 'created' AND i.key_kind IN ('escalation', 'escalation_replay', 'handoff', 'webhook_event')
		THEN 'system'
	WHEN e.kind = 'created' AND i.key_kind = 'webhook_replay'
		THEN 'user'
	WHEN e.kind = 'expired' AND e.reason = 'the deadline passed before anything was sent'
		THEN 'system'
	WHEN e.kind = 'effect_bound' AND e.reason = 'the address and key of this generation are settled'
		THEN 'system'
	WHEN e.kind IN ('canceled', 'cancellation_requested') AND e.reason = 'the alert cleared'
		THEN 'system'
	WHEN e.kind IN ('canceled', 'cancellation_requested')
		AND (e.reason LIKE 'the subscriber was disabled%' OR e.reason LIKE 'the subscriber was deleted%')
		THEN 'user'
	WHEN e.kind IN ('canceled', 'cancellation_requested') AND e.reason LIKE 'the recipient was erased%'
		THEN 'system'
	WHEN e.kind IN ('canceled', 'cancellation_requested')
		AND e.actor = 'recovery' AND e.reason = 'the lease expired with an attempt in flight'
		THEN 'system'
	WHEN e.kind = 'canceled' AND e.actor = 'worker' AND e.reason IS NULL
		THEN 'system'
	WHEN e.kind = 'desired_raised' AND e.reason = 'merge'
		THEN 'system'
	ELSE 'legacy'
	END,
    actor = CASE
	WHEN e.kind = 'expired' AND e.reason = 'the deadline passed before anything was sent'
		THEN COALESCE(e.actor, 'worker')
	WHEN e.kind = 'created' AND i.key_kind = 'webhook_event' AND e.actor = 'fan-out'
		THEN 'fanout'
	ELSE e.actor
	END
FROM outbound_intents i
WHERE i.id = e.intent_id AND e.actor_kind IS NULL`
