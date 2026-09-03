package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
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
	alertKey := "key-cpu-high-1"
	ag := &model.AlertGroup{
		ID:               agID,
		AlertKey:         alertKey,
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
	forceAlertGroupStatus(t, s, agID, model.AlertGroupStatusAcknowledged)

	// 5. Verify Update
	fetchedAfterUpdate, err := s.GetActiveAlertGroupByAlertKey(alertKey)
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
			AlertKey:         "key-" + id,
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
// AckAlertGroupAtomic Integration Tests
// ===================================================================================

func createTestTeamAndAG(t *testing.T, s *Store, teamID string, agStatus model.AlertGroupStatus) string {
	t.Helper()
	// Create team if not exists (ignore error if duplicate)
	s.CreateTeam(&model.Team{ID: teamID, Name: teamID, CreatedAt: time.Now()})

	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID:               agID,
		AlertKey:         "key-" + agID,
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
		AlertKey:         "key-" + agID,
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

	// Verify timeline events - ordering via µs offsets
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
		ID: agID, AlertKey: "key-" + agID, Status: model.AlertGroupStatusNew,
		Title: "First", TeamID: teamID, TeamNameSnapshot: teamID, Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateAlertGroup(ag); err != nil {
		t.Fatalf("setup CreateAlertGroup failed: %v", err)
	}

	// Try to create duplicate AG atomically - should fail on duplicate key
	dupAG := &model.AlertGroup{
		ID: agID, AlertKey: "key-dup", Status: model.AlertGroupStatusNew,
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
	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("TestUser"), meta, nil)
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

	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("TestUser"), nil, nil)
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

	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("TestUser"), nil, nil)
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
	changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("TestUser"), meta, nil)
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

	changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("TestUser"), nil, nil)
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

	changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("TestUser"), nil, nil)
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
			changed, err := s.AckAlertGroupAtomic(agID, actorNamed("User"+uuid.New().String()[:4]), nil, nil)
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

	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("TestUser"), nil, nil)
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

	changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("TestUser"), nil, nil)
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

// TestGetEscalationSources_SkipsAGAlreadyAdmitted. A group whose escalation was
// admitted is out of the loop for good, whatever became of the deliveries under
// it: the claim is held forever, and picking the group up again would promise
// the same page twice.
//
// It asks the admissions rather than the jobs. Asked about jobs, the answer
// becomes "no escalation job" for every group in the system the moment the job
// path is gone - and every processing group would come back round every thirty
// seconds to be escalated again.
func TestGetEscalationSources_SkipsAGAlreadyAdmitted(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-skip-escalation"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Skip Escalation", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Create AG in stale processing with succeeded escalation job (via alert_group_id)
	agID := uuid.New().String()
	ag := &model.AlertGroup{
		ID: agID, AlertKey: "dk-skip-" + agID, Status: model.AlertGroupStatusProcessing,
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

	// An admission for this group, with nothing left of its deliveries: the
	// claim is what says the group was escalated.
	_, err = s.GetDB().Exec(`
		INSERT INTO outbound_batches
			(id, batch_key, key_kind, delivery_family, grammar_version, alert_group_id,
			 fingerprint, fingerprint_version, admission_outcome, intent_count,
			 admission_snapshot, admission_digest, admission_schema_version,
			 admission_revision)
		VALUES ($1, $2, 'escalation', 'notification', 1, $3, $4, 1, 'admitted', 1,
			$5, $6, 1, 0)`,
		uuid.New().String(), "batch-"+agID, agID, digest32(0x20),
		`{"frozen":true}`, digest32(0x21))
	if err != nil {
		t.Fatalf("Failed to record the admission: %v", err)
	}

	results, err := s.GetEscalationSources(context.Background())
	if err != nil {
		t.Fatalf("GetEscalationSources failed: %v", err)
	}

	for _, r := range results {
		if r.ID == agID {
			t.Error("a group whose escalation was already admitted was picked up again")
		}
	}
}

func TestGetEscalationSources_IncludesStaleProcessing(t *testing.T) {
	s := setupTestDB(t)

	teamID := "team-reconcile"
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: "Reconcile", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// AG1: "new" - always returned
	ag1 := &model.AlertGroup{
		ID: "ag-new-1", AlertKey: "dk-new-1", Status: model.AlertGroupStatusNew,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	// AG2: "processing" with fresh updated_at - should NOT be returned
	ag2 := &model.AlertGroup{
		ID: "ag-proc-fresh", AlertKey: "dk-proc-fresh", Status: model.AlertGroupStatusProcessing,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	// AG3: "processing" with stale updated_at (> 30s ago) - should be returned
	ag3 := &model.AlertGroup{
		ID: "ag-proc-stale", AlertKey: "dk-proc-stale", Status: model.AlertGroupStatusProcessing,
		TeamID: teamID, TeamNameSnapshot: "Reconcile", CreatedAt: time.Now(), UpdatedAt: time.Now().Add(-60 * time.Second),
	}
	// AG4: "triggered" - should NOT be returned
	ag4 := &model.AlertGroup{
		ID: "ag-triggered", AlertKey: "dk-triggered", Status: model.AlertGroupStatusTriggered,
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

	results, err := s.GetEscalationSources(context.Background())
	if err != nil {
		t.Fatalf("GetEscalationSources failed: %v", err)
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

	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("TestUser"), nil, outboxEvent)
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

	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("TestUser"), nil, outboxEvent)
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

	changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("TestUser"), nil, outboxEvent)
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

	changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("TestUser"), nil, outboxEvent)
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
			changed, err := s.AckAlertGroupAtomic(agID, actorNamed(actor), nil, oe)
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
// A resolving payload, end to end
// ===================================================================================

// TestAPayloadThatClearsEverythingEndsTheIncident: alerts_data, timeline events,
// the outbox event and the job cancellation, in one transaction.
func TestAPayloadThatClearsEverythingEndsTheIncident(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	teamID := "team-resolve-alerts"
	s.CreateTeam(&model.Team{ID: teamID, Name: "Resolve Team", CreatedAt: time.Now()})

	agID := uuid.New().String()
	alertKey := "dk-resolve-" + agID
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: alertKey, Status: model.AlertGroupStatusNew,
		TeamID: teamID, TeamNameSnapshot: "Resolve Team", Severity: "warning",
		Alerts: []model.Alert{{
			Fingerprint: "fp1", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000000, 0), Labels: map[string]string{"alertname": "CPU"},
		}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAlertGroup: %v", err)
	}

	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), alertKey,
		[]model.Alert{{
			Fingerprint: "fp1", Status: model.AlertStatusResolved,
			StartsAt: time.Unix(1700000000, 0), Labels: map[string]string{"alertname": "CPU"},
		}}, "system")
	if err != nil {
		t.Fatalf("apply the payload: %v", err)
	}
	if result.Outcome != alertgroup.MergeResolved || result.AlertGroupID != agID {
		t.Fatalf("the payload came back %s for %s", result.Outcome, result.AlertGroupID)
	}

	var status, resolvedBy string
	var resolvedAt sql.NullTime
	if err := s.db.QueryRow(
		`SELECT status, resolved_by, resolved_at FROM alert_groups WHERE id = $1`, agID).
		Scan(&status, &resolvedBy, &resolvedAt); err != nil {
		t.Fatalf("read the incident: %v", err)
	}
	if status != string(model.AlertGroupStatusResolved) || resolvedBy != "system" || !resolvedAt.Valid {
		t.Fatalf("the incident is %s, resolved by %q at %v", status, resolvedBy, resolvedAt)
	}

	// The history says the alert cleared and that the incident ended with it.
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	var cleared, ended bool
	for _, e := range events {
		switch e.Type {
		case model.TimelineEventAlertResolved:
			cleared = true
		case model.TimelineEventResolved:
			ended = true
		}
	}
	if !cleared || !ended {
		t.Errorf("the history says cleared=%v ended=%v", cleared, ended)
	}

	// And the subscribers are told, in the same commit.
	var events2 int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM event_outbox WHERE alert_group_id = $1 AND event_type = $2`,
		agID, model.OutboxEventResolved).Scan(&events2); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	if events2 != 1 {
		t.Errorf("the outbox holds %d resolutions", events2)
	}
}

// TestAPayloadForAnIncidentThatIsOverBelongsToTheNextOne. A resolved incident
// is finished: the alert firing again is the next one, and the payload finds
// nothing open to apply itself to.
func TestAPayloadForAnIncidentThatIsOverBelongsToTheNextOne(t *testing.T) {
	s := setupTestDB(t)

	agID := createTestTeamAndAG(t, s, "team-resolve-idem", model.AlertGroupStatusResolved)
	var alertKey string
	if err := s.db.QueryRow(`SELECT alert_key FROM alert_groups WHERE id = $1`, agID).
		Scan(&alertKey); err != nil {
		t.Fatalf("read the alert key: %v", err)
	}

	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), alertKey,
		[]model.Alert{{Fingerprint: "fp1", Status: model.AlertStatusFiring}}, "system")
	if err != nil {
		t.Fatalf("apply the payload: %v", err)
	}
	if result.Outcome != alertgroup.MergeNoActive {
		t.Fatalf("a payload for a finished incident came back %s", result.Outcome)
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
		ID: agID, AlertKey: "dk-orphan-" + agID, Status: model.AlertGroupStatusTriggered,
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

	// 4. Re-run InitDB - should NOT crash despite orphaned team_id
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

	// Seed creates AGs for teams "devops" and "platform" - check a few
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
		// Snapshot should NOT equal teamID when team exists - it should be the team name
		if ag.TeamNameSnapshot == ag.TeamID {
			t.Errorf("AG %s: TeamNameSnapshot = TeamID %q, expected resolved team name", ag.ID, ag.TeamID)
		}
	}
}

// ===================================================================================
// What acknowledging and resolving stop
//
// A user who acknowledges is saying "I have this", and a user who resolves is
// saying "it is over". Either way nobody may be paged about it afterwards - and
// that has to hold in the SAME commit as the status change, because a crash
// between the two would page somebody for an alert that is already handled.
//
// All three doors are here. Only one of them used to be exercised against what
// it actually withdraws, and the other two were covered by a job that no longer
// exists.
// ===================================================================================

func TestTheDoorsThatStopADeliveryStopAllOfIt(t *testing.T) {
	s := setupTestDB(t)

	doors := map[string]func(t *testing.T, agID string){
		"a user acknowledged": func(t *testing.T, agID string) {
			changed, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
			if err != nil || !changed {
				t.Fatalf("AckAlertGroupAtomic = %v, %v", changed, err)
			}
		},
		"a user resolved": func(t *testing.T, agID string) {
			changed, err := s.ResolveAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
			if err != nil || !changed {
				t.Fatalf("ResolveAlertGroupAtomic = %v, %v", changed, err)
			}
		},
		"every alert resolved itself": func(t *testing.T, agID string) {
			var alertKey string
			if err := s.db.QueryRow(`SELECT alert_key FROM alert_groups WHERE id = $1`, agID).
				Scan(&alertKey); err != nil {
				t.Fatalf("read the alert key: %v", err)
			}
			result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), alertKey,
				[]model.Alert{{
					Fingerprint: "fp-1", Status: model.AlertStatusResolved,
					StartsAt: time.Unix(1700000000, 0),
					Labels:   map[string]string{"alertname": "DiskWillFill"},
				}}, "system")
			if err != nil || result.Outcome != alertgroup.MergeResolved {
				t.Fatalf("the payload came back %s (%v)", result.Outcome, err)
			}
		},
	}

	for name, close := range doors {
		t.Run(name, func(t *testing.T) {
			agID := outboundGroup(t, s)
			owed := admitOne(t, s, agID,
				channelCommitment("C0001", 0), channelCommitment("C0002", 5*time.Minute))

			close(t, agID)

			for _, intentID := range owed {
				if got := statusOf(t, s, intentID); got != outbound.StatusCanceled {
					t.Errorf("a notification survived as %s", got)
				}
			}

			// And the history says why, once per withdrawn commitment: an
			// operator reading the alert has to be able to tell "nobody was
			// told" from "everybody was told".
			var explained int
			if err := s.db.QueryRow(`
				SELECT count(*) FROM outbound_intent_events e
				JOIN outbound_intents i ON i.id = e.intent_id
				WHERE i.alert_group_id = $1 AND e.kind = 'canceled'`,
				agID).Scan(&explained); err != nil {
				t.Fatalf("count the explanations: %v", err)
			}
			if explained != len(owed) {
				t.Errorf("%d of %d withdrawals were explained", explained, len(owed))
			}
		})
	}
}

// TestADoorLeavesAnotherAlertAlone. The withdrawal names one alert group, and a
// query that stopped naming it would take every alert in the system with it.
func TestADoorLeavesAnotherAlertAlone(t *testing.T) {
	s := setupTestDB(t)

	acknowledged := outboundGroup(t, s)
	owed := admitOne(t, s, acknowledged)[0]

	bystander := outboundGroup(t, s)
	stillOwed := admitOne(t, s, bystander)[0]

	if _, err := s.AckAlertGroupAtomic(acknowledged, actorNamed("nina"), nil, nil); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}

	if got := statusOf(t, s, owed); got != outbound.StatusCanceled {
		t.Errorf("the acknowledged alert still owes a notification: %s", got)
	}
	if got := statusOf(t, s, stillOwed); got != outbound.StatusPending {
		t.Fatalf("another alert's notification was withdrawn too: %s", got)
	}
}

// TestAlertKeyRenameFromAnOlderSchema: a database written before the alert's
// key had a name of its own is brought forward on the next start.
//
// A fresh database never exercises this - it is created with the column already
// named - so the only way to test the migration is to put the old name back and
// start again. What has to survive it is not the column alone: the rule that
// there is one live incident per alert lives in a partial unique index over that
// column, and an index that quietly stopped applying would let one alert open
// two incidents.
func TestAlertKeyRenameFromAnOlderSchema(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	if _, err := s.db.Exec(`ALTER TABLE alert_groups RENAME COLUMN alert_key TO dedup_key`); err != nil {
		t.Fatalf("put the old schema back: %v", err)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB against the older schema: %v", err)
	}

	var renamed bool
	if err := s.db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'alert_groups' AND column_name = 'alert_key')`).Scan(&renamed); err != nil {
		t.Fatalf("ask the catalog: %v", err)
	}
	if !renamed {
		t.Fatal("the column was not brought forward")
	}

	// The rule the index is: one live group per alert key, and the key is free
	// again once the group is resolved.
	key := "alert-" + uuid.New().String()
	first := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: first, AlertKey: key, Status: model.AlertGroupStatusProcessing,
		Title: "first", Severity: "info",
	}); err != nil {
		t.Fatalf("create the first group: %v", err)
	}
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: uuid.New().String(), AlertKey: key, Status: model.AlertGroupStatusProcessing,
		Title: "second", Severity: "info",
	}); err == nil {
		t.Fatal("a second live group was created for one alert; the index no longer applies")
	}

	forceAlertGroupStatus(t, s, first, model.AlertGroupStatusResolved)
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: uuid.New().String(), AlertKey: key, Status: model.AlertGroupStatusProcessing,
		Title: "next incident", Severity: "info",
	}); err != nil {
		t.Fatalf("the same alert firing again could not open a new incident: %v", err)
	}

	active, err := s.GetActiveAlertGroupByAlertKey(key)
	if err != nil {
		t.Fatalf("GetActiveAlertGroupByAlertKey: %v", err)
	}
	if active == nil || active.Title != "next incident" {
		t.Fatalf("active group = %+v, want the new incident", active)
	}
}
