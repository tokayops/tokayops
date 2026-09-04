package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a database from the previous version gets when this one starts.
//
// The tables are created with CREATE TABLE IF NOT EXISTS, which is exactly why
// a column added to that statement never reaches a database that already has
// the table. Every test that starts from a fresh schema is blind to this: the
// only way to see it is to build the old shape and then run the start-up.

// previousOutboundShape is the batches table as the version before this one
// wrote it: no frozen admission state at all.
func previousOutboundShape(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE outbound_batches DROP CONSTRAINT IF EXISTS ` +
			outboundAdmittedStateConstraint,
		`ALTER TABLE outbound_batches DROP CONSTRAINT IF EXISTS ` +
			outboundAdmittedStateShape,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundFormKnownConstraint,
		`ALTER TABLE outbound_batches
			DROP COLUMN IF EXISTS admission_snapshot,
			DROP COLUMN IF EXISTS admission_digest,
			DROP COLUMN IF EXISTS admission_schema_version,
			DROP COLUMN IF EXISTS admission_revision`,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundReceiptNameConstraint,
		`ALTER TABLE outbound_intents DROP COLUMN IF EXISTS receipt_ref`,

		// The commitments and their claims had no families to keep in step and
		// nothing remembered what a payload was admitted as. The key between a
		// claim and its commitments goes before the unique index it points at,
		// and the length rule goes with the column it is about.
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundIntentBatchIdentity,
		`ALTER TABLE outbound_batches DROP CONSTRAINT IF EXISTS ` +
			outboundBatchIdentityUnique,
		`ALTER TABLE outbound_batches DROP CONSTRAINT IF EXISTS ` +
			outboundBatchFamilyConstraint,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundIntentFamilyConstraint,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundHandoffTargetAgreement,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundHandoffPersonTarget,
		// Nor anything the webhook family brought.
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundWebhookSubscriberTarget,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundWebhookTargetAgreement,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundWebhookProviderConstraint,
		`ALTER TABLE outbound_intents DROP COLUMN IF EXISTS payload_digest`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the previous shape: %v", err)
		}
	}
}

// sprint3OutboundShape is the schema as the version before the webhook family
// left it: the family vocabulary held under its FIRST name, with escalation and
// handover in it and nothing else; no webhook rules; no subscriber index; no
// tombstones; and the old deliveries table still holding its foreign key to
// integrations.
//
// The distinction from previousOutboundShape matters: this is the shape on which
// the rename of the vocabulary constraint is the whole upgrade, and a test from
// a fresh schema cannot see it - there the constraint is created once under the
// new name and the guard never meets the old.
func sprint3OutboundShape(t *testing.T, s *Store) {
	t.Helper()
	const previousVocabulary = `(
		(key_kind IN ('escalation', 'escalation_replay') AND delivery_family = 'notification')
		OR (key_kind = 'handoff' AND delivery_family = 'handoff')
	)`
	for _, statement := range []string{
		`ALTER TABLE outbound_batches DROP CONSTRAINT IF EXISTS ` + outboundBatchFamilyConstraint,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` + outboundIntentFamilyConstraint,
		`ALTER TABLE outbound_batches ADD CONSTRAINT ` + outboundBatchFamilyConstraintV1 +
			` CHECK ` + previousVocabulary,
		`ALTER TABLE outbound_intents ADD CONSTRAINT ` + outboundIntentFamilyConstraintV1 +
			` CHECK ` + previousVocabulary,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` + outboundWebhookSubscriberTarget,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` + outboundWebhookTargetAgreement,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` + outboundWebhookProviderConstraint,
		`DROP INDEX IF EXISTS idx_outbound_intents_subscriber`,
		`DROP TABLE IF EXISTS integration_tombstones`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the Sprint 3 shape: %v", err)
		}
	}
	legacyWebhookWorker(t, s)
}

// legacyWebhookWorker is the old worker's two tables, in the exact form the
// previous release created them - with the foreign keys to event_outbox and
// integrations that the start removes. This build does not create them, so a
// shape that has them has to.
func legacyWebhookWorker(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE IF EXISTS event_outbox_delivery_attempts`,
		`DROP TABLE IF EXISTS event_outbox_deliveries`,
		`CREATE TABLE event_outbox_deliveries (
			id TEXT PRIMARY KEY,
			event_id TEXT NOT NULL REFERENCES event_outbox(id),
			integration_id TEXT NOT NULL REFERENCES integrations(id),
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INT NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ,
			last_http_status INT,
			last_error TEXT,
			request_payload TEXT,
			response_body_trunc TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			sent_at TIMESTAMPTZ,
			UNIQUE(event_id, integration_id)
		)`,
		`CREATE INDEX idx_delivery_retry ON event_outbox_deliveries (next_attempt_at)
			WHERE status IN ('pending', 'retry')`,
		`CREATE INDEX idx_delivery_event ON event_outbox_deliveries (event_id)`,
		`CREATE TABLE event_outbox_delivery_attempts (
			id TEXT PRIMARY KEY,
			delivery_id TEXT NOT NULL REFERENCES event_outbox_deliveries(id),
			attempt INT NOT NULL,
			http_status INT,
			error TEXT,
			response_body_trunc TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX idx_delivery_attempts_delivery ON event_outbox_delivery_attempts (delivery_id)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the old worker's tables: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{"event_outbox_delivery_attempts", "event_outbox_deliveries"} {
			if _, err := s.db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
				t.Errorf("remove the legacy %s: %v", table, err)
			}
		}
	})
}

func relationExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var present bool
	if err := s.db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&present); err != nil {
		t.Fatalf("look for %s: %v", name, err)
	}
	return present
}

// insertWebhookRows writes one webhook claim and one webhook commitment the way
// the store will, in raw SQL, so that a database can be asked whether it ACCEPTS
// the family rather than whether a constraint with the right name is present.
// The optional overrides are how the tests below make a row wrong in exactly
// one way.
func insertWebhookRows(s *Store, override func(batch, intent map[string]any)) error {
	batchID, intentID := uuid.New().String(), uuid.New().String()
	batch := map[string]any{
		"key_kind": "webhook_event", "delivery_family": "webhook",
		// A webhook claim names its event, and the schema insists on it.
		"event_id": "evt-1",
	}
	intent := map[string]any{
		"key_kind": "webhook_event", "delivery_family": "webhook",
		"provider": "webhook", "target_kind": "subscriber", "target_ref": "int-a",
		"payload_kind": "subscriber", "payload_ref": "int-a",
	}
	if override != nil {
		override(batch, intent)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`
		INSERT INTO outbound_batches
			(id, batch_key, key_kind, delivery_family, grammar_version,
			 fingerprint, fingerprint_version, admission_outcome, intent_count, event_id)
		VALUES ($1, $2, $3, $4, 1, $5, 1, 'admitted', 1, $6)`,
		batchID, "evt-"+batchID, batch["key_kind"], batch["delivery_family"], digest32(0x20),
		batch["event_id"]); err != nil {
		return err
	}
	payload := `{"target":{"kind":"` + intent["payload_kind"].(string) + `","ref":"` +
		intent["payload_ref"].(string) +
		`"},"event_id":"evt-1","event_type":"alert_group.firing","body":"{}"}`
	if _, err := tx.Exec(`
		INSERT INTO outbound_intents
			(id, batch_id, idempotency_key, delivery_family, key_kind,
			 grammar_version, provider, target_kind, target_ref,
			 form, completion_mode, ambiguity_policy, payload_schema_version,
			 payload, payload_digest, provider_key_codec_version, status,
			 not_before, next_attempt_at, expires_at)
		VALUES ($1, $2, $3, $4, $5,
			 1, $6, $7, $8,
			 'one_shot', 'on_acceptance', 'retry', 1,
			 $9::jsonb, decode(repeat('ab', 32), 'hex'), 1, 'pending',
			 now(), now(), now() + interval '24 hours')`,
		intentID, batchID, "int-"+intentID, intent["delivery_family"], intent["key_kind"],
		intent["provider"], intent["target_kind"], intent["target_ref"], payload); err != nil {
		return err
	}
	return tx.Commit()
}

// TestAStartLearnsTheWebhookFamilyOnAnUpgradedDatabase is the upgrade the
// fresh-schema tests cannot see. The family vocabulary is a CHECK, the CHECK is
// guarded by name, and a database that already has the old name would keep the
// old rule - refusing the first webhook row - unless the start removes it and
// adds the new one under a new name. Proven by INSERTING webhook rows, because
// the presence of a constraint with the right name proves nothing about what it
// accepts.
func TestAStartLearnsTheWebhookFamilyOnAnUpgradedDatabase(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	// Live escalations, so the vocabulary is validated against real rows.
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-ops", 0), dmCommitment("U-nina"))

	sprint3OutboundShape(t, s)
	if !hasNamedConstraint(t, s, "outbound_intents", outboundIntentFamilyConstraintV1) {
		t.Fatal("the Sprint 3 shape does not hold the vocabulary under its first name")
	}
	if err := insertWebhookRows(s, nil); err == nil {
		t.Fatal("the Sprint 3 shape accepted a webhook row: the test proves nothing")
	}
	if !hasNamedConstraint(t, s, "event_outbox_deliveries", "event_outbox_deliveries_integration_id_fkey") {
		t.Fatal("the Sprint 3 shape does not hold the foreign key this start removes")
	}

	// The whole start, twice: the second one has to change nothing.
	for round := 1; round <= 2; round++ {
		if err := s.InitDB(); err != nil {
			t.Fatalf("start %d against the Sprint 3 shape: %v", round, err)
		}
	}

	for _, gone := range []struct{ table, name string }{
		{"outbound_batches", outboundBatchFamilyConstraintV1},
		{"outbound_intents", outboundIntentFamilyConstraintV1},
		{"event_outbox_deliveries", "event_outbox_deliveries_integration_id_fkey"},
	} {
		if hasNamedConstraint(t, s, gone.table, gone.name) {
			t.Errorf("%s is still on %s after the start", gone.name, gone.table)
		}
	}
	for _, rule := range []struct{ table, name string }{
		{"outbound_batches", outboundBatchFamilyConstraint},
		{"outbound_intents", outboundIntentFamilyConstraint},
		{"outbound_intents", outboundWebhookSubscriberTarget},
		{"outbound_intents", outboundWebhookTargetAgreement},
		{"outbound_intents", outboundWebhookProviderConstraint},
	} {
		var validated bool
		if err := s.db.QueryRow(`SELECT convalidated FROM pg_constraint
			WHERE conname = $1 AND conrelid = $2::regclass`, rule.name, rule.table).Scan(&validated); err != nil {
			t.Errorf("%s did not arrive on %s: %v", rule.name, rule.table, err)
		} else if !validated {
			t.Errorf("%s is on %s but was never checked against its rows", rule.name, rule.table)
		}
	}
	if !relationExists(t, s, "integration_tombstones") {
		t.Error("the tombstones table did not arrive")
	}
	if !relationExists(t, s, "idx_outbound_intents_subscriber") {
		t.Error("the subscriber index did not arrive")
	}

	// The point of the whole test: a webhook claim and commitment go in.
	if err := insertWebhookRows(s, nil); err != nil {
		t.Fatalf("the upgraded database refuses the webhook family: %v", err)
	}
	// The vocabulary still refuses the pairings it exists to refuse.
	if err := insertWebhookRows(s, func(b, i map[string]any) {
		b["delivery_family"], i["delivery_family"] = "notification", "notification"
	}); err == nil {
		t.Error("a webhook kind in the notification partition was accepted")
	} else if !strings.Contains(err.Error(), "family_matches_kind_v2") {
		t.Errorf("refused, but not by the vocabulary: %v", err)
	}
	// And each of the three webhook rules bites, by behaviour, on the upgraded
	// database - not on a fresh one where CREATE TABLE could have carried them.
	for _, refusal := range []struct {
		name  string
		spoil func(b, i map[string]any)
	}{
		// Aimed at a person in BOTH places, so the agreement rule is satisfied
		// and the one about who a webhook is for is the one that answers.
		// Postgres checks constraints in name order, and the agreement rule's
		// name sorts first.
		{outboundWebhookSubscriberTarget, func(b, i map[string]any) {
			i["target_kind"], i["payload_kind"] = "user", "user"
		}},
		{outboundWebhookTargetAgreement, func(b, i map[string]any) { i["payload_ref"] = "int-b" }},
		{outboundWebhookProviderConstraint, func(b, i map[string]any) { i["provider"] = "slack" }},
	} {
		err := insertWebhookRows(s, refusal.spoil)
		if err == nil {
			t.Errorf("the upgraded database accepted a row %s exists to forbid", refusal.name)
		} else if !strings.Contains(err.Error(), refusal.name) {
			t.Errorf("expected %s to refuse the row, got: %v", refusal.name, err)
		}
	}
}

func hasColumn(t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	var present bool
	if err := s.db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_name = $1 AND column_name = $2)`,
		table, column).Scan(&present); err != nil {
		t.Fatalf("look for %s.%s: %v", table, column, err)
	}
	return present
}

func hasNamedConstraint(t *testing.T, s *Store, table, name string) bool {
	t.Helper()
	var present bool
	if err := s.db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM pg_constraint
		               WHERE conname = $1 AND conrelid = $2::regclass)`,
		name, table).Scan(&present); err != nil {
		t.Fatalf("look for %s on %s: %v", name, table, err)
	}
	return present
}

// TestAStartUpgradesADatabaseFromThePreviousVersion. The columns appear, the
// rules that describe them appear, and the claims already in the database are
// filled in rather than left as commitments that cannot be rendered.
func TestAStartUpgradesADatabaseFromThePreviousVersion(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	// A group with an escalation admitted by the version before this one.
	agID := desiredGroup(t, s, "Disk filling up")
	intentIDs := admitOne(t, s, agID, channelCommitment("C-ops", 0), dmCommitment("U-nina"))

	// Now take the schema back to what that version had, keeping the rows.
	previousOutboundShape(t, s)
	if hasColumn(t, s, "outbound_batches", "admission_snapshot") {
		t.Fatal("the previous shape still has the column this test is about")
	}

	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("start against the previous shape: %v", err)
	}

	for _, column := range []string{"admission_snapshot", "admission_digest",
		"admission_schema_version", "admission_revision"} {
		if !hasColumn(t, s, "outbound_batches", column) {
			t.Errorf("%s did not arrive", column)
		}
	}
	for table, name := range map[string]string{
		"outbound_batches": outboundAdmittedStateShape,
		"outbound_intents": outboundFormKnownConstraint,
	} {
		if !hasNamedConstraint(t, s, table, name) {
			t.Errorf("%s did not arrive on %s", name, table)
		}
	}
	if !hasNamedConstraint(t, s, "outbound_batches", outboundAdmittedStateConstraint) {
		t.Errorf("%s did not arrive", outboundAdmittedStateConstraint)
	}
	if !hasColumn(t, s, "outbound_intents", "receipt_ref") {
		t.Error("receipt_ref did not arrive")
	}
	if !hasNamedConstraint(t, s, "outbound_intents", outboundReceiptNameConstraint) {
		t.Errorf("%s did not arrive", outboundReceiptNameConstraint)
	}

	// The claim was filled in from the state it was admitted from - which is
	// the group's own snapshot, because nothing in the previous version could
	// move it off revision 0.
	var revision int64
	var digest, groupDigest []byte
	if err := s.db.QueryRow(`
		SELECT b.admission_revision, b.admission_digest, g.snapshot_digest
		FROM outbound_batches b
		JOIN outbound_group_snapshots g ON g.alert_group_id = b.alert_group_id
		WHERE b.alert_group_id = $1`, agID).Scan(&revision, &digest, &groupDigest); err != nil {
		t.Fatalf("read the repaired claim: %v", err)
	}
	if revision != 0 {
		t.Errorf("the claim says it was admitted at revision %d", revision)
	}
	if string(digest) != string(groupDigest) {
		t.Error("the claim was filled in with something other than what was admitted")
	}

	// And the commitments under it still deliver: the direct message renders
	// the state that was repaired rather than failing as unreadable.
	dm := intentIDs[0]
	for _, id := range intentIDs {
		var form string
		if err := s.db.QueryRow(`SELECT form FROM outbound_intents WHERE id = $1`, id).
			Scan(&form); err != nil {
			t.Fatalf("read the form: %v", err)
		}
		if form == string(outbound.FormOneShot) {
			dm = id
		}
	}
	token := claimOne(t, s, dm)
	begun := beginOne(t, s, dm, token)
	if begun.Outcome != outbound.BeginStarted {
		t.Fatalf("the repaired commitment answered %q", begun.Outcome)
	}
}

// TestAStartRefusesSnapshotsItCannotRender is the case the structural test
// above cannot see.
//
// A database from the version before this one holds snapshots with the alert's
// history in them, under a tag this protocol no longer has. Copying one into a
// claim would produce a commitment that parses as nothing readable - and
// repairing it is not possible, because the digest it was keyed against covered
// that field. The start says so instead of leaving it to be discovered by a
// page that failed.
func TestAStartRefusesSnapshotsItCannotRender(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, dmCommitment("U-nina"))

	previousOutboundShape(t, s)
	// The shape of a snapshot the previous version wrote.
	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = jsonb_set(snapshot, '{timeline}', '[]')
		WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("write the previous snapshot shape: %v", err)
	}
	// The remedy this refusal names, applied to one alert: the claim and what
	// it admitted go with the snapshot nobody can render. Without it every
	// later start in this process refuses for the same reason.
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM outbound_intent_events e USING outbound_intents i
			 WHERE e.intent_id = i.id AND i.alert_group_id = $1`,
			`DELETE FROM outbound_attempts a USING outbound_intents i
			 WHERE a.intent_id = i.id AND i.alert_group_id = $1`,
			`UPDATE outbound_intents SET current_attempt_id = NULL, status = 'canceled',
			 lease_token = NULL, locked_until = NULL WHERE alert_group_id = $1`,
			`DELETE FROM outbound_intents WHERE alert_group_id = $1`,
			`DELETE FROM outbound_batches WHERE alert_group_id = $1`,
			`DELETE FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		} {
			if _, err := s.db.Exec(statement, agID); err != nil {
				t.Fatalf("apply the remedy: %v", err)
			}
		}
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("put the schema back: %v", err)
		}
	})

	err := s.applyOutboundSchema()
	if err == nil {
		t.Fatal("a start accepted a database it cannot deliver from")
	}
	if !strings.Contains(err.Error(), "2026-08-25") {
		t.Fatalf("the refusal does not say what is wrong: %v", err)
	}
	// And it changed nothing: a refusal that half-applied would be worse than
	// the state it refused.
	if hasColumn(t, s, "outbound_batches", "admission_snapshot") {
		t.Error("the refusal added the columns anyway")
	}
}

// TestAStartRefusesAMessageItCannotName is the second thing an upgrade can
// meet that no amount of repair fixes.
//
// The previous version wrote the coordinates of a message and nothing else. A
// change to that message needs the name the channel gave it, and that name is
// the channel's to give: it is in the answer that came back, which is gone. So
// a card left over from that version could be updated by nobody, and the start
// says so rather than letting the first escalation discover it.
func TestAStartRefusesAMessageItCannotName(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, agID)

	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM outbound_intent_events e USING outbound_intents i
			 WHERE e.intent_id = i.id AND i.alert_group_id = $1`,
			`DELETE FROM outbound_attempts a USING outbound_intents i
			 WHERE a.intent_id = i.id AND i.alert_group_id = $1`,
			`UPDATE outbound_intents SET current_attempt_id = NULL
			 WHERE alert_group_id = $1`,
			`DELETE FROM outbound_intents WHERE alert_group_id = $1`,
		} {
			if _, err := s.db.Exec(statement, agID); err != nil {
				t.Fatalf("clear the unnameable message: %v", err)
			}
		}
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("put the schema back: %v", err)
		}
	})

	previousOutboundShape(t, s)
	if hasColumn(t, s, "outbound_intents", "receipt_ref") {
		t.Fatal("the previous shape still has the column this test is about")
	}

	err := s.applyOutboundSchema()
	if err == nil {
		t.Fatal("a start accepted a message no change could ever address")
	}
	if !strings.Contains(err.Error(), "1 message(s)") {
		t.Fatalf("the refusal does not say how much is wrong: %v", err)
	}
	// A refusal that half-applied would leave the column behind and pass on the
	// next start, with the same rows still unnameable.
	if hasColumn(t, s, "outbound_intents", "receipt_ref") {
		t.Error("the refusal added the column anyway")
	}
}

// TestAStartUpgradesADatabaseThatHasEscalatedNothing. The commonest case, and
// the one with no claims to decide about: the structure moves, and that is all.
func TestAStartUpgradesADatabaseThatHasEscalatedNothing(t *testing.T) {
	s := setupTestDB(t)

	previousOutboundShape(t, s)
	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("start against the previous shape: %v", err)
	}
	if !hasColumn(t, s, "outbound_batches", "admission_snapshot") {
		t.Error("the columns did not arrive")
	}
}

// TestTheRulesRefuseWhatTheyAreFor. The three statements added above are only
// worth adding if they refuse something.
func TestTheRulesRefuseWhatTheyAreFor(t *testing.T) {
	s := setupTestDB(t)
	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-ops", 0))

	t.Run("half of the frozen state", func(t *testing.T) {
		for _, column := range []string{"admission_snapshot", "admission_digest",
			"admission_schema_version", "admission_revision"} {
			if _, err := s.db.Exec(
				`UPDATE outbound_batches SET `+column+` = NULL WHERE alert_group_id = $1`,
				agID); err == nil {
				t.Errorf("a claim without %s was accepted", column)
			}
		}
	})

	t.Run("an escalation with none of it", func(t *testing.T) {
		if _, err := s.db.Exec(`
			UPDATE outbound_batches
			SET admission_snapshot = NULL, admission_digest = NULL,
			    admission_schema_version = NULL, admission_revision = NULL
			WHERE alert_group_id = $1`, agID); err == nil {
			t.Error("an escalation admitted from no state at all was accepted")
		}
	})

	t.Run("a form nobody delivers", func(t *testing.T) {
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET form = 'thread' WHERE alert_group_id = $1`,
			agID); err == nil {
			t.Error("a commitment in a form this build does not deliver was accepted")
		}
	})
}

// TestAFormThisBuildDoesNotDeliverStopsBeforeTheNetwork. The database refuses
// the row; this is the same refusal one row further on, for a build that meets
// one anyway - written by a newer version, or damaged.
func TestAFormThisBuildDoesNotDeliverStopsBeforeTheNetwork(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := admitOne(t, s, agID, channelCommitment("C-ops", 0))[0]
	token := claimOne(t, s, intentID)

	if _, err := s.db.Exec(
		`ALTER TABLE outbound_intents DROP CONSTRAINT ` + outboundFormKnownConstraint); err != nil {
		t.Fatalf("take the rule off: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET form = 'thread' WHERE id = $1`, intentID); err != nil {
		t.Fatalf("write the unknown form: %v", err)
	}
	// Put BOTH back before leaving. Restoring the row alone would leave the
	// rule off for the rest of the process, and every test after this one would
	// be checking a schema nothing runs on.
	t.Cleanup(func() {
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET form = $2 WHERE id = $1`,
			intentID, string(outbound.FormEditable)); err != nil {
			t.Fatalf("put the form back: %v", err)
		}
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("put the schema back: %v", err)
		}
		if !hasNamedConstraint(t, s, "outbound_intents", outboundFormKnownConstraint) {
			t.Fatal("the rule did not come back, and every later test runs without it")
		}
	})

	if _, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "C-ops",
	}); err == nil {
		t.Fatal("a form nobody knows opened an attempt")
	}

	// And nothing was written: refusing after an attempt exists would mean the
	// provider might already have been called.
	var attempts int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_attempts WHERE intent_id = $1`, intentID).
		Scan(&attempts); err != nil {
		t.Fatalf("count the attempts: %v", err)
	}
	if attempts != 0 {
		t.Fatalf("the refusal left %d journal records behind", attempts)
	}
}

// outboundRulesAddedWithTheDigest is every rule that arrives with the payload
// digest, named once so the upgrade tests and the idempotence test cannot drift
// apart about what "all of them" means.
var outboundRulesAddedWithTheDigest = []struct{ table, name string }{
	{"outbound_intents", outboundPayloadDigestLenConstraint},
	{"outbound_batches", outboundBatchFamilyConstraint},
	{"outbound_intents", outboundIntentFamilyConstraint},
	{"outbound_batches", outboundBatchIdentityUnique},
	{"outbound_intents", outboundIntentBatchIdentity},
	{"outbound_intents", outboundHandoffTargetAgreement},
	{"outbound_intents", outboundHandoffPersonTarget},
	{"outbound_intents", outboundWebhookSubscriberTarget},
	{"outbound_intents", outboundWebhookTargetAgreement},
	{"outbound_intents", outboundWebhookProviderConstraint},
}

// TestAStartFillsInThePayloadDigests.
//
// The column is NOT NULL and the table has rows, so the upgrade cannot be one
// statement: it adds the column empty, fills it with this build's own codec and
// only then makes it required. What this asserts is the end of that - every
// commitment carrying the digest of its own payload, and every rule that came
// with it in force and checked against the rows that were already there.
func TestAStartFillsInThePayloadDigests(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	admitOne(t, s, agID, channelCommitment("C-ops", 0), dmCommitment("U-nina"))

	// A second alert with one direct message and nothing else, for the rules
	// below that need a claim holding a single commitment.
	aloneID := desiredGroup(t, s, "Certificate expiring")
	admitOne(t, s, aloneID, dmCommitment("U-nina"))

	previousOutboundShape(t, s)
	if hasColumn(t, s, "outbound_intents", "payload_digest") {
		t.Fatal("the previous shape still has the column this test is about")
	}

	if err := s.applyOutboundSchema(); err != nil {
		t.Fatalf("start against the previous shape: %v", err)
	}

	// Every commitment, digested from the bytes on its own row. Compared
	// against a value computed here rather than against "not null", because a
	// backfill that wrote the same constant everywhere would pass that.
	rows, err := s.db.Query(`
		SELECT id, key_kind, payload_schema_version, payload, payload_digest
		FROM outbound_intents WHERE alert_group_id = $1`, agID)
	if err != nil {
		t.Fatalf("read the commitments back: %v", err)
	}
	defer rows.Close()

	filled := 0
	for rows.Next() {
		var id, kind string
		var schemaVersion int
		var payload, stored []byte
		if err := rows.Scan(&id, &kind, &schemaVersion, &payload, &stored); err != nil {
			t.Fatalf("scan a commitment: %v", err)
		}
		want, err := keys.PayloadDigest(keys.Kind(kind), schemaVersion, payload)
		if err != nil {
			t.Fatalf("digest the payload of %s: %v", id, err)
		}
		if !bytes.Equal(stored, want) {
			t.Errorf("%s carries %x, its payload digests to %x", id, stored, want)
		}
		filled++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	if filled != 2 {
		t.Fatalf("the upgrade looked at %d commitment(s), there were 2", filled)
	}

	// The rules, and the fact that each was checked against the rows already in
	// the table. A constraint added NOT VALID would be present and would have
	// let every row that was there through unexamined.
	for _, rule := range outboundRulesAddedWithTheDigest {
		var validated bool
		if err := s.db.QueryRow(`
			SELECT convalidated FROM pg_constraint
			WHERE conname = $1 AND conrelid = $2::regclass`,
			rule.name, rule.table).Scan(&validated); err != nil {
			t.Errorf("%s did not arrive on %s: %v", rule.name, rule.table, err)
			continue
		}
		if !validated {
			t.Errorf("%s is on %s but was never checked against its rows",
				rule.name, rule.table)
		}
	}

	// And the rules bite on the upgraded database, which is the only place they
	// were added by a later statement rather than by CREATE TABLE.
	//
	// The two handover rules are reached by WHICH commitment is moved, not by
	// the statement: both are aimed at key_kind = 'handoff' and the one about
	// who takes a shift refuses first, so a channel-addressed row can never
	// show whether the other one is there at all.
	channelIntent := intentAddressed(t, s, agID, "channel")
	// From a claim of its OWN. A claim moves with its whole set of
	// commitments, so a person-addressed one sharing a claim with a
	// channel-addressed one can never get past the rule about who takes a
	// shift, whichever of the two is named.
	userIntent := intentAddressed(t, s, aloneID, "user")

	// The claim moves with the commitment, because a commitment alone cannot:
	// the key between them would refuse it before either handover rule was
	// reached.
	const becomeAHandover = `
		WITH claim AS (
			UPDATE outbound_batches SET key_kind = 'handoff', delivery_family = 'handoff'
			WHERE id = (SELECT batch_id FROM outbound_intents WHERE id = $1)
			RETURNING id
		)
		UPDATE outbound_intents SET key_kind = 'handoff', delivery_family = 'handoff'
		WHERE batch_id IN (SELECT id FROM claim)`

	for _, refusal := range []struct {
		name, statement, intentID string
	}{
		{outboundPayloadDigestLenConstraint, `UPDATE outbound_intents
			SET payload_digest = decode(repeat('ab', 31), 'hex') WHERE id = $1`, userIntent},
		{outboundIntentFamilyConstraint, `UPDATE outbound_intents
			SET delivery_family = 'handoff' WHERE id = $1`, userIntent},
		// Addressed to a channel: a shift is taken by a person.
		{outboundHandoffPersonTarget, becomeAHandover, channelIntent},
		// Addressed to a person, so the rule above is satisfied, and the
		// payload made to name somebody else. Two answers to "who is this
		// message for", which is the whole of this rule: the columns decide
		// where it is sent and the payload decides what it says, so a row like
		// this greets one person by another's name.
		{outboundHandoffTargetAgreement, `
			WITH claim AS (
				UPDATE outbound_batches SET key_kind = 'handoff', delivery_family = 'handoff'
				WHERE id = (SELECT batch_id FROM outbound_intents WHERE id = $1)
				RETURNING id
			)
			UPDATE outbound_intents
			SET key_kind = 'handoff', delivery_family = 'handoff',
			    payload = jsonb_set(payload, '{target,ref}', '"U-somebody-else"')
			WHERE batch_id IN (SELECT id FROM claim)`, userIntent},
	} {
		if _, err := s.db.Exec(refusal.statement, refusal.intentID); err == nil {
			t.Errorf("the upgraded database accepted a row %s exists to forbid", refusal.name)
		} else if !strings.Contains(err.Error(), refusal.name) {
			t.Errorf("expected %s to refuse the row, got: %v", refusal.name, err)
		}
	}
}

// intentAddressed picks the commitment aimed at one kind of target, and fails
// when there is none: which rule a statement reaches depends on it.
func intentAddressed(t *testing.T, s *Store, agID, targetKind string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`
		SELECT id FROM outbound_intents
		WHERE alert_group_id = $1 AND target_kind = $2`, agID, targetKind).Scan(&id); err != nil {
		t.Fatalf("no commitment addressed to a %s: %v", targetKind, err)
	}
	return id
}

// TestAStartRefusesAPayloadItCannotDigest.
//
// A payload the codec will not read has no digest anybody can compute, and
// there is no value to put in the column that would mean anything: a zero, or
// the digest of some other reading of the bytes, would be compared against on
// every attempt for the rest of that commitment's life. So the start stops and
// names the row.
//
// And it stops WHOLE. Half of this applied - the column present, the rules not
// - is a database where the next start finds a column it has no reason to
// distrust.
func TestAStartRefusesAPayloadItCannotDigest(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := admitOne(t, s, agID, channelCommitment("C-ops", 0))[0]

	previousOutboundShape(t, s)
	// A slot with no kind: the schema version this build renders, carrying
	// bytes it cannot read. The recipient is left exactly as it was, so the
	// rule about addressing is not what refuses this.
	exec(t, s, `
		UPDATE outbound_intents
		SET payload = jsonb_set(payload, '{slot}', '{"kind":""}') WHERE id = $1`,
		intentID)

	// The remedy the refusal names, applied here so that every later start in
	// this process is not refused for the same reason.
	t.Cleanup(func() {
		exec(t, s, `
			UPDATE outbound_intents
			SET payload = jsonb_set(payload, '{slot}', '{"kind":"firehose"}')
			WHERE id = $1`, intentID)
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("put the schema back: %v", err)
		}
	})

	err := s.applyOutboundSchema()
	if err == nil {
		t.Fatal("a start accepted a payload nothing can digest")
	}
	if !strings.Contains(err.Error(), intentID) {
		t.Fatalf("the refusal does not name the row to fix: %v", err)
	}
	if hasColumn(t, s, "outbound_intents", "payload_digest") {
		t.Error("the refusal added the column anyway")
	}
}

// TestAStartUpgradesADatabaseThatFollowedDevelop is the upgrade an
// installation running the `:develop` images before this version meets, in
// one start: the delivery domain already knowing the webhook family, but
// with no event on a claim, no fan-out date on an event, no kind on a journal
// line, and none of the indexes those came with - and beside it the job
// engine's tables in their last shape and the old webhook worker's tables,
// still standing and still holding rows, with the old worker's keys to the
// events and the subscribers. Each thing the start fills in is read back
// against what it was filled in from.
//
// This is not the upgrade from the last release: that database has no
// delivery domain at all, and TestAStartUpgradesTheDatabaseOfTheLastRelease
// starts from its exact schema.
func TestAStartUpgradesADatabaseThatFollowedDevelop(t *testing.T) {
	s := setupTestDB(t)
	teamOne(t, s)
	group := outboundGroup(t, s)
	a := subscriber(t, s, "a", model.WebhookScopeTeam, "team-1", true)

	// What the previous version had done: an escalation; an event it fanned
	// out to the subscriber; an event the old worker had sent and one it had
	// given up on, both before the port; and an event still waiting.
	escalation := admitOne(t, s, group, dmCommitment("U0001"))[0]
	fanned := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)
	fanOutNext(t, s)
	delivered := webhookCommitmentTo(t, s, a)
	sent := eventForGroup(t, s, group, "team-1", model.OutboxEventAcknowledged)
	failed := eventForGroup(t, s, group, "team-1", model.OutboxEventResolved)
	pending := eventForGroup(t, s, group, "team-1", model.OutboxEventFiring)

	// Back to the shape of the previous release, rows kept: the delivery
	// domain already knowing the webhook family, and the two engines it
	// replaced still standing beside it.
	legacyWebhookWorker(t, s)
	legacyJobEngine(t, s)
	oldDelivery := uuid.New().String()
	for _, statement := range []string{
		`UPDATE event_outbox SET status = 'completed', sent_at = now() - interval '3 days',
			created_at = now() - interval '4 days' WHERE id = '` + sent + `'`,
		`UPDATE event_outbox SET status = 'failed', created_at = now() - interval '5 days'
			WHERE id = '` + failed + `'`,
		// Dropping a column takes its index and its rule with it.
		`ALTER TABLE outbound_batches DROP COLUMN IF EXISTS event_id`,
		`ALTER TABLE event_outbox DROP COLUMN IF EXISTS fanned_out_at`,
		`DROP INDEX IF EXISTS idx_outbound_intents_batch, idx_outbound_intents_journal,
			idx_outbound_intents_retention, idx_outbound_batches_no_targets`,
		`DELETE FROM outbound_intent_events`,
		`ALTER TABLE outbound_intent_events DROP COLUMN IF EXISTS actor_kind`,
		// The rows of the previous release in the tables it owned.
		`INSERT INTO jobs (id, type, status, alert_group_id)
			VALUES ('job-1', 'escalation', 'completed', '` + group + `')`,
		`INSERT INTO event_outbox_deliveries (id, event_id, integration_id, status, attempts, sent_at)
			VALUES ('` + oldDelivery + `', '` + sent + `', '` + a + `', 'sent', 1, now() - interval '3 days')`,
		`INSERT INTO event_outbox_delivery_attempts (id, delivery_id, attempt, http_status)
			VALUES ('` + uuid.New().String() + `', '` + oldDelivery + `', 1, 200)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the previous release: %v", err)
		}
	}
	// Journal lines as that version wrote them, with no kind: one by each
	// write path the classification tells apart - a component, the fan-out
	// under its old name, and an acknowledgement under a person's display
	// name that happens to read like a component.
	lines := []struct {
		id, intent, kind    string
		reason, actor       *string
		wantKind, wantActor string
	}{
		{uuid.New().String(), escalation, "created", nil, str("engine"), "system", "engine"},
		{uuid.New().String(), delivered, "created", nil, str("fan-out"), "system", "fanout"},
		{uuid.New().String(), escalation, "canceled", str("the alert was acknowledged"), str("system"), "legacy", "system"},
	}
	for i, line := range lines {
		if _, err := s.db.Exec(`INSERT INTO outbound_intent_events (id, intent_id, seq, kind, reason, actor)
			VALUES ($1, $2, $3, $4, $5, $6)`, line.id, line.intent, i+1, line.kind, line.reason, line.actor); err != nil {
			t.Fatalf("write the old journal line %d: %v", i, err)
		}
	}
	for _, key := range []string{"event_outbox_deliveries_event_id_fkey", "event_outbox_deliveries_integration_id_fkey"} {
		if countWhere(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, key) != 1 {
			t.Fatalf("the previous release has no %s to remove", key)
		}
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused the database of the previous release: %v", err)
	}

	// The claims name their event, and every event named exists.
	var named string
	if err := s.db.QueryRow(`SELECT event_id FROM outbound_batches WHERE key_kind = 'webhook_event'
		AND batch_key LIKE $1`, fanned+":%").Scan(&named); err != nil || named != fanned {
		t.Errorf("the claim on the fanned-out event names %q, %v; want %q", named, err, fanned)
	}
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_batches b
		WHERE b.key_kind IN ('webhook_event', 'webhook_replay')
		  AND (b.event_id IS NULL OR NOT EXISTS (SELECT 1 FROM event_outbox e WHERE e.id = b.event_id))`); n != 0 {
		t.Errorf("%d webhook claims name no event, or one that does not exist", n)
	}

	// The events are dated: a fanned-out one by its claim, a finished one by
	// when the old worker sent it or, failing that, made it; a waiting one
	// not at all.
	dated := func(id string) *time.Time {
		var at *time.Time
		if err := s.db.QueryRow(`SELECT fanned_out_at FROM event_outbox WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	var admittedAt time.Time
	if err := s.db.QueryRow(`SELECT admitted_at FROM outbound_batches
		WHERE event_id = $1 AND key_kind = 'webhook_event'`, fanned).Scan(&admittedAt); err != nil {
		t.Fatal(err)
	}
	if at := dated(fanned); at == nil || !at.Equal(admittedAt) {
		t.Errorf("the fanned-out event is dated %v, want its claim's %v", at, admittedAt)
	}
	if at := dated(sent); at == nil || time.Since(*at) < 2*day || time.Since(*at) > 4*day {
		t.Errorf("the event the old worker sent is dated %v, want when it was sent", at)
	}
	if at := dated(failed); at == nil || time.Since(*at) < 4*day || time.Since(*at) > 6*day {
		t.Errorf("the event the old worker gave up on is dated %v, want when it was made", at)
	}
	if at := dated(pending); at != nil {
		t.Errorf("the waiting event is dated %v, want no date", at)
	}

	// The journal lines are classified by the path that wrote them.
	for i, line := range lines {
		var actor sql.NullString
		var kind string
		if err := s.db.QueryRow(`SELECT actor, actor_kind FROM outbound_intent_events WHERE id = $1`, line.id).
			Scan(&actor, &kind); err != nil {
			t.Fatalf("read the old journal line %d: %v", i, err)
		}
		if kind != line.wantKind || actor.String != line.wantActor {
			t.Errorf("line %d (%s by %v) is classified %s:%s, want %s:%s",
				i, line.kind, deref(line.actor), kind, actor.String, line.wantKind, line.wantActor)
		}
	}

	// The six indexes.
	for _, index := range []string{
		"idx_outbound_batches_event", "idx_outbound_intents_batch", "idx_outbound_intents_journal",
		"idx_outbound_batches_no_targets", "idx_outbound_intents_retention", "idx_event_outbox_retention",
	} {
		if !relationExists(t, s, index) {
			t.Errorf("%s did not arrive", index)
		}
	}

	// The old worker's keys are gone; its rows, and the job engine's, are
	// exactly where they were.
	for _, key := range []string{"event_outbox_deliveries_event_id_fkey", "event_outbox_deliveries_integration_id_fkey"} {
		if countWhere(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, key) != 0 {
			t.Errorf("the start left %s", key)
		}
	}
	for table, want := range map[string]int{"jobs": 1, "event_outbox_deliveries": 1, "event_outbox_delivery_attempts": 1} {
		if n := countWhere(t, s, `SELECT count(*) FROM `+table); n != want {
			t.Errorf("%s holds %d rows after the start, want %d untouched", table, n, want)
		}
	}

	// And the domain goes on from there: the event that was waiting is fanned
	// out by this build, under a claim that names it.
	fanOutNext(t, s)
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_batches WHERE event_id = $1 AND key_kind = 'webhook_event'`, pending); n != 1 {
		t.Errorf("the waiting event has %d claims after the first fan-out, want one", n)
	}
}

func str(v string) *string { return &v }

// throwawayDatabase is a database of the test's own, built from a schema
// file, with a store open on it; it is dropped when the test ends. The
// suite's database has been through this version's start already, which is
// exactly what a test of that start on an older shape cannot use.
func throwawayDatabase(t *testing.T, schemaFile string) *Store {
	t.Helper()
	if testStore == nil {
		t.Skip("TEST_DB_DSN not set")
	}
	dsn, err := url.Parse(os.Getenv("TEST_DB_DSN"))
	if err != nil {
		t.Fatalf("read TEST_DB_DSN: %v", err)
	}
	name := "upgrade_" + strings.ReplaceAll(uuid.New().String()[:13], "-", "")
	if _, err := testStore.db.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create the throwaway database: %v", err)
	}
	dsn.Path = "/" + name
	s, err := NewStore(dsn.String())
	if err != nil {
		t.Fatalf("open the throwaway database: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		if _, err := testStore.db.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop the throwaway database: %v", err)
		}
	})
	schema, err := os.ReadFile(filepath.Join("testdata", schemaFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(string(schema)); err != nil {
		t.Fatalf("build %s: %v", schemaFile, err)
	}
	return s
}

// TestAStartUpgradesTheDatabaseOfTheLastRelease is the upgrade every
// installation on the last release makes: the schema exactly as v0.1.0's own
// start built it (testdata/schema-v0.1.0.sql, dumped from it), holding the
// rows that release wrote - a team, an alert group under the old name of its
// key column, a finished escalation job and the message it delivered, a
// webhook subscriber, an event the old worker sent with the delivery row and
// attempt it wrote, an event it gave up on, and an event it had not reached -
// and no delivery domain at all. One start of this version brings it up, and
// what the start fills in is read back against the rows it was filled in
// from.
func TestAStartUpgradesTheDatabaseOfTheLastRelease(t *testing.T) {
	s := throwawayDatabase(t, "schema-v0.1.0.sql")
	if relationExists(t, s, "outbound_intents") || !hasColumn(t, s, "alert_groups", "dedup_key") {
		t.Fatal("the schema file is not the last release's")
	}

	cfg, _ := json.Marshal(model.GenericWebhookConfig{URL: "https://example.com/hook", Secret: "s"})
	sealed, err := encryptConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const group, subscriber, sent, failed, pending, delivery = "ag-1", "int-1", "evt-sent", "evt-failed", "evt-pending", "del-1"
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO teams (id, name, created_at) VALUES ('team-1', 'Team One', now())`, nil},
		{`INSERT INTO alert_groups (id, dedup_key, status, title, team_id, team_name_snapshot, severity, created_at, updated_at)
			VALUES ($1, 'group-key', 'resolved', 'Disk filling up', 'team-1', 'Team One', 'critical', now() - interval '6 days', now() - interval '6 days')`, []any{group}},
		{`INSERT INTO jobs (id, type, status, alert_group_id, created_at, updated_at, finished_at)
			VALUES ('job-1', 'escalation', 'completed', $1, now() - interval '6 days', now() - interval '6 days', now() - interval '6 days')`, []any{group}},
		{`INSERT INTO notification_deliveries (id, alert_group_id, provider, kind, target_type, target_id)
			VALUES ('nd-1', $1, 'slack', 'channel', 'channel', 'C-ops')`, []any{group}},
		{`INSERT INTO integrations (id, type, direction, name, enabled, config, scope, team_id)
			VALUES ($1, 'generic_webhook', 'outbound', 'hook', true, $2, 'team', 'team-1')`, []any{subscriber, sealed}},
		{`INSERT INTO event_outbox (id, event_type, alert_group_id, team_id, payload, status, attempts, created_at, sent_at)
			VALUES ($1, 'alert_group.firing', $2, 'team-1', $3, 'completed', 1, now() - interval '4 days', now() - interval '3 days')`,
			[]any{sent, group, `{"event":"alert_group.firing","alert_group":{"id":"` + group + `"}}`}},
		{`INSERT INTO event_outbox (id, event_type, alert_group_id, team_id, payload, status, attempts, last_error, created_at)
			VALUES ($1, 'alert_group.acknowledged', $2, 'team-1', $3, 'failed', 8, 'HTTP 404', now() - interval '5 days')`,
			[]any{failed, group, `{"event":"alert_group.acknowledged","alert_group":{"id":"` + group + `"}}`}},
		{`INSERT INTO event_outbox (id, event_type, alert_group_id, team_id, payload, status, created_at)
			VALUES ($1, 'alert_group.resolved', $2, 'team-1', $3, 'pending', now() - interval '1 hour')`,
			[]any{pending, group, `{"event":"alert_group.resolved","alert_group":{"id":"` + group + `"}}`}},
		{`INSERT INTO event_outbox_deliveries (id, event_id, integration_id, status, attempts, last_http_status, created_at, sent_at)
			VALUES ($1, $2, $3, 'sent', 1, 200, now() - interval '4 days', now() - interval '3 days')`, []any{delivery, sent, subscriber}},
		{`INSERT INTO event_outbox_delivery_attempts (id, delivery_id, attempt, http_status)
			VALUES ('att-1', $1, 1, 200)`, []any{delivery}},
	} {
		if _, err := s.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatalf("write the last release's rows: %v\n%s", err, statement.sql)
		}
	}
	for _, key := range []string{"event_outbox_deliveries_event_id_fkey", "event_outbox_deliveries_integration_id_fkey"} {
		if countWhere(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, key) != 1 {
			t.Fatalf("the last release has no %s to remove", key)
		}
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused the last release's database: %v", err)
	}

	// The key column of an alert group is renamed, and the group is still there
	// under it.
	if hasColumn(t, s, "alert_groups", "dedup_key") || !hasColumn(t, s, "alert_groups", "alert_key") {
		t.Error("the alert group's key column was not renamed")
	}
	if countWhere(t, s, `SELECT count(*) FROM alert_groups WHERE alert_key = 'group-key'`) != 1 {
		t.Error("the alert group did not survive the start")
	}

	// The delivery domain arrives whole, with the six indexes this version
	// added to it.
	for _, table := range []string{"outbound_batches", "outbound_intents", "outbound_attempts",
		"outbound_attempt_observations", "outbound_intent_events", "outbound_group_snapshots",
		"integration_tombstones"} {
		if !relationExists(t, s, table) {
			t.Errorf("%s did not arrive", table)
		}
	}
	for _, index := range []string{
		"idx_outbound_batches_event", "idx_outbound_intents_batch", "idx_outbound_intents_journal",
		"idx_outbound_batches_no_targets", "idx_outbound_intents_retention", "idx_event_outbox_retention",
	} {
		if !relationExists(t, s, index) {
			t.Errorf("%s did not arrive", index)
		}
	}
	var notNull bool
	if err := s.db.QueryRow(`SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'outbound_intent_events'::regclass AND attname = 'actor_kind'`).Scan(&notNull); err != nil || !notNull {
		t.Errorf("actor_kind is not required after the upgrade: %v", err)
	}

	// The old worker's events are dated by when it sent them or, failing
	// that, made them; the one it had not reached is not dated.
	dated := func(id string) *time.Time {
		var at *time.Time
		if err := s.db.QueryRow(`SELECT fanned_out_at FROM event_outbox WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if at := dated(sent); at == nil || time.Since(*at) < 2*day || time.Since(*at) > 4*day {
		t.Errorf("the event the old worker sent is dated %v, want when it was sent", at)
	}
	if at := dated(failed); at == nil || time.Since(*at) < 4*day || time.Since(*at) > 6*day {
		t.Errorf("the event the old worker gave up on is dated %v, want when it was made", at)
	}
	if at := dated(pending); at != nil {
		t.Errorf("the waiting event is dated %v, want no date", at)
	}

	// The old worker's keys are gone; its rows, and the job engine's, are
	// exactly where they were.
	for _, key := range []string{"event_outbox_deliveries_event_id_fkey", "event_outbox_deliveries_integration_id_fkey"} {
		if countWhere(t, s, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, key) != 0 {
			t.Errorf("the start left %s", key)
		}
	}
	for table, want := range map[string]int{"jobs": 1, "notification_deliveries": 1,
		"event_outbox_deliveries": 1, "event_outbox_delivery_attempts": 1} {
		if n := countWhere(t, s, `SELECT count(*) FROM `+table); n != want {
			t.Errorf("%s holds %d rows after the start, want %d untouched", table, n, want)
		}
	}

	// And the domain goes on from there: the event the old worker had not
	// reached is fanned out by this build to the subscriber that release
	// wrote, under a claim that names the event.
	fanOutNext(t, s)
	if n := countWhere(t, s, `SELECT count(*) FROM outbound_batches WHERE event_id = $1 AND key_kind = 'webhook_event'`, pending); n != 1 {
		t.Errorf("the waiting event has %d claims after the first fan-out, want one", n)
	}
	if got := webhookCommitmentTo(t, s, subscriber); got == "" {
		t.Error("nothing is owed to the last release's subscriber")
	}

	// The transition the checklist ends with, on this database: both files,
	// in the order they are listed, remove what the last release left and
	// nothing else, and a start afterwards puts nothing back.
	for _, file := range []string{"drop-job-engine.sql", "drop-webhook-outbox.sql"} {
		path := filepath.Join("..", "..", "migrations", file)
		statements, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(string(statements)); err != nil {
			t.Fatalf("%s on the last release's database: %v", file, err)
		}
	}
	assertJobEngineGone(t, s, "the transition")
	for _, gone := range []string{"event_outbox_deliveries", "event_outbox_delivery_attempts"} {
		if relationExists(t, s, gone) {
			t.Errorf("%s survived the transition", gone)
		}
	}
	if n := countWhere(t, s, `SELECT count(*) FROM event_outbox`); n != 1 {
		t.Errorf("%d events after the transition, want only the one this build fanned out", n)
	}
	if err := s.InitDB(); err != nil {
		t.Fatalf("start after the transition: %v", err)
	}
	assertJobEngineGone(t, s, "a start after the transition")
	if got, err := s.GetAlertGroupByID(group); err != nil || got == nil {
		t.Fatalf("read the alert group after the transition: %v, %v", got, err)
	}
}
