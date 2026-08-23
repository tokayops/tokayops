package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The lock-order rule under real concurrency.
//
// Acknowledgement and delivery both write to an alert group and to the
// commitments under it. If one of them took the group first and the other took
// a commitment first, the two would eventually meet head-on and Postgres would
// break the tie by killing one of them - at the exact moment somebody is being
// paged. The rule is that ANY unit of work that could end up writing to the
// group takes the group first, even when it will turn out not to need it.
//
// Sequential tests cannot show this. They run one order, then the other, and
// both pass whatever the locking does. These start both sides at the same
// instant, many times over, and fail on any deadlock, lock timeout or
// serialisation failure - which is the only way the rule can be observed at
// all.

const raceRounds = 12

// lockFailure names the way concurrency broke, or returns "" if the error is
// about something else.
func lockFailure(err error) string {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40P01":
			return "a deadlock"
		case "55P03":
			return "a lock timeout"
		case "40001":
			return "a serialisation failure"
		}
	}
	return ""
}

// raceRound is one alert with one commitment mid-flight, plus whatever the two
// concurrent transactions made of it.
type raceRound struct {
	groupID   string
	intentID  string
	attemptID string
	token     string
	ackErr    error
	otherErr  error
}

func (r *raceRound) check(t *testing.T, other string) {
	t.Helper()
	for _, e := range []struct {
		who string
		err error
	}{{"the acknowledgement", r.ackErr}, {other, r.otherErr}} {
		if e.err == nil {
			continue
		}
		if kind := lockFailure(e.err); kind != "" {
			t.Fatalf("%s and %s met in %s: %v", "the acknowledgement", other, kind, e.err)
		}
		t.Fatalf("%s failed: %v", e.who, e.err)
	}
}

// inFlight prepares one alert with a commitment whose attempt is open.
func inFlight(t *testing.T, s *Store, commitment keys.EscalationCommitment) *raceRound {
	t.Helper()
	round := &raceRound{groupID: outboundGroup(t, s)}
	round.intentID = admitOne(t, s, round.groupID, commitment)[0]
	round.token = claimOne(t, s, round.intentID)
	round.attemptID = beginOne(t, s, round.intentID, round.token).AttemptID
	return round
}

// timelineMessages is what the alert ended up telling whoever reads it.
func timelineMessages(t *testing.T, s *Store, groupID string,
	eventType model.TimelineEventType) []string {

	t.Helper()
	rows, err := s.db.Query(
		`SELECT message FROM timeline_events WHERE alert_group_id = $1 AND type = $2
		 ORDER BY created_at`, groupID, eventType)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	defer rows.Close()

	var messages []string
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			t.Fatalf("read the history: %v", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the history: %v", err)
	}
	return messages
}

func oneOf(t *testing.T, got []string, want ...string) string {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("the alert's history holds %d lines about this notification: %v", len(got), got)
	}
	for _, w := range want {
		if got[0] == w {
			return got[0]
		}
	}
	t.Fatalf("the alert says %q, which is neither of the two things that could have happened", got[0])
	return ""
}

// startTogether runs every piece of work at the same instant and waits for all
// of them.
func startTogether(work ...func()) {
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for _, w := range work {
		wg.Add(1)
		go func(w func()) {
			defer wg.Done()
			<-start
			w()
		}(w)
	}
	close(start)
	wg.Wait()
}

// TestAcknowledgementRacesADeliveryThatLanded is G10: the acknowledgement and a
// successful send reach the same alert together. The delivery happened either
// way - what the two orders decide is only what the alert's history says about
// it.
func TestAcknowledgementRacesADeliveryThatLanded(t *testing.T) {
	s := setupTestDB(t)

	rounds := make([]*raceRound, raceRounds)
	for i := range rounds {
		rounds[i] = inFlight(t, s, dmCommitment(fmt.Sprintf("U%04d", i)))
	}

	var work []func()
	for _, round := range rounds {
		round := round
		work = append(work,
			func() {
				_, round.ackErr = s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil)
			},
			func() {
				_, round.otherErr = s.FinalizeDeliveryAttempt(context.Background(),
					outbound.FinalizeRequest{
						AttemptID: round.attemptID, LeaseToken: round.token,
						Completion: acceptedCompletion(),
						Receipt:    json.RawMessage(`{"channel":"C0001","ts":"1700000000.000100"}`),
					})
			})
	}
	startTogether(work...)

	for _, round := range rounds {
		round.check(t, "the delivery")

		// The message went out, so the commitment is done whichever
		// transaction committed first.
		if got := statusOf(t, s, round.intentID); got != outbound.StatusSucceeded {
			t.Fatalf("a message that really went out ended as %s", got)
		}
		// And the acknowledgement stands: a delivery never un-acknowledges an
		// alert somebody has already taken.
		if got := groupStatusOf(t, s, round.groupID); got != model.AlertGroupStatusAcknowledged {
			t.Fatalf("the alert is %s after an acknowledgement raced a send", got)
		}
		oneOf(t, timelineMessages(t, s, round.groupID, model.TimelineEventNotificationSent),
			"Notification sent",
			"Notification went out at the same moment the alert was acknowledged")
	}
}

// TestAcknowledgementRacesAnAssumedDelivery is G16: the acknowledgement meets a
// result that says "I do not know". The policy is to assume the message
// arrived, which makes this a unit of work that will write to the alert - so it
// takes the alert first, exactly like a success does.
func TestAcknowledgementRacesAnAssumedDelivery(t *testing.T) {
	s := setupTestDB(t)

	rounds := make([]*raceRound, raceRounds)
	for i := range rounds {
		commitment := dmCommitment(fmt.Sprintf("U%04d", i))
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		rounds[i] = inFlight(t, s, commitment)
	}

	var work []func()
	for _, round := range rounds {
		round := round
		work = append(work,
			func() {
				_, round.ackErr = s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil)
			},
			func() {
				_, round.otherErr = s.FinalizeDeliveryAttempt(context.Background(),
					outbound.FinalizeRequest{
						AttemptID: round.attemptID, LeaseToken: round.token,
						Completion: keys.Completion{Outcome: keys.OutcomeAmbiguous},
					})
			})
	}
	startTogether(work...)

	for _, round := range rounds {
		round.check(t, "the doubtful result")
		assertAssumedOrWithdrawn(t, s, round)
	}
}

// TestAcknowledgementRacesRecovery is G15: the same fork, decided by the
// process that cleans up after a worker that never came back. Recovery has
// nobody waiting on it, so a deadlock here would be found later, in the shape
// of an alert that says nothing happened.
func TestAcknowledgementRacesRecovery(t *testing.T) {
	s := setupTestDB(t)

	rounds := make([]*raceRound, raceRounds)
	for i := range rounds {
		commitment := dmCommitment(fmt.Sprintf("U%04d", i))
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		rounds[i] = inFlight(t, s, commitment)
		expireLease(t, s, rounds[i].intentID)
	}

	// Two recoverers on purpose: they also race each other for the same
	// candidates, which is the other half of the rule (G18).
	recoveryErrs := make([]error, 2)
	work := []func(){
		func() {
			_, recoveryErrs[0] = s.RecoverStaleAttempts(context.Background(), testFamily, raceRounds)
		},
		func() {
			_, recoveryErrs[1] = s.RecoverStaleAttempts(context.Background(), testFamily, raceRounds)
		},
	}
	for _, round := range rounds {
		round := round
		work = append(work, func() {
			_, round.ackErr = s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil)
		})
	}
	startTogether(work...)

	for i, err := range recoveryErrs {
		if err == nil {
			continue
		}
		if kind := lockFailure(err); kind != "" {
			t.Fatalf("recovery %d met %s: %v", i, kind, err)
		}
		t.Fatalf("recovery %d failed: %v", i, err)
	}
	for _, round := range rounds {
		round.check(t, "recovery")
		assertAssumedOrWithdrawn(t, s, round)
	}
}

// assertAssumedOrWithdrawn pins the two endings a doubtful delivery can have
// next to an acknowledgement, and insists the alert's history says which one
// happened. Both are legal; a silent one is not.
func assertAssumedOrWithdrawn(t *testing.T, s *Store, round *raceRound) {
	t.Helper()

	if got := groupStatusOf(t, s, round.groupID); got != model.AlertGroupStatusAcknowledged {
		t.Fatalf("the alert is %s after an acknowledgement raced a doubtful delivery", got)
	}

	switch got := statusOf(t, s, round.intentID); got {
	case outbound.StatusSucceeded:
		// The doubt was settled before the withdrawal reached the commitment,
		// so the assumption stands - and has to be visible as an assumption.
		oneOf(t, timelineMessages(t, s, round.groupID, model.TimelineEventNotificationSent),
			"Notification assumed delivered: the provider never confirmed, and the risk was accepted")
		var risk bool
		if err := s.db.QueryRow(
			`SELECT accepted_duplicate_risk FROM outbound_intents WHERE id = $1`,
			round.intentID).Scan(&risk); err != nil {
			t.Fatalf("read the risk flag: %v", err)
		}
		if !risk {
			t.Fatal("a delivery nobody confirmed is recorded as if it were confirmed")
		}

	case outbound.StatusCanceled:
		// The withdrawal got there first. Nothing may be claimed about the
		// message, and the attempt that carried it stays on the record as
		// unknown.
		//
		// The acknowledgement writes its own summary line about the
		// notifications it withdrew, so this one is looked for among them
		// rather than alone - but nothing may claim a send.
		if sent := timelineMessages(t, s, round.groupID,
			model.TimelineEventNotificationSent); len(sent) != 0 {
			t.Fatalf("a withdrawn notification left the alert claiming %v", sent)
		}
		if !containsString(timelineMessages(t, s, round.groupID,
			model.TimelineEventNotificationFailed), "A notification was withdrawn") {
			t.Fatal("the withdrawal of a notification in flight is missing from the alert")
		}
		var outcome string
		if err := s.db.QueryRow(
			`SELECT outcome FROM outbound_attempts WHERE id = $1`, round.attemptID).
			Scan(&outcome); err != nil {
			t.Fatalf("read the attempt: %v", err)
		}
		if outcome != string(outbound.OutcomeAmbiguous) {
			t.Fatalf("the withdrawn attempt reads as %q, hiding that it may have arrived", outcome)
		}

	default:
		t.Fatalf("a doubtful delivery under assume_accepted ended as %s", got)
	}
}
