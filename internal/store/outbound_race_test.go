package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The lock-order rule under real concurrency, and what that can and cannot
// prove.
//
// A concurrent test shows that two transactions which write to the same alert
// cannot deadlock or wait each other out - that is the rule, and it can only be
// observed by running them at once. What it CANNOT show is what each order
// produces: nothing here decides which of the two reaches the alert first, and
// a test that asserted one particular ending would be asserting the scheduling
// of the machine it ran on. Twice in this file's history it did exactly that
// and passed for the wrong reason.
//
// So the two questions are asked separately. These tests race, and accept
// either legal ending. The ...InEitherOrder tests below settle the order by
// letting the first transaction commit before the second begins, and assert
// exactly what that order has to produce.
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

// raceLockTimeout is what these tests set the store's lock bound to.
//
// The production bound is a few seconds, which is right for production and
// useless here: a machine with something else on it makes an ordinary
// acknowledgement take longer than that, and the timeout that follows says
// nothing about who took which row first - the question these tests exist to
// ask. Raised to a minute, a timeout means something held an alert group for a
// minute, and that is worth failing over whatever caused it.
const raceLockTimeout = 60 * time.Second

// measuringLockOrder raises the bound for one test and puts it back.
func measuringLockOrder(t *testing.T, s *Store) {
	t.Helper()
	previous := s.lockTimeout
	s.lockTimeout = raceLockTimeout
	t.Cleanup(func() { s.lockTimeout = previous })
}

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
	ackTook   time.Duration
	otherErr  error
	otherTook time.Duration
}

// timed runs one side of a pair and remembers how long it took. The duration is
// not decoration: it is what tells a lock timeout caused by the lock order from
// one caused by a machine that cannot commit anything in time.
func timed(took *time.Duration, work func()) {
	started := time.Now()
	work()
	*took = time.Since(started)
}

// slowest is how long the slower half of this pair took end to end.
func (r *raceRound) slowest() time.Duration {
	if r.ackTook > r.otherTook {
		return r.ackTook
	}
	return r.otherTook
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
			// All three are failures, and none of them is excusable at the
			// bound these tests run under. A deadlock is a cycle, which no
			// amount of load invents. A timeout means somebody held the alert
			// group for a minute, which nothing here legitimately does.
			//
			// The duration is reported because it is the first thing to look
			// at: a pair that took most of that minute says the machine was
			// the problem, and one that took milliseconds says the lock order
			// was.
			t.Fatalf("the acknowledgement and %s met in %s; the pair took %s: %v",
				other, kind, r.slowest(), e.err)
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
	measuringLockOrder(t, s)

	for i := 0; i < raceRounds; i++ {
		round := inFlight(t, s, dmCommitment(fmt.Sprintf("U%04d", i)))

		startTogether(
			func() {
				timed(&round.ackTook, func() {
					_, round.ackErr = s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil)
				})
			},
			func() {
				timed(&round.otherTook, func() {
					_, round.otherErr = s.FinalizeDeliveryAttempt(context.Background(),
						outbound.FinalizeRequest{
							AttemptID: round.attemptID, LeaseToken: round.token,
							Conclusion: accepted(),
						})
				})
			})
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
	measuringLockOrder(t, s)

	for i := 0; i < raceRounds; i++ {
		commitment := dmCommitment(fmt.Sprintf("U%04d", i))
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		round := inFlight(t, s, commitment)

		startTogether(
			func() {
				timed(&round.ackTook, func() {
					_, round.ackErr = s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil)
				})
			},
			func() {
				timed(&round.otherTook, func() {
					_, round.otherErr = s.FinalizeDeliveryAttempt(context.Background(),
						outbound.FinalizeRequest{
							AttemptID: round.attemptID, LeaseToken: round.token,
							Conclusion: concluded(outbound.OutcomeAmbiguous, "no_response"),
						})
				})
			})
		round.check(t, "the doubtful result")
		assertAssumedOrWithdrawn(t, s, round)
	}
}

// TestAcknowledgementRacesRecovery is G15: the same fork, decided by the
// process that cleans up after a worker that never came back. Recovery has
// nobody waiting on it, so a deadlock here would be found later, in the shape
// of an alert that says nothing happened.
//
// Two recoverers on purpose: they also race each other for the same candidate,
// which is the other half of the rule (G18).
func TestAcknowledgementRacesRecovery(t *testing.T) {
	s := setupTestDB(t)
	measuringLockOrder(t, s)

	for i := 0; i < raceRounds; i++ {
		commitment := dmCommitment(fmt.Sprintf("U%04d", i))
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		round := inFlight(t, s, commitment)
		expireLease(t, s, round.intentID)

		recoveries := make([]raceRound, 2)
		startTogether(
			func() {
				timed(&round.ackTook, func() {
					_, round.ackErr = s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil)
				})
			},
			// The two recoverers start together with each other as well: their
			// race for the same candidate is the other half of the rule.
			func() {
				startTogether(
					func() {
						timed(&recoveries[0].otherTook, func() {
							_, recoveries[0].otherErr = s.RecoverStaleAttempts(
								context.Background(), testFamily, raceRounds)
						})
					},
					func() {
						timed(&recoveries[1].otherTook, func() {
							_, recoveries[1].otherErr = s.RecoverStaleAttempts(
								context.Background(), testFamily, raceRounds)
						})
					})
			})

		round.check(t, "recovery")
		for j := range recoveries {
			recoveries[j].check(t, fmt.Sprintf("recovery %d", j))
		}
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

// TestTheJournalIsOneInstant. The journal is what somebody reads when they are
// trying to establish what really happened, so it must never show a state the
// system was never in: a commitment still sending next to the attempt that
// already finished it. Four separate reads would show exactly that, at some
// rate nobody can predict, and only under load.
func TestTheJournalIsOneInstant(t *testing.T) {
	s := setupTestDB(t)
	measuringLockOrder(t, s)

	for i := 0; i < raceRounds; i++ {
		round := inFlight(t, s, dmCommitment(fmt.Sprintf("U%04d", i)))
		torn := ""

		startTogether(
			func() {
				timed(&round.otherTook, func() {
					_, round.otherErr = s.FinalizeDeliveryAttempt(context.Background(),
						outbound.FinalizeRequest{
							AttemptID: round.attemptID, LeaseToken: round.token,
							Conclusion: accepted(),
						})
				})
			},
			func() {
				// Read until the delivery has landed, and check every answer
				// on the way: the two halves have to move together.
				for attempt := 0; attempt < journalReads; attempt++ {
					journal, err := s.IntentJournal(context.Background(), round.intentID)
					if err != nil {
						round.ackErr = err
						return
					}
					if len(journal.Attempts) == 0 {
						continue
					}
					finished := journal.Attempts[len(journal.Attempts)-1].FinishedAt != nil
					sending := journal.Intent.Status == outbound.StatusSending
					if finished && sending {
						torn = "the attempt is closed and the commitment is still sending"
						return
					}
					if !finished && !sending {
						torn = "the commitment moved on while its attempt is still open"
						return
					}
					if finished {
						return
					}
				}
			})

		round.check(t, "the delivery")
		if torn != "" {
			t.Fatalf("the journal answered from two different moments: %s", torn)
		}
	}
}

// journalReads bounds the polling above. It is a loop against another
// transaction, not a load test: enough attempts to catch the commit in the
// middle, few enough that a slow machine is not asked to serve thousands of
// them.
const journalReads = 200

// TestAnAcknowledgementAndADeliveryInEitherOrder is G10 with the order decided.
// The message went out either way; what differs is what the alert's history is
// allowed to say about it.
func TestAnAcknowledgementAndADeliveryInEitherOrder(t *testing.T) {
	s := setupTestDB(t)

	t.Run("the acknowledgement lands first", func(t *testing.T) {
		round := inFlight(t, s, dmCommitment("U0001"))

		if _, err := s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		result, err := s.FinalizeDeliveryAttempt(context.Background(),
			outbound.FinalizeRequest{
				AttemptID: round.attemptID, LeaseToken: round.token,
				Conclusion: accepted(),
			})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}

		if result.To != outbound.StatusSucceeded {
			t.Fatalf("a message that really went out became %s", result.To)
		}
		if got := groupStatusOf(t, s, round.groupID); got != model.AlertGroupStatusAcknowledged {
			t.Fatalf("the alert is %s after a send that arrived behind the ack", got)
		}
		// The wording is the honest one for this order: it went out at the
		// moment somebody was already handling the alert.
		oneOf(t, timelineMessages(t, s, round.groupID, model.TimelineEventNotificationSent),
			"Notification went out at the same moment the alert was acknowledged")
	})

	t.Run("the delivery lands first", func(t *testing.T) {
		round := inFlight(t, s, dmCommitment("U0002"))

		result, err := s.FinalizeDeliveryAttempt(context.Background(),
			outbound.FinalizeRequest{
				AttemptID: round.attemptID, LeaseToken: round.token,
				Conclusion: accepted(),
			})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.To != outbound.StatusSucceeded {
			t.Fatalf("the delivery became %s", result.To)
		}
		// The delivery moved the alert out of processing, which is the whole
		// point of paging somebody.
		if got := groupStatusOf(t, s, round.groupID); got != model.AlertGroupStatusTriggered {
			t.Fatalf("the alert is %s after its notification landed", got)
		}

		if _, err := s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := groupStatusOf(t, s, round.groupID); got != model.AlertGroupStatusAcknowledged {
			t.Fatalf("the alert is %s after being acknowledged", got)
		}
		oneOf(t, timelineMessages(t, s, round.groupID, model.TimelineEventNotificationSent),
			"Notification sent")
	})
}

// TestAnAcknowledgementAndADoubtfulResultInEitherOrder is G16 with the order
// decided. This is the fork the domain exists to make explicit, so both sides
// of it are asserted exactly rather than as "one of these two".
func TestAnAcknowledgementAndADoubtfulResultInEitherOrder(t *testing.T) {
	s := setupTestDB(t)

	assumeAccepted := func(ref string) keys.EscalationCommitment {
		commitment := dmCommitment(ref)
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		return commitment
	}

	t.Run("the acknowledgement lands first", func(t *testing.T) {
		round := inFlight(t, s, assumeAccepted("U0001"))

		if _, err := s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		result, err := s.FinalizeDeliveryAttempt(context.Background(),
			outbound.FinalizeRequest{
				AttemptID: round.attemptID, LeaseToken: round.token,
				Conclusion: concluded(outbound.OutcomeAmbiguous, "no_response"),
			})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}

		// Nothing is assumed about a message nobody is waiting for any more.
		if result.To != outbound.StatusCanceled {
			t.Fatalf("a doubtful result after an acknowledgement became %s", result.To)
		}
		if sent := timelineMessages(t, s, round.groupID,
			model.TimelineEventNotificationSent); len(sent) != 0 {
			t.Fatalf("the alert claims %v about a message nobody could confirm", sent)
		}
		if !containsString(timelineMessages(t, s, round.groupID,
			model.TimelineEventNotificationFailed), "A notification was withdrawn") {
			t.Fatal("the withdrawal is missing from the alert")
		}
		// The attempt keeps the doubt: it may have arrived.
		var outcome string
		if err := s.db.QueryRow(`SELECT outcome FROM outbound_attempts WHERE id = $1`,
			round.attemptID).Scan(&outcome); err != nil {
			t.Fatalf("read the attempt: %v", err)
		}
		if outcome != string(outbound.OutcomeAmbiguous) {
			t.Fatalf("the withdrawn attempt reads as %q", outcome)
		}
	})

	t.Run("the doubtful result lands first", func(t *testing.T) {
		round := inFlight(t, s, assumeAccepted("U0002"))

		result, err := s.FinalizeDeliveryAttempt(context.Background(),
			outbound.FinalizeRequest{
				AttemptID: round.attemptID, LeaseToken: round.token,
				Conclusion: concluded(outbound.OutcomeAmbiguous, "no_response"),
			})
		if err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if result.To != outbound.StatusSucceeded {
			t.Fatalf("assume_accepted produced %s", result.To)
		}

		// The assumption is visible as an assumption, and the risk is on the
		// record.
		oneOf(t, timelineMessages(t, s, round.groupID, model.TimelineEventNotificationSent),
			"Notification assumed delivered: the provider never confirmed, "+
				"and the risk was accepted")
		var risk bool
		if err := s.db.QueryRow(
			`SELECT accepted_duplicate_risk FROM outbound_intents WHERE id = $1`,
			round.intentID).Scan(&risk); err != nil {
			t.Fatalf("read the risk flag: %v", err)
		}
		if !risk {
			t.Fatal("a delivery nobody confirmed is recorded as if it were confirmed")
		}

		if _, err := s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := statusOf(t, s, round.intentID); got != outbound.StatusSucceeded {
			t.Fatalf("the acknowledgement changed a settled commitment to %s", got)
		}
	})
}

// TestAnAcknowledgementAndRecoveryInEitherOrder is G15 with the order decided.
func TestAnAcknowledgementAndRecoveryInEitherOrder(t *testing.T) {
	s := setupTestDB(t)

	stale := func(ref string) *raceRound {
		commitment := dmCommitment(ref)
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		round := inFlight(t, s, commitment)
		expireLease(t, s, round.intentID)
		return round
	}

	t.Run("the acknowledgement lands first", func(t *testing.T) {
		round := stale("U0001")

		if _, err := s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(recovered) != 1 || recovered[0].To != outbound.StatusCanceled {
			t.Fatalf("recovery after an acknowledgement produced %+v", recovered)
		}
		if got := statusOf(t, s, round.intentID); got != outbound.StatusCanceled {
			t.Fatalf("the commitment is %s", got)
		}
	})

	t.Run("recovery lands first", func(t *testing.T) {
		round := stale("U0002")

		recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(recovered) != 1 || recovered[0].To != outbound.StatusSucceeded {
			t.Fatalf("recovery under assume_accepted produced %+v", recovered)
		}
		if got := groupStatusOf(t, s, round.groupID); got != model.AlertGroupStatusTriggered {
			t.Fatalf("the alert is %s after its only notification was assumed delivered", got)
		}

		if _, err := s.AckAlertGroupAtomic(round.groupID, "nina", nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if got := statusOf(t, s, round.intentID); got != outbound.StatusSucceeded {
			t.Fatalf("the acknowledgement changed a settled commitment to %s", got)
		}
	})
}
