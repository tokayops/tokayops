package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// TestAStartUpgradesTheDatabaseOfV030 is the upgrade every installation on
// v0.3.0 makes: the schema exactly as v0.3.0's own start built it
// (testdata/schema-v0.3.0.sql, dumped from it), holding a user and the Slack
// integration that release allowed one of. One start adds the phone tables,
// touches neither row, and lets the one-per-type rule go for Twilio only.
func TestAStartUpgradesTheDatabaseOfV030(t *testing.T) {
	s := throwawayDatabase(t, "schema-v0.3.0.sql")
	if !hasColumn(t, s, "alert_groups", "intake_integration_id") || relationExists(t, s, "user_contacts") {
		t.Fatal("the schema file is not v0.3.0's")
	}

	if _, err := s.db.Exec(`INSERT INTO users (id, email, name, role, created_at)
		VALUES ('alice', 'alice@example.com', 'Alice', 'user', now())`); err != nil {
		t.Fatalf("write v0.3.0's user: %v", err)
	}
	slack := &model.Integration{
		ID: "slack-1", Type: model.IntegrationTypeSlack, Direction: model.IntegrationDirectionOutbound,
		Name: "Slack", Enabled: true, Config: json.RawMessage(`{"token":"xoxb-1"}`),
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateIntegration(slack); err != nil {
		t.Fatalf("write v0.3.0's integration: %v", err)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused v0.3.0's database: %v", err)
	}

	for _, table := range []string{"user_contacts", "phone_dnd_checks", "phone_calls_log", "call_capacity", "call_reservations"} {
		if !relationExists(t, s, table) {
			t.Errorf("the start did not add %s", table)
		}
	}
	if in, err := s.GetIntegrationByID("slack-1"); err != nil || in == nil || !in.Enabled {
		t.Fatalf("the Slack integration did not survive the start: %+v, %v", in, err)
	}
	if user, err := s.GetUserByID("alice"); err != nil || user == nil {
		t.Fatalf("the user did not survive the start: %v", err)
	}

	// Slack is still one per installation; Twilio is not.
	second := *slack
	second.ID = "slack-2"
	if err := s.CreateIntegration(&second); err != ErrDuplicateIntegration {
		t.Fatalf("a second Slack integration after the upgrade = %v, want ErrDuplicateIntegration", err)
	}
	for i := 0; i < 2; i++ {
		createTwilioRow(t, s, i)
	}
}

// Every start rebuilds the one-per-type index. With two Twilio integrations in
// the table, a start whose rebuild did not know about Twilio would fail there,
// before any later phase could widen it.
func TestAStartSucceedsWithTwoTwilioIntegrations(t *testing.T) {
	s := throwawayDatabase(t, "schema-v0.3.0.sql")
	if err := s.InitDB(); err != nil {
		t.Fatalf("first start: %v", err)
	}
	for i := 0; i < 2; i++ {
		createTwilioRow(t, s, i)
	}
	if err := s.InitDB(); err != nil {
		t.Fatalf("a start with two Twilio integrations failed: %v", err)
	}
}

func createTwilioRow(t *testing.T, s *Store, i int) {
	t.Helper()
	cfg, _ := json.Marshal(model.TwilioConfig{
		AccountSID: fmt.Sprintf("AC%032x", 7), AuthToken: "secret",
		FromNumber: fmt.Sprintf("+1500555000%d", i), Coverage: []string{"+"}, CPS: 1, MaxConcurrent: 1,
	})
	in := &model.Integration{
		ID: uuid.New().String(), Type: model.IntegrationTypeTwilio, Direction: model.IntegrationDirectionOutbound,
		Name: fmt.Sprintf("twilio %d", i), Enabled: true, Config: cfg, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateIntegration(in); err != nil {
		t.Fatalf("Twilio integration %d: %v", i, err)
	}
}
