package store

import (
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// addLegacySlackColumn re-creates the pre-Epic-7 users.slack_user_id column to
// simulate an un-migrated database, and drops it again on cleanup so the shared
// package schema is left untouched for other tests.
func addLegacySlackColumn(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.GetDB().Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS slack_user_id TEXT`); err != nil {
		t.Fatalf("add legacy slack_user_id column: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.GetDB().Exec(`ALTER TABLE users DROP COLUMN IF EXISTS slack_user_id`)
	})
}

func mustCreateUser(t *testing.T, s *Store, id, email string) {
	t.Helper()
	if err := s.CreateUser(&model.User{ID: id, Email: email, Name: id}); err != nil {
		t.Fatalf("CreateUser %s: %v", id, err)
	}
}

func setLegacySlack(t *testing.T, s *Store, userID, slackID string) {
	t.Helper()
	if _, err := s.GetDB().Exec(`UPDATE users SET slack_user_id = $1 WHERE id = $2`, slackID, userID); err != nil {
		t.Fatalf("set legacy slack id for %s: %v", userID, err)
	}
}

func assertSlackIdentity(t *testing.T, s *Store, userID, wantExternalID string) {
	t.Helper()
	ei, err := s.GetExternalIdentity(userID, "slack")
	if err != nil {
		t.Fatalf("GetExternalIdentity(%s): %v", userID, err)
	}
	if ei.ExternalID != wantExternalID {
		t.Errorf("user %s: external id = %q, want %q", userID, ei.ExternalID, wantExternalID)
	}
}

func TestMigrateLegacySlackIdentities(t *testing.T) {
	s := setupTestDB(t)

	// Fresh schema has no legacy column → no-op.
	res, err := s.MigrateLegacySlackIdentities(false)
	if err != nil {
		t.Fatalf("migrate (no legacy column): %v", err)
	}
	if res.LegacyColumnPresent {
		t.Fatal("fresh schema should not have the legacy slack_user_id column")
	}

	// Simulate a pre-Epic-7 DB.
	addLegacySlackColumn(t, s)

	mustCreateUser(t, s, "u1", "u1@migrate.test")
	mustCreateUser(t, s, "u2", "u2@migrate.test")
	mustCreateUser(t, s, "u3", "u3@migrate.test") // no slack id — not a candidate
	mustCreateUser(t, s, "u4", "u4@migrate.test") // already linked
	setLegacySlack(t, s, "u1", "S1")
	setLegacySlack(t, s, "u2", "S2")
	setLegacySlack(t, s, "u4", "S4")
	if err := s.BindExternalIdentity(&model.ExternalIdentity{UserID: "u4", Provider: "slack", ExternalID: "S4"}); err != nil {
		t.Fatalf("pre-bind u4: %v", err)
	}

	// Dry-run: classifies but writes nothing.
	dry, err := s.MigrateLegacySlackIdentities(true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !dry.LegacyColumnPresent || dry.Candidates != 3 {
		t.Fatalf("dry-run: present=%v candidates=%d, want present=true candidates=3", dry.LegacyColumnPresent, dry.Candidates)
	}
	if dry.Migrated != 2 || dry.AlreadySatisfied != 1 || len(dry.Conflicts) != 0 {
		t.Errorf("dry-run: migrated=%d already=%d conflicts=%d, want 2/1/0", dry.Migrated, dry.AlreadySatisfied, len(dry.Conflicts))
	}
	if _, err := s.GetExternalIdentity("u1", "slack"); err == nil {
		t.Error("dry-run must not create identities, but u1 now has one")
	}

	// Apply.
	res, err = s.MigrateLegacySlackIdentities(false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Migrated != 2 || res.AlreadySatisfied != 1 || len(res.Conflicts) != 0 {
		t.Errorf("apply: migrated=%d already=%d conflicts=%d, want 2/1/0", res.Migrated, res.AlreadySatisfied, len(res.Conflicts))
	}
	assertSlackIdentity(t, s, "u1", "S1")
	assertSlackIdentity(t, s, "u2", "S2")

	// Idempotent: a second apply migrates nothing new.
	again, err := s.MigrateLegacySlackIdentities(false)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again.Migrated != 0 || again.AlreadySatisfied != 3 {
		t.Errorf("re-run not idempotent: migrated=%d already=%d, want 0/3", again.Migrated, again.AlreadySatisfied)
	}
}

func TestMigrateLegacySlackIdentities_Conflict(t *testing.T) {
	s := setupTestDB(t)
	addLegacySlackColumn(t, s)

	mustCreateUser(t, s, "owner", "owner@migrate.test")
	mustCreateUser(t, s, "loser", "loser@migrate.test")
	// owner already owns S_SHARED; loser's legacy id points at the same Slack account.
	if err := s.BindExternalIdentity(&model.ExternalIdentity{UserID: "owner", Provider: "slack", ExternalID: "S_SHARED"}); err != nil {
		t.Fatalf("bind owner: %v", err)
	}
	setLegacySlack(t, s, "loser", "S_SHARED")

	res, err := s.MigrateLegacySlackIdentities(false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Migrated != 0 {
		t.Errorf("expected 0 migrated, got %d", res.Migrated)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].UserID != "loser" || res.Conflicts[0].SlackUserID != "S_SHARED" {
		t.Fatalf("expected one conflict for loser/S_SHARED, got %+v", res.Conflicts)
	}
	// loser must NOT have stolen the binding.
	if _, err := s.GetExternalIdentity("loser", "slack"); err == nil {
		t.Error("loser should not have a slack identity after a conflict")
	}
}
