package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The delivery routes' source: the projection's closed vocabulary, and the
// replay as a new commitment under its own key.

// TestTheStatusProjectionIsClosed: every status the domain has reads as one of
// the route's four words, and the one a one-shot can never have is refused
// rather than shown as nothing.
func TestTheStatusProjectionIsClosed(t *testing.T) {
	for _, tc := range []struct {
		status   outbound.Status
		attempts int
		want     model.OutboxDeliveryStatus
	}{
		{outbound.StatusPending, 0, model.OutboxDeliveryPending},
		{outbound.StatusSending, 0, model.OutboxDeliveryPending},
		{outbound.StatusPending, 2, model.OutboxDeliveryRetry},
		{outbound.StatusSending, 1, model.OutboxDeliveryRetry},
		{outbound.StatusSucceeded, 1, model.OutboxDeliverySent},
		{outbound.StatusPermanentFailed, 1, model.OutboxDeliveryFailed},
		{outbound.StatusExpired, 0, model.OutboxDeliveryFailed},
		{outbound.StatusCanceled, 0, model.OutboxDeliveryFailed},
		{outbound.StatusManualReview, 3, model.OutboxDeliveryFailed},
	} {
		got, err := projectDeliveryStatus(tc.status, tc.attempts)
		if err != nil || got != tc.want {
			t.Errorf("%s after %d attempts reads as %q (%v), want %q", tc.status, tc.attempts, got, err, tc.want)
		}
	}
	for _, status := range []outbound.Status{outbound.StatusIdle, "carrier_pigeon"} {
		if got, err := projectDeliveryStatus(status, 0); err == nil {
			t.Errorf("%s read as %q instead of being refused", status, got)
		}
	}
}

func admissionsCount(t *testing.T, outcome string) float64 {
	t.Helper()
	counter, err := metrics.OutboundAdmissionsTotal.GetMetricWithLabelValues("webhook", outcome)
	if err != nil {
		t.Fatal(err)
	}
	var m dto.Metric
	if err := counter.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

// ended puts a commitment into a terminal state. The webhook family reaches
// them through the worker; the states are constructed here so the replay can be
// asked about each of them.
func ended(t *testing.T, s *Store, intentID string, status outbound.Status) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE outbound_intents SET status = $2, lease_token = NULL, locked_until = NULL,
		worker_id = NULL, current_attempt_id = NULL WHERE id = $1`, intentID, string(status)); err != nil {
		t.Fatalf("construct %s: %v", status, err)
	}
}

func replay(s *Store, integrationID, deliveryID, key string) (WebhookReplayResult, error) {
	return s.ReplayWebhookDelivery(context.Background(), WebhookReplayRequest{
		IntegrationID: integrationID, DeliveryID: deliveryID, ClientRequestID: key, Actor: byUser("nina"),
	})
}

func webhookRow(t *testing.T, s *Store, intentID string) (kind, status, eventID, body string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT key_kind, status, payload->>'event_id', payload->>'body'
		FROM outbound_intents WHERE id = $1`, intentID).Scan(&kind, &status, &eventID, &body); err != nil {
		t.Fatalf("read %s: %v", intentID, err)
	}
	return
}

// TestAReplayIsANewCommitmentUnderItsOwnKey: from each of the four endings a
// replay makes a new commitment of the replay kind for the same event and body;
// the original is untouched; the same key finds the same commitment and a
// different key makes another; nothing live, nothing of another family, nothing
// of a deleted subscriber and nothing with a swapped payload is replayed.
func TestAReplayIsANewCommitmentUnderItsOwnKey(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	for i := 0; i < 4; i++ {
		fannedOut(t, s)
	}
	originals := commitmentsOwedTo(t, s, id)
	countRows := func() int {
		return len(commitmentsOwedTo(t, s, id))
	}

	// Live: not replayed, nothing made.
	if _, err := replay(s, id, originals[0].ID, "k-live"); !errors.Is(err, ErrWebhookDeliveryNotTerminal) {
		t.Fatalf("a replay of a pending commitment answered %v", err)
	}
	inFlightWebhook(t, s, originals[1].ID)
	if _, err := replay(s, id, originals[1].ID, "k-wire"); !errors.Is(err, ErrWebhookDeliveryNotTerminal) {
		t.Fatalf("a replay of a commitment on the wire answered %v", err)
	}
	ended(t, s, originals[1].ID, outbound.StatusManualReview)
	if _, err := replay(s, id, originals[1].ID, "k-review"); !errors.Is(err, ErrWebhookDeliveryNotTerminal) {
		t.Fatalf("a replay of a commitment waiting on a person answered %v", err)
	}
	if countRows() != 4 {
		t.Fatalf("refused replays left rows behind: %d", countRows())
	}

	createdBefore, existingBefore := admissionsCount(t, "created"), admissionsCount(t, "existing")
	made := map[outbound.Status]WebhookReplayResult{}
	for i, status := range []outbound.Status{outbound.StatusSucceeded, outbound.StatusPermanentFailed,
		outbound.StatusExpired, outbound.StatusCanceled} {
		original := originals[i]
		ended(t, s, original.ID, status)
		_, _, wantEvent, wantBody := webhookRow(t, s, original.ID)

		result, err := replay(s, id, original.ID, "k-"+string(status))
		if err != nil || result.Outcome != outbound.SubmitCreated || result.DeliveryID == "" || result.DeliveryID == original.ID {
			t.Fatalf("replay from %s: %+v (%v)", status, result, err)
		}
		made[status] = result
		kind, newStatus, eventID, body := webhookRow(t, s, result.DeliveryID)
		if kind != string(keys.KindWebhookReplay) || newStatus != "pending" || eventID != wantEvent || body != wantBody {
			t.Fatalf("the replay of %s is %s/%s for event %s with body %s", status, kind, newStatus, eventID, body)
		}
		if _, still, _, _ := webhookRow(t, s, original.ID); still != string(status) {
			t.Fatalf("the original left %s for %s", status, still)
		}
	}
	if countRows() != 8 {
		t.Fatalf("four replays made %d commitments", countRows()-4)
	}
	if got := admissionsCount(t, "created") - createdBefore; got != 4 {
		t.Fatalf("created counted %v, want 4", got)
	}

	// The same key is the same commitment and the same answer; another key is
	// another decision.
	first := made[outbound.StatusSucceeded]
	again, err := replay(s, id, originals[0].ID, "k-succeeded")
	if err != nil || again.Outcome != outbound.SubmitExisting || again.DeliveryID != first.DeliveryID {
		t.Fatalf("the repeat answered %+v (%v), the first %+v", again, err, first)
	}
	if countRows() != 8 {
		t.Fatalf("a repeated key made a commitment: %d rows", countRows())
	}
	if got := admissionsCount(t, "existing") - existingBefore; got != 1 {
		t.Fatalf("existing counted %v, want 1", got)
	}
	second, err := replay(s, id, originals[0].ID, "k-second-decision")
	if err != nil || second.Outcome != outbound.SubmitCreated || second.DeliveryID == first.DeliveryID {
		t.Fatalf("a second key answered %+v (%v)", second, err)
	}

	// The replay of a replay is allowed once it has ended.
	ended(t, s, first.DeliveryID, outbound.StatusPermanentFailed)
	if result, err := replay(s, id, first.DeliveryID, "k-of-a-replay"); err != nil || result.Outcome != outbound.SubmitCreated {
		t.Fatalf("replaying a replay: %+v (%v)", result, err)
	}
	rows := countRows()

	// No key: the door refuses.
	if _, err := replay(s, id, originals[0].ID, ""); err == nil {
		t.Fatal("a replay without a key was admitted")
	}
	// Another family's commitment that happens to name this subscriber.
	agID := desiredGroup(t, s, "Disk filling up")
	foreign := admitOne(t, s, agID, channelCommitment(id, 0))[0]
	ended(t, s, foreign, outbound.StatusSucceeded)
	if _, err := replay(s, id, foreign, "k-foreign"); !errors.Is(err, ErrWebhookDeliveryNotFound) {
		t.Fatalf("a replay of another family's commitment answered %v", err)
	}
	// Another subscriber's id.
	other := subscriber(t, s, "other", model.WebhookScopeGlobal, "", true)
	if _, err := replay(s, other, originals[0].ID, "k-other"); !errors.Is(err, ErrWebhookDeliveryNotFound) {
		t.Fatalf("a replay through another subscriber answered %v", err)
	}
	// A payload swapped on the row: valid, addressed to the same subscriber,
	// and not what was admitted.
	if _, err := s.db.Exec(`UPDATE outbound_intents
		SET payload = jsonb_set(payload, '{body}', to_jsonb('{"event":"alert_group.firing","alert_group":{"id":"elsewhere"}}'::text))
		WHERE id = $1`, originals[2].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := replay(s, id, originals[2].ID, "k-swapped"); err == nil ||
		errors.Is(err, ErrWebhookDeliveryNotFound) || errors.Is(err, ErrWebhookDeliveryNotTerminal) {
		t.Fatalf("a replay of a swapped payload answered %v", err)
	}
	if countRows() != rows {
		t.Fatalf("refused replays left rows behind: %d -> %d", rows, countRows())
	}

	// A deleted subscriber: the history is readable, a new delivery is not made.
	if _, err := s.DeleteIntegration(context.Background(), id, "nina"); err != nil {
		t.Fatal(err)
	}
	if _, err := replay(s, id, originals[0].ID, "k-after-deletion"); !errors.Is(err, ErrWebhookDeliveryNotFound) {
		t.Fatalf("a replay to a deleted subscriber answered %v", err)
	}
	if countRows() != rows {
		t.Fatalf("a deleted subscriber was promised something: %d -> %d", rows, countRows())
	}
	deliveries, total, err := s.ListWebhookDeliveries(context.Background(), id, 50, 0)
	if err != nil || total != rows || len(deliveries) != rows {
		t.Fatalf("the deleted subscriber's history: %d of %d (%v)", len(deliveries), total, err)
	}
}

// TestReplayAndDeletionLeaveNoLiveCommitmentInEitherOrder: the replay reads the
// subscriber FOR SHARE, the deletion takes FOR UPDATE. Replay first: the
// deletion waits, is refused past its timeout, and its repeat withdraws the
// replay's commitment. Deletion first: the replay finds no subscriber.
func TestReplayAndDeletionLeaveNoLiveCommitmentInEitherOrder(t *testing.T) {
	t.Run("the replay wins", func(t *testing.T) {
		s := setupTestDB(t)
		s.lockTimeout = 300 * time.Millisecond
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		eventID := fannedOut(t, s)
		original := onlyOne(t, commitmentsOwedTo(t, s, id))
		ended(t, s, original.ID, outbound.StatusSucceeded)

		// A replay frozen after its reads: the shared lock is held and the new
		// commitment written, the transaction not yet committed.
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM integrations WHERE id = $1 FOR SHARE`, id); err != nil {
			t.Fatal(err)
		}
		batch := webhookBatch(keys.KindWebhookReplay, eventID, id)
		batch.ClientRequestID = "k-race"
		if _, err := admitWebhookTx(ctx, tx, batch, byUser("nina")); err != nil {
			t.Fatalf("admit the replay under the lock: %v", err)
		}

		if _, err := s.DeleteIntegration(ctx, id, "nina"); !errors.Is(err, ErrIntegrationBusy) {
			tx.Rollback()
			t.Fatalf("a deletion against a replay in progress answered %v, want busy", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		change, err := s.DeleteIntegration(ctx, id, "nina")
		if err != nil || change.Withdrawn != 1 {
			t.Fatalf("the repeated deletion: %+v (%v), want the replay's commitment withdrawn", change, err)
		}
		noLiveCommitment(t, s, id)
	})
	t.Run("the deletion holds the row: the replay waits, and is refused past the timeout", func(t *testing.T) {
		s := setupTestDB(t)
		s.lockTimeout = 300 * time.Millisecond
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		fannedOut(t, s)
		original := onlyOne(t, commitmentsOwedTo(t, s, id))
		ended(t, s, original.ID, outbound.StatusSucceeded)

		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `SELECT id FROM integrations WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := replay(s, id, original.ID, "k-held"); !errors.Is(err, ErrIntegrationBusy) {
			t.Fatalf("a replay against a held subscriber answered %v, want busy", err)
		}
		if got := commitmentsOwedTo(t, s, id); len(got) != 1 {
			t.Fatalf("a refused replay made a commitment: %+v", got)
		}
	})
	t.Run("the deletion wins", func(t *testing.T) {
		s := setupTestDB(t)
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		fannedOut(t, s)
		original := onlyOne(t, commitmentsOwedTo(t, s, id))
		ended(t, s, original.ID, outbound.StatusSucceeded)
		if _, err := s.DeleteIntegration(context.Background(), id, "nina"); err != nil {
			t.Fatal(err)
		}
		if _, err := replay(s, id, original.ID, "k-late"); !errors.Is(err, ErrWebhookDeliveryNotFound) {
			t.Fatalf("a replay after the deletion answered %v", err)
		}
		if got := commitmentsOwedTo(t, s, id); len(got) != 1 {
			t.Fatalf("the deleted subscriber was promised %+v", got)
		}
	})
}

// TestOperatorRetriesAreRefusedForWebhookCommitments: the replay is the one
// door to a new effect for these kinds. From every state an escalation's
// operator may retry, a webhook commitment answers invalid_decision - and the
// replay afterwards is the only live commitment.
func TestOperatorRetriesAreRefusedForWebhookCommitments(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	fannedOut(t, s)
	owed := commitmentsOwedTo(t, s, id)
	states := map[string]outbound.Status{
		owed[0].ID: outbound.StatusManualReview,
		owed[1].ID: outbound.StatusPermanentFailed,
	}
	for intentID, state := range states {
		ended(t, s, intentID, state)
	}

	for intentID, state := range states {
		for _, decision := range []outbound.Decision{outbound.DecisionRetryCurrentGeneration, outbound.DecisionRetryNewGeneration} {
			result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
				IntentID: intentID, Decision: decision, Actor: byUser("nina"), Reason: "try again", AcceptedDuplicateRisk: true,
			})
			if err != nil || result.Outcome != outbound.ResolveInvalidDecision {
				t.Fatalf("%s on a %s webhook commitment answered %+v (%v)", decision, state, result, err)
			}
		}
		if _, status, _, _ := webhookRow(t, s, intentID); status != string(state) {
			t.Fatalf("the operator's refused decision moved %s to %s", state, status)
		}
	}

	ended(t, s, owed[0].ID, outbound.StatusPermanentFailed)
	if _, err := replay(s, id, owed[0].ID, "k-after"); err != nil {
		t.Fatal(err)
	}
	var live int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intents WHERE delivery_family = 'webhook'
		AND target_ref = $1 AND status IN ('pending', 'sending', 'manual_review')`, id).Scan(&live); err != nil || live != 1 {
		t.Fatalf("%d live commitments after the replay (%v), want exactly the replay", live, err)
	}
}

// TestAReplayToASwitchedOffSubscriberIsRefused: switching a subscriber off
// withdrew what it was owed, and what was withdrawn is terminal - so it can be
// asked for again, and a replay that obliged would hand the work back through
// the other door and the channel, which does not look at the switch, would
// deliver it. Refused while the subscriber is off; allowed again once it is on.
// And a commitment addressed to something that is not a webhook subscriber has
// no replay at all.
func TestAReplayToASwitchedOffSubscriberIsRefused(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	fannedOut(t, s)
	owed := commitmentsOwedTo(t, s, id)
	ended(t, s, owed[0].ID, outbound.StatusSucceeded)
	off, on := false, true

	change, err := s.UpdateIntegration(ctx, id, IntegrationPatch{Enabled: &off}, "nina")
	if err != nil || change.Withdrawn != 1 {
		t.Fatalf("the switch answered %+v (%v)", change, err)
	}
	// The exact path: the withdrawn commitment is canceled, which is terminal,
	// and the delivered one is too. Neither may be replayed now.
	for _, original := range owed {
		if _, err := replay(s, id, original.ID, "k-off-"+original.ID); !errors.Is(err, ErrWebhookSubscriberDisabled) {
			t.Fatalf("a replay to a switched-off subscriber answered %v", err)
		}
	}
	if got := commitmentsOwedTo(t, s, id); len(got) != 2 {
		t.Fatalf("a refused replay made a commitment: %+v", got)
	}

	if _, err := s.UpdateIntegration(ctx, id, IntegrationPatch{Enabled: &on}, "nina"); err != nil {
		t.Fatal(err)
	}
	result, err := replay(s, id, owed[1].ID, "k-on")
	if err != nil || result.Outcome != outbound.SubmitCreated {
		t.Fatalf("a replay to the subscriber switched back on answered %+v (%v)", result, err)
	}

	// Not a subscriber: an integration of another type, named by a commitment
	// through the door. The replay route has nothing for it.
	slackCfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-1"})
	slack := &model.Integration{Type: model.IntegrationTypeSlack, Name: "slack", Enabled: true, Config: slackCfg}
	if err := s.CreateIntegration(slack); err != nil {
		t.Fatal(err)
	}
	misaddressed, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-slack", slack.ID))
	if err != nil {
		t.Fatal(err)
	}
	ended(t, s, misaddressed.IntentIDs[0], outbound.StatusSucceeded)
	if _, err := replay(s, slack.ID, misaddressed.IntentIDs[0], "k-slack"); !errors.Is(err, ErrWebhookDeliveryNotFound) {
		t.Fatalf("a replay to an integration that is not a subscriber answered %v", err)
	}
}

// TestReplayAndDisablingLeaveNoLiveCommitmentInEitherOrder: the replay reads
// the subscriber FOR SHARE, the switch takes FOR UPDATE. Replay first: the
// switch waits, is refused past its timeout, and its repeat withdraws the
// replay's commitment. Switch first: the replay is refused.
func TestReplayAndDisablingLeaveNoLiveCommitmentInEitherOrder(t *testing.T) {
	off := false
	t.Run("the replay wins", func(t *testing.T) {
		s := setupTestDB(t)
		s.lockTimeout = 300 * time.Millisecond
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		eventID := fannedOut(t, s)
		original := onlyOne(t, commitmentsOwedTo(t, s, id))
		ended(t, s, original.ID, outbound.StatusSucceeded)

		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT enabled FROM integrations WHERE id = $1 FOR SHARE`, id); err != nil {
			t.Fatal(err)
		}
		batch := webhookBatch(keys.KindWebhookReplay, eventID, id)
		batch.ClientRequestID = "k-race"
		if _, err := admitWebhookTx(ctx, tx, batch, byUser("nina")); err != nil {
			t.Fatalf("admit the replay under the lock: %v", err)
		}

		if _, err := s.UpdateIntegration(ctx, id, IntegrationPatch{Enabled: &off}, "nina"); !errors.Is(err, ErrIntegrationBusy) {
			tx.Rollback()
			t.Fatalf("a switch against a replay in progress answered %v, want busy", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		change, err := s.UpdateIntegration(ctx, id, IntegrationPatch{Enabled: &off}, "nina")
		if err != nil || change.Withdrawn != 1 {
			t.Fatalf("the repeated switch: %+v (%v), want the replay's commitment withdrawn", change, err)
		}
		noLiveCommitment(t, s, id)
	})
	t.Run("the switch wins", func(t *testing.T) {
		s := setupTestDB(t)
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		fannedOut(t, s)
		original := onlyOne(t, commitmentsOwedTo(t, s, id))
		ended(t, s, original.ID, outbound.StatusSucceeded)
		if _, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina"); err != nil {
			t.Fatal(err)
		}
		if _, err := replay(s, id, original.ID, "k-late"); !errors.Is(err, ErrWebhookSubscriberDisabled) {
			t.Fatalf("a replay after the switch answered %v", err)
		}
		if got := commitmentsOwedTo(t, s, id); len(got) != 1 {
			t.Fatalf("a switched-off subscriber was promised %+v", got)
		}
	})
}
