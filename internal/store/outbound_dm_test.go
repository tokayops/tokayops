package store

import (
	"context"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a direct message takes from the card is settled when its generation
// opens and never again inside it.

// stepCard is a channel card posted by a numbered step of the policy.
func stepCard(ref string, index int) keys.EscalationCommitment {
	c := channelCommitment(ref, 0)
	c.Slot = keys.Slot{Kind: keys.SlotPolicy, Index: index}
	return c
}

func intentAddressedTo(t *testing.T, s *Store, agID, ref string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM outbound_intents WHERE alert_group_id = $1 AND target_ref = $2`,
		agID, ref).Scan(&id); err != nil {
		t.Fatalf("find the commitment to %s: %v", ref, err)
	}
	return id
}

// takeOne claims one commitment after letting go of every lease an earlier
// claim of the test still holds on work that has not begun: a claim takes
// everything due, as a worker's does.
func takeOne(t *testing.T, s *Store, id string) string {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE outbound_intents SET locked_until = now() - interval '1 second'
		WHERE lease_token IS NOT NULL AND status = 'pending'`); err != nil {
		t.Fatalf("let go of the leases: %v", err)
	}
	return claimOne(t, s, id)
}

// postedAs delivers a card and records the receipt Slack gave it.
func postedAs(t *testing.T, s *Store, id, ref string) {
	t.Helper()
	token := takeOne(t, s, id)
	begun := beginOne(t, s, id, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: conclusion(outbound.ConclusionInput{
			Outcome: outbound.OutcomeAccepted, Status: "ok",
			Receipt: receiptOf(ref, `{"channel":"C","ts":"1"}`),
		}),
	}); err != nil {
		t.Fatalf("post %s: %v", id, err)
	}
}

func boundContextOf(t *testing.T, begun outbound.BeginAttemptResult) outbound.BoundContext {
	t.Helper()
	return begun.BoundContext
}

func slackWorkspaceIntegration(t *testing.T, s *Store, config string) string {
	t.Helper()
	integration := &model.Integration{
		Type: model.IntegrationTypeSlack, Name: "Slack", Enabled: true, Config: []byte(config),
	}
	if err := s.CreateIntegration(integration); err != nil {
		t.Fatalf("create the Slack integration: %v", err)
	}
	return integration.ID
}

// TestADirectMessagePointsBackToTheCardItsGenerationBound. A message that
// begins before any card has gone out binds nothing, and a retry inside the
// same generation still carries nothing, though a card exists by then: the
// bytes of one generation do not change. A new generation binds again - to
// the policy's own channel card when it has a message, else to the firehose
// card - together with the workspace the card is in.
func TestADirectMessagePointsBackToTheCardItsGenerationBound(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), stepCard("C-step", 1), dmCommitment("u-1"))
	fire := intentAddressedTo(t, s, agID, "C-fire")
	step := intentAddressedTo(t, s, agID, "C-step")
	dm := intentAddressedTo(t, s, agID, "u-1")

	// Before any card: nothing to point back to.
	token := takeOne(t, s, dm)
	begun := beginOne(t, s, dm, token)
	if !boundContextOf(t, begun).Empty() {
		t.Fatalf("a message sent before any card was bound to %+v", begun.BoundContext)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND bound_context IS NULL`, dm) != 1 {
		t.Fatal("an empty context was stored as something")
	}
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatal(err)
	}
	postedAs(t, s, fire, "C-fire/1700000000.000100")

	// The retry of the same generation: the card exists now, and the message
	// still does not point to it.
	due(t, s, dm)
	token = takeOne(t, s, dm)
	begun = beginOne(t, s, dm, token)
	if !boundContextOf(t, begun).Empty() {
		t.Fatalf("a retry inside the generation was bound to %+v", begun.BoundContext)
	}
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "user_not_found"),
	}); err != nil {
		t.Fatal(err)
	}

	// A new generation binds again: the firehose card, since the policy's own
	// step has no message yet, in the workspace the integration names.
	resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: dm, Decision: outbound.DecisionRetryNewGeneration, AcceptedDuplicateRisk: true, Reason: "again",
	})
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND bound_context IS NULL`, dm) != 1 {
		t.Fatal("a new generation kept the old context")
	}
	token = takeOne(t, s, dm)
	begun = beginOne(t, s, dm, token)
	if got := boundContextOf(t, begun); got != (outbound.BoundContext{
		CardReceiptRef: "C-fire/1700000000.000100", TeamURL: "https://acme.slack.com/"}) {
		t.Fatalf("the new generation was bound to %+v", got)
	}
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "user_not_found"),
	}); err != nil {
		t.Fatal(err)
	}

	// The policy's own card, once it has a message, wins over the firehose.
	// The retry lets go of the old context with the old address: what the
	// row shows between the retry and the next begin is a generation not yet
	// bound, not the last one's card.
	postedAs(t, s, step, "C-step/1700000000.000200")
	resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: dm, Decision: outbound.DecisionRetryNewGeneration, AcceptedDuplicateRisk: true, Reason: "again",
	})
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1 AND bound_context IS NULL`, dm) != 1 {
		t.Fatal("the new generation kept the last one's context")
	}
	begun = beginOne(t, s, dm, takeOne(t, s, dm))
	if got := boundContextOf(t, begun).CardReceiptRef; got != "C-step/1700000000.000200" {
		t.Fatalf("the message points to %q over the policy's own card", got)
	}
}

// TestTheFirehoseIsAFallbackTheInstallationCanRefuse. With the setting off,
// a message whose policy posted no channel card of its own points to nothing;
// and a card in a workspace no integration names is bound without one, so the
// message goes out without a link rather than with a broken one.
func TestTheFirehoseIsAFallbackTheInstallationCanRefuse(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	integration := slackWorkspaceIntegration(t, s, `{"token":"xoxb-test","team_url":"https://acme.slack.com/"}`)

	s.SetDMFallbackToFirehose(false)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), stepCard("C-step", 1), dmCommitment("u-1"))
	postedAs(t, s, intentAddressedTo(t, s, agID, "C-fire"), "C-fire/1700000000.000100")
	dm := intentAddressedTo(t, s, agID, "u-1")
	if begun := beginOne(t, s, dm, takeOne(t, s, dm)); !boundContextOf(t, begun).Empty() {
		t.Fatalf("with the fallback off the message was bound to %+v", begun.BoundContext)
	}

	s.SetDMFallbackToFirehose(true)
	if _, err := s.UpdateIntegration(context.Background(), integration,
		IntegrationPatch{Enabled: boolPtr(false)}, "nina"); err != nil {
		t.Fatalf("disable the integration: %v", err)
	}
	agID = desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-2"))
	postedAs(t, s, intentAddressedTo(t, s, agID, "C-fire"), "C-fire/1700000000.000300")
	dm = intentAddressedTo(t, s, agID, "u-2")
	begun := beginOne(t, s, dm, takeOne(t, s, dm))
	if got := boundContextOf(t, begun); got != (outbound.BoundContext{CardReceiptRef: "C-fire/1700000000.000300"}) {
		t.Fatalf("without an enabled integration the message was bound to %+v", got)
	}
}

func boolPtr(v bool) *bool { return &v }

// TestAContextNobodyCanReadIsRefusedBeforeTheNetwork. The bound context is
// written by this build's store and read back at every begin of the
// generation; a row this build cannot read is damage, and it ends the
// commitment where a person will see it - not inside the call, where it
// would be an attempt that never touched the network, retried forever.
func TestAContextNobodyCanReadIsRefusedBeforeTheNetwork(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-fire", 0), dmCommitment("u-1"))
	postedAs(t, s, intentAddressedTo(t, s, agID, "C-fire"), "C-fire/1700000000.000100")
	dm := intentAddressedTo(t, s, agID, "u-1")

	token := takeOne(t, s, dm)
	begun := beginOne(t, s, dm, token) // the generation is open and bound
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE outbound_intents
		SET bound_context = '{"card_receipt_ref":"C-fire/1700000000.000100","louder":true}' WHERE id = $1`, dm); err != nil {
		t.Fatal(err)
	}
	due(t, s, dm)

	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: dm, LeaseToken: takeOne(t, s, dm), WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "D0001",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if result.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("a context this build cannot read began as %s", result.Outcome)
	}
	if got := statusOf(t, s, dm); got != outbound.StatusPermanentFailed {
		t.Fatalf("the message is %s", got)
	}
	if got := lastErrorClass(t, s, dm); got != "bound_context_unreadable" {
		t.Fatalf("the refusal is classed %q", got)
	}
}
