package testutil

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	_ "github.com/lib/pq"
)

// BindIdentity binds an external identity (provider + external ID) to a user via
// the Epic 7 external_identities table. Fails the test on error. Use this so
// escalation/handoff/syncer recipients resolve to a provider-specific ID.
func BindIdentity(t *testing.T, s *store.Store, userID, provider, externalID string) {
	t.Helper()
	if err := s.BindExternalIdentity(&model.ExternalIdentity{
		UserID: userID, Provider: provider, ExternalID: externalID,
	}); err != nil {
		t.Fatalf("BindExternalIdentity %s/%s: %v", userID, provider, err)
	}
}

// BindSlack is a convenience wrapper for the common Slack case.
func BindSlack(t *testing.T, s *store.Store, userID, slackID string) {
	t.Helper()
	BindIdentity(t, s, userID, "slack", slackID)
}

// SetupDB creates a connection to the test database and truncates tables.
// It skips the test if TEST_DB_DSN is not set.
func SetupDB(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("Skipping integration test: TEST_DB_DSN not set")
	}

	s, err := store.NewStore(dsn)
	if err != nil {
		t.Fatalf("Failed to connect to test DB: %v", err)
	}

	// Ensure schema is up to date
	if err := s.InitDB(); err != nil {
		t.Fatalf("Failed to init test DB schema: %v", err)
	}

	t.Cleanup(func() {
		s.Close()
	})

	TruncateTables(t, s)

	return s
}

func TruncateTables(t *testing.T, s *store.Store) {
	t.Helper()

	tables := []string{
		"event_outbox_delivery_attempts",
		"event_outbox_deliveries",
		"event_outbox",
		"integrations",
		"schedule_overrides",
		"schedule_users",
		"rotation_epochs",
		"link_tokens",
		"external_identities",
		"schedules",
		"timeline_events",
		"team_members",
		"job_steps",
		"job_stages",
		"jobs",
		"alert_groups",
		"users",
		"teams",
	}

	// nosemgrep: string-formatted-query - table names are hardcoded, not user input
	query := fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", strings.Join(tables, ", "))
	if _, err := s.GetDB().Exec(query); err != nil {
		t.Fatalf("Failed to truncate tables: %v", err)
	}
}
