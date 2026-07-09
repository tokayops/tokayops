package scheduler

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// Helper to create a user map from users slice
func makeUserMap(users []*model.User) map[string]*model.User {
	m := make(map[string]*model.User)
	for _, u := range users {
		m[u.ID] = u
	}
	return m
}

func TestGetCurrentOnCall_DailyRotation(t *testing.T) {
	// Start rotation on Jan 1, 2025 at 11:00 UTC
	rotationStart := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)

	users := []*model.User{
		{ID: "user1", Name: "Denis"},
		{ID: "user2", Name: "John"},
		{ID: "user3", Name: "Anna"},
	}

	l1Epochs := []*model.RotationEpoch{{
		ID:        "epoch1",
		Layer:     "l1",
		Groups:    [][]string{{"user1"}, {"user2"}, {"user3"}},
		StartTime: rotationStart,
	}}

	schedule := &model.Schedule{
		ID:             "sched1",
		TeamID:         "devops",
		Timezone:       "UTC",
		L1RotationType: model.RotationDaily,
		L1HandoffTime:  "11:00",
	}

	tests := []struct {
		name       string
		at         time.Time
		wantUserID string
	}{
		{"Day 0 - first user", time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC), "user1"},
		{"Day 1 - second user", time.Date(2025, 1, 2, 12, 0, 0, 0, time.UTC), "user2"},
		{"Day 2 - third user", time.Date(2025, 1, 3, 12, 0, 0, 0, time.UTC), "user3"},
		{"Day 3 - wraps to first", time.Date(2025, 1, 4, 12, 0, 0, 0, time.UTC), "user1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetCurrentOnCall(schedule, l1Epochs, nil, nil, makeUserMap(users), tt.at)
			if len(result.L1Users) == 0 {
				t.Fatal("expected L1Users, got empty")
			}
			if result.L1Users[0].ID != tt.wantUserID {
				t.Errorf("got user %s, want %s", result.L1Users[0].ID, tt.wantUserID)
			}
		})
	}
}

func TestGetCurrentOnCall_WeeklyRotation(t *testing.T) {
	// Start rotation on Monday Jan 6, 2025 at 11:00 UTC
	rotationStart := time.Date(2025, 1, 6, 11, 0, 0, 0, time.UTC)
	monday := 1

	users := []*model.User{
		{ID: "user1", Name: "Denis"},
		{ID: "user2", Name: "John"},
	}

	l1Epochs := []*model.RotationEpoch{{
		ID:        "epoch1",
		Layer:     "l1",
		Groups:    [][]string{{"user1"}, {"user2"}},
		StartTime: rotationStart,
	}}

	schedule := &model.Schedule{
		ID:             "sched1",
		TeamID:         "devops",
		Timezone:       "UTC",
		L1RotationType: model.RotationWeekly,
		L1HandoffDay:   &monday,
		L1HandoffTime:  "11:00",
	}

	tests := []struct {
		name       string
		at         time.Time
		wantUserID string
	}{
		{"Week 0 - first user", time.Date(2025, 1, 7, 12, 0, 0, 0, time.UTC), "user1"},
		{"Week 1 - second user", time.Date(2025, 1, 14, 12, 0, 0, 0, time.UTC), "user2"},
		{"Week 2 - wraps to first", time.Date(2025, 1, 21, 12, 0, 0, 0, time.UTC), "user1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetCurrentOnCall(schedule, l1Epochs, nil, nil, makeUserMap(users), tt.at)
			if len(result.L1Users) == 0 {
				t.Fatal("expected L1Users, got empty")
			}
			if result.L1Users[0].ID != tt.wantUserID {
				t.Errorf("got user %s, want %s", result.L1Users[0].ID, tt.wantUserID)
			}
		})
	}
}

func TestGetCurrentOnCall_OverrideTakesPrecedence(t *testing.T) {
	rotationStart := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)

	users := []*model.User{
		{ID: "user1", Name: "Denis"},
		{ID: "user2", Name: "John"},
		{ID: "user3", Name: "Anna"},
	}

	l1Epochs := []*model.RotationEpoch{{
		ID:        "epoch1",
		Layer:     "l1",
		Groups:    [][]string{{"user1"}, {"user2"}},
		StartTime: rotationStart,
	}}

	schedule := &model.Schedule{
		ID:             "sched1",
		TeamID:         "devops",
		Timezone:       "UTC",
		L1RotationType: model.RotationDaily,
		L1HandoffTime:  "11:00",
	}

	overrides := []*model.ScheduleOverride{{
		ID:        "override1",
		UserID:    "user3",
		StartTime: time.Date(2025, 1, 1, 14, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2025, 1, 1, 18, 0, 0, 0, time.UTC),
	}}

	userMap := makeUserMap(users)

	// Before override - should be rotation user with segment times (not override times)
	result := GetCurrentOnCall(schedule, l1Epochs, nil, overrides, userMap, time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	if result.L1Users[0].ID != "user1" {
		t.Errorf("before override: got %s, want user1", result.L1Users[0].ID)
	}
	if result.Override != nil {
		t.Error("before override: expected Override to be nil")
	}
	if result.L1Since == nil {
		t.Fatal("before override: expected L1Since to be set")
	}

	// During override - should be override user with override's actual start/end
	result = GetCurrentOnCall(schedule, l1Epochs, nil, overrides, userMap, time.Date(2025, 1, 1, 15, 0, 0, 0, time.UTC))
	if result.L1Users[0].ID != "user3" {
		t.Errorf("during override: got %s, want user3", result.L1Users[0].ID)
	}
	if result.Override == nil {
		t.Fatal("during override: expected Override to be set")
	}
	if result.L1Since == nil || !result.L1Since.Equal(overrides[0].StartTime) {
		t.Errorf("during override: L1Since = %v, want %v", result.L1Since, overrides[0].StartTime)
	}
	if result.L1Until == nil || !result.L1Until.Equal(overrides[0].EndTime) {
		t.Errorf("during override: L1Until = %v, want %v", result.L1Until, overrides[0].EndTime)
	}

	// After override - should be rotation user again
	result = GetCurrentOnCall(schedule, l1Epochs, nil, overrides, userMap, time.Date(2025, 1, 1, 19, 0, 0, 0, time.UTC))
	if result.L1Users[0].ID != "user1" {
		t.Errorf("after override: got %s, want user1", result.L1Users[0].ID)
	}
}

func TestGetCurrentOnCall_L2Layer(t *testing.T) {
	rotationStart := time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC)

	l1Users := []*model.User{{ID: "user1", Name: "Denis"}}
	l2Users := []*model.User{{ID: "manager", Name: "Manager"}}

	l1Epochs := []*model.RotationEpoch{{
		ID:        "epoch1",
		Layer:     "l1",
		Groups:    [][]string{{"user1"}},
		StartTime: rotationStart,
	}}

	l2Epochs := []*model.RotationEpoch{{
		ID:        "epoch2",
		Layer:     "l2",
		Groups:    [][]string{{"manager"}},
		StartTime: rotationStart,
	}}

	schedule := &model.Schedule{
		ID:                  "sched1",
		TeamID:              "devops",
		Timezone:            "UTC",
		L1RotationType:      model.RotationDaily,
		L1HandoffTime:       "11:00",
		L2Enabled:           true,
		L2RotationType:      model.RotationWeekly,
		L2HandoffTime:       "11:00",
		L2EscalationTimeout: 5,
	}

	allUsers := append(l1Users, l2Users...)
	result := GetCurrentOnCall(schedule, l1Epochs, l2Epochs, nil, makeUserMap(allUsers), time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))

	if len(result.L1Users) == 0 || result.L1Users[0].ID != "user1" {
		t.Errorf("L1Users: got %v, want user1", result.L1Users)
	}
	if result.L2User == nil || result.L2User.ID != "manager" {
		t.Errorf("L2User: got %v, want manager", result.L2User)
	}
}

func TestGetCurrentOnCall_NilEpochs(t *testing.T) {
	schedule := &model.Schedule{
		ID:             "sched1",
		TeamID:         "empty",
		Timezone:       "UTC",
		L1RotationType: model.RotationDaily,
		L1HandoffTime:  "11:00",
	}

	result := GetCurrentOnCall(schedule, nil, nil, nil, nil, time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	if len(result.L1Users) > 0 {
		t.Error("expected empty L1Users for nil epochs")
	}
}

func TestGetCurrentOnCall_EmptyEpochs(t *testing.T) {
	schedule := &model.Schedule{
		ID:             "sched1",
		TeamID:         "empty",
		Timezone:       "UTC",
		L1RotationType: model.RotationDaily,
		L1HandoffTime:  "11:00",
	}

	result := GetCurrentOnCall(schedule, []*model.RotationEpoch{}, nil, nil, nil, time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	if len(result.L1Users) > 0 {
		t.Error("expected empty L1Users for empty epochs")
	}
}

func TestGetCurrentOnCall_MultiUserGroup(t *testing.T) {
	schedule := &model.Schedule{
		Timezone:       "UTC",
		L1RotationType: model.RotationDaily,
		L1HandoffTime:  "11:00",
	}

	epochs := []*model.RotationEpoch{
		{
			ID:        "ep1",
			Groups:    [][]string{{"user1", "user2"}, {"user3"}},
			StartTime: time.Date(2025, 1, 1, 11, 0, 0, 0, time.UTC),
		},
	}

	users := map[string]*model.User{
		"user1": {ID: "user1", Name: "User1"},
		"user2": {ID: "user2", Name: "User2"},
		"user3": {ID: "user3", Name: "User3"},
	}

	// Day 1: group [user1, user2]
	at := time.Date(2025, 1, 1, 15, 0, 0, 0, time.UTC)
	result := GetCurrentOnCall(schedule, epochs, nil, nil, users, at)

	if len(result.L1Users) != 2 {
		t.Fatalf("expected 2 L1Users, got %d", len(result.L1Users))
	}
	if result.L1Users[0].ID != "user1" || result.L1Users[1].ID != "user2" {
		t.Errorf("expected [user1, user2], got [%s, %s]", result.L1Users[0].ID, result.L1Users[1].ID)
	}

	// Day 2: group [user3]
	at2 := time.Date(2025, 1, 2, 15, 0, 0, 0, time.UTC)
	result2 := GetCurrentOnCall(schedule, epochs, nil, nil, users, at2)

	if len(result2.L1Users) != 1 || result2.L1Users[0].ID != "user3" {
		t.Errorf("day 2: expected [user3], got %v", result2.L1Users)
	}

	// Day 3: back to [user1, user2]
	at3 := time.Date(2025, 1, 3, 15, 0, 0, 0, time.UTC)
	result3 := GetCurrentOnCall(schedule, epochs, nil, nil, users, at3)

	if len(result3.L1Users) != 2 {
		t.Fatalf("day 3: expected 2 L1Users, got %d", len(result3.L1Users))
	}
	if result3.L1Users[0].ID != "user1" {
		t.Errorf("day 3: expected user1 first, got %s", result3.L1Users[0].ID)
	}
}
