package store

import (
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
)

var testStore *Store

func TestMain(m *testing.M) {
	// Integration tests that exercise encrypted integration configs require an
	// encryption key - notably Seed(), which creates Slack + Alertmanager webhook
	// integrations. CI sets ENCRYPTION_KEY in the job env; set a deterministic
	// default here so local runs (make test-integration) behave identically no
	// matter how the package is invoked. An explicitly provided key still wins.
	if os.Getenv(config.EncryptionKeyEnv) == "" {
		os.Setenv(config.EncryptionKeyEnv, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	}

	// Check if test DB is configured
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn != "" {
		var err error
		testStore, err = NewStore(dsn)
		if err != nil {
			log.Fatalf("Failed to connect to test DB: %v", err)
		}
		defer testStore.Close()

		// Initialize Schema
		if err := testStore.InitDB(); err != nil {
			log.Fatalf("Failed to init test DB schema: %v", err)
		}
	} else {
		fmt.Println("TEST_DB_DSN not set - skipping integration tests, running mock tests only")
	}

	code := m.Run()

	os.Exit(code)
}

// forceAlertGroupStatus puts a group into a status whatever it is in now.
//
// The store has no such method, on purpose: production moves a group through
// the door that owns the move, and each of those states what it expected to
// find. Fixtures that rewind a group to set up the next assertion have nothing
// to state, so they say it in SQL here rather than putting an unconditional
// setter back in the contract.
func forceAlertGroupStatus(t *testing.T, s *Store, id string, status model.AlertGroupStatus) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE alert_groups SET status = $1, updated_at = NOW() WHERE id = $2`, status, id); err != nil {
		t.Fatalf("force alert group %s to %s: %v", id, status, err)
	}
}

// setupTestDB cleans up the database for a fresh test run
// usage: s := setupTestDB(t)
func setupTestDB(t *testing.T) *Store {
	t.Helper()
	if testStore == nil {
		t.Skip("TEST_DB_DSN not set")
	}

	// Truncate all tables to ensure clean state
	// Order matters due to foreign keys if CASCADE is not used (but we use CASCADE here for safety)
	tables := []string{
		"outbound_intent_events",
		"outbound_attempt_observations",
		"outbound_attempts",
		"outbound_intents",
		"outbound_group_snapshots",
		"outbound_batches",
		"event_outbox_deliveries",
		"event_outbox",
		"notification_deliveries",
		"integrations",
		"schedule_revisions",
		"schedule_override_revisions",
		"migration_markers",
		"schedules",
		"timeline_events",
		"team_members",
		"job_steps",
		"jobs",
		"alert_groups",
		"users",
		"teams",
	}

	query := fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", strings.Join(tables, ", "))
	if _, err := testStore.db.Exec(query); err != nil {
		t.Fatalf("Failed to truncate tables: %v", err)
	}

	return testStore
}
