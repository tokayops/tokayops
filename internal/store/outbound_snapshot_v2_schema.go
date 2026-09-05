package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// The second version of the render snapshot, and what a database written under
// the first needs to become it.
//
// Two columns for the two forms a snapshot now has - the card and the thread
// under it are raised separately, by the digest of what each of them shows -
// and a rebuild of every group snapshot still stored as version 1: the same
// content, plus the history the thread shows and the buttons the card was
// drawn with, under the new digests. The revision and the finality of a row do
// not move; what was revision 3 is revision 3 afterwards.
//
// The admission state of the batches is NOT rebuilt. It is frozen with the
// claim and says what was admitted; rewriting it with a history "as of the
// upgrade" would be a lie, and the one-shot messages that render it read
// version 1 through the reader kept for exactly that.
//
// Two commitment columns come with it: the context a generation is bound to
// (what a direct message takes from its neighbour, frozen with the address)
// and the parent a satellite of a card follows. Both are empty until the code
// that writes them lands; the shape lands here so one start makes the schema.

const (
	outboundFormDigestLen        = "outbound_group_snapshots_form_digest_len"
	outboundSatelliteNamesParent = "outbound_intents_satellite_names_parent"
)

func applySnapshotV2Schema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		ALTER TABLE outbound_group_snapshots
			ADD COLUMN IF NOT EXISTS card_digest BYTEA,
			ADD COLUMN IF NOT EXISTS thread_digest BYTEA`); err != nil {
		return fmt.Errorf("add the form digests to the snapshots: %w", err)
	}
	if err := rebuildRenderSnapshots(ctx, tx); err != nil {
		return err
	}
	for _, step := range []struct{ what, sql string }{
		{
			what: "insist on the form digests",
			sql: `ALTER TABLE outbound_group_snapshots
				ALTER COLUMN card_digest SET NOT NULL,
				ALTER COLUMN thread_digest SET NOT NULL`,
		},
		{
			what: "add " + outboundFormDigestLen,
			sql: guardedConstraint("outbound_group_snapshots", outboundFormDigestLen,
				`CHECK (octet_length(card_digest) = 32 AND octet_length(thread_digest) = 32)`),
		},
		{
			what: "add the generation context and the parent to the commitments",
			sql: `ALTER TABLE outbound_intents
				ADD COLUMN IF NOT EXISTS bound_context JSONB,
				ADD COLUMN IF NOT EXISTS parent_intent_id TEXT REFERENCES outbound_intents(id)`,
		},
		{
			what: "add idx_outbound_intents_parent",
			sql: `CREATE INDEX IF NOT EXISTS idx_outbound_intents_parent
				ON outbound_intents (parent_intent_id) WHERE parent_intent_id IS NOT NULL`,
		},
		{
			what: "add " + outboundSatelliteNamesParent,
			sql: guardedConstraint("outbound_intents", outboundSatelliteNamesParent,
				`CHECK ((target_kind IN ('thread', 'thread_reply')) = (parent_intent_id IS NOT NULL))`),
		},
	} {
		if _, err := tx.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("failed to %s: %w", step.what, err)
		}
	}
	return nil
}

// rebuildRenderSnapshots turns every group snapshot still stored as version 1
// into version 2, with its own history and the buttons its cards were drawn
// with.
//
// The buttons come from the PAYLOADS of the group's live cards, not from the
// switches as they stand: a card sent with buttons before the switch was turned
// off has buttons, and a snapshot that said otherwise would let the start-up
// reconciliation compare "off" with "off" and leave them there. The history is
// read as it stands at the start, which is the only moment there is.
//
// A row that cannot be read as version 1 stops the start and names the group.
// It is the state a card is rendered from - execution data - and a guess would
// be a card about something nobody admitted.
func rebuildRenderSnapshots(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT alert_group_id, revision, snapshot, snapshot_digest
		FROM outbound_group_snapshots
		WHERE snapshot_schema_version = $1`, keys.RenderSnapshotSchemaV1)
	if err != nil {
		return fmt.Errorf("read the snapshots written under version 1: %w", err)
	}
	type stored struct {
		group       string
		revision    int64
		raw, digest []byte
	}
	var older []stored
	for rows.Next() {
		var row stored
		if err := rows.Scan(&row.group, &row.revision, &row.raw, &row.digest); err != nil {
			rows.Close()
			return err
		}
		older = append(older, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, row := range older {
		snapshot, err := keys.DecodeRenderSnapshotV1(row.raw)
		if err != nil {
			return fmt.Errorf("the state of alert group %s cannot be read as version 1, "+
				"so it cannot be rebuilt: %w", row.group, err)
		}
		// The row has to be what its digest says before it is signed again:
		// a rebuild that took a readable row on trust would put the new
		// digests on content nobody admitted.
		if !bytes.Equal(snapshot.Digest(), row.digest) {
			return fmt.Errorf("the state of alert group %s no longer matches the digest "+
				"its commitments were keyed against, and cannot be rebuilt", row.group)
		}
		history, omitted, err := timelineTailTx(ctx, tx, row.group)
		if err != nil {
			return err
		}
		buttons, err := drawnButtonsTx(ctx, tx, row.group)
		if err != nil {
			return err
		}

		in := snapshot.Content()
		in.Timeline = providers.HistoryOf(history)
		in.TimelineOmitted = omitted
		in.InteractiveProviders = buttons
		rebuilt, err := keys.NewRenderSnapshot(in)
		if err != nil {
			return fmt.Errorf("the state of alert group %s cannot be rebuilt as version 2: %w",
				row.group, err)
		}
		encoded, err := rebuilt.MarshalJSON()
		if err != nil {
			return fmt.Errorf("store the rebuilt state of alert group %s: %w", row.group, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbound_group_snapshots
			SET snapshot = $3, snapshot_digest = $4, card_digest = $5, thread_digest = $6,
			    snapshot_schema_version = $7
			WHERE alert_group_id = $1 AND revision = $2`,
			row.group, row.revision, encoded, rebuilt.Digest(), rebuilt.CardDigest(),
			rebuilt.ThreadDigest(), keys.RenderSnapshotSchemaV2); err != nil {
			return fmt.Errorf("store the rebuilt state of alert group %s: %w", row.group, err)
		}
	}
	return fillFormDigests(ctx, tx)
}

// fillFormDigests computes the two form digests of every version 2 row that
// has none - a row written by a build that had the version before it had the
// columns. The content is read through the version 2 door and its identity
// digest has to come out as stored, or the row is not what its column says.
func fillFormDigests(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT alert_group_id, snapshot, snapshot_digest
		FROM outbound_group_snapshots
		WHERE snapshot_schema_version = $1 AND (card_digest IS NULL OR thread_digest IS NULL)`,
		keys.RenderSnapshotSchemaV2)
	if err != nil {
		return fmt.Errorf("read the snapshots with no form digests: %w", err)
	}
	type stored struct {
		group       string
		raw, digest []byte
	}
	var missing []stored
	for rows.Next() {
		var row stored
		if err := rows.Scan(&row.group, &row.raw, &row.digest); err != nil {
			rows.Close()
			return err
		}
		missing = append(missing, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, row := range missing {
		var snapshot keys.RenderSnapshot
		if err := json.Unmarshal(row.raw, &snapshot); err != nil {
			return fmt.Errorf("the state of alert group %s cannot be read as version 2: %w", row.group, err)
		}
		if !bytes.Equal(snapshot.Digest(), row.digest) {
			return fmt.Errorf("the state of alert group %s no longer matches its digest", row.group)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbound_group_snapshots SET card_digest = $2, thread_digest = $3
			WHERE alert_group_id = $1`,
			row.group, snapshot.CardDigest(), snapshot.ThreadDigest()); err != nil {
			return fmt.Errorf("record the form digests of alert group %s: %w", row.group, err)
		}
	}
	return nil
}

// drawnButtonsTx reads which providers' cards of a group were drawn with
// buttons, from what each card's payload said when it was admitted. Every card
// a person can still bring back counts - a card that failed for good keeps
// its buttons in the chat until it is redrawn - and only the ended ones do not.
func drawnButtonsTx(ctx context.Context, tx *sql.Tx, alertGroupID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, provider, payload_schema_version, payload
		FROM outbound_intents
		WHERE alert_group_id = $1 AND form = $2 AND status IN (`+reachableCardStatuses+`)`,
		alertGroupID, string(outbound.FormEditable))
	if err != nil {
		return nil, fmt.Errorf("read the cards of %s: %w", alertGroupID, err)
	}
	defer rows.Close()

	on := map[string]bool{}
	for rows.Next() {
		var (
			id, provider  string
			schemaVersion int
			payload       []byte
		)
		if err := rows.Scan(&id, &provider, &schemaVersion, &payload); err != nil {
			return nil, err
		}
		// Only the first payload schema says anything about buttons; the
		// second reads them from the snapshot being built here.
		if schemaVersion != (keys.EscalationPayloadV1{}).SchemaVersion() {
			continue
		}
		decoded, err := keys.DecodeEscalationPayloadV1(schemaVersion, payload)
		if err != nil {
			return nil, fmt.Errorf("the payload of commitment %s cannot be read, so the "+
				"buttons of alert group %s cannot be settled: %w", id, alertGroupID, err)
		}
		if decoded.Interactive {
			on[provider] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	buttons := make([]string, 0, len(on))
	for p := range on {
		buttons = append(buttons, p)
	}
	sort.Strings(buttons)
	return buttons, nil
}
