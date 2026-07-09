package scheduler

import (
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// FetchCurrentOnCall retrieves necessary data from store and calculates the current on-call status.
func FetchCurrentOnCall(s store.StoreInterface, scheduleID string) (*model.OnCallResult, error) {
	sched, err := s.GetScheduleByID(scheduleID)
	if err != nil {
		return nil, fmt.Errorf("failed to load schedule %s: %w", scheduleID, err)
	}

	now := time.Now()
	// Fetch context data (window broad enough to catch current epoch)
	// Using +/- 60 days to be safe for long rotations
	windowStart := now.Add(-60 * 24 * time.Hour)
	windowEnd := now.Add(60 * 24 * time.Hour)

	l1Epochs, err := s.GetRotationEpochs(scheduleID, "l1", windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to load L1 epochs: %w", err)
	}

	var l2Epochs []*model.RotationEpoch
	if sched.L2Enabled {
		l2Epochs, err = s.GetRotationEpochs(scheduleID, "l2", windowStart, windowEnd)
		if err != nil {
			return nil, fmt.Errorf("failed to load L2 epochs: %w", err)
		}
	}

	overrides, err := s.GetScheduleOverrides(scheduleID, windowStart, windowEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to load overrides: %w", err)
	}

	// Resolve Users
	// Collect all unique user IDs from epochs and overrides
	userIDs := make(map[string]struct{})
	for _, ep := range l1Epochs {
		for _, group := range ep.Groups {
			for _, u := range group {
				userIDs[u] = struct{}{}
			}
		}
	}
	for _, ep := range l2Epochs {
		for _, group := range ep.Groups {
			for _, u := range group {
				userIDs[u] = struct{}{}
			}
		}
	}
	for _, o := range overrides {
		userIDs[o.UserID] = struct{}{}
	}

	uniqueIDs := make([]string, 0, len(userIDs))
	for id := range userIDs {
		uniqueIDs = append(uniqueIDs, id)
	}

	usersList, err := s.GetUsersByIDs(uniqueIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to load users: %w", err)
	}
	usersMap := make(map[string]*model.User)
	for _, u := range usersList {
		usersMap[u.ID] = u
	}

	// delegate to pure logic
	return GetCurrentOnCall(sched, l1Epochs, l2Epochs, overrides, usersMap, now), nil
}
