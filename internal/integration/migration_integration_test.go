//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/testutil"
	"github.com/google/uuid"
)

// TestMigration_Idempotent proves that InitDB can be called repeatedly:
// first on a clean DB, then again (idempotent), and existing data survives.
func TestMigration_Idempotent(t *testing.T) {
	s := testutil.SetupDB(t) // calls InitDB() internally

	// Second call must succeed (idempotent)
	if err := s.InitDB(); err != nil {
		t.Fatalf("Second InitDB call failed: %v", err)
	}

	// Insert test data
	team := testutil.SeedTeam(t, s, "team-migration")
	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "migration-dedup",
		Status:           model.AlertGroupStatusNew,
		Title:            "Migration Test Alert",
		TeamID:           team.ID,
		TeamNameSnapshot: team.Name,
		Severity:         "warning",
		Alerts:           []model.Alert{{Fingerprint: "fp-migration"}},
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup failed: %v", err)
	}

	// Third call — existing data must survive
	if err := s.InitDB(); err != nil {
		t.Fatalf("Third InitDB call failed: %v", err)
	}

	// Verify team still queryable
	fetchedTeam, err := s.GetTeamByID(team.ID)
	if err != nil {
		t.Fatalf("GetTeamByID after third InitDB: %v", err)
	}
	if fetchedTeam.Name != team.Name {
		t.Errorf("Team name mismatch: got %q, want %q", fetchedTeam.Name, team.Name)
	}

	// Verify alert group still queryable
	fetchedAG, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID after third InitDB: %v", err)
	}
	if fetchedAG.Title != ag.Title {
		t.Errorf("AG title mismatch: got %q, want %q", fetchedAG.Title, ag.Title)
	}
}

// TestMigration_EscalationStepsStepTypeToProviderKind verifies the Epic 7 Sprint 4
// upgrade migration: an old escalation_steps table keyed by step_type is migrated
// in place to (provider, target_kind), backfilling provider='slack' and mapping
// slack_dm->dm, slack_channel->channel, and any other legacy value->dm. This is the
// main upgrade regression from the Sprint 4 review (no automated coverage before).
func TestMigration_EscalationStepsStepTypeToProviderKind(t *testing.T) {
	s := testutil.SetupDB(t) // InitDB created the NEW (provider, target_kind) schema

	// Parent policy for the FK (escalation_steps.policy_id -> escalation_policies.id).
	team := testutil.SeedTeam(t, s, "team-esc-migration")
	policyID := uuid.New().String()
	if _, err := s.GetDB().Exec(
		`INSERT INTO escalation_policies (id, name, team_id) VALUES ($1, 'Legacy Policy', $2)`,
		policyID, team.ID,
	); err != nil {
		t.Fatalf("insert policy: %v", err)
	}

	// Revert escalation_steps to the legacy shape: drop the new columns and add
	// step_type, simulating a DB created before Sprint 4.
	if _, err := s.GetDB().Exec(`
		ALTER TABLE escalation_steps DROP COLUMN provider;
		ALTER TABLE escalation_steps DROP COLUMN target_kind;
		ALTER TABLE escalation_steps ADD COLUMN step_type TEXT NOT NULL DEFAULT 'slack_dm';
	`); err != nil {
		t.Fatalf("simulate legacy schema: %v", err)
	}

	// Insert legacy rows covering all three CASE branches.
	dmID := uuid.New().String()
	chID := uuid.New().String()
	otherID := uuid.New().String()
	if _, err := s.GetDB().Exec(`
		INSERT INTO escalation_steps (id, policy_id, step_index, step_type, target_type, target_id)
		VALUES ($1, $4, 0, 'slack_dm',      'user',    'U_LEGACY'),
		       ($2, $4, 1, 'slack_channel', 'channel', 'C_LEGACY'),
		       ($3, $4, 2, 'legacy_other',  'user',    'U_OTHER')
	`, dmID, chID, otherID, policyID); err != nil {
		t.Fatalf("insert legacy escalation_steps: %v", err)
	}

	// Run the migration.
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB (migration): %v", err)
	}

	assertStep := func(id, wantProvider, wantKind string) {
		t.Helper()
		var provider, kind string
		if err := s.GetDB().QueryRow(
			`SELECT provider, target_kind FROM escalation_steps WHERE id = $1`, id,
		).Scan(&provider, &kind); err != nil {
			t.Fatalf("read step %s: %v", id, err)
		}
		if provider != wantProvider || kind != wantKind {
			t.Errorf("step %s: got (provider=%q, target_kind=%q), want (%q, %q)",
				id, provider, kind, wantProvider, wantKind)
		}
	}
	assertStep(dmID, "slack", "dm")
	assertStep(chID, "slack", "channel")
	assertStep(otherID, "slack", "dm") // ELSE branch -> dm

	// The legacy step_type column must be gone after migration.
	var stepTypeCols int
	if err := s.GetDB().QueryRow(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'escalation_steps' AND column_name = 'step_type'
	`).Scan(&stepTypeCols); err != nil {
		t.Fatalf("check step_type column: %v", err)
	}
	if stepTypeCols != 0 {
		t.Errorf("step_type column should be dropped after migration, found %d", stepTypeCols)
	}

	// The new target_kind CHECK constraint must reject out-of-range values.
	if _, err := s.GetDB().Exec(`
		INSERT INTO escalation_steps (id, policy_id, step_index, provider, target_kind, target_type, target_id)
		VALUES ($1, $2, 3, 'slack', 'bogus', 'user', 'U_X')
	`, uuid.New().String(), policyID); err == nil {
		t.Error("expected target_kind CHECK to reject 'bogus', insert succeeded")
	}

	// Idempotency: re-running InitDB must not re-run the (now-inapplicable) migration
	// or corrupt the already-migrated rows.
	if err := s.InitDB(); err != nil {
		t.Fatalf("second InitDB (idempotency): %v", err)
	}
	assertStep(dmID, "slack", "dm")
	assertStep(chID, "slack", "channel")
}
