package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Fanning an event out: the audience, the claim, the lock, and what is left
// alone.

// subscriber creates one generic webhook integration in the given scope.
func subscriber(t *testing.T, s *Store, name string, scope model.WebhookScope, teamID string,
	enabled bool) string {
	t.Helper()
	cfg, _ := json.Marshal(model.GenericWebhookConfig{URL: "https://example.com/" + name, Secret: "s"})
	integration := &model.Integration{
		Type: model.IntegrationTypeGenericWebhook, Name: name, Enabled: enabled,
		Scope: &scope, Config: cfg,
	}
	if scope == model.WebhookScopeTeam {
		integration.TeamID = &teamID
	}
	if err := s.CreateIntegration(integration); err != nil {
		t.Fatalf("create subscriber %s: %v", name, err)
	}
	return integration.ID
}

// alertEvent writes one event of the alert outbox, the way the atomic alert
// transactions do, for a group of the given team.
func alertEvent(t *testing.T, s *Store, teamID string, eventType model.OutboxEventType) string {
	t.Helper()
	agID := desiredGroup(t, s, "Disk filling up")
	event := &model.OutboxEvent{
		ID: uuid.New().String(), EventType: eventType, AlertGroupID: agID, TeamID: teamID,
		Payload: json.RawMessage(`{"event":"` + string(eventType) + `","alert_group":{"id":"` + agID + `"}}`),
	}
	if err := s.CreateOutboxEvent(event); err != nil {
		t.Fatalf("write the event: %v", err)
	}
	return event.ID
}

func eventStatus(t *testing.T, s *Store, id string) string {
	t.Helper()
	var status string
	if err := s.db.QueryRow(`SELECT status FROM event_outbox WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read the event: %v", err)
	}
	return status
}

func webhookAudienceOf(t *testing.T, s *Store, eventID string) map[string]bool {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT i.target_ref FROM outbound_intents i
		JOIN outbound_batches b ON b.id = i.batch_id
		WHERE b.batch_key = (SELECT batch_key FROM outbound_batches
		                     WHERE batch_key LIKE $1 AND key_kind = 'webhook_event')`, eventID+":%")
	if err != nil {
		t.Fatalf("read the audience: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[ref] = true
	}
	return got
}

// TestAnEventIsFannedOutToTheSubscribersInScope: a global subscriber and the
// team's own get a commitment each; a disabled one and another team's do not;
// the event is marked; the alert domain is not touched; and the commitments
// carry no alert group of their own.
func TestAnEventIsFannedOutToTheSubscribersInScope(t *testing.T) {
	s := setupTestDB(t)
	for _, team := range []string{"team-1", "team-2"} {
		if err := s.CreateTeam(&model.Team{ID: team, Name: team, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("create %s: %v", team, err)
		}
	}
	global := subscriber(t, s, "global", model.WebhookScopeGlobal, "", true)
	own := subscriber(t, s, "team-1", model.WebhookScopeTeam, "team-1", true)
	subscriber(t, s, "team-2", model.WebhookScopeTeam, "team-2", true)
	subscriber(t, s, "off", model.WebhookScopeGlobal, "", false)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	before := alertDomainState(t, s)

	result, err := s.FanOutNextEvent(context.Background())
	if err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if !result.Found || result.EventID != eventID || result.Outcome != outbound.SubmitCreated ||
		result.Commitments != 2 {
		t.Fatalf("fanned out as %+v", result)
	}
	if status := eventStatus(t, s, eventID); status != "fanned_out" {
		t.Fatalf("the event is %q afterwards", status)
	}
	audience := webhookAudienceOf(t, s, eventID)
	if !audience[global] || !audience[own] || len(audience) != 2 {
		t.Fatalf("audience %v, want the global subscriber and the team's own", audience)
	}
	if after := alertDomainState(t, s); after != before {
		t.Fatalf("fan-out touched the alert domain: %+v -> %+v", before, after)
	}
	var withGroup int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intents
		WHERE delivery_family = 'webhook' AND alert_group_id IS NOT NULL`).Scan(&withGroup); err != nil ||
		withGroup != 0 {
		t.Fatalf("%d webhook commitments belong to an alert group (%v)", withGroup, err)
	}

	// Nothing pending: the next call finds nothing and writes nothing.
	again, err := s.FanOutNextEvent(context.Background())
	if err != nil || again.Found {
		t.Fatalf("a second fan-out answered %+v (%v)", again, err)
	}
}

// TestTwoInstancesFanOutOneEventOnce: the lock arbitrates. Two concurrent
// fan-outs of one pending event produce one claim, one set of commitments and
// one marked event; the loser sees nothing pending and writes nothing.
func TestTwoInstancesFanOutOneEventOnce(t *testing.T) {
	s := setupTestDB(t)
	subscriber(t, s, "global", model.WebhookScopeGlobal, "", true)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)

	var wg sync.WaitGroup
	results := make([]outbound.FanOutResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.FanOutNextEvent(context.Background())
		}(i)
	}
	wg.Wait()

	found := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("instance %d: %v", i, errs[i])
		}
		if results[i].Found {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("%d instances fanned the event out", found)
	}
	var claims, commitments int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_batches WHERE batch_key LIKE $1`,
		eventID+":%").Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("%d claims for one event (%v)", claims, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intents WHERE delivery_family = 'webhook'`).
		Scan(&commitments); err != nil || commitments != 1 {
		t.Fatalf("%d commitments for one subscriber (%v)", commitments, err)
	}
	if status := eventStatus(t, s, eventID); status != "fanned_out" {
		t.Fatalf("the event is %q", status)
	}
}

// TestASubscriberCreatedBeforeTheFanOutIsInTheAudience: the audience is the
// subscribers at the moment of fan-out, not at the moment of the event. One
// created after the event and before the fan-out receives it.
func TestASubscriberCreatedBeforeTheFanOutIsInTheAudience(t *testing.T) {
	s := setupTestDB(t)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	late := subscriber(t, s, "late", model.WebhookScopeGlobal, "", true)

	if _, err := s.FanOutNextEvent(context.Background()); err != nil {
		t.Fatalf("fan out: %v", err)
	}
	if !webhookAudienceOf(t, s, eventID)[late] {
		t.Fatal("a subscriber created before the fan-out was left out of it")
	}
	// And one created afterwards is not: the audience was read once.
	subscriber(t, s, "later", model.WebhookScopeGlobal, "", true)
	if len(webhookAudienceOf(t, s, eventID)) != 1 {
		t.Fatal("the audience changed after the fan-out")
	}
}

// TestAnEventNobodySubscribedToIsFannedOutToNobody: a claim of no_targets, the
// event marked, nothing pending afterwards.
func TestAnEventNobodySubscribedToIsFannedOutToNobody(t *testing.T) {
	s := setupTestDB(t)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventResolved)

	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found || result.Outcome != outbound.SubmitCreated || result.Commitments != 0 {
		t.Fatalf("fanned out as %+v (%v)", result, err)
	}
	var outcome string
	if err := s.db.QueryRow(`SELECT admission_outcome FROM outbound_batches WHERE batch_key LIKE $1`,
		eventID+":%").Scan(&outcome); err != nil || outcome != "no_targets" {
		t.Fatalf("the claim says %q (%v)", outcome, err)
	}
	if status := eventStatus(t, s, eventID); status != "fanned_out" {
		t.Fatalf("the event is %q", status)
	}
	if again, err := s.FanOutNextEvent(context.Background()); err != nil || again.Found {
		t.Fatalf("a second fan-out answered %+v (%v)", again, err)
	}
}

// TestAnEventTheOldWorkerLeftInProcessingIsFannedOut: the previous worker's
// "processing" is a claim nobody holds any more, and the new fan-out takes it.
func TestAnEventTheOldWorkerLeftInProcessingIsFannedOut(t *testing.T) {
	s := setupTestDB(t)
	subscriber(t, s, "global", model.WebhookScopeGlobal, "", true)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventAcknowledged)
	if _, err := s.db.Exec(`UPDATE event_outbox SET status = 'processing', locked_by = 'old-worker',
		locked_until = now() + interval '1 hour' WHERE id = $1`, eventID); err != nil {
		t.Fatalf("leave the event as the old worker would: %v", err)
	}
	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found || result.Commitments != 1 {
		t.Fatalf("fanned out as %+v (%v)", result, err)
	}
	if status := eventStatus(t, s, eventID); status != "fanned_out" {
		t.Fatalf("the event is %q", status)
	}
}

// TestAnEventThisBuildCannotReadHoldsTheQueue: an event type outside the set
// is refused with the event named; it is not marked, not skipped, and nothing
// is written for it. The next fan-out meets it again.
func TestAnEventThisBuildCannotReadHoldsTheQueue(t *testing.T) {
	s := setupTestDB(t)
	subscriber(t, s, "global", model.WebhookScopeGlobal, "", true)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	if _, err := s.db.Exec(`UPDATE event_outbox SET event_type = 'alert_group.snoozed' WHERE id = $1`,
		eventID); err != nil {
		t.Fatalf("damage the event: %v", err)
	}

	for round := 1; round <= 2; round++ {
		result, err := s.FanOutNextEvent(context.Background())
		if err == nil {
			t.Fatalf("round %d: an event this build cannot read was fanned out: %+v", round, result)
		}
		if !result.Found || result.EventID != eventID || !result.Refused {
			t.Fatalf("round %d: the refusal does not name the event: %+v", round, result)
		}
		if !errors.Is(err, ErrOutboundContract) || !strings.Contains(err.Error(), eventID) {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if status := eventStatus(t, s, eventID); status != "pending" {
		t.Fatalf("the refused event is %q, want pending", status)
	}
	var claims int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_batches WHERE batch_key LIKE $1`,
		eventID+":%").Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("%d claims written for a refused event (%v)", claims, err)
	}
}

// TestAClaimAlreadyHeldUnderTheLockIsAContractViolation: the door answering
// existing or conflict for an event the fan-out holds the lock on means a second
// path reached the door. Refused by name, event untouched.
func TestAClaimAlreadyHeldUnderTheLockIsAContractViolation(t *testing.T) {
	s := setupTestDB(t)
	sub := subscriber(t, s, "global", model.WebhookScopeGlobal, "", true)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)

	// Somebody admitted this event's claim by another path.
	var body []byte
	if err := s.db.QueryRow(`SELECT payload FROM event_outbox WHERE id = $1`, eventID).Scan(&body); err != nil {
		t.Fatalf("read the body: %v", err)
	}
	held := webhookBatch(keys.KindWebhookEvent, eventID, sub)
	held.Body = string(body)
	if _, err := admitWebhook(t, s, held); err != nil {
		t.Fatalf("hold the claim: %v", err)
	}

	result, err := s.FanOutNextEvent(context.Background())
	if err == nil || !result.Refused || !errors.Is(err, ErrOutboundContract) {
		t.Fatalf("a claim already held was fanned out over: %+v (%v)", result, err)
	}
	if !strings.Contains(err.Error(), "existing") {
		t.Fatalf("refused, but not for the claim being held: %v", err)
	}
	if status := eventStatus(t, s, eventID); status != "pending" {
		t.Fatalf("the event is %q after a refused fan-out", status)
	}
}
