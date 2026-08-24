package store

import (
	"context"
	"database/sql"
	"testing"

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
	// somebody; it does not unsend.
	if got := statusOf(t, s, delivered); got != outbound.StatusSucceeded {
		t.Errorf("a delivered notification was rewritten as %s", got)
	}

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
