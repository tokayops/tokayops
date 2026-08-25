package store

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Raising the desired state of a group is four writes - the snapshot, the
// revision on every editable commitment, the return of the parked ones to the
// queue, and the line each of them gets - and they are one fact. The tests
// below are mostly about the ways that fact can be broken in half.

func desiredGroup(t *testing.T, s *Store, title string) string {
	t.Helper()
	id := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: "desired-" + id, Status: model.AlertGroupStatusNew,
		Title: title, Severity: "critical", TeamID: "team-1",
		Alerts: []model.Alert{{
			Fingerprint: "fp-1", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000000, 0),
			Labels:   map[string]string{"alertname": "DiskWillFill"},
		}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}
	return id
}

// moveGroup puts the group in the state a transition would leave it in. The
// command checks the two against each other, so a test that raises a revision
// has to have made the move first - which is also what the real doors do, in
// the same transaction.
func moveGroup(t *testing.T, s *Store, agID string, status model.AlertGroupStatus) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE alert_groups SET status = $2, acknowledged_by = CASE WHEN $2 = 'acknowledged'
		 THEN 'nina' ELSE acknowledged_by END, resolved_by = CASE WHEN $2 = 'resolved'
		 THEN 'nina' ELSE resolved_by END WHERE id = $1`, agID, string(status)); err != nil {
		t.Fatalf("move the group to %s: %v", status, err)
	}
}

// storedStatus is what the stored snapshot says the alert looked like - the
// thing every message of that revision renders.
func storedStatus(t *testing.T, s *Store, agID string) keys.GroupStatus {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow(
		`SELECT snapshot FROM outbound_group_snapshots WHERE alert_group_id = $1`, agID).
		Scan(&raw); err != nil {
		t.Fatalf("read the stored snapshot: %v", err)
	}
	var snapshot keys.RenderSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("read the stored snapshot back: %v", err)
	}
	return snapshot.Content().Status
}

// storedDigest is the content identity of the stored state.
func storedDigest(t *testing.T, s *Store, agID string) []byte {
	t.Helper()
	var digest []byte
	if err := s.db.QueryRow(
		`SELECT snapshot_digest FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		agID).Scan(&digest); err != nil {
		t.Fatalf("read the stored digest: %v", err)
	}
	return digest
}

// raiseDesired runs the command the way every real caller does: inside a
// transaction that has already taken the group's row.
func raiseDesired(t *testing.T, s *Store,
	req outbound.DesiredStateRequest) (outbound.DesiredStateResult, error) {

	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`SELECT 1 FROM alert_groups WHERE id = $1 FOR UPDATE`, req.AlertGroupID); err != nil {
		t.Fatalf("lock the group: %v", err)
	}

	result, err := setDesiredStateTx(context.Background(), tx, s.render, req)
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return result, nil
}

func storedRevision(t *testing.T, s *Store, agID string) (int64, bool) {
	t.Helper()
	var revision int64
	var final bool
	if err := s.db.QueryRow(
		`SELECT revision, final FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		agID).Scan(&revision, &final); err != nil {
		t.Fatalf("read the stored state: %v", err)
	}
	return revision, final
}

// TestDesiredStateRaisesARevisionAndAimsTheCards is the whole happy path: what
// the snapshot row becomes, which commitments are aimed at it, which are left
// alone, and what their history says.
func TestDesiredStateRaisesARevisionAndAimsTheCards(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0), dmCommitment("U-nina")))
	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)

	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
	})
	if err != nil {
		t.Fatalf("raise the desired state: %v", err)
	}
	if result.Outcome != outbound.DesiredApplied {
		t.Fatalf("the proposal came back %s", result.Outcome)
	}
	if result.Revision != 1 {
		t.Fatalf("the group is at revision %d, want 1", result.Revision)
	}
	if result.Touched != 1 {
		t.Fatalf("%d commitments were aimed at it, want 1 - the card, not the DM",
			result.Touched)
	}

	revision, final := storedRevision(t, s, agID)
	if revision != 1 || final {
		t.Fatalf("the stored state is at revision %d, final=%v", revision, final)
	}
	// And it is the acknowledged alert that was frozen, not the one before it.
	if status := storedStatus(t, s, agID); status != keys.GroupAcknowledged {
		t.Fatalf("the stored state shows the alert as %s", status)
	}

	// The card is aimed at the new revision and back in the queue; the direct
	// message has no revisions at all and is left exactly as it was.
	rows, err := s.db.Query(`
		SELECT form, desired_revision, status, next_attempt_at <= now()
		FROM outbound_intents WHERE alert_group_id = $1 ORDER BY form`, agID)
	if err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var form, status string
		var desired int64
		var due bool
		if err := rows.Scan(&form, &desired, &status, &due); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		switch form {
		case string(outbound.FormEditable):
			if desired != 1 {
				t.Errorf("the card is aimed at revision %d", desired)
			}
			if status != string(outbound.StatusPending) || !due {
				t.Errorf("the card is %s, due=%v", status, due)
			}
		case string(outbound.FormOneShot):
			if desired != 0 {
				t.Errorf("the direct message was aimed at revision %d", desired)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("the group holds %d commitments, want 2", seen)
	}

	// The journal says WHICH revision was raised. Without it nothing can say
	// when a card started being out of date.
	var kind, reason string
	var detail []byte
	if err := s.db.QueryRow(`
		SELECT e.kind, e.reason, e.detail
		FROM outbound_intent_events e
		JOIN outbound_intents i ON i.id = e.intent_id
		WHERE i.alert_group_id = $1 AND e.kind = 'desired_raised'`, agID).
		Scan(&kind, &reason, &detail); err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if reason != string(outbound.DesiredAck) {
		t.Errorf("the journal says the reason was %q", reason)
	}
	var recorded struct {
		Revision int64  `json:"revision"`
		Final    bool   `json:"final"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(detail, &recorded); err != nil {
		t.Fatalf("read the recorded detail: %v", err)
	}
	if recorded.Revision != 1 || recorded.Final || recorded.Reason != "ack" {
		t.Errorf("the journal recorded %+v", recorded)
	}
}

// TestDesiredStateIsUnchangedWhenTheMessageWouldNotMove. Alertmanager repeats
// the same payload for as long as an alert is firing. A revision raised for
// every repeat is a polling loop with the sender's period, and every turn of it
// edits a message into exactly what it already said.
func TestDesiredStateIsUnchangedWhenTheMessageWouldNotMove(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))

	first, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredMerge, Actor: "ingester",
	})
	if err != nil || first.Outcome != outbound.DesiredApplied {
		t.Fatalf("the first proposal came back %s (%v)", first.Outcome, err)
	}

	second, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredMerge, Actor: "ingester",
	})
	if err != nil {
		t.Fatalf("raise the desired state again: %v", err)
	}
	if second.Outcome != outbound.DesiredUnchanged {
		t.Fatalf("an identical repeat came back %s", second.Outcome)
	}
	if second.Revision != first.Revision {
		t.Fatalf("the revision moved from %d to %d for a repeat",
			first.Revision, second.Revision)
	}

	var events int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_intent_events e
		JOIN outbound_intents i ON i.id = e.intent_id
		WHERE i.alert_group_id = $1 AND e.kind = 'desired_raised'`, agID).
		Scan(&events); err != nil {
		t.Fatalf("count the journal: %v", err)
	}
	if events != 1 {
		t.Fatalf("a repeat that changed nothing wrote %d lines of history", events)
	}
}

// TestTheFinalRevisionIsRaisedEvenWhenNothingMoved. Finality is a change of
// state, not of content: a commitment parked at an equal revision takes no
// further attempt, so without a revision of its own the resolve would never be
// applied and the card would never be finished.
func TestTheFinalRevisionIsRaisedEvenWhenNothingMoved(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))

	if _, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredMerge, Actor: "ingester",
	}); err != nil {
		t.Fatalf("raise the desired state: %v", err)
	}
	digestBefore := storedDigest(t, s, agID)

	moveGroup(t, s, agID, model.AlertGroupStatusResolved)
	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredResolve, Actor: "nina",
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if result.Outcome != outbound.DesiredApplied || result.Revision != 2 {
		t.Fatalf("the resolve came back %s at revision %d", result.Outcome, result.Revision)
	}
	if revision, final := storedRevision(t, s, agID); revision != 2 || !final {
		t.Fatalf("the stored state is at revision %d, final=%v", revision, final)
	}
	if status := storedStatus(t, s, agID); status != keys.GroupResolved {
		t.Fatalf("the final state shows the alert as %s", status)
	}

	// The point of the test, said directly: what changed is the alert's state
	// and nothing else about the message, and that alone is enough - a
	// commitment parked at an equal revision would take no further attempt.
	if bytes.Equal(digestBefore, storedDigest(t, s, agID)) {
		t.Fatal("the resolution did not change the state that gets rendered")
	}
}

// TestDesiredStateRefusesToMoveAfterFinal. A late payload cannot raise a
// revision over the one that resolved the alert: nothing would ever apply it -
// the commitment that could is terminal - and the lag it created could never be
// closed by anybody.
func TestDesiredStateRefusesToMoveAfterFinal(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))

	moveGroup(t, s, agID, model.AlertGroupStatusResolved)
	if _, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredResolve, Actor: "nina",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if err := s.UpdateAlertGroupAlerts(agID, []model.Alert{{
		Fingerprint: "fp-2", Status: model.AlertStatusFiring,
		StartsAt: time.Unix(1700000600, 0),
		Labels:   map[string]string{"alertname": "DiskSlow"},
	}}); err != nil {
		t.Fatalf("record the late alerts: %v", err)
	}

	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredResolve, Actor: "nina",
	})
	if err != nil {
		t.Fatalf("raise the desired state: %v", err)
	}
	if result.Outcome != outbound.DesiredStaleAfterFinal {
		t.Fatalf("a second resolve came back %s", result.Outcome)
	}
	if revision, _ := storedRevision(t, s, agID); revision != result.Revision {
		t.Fatal("the refused proposal moved the revision anyway")
	}
}

// TestDesiredStateWithoutASnapshotChangesNothing. A group nobody has been paged
// for has no state to supersede: its revision 0 is frozen by the admission,
// from a group that already includes whatever just happened to it.
func TestDesiredStateWithoutASnapshotChangesNothing(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Nobody has been paged")
	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)

	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
	})
	if err != nil {
		t.Fatalf("raise the desired state: %v", err)
	}
	if result.Outcome != outbound.DesiredNoSnapshot {
		t.Fatalf("a group with no admission came back %s", result.Outcome)
	}
}

// TestAStateThatCannotBeFrozenEndsTheWholeTransition is the execution model
// applied to a card.
//
// A render snapshot is execution data: damage in it is a read error an operator
// fixes, not something to route around. Committing the transition and dropping
// the revision would be worse than it looks - a resolve would leave the group
// closed, the snapshot not final, and the card waiting for a revision that can
// never arrive.
func TestAStateThatCannotBeFrozenEndsTheWholeTransition(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))

	// Two alerts under one fingerprint: the order the protocol sorts by stops
	// being total, so the state has no single canonical form.
	if _, err := s.db.Exec(`UPDATE alert_groups SET alerts_data = $2 WHERE id = $1`, agID,
		`[{"fingerprint":"fp-1","status":"firing","startsAt":"2023-11-14T22:13:20Z",`+
			`"labels":{"alertname":"A"}},`+
			`{"fingerprint":"fp-1","status":"firing","startsAt":"2023-11-14T22:14:20Z",`+
			`"labels":{"alertname":"B"}}]`); err != nil {
		t.Fatalf("write the damaged alerts: %v", err)
	}

	// The transition the caller was making, with the raise inside it.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(
		`UPDATE alert_groups SET status = $2 WHERE id = $1`,
		agID, model.AlertGroupStatusAcknowledged); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if _, err := setDesiredStateTx(context.Background(), tx, s.render,
		outbound.DesiredStateRequest{
			AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
		}); err == nil {
		tx.Rollback()
		t.Fatal("a state that cannot be frozen was accepted")
	}
	tx.Rollback()

	var status string
	if err := s.db.QueryRow(`SELECT status FROM alert_groups WHERE id = $1`, agID).
		Scan(&status); err != nil {
		t.Fatalf("read the group: %v", err)
	}
	if status != string(model.AlertGroupStatusNew) &&
		status != string(model.AlertGroupStatusProcessing) {
		t.Fatalf("the acknowledgement committed without its revision: status is %s", status)
	}
}

// TestAReasonThatTheGroupContradictsIsRefused. The reason is not a label on the
// history: it decides whether the revision is the last one, and it says which
// state the message is being frozen in. A caller that raises "acknowledged"
// over a group that is not acknowledged has not made the transition it claims,
// and the card would show a state nobody is in - permanently, because that is
// what every later attempt renders.
func TestAReasonThatTheGroupContradictsIsRefused(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))
	before, _ := storedRevision(t, s, agID)

	for _, tc := range []struct {
		name   string
		status model.AlertGroupStatus
		reason outbound.DesiredReason
	}{
		{"an acknowledgement of a group nobody acknowledged",
			model.AlertGroupStatusProcessing, outbound.DesiredAck},
		{"a resolution of a group that is still open",
			model.AlertGroupStatusAcknowledged, outbound.DesiredResolve},
		{"alerts merged into an incident that is over",
			model.AlertGroupStatusResolved, outbound.DesiredMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			moveGroup(t, s, agID, tc.status)
			if _, err := raiseDesired(t, s, outbound.DesiredStateRequest{
				AlertGroupID: agID, Reason: tc.reason, Actor: "nina",
			}); err == nil {
				t.Fatal("the desired state was raised for a transition nobody made")
			}
			if after, _ := storedRevision(t, s, agID); after != before {
				t.Fatalf("the refused proposal moved the revision to %d", after)
			}
		})
	}
}

// TestAStateThisBuildCannotWriteIsLeftAlone. A snapshot stored under a schema
// version this build does not know belongs to an instance that is ahead.
// Rewriting it with what this build can express would delete the part a newer
// renderer needs - and the read side already refuses such a row, so the card
// would then be unrenderable by both of us. Stopping leaves the work for the
// instance that can do it.
func TestAStateThisBuildCannotWriteIsLeftAlone(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))
	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)

	if _, err := s.db.Exec(
		`UPDATE outbound_group_snapshots SET snapshot_schema_version = 2
		 WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("write the newer state: %v", err)
	}

	if _, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
	}); err == nil {
		t.Fatal("a state from a newer schema was overwritten")
	}

	var version int
	if err := s.db.QueryRow(
		`SELECT snapshot_schema_version FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		agID).Scan(&version); err != nil {
		t.Fatalf("read the state back: %v", err)
	}
	if version != 2 {
		t.Fatalf("the stored state came back at version %d", version)
	}
}

// TestTheFourWritesCommitTogether. The snapshot, the revision on the
// commitments, their return to the queue and their history are one fact. A
// caller whose transaction fails after the raise must leave none of them
// behind: a card aimed at a revision whose snapshot was never stored has
// nothing to render, and a snapshot nothing was aimed at is a message that
// stays wrong for as long as the alert lives.
func TestTheFourWritesCommitTogether(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))

	before, _ := storedRevision(t, s, agID)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(
		`UPDATE alert_groups SET status = $2 WHERE id = $1 `,
		agID, string(model.AlertGroupStatusAcknowledged)); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	result, err := setDesiredStateTx(context.Background(), tx, s.render,
		outbound.DesiredStateRequest{
			AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
		})
	if err != nil || result.Outcome != outbound.DesiredApplied {
		tx.Rollback()
		t.Fatalf("the raise came back %s (%v)", result.Outcome, err)
	}
	// Whatever the caller was doing next did not work out.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("roll back: %v", err)
	}

	if after, _ := storedRevision(t, s, agID); after != before {
		t.Errorf("the snapshot survived a rolled back transition: %d -> %d", before, after)
	}

	var desired int64
	var status string
	if err := s.db.QueryRow(`
		SELECT desired_revision, status FROM outbound_intents
		WHERE alert_group_id = $1 AND form = $2`, agID, string(outbound.FormEditable)).
		Scan(&desired, &status); err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	if desired != before {
		t.Errorf("the commitment is aimed at revision %d, and no such state exists", desired)
	}

	var events int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_intent_events e
		JOIN outbound_intents i ON i.id = e.intent_id
		WHERE i.alert_group_id = $1 AND e.kind = 'desired_raised'`, agID).
		Scan(&events); err != nil {
		t.Fatalf("count the journal: %v", err)
	}
	if events != 0 {
		t.Errorf("the journal kept %d lines about a revision that does not exist", events)
	}
}
