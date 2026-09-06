package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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

// dmContextTx is the context a direct message's generation binds: the card
// of the same claim it points back to, and the workspace the card is in.
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
	if intent.TargetKind != keys.TargetUser || intent.Provider != keys.InteractiveSlack {
		return nil, nil
	}
	var ref sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT receipt_ref FROM outbound_intents
		WHERE batch_id = $1 AND provider = $2 AND target_kind = $3 AND receipt_recorded
		  AND (payload->'slot'->>'kind' = $4 OR $5)
		ORDER BY payload->'slot'->>'kind' = $6, (payload->'slot'->>'index')::int, created_at, id
		LIMIT 1`,
		intent.BatchID, intent.Provider, string(keys.TargetChannel),
		string(keys.SlotPolicy), s.dmFallsBackToFirehose(), string(keys.SlotFirehose)).Scan(&ref)
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
