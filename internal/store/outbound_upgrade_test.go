package store

import (
	"context"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound"
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
	// Put it back before leaving: the row outlives this test, and the next
	// start-up would refuse to add the rule again over a row that breaks it.
	t.Cleanup(func() {
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET form = $2 WHERE id = $1`,
			intentID, string(outbound.FormEditable)); err != nil {
			t.Fatalf("put the form back: %v", err)
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
