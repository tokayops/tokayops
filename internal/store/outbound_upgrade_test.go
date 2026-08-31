package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
		`ALTER TABLE outbound_intents DROP COLUMN IF EXISTS payload_digest`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the previous shape: %v", err)
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
