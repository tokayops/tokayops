package store

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// A fault in the middle of a transition, injected where the transition cannot
// see it coming, and asserted through the doors production actually uses.

// damageTheAlerts makes the group's state impossible to canonicalise: an alert
// in a state the protocol has no word for. Nothing repairs it on the way past -
// a merge keys alerts by fingerprint and carries this one through unchanged -
// so the fault arrives where it is meant to, after the transition has written
// its own row and before the revision it owes.
//
// A duplicated fingerprint is the other unfreezable state and would not do
// here: merging rebuilds the set from a map keyed by fingerprint, so the door
// that most needs testing would quietly repair the damage on its way in.
func damageTheAlerts(t *testing.T, s *Store, agID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE alert_groups SET alerts_data = $2 WHERE id = $1`, agID,
		`[{"fingerprint":"fp-1","status":"smouldering","startsAt":"2023-11-14T22:13:20Z",`+
			`"labels":{"alertname":"A"}}]`); err != nil {
		t.Fatalf("damage the alerts: %v", err)
	}
}

// TestADoorThatCannotRaiseItsRevisionDoesNotOpen.
//
// The command refusing is proven elsewhere. What is proven here is that the
// doors do not swallow it: an acknowledgement that could not tell the cards
// about itself must not be an acknowledgement, because the cards would then say
// "triggered" for as long as the alert lives and nothing would ever raise a
// revision to correct them - the group is already in its new status, so no
// later transition has anything to change.
//
// Asserted through AckAlertGroupAtomic, ResolveAlertGroupAtomic and
// ApplyAlertmanagerUpdateAtomic rather than the command inside them: a door
// that logged the failure and committed anyway would pass every test the
// command has.
func TestADoorThatCannotRaiseItsRevisionDoesNotOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, s *Store, agID string) error
	}{
		{
			name: "an acknowledgement",
			open: func(t *testing.T, s *Store, agID string) error {
				_, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
				return err
			},
		},
		{
			name: "a resolution",
			open: func(t *testing.T, s *Store, agID string) error {
				_, err := s.ResolveAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
				return err
			},
		},
		{
			name: "an alert that arrived",
			open: func(t *testing.T, s *Store, agID string) error {
				_, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(),
					"desired-"+agID, []model.Alert{{
						Fingerprint: "fp-9", Status: model.AlertStatusFiring,
						StartsAt: time.Unix(1700000600, 0),
						Labels:   map[string]string{"alertname": "DiskSlow"},
					}}, "ingester")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")

			agID := desiredGroup(t, s, "Disk filling up")
			changeableCard(t, s, agID)
			moveGroup(t, s, agID, model.AlertGroupStatusTriggered)

			was := groupFacts(t, s, agID)
			damageTheAlerts(t, s, agID)

			if err := tc.open(t, s, agID); err == nil {
				t.Fatal("the transition was made without the revision it owes")
			}

			now := groupFacts(t, s, agID)
			if now.Status != model.AlertGroupStatusTriggered {
				t.Errorf("the group moved to %s anyway", now.Status)
			}
			if now.Revision != was.Revision {
				t.Errorf("a revision %d was raised over a state nobody can render",
					now.Revision)
			}
			if now.Desired != was.Desired {
				t.Errorf("the card is aimed at revision %d, and no such state exists",
					now.Desired)
			}
			if now.Timeline != was.Timeline {
				t.Errorf("the history kept %d line(s) about a transition that did not happen",
					now.Timeline-was.Timeline)
			}
			if now.Outbox != was.Outbox {
				t.Errorf("%d webhook event(s) went out about a transition that did not happen",
					now.Outbox-was.Outbox)
			}
			if now.Owed != was.Owed {
				t.Errorf("the commitments changed: %d owed, was %d", now.Owed, was.Owed)
			}
		})
	}
}

// refuseTheCommit installs a deferred constraint trigger that fires when the
// transaction COMMITS, not when it writes.
//
// This is the fault the earlier one cannot reach. Damaging the state makes the
// command refuse, which proves the transition rolls back around a refusal; it
// says nothing about the window after the command has succeeded, where the
// snapshot, the revisions and the journal are all written and the commit is
// still to come. A door that treated a failed Commit as "well, it mostly
// worked" would leave a caller believing an alert was acknowledged while the
// database says it never was.
//
// It hangs off outbound_group_snapshots because every one of these transitions
// writes that row, which is the point: the fault is in the commit, not in any
// particular door.
func refuseTheCommit(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`CREATE OR REPLACE FUNCTION test_refuse_commit() RETURNS trigger
		 LANGUAGE plpgsql AS $$ BEGIN
		     RAISE EXCEPTION 'the commit was refused by the test';
		 END $$`,
		`CREATE CONSTRAINT TRIGGER test_refuse_commit
		 AFTER INSERT OR UPDATE ON outbound_group_snapshots
		 DEFERRABLE INITIALLY DEFERRED
		 FOR EACH ROW EXECUTE FUNCTION test_refuse_commit()`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("install the commit fault: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS test_refuse_commit ON outbound_group_snapshots`,
			`DROP FUNCTION IF EXISTS test_refuse_commit()`,
		} {
			if _, err := s.db.Exec(statement); err != nil {
				t.Fatalf("remove the commit fault: %v", err)
			}
		}
	})
}

// TestTheCommitFaultLandsWhereItIsMeantTo. The test above is only worth
// anything if the refusal happens at the commit rather than at the write - a
// trigger that fired early would make it a copy of the test before it.
func TestTheCommitFaultLandsWhereItIsMeantTo(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, agID)
	moveGroup(t, s, agID, model.AlertGroupStatusAcknowledged)
	refuseTheCommit(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	result, err := setDesiredStateTx(context.Background(), tx, s.render,
		outbound.DesiredStateRequest{
			AlertGroupID: agID, Reason: outbound.DesiredAck, Actor: byUser("nina"),
		})
	if err != nil || result.Outcome != outbound.DesiredApplied {
		t.Fatalf("the raise was refused before the commit: %s (%v)", result.Outcome, err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("the commit was accepted, so the fault is not where this test needs it")
	}
}

// TestADoorWhoseCommitFailsReportsIt. The whole transition is written and then
// the commit is refused.
//
// Every door has to come back with an error. The alternative is the worst
// failure this design has: the caller is told the alert was acknowledged, the
// webhook says so, and the database has none of it - so no later transition
// has anything to change and the cards say "triggered" until the alert ends.
func TestADoorWhoseCommitFailsReportsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, s *Store, agID string) error
	}{
		{
			name: "an acknowledgement",
			open: func(t *testing.T, s *Store, agID string) error {
				_, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
				return err
			},
		},
		{
			name: "a resolution",
			open: func(t *testing.T, s *Store, agID string) error {
				_, err := s.ResolveAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
				return err
			},
		},
		{
			name: "an alert that arrived",
			open: func(t *testing.T, s *Store, agID string) error {
				_, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(),
					"desired-"+agID, []model.Alert{{
						Fingerprint: "fp-9", Status: model.AlertStatusFiring,
						StartsAt: time.Unix(1700000600, 0),
						Labels:   map[string]string{"alertname": "DiskSlow"},
					}}, "ingester")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")

			agID := desiredGroup(t, s, "Disk filling up")
			changeableCard(t, s, agID)
			moveGroup(t, s, agID, model.AlertGroupStatusTriggered)

			was := groupFacts(t, s, agID)
			refuseTheCommit(t, s)

			if err := tc.open(t, s, agID); err == nil {
				t.Fatal("a transition whose commit was refused came back as done")
			}

			now := groupFacts(t, s, agID)
			if now != was {
				t.Errorf("the database kept part of a transition that never committed:\n"+
					"  was %+v\n  now %+v", was, now)
			}
		})
	}
}

// TestAnAcknowledgementDuringTheFirstSendEndsAcknowledgedEitherWay.
//
// The acknowledgement arrives while the card is being posted for the first
// time, which is the one moment where nobody can say yet whether there is a
// card at all. Both ways it can land have to end with nothing showing
// "triggered": either the message exists and is brought to the acknowledged
// revision, or it never existed and the commitment is withdrawn.
//
// The card being posted is what makes this different from a page. A page that
// went out during an acknowledgement is simply a page somebody got; a card is
// a thing that stays on the screen saying the wrong state.
func TestAnAcknowledgementDuringTheFirstSendEndsAcknowledgedEitherWay(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lands   bool
		settles outbound.Status
	}{
		{name: "the card landed", lands: true, settles: outbound.StatusPending},
		{name: "the card never landed", lands: false, settles: outbound.StatusCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")

			agID := desiredGroup(t, s, "Disk filling up")
			intentID := admitOne(t, s, agID, channelCommitment("C0001", 0))[0]
			token := claimOne(t, s, intentID)
			begun := beginOne(t, s, intentID, token)

			// The call is out. Nobody here knows what it did.
			moveGroup(t, s, agID, model.AlertGroupStatusTriggered)
			if _, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil); err != nil {
				t.Fatalf("acknowledge: %v", err)
			}
			if got := statusOf(t, s, intentID); got != outbound.StatusSending {
				t.Fatalf("a send in flight was interrupted rather than flagged: %s", got)
			}

			end := concluded(outbound.OutcomeRetryableRejection, "rate_limited")
			if tc.lands {
				end = conclusion(outbound.ConclusionInput{
					Outcome: outbound.OutcomeAccepted, Status: "ok",
					Receipt: receiptOf("C0001/1700000000.000100",
						`{"channel_id":"C0001","timestamp":"1700000000.000100"}`),
					Summary: "the card was posted",
				})
			}
			if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
				AttemptID: begun.AttemptID, LeaseToken: token, Conclusion: end,
			}); err != nil {
				t.Fatalf("finalize the send: %v", err)
			}

			card := readCard(t, s, intentID)
			if card.Status != tc.settles {
				t.Fatalf("the card settled as %s, want %s", card.Status, tc.settles)
			}
			if !tc.lands {
				// Nothing exists, so there is nothing left showing anything.
				return
			}
			// It exists, and it is owed the acknowledged revision. A card that
			// settled here would say "triggered" until the next alert.
			if card.Applied >= card.Desired {
				t.Fatalf("the card is finished at revision %d of %d and shows the "+
					"state before the acknowledgement", card.Applied, card.Desired)
			}

			// And one more attempt brings it there.
			due(t, s, intentID)
			next := claimOne(t, s, intentID)
			change := beginOne(t, s, intentID, next)
			if change.AttemptKind != outbound.AttemptMutation {
				t.Fatalf("the follow-up was planned as %s", change.AttemptKind)
			}
			if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
				AttemptID: change.AttemptID, LeaseToken: next,
				Conclusion: mutationAccepted(t, change.ReceiptRef, outbound.Receipt{}),
			}); err != nil {
				t.Fatalf("apply the acknowledgement: %v", err)
			}
			if done := readCard(t, s, intentID); done.Status != outbound.StatusIdle ||
				done.Applied != done.Desired {
				t.Fatalf("the card is %s at revision %d of %d",
					done.Status, done.Applied, done.Desired)
			}
			if shown := storedStatus(t, s, agID); shown != keys.GroupAcknowledged {
				t.Fatalf("the card shows %s", shown)
			}
		})
	}
}

// groupFacts is everything one of these transitions writes, in one read.
type facts struct {
	Status   model.AlertGroupStatus
	Revision int64
	Desired  int64
	Timeline int
	Outbox   int
	Owed     int
}

func groupFacts(t *testing.T, s *Store, agID string) facts {
	t.Helper()
	var out facts
	var status string
	if err := s.db.QueryRow(`
		SELECT ag.status,
		       COALESCE((SELECT revision FROM outbound_group_snapshots WHERE alert_group_id = ag.id), -1),
		       COALESCE((SELECT max(desired_revision) FROM outbound_intents WHERE alert_group_id = ag.id), -1),
		       (SELECT count(*) FROM timeline_events WHERE alert_group_id = ag.id),
		       (SELECT count(*) FROM event_outbox WHERE alert_group_id = ag.id),
		       (SELECT count(*) FROM outbound_intents
		         WHERE alert_group_id = ag.id AND status IN ('pending', 'sending'))
		FROM alert_groups ag WHERE ag.id = $1`, agID).
		Scan(&status, &out.Revision, &out.Desired, &out.Timeline, &out.Outbox, &out.Owed); err != nil {
		t.Fatalf("read what the group has: %v", err)
	}
	out.Status = model.AlertGroupStatus(status)
	return out
}
