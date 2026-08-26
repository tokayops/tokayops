package store

import (
	"bytes"
	"context"
	"database/sql"
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

	recordAlerts(t, s, agID, []model.Alert{{
		Fingerprint: "fp-2", Status: model.AlertStatusFiring,
		StartsAt: time.Unix(1700000600, 0),
		Labels:   map[string]string{"alertname": "DiskSlow"},
	}})

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

// intentState is what a raise may and may not touch on one commitment.
type intentState struct {
	status        string
	desired       int64
	nextAttempt   time.Time
	leaseToken    sql.NullString
	currentAttemp sql.NullString
}

func readIntentState(t *testing.T, s *Store, intentID string) intentState {
	t.Helper()
	var out intentState
	if err := s.db.QueryRow(`
		SELECT status, desired_revision, next_attempt_at, lease_token, current_attempt_id
		FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&out.status, &out.desired, &out.nextAttempt, &out.leaseToken,
			&out.currentAttemp); err != nil {
		t.Fatalf("read the commitment %s: %v", intentID, err)
	}
	return out
}

// park puts a commitment in a state the ordinary path does not reach cheaply.
// The lease and the attempt are cleared with it, because the schema says a
// commitment holding either is one that is working.
func park(t *testing.T, s *Store, intentID string, status outbound.Status) {
	t.Helper()
	if _, err := s.db.Exec(`
		UPDATE outbound_intents
		SET status = $2, lease_token = NULL, locked_until = NULL, current_attempt_id = NULL
		WHERE id = $1`, intentID, string(status)); err != nil {
		t.Fatalf("park %s as %s: %v", intentID, status, err)
	}
}

// TestARaiseTouchesExactlyTheFourRowsOfTheTable walks D1's T21-T24 and the
// states that are not in it.
//
// The command is one UPDATE over many rows, which is why it is not in the state
// machine - and why the machine's own comment points here. Nothing else proves
// that an attempt in flight is left alone, that a commitment waiting out a
// backoff keeps the wait its provider asked for, or that a terminal one cannot
// be brought back by an alert moving underneath it.
func TestARaiseTouchesExactlyTheFourRowsOfTheTable(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	ids := admitOne(t, s, agID,
		channelCommitment("C-idle", 0),
		channelCommitment("C-sending", 0),
		channelCommitment("C-backoff", 0),
		channelCommitment("C-review", 0),
		channelCommitment("C-succeeded", 0),
		channelCommitment("C-canceled", 0),
		dmCommitment("U-nina"))

	byTarget := map[string]string{}
	for _, id := range ids {
		var ref string
		if err := s.db.QueryRow(
			`SELECT target_ref FROM outbound_intents WHERE id = $1`, id).Scan(&ref); err != nil {
			t.Fatalf("read the target: %v", err)
		}
		byTarget[ref] = id
	}

	// One claim takes every commitment that is due, so the tokens are collected
	// once and the ones this test does not drive are released again.
	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: testFamily, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	tokens := map[string]string{}
	for _, l := range leased {
		tokens[l.Intent.TargetRef] = l.LeaseToken
	}
	for _, ref := range []string{"C-backoff", "C-review", "C-succeeded", "C-canceled", "U-nina"} {
		if _, err := s.db.Exec(`
			UPDATE outbound_intents SET lease_token = NULL, locked_until = NULL
			WHERE id = $1`, byTarget[ref]); err != nil {
			t.Fatalf("release the lease of %s: %v", ref, err)
		}
	}

	// idle: it sent its card and had caught up.
	idle := byTarget["C-idle"]
	token := tokens["C-idle"]
	begun := beginOne(t, s, idle, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: accepted(),
	}); err != nil {
		t.Fatalf("settle the card: %v", err)
	}
	if got := statusOf(t, s, idle); got != outbound.StatusIdle {
		t.Fatalf("the settled card is %s", got)
	}

	// sending: an attempt is in flight, with a lease and an attempt row.
	sending := byTarget["C-sending"]
	beginOne(t, s, sending, tokens["C-sending"])

	// pending on a backoff: coming back later, at a moment its provider chose.
	backoff := byTarget["C-backoff"]
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET next_attempt_at = now() + interval '1 hour'
		 WHERE id = $1`, backoff); err != nil {
		t.Fatalf("put %s on a backoff: %v", backoff, err)
	}

	park(t, s, byTarget["C-review"], outbound.StatusManualReview)
	park(t, s, byTarget["C-succeeded"], outbound.StatusSucceeded)
	park(t, s, byTarget["C-canceled"], outbound.StatusCanceled)

	before := map[string]intentState{}
	for ref, id := range byTarget {
		before[ref] = readIntentState(t, s, id)
	}

	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)
	result, raiseErr := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
	})
	if raiseErr != nil {
		t.Fatalf("raise the desired state: %v", raiseErr)
	}
	if result.Outcome != outbound.DesiredApplied {
		t.Fatalf("the raise came back %s", result.Outcome)
	}
	next := result.Revision

	cases := []struct {
		ref     string
		row     string
		status  outbound.Status
		aimed   bool
		claimed bool // due now rather than when it was
	}{
		{"C-idle", "T21", outbound.StatusPending, true, true},
		{"C-backoff", "T22", outbound.StatusPending, true, false},
		{"C-sending", "T23", outbound.StatusSending, true, false},
		{"C-review", "T24", outbound.StatusManualReview, true, false},
		{"C-succeeded", "terminal", outbound.StatusSucceeded, false, false},
		{"C-canceled", "terminal", outbound.StatusCanceled, false, false},
		{"U-nina", "one-shot", outbound.StatusPending, false, false},
	}
	if result.Touched != 4 {
		t.Fatalf("the raise aimed %d commitments, want the four rows of the table",
			result.Touched)
	}

	for _, tc := range cases {
		t.Run(tc.ref+" ("+tc.row+")", func(t *testing.T) {
			was, now := before[tc.ref], readIntentState(t, s, byTarget[tc.ref])

			if now.status != string(tc.status) {
				t.Errorf("status is %s, want %s", now.status, tc.status)
			}

			wantDesired := was.desired
			if tc.aimed {
				wantDesired = next
			}
			if now.desired != wantDesired {
				t.Errorf("aimed at revision %d, want %d", now.desired, wantDesired)
			}

			switch {
			case tc.claimed:
				if now.nextAttempt.After(time.Now()) {
					t.Errorf("it is not claimable until %s", now.nextAttempt)
				}
			default:
				if !now.nextAttempt.Equal(was.nextAttempt) {
					t.Errorf("its next attempt moved from %s to %s",
						was.nextAttempt, now.nextAttempt)
				}
			}

			// The lease and the attempt belong to whoever is executing. A raise
			// that touched either would take work away from a live worker.
			if now.leaseToken != was.leaseToken {
				t.Errorf("the lease changed from %v to %v", was.leaseToken, now.leaseToken)
			}
			if now.currentAttemp != was.currentAttemp {
				t.Errorf("the attempt in flight changed from %v to %v",
					was.currentAttemp, now.currentAttemp)
			}
		})
	}
}

// TestADamagedAlertDoesNotEndAnAcknowledgementItHasNoCardFor. A group nobody
// has been paged for has no card, so nothing about the alerts is needed to
// answer that. Reading them anyway - strictly, as execution data - would let a
// row nobody can render end an acknowledgement of a group with no message to
// render in the first place.
func TestADamagedAlertDoesNotEndAnAcknowledgementItHasNoCardFor(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Nobody has been paged")
	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)
	if _, err := s.db.Exec(
		`UPDATE alert_groups SET alerts_data = 'not json at all' WHERE id = $1`,
		agID); err != nil {
		t.Fatalf("write the damaged alerts: %v", err)
	}

	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
	})
	if err != nil {
		t.Fatalf("the acknowledgement was ended by alerts no card needed: %v", err)
	}
	if result.Outcome != outbound.DesiredNoSnapshot {
		t.Fatalf("a group with no admission came back %s", result.Outcome)
	}
}

// TestTwoRaisesWithoutTheGroupLockCannotBothWin is the defensive
// compare-and-set doing what it is for.
//
// Every real caller holds the group's row, which is what makes the revision
// monotonic without a predicate. Without it two transactions can both read
// revision N: the second is released when the first commits, finds the row is
// no longer at N, and updates nothing. Ignoring that answer would aim every
// card at a revision whose snapshot was never stored - and the transaction
// would commit that dangling reference as faithfully as a good one.
//
// The interleaving is forced rather than raced: a competitor holds the snapshot
// row uncommitted, so the raise reads the old revision and then waits on the
// update. Getting the wait wrong makes this test fail rather than pass by
// accident - a raise that read after the commit would simply succeed.
func TestTwoRaisesWithoutTheGroupLockCannotBothWin(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	mustSubmit(t, s, outboundAdmission(t, agID, "Disk filling up",
		channelCommitment("C-ops", 0)))
	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)
	before, _ := storedRevision(t, s, agID)

	// The competitor: it has the snapshot row and has not committed.
	competitor, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer competitor.Rollback()
	if _, err := competitor.Exec(
		`UPDATE outbound_group_snapshots SET revision = revision + 1
		 WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("the competitor's write: %v", err)
	}

	loser, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer loser.Rollback()

	raised := make(chan error, 1)
	go func() {
		_, err := setDesiredStateTx(context.Background(), loser, s.render,
			outbound.DesiredStateRequest{
				AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: "nina",
			})
		raised <- err
	}()

	waitForABlockedStatement(t, s)
	if err := competitor.Commit(); err != nil {
		t.Fatalf("commit the competitor: %v", err)
	}

	select {
	case err := <-raised:
		if err == nil {
			t.Fatal("the raise stored nothing and said it had")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the raise never came back")
	}
	if err := loser.Rollback(); err != nil {
		t.Fatalf("roll back: %v", err)
	}

	// Nothing of the loser's survives, and the competitor's number stands.
	if after, _ := storedRevision(t, s, agID); after != before+1 {
		t.Fatalf("the revision went from %d to %d", before, after)
	}
	var desired int64
	if err := s.db.QueryRow(`
		SELECT desired_revision FROM outbound_intents
		WHERE alert_group_id = $1 AND form = $2`, agID, string(outbound.FormEditable)).
		Scan(&desired); err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	if desired != before {
		t.Fatalf("the card was aimed at revision %d by a raise that stored nothing", desired)
	}
}

// waitForABlockedStatement waits until something in this database is waiting
// for a lock. The test database is ephemeral and serves one test at a time, so
// an ungranted lock is the statement this test is waiting to see block.
func waitForABlockedStatement(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&blocked); err != nil {
			t.Fatalf("read the locks: %v", err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing ever blocked, so the interleaving this test needs did not happen")
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
