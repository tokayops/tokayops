package store

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

func TestAlertGroupLifecycle(t *testing.T) {
	s := setupTestDB(t)

	// 1. Setup Data: We need a Team to create an AlertGroup (foreign key)
	teamID := "team-devops"
	team := &model.Team{
		ID:        teamID,
		Name:      "Dev Ops",
		CreatedAt: time.Now(),
	}
	if err := s.CreateTeam(team); err != nil {
		t.Fatalf("Failed to create team: %v", err)
	}

	// 2. Create AlertGroup
	agID := uuid.New().String()
	dedupKey := "key-cpu-high-1"
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         dedupKey,
		Status:           model.AlertGroupStatusNew,
		Title:            "High CPU Usage",
		TeamID:           teamID,
		TeamNameSnapshot: "Dev Ops",
		Severity:         "critical",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		Alerts: []model.Alert{
			{Status: "firing", Labels: map[string]string{"alertname": "CPUHigh"}},
		},
	}

	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup failed: %v", err)
	}

	// 3. Retrieve and Verify
	fetched, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}

	if fetched.Title != ag.Title {
		t.Errorf("Expected title %q, got %q", ag.Title, fetched.Title)
	}
	if fetched.TeamID != teamID {
		t.Errorf("Expected teamID %q, got %q", teamID, fetched.TeamID)
	}
	if len(fetched.Alerts) != 1 {
		t.Errorf("Expected 1 alert, got %d", len(fetched.Alerts))
	}

	// 4. Update Status
	if err := s.UpdateAlertGroupStatus(agID, model.AlertGroupStatusAcknowledged); err != nil {
		t.Fatalf("UpdateAlertGroupStatus failed: %v", err)
	}

	// 5. Verify Update
	fetchedAfterUpdate, err := s.GetActiveAlertGroup(dedupKey)
	if err != nil {
		t.Fatalf("GetActiveAlertGroup failed: %v", err)
	}
	if fetchedAfterUpdate.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status %q, got %q", model.AlertGroupStatusAcknowledged, fetchedAfterUpdate.Status)
	}
}

func TestGetProcessingAlertGroups(t *testing.T) {
	s := setupTestDB(t)

	// Setup: Create a team
	teamID := "team-sre"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "SRE", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Create 3 AGs: New, Processing, Resolved
	createAG := func(id, status string) {
		ag := &model.AlertGroup{
			ID:               id,
			DedupKey:         "key-" + id,
			Status:           model.AlertGroupStatus(status),
			TeamID:           teamID,
			TeamNameSnapshot: "SRE",
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		}
		if err := s.CreateAlertGroup(ag); err != nil {
			t.Fatalf("Failed to create AG %s: %v", id, err)
		}
	}

	createAG("ag-new", string(model.AlertGroupStatusNew))
	createAG("ag-proc", string(model.AlertGroupStatusProcessing))  // Should be returned
	createAG("ag-ack", string(model.AlertGroupStatusAcknowledged)) // Should be returned (as per logic in Store)
	createAG("ag-res", string(model.AlertGroupStatusResolved))

	// Test
	processing, err := s.GetProcessingAlertGroups()
	if err != nil {
		t.Fatalf("GetProcessingAlertGroups failed: %v", err)
	}

	// Expecting Processing + Acknowledged = 2
	if len(processing) != 2 {
		t.Errorf("Expected 2 processing groups, got %d", len(processing))
		for _, p := range processing {
			t.Logf("Got group: %s (%s)", p.ID, p.Status)
		}
	}
}

// ===================================================================================
// AckProcessedAt Integration Tests
// ===================================================================================

// TestMarkAckProcessed_Integration tests MarkAckProcessed with real database
func TestMarkAckProcessed_Integration(t *testing.T) {
	s := setupTestDB(t)

	// Setup: Create team and AG
	teamID := "team-ack-test"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Ack Test", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "key-ack-" + agID,
		Status:           model.AlertGroupStatusAcknowledged,
		TeamID:           teamID,
		TeamNameSnapshot: "Ack Test",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup failed: %v", err)
	}

	// Initially ack_processed_at should be nil
	fetched, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if fetched.AckProcessedAt != nil {
		t.Error("Expected AckProcessedAt to be nil initially")
	}

	// Mark as processed
	if err := s.MarkAckProcessed(agID); err != nil {
		t.Fatalf("MarkAckProcessed failed: %v", err)
	}

	// Verify ack_processed_at is set
	fetched, err = s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID after mark failed: %v", err)
	}
	if fetched.AckProcessedAt == nil {
		t.Error("Expected AckProcessedAt to be set after MarkAckProcessed - GetAlertGroupByID may not select it!")
	}
}

// TestGetAcknowledgedAlertGroups_FiltersProcessed_Integration tests that
// GetAcknowledgedAlertGroups correctly filters by ack_processed_at IS NULL
func TestGetAcknowledgedAlertGroups_FiltersProcessed_Integration(t *testing.T) {
	s := setupTestDB(t)

	// Setup
	teamID := "team-filter-test"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Filter Test", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Create two acknowledged AGs
	ag1ID := uuid.New().String()
	ag2ID := uuid.New().String()

	createAG := func(id string) {
		ag := &model.AlertGroup{
			ID:               id,
			DedupKey:         "key-" + id,
			Status:           model.AlertGroupStatusAcknowledged,
			TeamID:           teamID,
			TeamNameSnapshot: "Filter Test",
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		}
		if err := s.CreateAlertGroup(ag); err != nil {
			t.Fatalf("CreateAlertGroup failed: %v", err)
		}
	}

	createAG(ag1ID)
	createAG(ag2ID)

	// Mark ag2 as processed
	if err := s.MarkAckProcessed(ag2ID); err != nil {
		t.Fatalf("MarkAckProcessed failed: %v", err)
	}

	// GetAcknowledgedAlertGroups should only return ag1 (unprocessed)
	ags, err := s.GetAcknowledgedAlertGroups()
	if err != nil {
		t.Fatalf("GetAcknowledgedAlertGroups failed: %v", err)
	}

	// Should only find ag1
	foundAG1 := false
	foundAG2 := false
	for _, ag := range ags {
		if ag.ID == ag1ID {
			foundAG1 = true
		}
		if ag.ID == ag2ID {
			foundAG2 = true
		}
	}

	if !foundAG1 {
		t.Error("Unprocessed AG should be returned by GetAcknowledgedAlertGroups")
	}
	if foundAG2 {
		t.Error("Processed AG should NOT be returned by GetAcknowledgedAlertGroups")
	}
}

// TestReAck_ShouldClearAckProcessedAt_Integration tests that re-acknowledging
// an AG clears ack_processed_at to allow retry
//
// ISSUE: UpdateAlertGroupAcknowledged doesn't clear ack_processed_at
func TestReAck_ShouldClearAckProcessedAt_Integration(t *testing.T) {
	s := setupTestDB(t)

	// Setup
	teamID := "team-reack-test"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "ReAck Test", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "key-reack-" + agID,
		Status:           model.AlertGroupStatusTriggered,
		TeamID:           teamID,
		TeamNameSnapshot: "ReAck Test",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup failed: %v", err)
	}

	// First ack
	if err := s.UpdateAlertGroupAcknowledged(agID, "user1"); err != nil {
		t.Fatalf("First ack failed: %v", err)
	}

	// Mark as processed
	if err := s.MarkAckProcessed(agID); err != nil {
		t.Fatalf("MarkAckProcessed failed: %v", err)
	}

	// Verify it's processed
	fetched, _ := s.GetAlertGroupByID(agID)
	if fetched.AckProcessedAt == nil {
		t.Log("Note: GetAlertGroupByID may not select ack_processed_at - checking via GetAcknowledgedAlertGroups instead")
	}

	// Verify AG is NOT in GetAcknowledgedAlertGroups (it's processed)
	ags, _ := s.GetAcknowledgedAlertGroups()
	for _, ag := range ags {
		if ag.ID == agID {
			t.Fatal("Setup error: processed AG should not be in GetAcknowledgedAlertGroups")
		}
	}

	// Re-ack (user wants to retry)
	if err := s.UpdateAlertGroupAcknowledged(agID, "user2"); err != nil {
		t.Fatalf("Re-ack failed: %v", err)
	}

	// AG should now be in GetAcknowledgedAlertGroups again (ack_processed_at cleared)
	ags, _ = s.GetAcknowledgedAlertGroups()
	found := false
	for _, ag := range ags {
		if ag.ID == agID {
			found = true
			break
		}
	}
	if !found {
		t.Error("Re-acked AG should be returned by GetAcknowledgedAlertGroups - UpdateAlertGroupAcknowledged should clear ack_processed_at!")
	}
}

// ===================================================================================
// AckAlertGroupAtomic Integration Tests
// ===================================================================================

func createTestTeamAndAG(t *testing.T, s *Store, teamID string, agStatus model.AlertGroupStatus) string {
	t.Helper()
	// Create team if not exists (ignore error if duplicate)
	s.CreateTeam(&model.Team{ID: teamID, Name: teamID, CreatedAt: time.Now()})

	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "key-" + agID,
		Status:           agStatus,
		TeamID:           teamID,
		TeamNameSnapshot: teamID,
		Severity:         "critical",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup failed: %v", err)
	}
	return agID
}

// ===================================================================================
// CreateAlertGroupAtomic Integration Tests
// ===================================================================================

func TestCreateAlertGroupAtomic_Success(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-atomic-create"
	teamName := "Atomic Create Team"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamName, CreatedAt: time.Now()})

	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "key-" + agID,
		Status:           model.AlertGroupStatusNew,
		Title:            "Test AG",
		TeamID:           teamID,
		TeamNameSnapshot: teamName,
		Severity:         "critical",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		Alerts:           []model.Alert{{Fingerprint: "fp1"}, {Fingerprint: "fp2"}},
	}

	now := time.Now()
	timelineEvents := []*model.TimelineEvent{
		{
			ID:           uuid.New().String(),
			AlertGroupID: agID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created: Test AG",
			Actor:        "system",
			Metadata:     map[string]string{"team": teamID, "severity": "critical"},
			CreatedAt:    now,
		},
		{
			ID:           uuid.New().String(),
			AlertGroupID: agID,
			Type:         model.TimelineEventAlertAdded,
			Message:      "Alert: TestAlert1",
			Actor:        "system",
			Metadata:     map[string]string{"fingerprint": "fp1"},
			CreatedAt:    now.Add(1 * time.Microsecond),
		},
		{
			ID:           uuid.New().String(),
			AlertGroupID: agID,
			Type:         model.TimelineEventAlertAdded,
			Message:      "Alert: TestAlert2",
			Actor:        "system",
			Metadata:     map[string]string{"fingerprint": "fp2"},
			CreatedAt:    now.Add(2 * time.Microsecond),
		},
	}

	// Build typed payload for the outbox event
	eventPayload, err := model.BuildWebhookEventPayload(
		model.OutboxEventFiring, ag, teamName, "system", "", now,
	)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload failed: %v", err)
	}

	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventFiring,
		AlertGroupID: agID,
		TeamID:       teamID,
		Actor:        "system",
		Payload:      eventPayload,
	}

	if err := s.CreateAlertGroupAtomic(ag, timelineEvents, outboxEvent); err != nil {
		t.Fatalf("CreateAlertGroupAtomic failed: %v", err)
	}

	// Verify AG
	fetched, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if fetched.Status != model.AlertGroupStatusNew {
		t.Errorf("Expected status 'new', got '%s'", fetched.Status)
	}

	// Verify timeline events — ordering via µs offsets
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents failed: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("Expected 3 timeline events, got %d", len(events))
	}
	if events[0].Type != model.TimelineEventCreated {
		t.Errorf("Expected first event type 'created', got '%s'", events[0].Type)
	}
	if events[1].Type != model.TimelineEventAlertAdded {
		t.Errorf("Expected second event type 'alert_added', got '%s'", events[1].Type)
	}
	if events[2].Type != model.TimelineEventAlertAdded {
		t.Errorf("Expected third event type 'alert_added', got '%s'", events[2].Type)
	}
	// Verify strict ordering (µs offsets)
	if !events[0].CreatedAt.Before(events[1].CreatedAt) {
		t.Error("Expected events[0].CreatedAt < events[1].CreatedAt")
	}
	if !events[1].CreatedAt.Before(events[2].CreatedAt) {
		t.Error("Expected events[1].CreatedAt < events[2].CreatedAt")
	}

	// Verify outbox event
	if outboxEvent.ID == "" {
		t.Fatal("Expected outbox event ID to be set")
	}
	outbox, err := s.GetOutboxEventByID(outboxEvent.ID)
	if err != nil {
		t.Fatalf("GetOutboxEventByID failed: %v", err)
	}
	if outbox.EventType != model.OutboxEventFiring {
		t.Errorf("Expected event type 'alert_group.firing', got '%s'", outbox.EventType)
	}
	if outbox.AlertGroupID != agID {
		t.Errorf("Expected alert_group_id '%s', got '%s'", agID, outbox.AlertGroupID)
	}
	if outbox.TeamID != teamID {
		t.Errorf("Expected team_id '%s', got '%s'", teamID, outbox.TeamID)
	}
	if outbox.Status != model.OutboxEventStatusPending {
		t.Errorf("Expected status 'pending', got '%s'", outbox.Status)
	}

	// Verify outbox payload round-trip: typed webhook contract fields
	var p model.WebhookEventPayload
	if err := json.Unmarshal(outbox.Payload, &p); err != nil {
		t.Fatalf("Failed to unmarshal outbox payload: %v", err)
	}
	if p.AlertGroup.Status != "firing" {
		t.Errorf("Expected payload status 'firing', got %q", p.AlertGroup.Status)
	}
	if p.AlertGroup.TeamName != teamName {
		t.Errorf("Expected payload team_name %q, got %q", teamName, p.AlertGroup.TeamName)
	}
	if p.Actor.Name != "system" {
		t.Errorf("Expected payload actor.name 'system', got %q", p.Actor.Name)
	}
	// Verify no external_url in raw payload
	var rawMap map[string]json.RawMessage
	json.Unmarshal(outbox.Payload, &rawMap)
	var agMap map[string]json.RawMessage
	json.Unmarshal(rawMap["alert_group"], &agMap)
	if _, exists := agMap["external_url"]; exists {
		t.Error("Expected no 'external_url' field in outbox payload")
	}
}

func TestCreateAlertGroupAtomic_Rollback(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-atomic-rollback"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamID, CreatedAt: time.Now()})

	// Create first AG
	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID: agID, DedupKey: "key-" + agID, Status: model.AlertGroupStatusNew,
		Title: "First", TeamID: teamID, TeamNameSnapshot: teamID, Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("setup CreateAlertGroup failed: %v", err)
	}

	// Try to create duplicate AG atomically — should fail on duplicate key
	dupAG := &model.AlertGroup{
		ID: agID, DedupKey: "key-dup", Status: model.AlertGroupStatusNew,
		Title: "Dup", TeamID: teamID, TeamNameSnapshot: teamID, Severity: "warning",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	tlID := uuid.New().String()
	timelineEvents := []*model.TimelineEvent{
		{ID: tlID, AlertGroupID: agID, Type: model.TimelineEventCreated,
			Message: "should not exist", Actor: "system", CreatedAt: time.Now()},
	}
	outboxEvent := &model.OutboxEvent{
		EventType: model.OutboxEventFiring, AlertGroupID: agID,
		TeamID: teamID, Actor: "system",
	}

	err := s.CreateAlertGroupAtomic(dupAG, timelineEvents, outboxEvent)
	if err == nil {
		t.Fatal("Expected error for duplicate AG ID")
	}

	// Verify no partial timeline event leaked
	events, _ := s.GetTimelineEvents(agID)
	for _, ev := range events {
		if ev.ID == tlID {
			t.Error("Timeline event should have been rolled back")
		}
	}

	// Verify no partial outbox event leaked
	if outboxEvent.ID != "" {
		_, outboxErr := s.GetOutboxEventByID(outboxEvent.ID)
		if outboxErr == nil {
			t.Error("Outbox event should have been rolled back")
		}
	}
}

func TestAckAlertGroupAtomic_Success(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-ack", model.AlertGroupStatusTriggered)

	meta := map[string]string{"source": "slack"}
	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", meta, nil)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic failed: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true for triggered->acknowledged")
	}

	// Verify status
	ag, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status 'acknowledged', got '%s'", ag.Status)
	}
	if ag.AcknowledgedBy != "TestUser" {
		t.Errorf("Expected acknowledged_by 'TestUser', got '%s'", ag.AcknowledgedBy)
	}

	// Verify timeline event
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents failed: %v", err)
	}
	ackCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventAcknowledged {
			ackCount++
			if ev.Actor != "TestUser" {
				t.Errorf("Expected actor 'TestUser', got '%s'", ev.Actor)
			}
			if ev.Metadata == nil || ev.Metadata["source"] != "slack" {
				t.Errorf("Expected metadata {source: slack}, got %v", ev.Metadata)
			}
		}
	}
	if ackCount != 1 {
		t.Errorf("Expected 1 ack timeline event, got %d", ackCount)
	}
}

func TestAckAlertGroupAtomic_AlreadyAcked(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-ack2", model.AlertGroupStatusAcknowledged)

	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic failed: %v", err)
	}
	if changed {
		t.Error("Expected changed=false for already acknowledged AG")
	}

	// Verify no timeline event was created
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents failed: %v", err)
	}
	for _, ev := range events {
		if ev.Type == model.TimelineEventAcknowledged {
			t.Error("Should not create timeline event for already acked AG")
		}
	}
}

func TestAckAlertGroupAtomic_Resolved(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-ack3", model.AlertGroupStatusResolved)

	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic failed: %v", err)
	}
	if changed {
		t.Error("Expected changed=false for resolved AG")
	}
}

func TestResolveAlertGroupAtomic_FromTriggered(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-res1", model.AlertGroupStatusTriggered)

	meta := map[string]string{"source": "slack"}
	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", meta, nil)
	if err != nil {
		t.Fatalf("ResolveAlertGroupAtomic failed: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true for triggered->resolved")
	}

	ag, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if ag.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", ag.Status)
	}
	if ag.ResolvedAt == nil {
		t.Error("Expected resolved_at to be set")
	}

	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents failed: %v", err)
	}
	resolveCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventResolved {
			resolveCount++
			if ev.Metadata == nil || ev.Metadata["source"] != "slack" {
				t.Errorf("Expected metadata {source: slack}, got %v", ev.Metadata)
			}
		}
	}
	if resolveCount != 1 {
		t.Errorf("Expected 1 resolve timeline event, got %d", resolveCount)
	}
}

func TestResolveAlertGroupAtomic_FromAcknowledged(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-res2", model.AlertGroupStatusAcknowledged)

	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil {
		t.Fatalf("ResolveAlertGroupAtomic failed: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true for acknowledged->resolved")
	}

	ag, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if ag.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", ag.Status)
	}
}

func TestResolveAlertGroupAtomic_AlreadyResolved(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-res3", model.AlertGroupStatusResolved)

	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil {
		t.Fatalf("ResolveAlertGroupAtomic failed: %v", err)
	}
	if changed {
		t.Error("Expected changed=false for already resolved AG")
	}
}

func TestAckAtomicConcurrent(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-atomic-race", model.AlertGroupStatusTriggered)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	winners := make([]bool, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			changed, err := s.AckAlertGroupAtomic(agID, "User"+uuid.New().String()[:4], nil, nil)
			winners[idx] = changed
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	// Exactly 1 winner
	winCount := 0
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if winners[i] {
			winCount++
		}
	}
	if winCount != 1 {
		t.Errorf("Expected exactly 1 winner, got %d", winCount)
	}

	// Exactly 1 ack timeline event
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents failed: %v", err)
	}
	ackCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventAcknowledged {
			ackCount++
		}
	}
	if ackCount != 1 {
		t.Errorf("Expected exactly 1 ack timeline event, got %d", ackCount)
	}
}

// ===================================================================================
// Ack/Resolve from Processing Status
// ===================================================================================

func TestAckAlertGroupAtomic_FromProcessing(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-ack-proc", model.AlertGroupStatusProcessing)

	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic failed: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true for processing->acknowledged")
	}

	ag, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected status 'acknowledged', got '%s'", ag.Status)
	}
	if ag.AcknowledgedBy != "TestUser" {
		t.Errorf("Expected acknowledged_by 'TestUser', got '%s'", ag.AcknowledgedBy)
	}

	events, _ := s.GetTimelineEvents(agID)
	ackCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventAcknowledged {
			ackCount++
		}
	}
	if ackCount != 1 {
		t.Errorf("Expected 1 ack timeline event, got %d", ackCount)
	}
}

func TestResolveAlertGroupAtomic_FromProcessing(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-res-proc", model.AlertGroupStatusProcessing)

	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil {
		t.Fatalf("ResolveAlertGroupAtomic failed: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true for processing->resolved")
	}

	ag, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if ag.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", ag.Status)
	}
	if ag.ResolvedAt == nil {
		t.Error("Expected resolved_at to be set")
	}

	events, _ := s.GetTimelineEvents(agID)
	resolveCount := 0
	for _, ev := range events {
		if ev.Type == model.TimelineEventResolved {
			resolveCount++
		}
	}
	if resolveCount != 1 {
		t.Errorf("Expected 1 resolve timeline event, got %d", resolveCount)
	}
}

// ===================================================================================
// TransitionAlertGroupStatus Tests
// ===================================================================================

func TestTransitionAlertGroupStatus_Success(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-transition-ok", model.AlertGroupStatusProcessing)

	changed, err := s.TransitionAlertGroupStatus(agID,
		model.AlertGroupStatusProcessing, model.AlertGroupStatusTriggered)
	if err != nil {
		t.Fatalf("TransitionAlertGroupStatus failed: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true")
	}

	ag, _ := s.GetAlertGroupByID(agID)
	if ag.Status != model.AlertGroupStatusTriggered {
		t.Errorf("Expected 'triggered', got '%s'", ag.Status)
	}
}

func TestTransitionAlertGroupStatus_WrongFromStatus(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-transition-noop", model.AlertGroupStatusAcknowledged)

	changed, err := s.TransitionAlertGroupStatus(agID,
		model.AlertGroupStatusProcessing, model.AlertGroupStatusTriggered)
	if err != nil {
		t.Fatalf("TransitionAlertGroupStatus failed: %v", err)
	}
	if changed {
		t.Error("Expected changed=false when fromStatus does not match")
	}

	ag, _ := s.GetAlertGroupByID(agID)
	if ag.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Status should be unchanged, got '%s'", ag.Status)
	}
}

// ===================================================================================
// EnsureEscalationJob Integration Tests
// ===================================================================================

// findEscalationJob is the local answer to "which job is this group's
// escalation". The store used to expose a lookup by dedup key; nothing in
// production ever called it, so it went away with the model rather than being
// renamed into a lookup by identity nobody would call either.
func findEscalationJob(t *testing.T, s *Store, agID string) *model.Job {
	t.Helper()
	var jobID string
	err := s.GetDB().QueryRow(
		`SELECT id FROM jobs WHERE type = 'escalation' AND alert_group_id = $1`, agID).Scan(&jobID)
	if err != nil {
		t.Fatalf("no escalation job for alert group %s: %v", agID, err)
	}
	job, err := s.GetJobByID(jobID)
	if err != nil {
		t.Fatalf("GetJobByID(%s): %v", jobID, err)
	}
	return job
}

// makeTestJob builds an escalation job the way the real builder does - which,
// since the family became a closed registry, means NOT naming its identity, its
// type or its alert group: EnsureEscalationJob derives all three from the group
// it locks. The parameter is here for the stage and step fixtures.
func makeTestJob(agID string) (*model.Job, []*model.JobStage, []*model.JobStep, *model.EscalationPolicySnapshot) {
	now := time.Now()
	jobID := uuid.New().String()
	stageID := uuid.New().String()
	stepID := uuid.New().String()

	job := &model.Job{
		ID:           jobID,
		Status:       model.JobStatusPending,
		CurrentStage: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	stages := []*model.JobStage{
		{
			ID:         stageID,
			JobID:      jobID,
			StageIndex: 0,
			Status:     model.JobStageStatusActive,
			CreatedAt:  now,
			UpdatedAt:  now,
		},
	}
	steps := []*model.JobStep{
		{
			ID:          stepID,
			JobID:       jobID,
			StageID:     stageID,
			StepIndex:   0,
			StepType:    "dm",
			Status:      model.JobStepStatusPending,
			MaxAttempts: 3,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
	}
	snapshot := &model.EscalationPolicySnapshot{
		PolicyID: "test-policy",
		Name:     "Test Policy",
		Steps: []*model.EscalationStepSnapshot{
			{Provider: "slack", TargetKind: "dm", TargetType: "user", TargetID: "U999"},
		},
	}
	return job, stages, steps, snapshot
}

func TestEnsureEscalationJob_CreatesJobAndSnapshot(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-ensure-create", model.AlertGroupStatusNew)
	job, stages, steps, snapshot := makeTestJob(agID)

	created, err := s.EnsureEscalationJob(agID, job, stages, steps, snapshot)
	if err != nil {
		t.Fatalf("EnsureEscalationJob failed: %v", err)
	}
	if !created {
		t.Error("Expected created=true for new AG")
	}

	// Verify AG is now processing with snapshot
	updated, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID failed: %v", err)
	}
	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status 'processing', got '%s'", updated.Status)
	}
	if updated.PolicyID != "test-policy" {
		t.Errorf("Expected policy_id 'test-policy', got '%s'", updated.PolicyID)
	}
	if updated.PolicySnapshot == nil {
		t.Fatal("Expected policy snapshot to be saved")
	}
	if len(updated.PolicySnapshot.Steps) != 1 || updated.PolicySnapshot.Steps[0].TargetID != "U999" {
		t.Errorf("Snapshot has wrong data: %+v", updated.PolicySnapshot)
	}

	// Verify job exists
	fetchedJob := findEscalationJob(t, s, agID)
	if fetchedJob.Type != "escalation" {
		t.Errorf("Expected job type 'escalation', got '%s'", fetchedJob.Type)
	}
}

func TestEnsureEscalationJob_DedupSkipsSnapshotOverwrite(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-ensure-dedup", model.AlertGroupStatusNew)

	// First call — creates job with V1 snapshot
	job1, stages1, steps1, snapshot1 := makeTestJob(agID)
	snapshot1.Steps[0].TargetID = "UserV1"

	created, err := s.EnsureEscalationJob(agID, job1, stages1, steps1, snapshot1)
	if err != nil {
		t.Fatalf("First EnsureEscalationJob failed: %v", err)
	}
	if !created {
		t.Fatal("Expected first call to create")
	}

	// Force AG back to "new" to allow re-processing
	s.UpdateAlertGroupStatus(agID, model.AlertGroupStatusNew)

	// Second call — same dedup_key, V2 snapshot
	job2, stages2, steps2, snapshot2 := makeTestJob(agID)
	snapshot2.Steps[0].TargetID = "UserV2"

	created, err = s.EnsureEscalationJob(agID, job2, stages2, steps2, snapshot2)
	if err != nil {
		t.Fatalf("Second EnsureEscalationJob failed: %v", err)
	}
	if created {
		t.Error("Expected created=false on dedup conflict")
	}

	// Verify snapshot is still V1
	updated, _ := s.GetAlertGroupByID(agID)
	if updated.PolicySnapshot == nil {
		t.Fatal("Snapshot should still exist")
	}
	if updated.PolicySnapshot.Steps[0].TargetID != "UserV1" {
		t.Errorf("Snapshot should stay UserV1, got %s", updated.PolicySnapshot.Steps[0].TargetID)
	}
}

func TestEnsureEscalationJob_UserWins_ConcurrentAck(t *testing.T) {
	s := setupTestDB(t)

	// Start AG in "processing" so both EnsureEscalationJob and AckAlertGroupAtomic
	// can legitimately operate on it — this is the real race window.
	agID := createTestTeamAndAG(t, s, "team-ensure-race-ack", model.AlertGroupStatusProcessing)

	const iterations = 20
	for i := 0; i < iterations; i++ {
		// Reset AG to processing for each iteration
		s.UpdateAlertGroupStatus(agID, model.AlertGroupStatusProcessing)
		// Cancel any existing active job so dedup doesn't interfere
		s.CancelEscalationJobByAlertGroupID(agID)

		var wg sync.WaitGroup
		wg.Add(2)

		var ackChanged bool
		var ackErr error
		var ensureCreated bool
		var ensureErr error

		// Goroutine 1: user acks the AG
		go func() {
			defer wg.Done()
			ackChanged, ackErr = s.AckAlertGroupAtomic(agID, "UserWins", nil, nil)
		}()

		// Goroutine 2: engine ensures escalation job
		go func() {
			defer wg.Done()
			job, stages, steps, snapshot := makeTestJob(agID)
			ensureCreated, ensureErr = s.EnsureEscalationJob(agID, job, stages, steps, snapshot)
		}()

		wg.Wait()

		if ackErr != nil {
			t.Fatalf("iteration %d: AckAlertGroupAtomic error: %v", i, ackErr)
		}
		if ensureErr != nil {
			t.Fatalf("iteration %d: EnsureEscalationJob error: %v", i, ensureErr)
		}

		// Invariant: exactly one wins. They must not both succeed.
		// - If ack wins: ackChanged=true, ensureCreated=false, status=acknowledged
		// - If ensure wins: ensureCreated=true, ackChanged may be true (ack from processing is valid),
		//   but status ends up acknowledged (ack runs after ensure's commit)
		//   OR ackChanged=false if ack saw acknowledged/other non-eligible status.
		//
		// The critical invariant: if ensureCreated=true, the AG must NOT silently
		// lose the ack (i.e., stay processing without the ack timeline event).
		updated, _ := s.GetAlertGroupByID(agID)

		if ackChanged && ensureCreated {
			// Both succeeded — this is valid only if ensure ran first (AG: processing→processing+job),
			// then ack ran (AG: processing→acknowledged). Final status must be acknowledged.
			if updated.Status != model.AlertGroupStatusAcknowledged {
				t.Fatalf("iteration %d: both succeeded but status is '%s', expected 'acknowledged'",
					i, updated.Status)
			}
		} else if ackChanged && !ensureCreated {
			// Ack won, ensure saw non-eligible status
			if updated.Status != model.AlertGroupStatusAcknowledged {
				t.Fatalf("iteration %d: ack won but status is '%s'", i, updated.Status)
			}
		} else if !ackChanged && ensureCreated {
			// Ensure won, ack saw already-acknowledged or lost the race.
			// Status should be processing (ensure set it, ack failed).
			if updated.Status != model.AlertGroupStatusProcessing {
				t.Fatalf("iteration %d: ensure won but status is '%s'", i, updated.Status)
			}
		} else {
			// Neither succeeded — shouldn't happen from processing status
			t.Fatalf("iteration %d: neither ack nor ensure succeeded (ack=%v, ensure=%v, status=%s)",
				i, ackChanged, ensureCreated, updated.Status)
		}
	}
}

func TestEnsureEscalationJob_ConcurrentRace(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-ensure-concurrent", model.AlertGroupStatusNew)

	const goroutines = 5
	var wg sync.WaitGroup
	wg.Add(goroutines)

	results := make([]bool, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			job, stages, steps, snapshot := makeTestJob(agID)
			created, err := s.EnsureEscalationJob(agID, job, stages, steps, snapshot)
			results[idx] = created
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	createdCount := 0
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if results[i] {
			createdCount++
		}
	}

	if createdCount != 1 {
		t.Errorf("Expected exactly 1 created=true, got %d", createdCount)
	}

	// Verify AG is processing
	updated, _ := s.GetAlertGroupByID(agID)
	if updated.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status 'processing', got '%s'", updated.Status)
	}
}

func TestEnsureEscalationJob_BlockedBySucceededJob(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-blocked-succeeded", model.AlertGroupStatusNew)
	job, stages, steps, snapshot := makeTestJob(agID)
	// Set AlertGroupID on the job
	job.AlertGroupID = &agID

	// First call — creates escalation job
	created, err := s.EnsureEscalationJob(agID, job, stages, steps, snapshot)
	if err != nil {
		t.Fatalf("First EnsureEscalationJob failed: %v", err)
	}
	if !created {
		t.Fatal("Expected first call to create")
	}

	// Mark job as succeeded
	_, err = s.GetDB().Exec(`UPDATE jobs SET status = 'succeeded', finished_at = NOW() WHERE id = $1`, job.ID)
	if err != nil {
		t.Fatalf("Failed to mark job as succeeded: %v", err)
	}

	// Force AG back to "new" to allow re-processing
	s.UpdateAlertGroupStatus(agID, model.AlertGroupStatusNew)

	// Second call — should be blocked by the unique index (succeeded job exists)
	job2, stages2, steps2, snapshot2 := makeTestJob(agID)
	job2.AlertGroupID = &agID

	created, err = s.EnsureEscalationJob(agID, job2, stages2, steps2, snapshot2)
	if err != nil {
		t.Fatalf("Second EnsureEscalationJob failed: %v", err)
	}
	if created {
		t.Error("Expected created=false — DB invariant should block second escalation job for same AG")
	}
}

func TestGetNewAlertGroups_SkipsAGWithEscalationJob(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-skip-escalation"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Skip Escalation", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Create AG in stale processing with succeeded escalation job (via alert_group_id)
	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID: agID, DedupKey: "dk-skip-" + agID, Status: model.AlertGroupStatusProcessing,
		TeamID: teamID, TeamNameSnapshot: "Skip Test", CreatedAt: time.Now(), UpdatedAt: time.Now().Add(-60 * time.Second),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup failed: %v", err)
	}

	// Backdate updated_at
	_, err := s.GetDB().Exec(`UPDATE alert_groups SET updated_at = $1 WHERE id = $2`,
		time.Now().Add(-60*time.Second), agID)
	if err != nil {
		t.Fatalf("Failed to backdate AG: %v", err)
	}

	// Create a succeeded escalation job linked to this AG via alert_group_id
	now := time.Now()
	jobID := uuid.New().String()
	_, err = s.GetDB().Exec(`
		INSERT INTO jobs (id, type, status, payload, dedup_namespace, dedup_key, dedup_scope,
			alert_group_id, current_stage, created_at, updated_at, finished_at)
		VALUES ($1, 'escalation', 'succeeded', '{}', 'escalation', $2, 'forever', $2, 0, $3, $3, $3)`,
		jobID, agID, now)
	if err != nil {
		t.Fatalf("Failed to create succeeded job: %v", err)
	}

	// GetNewAlertGroups should NOT return this AG
	results, err := s.GetNewAlertGroups()
	if err != nil {
		t.Fatalf("GetNewAlertGroups failed: %v", err)
	}

	for _, r := range results {
		if r.ID == agID {
			t.Error("Expected AG with succeeded escalation job (via alert_group_id) to be excluded from GetNewAlertGroups")
		}
	}
}

func TestGetNewAlertGroups_IncludesStaleProcessing(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-reconcile"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Reconcile", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// AG1: "new" — always returned
	ag1 := &model.AlertGroup{
		ID: "ag-new-1", DedupKey: "dk-new-1", Status: model.AlertGroupStatusNew,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	// AG2: "processing" with fresh updated_at — should NOT be returned
	ag2 := &model.AlertGroup{
		ID: "ag-proc-fresh", DedupKey: "dk-proc-fresh", Status: model.AlertGroupStatusProcessing,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	// AG3: "processing" with stale updated_at (> 30s ago) — should be returned
	ag3 := &model.AlertGroup{
		ID: "ag-proc-stale", DedupKey: "dk-proc-stale", Status: model.AlertGroupStatusProcessing,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now().Add(-60 * time.Second),
	}
	// AG4: "triggered" — should NOT be returned
	ag4 := &model.AlertGroup{
		ID: "ag-triggered", DedupKey: "dk-triggered", Status: model.AlertGroupStatusTriggered,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now().Add(-60 * time.Second),
	}

	for _, ag := range []*model.AlertGroup{ag1, ag2, ag3, ag4} {
		if err := s.CreateAlertGroup(ag); err != nil {
			t.Fatalf("Failed to create AG %s: %v", ag.ID, err)
		}
	}

	// Backdate ag3's updated_at directly (CreateAlertGroup may overwrite it)
	_, err := s.GetDB().Exec(`UPDATE alert_groups SET updated_at = $1 WHERE id = $2`,
		time.Now().Add(-60*time.Second), "ag-proc-stale")
	if err != nil {
		t.Fatalf("Failed to backdate AG: %v", err)
	}

	results, err := s.GetNewAlertGroups()
	if err != nil {
		t.Fatalf("GetNewAlertGroups failed: %v", err)
	}

	ids := make(map[string]bool)
	for _, ag := range results {
		ids[ag.ID] = true
	}

	if !ids["ag-new-1"] {
		t.Error("Expected ag-new-1 (status=new) to be returned")
	}
	if !ids["ag-proc-stale"] {
		t.Error("Expected ag-proc-stale (stale processing) to be returned")
	}
	if ids["ag-proc-fresh"] {
		t.Error("Did not expect ag-proc-fresh (fresh processing) to be returned")
	}
	if ids["ag-triggered"] {
		t.Error("Did not expect ag-triggered to be returned")
	}
}

// ===================================================================================
// Ack/Resolve Outbox Event Integration Tests
// ===================================================================================

func TestAckAlertGroupAtomic_OutboxEvent(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-ack-outbox"
	teamName := "Ack Outbox Team"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamName, CreatedAt: time.Now()})

	agID := createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusTriggered)

	now := time.Now()
	ag, _ := s.GetAlertGroupByID(agID)
	eventPayload, err := model.BuildWebhookEventPayload(
		model.OutboxEventAcknowledged, ag, teamName, "TestUser", "test@example.com", now,
	)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload: %v", err)
	}
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventAcknowledged,
		AlertGroupID: agID,
		TeamID:       teamID,
		Actor:        "TestUser",
		Payload:      eventPayload,
	}

	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", nil, outboxEvent)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true")
	}

	// Verify outbox event was persisted
	pending, err := s.GetPendingOutboxEvents(10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents: %v", err)
	}
	var found *model.OutboxEvent
	for _, ev := range pending {
		if ev.AlertGroupID == agID && ev.EventType == model.OutboxEventAcknowledged {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("Expected outbox event to be persisted")
	}

	var payload model.WebhookEventPayload
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if payload.AlertGroup.Status != "acknowledged" {
		t.Errorf("Expected status 'acknowledged', got %q", payload.AlertGroup.Status)
	}
	if payload.AlertGroup.TeamName != teamName {
		t.Errorf("Expected team_name %q, got %q", teamName, payload.AlertGroup.TeamName)
	}
	if payload.Actor.Name != "TestUser" {
		t.Errorf("Expected actor.name 'TestUser', got %q", payload.Actor.Name)
	}
	if payload.Actor.Email != "test@example.com" {
		t.Errorf("Expected actor.email 'test@example.com', got %q", payload.Actor.Email)
	}
}

func TestAckAlertGroupAtomic_NoOutboxWhenNotChanged(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-ack-nooutbox"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamID, CreatedAt: time.Now()})

	// Already acknowledged AG
	agID := createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusAcknowledged)

	ag, _ := s.GetAlertGroupByID(agID)
	eventPayload, _ := model.BuildWebhookEventPayload(
		model.OutboxEventAcknowledged, ag, teamID, "TestUser", "test@example.com", time.Now(),
	)
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventAcknowledged,
		AlertGroupID: agID,
		TeamID:       teamID,
		Actor:        "TestUser",
		Payload:      eventPayload,
	}

	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", nil, outboxEvent)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic: %v", err)
	}
	if changed {
		t.Error("Expected changed=false for already-acked AG")
	}

	// Outbox event should NOT have been set (TX rolled back)
	if outboxEvent.ID != "" {
		_, err := s.GetOutboxEventByID(outboxEvent.ID)
		if err == nil {
			t.Error("Outbox event should NOT be persisted when changed=false")
		}
	}

	// Double check via GetPendingOutboxEvents
	pending, _ := s.GetPendingOutboxEvents(100)
	for _, ev := range pending {
		if ev.AlertGroupID == agID && ev.EventType == model.OutboxEventAcknowledged {
			t.Error("Should not have outbox event for unchanged ack")
		}
	}
}

func TestResolveAlertGroupAtomic_OutboxEvent(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-res-outbox"
	teamName := "Resolve Outbox Team"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamName, CreatedAt: time.Now()})

	agID := createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusTriggered)

	now := time.Now()
	ag, _ := s.GetAlertGroupByID(agID)
	eventPayload, err := model.BuildWebhookEventPayload(
		model.OutboxEventResolved, ag, teamName, "TestUser", "test@example.com", now,
	)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload: %v", err)
	}
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventResolved,
		AlertGroupID: agID,
		TeamID:       teamID,
		Actor:        "TestUser",
		Payload:      eventPayload,
	}

	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", nil, outboxEvent)
	if err != nil {
		t.Fatalf("ResolveAlertGroupAtomic: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true")
	}

	pending, err := s.GetPendingOutboxEvents(10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents: %v", err)
	}
	var found *model.OutboxEvent
	for _, ev := range pending {
		if ev.AlertGroupID == agID && ev.EventType == model.OutboxEventResolved {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("Expected outbox event to be persisted")
	}

	var payload model.WebhookEventPayload
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if payload.AlertGroup.Status != "resolved" {
		t.Errorf("Expected status 'resolved', got %q", payload.AlertGroup.Status)
	}
	if payload.AlertGroup.TeamName != teamName {
		t.Errorf("Expected team_name %q, got %q", teamName, payload.AlertGroup.TeamName)
	}
}

func TestResolveAlertGroupAtomic_NoOutboxWhenNotChanged(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-res-nooutbox"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamID, CreatedAt: time.Now()})

	agID := createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusResolved)

	ag, _ := s.GetAlertGroupByID(agID)
	eventPayload, _ := model.BuildWebhookEventPayload(
		model.OutboxEventResolved, ag, teamID, "TestUser", "test@example.com", time.Now(),
	)
	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventResolved,
		AlertGroupID: agID,
		TeamID:       teamID,
		Actor:        "TestUser",
		Payload:      eventPayload,
	}

	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", nil, outboxEvent)
	if err != nil {
		t.Fatalf("ResolveAlertGroupAtomic: %v", err)
	}
	if changed {
		t.Error("Expected changed=false for already-resolved AG")
	}

	pending, _ := s.GetPendingOutboxEvents(100)
	for _, ev := range pending {
		if ev.AlertGroupID == agID && ev.EventType == model.OutboxEventResolved {
			t.Error("Should not have outbox event for unchanged resolve")
		}
	}
}

func TestAckAtomicConcurrent_WithOutbox(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-ack-race-outbox"
	teamName := "Race Outbox Team"
	s.CreateTeam(&model.Team{ID: teamID, Name: teamName, CreatedAt: time.Now()})

	agID := createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusTriggered)
	ag, _ := s.GetAlertGroupByID(agID)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	winners := make([]bool, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			actor := "User" + uuid.New().String()[:4]
			payload, _ := model.BuildWebhookEventPayload(
				model.OutboxEventAcknowledged, ag, teamName, actor, actor+"@example.com", time.Now(),
			)
			oe := &model.OutboxEvent{
				EventType:    model.OutboxEventAcknowledged,
				AlertGroupID: agID,
				TeamID:       teamID,
				Actor:        actor,
				Payload:      payload,
			}
			changed, err := s.AckAlertGroupAtomic(agID, actor, nil, oe)
			winners[idx] = changed
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	winCount := 0
	for i := 0; i < goroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if winners[i] {
			winCount++
		}
	}
	if winCount != 1 {
		t.Errorf("Expected exactly 1 winner, got %d", winCount)
	}

	// Exactly 1 outbox event for this AG
	pending, err := s.GetPendingOutboxEvents(100)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents: %v", err)
	}
	ackOutboxCount := 0
	for _, ev := range pending {
		if ev.AlertGroupID == agID && ev.EventType == model.OutboxEventAcknowledged {
			ackOutboxCount++
		}
	}
	if ackOutboxCount != 1 {
		t.Errorf("Expected exactly 1 ack outbox event, got %d", ackOutboxCount)
	}
}

// ===================================================================================
// ResolveAlertGroupWithAlertsAtomic Tests
// ===================================================================================

// TestResolveAlertGroupWithAlertsAtomic_FromNew verifies atomic resolve from "new" status
// with alerts_data update, timeline events, outbox event, and job cancellation.
func TestResolveAlertGroupWithAlertsAtomic_FromNew(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-resolve-alerts"
	s.CreateTeam(&model.Team{ID: teamID, Name: "Resolve Team", CreatedAt: time.Now()})

	agID := uuid.New().String()
	dedupKey := "dk-resolve-" + agID
	ag := &model.AlertGroup{
		ID: agID, DedupKey: dedupKey, Status: model.AlertGroupStatusNew,
		TeamID: teamID, TeamNameSnapshot: "Resolve Team", Severity: "warning",
		Alerts:    []model.Alert{{Fingerprint: "fp1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "CPU"}}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup: %v", err)
	}

	// Resolved alerts
	resolvedAlerts := []model.Alert{
		{Fingerprint: "fp1", Status: model.AlertStatusResolved, Labels: map[string]string{"alertname": "CPU"}},
	}

	timelineEvents := []*model.TimelineEvent{
		{
			ID: uuid.New().String(), AlertGroupID: agID,
			Type: model.TimelineEventAlertResolved, Message: "Alert resolved: CPU",
			Actor: "system", Metadata: map[string]string{"fingerprint": "fp1"},
			CreatedAt: time.Now(),
		},
		{
			ID: uuid.New().String(), AlertGroupID: agID,
			Type: model.TimelineEventResolved, Message: "Alert group resolved: all alerts cleared",
			Actor: "system", CreatedAt: time.Now().Add(time.Microsecond),
		},
	}

	outboxEvent := &model.OutboxEvent{
		EventType:    model.OutboxEventResolved,
		AlertGroupID: agID,
		TeamID:       teamID,
		Actor:        "system",
		Payload:      []byte(`{"event":"alert_group.resolved"}`),
	}

	changed, err := s.ResolveAlertGroupWithAlertsAtomic(agID, resolvedAlerts, timelineEvents, outboxEvent)
	if err != nil {
		t.Fatalf("ResolveAlertGroupWithAlertsAtomic: %v", err)
	}
	if !changed {
		t.Error("Expected changed=true for new->resolved")
	}

	// Verify status + resolved fields
	fetched, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if fetched.Status != model.AlertGroupStatusResolved {
		t.Errorf("Expected status 'resolved', got '%s'", fetched.Status)
	}
	if fetched.ResolvedAt == nil {
		t.Error("Expected resolved_at to be set")
	}
	if fetched.ResolvedBy != "system" {
		t.Errorf("Expected resolved_by 'system', got '%s'", fetched.ResolvedBy)
	}

	// Verify alerts_data updated
	if len(fetched.Alerts) != 1 || fetched.Alerts[0].Status != model.AlertStatusResolved {
		t.Errorf("Expected 1 resolved alert, got %v", fetched.Alerts)
	}

	// Verify timeline events
	events, _ := s.GetTimelineEvents(agID)
	var resolveCount, alertResolveCount int
	for _, ev := range events {
		if ev.Type == model.TimelineEventResolved {
			resolveCount++
		}
		if ev.Type == model.TimelineEventAlertResolved {
			alertResolveCount++
		}
	}
	if resolveCount != 1 {
		t.Errorf("Expected 1 resolve timeline event, got %d", resolveCount)
	}
	if alertResolveCount != 1 {
		t.Errorf("Expected 1 alert_resolved timeline event, got %d", alertResolveCount)
	}

	// Verify outbox event
	outboxEvents, _ := s.GetPendingOutboxEvents(10)
	var found bool
	for _, oe := range outboxEvents {
		if oe.AlertGroupID == agID && oe.EventType == model.OutboxEventResolved {
			found = true
		}
	}
	if !found {
		t.Error("Expected outbox event with type alert_group.resolved")
	}
}

// TestResolveAlertGroupWithAlertsAtomic_AlreadyResolved verifies idempotent behavior.
func TestResolveAlertGroupWithAlertsAtomic_AlreadyResolved(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-resolve-idem", model.AlertGroupStatusResolved)

	changed, err := s.ResolveAlertGroupWithAlertsAtomic(agID, nil, nil, nil)
	if err != nil {
		t.Fatalf("ResolveAlertGroupWithAlertsAtomic: %v", err)
	}
	if changed {
		t.Error("Expected changed=false for already resolved AG")
	}
}

// ===================================================================================
// Migration Tests
// ===================================================================================

// TestMigration_OrphanedTeam_BackfillSnapshot verifies that InitDB backfill
// handles AGs whose team was deleted (orphaned team_id) without crashing.
// The snapshot should fall back to team_id.
func TestMigration_OrphanedTeam_BackfillSnapshot(t *testing.T) {
	s := setupTestDB(t)

	// 1. Create a team and an AG referencing it
	teamID := "team-will-delete"
	s.CreateTeam(&model.Team{ID: teamID, Name: "Doomed Team", CreatedAt: time.Now()})

	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID: agID, DedupKey: "dk-orphan-" + agID, Status: model.AlertGroupStatusTriggered,
		TeamID: teamID, TeamNameSnapshot: "Doomed Team", Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("CreateAlertGroup: %v", err)
	}

	// 2. Delete the team (orphans the AG).
	//
	// Straight SQL: deleting a team is a scheduleconfig command now, and what
	// this test needs is the end state, not the guards around reaching it.
	// alert_groups.team_id carries no foreign key, which is what makes an
	// orphaned group possible at all and is exactly what is under test here.
	if _, err := s.GetDB().Exec(`DELETE FROM team_members WHERE team_id = $1`, teamID); err != nil {
		t.Fatalf("delete team_members: %v", err)
	}
	if _, err := s.GetDB().Exec(`DELETE FROM teams WHERE id = $1`, teamID); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	// 3. Simulate re-migration: drop NOT NULL first, then clear snapshot
	_, err := s.GetDB().Exec(`ALTER TABLE alert_groups ALTER COLUMN team_name_snapshot DROP NOT NULL`)
	if err != nil {
		t.Fatalf("Failed to drop NOT NULL: %v", err)
	}
	_, err = s.GetDB().Exec(`UPDATE alert_groups SET team_name_snapshot = NULL WHERE id = $1`, agID)
	if err != nil {
		t.Fatalf("Failed to clear snapshot: %v", err)
	}

	// 4. Re-run InitDB — should NOT crash despite orphaned team_id
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB crashed on orphaned team_id: %v", err)
	}

	// 5. Verify snapshot was backfilled with team_id as fallback
	fetched, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if fetched.TeamNameSnapshot != teamID {
		t.Errorf("Expected TeamNameSnapshot = %q (fallback to team_id), got %q", teamID, fetched.TeamNameSnapshot)
	}
}

// TestSeed_AlertGroupHasTeamNameSnapshot verifies that seeded alert groups
// have a non-empty TeamNameSnapshot resolved from the team name.
func TestSeed_AlertGroupHasTeamNameSnapshot(t *testing.T) {
	s := setupTestDB(t)

	if err := s.Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	// Seed creates AGs for teams "devops" and "platform" — check a few
	ags, _, err := s.GetAllAlertGroups(nil, 100, 0)
	if err != nil {
		t.Fatalf("GetAllAlertGroups: %v", err)
	}
	if len(ags) == 0 {
		t.Fatal("Expected seeded alert groups")
	}

	for _, ag := range ags {
		if ag.TeamNameSnapshot == "" {
			t.Errorf("AG %s (%s) has empty TeamNameSnapshot", ag.ID, ag.Title)
		}
		// Snapshot should NOT equal teamID when team exists — it should be the team name
		if ag.TeamNameSnapshot == ag.TeamID {
			t.Errorf("AG %s: TeamNameSnapshot = TeamID %q, expected resolved team name", ag.ID, ag.TeamID)
		}
	}
}

// ===================================================================================
// Escalation cancellation by alert group (Epic 11 Sprint 1)
//
// Cancellation used to address the job by its raw dedup key, which made its
// correctness depend on a uniqueness index rather than on the query. These tests
// pin what it addresses now.
//
// They lean on createTestTeamAndAG setting DedupKey = "key-" + agID: the two are
// deliberately different, so a query that went back to matching on dedup_key
// while being handed an alert group id would find nothing and redden every
// positive case below.
// ===================================================================================

// jobStatuses reads back a job with its stages and steps.
func jobStatuses(t *testing.T, s *Store, jobID string) (string, []string, []string) {
	t.Helper()

	var jobStatus string
	if err := s.GetDB().QueryRow(`SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&jobStatus); err != nil {
		t.Fatalf("read job %s: %v", jobID, err)
	}

	read := func(query string) []string {
		rows, err := s.GetDB().Query(query, jobID)
		if err != nil {
			t.Fatalf("read children of %s: %v", jobID, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var st string
			if err := rows.Scan(&st); err != nil {
				t.Fatalf("scan status: %v", err)
			}
			out = append(out, st)
		}
		return out
	}
	return jobStatus,
		read(`SELECT status FROM job_stages WHERE job_id = $1`),
		read(`SELECT status FROM job_steps WHERE job_id = $1`)
}

// seedEscalation puts an alert group into processing with a live escalation job,
// the way the engine does.
func seedEscalation(t *testing.T, s *Store, teamID string) (agID, jobID string) {
	t.Helper()
	agID = createTestTeamAndAG(t, s, teamID, model.AlertGroupStatusProcessing)
	job, stages, steps, snapshot := makeTestJob(agID)
	created, err := s.EnsureEscalationJob(agID, job, stages, steps, snapshot)
	if err != nil || !created {
		t.Fatalf("EnsureEscalationJob(%s) = %v, %v", agID, created, err)
	}
	return agID, job.ID
}

func requireCanceledThrough(t *testing.T, s *Store, jobID string) {
	t.Helper()
	jobStatus, stages, steps := jobStatuses(t, s, jobID)
	if jobStatus != string(model.JobStatusCanceled) {
		t.Errorf("job %s = %s, want canceled", jobID, jobStatus)
	}
	for _, st := range stages {
		if st != string(model.JobStageStatusCanceled) {
			t.Errorf("stage of %s = %s, want canceled", jobID, st)
		}
	}
	for _, st := range steps {
		if st != string(model.JobStepStatusCanceled) {
			t.Errorf("step of %s = %s, want canceled", jobID, st)
		}
	}
}

func TestAckAlertGroupAtomic_CancelsEscalationOfItsGroup(t *testing.T) {
	s := setupTestDB(t)
	agID, jobID := seedEscalation(t, s, "team-cancel-ack")

	changed, err := s.AckAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil || !changed {
		t.Fatalf("AckAlertGroupAtomic = %v, %v", changed, err)
	}
	requireCanceledThrough(t, s, jobID)
}

func TestResolveAlertGroupAtomic_CancelsEscalationOfItsGroup(t *testing.T) {
	s := setupTestDB(t)
	agID, jobID := seedEscalation(t, s, "team-cancel-resolve")

	changed, err := s.ResolveAlertGroupAtomic(agID, "TestUser", nil, nil)
	if err != nil || !changed {
		t.Fatalf("ResolveAlertGroupAtomic = %v, %v", changed, err)
	}
	requireCanceledThrough(t, s, jobID)
}

func TestResolveAlertGroupWithAlertsAtomic_CancelsEscalationOfItsGroup(t *testing.T) {
	s := setupTestDB(t)
	agID, jobID := seedEscalation(t, s, "team-cancel-resolve-alerts")

	changed, err := s.ResolveAlertGroupWithAlertsAtomic(agID, nil, nil, nil)
	if err != nil || !changed {
		t.Fatalf("ResolveAlertGroupWithAlertsAtomic = %v, %v", changed, err)
	}
	requireCanceledThrough(t, s, jobID)
}

// TestCancelEscalationJob_LeavesAnotherGroupAlone: cancellation names one alert
// group, and a query that stopped naming it would take the whole table with it.
func TestCancelEscalationJob_LeavesAnotherGroupAlone(t *testing.T) {
	s := setupTestDB(t)
	acked, ackedJob := seedEscalation(t, s, "team-cancel-mine")
	_, otherJob := seedEscalation(t, s, "team-cancel-theirs")

	if err := s.CancelEscalationJobByAlertGroupID(acked); err != nil {
		t.Fatalf("CancelEscalationJobByAlertGroupID: %v", err)
	}

	requireCanceledThrough(t, s, ackedJob)
	if status, _, _ := jobStatuses(t, s, otherJob); status != string(model.JobStatusPending) {
		t.Errorf("another group's escalation = %s, want pending", status)
	}
}

// TestCancelEscalationJob_LeavesOtherJobTypesAlone: the type predicate.
//
// Production does not put alert_group_id on resolution jobs today, so the row is
// inserted directly - the point is the predicate, not the scenario. It stops
// being hypothetical the moment another family gains that column.
func TestCancelEscalationJob_LeavesOtherJobTypesAlone(t *testing.T) {
	s := setupTestDB(t)
	agID, escalationJob := seedEscalation(t, s, "team-cancel-types")

	resolutionJob := uuid.New().String()
	if _, err := s.GetDB().Exec(`
		INSERT INTO jobs (id, type, status, payload, dedup_namespace, dedup_key, dedup_scope,
			alert_group_id, current_stage, created_at, updated_at)
		VALUES ($1, 'resolution', 'pending', '{}', 'resolution', $2, 'while_active', $3, 0, NOW(), NOW())`,
		resolutionJob, "resolve_"+agID, agID); err != nil {
		t.Fatalf("seed resolution job: %v", err)
	}

	if err := s.CancelEscalationJobByAlertGroupID(agID); err != nil {
		t.Fatalf("CancelEscalationJobByAlertGroupID: %v", err)
	}

	requireCanceledThrough(t, s, escalationJob)
	if status, _, _ := jobStatuses(t, s, resolutionJob); status != string(model.JobStatusPending) {
		t.Errorf("resolution job of the same group = %s, want pending", status)
	}
}

func TestCancelEscalationJobByAlertGroupID_NoActiveJob(t *testing.T) {
	s := setupTestDB(t)
	agID := createTestTeamAndAG(t, s, "team-cancel-none", model.AlertGroupStatusTriggered)

	if err := s.CancelEscalationJobByAlertGroupID(agID); err != nil {
		t.Errorf("cancelling with no active escalation = %v, want nil", err)
	}
}

// TestCancelEscalationJob_ReachesChildrenOfEveryMatch: cancellation must not
// stop at the first job it cancelled.
//
// The UPDATE cancels every matching row, so reading one id back and cancelling
// only that job's stages and steps leaves the rest half-cancelled - jobs marked
// canceled with children still pending, which the worker would happily claim.
//
// The dedup model admits one escalation per alert group, so a second row of the
// same identity cannot exist. A row with no identity at all can: that is the
// shape a historical job takes when the upgrade could not classify it, and
// nothing stops it from being active and pointing at this group. It is exactly
// the second match the predicate has to reach.
func TestCancelEscalationJob_ReachesChildrenOfEveryMatch(t *testing.T) {
	s := setupTestDB(t)
	agID, firstJob := seedEscalation(t, s, "team-cancel-many")

	secondJob, secondStage, secondStep := uuid.New().String(), uuid.New().String(), uuid.New().String()
	if _, err := s.GetDB().Exec(`
		INSERT INTO jobs (id, type, status, payload, alert_group_id, current_stage, created_at, updated_at)
		VALUES ($1, 'escalation', 'pending', '{}', $2, 0, NOW(), NOW())`,
		secondJob, agID); err != nil {
		t.Fatalf("seed the second escalation: %v", err)
	}
	if _, err := s.GetDB().Exec(`
		INSERT INTO job_stages (id, job_id, stage_index, status, created_at, updated_at)
		VALUES ($1, $2, 0, 'active', NOW(), NOW())`, secondStage, secondJob); err != nil {
		t.Fatalf("seed its stage: %v", err)
	}
	if _, err := s.GetDB().Exec(`
		INSERT INTO job_steps (id, job_id, stage_id, step_index, step_type, status, data, max_attempts, created_at, updated_at)
		VALUES ($1, $2, $3, 0, 'dm', 'pending', '{}', 3, NOW(), NOW())`,
		secondStep, secondJob, secondStage); err != nil {
		t.Fatalf("seed its step: %v", err)
	}

	if err := s.CancelEscalationJobByAlertGroupID(agID); err != nil {
		t.Fatalf("CancelEscalationJobByAlertGroupID: %v", err)
	}

	requireCanceledThrough(t, s, firstJob)
	requireCanceledThrough(t, s, secondJob)
}
