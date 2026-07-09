package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBuildWebhookEventPayload_Firing(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	ag := &AlertGroup{
		ID:        "ag-123",
		Title:     "High CPU",
		Status:    AlertGroupStatusNew,
		Severity:  "critical",
		TeamID:    "team-devops",
		Alerts:    []Alert{{Fingerprint: "fp1"}, {Fingerprint: "fp2"}, {Fingerprint: "fp3"}},
		CreatedAt: now.Add(-5 * time.Minute),
	}

	raw, err := BuildWebhookEventPayload(OutboxEventFiring, ag, "DevOps", "system", "", now)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload failed: %v", err)
	}

	var p WebhookEventPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("Failed to unmarshal payload: %v", err)
	}

	if p.Event != "alert_group.firing" {
		t.Errorf("Expected event 'alert_group.firing', got %q", p.Event)
	}
	if p.AlertGroup.Status != "firing" {
		t.Errorf("Expected status 'firing', got %q", p.AlertGroup.Status)
	}
	if p.AlertGroup.TeamName != "DevOps" {
		t.Errorf("Expected team_name 'DevOps', got %q", p.AlertGroup.TeamName)
	}
	if p.AlertGroup.AlertCount != 3 {
		t.Errorf("Expected alert_count 3, got %d", p.AlertGroup.AlertCount)
	}
	if p.Actor.Name != "system" {
		t.Errorf("Expected actor.name 'system', got %q", p.Actor.Name)
	}

	// Verify email key is absent in raw JSON for system actor (omitempty)
	var actorMap map[string]json.RawMessage
	var topLevel map[string]json.RawMessage
	json.Unmarshal(raw, &topLevel)
	json.Unmarshal(topLevel["actor"], &actorMap)
	if _, exists := actorMap["email"]; exists {
		t.Error("Expected no 'email' key in actor for system events (omitempty), but it was present")
	}

	if p.AlertGroup.ID != "ag-123" {
		t.Errorf("Expected id 'ag-123', got %q", p.AlertGroup.ID)
	}
	if p.AlertGroup.Title != "High CPU" {
		t.Errorf("Expected title 'High CPU', got %q", p.AlertGroup.Title)
	}
	if p.AlertGroup.Severity != "critical" {
		t.Errorf("Expected severity 'critical', got %q", p.AlertGroup.Severity)
	}
	if p.AlertGroup.TeamID != "team-devops" {
		t.Errorf("Expected team_id 'team-devops', got %q", p.AlertGroup.TeamID)
	}

	// Verify no url field (omitempty)
	var rawMap map[string]json.RawMessage
	json.Unmarshal(raw, &rawMap)
	var agMap map[string]json.RawMessage
	json.Unmarshal(rawMap["alert_group"], &agMap)
	if _, exists := agMap["url"]; exists {
		t.Error("Expected no 'url' field in alert_group (omitempty), but it was present")
	}

	// Verify no external_url field
	if _, exists := agMap["external_url"]; exists {
		t.Error("Expected no 'external_url' field in alert_group")
	}
}

func TestBuildWebhookEventPayload_Acknowledged(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	ag := &AlertGroup{
		ID:        "ag-456",
		Title:     "Disk Full",
		Status:    AlertGroupStatusAcknowledged,
		Severity:  "warning",
		TeamID:    "team-sre",
		Alerts:    []Alert{{Fingerprint: "fp1"}},
		CreatedAt: now.Add(-10 * time.Minute),
	}

	raw, err := BuildWebhookEventPayload(OutboxEventAcknowledged, ag, "SRE", "Alice", "alice@example.com", now)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload failed: %v", err)
	}

	var p WebhookEventPayload
	json.Unmarshal(raw, &p)

	if p.Event != "alert_group.acknowledged" {
		t.Errorf("Expected event 'alert_group.acknowledged', got %q", p.Event)
	}
	if p.AlertGroup.Status != "acknowledged" {
		t.Errorf("Expected status 'acknowledged', got %q", p.AlertGroup.Status)
	}
	if p.Actor.Name != "Alice" {
		t.Errorf("Expected actor.name 'Alice', got %q", p.Actor.Name)
	}
	if p.Actor.Email != "alice@example.com" {
		t.Errorf("Expected actor.email 'alice@example.com', got %q", p.Actor.Email)
	}

	// Verify email key IS present in raw JSON for user actors
	var topLevel map[string]json.RawMessage
	json.Unmarshal(raw, &topLevel)
	var actorMap map[string]json.RawMessage
	json.Unmarshal(topLevel["actor"], &actorMap)
	if _, exists := actorMap["email"]; !exists {
		t.Error("Expected 'email' key in actor for user-initiated events")
	}
}

func TestBuildWebhookEventPayload_Resolved(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	ag := &AlertGroup{
		ID:        "ag-789",
		Title:     "Memory Leak",
		Status:    AlertGroupStatusResolved,
		Severity:  "info",
		TeamID:    "team-platform",
		Alerts:    []Alert{},
		CreatedAt: now.Add(-30 * time.Minute),
	}

	raw, err := BuildWebhookEventPayload(OutboxEventResolved, ag, "Platform", "system", "", now)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload failed: %v", err)
	}

	var p WebhookEventPayload
	json.Unmarshal(raw, &p)

	if p.Event != "alert_group.resolved" {
		t.Errorf("Expected event 'alert_group.resolved', got %q", p.Event)
	}
	if p.AlertGroup.Status != "resolved" {
		t.Errorf("Expected status 'resolved', got %q", p.AlertGroup.Status)
	}
}

func TestBuildWebhookEventPayload_UnknownType(t *testing.T) {
	ag := &AlertGroup{ID: "ag-x", CreatedAt: time.Now()}
	_, err := BuildWebhookEventPayload(OutboxEventType("alert_group.invalid"), ag, "Team", "system", "", time.Now())
	if err == nil {
		t.Fatal("Expected error for unknown event type")
	}
}
