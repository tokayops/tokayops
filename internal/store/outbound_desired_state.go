package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// Raising the desired state of an alert group: the one door through which a
// message is told that what it says is out of date.
//
// Four things move together or not at all - the new snapshot, the revision on
// every editable commitment of the group, the return of the parked ones to the
// queue, and the line each of them gets in its own history. They run inside the
// transaction that made the alert move, so a crash between any two of them is
// not a state the system can be in: a card aimed at a revision whose snapshot
// was never stored has nothing to render, and a snapshot nothing was aimed at
// is a message that stays wrong forever.

// renderEnvironment is what a message needs that the alert group does not
// carry: where this installation lives and which zone times are printed in.
//
// It is configuration of the process, not state of the group, and it is frozen
// into every revision so that two instances - or one instance a month later -
// render the same bytes. The producer of revision 0 freezes the same two
// values; a store that disagreed with it would make the first update of a card
// differ from the card.
type renderEnvironment struct {
	selfURL string
	zone    string
}

// SetRenderEnvironment is called once at wiring. The zone falls back to UTC
// rather than to whatever this machine is set to: "the process zone" is the one
// thing a snapshot may never mean.
func (s *Store) SetRenderEnvironment(selfURL, zone string) {
	s.render = renderEnvironment{selfURL: selfURL, zone: zone}
}

func (e renderEnvironment) displayZone() string {
	if e.zone == "" {
		return "UTC"
	}
	return e.zone
}

// setDesiredStateTx raises what a group's messages have to show.
//
// It runs inside the caller's transaction and expects the group's row to be
// locked by it already - the ack, the resolve and the merge all take that lock
// as their first step, which is what makes the revision monotonic without a
// compare-and-set standing in for it.
func setDesiredStateTx(ctx context.Context, tx *sql.Tx, env renderEnvironment,
	req outbound.DesiredStateRequest) (outbound.DesiredStateResult, error) {

	if !req.Reason.Known() {
		return outbound.DesiredStateResult{}, outboundContractf(
			"the desired state of %s was raised for %q, which is not a reason this build states",
			req.AlertGroupID, req.Reason)
	}

	// The group first, and with it the check that the caller made the
	// transition it is claiming. It comes before everything else on purpose: a
	// wrong reason is a contract violation, and answering it with one of the
	// softer outcomes below would file a bug under "nothing to do".
	view, err := groupViewTx(ctx, tx, env, req)
	if err != nil {
		return outbound.DesiredStateResult{}, err
	}

	var (
		revision      int64
		digest        []byte
		schemaVersion int
		final         bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT revision, snapshot_digest, snapshot_schema_version, final
		FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		req.AlertGroupID).Scan(&revision, &digest, &schemaVersion, &final)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.DesiredStateResult{Outcome: outbound.DesiredNoSnapshot}, nil
	}
	if err != nil {
		return outbound.DesiredStateResult{}, fmt.Errorf(
			"read the state of %s: %w", req.AlertGroupID, err)
	}

	// A version this build does not know belongs to an instance that is ahead,
	// and the answer is to stop rather than to write. Overwriting it with what
	// this build can express would silently delete the part of the state a
	// newer renderer needs - and the read side already refuses such a row, so
	// the card would then be unrenderable by both of us.
	if schemaVersion != keys.RenderSnapshotSchemaV1 {
		return outbound.DesiredStateResult{}, outboundContractf(
			"the state of %s is stored under schema version %d, which this build cannot write",
			req.AlertGroupID, schemaVersion)
	}

	if final {
		return outbound.DesiredStateResult{
			Outcome: outbound.DesiredStaleAfterFinal, Revision: revision,
		}, nil
	}

	// Both snapshots below are built from the ONE read above. Reading the group
	// twice would let the comparison and the thing being stored describe two
	// different instants, which is the whole failure this command exists to
	// avoid.
	//
	// The candidate is built at the CURRENT number, because the comparison
	// below is about content: the revision is part of the material, so a
	// candidate carrying the next number would differ from the stored digest
	// every single time and the guard would quietly become a no-op.
	candidate, err := freezeAt(view, revision, req.AlertGroupID)
	if err != nil {
		return outbound.DesiredStateResult{}, err
	}

	if bytes.Equal(candidate.Digest(), digest) && !req.Reason.Final() {
		return outbound.DesiredStateResult{
			Outcome: outbound.DesiredUnchanged, Revision: revision,
		}, nil
	}

	// Rebuilt rather than renumbered: the number lives inside the snapshot as
	// well as in its column, and storing the candidate under a number it does
	// not contain makes a row every reader is obliged to refuse.
	next := revision + 1
	stored, err := freezeAt(view, next, req.AlertGroupID)
	if err != nil {
		return outbound.DesiredStateResult{}, err
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return outbound.DesiredStateResult{}, fmt.Errorf(
			"store the state of %s: %w", req.AlertGroupID, err)
	}
	written, err := tx.ExecContext(ctx, `
		UPDATE outbound_group_snapshots
		SET revision = $2, snapshot = $3, snapshot_digest = $4,
		    snapshot_schema_version = $5, final = $6, updated_at = now()
		WHERE alert_group_id = $1 AND revision = $7 AND final = FALSE`,
		req.AlertGroupID, next, encoded, stored.Digest(),
		keys.RenderSnapshotSchemaV1, req.Reason.Final(), revision,
	)
	if err != nil {
		return outbound.DesiredStateResult{}, fmt.Errorf(
			"store the state of %s: %w", req.AlertGroupID, err)
	}

	// The predicate is not decoration, so neither is its answer. Every caller
	// holds the group's row, which is what makes the revision monotonic - so
	// zero rows here means that lock was not taken, and continuing would aim
	// every card at a revision whose snapshot does not exist. Atomicity is no
	// help against that: the transaction would commit the dangling reference
	// exactly as faithfully as it commits a good one.
	//
	// It has no test, and deliberately: under the lock it cannot happen, and
	// without the lock the reader above sees the moved row and its own
	// predicate matches. Reaching this line means an assumption broke, which is
	// why it stops rather than corrects.
	affected, err := written.RowsAffected()
	if err != nil {
		return outbound.DesiredStateResult{}, err
	}
	if affected != 1 {
		return outbound.DesiredStateResult{}, outboundContractf(
			"the state of %s moved while its revision was being raised from %d "+
				"- the group's row was not held", req.AlertGroupID, revision)
	}

	touched, err := aimEditableIntentsTx(ctx, tx, req, next)
	if err != nil {
		return outbound.DesiredStateResult{}, err
	}
	return outbound.DesiredStateResult{
		Outcome: outbound.DesiredApplied, Revision: next, Touched: len(touched),
	}, nil
}

// aimEditableIntentsTx points the group's editable commitments at the new
// revision and puts the parked ones back in the queue.
//
// One statement for four rows of the transition table, because they differ only
// in what they do to the status: a commitment waiting for a person keeps
// waiting, one in flight finishes what it is doing and finds the desired state
// has moved, and one that had caught up becomes claimable again. What none of
// them may do is come back from a terminal state, which is what the status
// filter says.
//
// A commitment already retrying keeps its next attempt where it was. It is
// already coming back, and pulling it forward would let a card being updated
// every few seconds outrun the backoff its provider asked for.
func aimEditableIntentsTx(ctx context.Context, tx *sql.Tx,
	req outbound.DesiredStateRequest, revision int64) ([]string, error) {

	rows, err := tx.QueryContext(ctx, `
		UPDATE outbound_intents
		SET desired_revision = $2,
		    status = CASE WHEN status = 'idle' THEN 'pending' ELSE status END,
		    next_attempt_at = CASE WHEN status = 'idle' THEN now() ELSE next_attempt_at END,
		    updated_at = now()
		WHERE alert_group_id = $1 AND form = $3
		  AND status IN ('idle', 'pending', 'sending', 'manual_review')
		RETURNING id`,
		req.AlertGroupID, revision, string(outbound.FormEditable))
	if err != nil {
		return nil, fmt.Errorf("aim the commitments of %s: %w", req.AlertGroupID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The journal says WHICH revision was raised, not just that one was. It is
	// the only durable record of when a card started being out of date, and the
	// measure of how long it has been reads it back.
	detail, err := json.Marshal(struct {
		Revision int64  `json:"revision"`
		Final    bool   `json:"final"`
		Reason   string `json:"reason"`
	}{revision, req.Reason.Final(), string(req.Reason)})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := appendIntentEventDetailTx(ctx, tx, id, nextEventSeq, "desired_raised",
			string(req.Reason), req.Actor, detail); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// groupViewTx reads the group as it stands, after the transition wrote, and
// checks that the transition the caller claims is the one the group is in.
//
// The check is the cheap half of what keeps a snapshot honest. A reason is not
// a label: it decides finality, and it says which state the message is being
// frozen in. An acknowledgement whose group is not acknowledged means the
// caller never made the transition, and the revision would freeze a card
// showing a state nobody is in - permanently, because it is what every later
// attempt renders.
func groupViewTx(ctx context.Context, tx *sql.Tx, env renderEnvironment,
	req outbound.DesiredStateRequest) (providers.GroupView, error) {

	group, err := scanAlertGroupRow(tx.QueryRowContext(ctx,
		`SELECT `+alertGroupColumns+` FROM alert_groups WHERE id = $1`, req.AlertGroupID))
	if err != nil {
		return providers.GroupView{}, fmt.Errorf(
			"read the alert group %s: %w", req.AlertGroupID, err)
	}
	if group == nil {
		return providers.GroupView{}, outboundContractf(
			"the desired state of %s was raised, and there is no such alert group",
			req.AlertGroupID)
	}
	if err := reasonMatchesGroup(req.Reason, group.Status); err != nil {
		return providers.GroupView{}, err
	}

	// Whether the alert's team is set up here. An alert that names no team has
	// nothing anybody failed to set up, so its card says nothing about it -
	// the same answer the producer of revision 0 gives.
	var onboarded bool
	if err := tx.QueryRowContext(ctx,
		`SELECT $1 = '' OR EXISTS (SELECT 1 FROM teams WHERE id = $1)`,
		group.TeamID).Scan(&onboarded); err != nil {
		return providers.GroupView{}, fmt.Errorf(
			"read the team of %s: %w", req.AlertGroupID, err)
	}

	return providers.GroupView{
		Group:         group,
		SelfURL:       env.selfURL,
		TeamOnboarded: onboarded,
		Zone:          env.displayZone(),
	}, nil
}

// reasonMatchesGroup says whether the group is in the state the reason claims
// it moved to.
func reasonMatchesGroup(reason outbound.DesiredReason, status model.AlertGroupStatus) error {
	switch reason {
	case outbound.DesiredAck:
		if status != model.AlertGroupStatusAcknowledged {
			return outboundContractf(
				"the desired state was raised for an acknowledgement, and the group is %s", status)
		}
	case outbound.DesiredResolve:
		if status != model.AlertGroupStatusResolved {
			return outboundContractf(
				"the desired state was raised for a resolution, and the group is %s", status)
		}
	case outbound.DesiredMerge:
		// A merge reaches the incident that is open, whatever it has been
		// through: new, being escalated, escalated, or already acknowledged.
		// What it may never reach is one that is over - that alert is finished,
		// and a firing alert arriving now belongs to the next incident.
		switch status {
		case model.AlertGroupStatusResolved, model.AlertGroupStatusClosed:
			return outboundContractf(
				"alerts were merged into %s, an incident that is already over", status)
		}
	}
	return nil
}

// freezeAt canonicalises the group's state at one revision.
//
// A state that cannot be canonicalised fails the whole transition, and that is
// the execution model rather than a policy choice: a render snapshot is
// execution data, and damage in it is a read error for an operator to fix
// rather than something to route around. Committing the transition and dropping
// the revision would be worse than it looks - a resolve would leave the group
// closed, the snapshot not final and the card waiting for a revision that can
// never arrive.
func freezeAt(view providers.GroupView, revision int64,
	alertGroupID string) (keys.RenderSnapshot, error) {

	in := providers.ViewOf(view)
	in.Revision = revision

	state, err := keys.NewRenderSnapshot(in)
	if err != nil {
		return keys.RenderSnapshot{}, fmt.Errorf(
			"freeze the state of %s: %w", alertGroupID, err)
	}
	return state, nil
}
