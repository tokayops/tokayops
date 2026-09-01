package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The webhook family under the machinery every family shares: expiry, lease
// loss and recovery, the claim partition, the alert's own transitions, the
// payload digest, and erasure. None of it is new code; all of it is asserted
// for the third family rather than assumed from the first two.

// webhookEventFor writes one alert event about a given group and fans it out.
func webhookEventFor(t *testing.T, s *Store, agID, teamID string, eventType model.OutboxEventType, body string) string {
	t.Helper()
	event := &model.OutboxEvent{
		ID: uuid.New().String(), EventType: eventType, AlertGroupID: agID, TeamID: teamID,
		Payload: json.RawMessage(body),
	}
	if err := s.CreateOutboxEvent(event); err != nil {
		t.Fatalf("write the event: %v", err)
	}
	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found || result.EventID != event.ID {
		t.Fatalf("fan out: %+v (%v)", result, err)
	}
	return event.ID
}

// leasedWebhook takes a webhook commitment through the real claim and the real
// beginning of an attempt, and hands back what the worker would hold.
func leasedWebhook(t *testing.T, s *Store, intentID string) (token, attemptID string) {
	t.Helper()
	ctx := context.Background()
	leased, err := s.ClaimDueIntents(ctx, outbound.ClaimRequest{
		Family: outbound.FamilyWebhook, Provider: keys.ProviderWebhook,
		Phase: outbound.ClaimRetriesFirst, Limit: 10, Lease: outbound.WebhookLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, l := range leased {
		if l.Intent.ID != intentID {
			continue
		}
		begun, err := s.BeginAttempt(ctx, outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: l.LeaseToken, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "https://example.com/hooks",
		})
		if err != nil {
			t.Fatalf("begin the attempt: %v", err)
		}
		return l.LeaseToken, begun.AttemptID
	}
	t.Fatalf("the claim did not include %s", intentID)
	return "", ""
}

// webhookAccepted is what the channel says after a 200: accepted, and no
// receipt, because a POST makes no object.
func webhookAccepted() outbound.Conclusion {
	return conclusion(outbound.ConclusionInput{
		Outcome: outbound.OutcomeAccepted, Status: "200", KeyKind: keys.KindWebhookEvent,
	})
}

func webhookClaims(t *testing.T, s *Store, limit int) []outbound.Leased {
	t.Helper()
	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: outbound.FamilyWebhook, Provider: keys.ProviderWebhook,
		Phase: outbound.ClaimRetriesFirst, Limit: limit, Lease: outbound.WebhookLease, WorkerID: "worker-w",
	})
	if err != nil {
		t.Fatalf("claim webhooks: %v", err)
	}
	return leased
}

// TestAWebhookCommitmentOlderThanADayExpires: the family's expiry ends a
// commitment nothing was ever sent for, it is counted once, and no worker takes
// it afterwards.
func TestAWebhookCommitmentOlderThanADayExpires(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	owed := onlyOne(t, commitmentsOwedTo(t, s, id))
	var expiresAt time.Time
	if err := s.db.QueryRow(`SELECT expires_at FROM outbound_intents WHERE id = $1`, owed.ID).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	if until := time.Until(expiresAt); until < 23*time.Hour || until > 25*time.Hour {
		t.Fatalf("a webhook commitment is owed for %s, want a day", until)
	}
	expiredBefore := terminalCount(t, "webhook", "expired")

	if _, err := s.db.Exec(`UPDATE outbound_intents SET expires_at = now() - interval '1 second' WHERE id = $1`, owed.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := s.ExpireDueIntents(context.Background(), outbound.FamilyWebhook, 10)
	if err != nil || len(expired) != 1 || expired[0].IntentID != owed.ID {
		t.Fatalf("expiry answered %+v (%v)", expired, err)
	}
	if got := statusOf(t, s, owed.ID); got != outbound.StatusExpired {
		t.Fatalf("the commitment is %s", got)
	}
	if got := terminalCount(t, "webhook", "expired") - expiredBefore; got != 1 {
		t.Fatalf("expired counted %v, want 1", got)
	}
	if lines := journalOf(t, s, owed.ID); !strings.HasPrefix(lines[len(lines)-1], "expired|") {
		t.Fatalf("the journal ends with %q", lines[len(lines)-1])
	}
	if leased := webhookClaims(t, s, 10); len(leased) != 0 {
		t.Fatalf("an expired commitment was claimed: %d", len(leased))
	}
	// Once is enough: there is nothing left to expire.
	if again, err := s.ExpireDueIntents(context.Background(), outbound.FamilyWebhook, 10); err != nil || len(again) != 0 {
		t.Fatalf("a second expiry answered %+v (%v)", again, err)
	}
}

// TestAWebhookAttemptWhoseWorkerVanishedIsDoubt: while the lease is live nobody
// else can finish the attempt; once it has lapsed, recovery closes the attempt
// as ambiguous and the family's policy puts the commitment back to be retried;
// and the vanished worker's late answer is recorded as an observation, not as a
// result.
func TestAWebhookAttemptWhoseWorkerVanishedIsDoubt(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	owed := onlyOne(t, commitmentsOwedTo(t, s, id))
	token, attemptID := leasedWebhook(t, s, owed.ID)
	ctx := context.Background()

	// Somebody else's token finishes nothing.
	stranger, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: attemptID, LeaseToken: "not-the-holder", Conclusion: webhookAccepted(),
	})
	if err != nil || stranger.Outcome == outbound.FinalizeFinalized {
		t.Fatalf("a stranger's finalize answered %+v (%v)", stranger, err)
	}
	if got := statusOf(t, s, owed.ID); got != outbound.StatusSending {
		t.Fatalf("after a stranger's finalize the commitment is %s", got)
	}

	// The worker never comes back: its lease lapses and recovery closes the attempt.
	if _, err := s.db.Exec(`UPDATE outbound_intents SET locked_until = now() - interval '1 second' WHERE id = $1`, owed.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RecoverStaleAttempts(ctx, outbound.FamilyWebhook, 10)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recovery answered %+v (%v)", recovered, err)
	}
	var outcome string
	if err := s.db.QueryRow(`SELECT outcome FROM outbound_attempts WHERE id = $1`, attemptID).Scan(&outcome); err != nil ||
		outcome != "ambiguous" {
		t.Fatalf("the abandoned attempt closed as %q (%v)", outcome, err)
	}
	var status string
	var streak int
	var lease *string
	var nextAt time.Time
	if err := s.db.QueryRow(`SELECT status, failure_streak, lease_token, next_attempt_at FROM outbound_intents WHERE id = $1`,
		owed.ID).Scan(&status, &streak, &lease, &nextAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || streak != 1 || lease != nil || !nextAt.After(time.Now()) {
		t.Fatalf("after recovery: status=%s streak=%d lease=%v next=%s; the family retries doubt with backoff",
			status, streak, lease, nextAt)
	}

	// The late answer of the vanished worker.
	late, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: attemptID, LeaseToken: token, Conclusion: webhookAccepted(),
	})
	if err != nil || late.Outcome != outbound.FinalizeLeaseLost || !late.ObservationRecorded {
		t.Fatalf("the late answer was taken as %+v (%v)", late, err)
	}
	var observations int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_attempt_observations WHERE attempt_id = $1`, attemptID).
		Scan(&observations); err != nil || observations != 1 {
		t.Fatalf("%d observations of the late answer (%v)", observations, err)
	}
	if got := statusOf(t, s, owed.ID); got != outbound.StatusPending {
		t.Fatalf("the late answer moved the commitment to %s", got)
	}
}

// TestAWebhookBacklogDoesNotDelayPaging: the claim is partitioned by family.
// Twenty webhook commitments due now do not stand between a page and its
// worker, and the webhook worker's claim holds no page.
func TestAWebhookBacklogDoesNotDelayPaging(t *testing.T) {
	s := setupTestDB(t)
	subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	for i := 0; i < 20; i++ {
		fannedOut(t, s)
	}
	agID := desiredGroup(t, s, "Disk filling up")
	page := admitOne(t, s, agID)[0]

	leased, err := s.ClaimDueIntents(context.Background(), outbound.ClaimRequest{
		Family: outbound.FamilyNotification, Provider: "slack", Phase: outbound.ClaimRetriesFirst,
		Limit: 10, Lease: outbound.NotificationLease, WorkerID: "pager",
	})
	if err != nil || len(leased) != 1 || leased[0].Intent.ID != page {
		t.Fatalf("the paging claim answered %d commitments (%v), want the page alone", len(leased), err)
	}
	webhooks := webhookClaims(t, s, 8)
	if len(webhooks) != 8 {
		t.Fatalf("the webhook claim took %d, want the pool of 8", len(webhooks))
	}
	for _, l := range webhooks {
		if l.Intent.Family != outbound.FamilyWebhook {
			t.Fatalf("the webhook claim took a %s commitment", l.Intent.Family)
		}
	}
}

// TestAcknowledgingTheAlertLeavesItsWebhookAlone: the acknowledgement withdraws
// what pages people about the alert; the event about the alert still goes to
// its subscribers, and the acknowledgement itself is fanned out after it.
// Webhook commitments belong to no alert group, so nothing that acts on a
// group - an acknowledgement, a merge - counts them as paging.
func TestAcknowledgingTheAlertLeavesItsWebhookAlone(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	agID := desiredGroup(t, s, "Disk filling up")
	page := admitOne(t, s, agID)[0]
	webhookEventFor(t, s, agID, "team-1", model.OutboxEventFiring,
		`{"event":"alert_group.firing","alert_group":{"id":"`+agID+`"}}`)
	firing := onlyOne(t, commitmentsOwedTo(t, s, id))
	canceledBefore := terminalCount(t, "webhook", "canceled")

	changed, err := s.AckAlertGroupAtomic(agID, "nina", nil, &model.OutboxEvent{
		ID: uuid.New().String(), EventType: model.OutboxEventAcknowledged, AlertGroupID: agID, TeamID: "team-1",
		Payload: json.RawMessage(`{"event":"alert_group.acknowledged","alert_group":{"id":"` + agID + `"}}`),
	})
	if err != nil || !changed {
		t.Fatalf("acknowledge: changed=%v err=%v", changed, err)
	}
	if got := statusOf(t, s, page); got != outbound.StatusCanceled {
		t.Fatalf("the page is %s after the acknowledgement", got)
	}
	if got := onlyOne(t, commitmentsOwedTo(t, s, id)); got.ID != firing.ID || got.Status != "pending" || got.Flagged {
		t.Fatalf("the acknowledgement touched the webhook: %+v", got)
	}
	if got := terminalCount(t, "webhook", "canceled"); got != canceledBefore {
		t.Fatalf("the acknowledgement counted %v webhook withdrawals", got-canceledBefore)
	}

	// And the acknowledgement is the next event the subscriber is owed.
	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found || result.Commitments != 1 {
		t.Fatalf("fanning out the acknowledgement: %+v (%v)", result, err)
	}
	if owed := commitmentsOwedTo(t, s, id); len(owed) != 2 {
		t.Fatalf("the subscriber is owed %d commitments, want the firing and the acknowledgement", len(owed))
	}
}

// TestASwappedWebhookPayloadStopsBeforeTheCall: a body changed on the row after
// admission still has the right shape and the right subscriber, and it is not
// what was promised. No attempt opens and nothing is sent; the commitment ends
// visibly, with the row named, as a broken row rather than as an incompatible
// build.
func TestASwappedWebhookPayloadStopsBeforeTheCall(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	owed := onlyOne(t, commitmentsOwedTo(t, s, id))
	if _, err := s.db.Exec(`UPDATE outbound_intents
		SET payload = jsonb_set(payload, '{body}', to_jsonb('{"event":"alert_group.firing","alert_group":{"id":"elsewhere"}}'::text))
		WHERE id = $1`, owed.ID); err != nil {
		t.Fatal(err)
	}

	leased := webhookClaims(t, s, 10)
	if len(leased) != 1 {
		t.Fatalf("claimed %d", len(leased))
	}
	failedBefore := terminalCount(t, "webhook", "permanent_failed")
	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: owed.ID, LeaseToken: leased[0].LeaseToken, WorkerID: "worker-w",
		Preparation: outbound.PreparationReady, BoundEndpoint: "https://example.com/hooks",
	})
	if err != nil {
		t.Fatalf("an attempt over a swapped payload answered an error rather than a refusal: %v", err)
	}
	if result.Outcome == outbound.BeginStarted {
		t.Fatal("an attempt was opened over a swapped payload")
	}
	if got := statusOf(t, s, owed.ID); got != outbound.StatusPermanentFailed {
		t.Fatalf("the commitment is %s, want it ended for good with the row named", got)
	}
	var attempts, refusals int
	var class string
	if err := s.db.QueryRow(`
		SELECT count(*) FILTER (WHERE record_kind = 'attempt'),
		       count(*) FILTER (WHERE record_kind = 'preparation'),
		       COALESCE(max(error_class) FILTER (WHERE record_kind = 'preparation'), '')
		FROM outbound_attempts WHERE intent_id = $1`, owed.ID).Scan(&attempts, &refusals, &class); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || refusals != 1 || class != "state_unreadable" {
		t.Fatalf("the journal holds %d attempts and %d refusals of class %q; want no attempt and one refusal naming the row",
			attempts, refusals, class)
	}
	if got := terminalCount(t, "webhook", "permanent_failed") - failedBefore; got != 1 {
		t.Fatalf("the ending was counted %v times", got)
	}
}

// TestErasingAPersonNamedInTheBodyLeavesTheBodyAlone: erasure acts on the
// recipients of commitments, and a subscriber is not a person. A body that
// happens to carry the erased person's name is the event as it happened; not a
// byte of it changes, its digest still holds, and it is still delivered.
func TestErasingAPersonNamedInTheBodyLeavesTheBodyAlone(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops")
	// The first user of a database is its administrator, and erasure refuses
	// the last one; alice is somebody else.
	for _, u := range []model.User{
		{ID: "root", Email: "root@example.com", Name: "Root"},
		{ID: "alice", Email: "alice@example.com", Name: "Alice"},
	} {
		if err := s.CreateUser(&u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddTeamMember("devops", "alice", model.TeamMemberRoleMember); err != nil {
		t.Fatal(err)
	}
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	agID := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "erasure-" + agID, Status: model.AlertGroupStatusNew, Title: "Disk", Severity: "critical",
		TeamID: "devops", Alerts: []model.Alert{{Fingerprint: "fp", Status: "firing"}},
	}); err != nil {
		t.Fatal(err)
	}
	webhookEventFor(t, s, agID, "devops", model.OutboxEventAcknowledged,
		`{"event":"alert_group.acknowledged","actor":{"name":"Alice","email":"alice@example.com"}}`)
	owed := onlyOne(t, commitmentsOwedTo(t, s, id))
	before, err := s.GetIntent(context.Background(), owed.ID)
	if err != nil || before == nil {
		t.Fatal(err)
	}

	// The whole erasure, through the service that runs it in production - the
	// step that marks and withdraws what the system owed the person included.
	if err := erasure.NewService(s.ErasureRepository()).Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("erase alice: %v", err)
	}

	after, err := s.GetIntent(context.Background(), owed.ID)
	if err != nil || after == nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Payload, after.Payload) || !strings.Contains(string(after.Payload), "alice@example.com") {
		t.Fatalf("erasure changed the body:\n  %s\n  %s", before.Payload, after.Payload)
	}
	if after.RecipientErased {
		t.Fatal("a subscriber was marked as an erased recipient")
	}
	digest, err := keys.PayloadDigest(after.KeyKind, after.PayloadSchemaVersion, after.Payload)
	if err != nil || !bytes.Equal(digest, after.PayloadDigest) {
		t.Fatalf("the digest no longer holds (%v)", err)
	}
	// Still deliverable: the attempt opens over the same bytes.
	leasedWebhook(t, s, owed.ID)
}
