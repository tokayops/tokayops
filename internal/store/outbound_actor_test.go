package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// Who signs the journal. Every writer of this build signs with a kind and a
// reference; the lines a build before this one wrote are classified once, at
// the start, by the path that wrote them.

// signedBy is the last line of a kind in a commitment's journal: its actor and
// the actor's kind.
func signedBy(t *testing.T, s *Store, intentID, kind string) (actor, actorKind string) {
	t.Helper()
	var ref sql.NullString
	if err := s.db.QueryRow(`
		SELECT actor, actor_kind FROM outbound_intent_events
		WHERE intent_id = $1 AND kind = $2 ORDER BY seq DESC LIMIT 1`, intentID, kind).
		Scan(&ref, &actorKind); err != nil {
		t.Fatalf("read the %s line of %s: %v", kind, intentID, err)
	}
	return ref.String, actorKind
}

// TestEveryWriterSignsWithItsKind drives every writer of this build through
// its own door and reads what it signed: components by name, people by id,
// and not one line of the legacy kind.
func TestEveryWriterSignsWithItsKind(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	seedTeam(t, s, "devops", "alice", "bob")
	teamOne(t, s)

	// The engine admits; the worker binds the effect of the first attempt.
	group := outboundGroup(t, s)
	engineIntent := admitOne(t, s, group, dmCommitment("U0001"))[0]
	beginOne(t, s, engineIntent, claimOne(t, s, engineIntent))

	// The notifier admits an announcement.
	announced := mustSubmit(t, s, handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))).IntentIDs[0]

	// The fan-out admits; a person switches the subscriber off, which withdraws,
	// and replays, which admits again.
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	fanned := webhookCommitmentTo(t, s, a)
	replayed := replayThrough(t, s, a, fanned, "key-1")

	// A person acknowledges an alert, which withdraws what it owed.
	acked := outboundGroup(t, s)
	owedByAcked := admitOne(t, s, acked, dmCommitment("U0002"))[0]
	if _, err := s.AckAlertGroupAtomic(acked, actorNamed("u-nina"), nil, nil); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	// The sweep expires what is overdue.
	overdue := admitOne(t, s, outboundGroup(t, s), dmCommitment("U0003"))[0]
	if _, err := s.db.Exec(`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`,
		overdue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireDueIntents(ctx, testFamily, 10); err != nil {
		t.Fatalf("expire: %v", err)
	}

	// Erasure withdraws what was owed to the person being erased.
	toBob := admitOne(t, s, outboundGroup(t, s), dmForUser("bob", 0))[0]
	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob"); err != nil {
		t.Fatalf("erase bob: %v", err)
	}

	// A person decides about a commitment nobody could resolve.
	stuck := stuckInReview(t, s, outboundGroup(t, s))
	if got := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: stuck, Decision: outbound.DecisionCancel, Actor: byUser("u-nina"), Reason: "nobody is listening",
	}); got.Outcome != outbound.ResolveResolved {
		t.Fatalf("the decision answered %+v", got)
	}

	for _, tt := range []struct {
		intent, kind, actor, actorKind string
	}{
		{engineIntent, "created", "engine", "system"},
		{engineIntent, "effect_bound", "worker", "system"},
		{announced, "created", "notifier", "system"},
		{fanned, "created", "fanout", "system"},
		{fanned, "canceled", "tester", "user"},
		{replayed, "created", "tester", "user"},
		{owedByAcked, "canceled", "u-nina", "user"},
		{overdue, "expired", "worker", "system"},
		{toBob, "canceled", "erasure", "system"},
		{stuck, "operator_decision", "u-nina", "user"},
		{stuck, "canceled", "u-nina", "user"},
	} {
		actor, kind := signedBy(t, s, tt.intent, tt.kind)
		if actor != tt.actor || kind != tt.actorKind {
			t.Errorf("%s of %s is signed %s:%s, want %s:%s", tt.kind, tt.intent, kind, actor, tt.actorKind, tt.actor)
		}
	}

	// Which worker bound the effect is a fact of the line, not its author.
	var workerID sql.NullString
	if err := s.db.QueryRow(`SELECT detail->>'worker_id' FROM outbound_intent_events
		WHERE intent_id = $1 AND kind = 'effect_bound'`, engineIntent).Scan(&workerID); err != nil {
		t.Fatal(err)
	}
	if !workerID.Valid || workerID.String == "" {
		t.Error("the effect_bound line does not say which worker")
	}

	// Nothing this build writes is legacy: the kind has no constructor.
	var legacy int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intent_events WHERE actor_kind = 'legacy'`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Errorf("%d lines written by this build are legacy", legacy)
	}
}

// TestTheUpgradeClassifiesActorsByTheirWritePath: a journal in the shape
// before this sprint - no kind, and the actors that build wrote - is
// classified by (kind, key_kind, reason), never by what the actor is called.
// A person whose display name was "system", written by an acknowledgement,
// stays legacy; a component's name in a line an acknowledgement wrote stays
// legacy too.
func TestTheUpgradeClassifiesActorsByTheirWritePath(t *testing.T) {
	s := setupTestDB(t)
	teamOne(t, s)

	group := outboundGroup(t, s)
	escalation := admitOne(t, s, group, dmCommitment("U0001"))[0]
	announced := mustSubmit(t, s, handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))).IntentIDs[0]
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)
	eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	fanned := webhookCommitmentTo(t, s, a)
	replayed := replayThrough(t, s, a, fanned, "key-1")

	// The shape before this sprint: the column is gone, and with it the rule;
	// the lines are the ones that build wrote.
	for _, statement := range []string{
		`DELETE FROM outbound_intent_events`,
		`ALTER TABLE outbound_intent_events DROP COLUMN IF EXISTS actor_kind`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the previous shape: %v", err)
		}
	}
	str := func(v string) *string { return &v }
	rows := []struct {
		intent, kind        string
		reason, actor       *string
		wantKind, wantActor string
	}{
		{escalation, "created", nil, str("engine"), "system", "engine"},
		{announced, "created", nil, str("notifier"), "system", "notifier"},
		{fanned, "created", nil, str("fan-out"), "system", "fanout"},
		{replayed, "created", nil, str("u-raw"), "user", "u-raw"},
		{escalation, "effect_bound", str("the address and key of this generation are settled"), str("worker-7"), "system", "worker-7"},
		{escalation, "expired", str("the deadline passed before anything was sent"), nil, "system", "worker"},
		{escalation, "canceled", str("the alert was acknowledged"), str("system"), "legacy", "system"},
		{escalation, "canceled", str("the alert was acknowledged"), str("engine"), "legacy", "engine"},
		{escalation, "canceled", str("the alert was resolved"), str("user:u-other"), "legacy", "user:u-other"},
		{escalation, "cancellation_requested", str("the alert cleared"), str("system"), "system", "system"},
		{fanned, "canceled", str("the subscriber was deleted"), str("u-raw"), "user", "u-raw"},
		{fanned, "cancellation_requested", str("the subscriber was disabled"), str("u-raw"), "user", "u-raw"},
		{escalation, "canceled", str("the recipient was erased"), str("erasure"), "system", "erasure"},
		{escalation, "cancellation_requested", str("the recipient was erased; this send was already in flight"), str("erasure"), "system", "erasure"},
		{escalation, "canceled", str("the lease expired with an attempt in flight"), str("recovery"), "system", "recovery"},
		{escalation, "canceled", nil, str("worker"), "system", "worker"},
		{escalation, "desired_raised", str("merge"), str("system"), "system", "system"},
		{escalation, "desired_raised", str("ack"), str("system"), "legacy", "system"},
		{escalation, "operator_decision", str("cancel: nobody is listening"), str("nina"), "legacy", "nina"},
	}
	ids := make([]string, len(rows))
	seq := map[string]int{}
	for i, row := range rows {
		seq[row.intent]++
		ids[i] = uuid.New().String()
		if _, err := s.db.Exec(`
			INSERT INTO outbound_intent_events (id, intent_id, seq, kind, reason, actor)
			VALUES ($1, $2, $3, $4, $5, $6)`, ids[i], row.intent, seq[row.intent], row.kind, row.reason, row.actor); err != nil {
			t.Fatalf("write the old line %d: %v", i, err)
		}
	}

	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("the start refused the previous shape: %v", err)
	}

	for i, row := range rows {
		var actor sql.NullString
		var kind string
		if err := s.db.QueryRow(`SELECT actor, actor_kind FROM outbound_intent_events WHERE id = $1`, ids[i]).
			Scan(&actor, &kind); err != nil {
			t.Fatalf("read line %d: %v", i, err)
		}
		if kind != row.wantKind || actor.String != row.wantActor {
			t.Errorf("line %d (%s, reason %v, actor %v) is classified %s:%s, want %s:%s",
				i, row.kind, deref(row.reason), deref(row.actor), kind, actor.String, row.wantKind, row.wantActor)
		}
	}

	var notNull, validated bool
	if err := s.db.QueryRow(`SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'outbound_intent_events'::regclass AND attname = 'actor_kind'`).Scan(&notNull); err != nil || !notNull {
		t.Errorf("actor_kind is not required after the upgrade: %v", err)
	}
	if err := s.db.QueryRow(`SELECT convalidated FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'outbound_intent_events'::regclass`,
		outboundEventActorKindConstraint).Scan(&validated); err != nil || !validated {
		t.Errorf("the vocabulary of actor_kind is not enforced after the upgrade: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO outbound_intent_events (id, intent_id, seq, kind, actor, actor_kind)
		VALUES ($1, $2, 99, 'created', 'x', 'robot')`, uuid.New().String(), escalation); err == nil {
		t.Error("a kind outside the vocabulary was accepted")
	}
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}

// TestARefusalSaysWhy: a decision the guards refuse comes back with the words
// of the guard, so that the person can satisfy it - and nothing outside the
// store has to know the guards to explain them.
func TestARefusalSaysWhy(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// A new generation after an attempt whose fate is unknown needs the risk
	// of a duplicate accepted, on the record.
	stuck := stuckInReview(t, s, outboundGroup(t, s))
	got := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: stuck, Decision: outbound.DecisionRetryNewGeneration, Reason: "the channel is back",
	})
	if got.Outcome != outbound.ResolveInvalidDecision || got.Status != outbound.StatusManualReview ||
		got.Detail != "a new generation after an ambiguous attempt needs the duplicate risk accepted" {
		t.Errorf("a new generation without the flag answered %+v", got)
	}
	got = resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: stuck, Decision: outbound.DecisionRetryNewGeneration, Reason: "the channel is back",
		AcceptedDuplicateRisk: true,
	})
	if got.Outcome != outbound.ResolveResolved || got.Detail != "" ||
		got.Intent == nil || got.Intent.Status != got.Status {
		t.Errorf("a new generation with the flag answered %+v", got)
	}
	if got.Intent != nil && got.Intent.GenerationNo == 0 {
		t.Errorf("the decided commitment is still in generation 0: %+v", got.Intent)
	}

	// Retrying an expired commitment needs a new deadline.
	overdue := admitOne(t, s, outboundGroup(t, s), dmCommitment("U0002"))[0]
	if _, err := s.db.Exec(`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`, overdue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireDueIntents(ctx, testFamily, 10); err != nil {
		t.Fatalf("expire: %v", err)
	}
	got = resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: overdue, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again",
	})
	if got.Outcome != outbound.ResolveInvalidDecision || got.Detail != "retrying an expired commitment needs a new deadline" {
		t.Errorf("a retry of an expired commitment without a deadline answered %+v", got)
	}

	// The alert is over: nothing is retried for it, and the answer says so.
	over := outboundGroup(t, s)
	stuckOver := stuckInReview(t, s, over)
	moveGroup(t, s, over, model.AlertGroupStatusResolved)
	got = resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: stuckOver, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again",
	})
	if got.Outcome != outbound.ResolveBusinessClosed || got.Detail == "" {
		t.Errorf("a retry for an alert that is over answered %+v", got)
	}

	// The person is gone: a commitment that had failed for good before the
	// erasure is left in place by it, and nothing but cancel applies to it
	// afterwards - reviving it would need an address. The answer says so.
	seedTeam(t, s, "devops", "alice", "bob")
	gone := admitOne(t, s, outboundGroup(t, s), dmForUser("bob", 0))[0]
	token := claimOne(t, s, gone)
	begun := beginOne(t, s, gone, token)
	if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "channel_not_found"),
	}); err != nil {
		t.Fatalf("finalize as refused: %v", err)
	}
	if err := erasure.NewService(s.ErasureRepository()).Erase(ctx, "bob"); err != nil {
		t.Fatalf("erase bob: %v", err)
	}
	got = resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: gone, Decision: outbound.DecisionRetryNewGeneration, Reason: "try again",
	})
	if got.Outcome != outbound.ResolveRecipientErased || got.Detail == "" {
		t.Errorf("a decision about an erased recipient answered %+v", got)
	}
	if got = resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: gone, Decision: outbound.DecisionCancel, Reason: "the person is gone",
	}); got.Outcome != outbound.ResolveResolved {
		t.Errorf("cancel for an erased recipient answered %+v", got)
	}
}

// TestANewDeadlineIsJudgedAfterTheLock: the transaction opens, then waits
// for the row; now() is the moment it opened. A deadline a second ahead of
// that moment is past by the time the row is ours, and a check by now()
// would write it - the next sweep would expire the commitment again, and the
// history would say it was revived. The check is by clock_timestamp(), after
// the lock.
func TestANewDeadlineIsJudgedAfterTheLock(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	// The wait has to outlast the hold: the store's own timeout, not whatever
	// an earlier test left on the shared store.
	previous := s.lockTimeout
	s.lockTimeout = outbound.OutboundLockTimeout
	t.Cleanup(func() { s.lockTimeout = previous })
	overdue := admitOne(t, s, outboundGroup(t, s), dmCommitment("U0001"))[0]
	if _, err := s.db.Exec(`UPDATE outbound_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`, overdue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExpireDueIntents(ctx, testFamily, 10); err != nil {
		t.Fatalf("expire: %v", err)
	}

	// Somebody else holds the row for two seconds.
	held, released := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(released)
		tx, err := s.db.Begin()
		if err != nil {
			t.Error(err)
			close(held)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`SELECT id FROM outbound_intents WHERE id = $1 FOR UPDATE`, overdue); err != nil {
			t.Error(err)
		}
		close(held)
		time.Sleep(2 * time.Second)
	}()
	<-held

	soon := time.Now().Add(time.Second)
	got := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: overdue, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again", NewExpiresAt: &soon,
	})
	<-released
	if got.Outcome != outbound.ResolveInvalidDecision || got.Detail != "the new deadline is already past" ||
		got.Status != outbound.StatusExpired {
		t.Fatalf("a deadline that passed while waiting for the lock answered %+v", got)
	}
	if statusOf(t, s, overdue) != outbound.StatusExpired {
		t.Fatal("the commitment was revived with a deadline already past")
	}
	var decisions int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intent_events
		WHERE intent_id = $1 AND kind = 'operator_decision'`, overdue).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 0 {
		t.Fatalf("%d decisions were recorded for one that was refused", decisions)
	}

	// A deadline still ahead when the row is ours goes through.
	later := time.Now().Add(time.Hour)
	got = resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: overdue, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "again", NewExpiresAt: &later,
	})
	if got.Outcome != outbound.ResolveResolved || got.Status != outbound.StatusPending {
		t.Fatalf("a deadline ahead answered %+v", got)
	}
	// The answer carries the commitment as the decision left it - with the
	// new deadline and the new status - read in the same transaction, not
	// after it.
	if got.Intent == nil || got.Intent.ID != overdue || got.Intent.Status != outbound.StatusPending ||
		got.Intent.ExpiresAt == nil || got.Intent.ExpiresAt.Sub(later).Abs() > time.Millisecond {
		t.Fatalf("the decided commitment reads %+v", got.Intent)
	}
}
