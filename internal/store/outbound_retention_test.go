package store

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The history of finished deliveries has a term. What the sweep removes, what
// it never touches, whom it makes way for, and what the doors answer once it
// has been through.

const day = 24 * time.Hour

// sweepOlderThan runs one chunk over everything older than the age.
func sweepOlderThan(t *testing.T, s *Store, age time.Duration) outbound.SweepResult {
	t.Helper()
	result, err := s.SweepDeliveryHistory(context.Background(), time.Now().Add(-age), 1000)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Busy {
		t.Fatal("the sweep found the lock held")
	}
	return result
}

func ageIntent(t *testing.T, s *Store, intentID string, age time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE outbound_intents SET updated_at = now() - $2::interval WHERE id = $1`,
		intentID, age.String()); err != nil {
		t.Fatalf("age %s: %v", intentID, err)
	}
}

func ageEvent(t *testing.T, s *Store, eventID string, age time.Duration) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE event_outbox SET fanned_out_at = now() - $2::interval WHERE id = $1`,
		eventID, age.String()); err != nil {
		t.Fatalf("age the event %s: %v", eventID, err)
	}
}

func countWhere(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// refusedForGood takes a paging commitment to permanent_failed through the
// worker's doors.
func refusedForGood(t *testing.T, s *Store, intentID string) {
	t.Helper()
	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "channel_not_found"),
	}); err != nil {
		t.Fatalf("finalize as refused: %v", err)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusPermanentFailed {
		t.Fatalf("the commitment is %s, want permanent_failed", got)
	}
}

// TestRetentionRemovesFinishedHistoryAndKeepsTheClaim: a commitment that
// failed for good, with two attempts, a late result on the first and its
// lifecycle events, goes whole once older than the window; the claim it came
// from stays as it was, and so does the alert's own history.
func TestRetentionRemovesFinishedHistoryAndKeepsTheClaim(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	group := outboundGroup(t, s)
	admitted := mustSubmit(t, s, outboundAdmission(t, group, "first", dmCommitment("U0001")))
	intentID := admitted.IntentIDs[0]

	// Attempt one is closed by recovery after its lease expired, and its
	// answer arrives afterwards - a late result beside a closed attempt.
	token := claimOne(t, s, intentID)
	first := beginOne(t, s, intentID, token)
	expireLease(t, s, intentID)
	if _, err := s.RecoverStaleAttempts(ctx, testFamily, 10); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: first.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomeRetryableRejection, "rate_limited"),
	}); err != nil {
		t.Fatalf("the late answer: %v", err)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_attempt_observations o
		JOIN outbound_attempts a ON a.id = o.attempt_id WHERE a.intent_id = $1`, intentID) != 1 {
		t.Fatal("the late answer left no observation; the fixture is not what it claims")
	}
	if _, err := s.db.Exec(`UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, intentID); err != nil {
		t.Fatal(err)
	}
	refusedForGood(t, s, intentID)

	attempts := countWhere(t, s, `SELECT count(*) FROM outbound_attempts WHERE intent_id = $1`, intentID)
	events := countWhere(t, s, `SELECT count(*) FROM outbound_intent_events WHERE intent_id = $1`, intentID)
	timeline := countWhere(t, s, `SELECT count(*) FROM timeline_events WHERE alert_group_id = $1`, group)
	if attempts < 2 || events < 2 || timeline == 0 {
		t.Fatalf("the fixture has %d attempts, %d events, %d timeline lines", attempts, events, timeline)
	}

	// Younger than the window: nothing goes.
	if got := sweepOlderThan(t, s, 30*day); got.Deleted != (outbound.SweepCounts{}) {
		t.Fatalf("a sweep took %+v from a commitment younger than the window", got.Deleted)
	}

	ageIntent(t, s, intentID, 31*day)
	got := sweepOlderThan(t, s, 30*day)
	if got.Deleted.Intents != 1 || got.Deleted.Attempts != int64(attempts) ||
		got.Deleted.Observations != 1 || got.Deleted.Events != int64(events) || got.Deleted.Outbox != 0 {
		t.Fatalf("the sweep reports %+v; want 1 commitment, %d attempts, 1 observation, %d events",
			got.Deleted, attempts, events)
	}
	for _, q := range []string{
		`SELECT count(*) FROM outbound_intents WHERE id = $1`,
		`SELECT count(*) FROM outbound_attempts WHERE intent_id = $1`,
		`SELECT count(*) FROM outbound_intent_events WHERE intent_id = $1`,
	} {
		if n := countWhere(t, s, q, intentID); n != 0 {
			t.Errorf("%d rows left by %s", n, q)
		}
	}
	// The claim stays, unchanged: it is what refuses the same work twice.
	var intentCount int
	if err := s.db.QueryRow(`SELECT intent_count FROM outbound_batches WHERE id = $1`, admitted.BatchID).
		Scan(&intentCount); err != nil || intentCount != 1 {
		t.Errorf("the claim reads intent_count %d (%v), want 1 and present", intentCount, err)
	}
	if n := countWhere(t, s, `SELECT count(*) FROM timeline_events WHERE alert_group_id = $1`, group); n != timeline {
		t.Errorf("the alert's history has %d lines after the sweep, had %d", n, timeline)
	}
	// And the journal is gone, with nothing to say but that.
	if journal, err := s.IntentJournal(ctx, intentID); err != nil || journal != nil {
		t.Errorf("the journal of a removed commitment reads %v, %v", journal, err)
	}
	// The same admission again is refused: the claim answers existing, with
	// nothing under it, and no second page starts.
	again, err := s.SubmitBatch(ctx, outboundAdmission(t, group, "first", dmCommitment("U0001")))
	if err != nil {
		t.Fatalf("admit the same work again: %v", err)
	}
	if again.Outcome != outbound.SubmitExisting || len(again.IntentIDs) != 0 {
		t.Errorf("the same admission after the sweep answered %+v, want existing with nothing under it", again)
	}
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1`, group); n != 0 {
		t.Errorf("%d commitments exist after the same admission was refused", n)
	}
}

// TestRetentionLeavesLiveWorkAlone: pending, sending, idle and manual_review
// commitments of any age stay, with all their history.
func TestRetentionLeavesLiveWorkAlone(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	group := outboundGroup(t, s)

	pending := admitOne(t, s, group, dmCommitment("U0001"))[0]
	sending := admitOne(t, s, outboundGroup(t, s), dmCommitment("U0002"))[0]
	beginOne(t, s, sending, claimOne(t, s, sending))
	waiting := stuckInReview(t, s, outboundGroup(t, s))

	// An editable card that reached its channel: it stays idle for as long as
	// the alert is open, whatever its age.
	card := admitOne(t, s, outboundGroup(t, s), channelCommitment("C0001", 0))[0]
	token := claimOne(t, s, card)
	begun := beginOne(t, s, card, token)
	if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: conclusion(outbound.ConclusionInput{
			Outcome: outbound.OutcomeAccepted, Status: "ok",
			Receipt: receiptOf("C0001/1700000000.000100", `{"channel":"C0001","ts":"1700000000.000100"}`),
		}),
	}); err != nil {
		t.Fatalf("deliver the card: %v", err)
	}
	live := map[string]outbound.Status{
		pending: outbound.StatusPending, sending: outbound.StatusSending,
		waiting: outbound.StatusManualReview, card: statusOf(t, s, card),
	}
	if live[card] != outbound.StatusIdle {
		t.Fatalf("the delivered card is %s, want idle", live[card])
	}
	for id := range live {
		ageIntent(t, s, id, 400*day)
	}
	before := countWhere(t, s, `SELECT count(*) FROM outbound_intent_events`)

	if got := sweepOlderThan(t, s, 30*day); got.Deleted != (outbound.SweepCounts{}) {
		t.Fatalf("the sweep took %+v from live work", got.Deleted)
	}
	for id, status := range live {
		if got := statusOf(t, s, id); got != status {
			t.Errorf("%s is %s after the sweep, was %s", id, got, status)
		}
	}
	if after := countWhere(t, s, `SELECT count(*) FROM outbound_intent_events`); after != before {
		t.Errorf("%d of %d lifecycle events of live work are gone", before-after, before)
	}
}

// TestRetentionHonoursTheBoundary: older than the cutoff goes, at the cutoff
// stays, younger stays.
func TestRetentionHonoursTheBoundary(t *testing.T) {
	s := setupTestDB(t)
	var ids []string
	for _, user := range []string{"U0001", "U0002", "U0003"} {
		id := admitOne(t, s, outboundGroup(t, s), dmCommitment(user))[0]
		refusedForGood(t, s, id)
		ids = append(ids, id)
	}
	cutoff := time.Now().Add(-30 * day).Truncate(time.Microsecond)
	for id, at := range map[string]time.Time{
		ids[0]: cutoff.Add(-time.Second), ids[1]: cutoff, ids[2]: cutoff.Add(time.Second),
	} {
		if _, err := s.db.Exec(`UPDATE outbound_intents SET updated_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.SweepDeliveryHistory(context.Background(), cutoff, 1000)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Deleted.Intents != 1 {
		t.Fatalf("the sweep took %d commitments, want the one older than the cutoff", result.Deleted.Intents)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1`, ids[0]) != 0 {
		t.Error("the commitment older than the cutoff stayed")
	}
	for _, id := range ids[1:] {
		if countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1`, id) != 1 {
			t.Errorf("%s, at or after the cutoff, is gone", id)
		}
	}
}

// TestRetentionChunks: 2500 finished commitments, chunk 1000 - three
// transactions and then nothing; the chunk is read through its index, and so
// is the event it looks for.
func TestRetentionChunks(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	// Five alerts of five hundred commitments, withdrawn by an acknowledgement:
	// canceled is terminal, and the acknowledgement is one door for all of
	// them.
	var groups []string
	for g := 0; g < 5; g++ {
		group := outboundGroup(t, s)
		groups = append(groups, group)
		commitments := make([]keys.EscalationCommitment, 0, 500)
		for i := 0; i < 500; i++ {
			commitments = append(commitments, dmCommitment(uuid.New().String()))
		}
		admitOne(t, s, group, commitments...)
		if _, err := s.AckAlertGroupAtomic(group, actorNamed("u-nina"), nil, nil); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
	}
	if _, err := s.db.Exec(`UPDATE outbound_intents SET updated_at = now() - interval '31 days'`); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE status = 'canceled'`); n != 2500 {
		t.Fatalf("%d canceled commitments, want 2500", n)
	}

	var removed []int64
	for i := 0; i < 5; i++ {
		result, err := s.SweepDeliveryHistory(ctx, time.Now().Add(-30*day), 1000)
		if err != nil {
			t.Fatalf("chunk %d: %v", i+1, err)
		}
		removed = append(removed, result.Deleted.Intents)
	}
	if want := []int64{1000, 1000, 500, 0, 0}; len(removed) != 5 || removed[0] != want[0] ||
		removed[1] != want[1] || removed[2] != want[2] || removed[3] != 0 || removed[4] != 0 {
		t.Fatalf("the chunks removed %v, want %v", removed, want)
	}
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_intents`); n != 0 {
		t.Errorf("%d commitments remain", n)
	}

	// The plans, with the sequential scan disallowed so that a small table
	// cannot hide a missing index.
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
	chunk := plan(`SELECT id FROM outbound_intents
		WHERE status IN (`+terminalStatusList+`) AND updated_at < $1
		ORDER BY updated_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`, time.Now(), 1000)
	if !strings.Contains(chunk, "idx_outbound_intents_retention") {
		t.Errorf("the chunk is not read through its index:\n%s", chunk)
	}
	doomed := plan(`SELECT e.id FROM event_outbox e
		WHERE e.status IN (`+finishedOutboxStatusList+`) AND e.fanned_out_at < $1
		  AND NOT EXISTS (SELECT 1 FROM outbound_batches b JOIN outbound_intents i ON i.batch_id = b.id WHERE b.event_id = e.id)
		ORDER BY e.fanned_out_at, e.id LIMIT $2 FOR UPDATE SKIP LOCKED`, time.Now(), 1000)
	if !strings.Contains(doomed, "idx_event_outbox_retention") {
		t.Errorf("the events are not read through their index:\n%s", doomed)
	}
	if !relationExists(t, s, "idx_event_outbox_retention") || !relationExists(t, s, "idx_outbound_intents_retention") {
		t.Error("a retention index is missing")
	}
}

// TestRetentionIsOnePassAtATime: two instances tick at once; the second finds
// the lock held and answers busy at once, without waiting for the lock
// timeout; what was removed adds up to one pass.
func TestRetentionIsOnePassAtATime(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() { afterSweepLock = nil })
	var groups, ids []string
	for _, user := range []string{"U0001", "U0002"} {
		group := outboundGroup(t, s)
		id := admitOne(t, s, group, dmCommitment(user))[0]
		refusedForGood(t, s, id)
		ageIntent(t, s, id, 31*day)
		groups = append(groups, group)
		ids = append(ids, id)
	}

	held, release := make(chan struct{}), make(chan struct{})
	afterSweepLock = func() {
		close(held)
		<-release
	}
	var first outbound.SweepResult
	var firstErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first, firstErr = s.SweepDeliveryHistory(ctx, time.Now().Add(-30*day), 1000)
	}()
	<-held
	afterSweepLock = nil

	started := time.Now()
	second, err := s.SweepDeliveryHistory(ctx, time.Now().Add(-30*day), 1000)
	if err != nil {
		t.Fatalf("the second instance: %v", err)
	}
	if !second.Busy || second.Deleted != (outbound.SweepCounts{}) {
		t.Fatalf("the second instance answered %+v, want busy with nothing removed", second)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("the second instance waited %s for the lock; busy is not a wait", waited)
	}
	close(release)
	wg.Wait()
	if firstErr != nil || first.Busy || first.Deleted.Intents != 2 {
		t.Fatalf("the first instance answered %+v, %v; want two commitments removed", first, firstErr)
	}
	for _, group := range groups {
		if n := countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1`, group); n != 0 {
			t.Errorf("%d commitments of %s remain after one pass", n, group)
		}
	}
}

// TestAnEventLivesWhileItsReplayLives: the event was fanned out a month ago;
// a replay two days ago holds it, whatever the fan-out's own commitment did.
// When the replay's result goes, the event goes with it.
func TestAnEventLivesWhileItsReplayLives(t *testing.T) {
	s := setupTestDB(t)
	teamOne(t, s)
	group := outboundGroup(t, s)
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	event := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	original := webhookCommitmentTo(t, s, a)
	replayed := replayThrough(t, s, a, original, "key-1")
	// The replay's commitment ends too - the subscriber is switched off once
	// more - so that only its age keeps it.
	off, on := false, true
	for _, enabled := range []*bool{&off, &on} {
		if _, err := s.UpdateIntegration(context.Background(), a, IntegrationPatch{Enabled: enabled}, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	if got := statusOf(t, s, replayed); got != outbound.StatusCanceled {
		t.Fatalf("the replay is %s, want canceled", got)
	}
	ageEvent(t, s, event, 31*day)
	ageIntent(t, s, original, 31*day)
	ageIntent(t, s, replayed, 2*day)

	got := sweepOlderThan(t, s, 30*day)
	if got.Deleted.Intents != 1 || got.Deleted.Outbox != 0 {
		t.Fatalf("the sweep reports %+v; want the fan-out's commitment gone and the event kept", got.Deleted)
	}
	if eventStatus(t, s, event) != string(model.OutboxEventStatusFannedOut) {
		t.Fatal("the event went while its replay lived")
	}

	ageIntent(t, s, replayed, 31*day)
	got = sweepOlderThan(t, s, 30*day)
	if got.Deleted.Intents != 1 || got.Deleted.Outbox != 1 {
		t.Fatalf("the sweep reports %+v; want the replay's commitment and the event gone", got.Deleted)
	}
	if countWhere(t, s, `SELECT count(*) FROM event_outbox WHERE id = $1`, event) != 0 {
		t.Error("the event stayed with nothing under it")
	}
	// The claims stay: the fan-out's and the replay's, both naming the event
	// that is gone, with no key to stop them.
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_batches WHERE event_id = $1`, event); n != 2 {
		t.Errorf("%d claims name the event, want 2", n)
	}
	if countWhere(t, s, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'outbound_batches'::regclass AND contype = 'f'
		  AND confrelid = 'event_outbox'::regclass`) != 0 {
		t.Error("a claim is keyed to the event it outlives")
	}
}

// TestRetentionKeepsAnEventWhoseWorkerStoodStill: an event fanned out forty
// days ago whose commitment is still pending is kept; a pending event of any
// age is untouched; and an event that waited forty days for its fan-out is as
// old as its fan-out, not as its creation.
func TestRetentionKeepsAnEventWhoseWorkerStoodStill(t *testing.T) {
	s := setupTestDB(t)
	teamOne(t, s)
	group := outboundGroup(t, s)
	subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)

	stood := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	ageEvent(t, s, stood, 40*day)

	waited := eventForGroup(t, s, group, "team-1", model.OutboxEventAcknowledged)
	if _, err := s.db.Exec(`UPDATE event_outbox SET created_at = now() - interval '40 days' WHERE id = $1`, waited); err != nil {
		t.Fatal(err)
	}
	fanOutNext(t, s)
	var fannedOutAt time.Time
	if err := s.db.QueryRow(`SELECT fanned_out_at FROM event_outbox WHERE id = $1`, waited).Scan(&fannedOutAt); err != nil {
		t.Fatalf("the fan-out did not date the event: %v", err)
	}
	if time.Since(fannedOutAt) > time.Minute {
		t.Fatalf("the event is dated %s, want the moment of its fan-out", fannedOutAt)
	}

	untaken := eventForGroup(t, s, group, "team-1", model.OutboxEventResolved)
	if _, err := s.db.Exec(`UPDATE event_outbox SET created_at = now() - interval '40 days' WHERE id = $1`, untaken); err != nil {
		t.Fatal(err)
	}

	if got := sweepOlderThan(t, s, 30*day); got.Deleted != (outbound.SweepCounts{}) {
		t.Fatalf("the sweep took %+v", got.Deleted)
	}
	for _, event := range []string{stood, waited, untaken} {
		if countWhere(t, s, `SELECT count(*) FROM event_outbox WHERE id = $1`, event) != 1 {
			t.Errorf("event %s is gone", event)
		}
	}
	if eventStatus(t, s, untaken) != string(model.OutboxEventStatusPending) {
		t.Error("the untaken event is no longer pending")
	}
}

// TestRetentionRemovesTheOldWorkersEvents: events the worker before this
// build finished carry no claim; dated by the upgrade, the old ones go and
// the young ones stay, and the hand-run drop afterwards finds nothing.
func TestRetentionRemovesTheOldWorkersEvents(t *testing.T) {
	s := setupTestDB(t)
	group := outboundGroup(t, s)
	old := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	young := eventForGroup(t, s, group, "team-1", model.OutboxEventResolved)
	failed := eventForGroup(t, s, group, "team-1", model.OutboxEventAcknowledged)
	for id, statement := range map[string]string{
		old:    `UPDATE event_outbox SET status = 'completed', sent_at = now() - interval '45 days' WHERE id = $1`,
		young:  `UPDATE event_outbox SET status = 'completed', sent_at = now() - interval '2 days' WHERE id = $1`,
		failed: `UPDATE event_outbox SET status = 'failed', created_at = now() - interval '45 days' WHERE id = $1`,
	} {
		if _, err := s.db.Exec(statement, id); err != nil {
			t.Fatal(err)
		}
	}
	// The upgrade dates them: when they were sent, else when they were made.
	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("the start: %v", err)
	}

	got := sweepOlderThan(t, s, 30*day)
	if got.Deleted.Outbox != 2 {
		t.Fatalf("the sweep took %d old events, want the completed and the failed one", got.Deleted.Outbox)
	}
	if countWhere(t, s, `SELECT count(*) FROM event_outbox WHERE id = $1`, young) != 1 {
		t.Error("the young event is gone")
	}
	for _, id := range []string{old, failed} {
		if countWhere(t, s, `SELECT count(*) FROM event_outbox WHERE id = $1`, id) != 0 {
			t.Errorf("event %s stayed", id)
		}
	}

	// The hand-run drop, after the sweep: it removes the tables this build
	// does not have and finds no finished event older than the window - the
	// young one is not old, and nothing fails.
	script, err := os.ReadFile("../../migrations/drop-webhook-outbox.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// The comments first, then the statements: a semicolon inside a comment
	// is prose, not a boundary.
	var kept []string
	for _, line := range strings.Split(string(script), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	for _, statement := range strings.Split(strings.Join(kept, "\n"), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		result, err := tx.Exec(statement)
		if err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		if strings.HasPrefix(strings.ToUpper(statement), "DELETE") {
			if n, _ := result.RowsAffected(); n != 1 {
				t.Errorf("the drop's DELETE removed %d rows, want only the young completed event it may still find", n)
			}
		}
	}
	tx.Rollback()
}

// TestRetentionAgainstTheOperator: a person reviving an expired commitment
// while the sweep removes it. In either order exactly one of two things is
// true afterwards: the commitment is pending and the next pass leaves it, or
// it is gone and the person is told so. The loser is told busy or not found,
// never anything worse.
func TestRetentionAgainstTheOperator(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() {
		afterSweepLock = nil
		afterOperatorLock = nil
	})
	previous := s.lockTimeout
	s.lockTimeout = outbound.OutboundLockTimeout
	t.Cleanup(func() { s.lockTimeout = previous })

	expiredCommitment := func() string {
		id := admitOne(t, s, outboundGroup(t, s), dmCommitment("U0001"))[0]
		if _, err := s.db.Exec(`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ExpireDueIntents(ctx, testFamily, 10); err != nil {
			t.Fatalf("expire: %v", err)
		}
		ageIntent(t, s, id, 31*day)
		return id
	}
	later := time.Now().Add(time.Hour)
	revive := func(id string) (outbound.ResolveAmbiguityResult, error) {
		return s.ResolveAmbiguity(ctx, outbound.ResolveAmbiguityRequest{
			IntentID: id, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again",
			Actor: byUser("u-nina"), NewExpiresAt: &later,
		})
	}
	// The invariant, whichever won.
	settled := func(t *testing.T, id string, decision outbound.ResolveAmbiguityResult, decisionErr error) {
		t.Helper()
		exists := countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1`, id) == 1
		switch {
		case exists:
			if statusOf(t, s, id) != outbound.StatusPending {
				t.Fatalf("the commitment exists as %s: neither revived nor removed", statusOf(t, s, id))
			}
			if got := sweepOlderThan(t, s, 30*day); got.Deleted.Intents != 0 {
				t.Fatal("the next pass removed a commitment that was revived")
			}
			if decisionErr != nil && !errorsIsBusy(decisionErr) {
				t.Fatalf("the revival that won answered %v", decisionErr)
			}
		default:
			if decisionErr == nil && decision.Outcome == outbound.ResolveResolved {
				t.Fatal("the decision applied and the commitment is gone: both outcomes at once")
			}
			if decisionErr != nil && !errorsIsBusy(decisionErr) {
				t.Fatalf("the loser answered %v, want busy or not found", decisionErr)
			}
			if again, err := revive(id); err != nil || again.Outcome != outbound.ResolveNotFound {
				t.Fatalf("a repeat after the removal answered %+v, %v; want not_found", again, err)
			}
		}
	}

	t.Run("the sweep holds the row while the person decides", func(t *testing.T) {
		id := expiredCommitment()
		held, release := make(chan struct{}), make(chan struct{})
		afterSweepLock = func() { close(held); <-release }
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); sweepOlderThan(t, s, 30*day) }()
		<-held
		afterSweepLock = nil
		decision, err := revive(id)
		close(release)
		wg.Wait()
		settled(t, id, decision, err)
	})

	t.Run("the person holds the row while the sweep runs", func(t *testing.T) {
		id := expiredCommitment()
		held, release := make(chan struct{}), make(chan struct{})
		afterOperatorLock = func() { close(held); <-release }
		var decision outbound.ResolveAmbiguityResult
		var err error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); decision, err = revive(id) }()
		<-held
		afterOperatorLock = nil
		swept := sweepOlderThan(t, s, 30*day)
		close(release)
		wg.Wait()
		if swept.Deleted.Intents != 0 {
			t.Fatal("the sweep removed a commitment a person was holding")
		}
		settled(t, id, decision, err)
	})
}

func errorsIsBusy(err error) bool {
	return err == ErrCommitmentBusy || strings.Contains(err.Error(), ErrCommitmentBusy.Error())
}

// TestRetentionAgainstAReplay: a replay reading a finished delivery while the
// sweep removes it and the event under it. In either order exactly one of two
// things is true afterwards: the replay was admitted and the event kept, or
// nothing was made and the replay was told busy or not found. A claim naming
// an event that is gone is never made.
func TestRetentionAgainstAReplay(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	t.Cleanup(func() {
		afterSweepLock = nil
		beforeReplayAdmission = nil
	})
	previous := s.lockTimeout
	s.lockTimeout = outbound.OutboundLockTimeout
	t.Cleanup(func() { s.lockTimeout = previous })
	teamOne(t, s)

	// A finished delivery of a month-old event, ready to be swept.
	sweepable := func(name string) (integration, original, event string) {
		group := outboundGroup(t, s)
		integration = subscriber(t, s, name, model.WebhookScopeTeam, "team-1", true)
		event = eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
		fanOutNext(t, s)
		original = webhookCommitmentTo(t, s, integration)
		off, on := false, true
		for _, enabled := range []*bool{&off, &on} {
			if _, err := s.UpdateIntegration(ctx, integration, IntegrationPatch{Enabled: enabled}, "tester"); err != nil {
				t.Fatal(err)
			}
		}
		ageIntent(t, s, original, 31*day)
		ageEvent(t, s, event, 31*day)
		return integration, original, event
	}
	replay := func(integration, original, key string) (WebhookReplayResult, error) {
		return s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
			IntegrationID: integration, DeliveryID: original, ClientRequestID: key, Actor: byUser("tester"),
		})
	}
	settled := func(t *testing.T, integration, original, event string, result WebhookReplayResult, err error) {
		t.Helper()
		orphans := countWhere(t, s, `SELECT count(*) FROM outbound_batches b
			WHERE b.event_id = $1 AND NOT EXISTS (SELECT 1 FROM event_outbox e WHERE e.id = b.event_id)`, event)
		replays := countWhere(t, s, `SELECT count(*) FROM outbound_intents
			WHERE key_kind = 'webhook_replay' AND target_ref = $1`, integration)
		eventStays := countWhere(t, s, `SELECT count(*) FROM event_outbox WHERE id = $1`, event) == 1
		switch {
		case err == nil:
			if replays != 1 || !eventStays || orphans != 0 {
				t.Fatalf("the replay was admitted: %d replays, event kept %v, %d claims without their event",
					replays, eventStays, orphans)
			}
			if got := sweepOlderThan(t, s, 30*day); got.Deleted.Outbox != 0 {
				t.Fatal("the next pass removed the event under a live replay")
			}
		default:
			if err != ErrIntegrationBusy && err != ErrWebhookDeliveryNotFound {
				t.Fatalf("the replay that lost answered %v, want busy or not found", err)
			}
			if replays != 0 || orphans != 0 {
				t.Fatalf("the replay lost and still made %d commitments, %d claims without their event", replays, orphans)
			}
			if _, err := replay(integration, original, "again"); err != ErrWebhookDeliveryNotFound {
				t.Fatalf("a repeat after the removal answered %v, want not found", err)
			}
		}
	}

	t.Run("the replay holds the original while the sweep runs", func(t *testing.T) {
		integration, original, event := sweepable("a")
		held, release := make(chan struct{}), make(chan struct{})
		beforeReplayAdmission = func() { close(held); <-release }
		var result WebhookReplayResult
		var err error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); result, err = replay(integration, original, "key-1") }()
		<-held
		beforeReplayAdmission = nil
		swept := sweepOlderThan(t, s, 30*day)
		close(release)
		wg.Wait()
		if swept.Deleted.Intents != 0 || swept.Deleted.Outbox != 0 {
			t.Fatalf("the sweep took %+v from under a replay", swept.Deleted)
		}
		settled(t, integration, original, event, result, err)
	})

	t.Run("the sweep holds the original while the replay reads", func(t *testing.T) {
		integration, original, event := sweepable("b")
		held, release := make(chan struct{}), make(chan struct{})
		afterSweepLock = func() { close(held); <-release }
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); sweepOlderThan(t, s, 30*day) }()
		<-held
		afterSweepLock = nil
		result, err := replay(integration, original, "key-2")
		close(release)
		wg.Wait()
		settled(t, integration, original, event, result, err)
	})
}

// TestARepeatedKeyAfterRetentionIsGone: two replays of one event under keys A
// and B; the sweep removes A's result; A again is gone, B again is the same
// delivery. And once the event itself is gone the group no longer shows it,
// while the claims stay.
func TestARepeatedKeyAfterRetentionIsGone(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	teamOne(t, s)
	group := outboundGroup(t, s)
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	event := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	original := webhookCommitmentTo(t, s, a)
	replayA := replayThrough(t, s, a, original, "A")
	resultB, err := s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
		IntegrationID: a, DeliveryID: original, ClientRequestID: "B", Actor: byUser("tester"),
	})
	if err != nil {
		t.Fatalf("replay B: %v", err)
	}
	// Both results end: the subscriber is switched off once more.
	off, on := false, true
	for _, enabled := range []*bool{&off, &on} {
		if _, err := s.UpdateIntegration(ctx, a, IntegrationPatch{Enabled: enabled}, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	ageEvent(t, s, event, 31*day)
	ageIntent(t, s, original, 31*day)
	ageIntent(t, s, replayA, 31*day)
	if got := sweepOlderThan(t, s, 30*day); got.Deleted.Intents != 2 || got.Deleted.Outbox != 0 {
		t.Fatalf("the sweep reports %+v; want the original and A gone, the event kept by B", got.Deleted)
	}

	if _, err := s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
		IntegrationID: a, DeliveryID: resultB.DeliveryID, ClientRequestID: "A", Actor: byUser("tester"),
	}); err != ErrWebhookReplayRetired {
		t.Fatalf("A again answered %v, want retired", err)
	}
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE key_kind = 'webhook_replay'`); n != 1 {
		t.Fatalf("%d replay commitments after A was repeated, want B's alone", n)
	}
	again, err := s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
		IntegrationID: a, DeliveryID: resultB.DeliveryID, ClientRequestID: "B", Actor: byUser("tester"),
	})
	if err != nil || again.DeliveryID != resultB.DeliveryID {
		t.Fatalf("B again answered %+v, %v; want the same delivery", again, err)
	}

	// The event goes once B's result goes; the group's deliveries no longer
	// show it, the three claims stay, and A is still gone.
	ageIntent(t, s, resultB.DeliveryID, 31*day)
	if got := sweepOlderThan(t, s, 30*day); got.Deleted.Intents != 1 || got.Deleted.Outbox != 1 {
		t.Fatalf("the sweep reports %+v; want B's result and the event gone", got.Deleted)
	}
	deliveries, err := s.AlertGroupDeliveries(ctx, group)
	if err != nil {
		t.Fatalf("the group's deliveries: %v", err)
	}
	if len(deliveries.Events) != 0 {
		t.Errorf("the group still shows %d events after the event was removed", len(deliveries.Events))
	}
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_batches WHERE event_id = $1`, event); n != 3 {
		t.Errorf("%d claims name the event, want 3", n)
	}
	if _, err := s.ReplayWebhookDelivery(ctx, WebhookReplayRequest{
		IntegrationID: a, DeliveryID: resultB.DeliveryID, ClientRequestID: "A", Actor: byUser("tester"),
	}); err != ErrWebhookDeliveryNotFound {
		t.Fatalf("A again, with the original gone, answered %v", err)
	}
}

// TestTheUpgradeDatesFinishedEvents: on a database from before the column,
// the start dates an event this build fanned out by its claim, an event the
// old worker finished by when it was sent or made, and leaves a pending event
// undated.
func TestTheUpgradeDatesFinishedEvents(t *testing.T) {
	s := setupTestDB(t)
	teamOne(t, s)
	group := outboundGroup(t, s)
	subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	fanned := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	sent := eventForGroup(t, s, group, "team-1", model.OutboxEventAcknowledged)
	made := eventForGroup(t, s, group, "team-1", model.OutboxEventResolved)
	pending := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	for _, statement := range []string{
		`UPDATE event_outbox SET status = 'completed', sent_at = now() - interval '3 days', created_at = now() - interval '4 days' WHERE id = '` + sent + `'`,
		`UPDATE event_outbox SET status = 'failed', created_at = now() - interval '5 days' WHERE id = '` + made + `'`,
		`ALTER TABLE event_outbox DROP COLUMN IF EXISTS fanned_out_at`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the previous shape: %v", err)
		}
	}
	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("the start: %v", err)
	}
	dated := func(id string) *time.Time {
		var at *time.Time
		if err := s.db.QueryRow(`SELECT fanned_out_at FROM event_outbox WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	var admittedAt time.Time
	if err := s.db.QueryRow(`SELECT admitted_at FROM outbound_batches WHERE event_id = $1 AND key_kind = 'webhook_event'`,
		fanned).Scan(&admittedAt); err != nil {
		t.Fatal(err)
	}
	if at := dated(fanned); at == nil || !at.Equal(admittedAt) {
		t.Errorf("the fanned-out event is dated %v, want its claim's %v", at, admittedAt)
	}
	if at := dated(sent); at == nil || time.Since(*at) < 2*day || time.Since(*at) > 4*day {
		t.Errorf("the sent event is dated %v, want when it was sent", at)
	}
	if at := dated(made); at == nil || time.Since(*at) < 4*day || time.Since(*at) > 6*day {
		t.Errorf("the failed event is dated %v, want when it was made", at)
	}
	if at := dated(pending); at != nil {
		t.Errorf("the pending event is dated %v, want no date", at)
	}
	if !relationExists(t, s, "idx_event_outbox_retention") {
		t.Error("the outbox retention index did not arrive")
	}
}

// TestRetentionRemovesAnEventTheOldTableStillNames: a database from the
// release before, with the old worker's tables keyed to event_outbox and a
// row of theirs on an event the fan-out is yet to take. The start removes the
// key; the fan-out takes the event; the sweep removes it once old, and the
// old row is left an orphan rather than stopping every pass.
func TestRetentionRemovesAnEventTheOldTableStillNames(t *testing.T) {
	s := setupTestDB(t)
	teamOne(t, s)
	group := outboundGroup(t, s)
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	event := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	if _, err := s.db.Exec(`UPDATE event_outbox SET status = 'processing' WHERE id = $1`, event); err != nil {
		t.Fatal(err)
	}
	sprint3OutboundShape(t, s)
	if _, err := s.db.Exec(`INSERT INTO event_outbox_deliveries (id, event_id, integration_id, status)
		VALUES ($1, $2, $3, 'pending')`, uuid.New().String(), event, a); err != nil {
		t.Fatalf("write the old worker's row: %v", err)
	}
	if countWhere(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = 'event_outbox_deliveries_event_id_fkey'`) != 1 {
		t.Fatal("the previous shape has no key to remove")
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start: %v", err)
	}
	if countWhere(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = 'event_outbox_deliveries_event_id_fkey'`) != 0 {
		t.Fatal("the start left the key from the old table to the outbox")
	}

	fanOutNext(t, s)
	original := webhookCommitmentTo(t, s, a)
	off, on := false, true
	for _, enabled := range []*bool{&off, &on} {
		if _, err := s.UpdateIntegration(context.Background(), a, IntegrationPatch{Enabled: enabled}, "tester"); err != nil {
			t.Fatal(err)
		}
	}
	ageIntent(t, s, original, 31*day)
	ageEvent(t, s, event, 31*day)
	got := sweepOlderThan(t, s, 30*day)
	if got.Deleted.Intents != 1 || got.Deleted.Outbox != 1 {
		t.Fatalf("the sweep reports %+v; want the commitment and the event gone", got.Deleted)
	}
	if countWhere(t, s, `SELECT count(*) FROM event_outbox_deliveries WHERE event_id = $1`, event) != 1 {
		t.Error("the old worker's row did not survive as an orphan")
	}
}
