package store

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

func TestMockStore_ListDeliveries(t *testing.T) {
	s := NewMockStore()

	now := time.Now()
	agID := "ag-list"

	d1 := &model.NotificationDelivery{
		AlertGroupID:   agID,
		Provider:       "slack",
		Kind:           "slack_channel",
		SupportsUpdate: true,
		CreatedAt:      now.Add(-2 * time.Minute),
	}
	d2 := &model.NotificationDelivery{
		AlertGroupID:   agID,
		Provider:       "slack",
		Kind:           "slack_dm",
		SupportsUpdate: false,
		CreatedAt:      now.Add(-1 * time.Minute),
	}
	if err := s.UpsertNotificationDelivery(d1); err != nil {
		t.Fatalf("upsert d1 failed: %v", err)
	}
	if err := s.UpsertNotificationDelivery(d2); err != nil {
		t.Fatalf("upsert d2 failed: %v", err)
	}

	deliveries, err := s.ListDeliveries(agID)
	if err != nil {
		t.Fatalf("ListDeliveries failed: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(deliveries))
	}
	if deliveries[0].Kind != "slack_channel" || deliveries[1].Kind != "slack_dm" {
		t.Fatalf("expected deliveries ordered by CreatedAt asc, got %s then %s", deliveries[0].Kind, deliveries[1].Kind)
	}
}

func TestMockStore_HasPrimaryDelivery(t *testing.T) {
	s := NewMockStore()

	agID := "ag-primary"
	d := &model.NotificationDelivery{
		AlertGroupID:   agID,
		Provider:       "slack",
		Kind:           "slack_channel",
		SupportsUpdate: true,
		IsPrimary:      true,
		CreatedAt:      time.Now(),
	}
	if err := s.UpsertNotificationDelivery(d); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	hasPrimary, err := s.HasPrimaryDelivery(agID, "slack")
	if err != nil {
		t.Fatalf("HasPrimaryDelivery failed: %v", err)
	}
	if !hasPrimary {
		t.Fatalf("expected primary delivery to be present")
	}
}
