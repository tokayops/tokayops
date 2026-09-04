//go:build integration

package integration

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/testutil"
)

// testEncryptionKey is a 32-byte hex key used for integration tests.
const testEncryptionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// setEncryptionKey sets ENCRYPTION_KEY for the duration of the test.
func setEncryptionKey(t *testing.T) {
	t.Helper()
	prev := os.Getenv("ENCRYPTION_KEY")
	os.Setenv("ENCRYPTION_KEY", testEncryptionKey)
	t.Cleanup(func() {
		if prev == "" {
			os.Unsetenv("ENCRYPTION_KEY")
		} else {
			os.Setenv("ENCRYPTION_KEY", prev)
		}
	})
}

// TestOutbox_AtomicEventSurvives proves the transactional guarantee: alert group,
// timeline events, and outbox event are all visible after a single TX commit.
func TestOutbox_AtomicEventSurvives(t *testing.T) {
	setEncryptionKey(t)
	s := testutil.SetupDB(t)

	team := testutil.SeedTeam(t, s, "team-outbox")

	agID := uuid.New().String()
	now := time.Now()
	ag := &model.AlertGroup{
		ID:               agID,
		AlertKey:         "outbox-atomic-dedup",
		Status:           model.AlertGroupStatusTriggered,
		Title:            "Atomic Outbox Test",
		TeamID:           team.ID,
		TeamNameSnapshot: team.Name,
		Severity:         "critical",
		Alerts:           []model.Alert{{Fingerprint: "fp-atomic", Status: "firing"}},
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	tlID := uuid.New().String()
	timeline := []*model.TimelineEvent{
		{
			ID:           tlID,
			AlertGroupID: agID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created",
			Actor:        "system",
			CreatedAt:    now,
		},
	}

	payload, err := model.BuildWebhookEventPayload(
		model.OutboxEventFiring, ag, team.Name, "system", "", now,
	)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload: %v", err)
	}

	eventID := uuid.New().String()
	event := &model.OutboxEvent{
		ID:           eventID,
		EventType:    model.OutboxEventFiring,
		AlertGroupID: agID,
		TeamID:       team.ID,
		Actor:        "system",
		Payload:      payload,
	}

	if err := s.CreateAlertGroupAtomic(ag, timeline, event); err != nil {
		t.Fatalf("CreateAlertGroupAtomic: %v", err)
	}

	// Verify outbox event
	fetched, err := s.GetOutboxEventByID(eventID)
	if err != nil {
		t.Fatalf("GetOutboxEventByID: %v", err)
	}
	if fetched.Status != model.OutboxEventStatusPending {
		t.Errorf("event status: got %q, want %q", fetched.Status, model.OutboxEventStatusPending)
	}
	if fetched.TeamID != team.ID {
		t.Errorf("event team_id: got %q, want %q", fetched.TeamID, team.ID)
	}

	// Verify alert group
	fetchedAG, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if fetchedAG.Title != ag.Title {
		t.Errorf("AG title: got %q, want %q", fetchedAG.Title, ag.Title)
	}

	// Verify timeline events
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("Expected at least 1 timeline event, got 0")
	}
}
