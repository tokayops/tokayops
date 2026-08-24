package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What erasure has to do about deliveries, and what it must not touch.
//
// The delivery domain keeps two different things about a person, and they are
// not the same kind of data. target_ref is this system's own user id, and the
// history of a page has to stay joinable to the anonymized person it was for.
// bound_endpoint is the provider's address - the same data as the identity row
// erasure already deletes - kept in two more places by the effects that used
// it.
//
// And what is still OWED has to be withdrawn in the same transaction: the
// address is being deleted, so the first attempt afterwards would resolve
// nobody and end as permanently failed, raising the alert that says a page did
// not happen. That alert would be about erasure working.

func dmForUser(userID string, slot int) keys.EscalationCommitment {
	c := dmCommitment(userID)
	c.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: slot}
	return c
}

func endpointOf(t *testing.T, s *Store, intentID string) sql.NullString {
	t.Helper()
	var endpoint sql.NullString
	if err := s.db.QueryRow(
		`SELECT bound_endpoint FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&endpoint); err != nil {
		t.Fatalf("read the endpoint: %v", err)
	}
	return endpoint
}

func TestErasureWithdrawsWhatIsOwedAndForgetsTheAddress(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedTeam(t, s, "devops", "alice", "bob")

	agID := outboundGroup(t, s)
	owed := admitOne(t, s, agID,
		dmForUser("bob", 1), // never tried
		dmForUser("bob", 2), // in flight
		dmForUser("bob", 3), // already delivered
		channelCommitment("C0001", 0),
	)
	// Which of the four is which. The ids come back in key order, so they are
	// told apart by what they are aimed at rather than by position.
	var toBob []string
	var channel string
	for _, id := range owed {
		var kind, ref string
		if err := s.db.QueryRow(
			`SELECT target_kind, target_ref FROM outbound_intents WHERE id = $1`, id).
			Scan(&kind, &ref); err != nil {
			t.Fatalf("read the commitment: %v", err)
		}
		if kind == string(keys.TargetUser) && ref == "bob" {
			toBob = append(toBob, id)
			continue
		}
		channel = id
	}
	if len(toBob) != 3 || channel == "" {
		t.Fatalf("the fixture admitted %d commitments to bob and channel=%q", len(toBob), channel)
	}
	untried, inFlight, delivered := toBob[0], toBob[1], toBob[2]

	// One claim, not two: a claim leases everything it can reach, so asking
	// twice leaves the second call with nothing to find.
	leased, err := s.ClaimDueIntents(ctx, outbound.ClaimRequest{
		Family: outbound.FamilyNotification, Provider: "slack",
		Phase: outbound.ClaimRetriesFirst, Limit: 10,
		Lease: outbound.NotificationLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	tokens := map[string]string{}
	for _, l := range leased {
		tokens[l.Intent.ID] = l.LeaseToken
	}

	// One in flight: begun, and waiting for an answer.
	beginOne(t, s, inFlight, tokens[inFlight])

	// One already delivered, with the coordinates to prove it.
	deliveredToken := tokens[delivered]
	deliveredAttempt := beginOne(t, s, delivered, deliveredToken)
	if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: deliveredAttempt.AttemptID, LeaseToken: deliveredToken,
		Conclusion: acceptedConclusion(t, "U0001"),
	}); err != nil {
		t.Fatalf("finalize the delivered one: %v", err)
	}

	before := terminalCount(t, outbound.FamilyNotification, string(outbound.StatusCanceled))

	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob"); err != nil {
		t.Fatalf("erase: %v", err)
	}

	// Nothing had gone out, so nothing will: withdrawn outright.
	if got := statusOf(t, s, untried); got != outbound.StatusCanceled {
		t.Errorf("an unsent notification to an erased person is %s", got)
	}
	// The lease goes with it, so the worker holding one finds out at its next
	// compare-and-set rather than sending to an address that no longer exists.
	var stillLeased sql.NullString
	if err := s.db.QueryRow(
		`SELECT lease_token FROM outbound_intents WHERE id = $1`, untried).
		Scan(&stillLeased); err != nil {
		t.Fatalf("read the lease: %v", err)
	}
	if stillLeased.Valid {
		t.Error("a withdrawn notification is still leased to a worker")
	}

	// A call in flight may already have landed. Flagged, not withdrawn - the
	// flag is consumed when the send finishes.
	if got := statusOf(t, s, inFlight); got != outbound.StatusSending {
		t.Errorf("a send in flight was interrupted rather than flagged: %s", got)
	}
	var requested bool
	if err := s.db.QueryRow(
		`SELECT cancellation_requested FROM outbound_intents WHERE id = $1`, inFlight).
		Scan(&requested); err != nil {
		t.Fatalf("read the flag: %v", err)
	}
	if !requested {
		t.Error("a send in flight to an erased person was not flagged for withdrawal")
	}

	// Something exists out there. Erasure removes the ability to contact
	// somebody; it does not unsend, and it does not pretend nothing happened.
	if got := statusOf(t, s, delivered); got != outbound.StatusSucceeded {
		t.Errorf("a delivered notification was rewritten as %s", got)
	}
	requireRedacted(t, s, delivered)

	// Somebody else's commitment is nobody else's business.
	if got := statusOf(t, s, channel); got != outbound.StatusPending {
		t.Errorf("a channel notification was withdrawn by an unrelated erasure: %s", got)
	}
	if endpointOf(t, s, channel).Valid {
		// It has no bound endpoint yet, but if it ever did, erasing a person
		// must not reach it.
		t.Error("a channel commitment was scrubbed by an unrelated erasure")
	}

	// The address is gone from both places the domain kept it.
	for _, intentID := range []string{inFlight, delivered} {
		if endpoint := endpointOf(t, s, intentID); endpoint.Valid {
			t.Errorf("the commitment still names %q as the address to send to", endpoint.String)
		}
	}
	var attemptEndpoints int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_attempts a
		JOIN outbound_intents i ON i.id = a.intent_id
		WHERE i.target_kind = 'user' AND i.target_ref = 'bob'
		  AND a.bound_endpoint IS NOT NULL`).Scan(&attemptEndpoints); err != nil {
		t.Fatalf("read the attempt endpoints: %v", err)
	}
	if attemptEndpoints != 0 {
		t.Errorf("%d attempts still name the address they sent to", attemptEndpoints)
	}

	// And the recipient survives, because the history is about a person this
	// system still has an (anonymized) row for.
	var stillNamed int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_intents WHERE target_kind = 'user' AND target_ref = 'bob'`).
		Scan(&stillNamed); err != nil {
		t.Fatalf("read the recipients: %v", err)
	}
	if stillNamed != 3 {
		t.Errorf("%d commitments still name their recipient, want 3", stillNamed)
	}

	// The withdrawal is an ending, and it is counted like any other - after
	// the erasure's own transaction committed.
	if got := terminalCount(t, outbound.FamilyNotification, string(outbound.StatusCanceled)) - before; got != 1 {
		t.Errorf("the erasure counted %v withdrawals, want 1", got)
	}
}

// requireRedacted is the third receipt state: the external object existed and
// its coordinates are gone.
//
// Both halves matter. Without the fact, the state machine decides nothing was
// ever sent and offers to send it again; without the redaction, erasure has
// removed an address from one table and left it in another.
func requireRedacted(t *testing.T, s *Store, intentID string) {
	t.Helper()

	var recorded bool
	var raw []byte
	var redactedAt sql.NullTime
	if err := s.db.QueryRow(`
		SELECT receipt_recorded, receipt, receipt_redacted_at
		FROM outbound_intents WHERE id = $1`, intentID).
		Scan(&recorded, &raw, &redactedAt); err != nil {
		t.Fatalf("read the receipt state: %v", err)
	}
	if !recorded {
		t.Error("the commitment forgot that a message exists out there")
	}
	if raw != nil {
		t.Errorf("the commitment still holds the coordinates: %s", raw)
	}
	if !redactedAt.Valid {
		t.Error("the commitment does not say its coordinates were redacted")
	}

	var leftovers int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_attempts
		WHERE intent_id = $1 AND (receipt IS NOT NULL OR response_summary IS NOT NULL)`,
		intentID).Scan(&leftovers); err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d attempts still hold coordinates or a summary", leftovers)
	}

	var observed int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_attempt_observations o
		JOIN outbound_attempts a ON a.id = o.attempt_id
		WHERE a.intent_id = $1 AND (o.receipt IS NOT NULL OR o.response_summary IS NOT NULL)`,
		intentID).Scan(&observed); err != nil {
		t.Fatalf("read the observations: %v", err)
	}
	if observed != 0 {
		t.Errorf("%d late results still hold coordinates or a summary", observed)
	}
}

// TestTheAddressDoesNotComeBack.
//
// A one-time sweep is not erasure. Everything below is a writer that runs AFTER
// the person is gone and would, without the durable marker, put a working
// address back into a table it was just removed from - and each is driven in
// both orders, because "erase, then write" and "write, then erase" are
// different code paths and only one of them is the sweep.
func TestTheAddressDoesNotComeBack(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	t.Run("a call that was already in flight answers after the erasure", func(t *testing.T) {
		seedTeam(t, s, "devops-1", "alice", "erased-1")
		intentID := oneOwed(t, s, dmForUser("erased-1", 1))
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "erased-1"); err != nil {
			t.Fatalf("erase: %v", err)
		}

		// Slack answers a moment later, with the coordinates of the message it
		// made. The result is kept; the coordinates are not.
		if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: acceptedConclusion(t, "D0001"),
		}); err != nil {
			t.Fatalf("finalize after the erasure: %v", err)
		}
		requireRedacted(t, s, intentID)
	})

	t.Run("a late result arrives after the erasure", func(t *testing.T) {
		seedTeam(t, s, "devops-2", "alice", "erased-2")
		intentID := oneOwed(t, s, dmForUser("erased-2", 1))
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		// Recovery reclaims the attempt: the worker's lease is gone.
		expireLease(t, s, intentID)
		if _, err := s.RecoverStaleAttempts(ctx, outbound.FamilyNotification, 10); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "erased-2"); err != nil {
			t.Fatalf("erase: %v", err)
		}

		// And only now the original worker answers. Its result is durable
		// proof, kept as an observation - without the address.
		result, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: acceptedConclusion(t, "D0002"),
		})
		if err != nil {
			t.Fatalf("finalize late: %v", err)
		}
		if !result.ObservationRecorded {
			t.Fatalf("the late result was not kept at all (%s)", result.Outcome)
		}

		var raw []byte
		var recorded bool
		if err := s.db.QueryRow(`
			SELECT receipt, receipt_recorded FROM outbound_attempt_observations
			WHERE attempt_id = $1`, begun.AttemptID).Scan(&raw, &recorded); err != nil {
			t.Fatalf("read the observation: %v", err)
		}
		if raw != nil {
			t.Errorf("the late result kept the coordinates: %s", raw)
		}
		if !recorded {
			t.Error("the late result forgot that a message exists out there")
		}
	})

	t.Run("a plan built before the erasure is admitted after it", func(t *testing.T) {
		seedTeam(t, s, "devops-3", "alice", "erased-3")
		agID := outboundGroup(t, s)
		// The producer built this while the person still existed.
		adm := outboundAdmission(t, agID, "first", dmForUser("erased-3", 1))

		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "erased-3"); err != nil {
			t.Fatalf("erase: %v", err)
		}

		result := mustSubmit(t, s, adm)
		if result.Outcome != outbound.SubmitRecipientErased {
			t.Fatalf("an admission promising an erased person answered %q", result.Outcome)
		}
		var owed int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM outbound_intents WHERE target_ref = 'erased-3'`).
			Scan(&owed); err != nil {
			t.Fatalf("count the commitments: %v", err)
		}
		if owed != 0 {
			t.Fatalf("%d commitments were created for an erased person", owed)
		}
	})

	t.Run("an operator tries to revive a commitment after the erasure", func(t *testing.T) {
		seedTeam(t, s, "devops-4", "alice", "erased-4")
		// A commitment erasure does NOT withdraw: it already ended, so there
		// is nothing owed - and reviving it is exactly the door the marker has
		// to keep shut. An unsent one would simply have been withdrawn.
		intentID := oneOwed(t, s, dmForUser("erased-4", 1))
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)
		if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomePermanentRejection, "channel_not_found"),
		}); err != nil {
			t.Fatalf("finalize as permanently failed: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusPermanentFailed {
			t.Fatalf("the fixture is %s, not a commitment an operator could revive", got)
		}

		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "erased-4"); err != nil {
			t.Fatalf("erase: %v", err)
		}

		for _, decision := range []outbound.Decision{
			outbound.DecisionRetryNewGeneration, outbound.DecisionAssumeAccepted,
		} {
			result, err := s.ResolveAmbiguity(ctx, outbound.ResolveAmbiguityRequest{
				IntentID: intentID, Decision: decision, Actor: "nina",
				Reason: "trying anyway", AcceptedDuplicateRisk: true,
			})
			if err != nil {
				t.Fatalf("%s: %v", decision, err)
			}
			if result.Outcome != outbound.ResolveRecipientErased {
				t.Errorf("%s on an erased recipient answered %q", decision, result.Outcome)
			}
		}

		// Withdrawing is still allowed: it sends nothing, and the alternative
		// is a commitment nobody can ever close.
		result, err := s.ResolveAmbiguity(ctx, outbound.ResolveAmbiguityRequest{
			IntentID: intentID, Decision: outbound.DecisionCancel,
			Actor: "nina", Reason: "the recipient is gone",
		})
		if err != nil || result.Outcome != outbound.ResolveResolved {
			t.Fatalf("withdrawing an erased recipient's commitment answered %q: %v",
				result.Outcome, err)
		}
	})
}

// TestTheJournalSaysWhatActuallyHappenedToASendInFlight. A call that is already
// out may have landed. Recorded as "canceled" it would be a claim about an
// external effect nobody knows the fate of - the one thing this domain refuses
// to guess at anywhere else.
func TestTheJournalSaysWhatActuallyHappenedToASendInFlight(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedTeam(t, s, "devops", "alice", "bob")

	intentID := oneOwed(t, s, dmForUser("bob", 1))
	token := claimOne(t, s, intentID)
	beginOne(t, s, intentID, token)

	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob"); err != nil {
		t.Fatalf("erase: %v", err)
	}

	var kind string
	if err := s.db.QueryRow(`
		SELECT kind FROM outbound_intent_events
		WHERE intent_id = $1 AND actor = 'erasure'
		ORDER BY seq DESC LIMIT 1`, intentID).Scan(&kind); err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if kind != "cancellation_requested" {
		t.Fatalf("a send in flight was recorded as %q", kind)
	}
}

// blocked reports whether something is still waiting, without a sleep deciding
// the answer.
//
// A short wait is unavoidable when the claim is "this has NOT finished" - there
// is no event for a thing that did not happen. What is avoided is a sleep
// deciding the ORDER: the order below is settled by a row lock somebody else is
// holding, so the test proves the serialisation rather than hoping for it.
func blocked(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-time.After(150 * time.Millisecond):
		return true
	}
}

// TestAdmissionWaitsForAnErasureItCannotSee.
//
// The sequence this exists for is entirely ordinary: a plan built while
// somebody was still on call, an operator taking them off it, an erasure, and
// an admission arriving a moment later with a commitment aimed at them. If the
// admission does not lock the recipients it can slip its inserts in after the
// erasure's sweep - leaving an obligation to an erased person that nothing
// marked, that fails for a reason nobody can act on, and that a second erasure
// will not pick up because erasing an erased user is a no-op.
//
// The lock is the whole fix, so the test holds one: the erasure's transaction
// is simulated by a real FOR UPDATE on the user row that does not commit until
// the test says so.
func TestAdmissionWaitsForAnErasureItCannotSee(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedTeam(t, s, "devops", "alice", "slow-erasure")

	agID := outboundGroup(t, s)
	adm := outboundAdmission(t, agID, "first", dmForUser("slow-erasure", 1))

	// An erasure in progress: the user row is taken and marked, and nothing is
	// committed yet.
	erasing, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the erasure: %v", err)
	}
	defer erasing.Rollback()
	if _, err := erasing.ExecContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR UPDATE`, "slow-erasure"); err != nil {
		t.Fatalf("lock the user: %v", err)
	}
	if _, err := erasing.ExecContext(ctx,
		`UPDATE users SET deleted_at = now() WHERE id = $1`, "slow-erasure"); err != nil {
		t.Fatalf("erase the user: %v", err)
	}

	admitted := make(chan outbound.SubmitResult, 1)
	failed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := s.SubmitEscalationBatch(ctx, adm)
		if err != nil {
			failed <- err
			return
		}
		admitted <- result
	}()

	if !blocked(done) {
		t.Fatal("the admission did not wait for the erasure holding its recipient; " +
			"it can insert a commitment to a person who is about to be gone")
	}

	if err := erasing.Commit(); err != nil {
		t.Fatalf("commit the erasure: %v", err)
	}

	select {
	case err := <-failed:
		t.Fatalf("submit: %v", err)
	case result := <-admitted:
		if result.Outcome != outbound.SubmitRecipientErased {
			t.Fatalf("after the erasure committed the admission answered %q", result.Outcome)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admission never came back after the erasure committed")
	}

	var owed int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_intents WHERE target_ref = 'slow-erasure'`).
		Scan(&owed); err != nil {
		t.Fatalf("count the commitments: %v", err)
	}
	if owed != 0 {
		t.Fatalf("%d commitments were created for an erased person", owed)
	}
}

// TestAnErasureWaitsForAnAdmissionThatGotThereFirst is the other order, and it
// is the reason the admission takes a SHARED lock rather than an exclusive one:
// two admissions naming the same people must not queue behind each other, and
// an erasure must.
//
// What the erasure finds when it gets in is the new commitment, which it
// withdraws like any other - which is why admitting first is safe.
func TestAnErasureWaitsForAnAdmissionThatGotThereFirst(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedTeam(t, s, "devops", "alice", "late-erasure")

	agID := outboundGroup(t, s)
	adm := outboundAdmission(t, agID, "first", dmForUser("late-erasure", 1))
	if got := mustSubmit(t, s, adm).Outcome; got != outbound.SubmitCreated {
		t.Fatalf("the admission answered %q", got)
	}

	// An admission in progress over the same person, holding the shared lock.
	admitting, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the admission: %v", err)
	}
	defer admitting.Rollback()
	if _, err := admitting.ExecContext(ctx,
		`SELECT id FROM users WHERE id = $1 FOR SHARE`, "late-erasure"); err != nil {
		t.Fatalf("lock the user: %v", err)
	}

	erased := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		erased <- erasure.NewService(s.ErasureRepository()).Erase(ctx, "late-erasure")
	}()

	if !blocked(done) {
		t.Fatal("the erasure did not wait for the admission holding its user")
	}

	if err := admitting.Commit(); err != nil {
		t.Fatalf("commit the admission: %v", err)
	}
	select {
	case err := <-erased:
		if err != nil {
			t.Fatalf("erase: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the erasure never came back after the admission committed")
	}

	// And what it found, it withdrew.
	var owed, marked int
	if err := s.db.QueryRow(`
		SELECT count(*) FILTER (WHERE status <> 'canceled'),
		       count(*) FILTER (WHERE recipient_erased_at IS NOT NULL)
		FROM outbound_intents WHERE target_ref = 'late-erasure'`).
		Scan(&owed, &marked); err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	if owed != 0 {
		t.Fatalf("%d commitments to an erased person are still owed", owed)
	}
	if marked == 0 {
		t.Fatal("the commitments were withdrawn without the erasure marker, so a " +
			"later writer could still put the address back")
	}
}

// TestErasureAndAResultInFlightMeetInBothOrders.
//
// Both sequences, and the assertion is the same in each: the coordinates do not
// survive and the fact does. If the result lands first, its receipt is written
// and the erasure redacts it; if the erasure lands first, the marker is already
// there and the result never writes an address at all.
//
// The commitment ends up in different states in the two orders - withdrawn in
// one, delivered-then-redacted in the other - which is why the check afterwards
// is "the address is nowhere", asked of every column of all three tables,
// rather than a status.
func TestErasureAndAResultInFlightMeetInBothOrders(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	orders := []struct {
		name         string
		erasureFirst bool
	}{
		{name: "the erasure starts first", erasureFirst: true},
		{name: "the result starts first"},
	}

	for i, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			userID := fmt.Sprintf("racer-%d", i)
			seedTeam(t, s, fmt.Sprintf("team-race-%d", i), "alice", userID)

			intentID := oneOwed(t, s, dmForUser(userID, 1))
			token := claimOne(t, s, intentID)
			begun := beginOne(t, s, intentID, token)

			var wg sync.WaitGroup
			start := make(chan struct{})
			var eraseErr, finalizeErr error

			wg.Add(2)
			// The order is TAKEN, not slept for: whichever goes second is
			// released only once the first has committed, so the interleaving
			// the subtest names is the one that actually happens.
			second := make(chan struct{})
			go func() {
				defer wg.Done()
				<-start
				if !order.erasureFirst {
					<-second
				}
				eraseErr = erasure.NewService(s.ErasureRepository()).Erase(ctx, userID)
				if order.erasureFirst {
					close(second)
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if order.erasureFirst {
					<-second
				}
				_, finalizeErr = s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
					AttemptID: begun.AttemptID, LeaseToken: token,
					Conclusion: acceptedConclusion(t, "D9999"),
				})
				if !order.erasureFirst {
					close(second)
				}
			}()
			close(start)
			wg.Wait()

			if eraseErr != nil {
				t.Fatalf("erase: %v", eraseErr)
			}
			if finalizeErr != nil {
				t.Fatalf("finalize: %v", finalizeErr)
			}

			requireRedacted(t, s, intentID)

			// And nowhere else either: the address must not be recoverable
			// from any column of any of the three tables.
			var leaks int
			if err := s.db.QueryRow(`
				SELECT (SELECT count(*) FROM outbound_intents
				        WHERE target_ref = $1 AND receipt::text LIKE '%D9999%')
				     + (SELECT count(*) FROM outbound_attempts a
				        JOIN outbound_intents i ON i.id = a.intent_id
				        WHERE i.target_ref = $1
				          AND (a.receipt::text LIKE '%D9999%'
				               OR a.bound_endpoint LIKE '%D9999%'
				               OR a.response_summary LIKE '%D9999%'))
				     + (SELECT count(*) FROM outbound_attempt_observations o
				        JOIN outbound_attempts a ON a.id = o.attempt_id
				        JOIN outbound_intents i ON i.id = a.intent_id
				        WHERE i.target_ref = $1
				          AND (o.receipt::text LIKE '%D9999%'
				               OR o.response_summary LIKE '%D9999%'))`,
				userID).Scan(&leaks); err != nil {
				t.Fatalf("look for the address: %v", err)
			}
			if leaks != 0 {
				t.Fatalf("the address survived the race in %d places", leaks)
			}
		})
	}
}

// TestTheJournalTellsAnErasedDeliveryFromOneThatNeverHappened.
//
// Read through the journal, not through SQL, because the journal is what a
// person looking at a stuck alert actually sees - and after an erasure it shows
// no receipt on both a delivery that happened and one that never did. Those
// mean opposite things, and the difference cannot be recovered from the
// commitment: after a new generation its own receipt describes the CURRENT
// effect, while the question is about the attempt in front of you.
func TestTheJournalTellsAnErasedDeliveryFromOneThatNeverHappened(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	t.Run("a delivery that happened, then was erased", func(t *testing.T) {
		seedTeam(t, s, "devops-j1", "alice", "journal-1")
		intentID := oneOwed(t, s, dmForUser("journal-1", 1))
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)
		if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: acceptedConclusion(t, "D7001"),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "journal-1"); err != nil {
			t.Fatalf("erase: %v", err)
		}

		journal, err := s.IntentJournal(ctx, intentID)
		if err != nil {
			t.Fatalf("read the journal: %v", err)
		}
		if len(journal.Attempts) != 1 {
			t.Fatalf("the journal holds %d attempts", len(journal.Attempts))
		}
		attempt := journal.Attempts[0]
		if !attempt.ReceiptRecorded {
			t.Error("the journal says nothing was ever delivered")
		}
		if attempt.Receipt != nil {
			t.Errorf("the journal still shows the coordinates: %s", attempt.Receipt)
		}
		if attempt.ReceiptRedactedAt == nil {
			t.Error("the journal cannot say the coordinates were removed rather than absent")
		}
	})

	t.Run("a late result that was kept, then erased", func(t *testing.T) {
		seedTeam(t, s, "devops-j2", "alice", "journal-2")
		intentID := oneOwed(t, s, dmForUser("journal-2", 1))
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		expireLease(t, s, intentID)
		if _, err := s.RecoverStaleAttempts(ctx, outbound.FamilyNotification, 10); err != nil {
			t.Fatalf("recover: %v", err)
		}
		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "journal-2"); err != nil {
			t.Fatalf("erase: %v", err)
		}
		if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: acceptedConclusion(t, "D7002"),
		}); err != nil {
			t.Fatalf("finalize late: %v", err)
		}

		journal, err := s.IntentJournal(ctx, intentID)
		if err != nil {
			t.Fatalf("read the journal: %v", err)
		}
		if len(journal.Observations) != 1 {
			t.Fatalf("the journal holds %d late results", len(journal.Observations))
		}
		observed := journal.Observations[0]
		if !observed.ReceiptRecorded {
			t.Error("the journal says the late result proved nothing")
		}
		if observed.Receipt != nil {
			t.Errorf("the journal still shows the coordinates: %s", observed.Receipt)
		}
		if observed.ReceiptRedactedAt == nil {
			t.Error("the journal cannot say the coordinates were removed rather than absent")
		}
	})

	t.Run("a delivery that never happened", func(t *testing.T) {
		seedTeam(t, s, "devops-j3", "alice", "journal-3")
		intentID := oneOwed(t, s, dmForUser("journal-3", 1))
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)
		if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token,
			Conclusion: concluded(outbound.OutcomePermanentRejection, "channel_not_found"),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "journal-3"); err != nil {
			t.Fatalf("erase: %v", err)
		}

		journal, err := s.IntentJournal(ctx, intentID)
		if err != nil {
			t.Fatalf("read the journal: %v", err)
		}
		attempt := journal.Attempts[0]
		if attempt.ReceiptRecorded || attempt.ReceiptRedactedAt != nil {
			t.Errorf("an attempt that produced nothing reads as recorded=%v redacted=%v, "+
				"which is what an erased delivery is supposed to look like",
				attempt.ReceiptRecorded, attempt.ReceiptRedactedAt)
		}
	})
}
