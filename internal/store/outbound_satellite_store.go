package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The satellites of a card in the store: how a satellite follows its card
// through the claim, the attempt and the operator, and how the cards of a
// previous version get theirs.

// The claim's view of a satellite, and the lateness measure's: the card it
// follows and whether the group's last revision is out, joined to the queue
// row as `due`. A thread goes once its card has a message or has ended. The
// reply goes only once the group's last revision is out AND the card has
// said its last word - ended, which for a card with a message means the
// last revision applied. Not merely once the card has a message: a reply
// posted while the card's last update is in flight makes Slack tell every
// client about the parent - its reply count changed - with the card as it
// was, and a client that hears that after the update shows the old card
// until it is reloaded. The resolution is announced under a card that
// already shows it. A satellite whose card is alive without a message is not
// late - it is waiting for the card - and a card in manual review must not
// light the paging alarm through its thread.
//
// Joins, not correlated subqueries: each is one index probe, and a subquery
// leaves the planner an alternative that walks the table, which the claim's
// plan test refuses.
const satelliteJoins = `
	LEFT JOIN outbound_intents parent ON parent.id = due.parent_intent_id
	LEFT JOIN outbound_group_snapshots closing
	       ON closing.alert_group_id = due.alert_group_id AND due.target_kind = 'thread_reply'`

var satelliteMayGo = `
	AND (due.parent_intent_id IS NULL
	     OR parent.receipt_recorded OR parent.status IN (` + terminalStatusList + `))
	AND (due.target_kind <> 'thread_reply'
	     OR (closing.final AND parent.status IN (` + terminalStatusList + `)))`

// notFollowingASentCard is the withdrawal's predicate: a satellite is
// withdrawn only while its card has no message. A card that went out keeps its
// thread - the person who acknowledged the alert is exactly who reads the
// history - and the thread goes out with the revision the acknowledgement
// raised.
const notFollowingASentCard = `
	AND (parent_intent_id IS NULL
	     OR NOT EXISTS (SELECT 1 FROM outbound_intents p
	                    WHERE p.id = outbound_intents.parent_intent_id AND p.receipt_recorded))`

// parentStateTx reads the card a satellite follows as it stands. Shared, not
// locked: what the reader does with the answer is decided again under a lock
// where it matters.
func parentStateTx(ctx context.Context, q sqlQueryer, parentID string) (*outbound.ParentState, error) {
	var (
		parent outbound.ParentState
		ref    sql.NullString
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, status, receipt_recorded, receipt_ref FROM outbound_intents WHERE id = $1`,
		parentID).Scan(&parent.ID, &parent.Status, &parent.ReceiptRecorded, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		// The schema keeps a parent from being removed under its satellite;
		// a satellite whose parent is not there is a row this build did not
		// write.
		return nil, outboundContractf("a satellite follows %s, and there is no such commitment", parentID)
	}
	if err != nil {
		return nil, fmt.Errorf("read the card %s: %w", parentID, err)
	}
	parent.ReceiptRef = ref.String
	return &parent, nil
}

// lockParentSharedTx takes the card a satellite follows FOR SHARE, before the
// satellite itself: the order is parent, then satellite, the same order the
// operator's retry takes them in after the group.
func lockParentSharedTx(ctx context.Context, tx *sql.Tx, parentID string) (*outbound.ParentState, error) {
	var (
		parent outbound.ParentState
		ref    sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, status, receipt_recorded, receipt_ref FROM outbound_intents
		WHERE id = $1 FOR SHARE`, parentID).Scan(&parent.ID, &parent.Status, &parent.ReceiptRecorded, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, outboundContractf("a satellite follows %s, and there is no such commitment", parentID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock the card %s: %w", parentID, err)
	}
	parent.ReceiptRef = ref.String
	return &parent, nil
}

// refusalIsStaleTx says whether a satellite's refusal - its card ended without
// a message - still holds: it does not once a person has brought the card
// back, or once the card has a message after all.
//
// Reachable, and not by anything exotic: the worker prepares from the card as
// it read it at the claim, and an operator's retry of the card between that
// read and the begin would leave the satellite refused for good, over a card
// that is alive again. Checked under the card's shared lock, so the retry and
// this either see each other or are ordered.
// afterParentCheck is a test hook, called by a begin that holds the card
// shared and has found its refusal still true, before it takes the satellite.
// It is how a test puts an operator's retry exactly there.
var afterParentCheck func()

func refusalIsStaleTx(ctx context.Context, tx *sql.Tx, parentID string) (bool, error) {
	parent, err := lockParentSharedTx(ctx, tx, parentID)
	if err != nil {
		return false, err
	}
	return parent.ReceiptRecorded || !parent.Ended(), nil
}

// releaseForRetryTx puts a satellite whose refusal went stale back in the
// queue: the lease goes, it is due now, and nothing is recorded about the
// refusal - there was none.
func releaseForRetryTx(ctx context.Context, tx *sql.Tx, intentID string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_intents
		SET lease_token = NULL, locked_until = NULL, worker_id = NULL,
		    next_attempt_at = statement_timestamp(), updated_at = now()
		WHERE id = $1`, intentID); err != nil {
		return fmt.Errorf("release %s for another claim: %w", intentID, err)
	}
	return nil
}

// reviveSatellitesTx brings back the satellites that were refused because
// their card had ended without a message, now that a person has brought the
// card back. In the retry's own transaction, after the group and the card:
// the order is group, parent, satellites.
//
// Only those. A satellite withdrawn with its card stays withdrawn - canceled
// is the end of a commitment, and the card that was canceled does not come
// back either. The one withdrawal that is undone is the one a card's own send
// overtook, and that is the send's doing (reviveWithdrawnSatellitesTx).
func reviveSatellitesTx(ctx context.Context, tx *sql.Tx, parentID string, by outbound.Actor) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		UPDATE outbound_intents i
		SET status = 'pending', next_attempt_at = now(), lease_token = NULL,
		    locked_until = NULL, worker_id = NULL, updated_at = now()
		WHERE i.parent_intent_id = $1 AND i.status = 'permanent_failed'
		  AND (SELECT a.error_class FROM outbound_attempts a
		       WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1) = $2
		RETURNING i.id`, parentID, outbound.ParentEndedWithoutMessage)
	if err != nil {
		return 0, fmt.Errorf("revive the satellites of %s: %w", parentID, err)
	}
	var revived []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		revived = append(revived, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range revived {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "revived",
			"the card was retried", by); err != nil {
			return 0, err
		}
	}
	return len(revived), nil
}

// reviveWithdrawnSatellitesTx brings back the satellites withdrawn with a
// card that then went out anyway. The withdrawal found the card in flight
// without a message and took its thread and its reply, as it takes those of
// any card without one; the card's send then won the race (T16). A card with
// a message keeps its satellites, so they return: the thread aimed at the
// revision the withdrawal raised, the reply waiting for the end as before.
// Only satellites nobody ever attempted: a canceled satellite with an attempt
// was ended by a person from a failure, and a person's decision is not undone
// by a race.
func reviveWithdrawnSatellitesTx(ctx context.Context, tx *sql.Tx, parentID string) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		UPDATE outbound_intents i
		SET status = 'pending', next_attempt_at = now(), cancellation_requested = FALSE,
		    desired_revision = CASE WHEN i.form = $2 THEN COALESCE(
		        (SELECT g.revision FROM outbound_group_snapshots g WHERE g.alert_group_id = i.alert_group_id),
		        i.desired_revision) ELSE i.desired_revision END,
		    updated_at = now()
		WHERE i.parent_intent_id = $1 AND i.status = 'canceled'
		  AND NOT EXISTS (SELECT 1 FROM outbound_attempts a WHERE a.intent_id = i.id)
		RETURNING i.id`, parentID, string(outbound.FormEditable))
	if err != nil {
		return 0, fmt.Errorf("bring back the satellites of %s: %w", parentID, err)
	}
	var revived []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		revived = append(revived, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range revived {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "revived",
			"the card went out alongside the withdrawal", outbound.ActorSystem); err != nil {
			return 0, err
		}
	}
	return len(revived), nil
}

// admitSatellitesTx gives the satellites to every Slack channel card admitted
// by a version that had none: the thread and the reply go into the card's own
// claim, aimed at the revision the group is at, following the card. Once, at
// the start of the first version that has them.
//
// The one place a commitment is made outside an admission, and named as such.
// The claim's fingerprint stays what it was: it identified what was proposed,
// and nothing proposes the same group again while its claim stands - the
// producer does not plan a group that holds one. A producer that ever does
// will have to recompute it, or compare without the satellites.
//
// Every card whose group is not finished and that was not withdrawn: with a
// message or without, live or failed. A card that has not gone out yet gets
// its satellites now or never - nothing admits into an existing claim later -
// and the claim's own gate keeps them waiting for the card.
func admitSatellitesTx(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT i.id, i.batch_id, i.alert_group_id, i.provider, i.target_ref,
		       i.payload_schema_version, i.payload, i.desired_revision,
		       b.key_kind, b.grammar_version, b.admitted_at, g.revision
		FROM outbound_intents i
		JOIN outbound_batches b ON b.id = i.batch_id
		JOIN outbound_group_snapshots g ON g.alert_group_id = i.alert_group_id
		WHERE i.provider = $1 AND i.target_kind = $2 AND i.form = $3
		  AND i.status <> 'canceled' AND NOT g.final
		  AND NOT EXISTS (SELECT 1 FROM outbound_intents c WHERE c.parent_intent_id = i.id)
		ORDER BY i.created_at, i.id`,
		keys.ProviderSlack, string(keys.TargetChannel), string(outbound.FormEditable))
	if err != nil {
		return fmt.Errorf("find the cards without satellites: %w", err)
	}
	type card struct {
		id, batchID, groupID, provider, channel string
		schemaVersion                           int
		payload                                 []byte
		desiredRevision, snapshotRevision       int64
		kind                                    string
		grammar                                 int
		admittedAt                              time.Time
	}
	var cards []card
	for rows.Next() {
		var c card
		if err := rows.Scan(&c.id, &c.batchID, &c.groupID, &c.provider, &c.channel,
			&c.schemaVersion, &c.payload, &c.desiredRevision, &c.kind, &c.grammar,
			&c.admittedAt, &c.snapshotRevision); err != nil {
			rows.Close()
			return err
		}
		cards = append(cards, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, c := range cards {
		payload, err := keys.DecodeEscalationPayload(c.schemaVersion, c.payload)
		if err != nil {
			return fmt.Errorf("the payload of card %s cannot be read, so its satellites "+
				"cannot be made: %w", c.id, err)
		}
		satellites, err := keys.SatellitesOf(keys.Kind(c.kind), c.grammar, c.groupID,
			payload.Slot, c.provider, c.channel)
		if err != nil {
			return fmt.Errorf("the satellites of card %s: %w", c.id, err)
		}
		for _, satellite := range satellites {
			if _, err := insertCommitmentRowTx(ctx, tx, batchRow{
				ID: c.batchID, Kind: keys.Kind(c.kind), GrammarVersion: c.grammar,
				AlertGroupID: c.groupID, Revision: c.snapshotRevision,
				Family: outbound.FamilyNotification, AdmittedAt: c.admittedAt,
			}, satellite, c.id, outbound.ActorSystem); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbound_batches SET intent_count = intent_count + $2 WHERE id = $1`,
			c.batchID, len(satellites)); err != nil {
			return fmt.Errorf("count the satellites of card %s: %w", c.id, err)
		}
	}
	return nil
}
