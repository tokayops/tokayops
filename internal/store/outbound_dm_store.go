package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a direct message takes from the card: settled when its generation
// opens, never read on the attempt.

// SetDMFallbackToFirehose is the installation's answer to a policy that
// posted no channel card of its own: point the direct message back to the
// firehose card, or to nothing. Unset means the firehose, as it always did.
func (s *Store) SetDMFallbackToFirehose(on bool) {
	s.dmFallbackToFirehose = &on
}

func (s *Store) dmFallsBackToFirehose() bool {
	return s.dmFallbackToFirehose == nil || *s.dmFallbackToFirehose
}

// The claim's view of a direct message, and the lateness measure's: whether
// a card it could link to has yet to have its first attempt. The cards were
// named at admission (awaits_intent_ids), so this is a probe of the primary
// key; the batch is not searched here, and the claim's plan test keeps it
// that way. The message waits only while its generation is unbound - a bound
// generation carries what it carries, and its retries do not wait. A card is
// done being waited for once it has a receipt; once an attempt of it was
// settled either way (failure_streak counts settled failures - a rejected
// card back in pending has had its turn, and the message goes without the
// link rather than through the retries; attempts_in_generation would not do,
// it is raised before the provider is called); or once it left pending and
// sending - a page does not wait for an operator's decision on a card. The
// lateral runs for every queue row and, for one that is not a waiting
// message, returns nothing without a probe. The message's own expiry is not
// paused: it can lose one attempt of the card to the wait, and no more.
const dmJoins = `
	LEFT JOIN LATERAL (
		SELECT TRUE AS open FROM outbound_intents card
		WHERE due.awaits_intent_ids IS NOT NULL AND due.create_key IS NULL
		  AND card.id = ANY(due.awaits_intent_ids)
		  AND NOT card.receipt_recorded
		  AND card.failure_streak = 0
		  AND card.status IN ('pending', 'sending')
		LIMIT 1) waiting ON TRUE`

const dmMayGo = `
	AND waiting.open IS NULL`

// dmContextTx is the context a direct message's generation binds: the card
// of the same claim it points back to, and the workspace the card is in.
// A message that named its cards at admission chooses among them - the
// earliest policy card with a message, else the firehose - and applies no
// switch of its own: the array is the answer. A row an earlier build admitted
// has no array and is answered from its batch, as before.
// The card is a channel step of the policy with a message - the earliest
// step, when several posted - and otherwise the firehose card, when the
// installation allows that; a message that goes out before any card leaves
// the context empty, and the generation stays that way. Anything but a Slack
// direct message binds nothing.
//
// The workspace's address is read from the integration as it stands and
// frozen with the coordinates: it is the other half of the link, and a
// setting read on the attempt would be an input outside the snapshot, the
// payload and the generation. A configuration that cannot be read stops the
// begin and names the row: the answer decides the bytes of the message.
func (s *Store) dmContextTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent) (json.RawMessage, error) {
	if intent.TargetKind != keys.TargetUser || intent.Provider != keys.ProviderSlack {
		return nil, nil
	}
	var (
		ref sql.NullString
		err error
	)
	if len(intent.AwaitsIntentIDs) > 0 {
		err = tx.QueryRowContext(ctx, `
			SELECT receipt_ref FROM outbound_intents
			WHERE id = ANY($1) AND receipt_recorded
			ORDER BY payload->'slot'->>'kind' = $2, (payload->'slot'->>'index')::int, created_at, id
			LIMIT 1`,
			pq.Array(intent.AwaitsIntentIDs), string(keys.SlotFirehose)).Scan(&ref)
	} else {
		err = tx.QueryRowContext(ctx, `
			SELECT receipt_ref FROM outbound_intents
			WHERE batch_id = $1 AND provider = $2 AND target_kind = $3 AND receipt_recorded
			  AND (payload->'slot'->>'kind' = $4 OR $5)
			ORDER BY payload->'slot'->>'kind' = $6, (payload->'slot'->>'index')::int, created_at, id
			LIMIT 1`,
			intent.BatchID, intent.Provider, string(keys.TargetChannel),
			string(keys.SlotPolicy), s.dmFallsBackToFirehose(), string(keys.SlotFirehose)).Scan(&ref)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find the card %s points back to: %w", intent.ID, err)
	}
	teamURL, err := slackTeamURLTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	return outbound.BoundContext{CardReceiptRef: ref.String, TeamURL: teamURL}.Encode()
}

// slackTeamURLTx is the workspace's address as the enabled Slack integration
// records it, or nothing when no integration is enabled or none was saved
// since the address is recorded.
func slackTeamURLTx(ctx context.Context, q sqlQueryer) (string, error) {
	var id, encrypted string
	err := q.QueryRowContext(ctx, `
		SELECT id, config FROM integrations WHERE enabled AND type = $1
		ORDER BY created_at LIMIT 1`, model.IntegrationTypeSlack).Scan(&id, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the Slack integration: %w", err)
	}
	raw, err := decryptConfig(encrypted)
	if err != nil {
		return "", fmt.Errorf("read the configuration of integration %s: %w", id, err)
	}
	var cfg model.SlackConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("read the configuration of integration %s: %w", id, err)
	}
	return cfg.TeamURL, nil
}
