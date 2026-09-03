package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The webhook family's own door, and what goes through it.

func webhookBatch(kind keys.Kind, event string, subscribers ...string) keys.WebhookBatch {
	b := keys.WebhookBatch{
		Kind:               kind,
		EventID:            event,
		EventType:          keys.WebhookEventFiring,
		Body:               `{"event":"alert_group.firing","alert_group":{"id":"ag-1"}}`,
		IntegrationIDs:     subscribers,
		Expiry:             24 * time.Hour,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
	}
	if kind == keys.KindWebhookReplay {
		b.ClientRequestID = "req-1"
	}
	return b
}

// admitWebhook runs the door in a transaction of its own and commits it, the way
// a caller would - the door itself never owns one.
func admitWebhook(t *testing.T, s *Store, batch keys.WebhookBatch) (outbound.SubmitResult, error) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	result, err := admitWebhookTx(context.Background(), tx, batch, byUser("test"))
	if err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return result, nil
}

// TestAWebhookEventIsAdmittedThroughItsOwnDoor: one claim for the event, one
// commitment per subscriber, and every column that the family fixes is what
// the family says - read back from the rows, not from the value the door
// returned.
func TestAWebhookEventIsAdmittedThroughItsOwnDoor(t *testing.T) {
	s := setupTestDB(t)

	result, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-1", "int-b", "int-a"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if result.Outcome != outbound.SubmitCreated || len(result.IntentIDs) != 2 {
		t.Fatalf("admitted as %q with %d commitments", result.Outcome, len(result.IntentIDs))
	}

	var kind, family, outcome string
	var group, snapshot sql.NullString
	var count int
	var admittedAt time.Time
	if err := s.db.QueryRow(`
		SELECT key_kind, delivery_family, alert_group_id, admission_outcome, intent_count,
		       admission_snapshot::text, admitted_at
		FROM outbound_batches WHERE id = $1`, result.BatchID).
		Scan(&kind, &family, &group, &outcome, &count, &snapshot, &admittedAt); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if kind != "webhook_event" || family != "webhook" || outcome != "admitted" || count != 2 {
		t.Errorf("claim is %s/%s %s x%d", kind, family, outcome, count)
	}
	if group.Valid || snapshot.Valid {
		t.Error("a webhook claim carries an alert group or a frozen state")
	}

	rows, err := s.db.Query(`
		SELECT key_kind, delivery_family, provider, target_kind, target_ref, alert_group_id,
		       form, completion_mode, ambiguity_policy, status, payload_schema_version,
		       payload, payload_digest, not_before, next_attempt_at, expires_at, receipt_recorded
		FROM outbound_intents WHERE batch_id = $1 ORDER BY idempotency_key`, result.BatchID)
	if err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var kind, family, provider, targetKind, targetRef, form, completion, policy, status string
		var group sql.NullString
		var schema int
		var payload, digest []byte
		var notBefore, nextAttempt time.Time
		var expires sql.NullTime
		var receiptRecorded bool
		if err := rows.Scan(&kind, &family, &provider, &targetKind, &targetRef, &group,
			&form, &completion, &policy, &status, &schema, &payload, &digest,
			&notBefore, &nextAttempt, &expires, &receiptRecorded); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[targetRef] = true
		if kind != "webhook_event" || family != "webhook" || provider != keys.ProviderWebhook {
			t.Errorf("%s: %s/%s via %s", targetRef, kind, family, provider)
		}
		if targetKind != "subscriber" || group.Valid {
			t.Errorf("%s: addressed to a %s, group %v", targetRef, targetKind, group)
		}
		if form != "one_shot" || completion != "on_acceptance" || policy != "retry" || status != "pending" {
			t.Errorf("%s: %s %s %s %s", targetRef, form, completion, policy, status)
		}
		// Due at admission, and owed for a day from it. Both from the
		// database's clock, both read back.
		if !notBefore.Equal(admittedAt) || !nextAttempt.Equal(admittedAt) {
			t.Errorf("%s: due %v / %v, admitted %v", targetRef, notBefore, nextAttempt, admittedAt)
		}
		if !expires.Valid || !expires.Time.Equal(admittedAt.Add(24*time.Hour)) {
			t.Errorf("%s: expires %v, admitted %v", targetRef, expires, admittedAt)
		}
		decoded, err := keys.DecodeWebhookPayloadV1(schema, payload)
		if err != nil {
			t.Fatalf("%s: the stored payload does not read: %v", targetRef, err)
		}
		if decoded.Target.Ref != targetRef || decoded.EventID != "evt-1" {
			t.Errorf("%s: payload for %s about %s", targetRef, decoded.Target.Ref, decoded.EventID)
		}
		want, err := keys.PayloadDigest(keys.KindWebhookEvent, schema, payload)
		if err != nil || !bytes.Equal(want, digest) {
			t.Errorf("%s: digest %x, payload digests to %x (%v)", targetRef, digest, want, err)
		}
		if receiptRecorded {
			t.Errorf("%s: a receipt recorded before anything was sent", targetRef)
		}
	}
	if !seen["int-a"] || !seen["int-b"] {
		t.Fatalf("audience %v", seen)
	}

	var created int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM outbound_intent_events e
		JOIN outbound_intents i ON i.id = e.intent_id
		WHERE i.batch_id = $1 AND e.kind = 'created'`, result.BatchID).Scan(&created); err != nil {
		t.Fatalf("count the journal: %v", err)
	}
	if created != 2 {
		t.Errorf("%d journal entries for 2 commitments", created)
	}
}

// TestARepeatedWebhookAdmissionIsTheSameAdmission: the same event with the same
// audience is existing and names the same rows; a different audience under the
// same claim is a conflict, and the first set stands. Both are reachable at the
// door - the fan-out's lock is what makes them unreachable THERE.
func TestARepeatedWebhookAdmissionIsTheSameAdmission(t *testing.T) {
	s := setupTestDB(t)

	first, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-2", "int-a"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	again, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-2", "int-a"))
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if again.Outcome != outbound.SubmitExisting || again.BatchID != first.BatchID ||
		len(again.IntentIDs) != 1 || again.IntentIDs[0] != first.IntentIDs[0] {
		t.Fatalf("the repeat answered %+v, the first was %+v", again, first)
	}

	other, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-2", "int-a", "int-b"))
	if err != nil {
		t.Fatalf("different audience: %v", err)
	}
	if other.Outcome != outbound.SubmitConflict || other.BatchID != first.BatchID {
		t.Fatalf("a different audience under the same claim answered %+v", other)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_intents WHERE batch_id = $1`,
		first.BatchID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("the losing audience left %d commitments (%v)", count, err)
	}
}

// TestAnEventNobodySubscribedToIsAnAnswer: a claim of no_targets, no
// commitments, and a repeat that finds it.
func TestAnEventNobodySubscribedToIsAnAnswer(t *testing.T) {
	s := setupTestDB(t)

	result, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-3"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if result.Outcome != outbound.SubmitCreated || len(result.IntentIDs) != 0 {
		t.Fatalf("answered %+v", result)
	}
	var outcome string
	if err := s.db.QueryRow(`SELECT admission_outcome FROM outbound_batches WHERE id = $1`,
		result.BatchID).Scan(&outcome); err != nil || outcome != "no_targets" {
		t.Fatalf("the claim says %q (%v)", outcome, err)
	}
	again, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-3"))
	if err != nil || again.Outcome != outbound.SubmitExisting || again.BatchID != result.BatchID {
		t.Fatalf("the repeat answered %+v (%v)", again, err)
	}
}

// TestAReplayIsItsOwnClaim: a replay of an event to a subscriber who already
// has a commitment for it is a NEW commitment under a new claim, and the first
// is left as it is.
func TestAReplayIsItsOwnClaim(t *testing.T) {
	s := setupTestDB(t)

	first, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookEvent, "evt-4", "int-a"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	replay, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookReplay, "evt-4", "int-a"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Outcome != outbound.SubmitCreated || replay.BatchID == first.BatchID ||
		len(replay.IntentIDs) != 1 || replay.IntentIDs[0] == first.IntentIDs[0] {
		t.Fatalf("the replay answered %+v against %+v", replay, first)
	}
	var kind string
	if err := s.db.QueryRow(`SELECT key_kind FROM outbound_intents WHERE id = $1`,
		replay.IntentIDs[0]).Scan(&kind); err != nil || kind != "webhook_replay" {
		t.Fatalf("the replay commitment is a %q (%v)", kind, err)
	}
	// The same request again is the same replay.
	again, err := admitWebhook(t, s, webhookBatch(keys.KindWebhookReplay, "evt-4", "int-a"))
	if err != nil || again.Outcome != outbound.SubmitExisting || again.IntentIDs[0] != replay.IntentIDs[0] {
		t.Fatalf("the repeated replay answered %+v (%v)", again, err)
	}
}

// TestTheDoorRefusesWhatTheGrammarCannotSay: a bad input is refused before a
// row is written, and the refusal is a contract violation.
func TestTheDoorRefusesWhatTheGrammarCannotSay(t *testing.T) {
	s := setupTestDB(t)

	for name, spoil := range map[string]func(*keys.WebhookBatch){
		"an event type outside the set": func(b *keys.WebhookBatch) { b.EventType = "alert_group.snoozed" },
		"no body":                       func(b *keys.WebhookBatch) { b.Body = "" },
		"a subscriber named twice":      func(b *keys.WebhookBatch) { b.IntegrationIDs = []string{"int-a", "int-a"} },
		"a fan-out with a request id":   func(b *keys.WebhookBatch) { b.ClientRequestID = "req-1" },
		"a replay to two subscribers": func(b *keys.WebhookBatch) {
			b.Kind, b.ClientRequestID, b.IntegrationIDs = keys.KindWebhookReplay, "req-1", []string{"int-a", "int-b"}
		},
		"a replay with no request id": func(b *keys.WebhookBatch) { b.Kind = keys.KindWebhookReplay },
	} {
		t.Run(name, func(t *testing.T) {
			b := webhookBatch(keys.KindWebhookEvent, "evt-5", "int-a")
			spoil(&b)
			_, err := admitWebhook(t, s, b)
			if !errors.Is(err, ErrOutboundContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
			var count int
			if err := s.db.QueryRow(`SELECT count(*) FROM outbound_batches WHERE batch_key LIKE 'evt-5:%'`).
				Scan(&count); err != nil || count != 0 {
				t.Fatalf("%d claim(s) written by a refused admission (%v)", count, err)
			}
		})
	}
}

// TestSubmitBatchIsNotTheWebhookFamilysDoor: a ready-made webhook admission
// offered to SubmitBatch is refused by name, whichever context it claims, and
// nothing is written.
func TestSubmitBatchIsNotTheWebhookFamilysDoor(t *testing.T) {
	s := setupTestDB(t)

	admission, err := webhookBatch(keys.KindWebhookEvent, "evt-6", "int-a").Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	for name, ctx := range map[string]outbound.BatchContext{
		"as a handover":    outbound.AnnouncingShiftChange(),
		"as an escalation": outbound.EscalatingAlertGroup(outbound.EscalationContext{}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.SubmitBatch(context.Background(), outbound.Batch{
				Admission: admission, Context: ctx, Actor: byUser("test"),
			})
			if !errors.Is(err, ErrOutboundContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
			if !strings.Contains(err.Error(), "own doors") {
				t.Fatalf("refused, but not for being on the wrong path: %v", err)
			}
		})
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbound_batches WHERE batch_key LIKE 'evt-6:%'`).
		Scan(&count); err != nil || count != 0 {
		t.Fatalf("%d claim(s) written through the wrong door (%v)", count, err)
	}
}

// TestAWebhookAttemptIsADelivery: the verb of a webhook's first effect is
// deliver, an escalation's and a handover's is send, and a kind nobody knows is
// refused rather than sent. Pure - no database - because planAttempt is.
func TestAWebhookAttemptIsADelivery(t *testing.T) {
	for kind, want := range map[keys.Kind]outbound.Operation{
		keys.KindEscalation:       outbound.OperationSend,
		keys.KindEscalationReplay: outbound.OperationSend,
		keys.KindHandoff:          outbound.OperationSend,
		keys.KindWebhookEvent:     outbound.OperationDeliver,
		keys.KindWebhookReplay:    outbound.OperationDeliver,
	} {
		intent := outbound.Intent{ID: "i", KeyKind: kind, Form: outbound.FormOneShot}
		planned, err := planAttempt(intent, outbound.AttemptContent{})
		if err != nil || planned.Kind != outbound.AttemptCreate || planned.Operation != want {
			t.Errorf("%s: planned %+v (%v), want %s", kind, planned, err, want)
		}
		refused, err := refusalShape(intent)
		if err != nil || refused.Operation != want {
			t.Errorf("%s: refusal shaped %+v (%v), want %s", kind, refused, err, want)
		}
	}
	unknown := outbound.Intent{ID: "i", KeyKind: keys.Kind("something_newer"), Form: outbound.FormOneShot}
	if _, err := planAttempt(unknown, outbound.AttemptContent{}); !errors.Is(err, ErrOutboundContract) {
		t.Fatalf("an unknown kind was planned: %v", err)
	}
	if _, err := refusalShape(unknown); !errors.Is(err, ErrOutboundContract) {
		t.Fatalf("an unknown kind was shaped: %v", err)
	}
	if form := contentFormOf(keys.KindWebhookEvent); form != outbound.ContentPayload {
		t.Fatalf("a webhook draws from %q", form)
	}
}
