package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Admission is the moment a promise starts existing, so these tests are mostly
// about what happens when two producers reach it at once. One of them has to
// win completely: its set of commitments, its snapshot, its policy on the
// group. The loser has to be told which of the two answers it got - the same
// work was already accepted, or different work was - and must leave nothing
// behind either way.

func outboundSnapshot(t *testing.T, groupID, title string) keys.RenderSnapshot {
	t.Helper()
	snapshot, err := keys.NewRenderSnapshot(keys.SnapshotInput{
		AlertGroupID:    groupID,
		Revision:        0,
		Status:          keys.GroupProcessing,
		Title:           title,
		Severity:        "critical",
		DisplayTimezone: "UTC",
		Alerts: []keys.AlertSnapshot{{
			Fingerprint: "fp-1", Status: keys.AlertFiring,
			StartsAt: time.Unix(1700000000, 0), AlertName: "DiskWillFill",
			Severity: "critical",
		}},
	})
	if err != nil {
		t.Fatalf("build the snapshot: %v", err)
	}
	return snapshot
}

func channelCommitment(ref string, offset time.Duration) keys.EscalationCommitment {
	return keys.EscalationCommitment{
		Slot:            keys.Slot{Kind: keys.SlotFirehose},
		Provider:        "slack",
		Target:          keys.Target{Kind: keys.TargetChannel, Ref: ref},
		Editable:        true,
		Interactive:     true,
		Timing:          keys.TimingSpec{Kind: keys.TimingRelativeToAdmission, Offset: offset},
		CompletionMode:  keys.CompletionOnAcceptance,
		AmbiguityPolicy: keys.PolicyRetry,
	}
}

func outboundAdmission(t *testing.T, groupID, title string,
	commitments ...keys.EscalationCommitment) outbound.EscalationAdmission {
	t.Helper()

	admission, err := keys.EscalationBatch{
		Kind:               keys.KindEscalation,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Snapshot:           outboundSnapshot(t, groupID, title),
		Commitments:        commitments,
	}.Admit()
	if err != nil {
		t.Fatalf("admit the batch: %v", err)
	}

	return outbound.EscalationAdmission{
		Admission:      admission,
		PolicyID:       "policy-" + title,
		PolicySnapshot: json.RawMessage(`{"name":"` + title + `"}`),
		// Who this producer believed was on duty. It travels with the admission
		// so the winner writes it and the loser does not - the group must not
		// display one set of people while another set is being paged.
		OnCallSnapshot: json.RawMessage(`{"l1_users":[],"source":"` + title + `"}`),
		Actor:          "engine",
	}
}

// storedOnCall is what the group says about who was on duty. A NULL column
// comes back as "", which is how these tests spell "nothing was recorded".
func storedOnCall(t *testing.T, s *Store, agID string) string {
	t.Helper()
	var source sql.NullString
	if err := s.db.QueryRow(
		`SELECT oncall_snapshot->>'source' FROM alert_groups WHERE id = $1`, agID).
		Scan(&source); err != nil {
		t.Fatalf("read the on-call snapshot: %v", err)
	}
	return source.String
}

func mustSubmit(t *testing.T, s *Store, adm outbound.EscalationAdmission) outbound.SubmitResult {
	t.Helper()
	result, err := s.SubmitEscalationBatch(context.Background(), adm)
	if err != nil {
		t.Fatalf("submit the admission: %v", err)
	}
	return result
}

// TestSubmitAdmitsAnEscalationOnce covers the whole happy path in one place:
// what the claim writes, what the commitments look like, and what the group
// ends up saying about the escalation it is running.
func TestSubmitAdmitsAnEscalationOnce(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	adm := outboundAdmission(t, agID, "first",
		channelCommitment("C0001", 0),
		channelCommitment("C0002", 5*time.Minute))

	result := mustSubmit(t, s, adm)
	if result.Outcome != outbound.SubmitCreated {
		t.Fatalf("the first admission answered %q", result.Outcome)
	}
	if len(result.IntentIDs) != 2 {
		t.Fatalf("expected two commitments, got %d", len(result.IntentIDs))
	}

	intents, err := s.ListIntentsByAlertGroup(context.Background(), agID)
	if err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	if len(intents) != 2 {
		t.Fatalf("the group holds %d commitments", len(intents))
	}
	for _, intent := range intents {
		if intent.Status != outbound.StatusPending {
			t.Errorf("a fresh commitment is %s", intent.Status)
		}
		if intent.AttemptsInGeneration != 0 || intent.FailureStreak != 0 {
			t.Errorf("a fresh commitment already has history: %+v", intent)
		}
	}

	// The step with a delay is due later than the one without, and both are
	// measured from the admission rather than from either instance's clock.
	var immediate, delayed time.Time
	if err := s.db.QueryRow(`
		SELECT min(not_before), max(not_before) FROM outbound_intents WHERE alert_group_id = $1`,
		agID).Scan(&immediate, &delayed); err != nil {
		t.Fatalf("read the schedule: %v", err)
	}
	if gap := delayed.Sub(immediate); gap != 5*time.Minute {
		t.Fatalf("the delayed step is %s behind the first, want 5m", gap)
	}

	// The state the commitments are about, stored once, with its digest.
	var revision int64
	var digest []byte
	if err := s.db.QueryRow(`
		SELECT revision, snapshot_digest FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		agID).Scan(&revision, &digest); err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if revision != 0 {
		t.Errorf("the first snapshot is revision %d", revision)
	}
	if want := adm.Admission.Snapshot.Digest(); string(digest) != string(want) {
		t.Error("the stored snapshot digest is not the one the admission was built on")
	}

	// The group is escalating, and says what it is escalating by.
	var status, policyID string
	if err := s.db.QueryRow(
		`SELECT status, policy_id FROM alert_groups WHERE id = $1`, agID).
		Scan(&status, &policyID); err != nil {
		t.Fatalf("read the group: %v", err)
	}
	if status != string(model.AlertGroupStatusProcessing) {
		t.Errorf("the group is %s", status)
	}
	if policyID != "policy-first" {
		t.Errorf("the group escalates by %q", policyID)
	}

	// Every commitment says it was created.
	var events int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_intent_events e
		JOIN outbound_intents i ON i.id = e.intent_id
		WHERE i.alert_group_id = $1 AND e.kind = 'created'`, agID).Scan(&events); err != nil {
		t.Fatalf("count the events: %v", err)
	}
	if events != 2 {
		t.Errorf("%d commitments recorded their creation, want 2", events)
	}
}

// TestSubmitRepeatIsIdempotent is the answer a producer needs after a lost
// reply: the same work, already accepted, and no second set of commitments.
func TestSubmitRepeatIsIdempotent(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))

	first := mustSubmit(t, s, adm)
	second := mustSubmit(t, s, adm)

	if second.Outcome != outbound.SubmitExisting {
		t.Fatalf("a repeat answered %q", second.Outcome)
	}
	if second.BatchID != first.BatchID {
		t.Error("a repeat found a different claim")
	}
	if len(second.IntentIDs) != 1 || second.IntentIDs[0] != first.IntentIDs[0] {
		t.Errorf("a repeat returned different commitments: %v against %v",
			second.IntentIDs, first.IntentIDs)
	}

	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1`, agID).Scan(&count); err != nil {
		t.Fatalf("count the commitments: %v", err)
	}
	if count != 1 {
		t.Fatalf("a repeat created %d commitments", count)
	}
}

// TestSubmitRefusesDifferentWorkUnderOneClaim is the case that must never be a
// merge. Two producers computed different audiences - somebody's identity was
// linked between their reads - and the second one is told so rather than having
// its recipients quietly added to the first one's page.
func TestSubmitRefusesDifferentWorkUnderOneClaim(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	first := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
	if got := mustSubmit(t, s, first).Outcome; got != outbound.SubmitCreated {
		t.Fatalf("the first admission answered %q", got)
	}

	second := outboundAdmission(t, agID, "first",
		channelCommitment("C0001", 0), channelCommitment("C0002", 0))
	result := mustSubmit(t, s, second)
	if result.Outcome != outbound.SubmitConflict {
		t.Fatalf("a different set answered %q", result.Outcome)
	}

	var count int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1`, agID).Scan(&count); err != nil {
		t.Fatalf("count the commitments: %v", err)
	}
	if count != 1 {
		t.Fatalf("the refused set left %d commitments behind", count)
	}
}

// TestSubmitLoserWritesNothingAboutTheGroup is the race that cost a design
// round: the loser used to record its own policy snapshot on the group, so the
// group described one escalation while another one was executing.
func TestSubmitLoserWritesNothingAboutTheGroup(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	winner := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
	mustSubmit(t, s, winner)

	loser := outboundAdmission(t, agID, "second", channelCommitment("C0009", 0))
	if got := mustSubmit(t, s, loser).Outcome; got != outbound.SubmitConflict {
		t.Fatalf("the second admission answered %q", got)
	}

	var policyID string
	var snapshot []byte
	if err := s.db.QueryRow(
		`SELECT policy_id, policy_snapshot FROM alert_groups WHERE id = $1`, agID).
		Scan(&policyID, &snapshot); err != nil {
		t.Fatalf("read the group: %v", err)
	}
	if policyID != "policy-first" {
		t.Fatalf("the loser overwrote the policy: the group escalates by %q", policyID)
	}
	// Compared as JSON: the column normalises whitespace, and what matters is
	// whose policy is recorded, not how the database spells it.
	var stored struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(snapshot, &stored); err != nil {
		t.Fatalf("read the stored policy snapshot: %v", err)
	}
	if stored.Name != "first" {
		t.Fatalf("the loser overwrote the policy snapshot: it names %q", stored.Name)
	}

	// And who was on duty, which is the same claim about the same moment: the
	// two producers read the schedule at different instants, and the group has
	// to name the people the accepted commitments are aimed at.
	if got := storedOnCall(t, s, agID); got != "first" {
		t.Fatalf("the loser overwrote the on-call snapshot: it names %q", got)
	}

	var storedTitle string
	if err := s.db.QueryRow(
		`SELECT snapshot->>'title' FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		agID).Scan(&storedTitle); err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if storedTitle != "first" {
		t.Fatalf("the loser overwrote the content snapshot: it says %q", storedTitle)
	}
}

// TestSubmitConcurrentProducers is the same two answers, arrived at from two
// connections at once rather than one after the other.
func TestSubmitConcurrentProducers(t *testing.T) {
	s := setupTestDB(t)

	t.Run("the same work", func(t *testing.T) {
		agID := outboundGroup(t, s)
		adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))

		outcomes := submitConcurrently(t, s, adm, adm)
		if outcomes[outbound.SubmitCreated] != 1 || outcomes[outbound.SubmitExisting] != 1 {
			t.Fatalf("two identical admissions answered %v", outcomes)
		}
	})

	t.Run("different work", func(t *testing.T) {
		agID := outboundGroup(t, s)
		first := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
		second := outboundAdmission(t, agID, "first",
			channelCommitment("C0001", 0), channelCommitment("C0002", 0))

		outcomes := submitConcurrently(t, s, first, second)
		if outcomes[outbound.SubmitCreated] != 1 || outcomes[outbound.SubmitConflict] != 1 {
			t.Fatalf("two different admissions answered %v", outcomes)
		}

		var count int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1`, agID).
			Scan(&count); err != nil {
			t.Fatalf("count the commitments: %v", err)
		}
		if count != 1 && count != 2 {
			t.Fatalf("neither set was accepted whole: %d commitments", count)
		}
	})
}

func submitConcurrently(t *testing.T, s *Store,
	admissions ...outbound.EscalationAdmission) map[outbound.SubmitOutcome]int {
	t.Helper()

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]outbound.SubmitResult, len(admissions))
	errs := make([]error, len(admissions))

	for i, adm := range admissions {
		wg.Add(1)
		go func(i int, adm outbound.EscalationAdmission) {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.SubmitEscalationBatch(context.Background(), adm)
		}(i, adm)
	}
	close(start)
	wg.Wait()

	outcomes := map[outbound.SubmitOutcome]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent admission %d: %v", i, err)
		}
		outcomes[results[i].Outcome]++
	}
	return outcomes
}

// TestSubmitLeavesAnAcknowledgedGroupAlone: the user got there first, and an
// escalation admitted afterwards would page people for an alert somebody is
// already handling.
func TestSubmitLeavesAnAcknowledgedGroupAlone(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	forceAlertGroupStatus(t, s, agID, model.AlertGroupStatusAcknowledged)

	result := mustSubmit(t, s, outboundAdmission(t, agID, "first", channelCommitment("C0001", 0)))
	if result.Outcome != outbound.SubmitGroupNotAdmitted {
		t.Fatalf("an acknowledged group answered %q", result.Outcome)
	}

	var batches, intents int
	if err := s.db.QueryRow(
		`SELECT (SELECT count(*) FROM outbound_batches WHERE alert_group_id = $1),
		        (SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1)`, agID).
		Scan(&batches, &intents); err != nil {
		t.Fatalf("count what was written: %v", err)
	}
	if batches != 0 || intents != 0 {
		t.Fatalf("a refused admission left %d claims and %d commitments", batches, intents)
	}
}

// TestSubmitRecordsAnAdmissionWithNobodyToNotify is the outcome that used to be
// silence. The claim exists, the snapshot exists, the history says why, and the
// group is never picked up again - which is exactly why the fact has to be
// visible rather than implied by an empty set.
func TestSubmitRecordsAnAdmissionWithNobodyToNotify(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	adm := outboundAdmission(t, agID, "first")
	adm.Unpromised = []outbound.UnpromisedStep{{
		Step: "schedule sched-1", Reason: outbound.ReasonNobodyOnCall,
	}}

	result := mustSubmit(t, s, adm)
	if result.Outcome != outbound.SubmitCreated {
		t.Fatalf("an empty admission answered %q", result.Outcome)
	}

	var outcome string
	var count int
	if err := s.db.QueryRow(
		`SELECT admission_outcome, intent_count FROM outbound_batches WHERE alert_group_id = $1`,
		agID).Scan(&outcome, &count); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if outcome != string(keys.OutcomeNoTargets) || count != 0 {
		t.Fatalf("the claim says %q with %d commitments", outcome, count)
	}

	var snapshots int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_group_snapshots WHERE alert_group_id = $1`, agID).
		Scan(&snapshots); err != nil {
		t.Fatalf("count the snapshots: %v", err)
	}
	if snapshots != 1 {
		t.Fatalf("an empty admission stored %d snapshots", snapshots)
	}

	var notes int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM timeline_events
		WHERE alert_group_id = $1 AND type = $2`,
		agID, model.TimelineEventNotificationFailed).Scan(&notes); err != nil {
		t.Fatalf("count the timeline notes: %v", err)
	}
	if notes != 2 {
		t.Fatalf("the history has %d notes about having nobody to notify, want 2", notes)
	}

	// And the note says WHICH of the reasons it was. "No delivery" sends a
	// reader to the schedule, the policy or the deployment depending on the
	// answer, so the answer travels with the line rather than being inferred
	// from its wording.
	var reason string
	if err := s.db.QueryRow(`
		SELECT metadata::jsonb->>'reason' FROM timeline_events
		WHERE alert_group_id = $1 AND metadata::jsonb->>'step' = 'schedule sched-1'`,
		agID).Scan(&reason); err != nil {
		t.Fatalf("read the note: %v", err)
	}
	if reason != string(outbound.ReasonNobodyOnCall) {
		t.Errorf("the note blames %q", reason)
	}
}

// TestSubmitRefusesWhatThisBuildCannotDeliver is the admission gate. Each of
// these would become a durable promise whose first attempt is guaranteed to
// fail - a promise the domain would have to break by design.
func TestSubmitRefusesWhatThisBuildCannotDeliver(t *testing.T) {
	s := setupTestDB(t)

	spoil := []struct {
		name   string
		change func(*keys.EscalationCommitment)
	}{
		{
			name:   "a provider nobody delivers through",
			change: func(c *keys.EscalationCommitment) { c.Provider = "carrier-pigeon" },
		},
		{
			name: "a channel that waits for the provider's own confirmation",
			change: func(c *keys.EscalationCommitment) {
				c.CompletionMode = keys.CompletionOnProviderReceipt
			},
		},
		{
			name: "a policy that promises a reconciliation nobody can perform",
			change: func(c *keys.EscalationCommitment) {
				c.AmbiguityPolicy = keys.PolicyReconcileThenRetry
			},
		},
		{
			name: "assuming delivery of a card that could still be updated",
			change: func(c *keys.EscalationCommitment) {
				c.AmbiguityPolicy = keys.PolicyAssumeAccepted
				c.Editable = true
			},
		},
		{
			name: "a deadline that has already passed",
			change: func(c *keys.EscalationCommitment) {
				at := time.Now().Add(-time.Hour)
				c.Expiry = &keys.TimingSpec{Kind: keys.TimingAbsolute, At: at}
			},
		},
	}

	for _, tc := range spoil {
		t.Run(tc.name, func(t *testing.T) {
			agID := outboundGroup(t, s)
			commitment := channelCommitment("C0001", 0)
			tc.change(&commitment)

			_, err := s.SubmitEscalationBatch(context.Background(),
				outboundAdmission(t, agID, "first", commitment))
			if err == nil {
				t.Fatal("the admission gate accepted a delivery this build cannot make")
			}
			if !errors.Is(err, outbound.ErrNotAdmissible) {
				t.Fatalf("expected the admission to be refused, got: %v", err)
			}

			var batches int
			if err := s.db.QueryRow(
				`SELECT count(*) FROM outbound_batches WHERE alert_group_id = $1`, agID).
				Scan(&batches); err != nil {
				t.Fatalf("count the claims: %v", err)
			}
			if batches != 0 {
				t.Fatalf("a refused admission left %d claims behind", batches)
			}
		})
	}
}

// TestSubmitRecordsWhoWasOnDuty: the snapshot is part of the admission, so it
// lands in the same commit as the commitments it describes. It used to be a
// separate write after the fact, which left a window where the group was
// escalating and named nobody - and, on the losing branch, a window where it
// named the wrong people.
func TestSubmitRecordsWhoWasOnDuty(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	mustSubmit(t, s, outboundAdmission(t, agID, "first", channelCommitment("C0001", 0)))

	if got := storedOnCall(t, s, agID); got != "first" {
		t.Fatalf("the group says %q was on duty", got)
	}
}

// TestSubmitWithoutAnOnCallAnswerRecordsNothing. Nothing to say and "nobody was
// on call" are different claims, and only one of them belongs on the group. A
// producer whose schedule read failed hands over no snapshot, and the column
// stays as it was rather than being blanked into an assertion nobody made.
func TestSubmitWithoutAnOnCallAnswerRecordsNothing(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	t.Run("a fresh group is left empty", func(t *testing.T) {
		agID := outboundGroup(t, s)
		adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
		adm.OnCallSnapshot = nil

		mustSubmit(t, s, adm)

		var snapshot sql.NullString
		if err := s.db.QueryRow(
			`SELECT oncall_snapshot FROM alert_groups WHERE id = $1`, agID).
			Scan(&snapshot); err != nil {
			t.Fatalf("read the group: %v", err)
		}
		if snapshot.Valid {
			t.Fatalf("a producer that could not read the schedule recorded %q", snapshot.String)
		}
	})

	t.Run("an answer already there survives", func(t *testing.T) {
		agID := outboundGroup(t, s)
		if _, err := s.db.ExecContext(ctx,
			`UPDATE alert_groups SET oncall_snapshot = '{"source":"earlier"}'::jsonb WHERE id = $1`,
			agID); err != nil {
			t.Fatalf("seed the on-call snapshot: %v", err)
		}

		adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
		adm.OnCallSnapshot = nil
		mustSubmit(t, s, adm)

		if got := storedOnCall(t, s, agID); got != "earlier" {
			t.Fatalf("the admission blanked an answer that was already there: %q", got)
		}
	})
}

// A claim that already exists answers the repeat, whatever the world has done
// since.
//
// A producer retrying after a lost reply is asking one question: "was my work
// accepted?" The answer was written minutes ago and cannot change. Every other
// rule in an admission is about taking on NEW work - is the group still
// waiting, is the plan still current, is the deadline still ahead - and running
// those first turns a lost reply into a refusal over commitments that exist and
// are being delivered right now (D2/D3).

// expiringCommitment is a promise with a deadline, so the repeat can be made
// after the deadline has passed. The bytes are identical on both submits, which
// is what makes the second one a repeat rather than different work.
func expiringCommitment(ref string, at time.Time) keys.EscalationCommitment {
	c := channelCommitment(ref, 0)
	c.Expiry = &keys.TimingSpec{Kind: keys.TimingAbsolute, At: at}
	return c
}

// waitPastTheDeadline blocks until the DATABASE says the deadline has passed.
// The admission compares against the database's clock, so waiting on this one
// is what makes the test independent of the two agreeing.
func waitPastTheDeadline(t *testing.T, s *Store, deadline time.Time) {
	t.Helper()
	for i := 0; i < 100; i++ {
		var past bool
		if err := s.db.QueryRow(`SELECT now() > $1`, deadline).Scan(&past); err != nil {
			t.Fatalf("ask the database for the time: %v", err)
		}
		if past {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the deadline never passed")
}

func TestSubmitRepeatSurvivesTheWorldMovingOn(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// Each case gets its own deadline: one that passes during the test, or one
	// that never does. Shared, the case that waits out its deadline would leave
	// the others unable to make their FIRST admission.
	moveOn := []struct {
		name    string
		expires time.Duration
		moveOn  func(t *testing.T, agID string, deadline time.Time)
	}{
		{
			name:    "the deadline passed",
			expires: 200 * time.Millisecond,
			moveOn: func(t *testing.T, agID string, deadline time.Time) {
				waitPastTheDeadline(t, s, deadline)
			},
		},
		{
			name:    "an alert joined the group",
			expires: time.Hour,
			moveOn: func(t *testing.T, agID string, deadline time.Time) {
				recordAlerts(t, s, agID, []model.Alert{{
					Fingerprint: "fp-late", Status: "firing", StartsAt: time.Now(),
				}})
			},
		},
		{
			name:    "the user acknowledged",
			expires: time.Hour,
			moveOn: func(t *testing.T, agID string, deadline time.Time) {
				if _, err := s.db.ExecContext(ctx,
					`UPDATE alert_groups SET status = $1 WHERE id = $2`,
					model.AlertGroupStatusAcknowledged, agID); err != nil {
					t.Fatalf("acknowledge the group: %v", err)
				}
			},
		},
	}

	for _, move := range moveOn {
		t.Run(move.name, func(t *testing.T) {
			// The deadline is taken from the database, because that is the
			// clock the admission checks it against.
			var dbNow time.Time
			if err := s.db.QueryRow(`SELECT now()`).Scan(&dbNow); err != nil {
				t.Fatalf("ask the database for the time: %v", err)
			}
			deadline := dbNow.Add(move.expires)

			agID := outboundGroup(t, s)
			adm := outboundAdmission(t, agID, "first", expiringCommitment("C0001", deadline))

			first := mustSubmit(t, s, adm)
			if first.Outcome != outbound.SubmitCreated {
				t.Fatalf("the first admission answered %q", first.Outcome)
			}

			move.moveOn(t, agID, deadline)

			repeat := mustSubmit(t, s, adm)
			if repeat.Outcome != outbound.SubmitExisting {
				t.Fatalf("a repeat answered %q, so a lost reply became a refusal over "+
					"commitments that already exist", repeat.Outcome)
			}
			if repeat.BatchID != first.BatchID {
				t.Errorf("the repeat found claim %s, the work is under %s",
					repeat.BatchID, first.BatchID)
			}
			if len(repeat.IntentIDs) != len(first.IntentIDs) {
				t.Fatalf("the repeat named %d commitments, the claim holds %d",
					len(repeat.IntentIDs), len(first.IntentIDs))
			}
			for i := range first.IntentIDs {
				if repeat.IntentIDs[i] != first.IntentIDs[i] {
					t.Errorf("the repeat named commitment %s, the claim holds %s",
						repeat.IntentIDs[i], first.IntentIDs[i])
				}
			}

			// And nothing was created a second time.
			var intents int
			if err := s.db.QueryRow(
				`SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1`,
				agID).Scan(&intents); err != nil {
				t.Fatalf("count the commitments: %v", err)
			}
			if intents != len(first.IntentIDs) {
				t.Fatalf("the group holds %d commitments after a repeat, want %d",
					intents, len(first.IntentIDs))
			}
		})
	}
}

// TestSubmitAnswersDifferentWorkAsAConflictWhateverHasMoved. The same door, the
// other answer: a claim held by DIFFERENT work is a conflict, and it stays a
// conflict after the group has been acknowledged. Refusing it as "not
// admissible" instead would tell the producer to fix its plan, when what
// actually happened is that somebody else's plan is already running.
func TestSubmitAnswersDifferentWorkAsAConflictWhateverHasMoved(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	agID := outboundGroup(t, s)

	winner := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
	first := mustSubmit(t, s, winner)

	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_groups SET status = $1, render_source_version = 9 WHERE id = $2`,
		model.AlertGroupStatusAcknowledged, agID); err != nil {
		t.Fatalf("acknowledge the group: %v", err)
	}

	loser := outboundAdmission(t, agID, "second", channelCommitment("C0009", 0))
	result := mustSubmit(t, s, loser)
	if result.Outcome != outbound.SubmitConflict {
		t.Fatalf("different work under a held claim answered %q", result.Outcome)
	}
	if result.BatchID != first.BatchID {
		t.Errorf("the conflict names claim %s, the work is under %s",
			result.BatchID, first.BatchID)
	}
}

// TestSubmitRefusesAClaimThatIsNotAboutThisAdmission. The batch key carries the
// alert, the kind of claim and the grammar that built it, so a stored row under
// that key naming any of the three differently is not an answer about this
// admission at all. Read as one, a repeat would be told its work exists on the
// strength of somebody else's - and a conflict would be reported over an
// escalation belonging to another alert.
func TestSubmitRefusesAClaimThatIsNotAboutThisAdmission(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	spoil := map[string]func(t *testing.T, agID string){
		"held for another alert group": func(t *testing.T, agID string) {
			exec(t, s, `UPDATE outbound_batches SET alert_group_id = $2 WHERE alert_group_id = $1`,
				agID, outboundGroup(t, s))
		},
		"held as another kind of claim": func(t *testing.T, agID string) {
			exec(t, s, `UPDATE outbound_batches SET key_kind = 'escalation_replay' WHERE alert_group_id = $1`,
				agID)
		},
		"keyed under another grammar": func(t *testing.T, agID string) {
			exec(t, s, `UPDATE outbound_batches SET grammar_version = 99 WHERE alert_group_id = $1`,
				agID)
		},
	}

	for name, damage := range spoil {
		t.Run(name, func(t *testing.T) {
			agID := outboundGroup(t, s)
			adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
			mustSubmit(t, s, adm)

			damage(t, agID)

			_, err := s.SubmitEscalationBatch(ctx, adm)
			if !errors.Is(err, ErrOutboundContract) {
				t.Fatalf("a claim that is not about this admission answered %v", err)
			}
		})
	}
}

func exec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("run %s: %v", query, err)
	}
}
