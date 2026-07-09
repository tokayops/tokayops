package scheduler

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

func TestRenderCalendarSchedule_MergeOverrides(t *testing.T) {
	gen := NewSegmentGenerator()

	// Scenario: Two adjacent overrides for the SAME user, but different IDs.
	// This happens if I create an override 12:00-14:00, and another 14:00-16:00.
	// They should NOT be merged in the backend logic strictly speaking if we want to preserve distinct override objects.

	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	epochs := []*model.RotationEpoch{
		{
			ID: "e1", Layer: "l1", Groups: [][]string{{"bob"}},
			StartTime: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			EndTime:   nil,
		},
	}

	users := map[string]*model.User{
		"alice": {ID: "alice", Name: "Alice"},
		"bob":   {ID: "bob", Name: "Bob"},
	}

	overrides := []*model.ScheduleOverride{
		{
			ID: "ov1", UserID: "alice",
			StartTime: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC),
		},
		{
			ID: "ov2", UserID: "alice",
			StartTime: time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 1, 16, 0, 0, 0, time.UTC),
		},
	}

	from := time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 1, 18, 0, 0, 0, time.UTC)

	results := gen.GenerateSegments(schedule, epochs, overrides, users, from, until)

	// Expected:
	// 1. Bob (11:00-12:00)
	// 2. Alice (12:00-14:00) - Ov1
	// 3. Alice (14:00-16:00) - Ov2
	// 4. Bob (16:00-...)

	// Find Alice segments
	var aliceSegments []model.OnCallSegment
	for _, seg := range results {
		if seg.UserIDs[0] == "alice" {
			aliceSegments = append(aliceSegments, seg)
		}
	}

	if len(aliceSegments) != 2 {
		t.Fatalf("Expected 2 separate Alice segments, got %d. Dump: %+v", len(aliceSegments), aliceSegments)
	}

	if aliceSegments[0].Override == nil || aliceSegments[0].Override.ID != "ov1" {
		t.Errorf("First segment should be ov1, got %+v", aliceSegments[0].Override)
	}
	if aliceSegments[1].Override == nil || aliceSegments[1].Override.ID != "ov2" {
		t.Errorf("Second segment should be ov2, got %+v", aliceSegments[1].Override)
	}
}
