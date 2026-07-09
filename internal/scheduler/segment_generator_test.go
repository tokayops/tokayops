package scheduler

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

func TestGenerateSegments_DailyRotation(t *testing.T) {
	gen := NewSegmentGenerator()

	// Schedule with daily rotation, handoff at 11:00
	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	// Epoch starting Jan 1 with 3 users
	epochs := []*model.RotationEpoch{
		{
			ID:        "epoch1",
			Layer:     "l1",
			Groups:    [][]string{{"user1"}, {"user2"}, {"user3"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
			EndTime:   nil, // Current
		},
	}

	users := map[string]*model.User{
		"user1": {ID: "user1", Name: "Alice"},
		"user2": {ID: "user2", Name: "Bob"},
		"user3": {ID: "user3", Name: "Charlie"},
	}

	// Query 3 days: Jan 1-4
	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)

	segments := gen.GenerateSegments(schedule, epochs, nil, users, from, until)

	// Expected: 3 segments (one per day)
	// Day 1 (Jan 1 11:00 -> Jan 2 11:00): user1
	// Day 2 (Jan 2 11:00 -> Jan 3 11:00): user2
	// Day 3 (Jan 3 11:00 -> Jan 4 11:00): user3
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}

	testCases := []struct {
		idx      int
		userID   string
		userName string
	}{
		{0, "user1", "Alice"},
		{1, "user2", "Bob"},
		{2, "user3", "Charlie"},
	}

	for _, tc := range testCases {
		seg := segments[tc.idx]
		if seg.UserIDs[0] != tc.userID {
			t.Errorf("segment %d: expected user %s, got %s", tc.idx, tc.userID, seg.UserIDs[0])
		}
		if len(seg.Users) > 0 && seg.Users[0].Name != tc.userName {
			t.Errorf("segment %d: expected name %s, got %s", tc.idx, tc.userName, seg.Users[0].Name)
		}
		if seg.Layer != "l1" {
			t.Errorf("segment %d: expected layer l1, got %s", tc.idx, seg.Layer)
		}
	}
}

func TestGenerateSegments_WeeklyRotation(t *testing.T) {
	gen := NewSegmentGenerator()

	monday := 1
	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationWeekly,
		L1HandoffTime:   "11:00",
		L1HandoffDay:    &monday,
		L1RotationStart: time.Date(2025, 1, 6, 11, 0, 0, 0, time.UTC), // Monday
	}

	epochs := []*model.RotationEpoch{
		{
			ID:        "epoch1",
			Layer:     "l1",
			Groups:    [][]string{{"user1"}, {"user2"}},
			StartTime: time.Date(2025, 1, 6, 11, 0, 0, 0, time.UTC),
			EndTime:   nil,
		},
	}

	users := map[string]*model.User{
		"user1": {ID: "user1", Name: "Alice"},
		"user2": {ID: "user2", Name: "Bob"},
	}

	// Query 2 weeks
	from := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 20, 11, 0, 0, 0, time.UTC)

	segments := gen.GenerateSegments(schedule, epochs, nil, users, from, until)

	// Week 1 (Jan 6-12) -> User1 (Merged)
	// Week 2 (Jan 13-19) -> User2 (Merged)
	// Should have 2 natural segments
	if len(segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segments))
	}

	// Week 1: User1
	if segments[0].UserIDs[0] != "user1" {
		t.Errorf("segment 0: expected user1, got %s", segments[0].UserIDs[0])
	}
	expectedEnd1 := time.Date(2025, 1, 13, 11, 0, 0, 0, time.UTC)
	if !segments[0].EndTime.Equal(expectedEnd1) {
		t.Errorf("segment 0: expected end %v, got %v", expectedEnd1, segments[0].EndTime)
	}

	// Week 2: User2
	if segments[1].UserIDs[0] != "user2" {
		t.Errorf("segment 1: expected user2, got %s", segments[1].UserIDs[0])
	}
	expectedEnd2 := time.Date(2025, 1, 20, 11, 0, 0, 0, time.UTC)
	if !segments[1].EndTime.Equal(expectedEnd2) {
		t.Errorf("segment 1: expected end %v, got %v", expectedEnd2, segments[1].EndTime)
	}
}

func TestGenerateSegments_WithOverride(t *testing.T) {
	gen := NewSegmentGenerator()

	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	epochs := []*model.RotationEpoch{
		{
			ID:        "epoch1",
			Layer:     "l1",
			Groups:    [][]string{{"user1"}, {"user2"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
			EndTime:   nil,
		},
	}

	// Override on Jan 1: 14:00-18:00 user3 takes over
	overrides := []*model.ScheduleOverride{
		{
			ID:        "override1",
			UserID:    "user3",
			StartTime: time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 1, 18, 0, 0, 0, time.UTC),
		},
	}

	users := map[string]*model.User{
		"user1": {ID: "user1", Name: "Alice"},
		"user2": {ID: "user2", Name: "Bob"},
		"user3": {ID: "user3", Name: "Charlie"},
	}

	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)

	segments := gen.GenerateSegments(schedule, epochs, overrides, users, from, until)

	// Expected: 3 segments
	// 1. user1: 11:00-14:00
	// 2. user3: 14:00-18:00 (override)
	// 3. user1: 18:00-next day 11:00
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}

	// First segment: rotation user before override
	if segments[0].UserIDs[0] != "user1" || segments[0].Layer != "l1" {
		t.Errorf("segment 0: expected user1/l1, got %s/%s", segments[0].UserIDs[0], segments[0].Layer)
	}

	// Second segment: override
	if segments[1].UserIDs[0] != "user3" || segments[1].Layer != "override" {
		t.Errorf("segment 1: expected user3/override, got %s/%s", segments[1].UserIDs[0], segments[1].Layer)
	}

	// Third segment: rotation user after override
	if segments[2].UserIDs[0] != "user1" || segments[2].Layer != "l1" {
		t.Errorf("segment 2: expected user1/l1, got %s/%s", segments[2].UserIDs[0], segments[2].Layer)
	}
}

func TestGenerateSegments_EpochTransition(t *testing.T) {
	gen := NewSegmentGenerator()

	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	// Two epochs: first ends Jan 2 11:00, second starts then
	epoch1End := time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC)
	epochs := []*model.RotationEpoch{
		{
			ID:        "epoch1",
			Layer:     "l1",
			Groups:    [][]string{{"user1"}, {"user2"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
			EndTime:   &epoch1End,
		},
		{
			ID:        "epoch2",
			Layer:     "l1",
			Groups:    [][]string{{"user3"}, {"user4"}}, // Different users!
			StartTime: time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC),
			EndTime:   nil,
		},
	}

	users := map[string]*model.User{
		"user1": {ID: "user1", Name: "Alice"},
		"user2": {ID: "user2", Name: "Bob"},
		"user3": {ID: "user3", Name: "Charlie"},
		"user4": {ID: "user4", Name: "Diana"},
	}

	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)

	segments := gen.GenerateSegments(schedule, epochs, nil, users, from, until)

	// Day 1: epoch1, user1
	// Day 2: epoch2, user3 (first user of new epoch)
	// Day 3: epoch2, user4
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}

	if segments[0].UserIDs[0] != "user1" {
		t.Errorf("day 1: expected user1 (epoch1), got %s", segments[0].UserIDs[0])
	}
	if segments[1].UserIDs[0] != "user3" {
		t.Errorf("day 2: expected user3 (epoch2 day 0), got %s", segments[1].UserIDs[0])
	}
	if segments[2].UserIDs[0] != "user4" {
		t.Errorf("day 3: expected user4 (epoch2 day 1), got %s", segments[2].UserIDs[0])
	}
}

func TestGenerateSegments_Timezone(t *testing.T) {
	gen := NewSegmentGenerator()

	// Bangkok timezone (UTC+7)
	// Handoff at 11:00 Bangkok = 04:00 UTC
	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "Asia/Bangkok",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 4, 0, 0, 0, time.UTC), // 11:00 Bangkok
	}

	epochs := []*model.RotationEpoch{
		{
			ID:        "epoch1",
			Layer:     "l1",
			Groups:    [][]string{{"user1"}, {"user2"}},
			StartTime: time.Date(2025, 1, 1, 4, 0, 0, 0, time.UTC),
			EndTime:   nil,
		},
	}

	users := map[string]*model.User{
		"user1": {ID: "user1", Name: "Thai User 1"},
		"user2": {ID: "user2", Name: "Thai User 2"},
	}

	// Query starting from Jan 1 11:00 Bangkok (04:00 UTC) to Jan 3 11:00 Bangkok (04:00 UTC)
	// This should give us exactly 2 segments
	from := time.Date(2025, 1, 1, 4, 0, 0, 0, time.UTC)  // 11:00 Bangkok Jan 1
	until := time.Date(2025, 1, 3, 4, 0, 0, 0, time.UTC) // 11:00 Bangkok Jan 3

	segments := gen.GenerateSegments(schedule, epochs, nil, users, from, until)

	if len(segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(segments))
	}

	// Verify handoff times are in UTC but represent 11:00 Bangkok
	expectedStart := time.Date(2025, 1, 1, 4, 0, 0, 0, time.UTC) // 11:00 Bangkok
	if !segments[0].StartTime.Equal(expectedStart) {
		t.Errorf("segment 0: expected start %v, got %v", expectedStart, segments[0].StartTime)
	}

	// First day: user1, second day: user2
	if segments[0].UserIDs[0] != "user1" {
		t.Errorf("segment 0: expected user1, got %s", segments[0].UserIDs[0])
	}
	if segments[1].UserIDs[0] != "user2" {
		t.Errorf("segment 1: expected user2, got %s", segments[1].UserIDs[0])
	}
}

func TestGenerateSegments_EmptyEpochs(t *testing.T) {
	gen := NewSegmentGenerator()

	schedule := &model.Schedule{
		ID:             "sched1",
		Timezone:       "UTC",
		L1RotationType: model.RotationDaily,
		L1HandoffTime:  "11:00",
	}

	from := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)

	segments := gen.GenerateSegments(schedule, nil, nil, nil, from, until)

	if len(segments) != 0 {
		t.Errorf("expected 0 segments for empty epochs, got %d", len(segments))
	}
}

func TestParseHandoffTime(t *testing.T) {
	tests := []struct {
		input      string
		wantHour   int
		wantMinute int
	}{
		{"11:00", 11, 0},
		{"09:30", 9, 30},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
		{"", 11, 0},        // default
		{"invalid", 11, 0}, // default on parse error
	}

	for _, tt := range tests {
		h, m := parseHandoffTime(tt.input)
		if h != tt.wantHour || m != tt.wantMinute {
			t.Errorf("parseHandoffTime(%q) = %d:%d, want %d:%d", tt.input, h, m, tt.wantHour, tt.wantMinute)
		}
	}
}

func TestRenderCalendarSchedule_SplitAndMerge(t *testing.T) {
	gen := NewSegmentGenerator()
	loc := time.UTC

	// Simulate: Alice on-call Jan 1 11:00 -> Jan 3 11:00 (continuous 2 days)
	// After split by day:
	//   Jan 1: 11:00-24:00 Alice
	//   Jan 2: 00:00-11:00 Alice, 11:00-24:00 Alice -> should merge to 00:00-24:00 Alice
	//   Jan 3: 00:00-11:00 Alice
	segments := []model.OnCallSegment{
		{
			UserIDs:   []string{"alice"},
			Users:     []*model.User{{ID: "alice", Name: "Alice"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC),
			Layer:     "l1",
		},
		{
			UserIDs:   []string{"alice"},
			Users:     []*model.User{{ID: "alice", Name: "Alice"}},
			StartTime: time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 3, 11, 0, 0, 0, time.UTC),
			Layer:     "l1",
		},
	}

	result := gen.RenderCalendarSchedule(segments, loc)

	// Expected: 3 segments after split and merge
	// Day 1: 11:00-24:00 Alice
	// Day 2: 00:00-24:00 Alice (merged)
	// Day 3: 00:00-11:00 Alice
	if len(result) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(result))
	}

	// Check Day 1: 11:00-24:00
	if result[0].StartTime.Hour() != 11 || result[0].EndTime.Hour() != 0 {
		t.Errorf("day 1: expected 11:00-00:00, got %v-%v",
			result[0].StartTime.Format("15:04"), result[0].EndTime.Format("15:04"))
	}

	// Check Day 2: 00:00-24:00 (merged)
	if result[1].StartTime.Hour() != 0 || result[1].EndTime.Hour() != 0 || result[1].StartTime.Day() != 2 {
		t.Errorf("day 2: expected 00:00-24:00 on Jan 2, got %v-%v day %d",
			result[1].StartTime.Format("15:04"), result[1].EndTime.Format("15:04"), result[1].StartTime.Day())
	}
	// Verify it covers full day (24 hours)
	duration := result[1].EndTime.Sub(result[1].StartTime)
	if duration != 24*time.Hour {
		t.Errorf("day 2: expected 24h duration, got %v", duration)
	}

	// Check Day 3: 00:00-11:00
	if result[2].StartTime.Hour() != 0 || result[2].EndTime.Hour() != 11 || result[2].StartTime.Day() != 3 {
		t.Errorf("day 3: expected 00:00-11:00 on Jan 3, got %v-%v day %d",
			result[2].StartTime.Format("15:04"), result[2].EndTime.Format("15:04"), result[2].StartTime.Day())
	}
}

func TestRenderCalendarSchedule_DifferentUsers(t *testing.T) {
	gen := NewSegmentGenerator()
	loc := time.UTC

	// Alice Jan 1 11:00 -> Jan 2 11:00
	// Bob   Jan 2 11:00 -> Jan 3 11:00
	// After split: Day 2 has Alice 00:00-11:00 + Bob 11:00-24:00 (NO merge)
	segments := []model.OnCallSegment{
		{
			UserIDs:   []string{"alice"},
			Users:     []*model.User{{ID: "alice", Name: "Alice"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC),
			Layer:     "l1",
		},
		{
			UserIDs:   []string{"bob"},
			Users:     []*model.User{{ID: "bob", Name: "Bob"}},
			StartTime: time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 3, 11, 0, 0, 0, time.UTC),
			Layer:     "l1",
		},
	}

	result := gen.RenderCalendarSchedule(segments, loc)

	// Expected: 4 segments (no merging because different users)
	// Day 1: 11:00-24:00 Alice
	// Day 2: 00:00-11:00 Alice, 11:00-24:00 Bob
	// Day 3: 00:00-11:00 Bob
	if len(result) != 4 {
		t.Fatalf("expected 4 segments, got %d", len(result))
	}

	// Day 2 first segment: Alice 00:00-11:00
	if result[1].UserIDs[0] != "alice" {
		t.Errorf("day 2 first segment: expected alice, got %s", result[1].UserIDs[0])
	}

	// Day 2 second segment: Bob 11:00-24:00
	if result[2].UserIDs[0] != "bob" {
		t.Errorf("day 2 second segment: expected bob, got %s", result[2].UserIDs[0])
	}
}

func TestGenerateSegments_MultiUserGroup(t *testing.T) {
	gen := NewSegmentGenerator()

	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	// Groups: [["alice","bob"], ["carol"]] — group of 2 alternates with single user
	epochs := []*model.RotationEpoch{
		{
			ID:        "ep1",
			Groups:    [][]string{{"alice", "bob"}, {"carol"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
		},
	}

	users := map[string]*model.User{
		"alice": {ID: "alice", Name: "Alice"},
		"bob":   {ID: "bob", Name: "Bob"},
		"carol": {ID: "carol", Name: "Carol"},
	}

	from := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 4, 11, 0, 0, 0, time.UTC) // 3 days

	segments := gen.GenerateSegments(schedule, epochs, nil, users, from, until)

	// Day 1: [alice, bob], Day 2: [carol], Day 3: [alice, bob]
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}

	// Day 1: group [alice, bob]
	if len(segments[0].UserIDs) != 2 || segments[0].UserIDs[0] != "alice" || segments[0].UserIDs[1] != "bob" {
		t.Errorf("day 1: expected [alice, bob], got %v", segments[0].UserIDs)
	}
	if len(segments[0].Users) != 2 || segments[0].Users[0].Name != "Alice" || segments[0].Users[1].Name != "Bob" {
		t.Errorf("day 1: expected users [Alice, Bob], got %v", segments[0].Users)
	}

	// Day 2: group [carol]
	if len(segments[1].UserIDs) != 1 || segments[1].UserIDs[0] != "carol" {
		t.Errorf("day 2: expected [carol], got %v", segments[1].UserIDs)
	}

	// Day 3: group [alice, bob] again
	if len(segments[2].UserIDs) != 2 || segments[2].UserIDs[0] != "alice" || segments[2].UserIDs[1] != "bob" {
		t.Errorf("day 3: expected [alice, bob], got %v", segments[2].UserIDs)
	}
}

func TestGenerateSegments_MultiUserGroupWithOverride(t *testing.T) {
	gen := NewSegmentGenerator()

	schedule := &model.Schedule{
		ID:              "sched1",
		Timezone:        "UTC",
		L1RotationType:  model.RotationDaily,
		L1HandoffTime:   "11:00",
		L1RotationStart: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
	}

	epochs := []*model.RotationEpoch{
		{
			ID:        "ep1",
			Groups:    [][]string{{"alice", "bob"}, {"carol"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
		},
	}

	// Override replaces entire group [alice, bob] on day 1 with dave
	overrides := []*model.ScheduleOverride{
		{
			ID:        "ovr1",
			UserID:    "dave",
			StartTime: time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC),
			EndTime:   time.Date(2025, 1, 1, 20, 0, 0, 0, time.UTC),
		},
	}

	users := map[string]*model.User{
		"alice": {ID: "alice", Name: "Alice"},
		"bob":   {ID: "bob", Name: "Bob"},
		"carol": {ID: "carol", Name: "Carol"},
		"dave":  {ID: "dave", Name: "Dave"},
	}

	from := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)
	until := time.Date(2025, 1, 2, 11, 0, 0, 0, time.UTC) // 1 day

	segments := gen.GenerateSegments(schedule, epochs, overrides, users, from, until)

	// Expected: [alice,bob] 11:00-14:00, [dave] 14:00-20:00, [alice,bob] 20:00-11:00
	if len(segments) != 3 {
		t.Fatalf("expected 3 segments, got %d", len(segments))
	}

	// Before override: group [alice, bob]
	if len(segments[0].UserIDs) != 2 {
		t.Errorf("before override: expected 2 users, got %v", segments[0].UserIDs)
	}

	// Override: single user [dave]
	if len(segments[1].UserIDs) != 1 || segments[1].UserIDs[0] != "dave" {
		t.Errorf("override: expected [dave], got %v", segments[1].UserIDs)
	}
	if segments[1].Layer != "override" {
		t.Errorf("override segment: expected layer override, got %s", segments[1].Layer)
	}

	// After override: group [alice, bob]
	if len(segments[2].UserIDs) != 2 {
		t.Errorf("after override: expected 2 users, got %v", segments[2].UserIDs)
	}
}
