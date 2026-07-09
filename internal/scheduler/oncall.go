package scheduler

import (
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// GetCurrentOnCall returns who is on-call at a specific time.
// Uses SegmentGenerator as the SINGLE SOURCE OF TRUTH — no duplicate logic.
func GetCurrentOnCall(
	schedule *model.Schedule,
	l1Epochs []*model.RotationEpoch,
	l2Epochs []*model.RotationEpoch,
	overrides []*model.ScheduleOverride,
	users map[string]*model.User,
	at time.Time,
) *model.OnCallResult {
	result := &model.OnCallResult{}
	gen := NewSegmentGenerator()

	// L1: generate segments and find who's on-call now (with lookahead)
	if len(l1Epochs) > 0 {
		seg := gen.GenerateCurrentSegment(schedule, l1Epochs, overrides, users, at)
		if seg != nil {
			result.L1Users = seg.Users

			// For overrides, use the override's actual start/end times
			// (segment times may be clipped by epoch boundaries).
			if seg.Layer == "override" && seg.Override != nil {
				result.Override = seg.Override
				result.L1Since = &seg.Override.StartTime
				result.L1Until = &seg.Override.EndTime
			} else {
				result.L1Since = &seg.StartTime
				if seg.IsForever {
					result.L1Until = nil
				} else {
					result.L1Until = &seg.EndTime
				}
			}
		}
	}

	// L2: same approach with L2 config
	if schedule.L2Enabled && len(l2Epochs) > 0 {
		l2Schedule := &model.Schedule{
			Timezone:       schedule.Timezone,
			L1RotationType: schedule.L2RotationType,
			L1HandoffTime:  schedule.L2HandoffTime,
			L1HandoffDay:   schedule.L2HandoffDay,
		}
		// Use same logic for L2
		seg := gen.GenerateCurrentSegment(l2Schedule, l2Epochs, nil, users, at)
		if seg != nil && len(seg.Users) > 0 {
			result.L2User = seg.Users[0]
		}
	}

	return result
}
