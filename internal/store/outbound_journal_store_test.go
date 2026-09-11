package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The delivery journal read from the outside: a group's deliveries from its
// events, and the operational log over a period. Everything here goes through
// the doors the product uses - admission, fan-out, the lifecycle commands, the
// replay - and reads back through the two new reads.

// eventForGroup writes one alert event for an EXISTING group, the way the
// atomic alert transactions do, under the team the audience is resolved by.
func eventForGroup(t *testing.T, s *Store, groupID, teamID string, eventType model.OutboxEventType) string {
	t.Helper()
	event := &model.OutboxEvent{
		ID: uuid.New().String(), EventType: eventType, AlertGroupID: groupID, TeamID: teamID,
		Payload: []byte(`{"event":"` + string(eventType) + `","alert_group":{"id":"` + groupID + `"}}`),
	}
	if err := s.CreateOutboxEvent(event); err != nil {
		t.Fatalf("write the event: %v", err)
	}
	return event.ID
}

// teamOne is the team the subscribers here belong to.
func teamOne(t *testing.T, s *Store) {
	t.Helper()
	if err := s.CreateTeam(&model.Team{ID: "team-1", Name: "team-1", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create team-1: %v", err)
	}
}

// fanOutNext runs one fan-out and fails unless it found an event.
func fanOutNext(t *testing.T, s *Store) outbound.FanOutResult {
	t.Helper()
	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found {
		t.Fatalf("fan out: %+v (%v)", result, err)
	}
	return result
}

// webhookCommitmentTo is the one webhook commitment owed to a subscriber.
func webhookCommitmentTo(t *testing.T, s *Store, integrationID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM outbound_intents
		WHERE delivery_family = 'webhook' AND target_ref = $1 AND key_kind = 'webhook_event'`,
		integrationID).Scan(&id); err != nil {
		t.Fatalf("find the commitment to %s: %v", integrationID, err)
	}
	return id
}

// replayThrough ends a subscriber's pending delivery the way an operator can -
// switching the subscriber off withdraws it - switches it back on and replays
// the delivery under a key. What comes back is the replay's own commitment,
// under its own claim on the same event.
func replayThrough(t *testing.T, s *Store, integrationID, deliveryID, key string) string {
	t.Helper()
	ctx := context.Background()
	off, on := false, true
	if _, err := s.UpdateIntegration(ctx, integrationID, IntegrationPatch{Enabled: &off}, "tester"); err != nil {
		t.Fatalf("switch %s off: %v", integrationID, err)
	}
	if _, err := s.UpdateIntegration(ctx, integrationID, IntegrationPatch{Enabled: &on}, "tester"); err != nil {
		t.Fatalf("switch %s on: %v", integrationID, err)
	}
	result, err := s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
		IntegrationID: integrationID, DeliveryID: deliveryID, ClientRequestID: key, Actor: byUser("tester"),
	})
	if err != nil {
		t.Fatalf("replay %s: %v", deliveryID, err)
	}
	return result.DeliveryID
}

// TestTheGroupsDeliveriesStartFromItsEvents: the paging the group owns, and
// every claim on every event of the group - the fan-out's with two deliveries,
// a replay's with one under the same event, a fan-out that found nobody, and
// an event nobody has reached yet. The obligation-centred form could show
// only the first of those.
func TestTheGroupsDeliveriesStartFromItsEvents(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	group := outboundGroup(t, s)
	teamOne(t, s)
	paging := mustSubmit(t, s, outboundAdmission(t, group, "first",
		channelCommitment("C0001", 0), channelCommitment("C0002", 5*time.Minute)))
	if len(paging.IntentIDs) == 0 {
		t.Fatal("the escalation promised nobody")
	}

	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	b := subscriber(t, s, "b", model.WebhookScopeTeam, "team-1", true)

	fannedOut := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	if got := fanOutNext(t, s); got.EventID != fannedOut || got.Commitments != 2 {
		t.Fatalf("the first fan-out: %+v", got)
	}
	replayed := replayThrough(t, s, a, webhookCommitmentTo(t, s, a), "key-1")

	nobody := eventForGroup(t, s, group, "team-with-no-subscribers", model.OutboxEventAcknowledged)
	if got := fanOutNext(t, s); got.EventID != nobody || got.Commitments != 0 {
		t.Fatalf("the fan-out to nobody: %+v", got)
	}
	pending := eventForGroup(t, s, group, "team-1", model.OutboxEventResolved)

	got, err := s.AlertGroupDeliveries(ctx, group)
	if err != nil {
		t.Fatalf("read the group's deliveries: %v", err)
	}
	if len(got.Paging) != len(paging.IntentIDs) {
		t.Errorf("%d paging commitments shown, %d were admitted", len(got.Paging), len(paging.IntentIDs))
	}
	for _, p := range got.Paging {
		if p.AlertGroupID != group || p.Family != outbound.FamilyNotification {
			t.Errorf("a paging commitment of another group or family: %+v", p)
		}
	}
	if len(got.Events) != 3 {
		t.Fatalf("%d events shown, want 3", len(got.Events))
	}

	first := got.Events[0]
	if first.EventID != fannedOut || first.Status != string(model.OutboxEventStatusFannedOut) {
		t.Errorf("the fanned-out event reads %+v", first)
	}
	if len(first.Batches) != 2 {
		t.Fatalf("the fanned-out event has %d claims, want the fan-out's and the replay's", len(first.Batches))
	}
	byKind := map[keys.Kind]outbound.BatchDeliveries{}
	for _, batch := range first.Batches {
		byKind[batch.Kind] = batch
	}
	fan := byKind[keys.KindWebhookEvent]
	if fan.Outcome != string(keys.OutcomeAdmitted) || fan.IntentCount != 2 || len(fan.Deliveries) != 2 {
		t.Errorf("the fan-out's claim reads %+v", fan)
	}
	targets := map[string]bool{}
	for _, d := range fan.Deliveries {
		targets[d.TargetRef] = true
		if d.BatchID != fan.BatchID {
			t.Errorf("delivery %s of claim %s is shown under claim %s", d.ID, d.BatchID, fan.BatchID)
		}
	}
	if !targets[a] || !targets[b] {
		t.Errorf("the fan-out's deliveries go to %v, want %s and %s", targets, a, b)
	}
	replay := byKind[keys.KindWebhookReplay]
	if len(replay.Deliveries) != 1 || replay.Deliveries[0].ID != replayed || replay.Deliveries[0].TargetRef != a {
		t.Errorf("the replay's claim reads %+v", replay)
	}

	second := got.Events[1]
	if second.EventID != nobody || len(second.Batches) != 1 {
		t.Fatalf("the event nobody subscribed to reads %+v", second)
	}
	if second.Batches[0].Outcome != string(keys.OutcomeNoTargets) || second.Batches[0].IntentCount != 0 ||
		len(second.Batches[0].Deliveries) != 0 {
		t.Errorf("the claim that found nobody reads %+v", second.Batches[0])
	}

	third := got.Events[2]
	if third.EventID != pending || third.Status != string(model.OutboxEventStatusPending) || len(third.Batches) != 0 {
		t.Errorf("the event nobody reached reads %+v", third)
	}
}

// TestTheGroupsDeliveriesAreOneSnapshot: a fan-out committing between the read
// of the events and the read of the claims must not splice into the answer -
// an event still pending with a claim under it is a state the database was
// never in. Under REPEATABLE READ the answer is the moment of the first read.
func TestTheGroupsDeliveriesAreOneSnapshot(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { groupDeliveriesSeam = nil })

	group := outboundGroup(t, s)
	teamOne(t, s)
	subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	event := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)

	fannedOutDuringTheRead := false
	groupDeliveriesSeam = func(step int) {
		if step == 3 {
			fanOutNext(t, s)
			fannedOutDuringTheRead = true
		}
	}
	got, err := s.AlertGroupDeliveries(context.Background(), group)
	if err != nil {
		t.Fatalf("read the group's deliveries: %v", err)
	}
	if !fannedOutDuringTheRead {
		t.Fatal("the seam never ran")
	}
	if len(got.Events) != 1 || got.Events[0].EventID != event {
		t.Fatalf("the events read %+v", got.Events)
	}
	if got.Events[0].Status != string(model.OutboxEventStatusPending) || len(got.Events[0].Batches) != 0 {
		t.Fatalf("the answer spliced two moments: event %s with %d claim(s)",
			got.Events[0].Status, len(got.Events[0].Batches))
	}

	groupDeliveriesSeam = nil
	after, err := s.AlertGroupDeliveries(context.Background(), group)
	if err != nil {
		t.Fatalf("read again: %v", err)
	}
	if after.Events[0].Status != string(model.OutboxEventStatusFannedOut) || len(after.Events[0].Batches) != 1 {
		t.Fatalf("the fan-out that committed is not in the next read: %+v", after.Events[0])
	}
}

// TestTheGroupsDeliveriesAreFourStatementsHoweverManyClaims: every replay
// under a new key is one more claim on the same event, and claims are never
// deleted; the read must not grow with them, because it holds a REPEATABLE
// READ connection for as long as it runs. The seam is called once before each
// statement, so its record is the count.
func TestTheGroupsDeliveriesAreFourStatementsHoweverManyClaims(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { groupDeliveriesSeam = nil })

	group := outboundGroup(t, s)
	teamOne(t, s)
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	event := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	original := webhookCommitmentTo(t, s, a)
	replays := []string{replayThrough(t, s, a, original, "key-1")}
	for _, key := range []string{"key-2", "key-3"} {
		result, err := s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
			IntegrationID: a, DeliveryID: original, ClientRequestID: key, Actor: byUser("tester"),
		})
		if err != nil {
			t.Fatalf("replay under %s: %v", key, err)
		}
		replays = append(replays, result.DeliveryID)
	}

	var statements []int
	groupDeliveriesSeam = func(step int) { statements = append(statements, step) }
	got, err := s.AlertGroupDeliveries(ctx, group)
	if err != nil {
		t.Fatalf("read the group's deliveries: %v", err)
	}
	if want := []int{1, 2, 3, 4}; fmt.Sprint(statements) != fmt.Sprint(want) {
		t.Errorf("the read ran statements %v with four claims, want %v", statements, want)
	}

	if len(got.Events) != 1 || got.Events[0].EventID != event || len(got.Events[0].Batches) != 4 {
		t.Fatalf("the event reads %+v", got.Events)
	}
	seen := map[string]string{}
	for _, batch := range got.Events[0].Batches {
		if len(batch.Deliveries) != 1 {
			t.Errorf("claim %s (%s) shows %d deliveries, want 1", batch.BatchID, batch.Kind, len(batch.Deliveries))
			continue
		}
		d := batch.Deliveries[0]
		if d.BatchID != batch.BatchID {
			t.Errorf("delivery %s of claim %s is shown under claim %s", d.ID, d.BatchID, batch.BatchID)
		}
		seen[d.ID] = batch.BatchID
	}
	if seen[original] == "" {
		t.Error("the fan-out's delivery is not under its claim")
	}
	for _, id := range replays {
		if seen[id] == "" {
			t.Errorf("replay %s is not under its claim", id)
		}
	}
}

// TestTheJournalListsAPeriodNewestFirst: the operational log over the last day
// by default, or over the window asked for; narrowed by any of the filters,
// each of which is a closed vocabulary; newest first with a stable tie-break;
// and pages that add up to the total.
func TestTheJournalListsAPeriodNewestFirst(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	group := outboundGroup(t, s)
	teamOne(t, s)
	paging := mustSubmit(t, s, outboundAdmission(t, group, "first",
		channelCommitment("C0001", 0), channelCommitment("C0002", 5*time.Minute)))
	if _, err := s.SubmitBatch(ctx, handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))); err != nil {
		t.Fatalf("admit the announcement: %v", err)
	}
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	event := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	replayed := replayThrough(t, s, a, webhookCommitmentTo(t, s, a), "key-1")

	var all int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intents`).Scan(&all); err != nil {
		t.Fatal(err)
	}

	list := func(f IntentFilter, limit, offset int) ([]outbound.Intent, int) {
		t.Helper()
		intents, total, err := s.ListIntents(ctx, f, limit, offset)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		return intents, total
	}

	// The default period is the last day, and everything here was admitted
	// within it.
	if got, total := list(IntentFilter{}, 200, 0); total != all || len(got) != all {
		t.Errorf("the default period lists %d of %d (total %d)", len(got), all, total)
	}
	// A window that ended before the admissions, and one that starts after.
	hourAgo := time.Now().Add(-time.Hour)
	if _, total := list(IntentFilter{To: &hourAgo}, 200, 0); total != 0 {
		t.Errorf("a window that ended an hour ago lists %d", total)
	}
	inAnHour := time.Now().Add(time.Hour)
	if _, total := list(IntentFilter{From: &inAnHour}, 200, 0); total != 0 {
		t.Errorf("a window that starts in an hour lists %d", total)
	}

	// Each filter narrows to exactly its own.
	if got, _ := list(IntentFilter{Family: outbound.FamilyNotification}, 200, 0); len(got) != len(paging.IntentIDs) {
		t.Errorf("family=notification lists %d, want %d", len(got), len(paging.IntentIDs))
	}
	if got, _ := list(IntentFilter{Family: outbound.FamilyHandoff}, 200, 0); len(got) != 1 {
		t.Errorf("family=handoff lists %d, want 1", len(got))
	}
	if got, _ := list(IntentFilter{Provider: keys.ProviderWebhook}, 200, 0); len(got) != 2 {
		t.Errorf("provider=webhook lists %d, want the fan-out's and the replay's", len(got))
	}
	if got, _ := list(IntentFilter{Statuses: []outbound.Status{outbound.StatusCanceled}}, 200, 0); len(got) != 1 {
		t.Errorf("status=canceled lists %d, want the withdrawn original", len(got))
	}
	if got, _ := list(IntentFilter{TargetKind: keys.TargetSubscriber, TargetRef: a}, 200, 0); len(got) != 2 {
		t.Errorf("target a lists %d, want 2", len(got))
	}
	if got, _ := list(IntentFilter{AlertGroupID: group}, 200, 0); len(got) != len(paging.IntentIDs) {
		t.Errorf("alert_group_id lists %d, want the paging only", len(got))
	}
	byEvent, _ := list(IntentFilter{EventID: event}, 200, 0)
	if len(byEvent) != 2 {
		t.Fatalf("event_id lists %d, want the fan-out's and the replay's", len(byEvent))
	}
	found := false
	for _, i := range byEvent {
		if i.ID == replayed {
			found = true
		}
	}
	if !found {
		t.Error("event_id does not find the replay, whose claim has a different key")
	}
	// Filters intersect.
	if got, _ := list(IntentFilter{Family: outbound.FamilyWebhook, Statuses: []outbound.Status{outbound.StatusPending}}, 200, 0); len(got) != 1 {
		t.Errorf("webhook AND pending lists %d, want the replay", len(got))
	}

	// Newest first, ties broken by id descending, and pages that add up.
	first, total := list(IntentFilter{}, 2, 0)
	rest, _ := list(IntentFilter{}, 200, 2)
	if len(first) != 2 || len(rest) != total-2 {
		t.Fatalf("pages of %d and %d over %d", len(first), len(rest), total)
	}
	whole := append(first, rest...)
	for i := 1; i < len(whole); i++ {
		prev, cur := whole[i-1], whole[i]
		if cur.CreatedAt.After(prev.CreatedAt) || (cur.CreatedAt.Equal(prev.CreatedAt) && cur.ID > prev.ID) {
			t.Errorf("row %d (%s at %s) is out of order after %s at %s", i, cur.ID, cur.CreatedAt, prev.ID, prev.CreatedAt)
		}
	}
}

// TestTheJournalReadsThroughItsIndexes: the plans, with the sequential scan
// disallowed so that a small table cannot hide a missing index behind the
// planner's preference for reading it whole.
func TestTheJournalReadsThroughItsIndexes(t *testing.T) {
	s := setupTestDB(t)
	group := outboundGroup(t, s)
	mustSubmit(t, s, outboundAdmission(t, group, "first", channelCommitment("C0001", 0)))

	plan := func(query string, args ...any) string {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		rows, err := tx.Query("EXPLAIN "+query, args...)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}

	journal := plan(outboundIntentColumns + ` FROM outbound_intents
		WHERE created_at >= now() - interval '24 hours' AND created_at < now()
		ORDER BY created_at DESC, id DESC LIMIT 50 OFFSET 0`)
	if !strings.Contains(journal, "idx_outbound_intents_journal") {
		t.Errorf("the period-only journal does not read through its index:\n%s", journal)
	}
	if strings.Contains(journal, "Sort") {
		t.Errorf("the period-only journal sorts instead of walking the index:\n%s", journal)
	}
	byBatch := plan(`SELECT id FROM outbound_intents WHERE batch_id = $1`, "batch-x")
	if !strings.Contains(byBatch, "idx_outbound_intents_batch") {
		t.Errorf("commitments by claim do not read through their index:\n%s", byBatch)
	}
	byEvent := plan(`SELECT id FROM outbound_batches WHERE event_id = ANY($1)`, "{evt-1}")
	if !strings.Contains(byEvent, "idx_outbound_batches_event") {
		t.Errorf("claims by event do not read through their index:\n%s", byEvent)
	}
}

// TestAWebhookClaimNamesItsEvent: the rule holds both ways - a webhook claim
// with no event and any other claim with one are refused by name.
func TestAWebhookClaimNamesItsEvent(t *testing.T) {
	s := setupTestDB(t)
	group := outboundGroup(t, s)

	insert := func(kind, family string, groupID, eventID any) error {
		_, err := s.db.Exec(`
			INSERT INTO outbound_batches
				(id, batch_key, key_kind, delivery_family, grammar_version, alert_group_id,
				 fingerprint, fingerprint_version, admission_outcome, intent_count, event_id)
			VALUES ($1, $2, $3, $4, 1, $5, $6, 1, 'no_targets', 0, $7)`,
			uuid.New().String(), "key-"+uuid.New().String(), kind, family, groupID, digest32(0x30), eventID)
		return err
	}
	if err := insert("webhook_event", "webhook", nil, nil); err == nil {
		t.Error("a webhook claim with no event was accepted")
	} else if !strings.Contains(err.Error(), outboundBatchNamesEventConstraint) {
		t.Errorf("refused, but not by the rule: %v", err)
	}
	if err := insert("webhook_replay", "webhook", nil, nil); err == nil {
		t.Error("a replay claim with no event was accepted")
	}
	if err := insert("handoff", "handoff", nil, "evt-1"); err == nil {
		t.Error("an announcement claim naming an event was accepted")
	} else if !strings.Contains(err.Error(), outboundBatchNamesEventConstraint) {
		t.Errorf("refused, but not by the rule: %v", err)
	}
	if err := insert("webhook_event", "webhook", nil, "evt-1"); err != nil {
		t.Errorf("a webhook claim naming its event was refused: %v", err)
	}
	_ = group
}

// TestTheUpgradeGivesOldClaimsTheirEvent: on a database written before the
// column existed, the start reads the event out of the key and checks it
// against the events table; a claim whose prefix names no event stops the
// start naming the row, and nothing is applied.
func TestTheUpgradeGivesOldClaimsTheirEvent(t *testing.T) {
	s := setupTestDB(t)

	previousShape := func() {
		t.Helper()
		// Dropping the column takes its index and its rule with it: the shape
		// before this sprint.
		if _, err := s.db.Exec(`ALTER TABLE outbound_batches DROP COLUMN IF EXISTS event_id`); err != nil {
			t.Fatalf("build the previous shape: %v", err)
		}
	}
	oldClaim := func(prefix string) string {
		t.Helper()
		id := uuid.New().String()
		if _, err := s.db.Exec(`
			INSERT INTO outbound_batches
				(id, batch_key, key_kind, delivery_family, grammar_version,
				 fingerprint, fingerprint_version, admission_outcome, intent_count)
			VALUES ($1, $2, 'webhook_event', 'webhook', 1, $3, 1, 'no_targets', 0)`,
			id, prefix+":"+strings.Repeat("ab", 32), digest32(0x40)); err != nil {
			t.Fatalf("write the old claim: %v", err)
		}
		return id
	}

	t.Run("a claim whose key names an event gets it", func(t *testing.T) {
		event := alertEvent(t, s, "team-1", model.OutboxEventFiring)
		previousShape()
		claim := oldClaim(event)
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("the start refused an upgradeable database: %v", err)
		}
		var got string
		if err := s.db.QueryRow(`SELECT event_id FROM outbound_batches WHERE id = $1`, claim).Scan(&got); err != nil {
			t.Fatalf("read the claim: %v", err)
		}
		if got != event {
			t.Errorf("the claim names event %q after the upgrade, want %q", got, event)
		}
		var validated bool
		if err := s.db.QueryRow(`SELECT convalidated FROM pg_constraint
			WHERE conname = $1 AND conrelid = 'outbound_batches'::regclass`,
			outboundBatchNamesEventConstraint).Scan(&validated); err != nil || !validated {
			t.Errorf("the rule did not arrive validated: %v", err)
		}
		for _, index := range []string{"idx_outbound_batches_event", "idx_outbound_intents_batch", "idx_outbound_intents_journal"} {
			if !relationExists(t, s, index) {
				t.Errorf("%s did not arrive", index)
			}
		}
	})

	t.Run("a claim whose key names no event stops the start", func(t *testing.T) {
		previousShape()
		ghost := oldClaim("ghost")
		err := s.applyOutboundSchema()
		if err == nil {
			t.Fatal("the start accepted a claim naming an event that does not exist")
		}
		if !strings.Contains(err.Error(), ghost) || !strings.Contains(err.Error(), `"ghost"`) {
			t.Errorf("the refusal does not name the row and the event: %v", err)
		}
		// Nothing was applied: the transaction is one.
		if relationExists(t, s, "idx_outbound_batches_event") {
			t.Error("the index arrived although the start was refused")
		}
		// And the way out is the one the message names: repair or remove the
		// row, after which the start goes through.
		if _, err := s.db.Exec(`DELETE FROM outbound_batches WHERE id = $1`, ghost); err != nil {
			t.Fatal(err)
		}
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("the start still refuses after the row was removed: %v", err)
		}
	})
}

// TestTheJournalEventsSayWhen: a line of the commitment's journal carries the
// moment it was written, from the row rather than from any clock of ours.
func TestTheJournalEventsSayWhen(t *testing.T) {
	s := setupTestDB(t)
	group := outboundGroup(t, s)
	before := time.Now().Add(-time.Minute)
	paging := mustSubmit(t, s, outboundAdmission(t, group, "first",
		channelCommitment("C0001", 0), channelCommitment("C0002", 5*time.Minute)))

	journal, err := s.IntentJournal(context.Background(), paging.IntentIDs[0])
	if err != nil || journal == nil {
		t.Fatalf("read the journal: %v", err)
	}
	if len(journal.Events) == 0 {
		t.Fatal("no events in a fresh commitment's journal")
	}
	for _, e := range journal.Events {
		if e.At.IsZero() || e.At.Before(before) || e.At.After(time.Now().Add(time.Minute)) {
			t.Errorf("event %d (%s) is dated %s", e.Seq, e.Kind, e.At)
		}
	}
}
