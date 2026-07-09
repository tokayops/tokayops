package scheduler

import (
	"strconv"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// SegmentGenerator creates on-call segments for a time range.
// This is a pure function with no side effects.
type SegmentGenerator struct{}

// NewSegmentGenerator creates a new segment generator
func NewSegmentGenerator() *SegmentGenerator {
	return &SegmentGenerator{}
}

// GenerateSegments computes on-call segments for a time range.
// This is a PURE FUNCTION: no DB calls, no side effects.
//
// GenerateSegments computes on-call segments for a time range.
// This is a PURE FUNCTION: no DB calls, no side effects.
//
// Architecture "Ideal Handoff Grid":
// The algorithm treats the schedule as a continuous grid of "Ideal Segments" defined by the Handoff Time
// (e.g., Daily 11:00 -> 11:00). It then intersects this grid with the actual history of configuration
// changes (Epochs).
//
// Algorithm:
//  1. Iterate through "Ideal Segments" (handoff-to-handoff intervals).
//  2. Find all RotationEpochs that overlap with each Ideal Segment.
//  3. Intersect the Ideal Segment with the Epoch's valid time range [Start, End).
//  4. Generate sub-segments for these intersections.
//  5. Calculate rotation user based on the Ideal Grid (Virtual Start), ensuring consistent rotation
//     regardless of when the Epoch actually started.
//  6. Apply overrides to split segments further.
func (g *SegmentGenerator) GenerateSegments(
	schedule *model.Schedule,
	epochs []*model.RotationEpoch,
	overrides []*model.ScheduleOverride,
	users map[string]*model.User,
	from, until time.Time,
) []model.OnCallSegment {
	if len(epochs) == 0 {
		return nil
	}

	// Load timezone
	loc, err := time.LoadLocation(schedule.Timezone)
	if err != nil {
		loc = time.UTC
	}

	// Parse handoff time
	handoffHour, handoffMin := parseHandoffTime(schedule.L1HandoffTime)

	// Convert to local timezone
	localFrom := from.In(loc)
	localUntil := until.In(loc)

	// Align to the start of the rotation period covering localFrom
	// For Daily: The most recent Day at HandoffTime <= localFrom
	// For Weekly: The most recent HandoffDay at HandoffTime <= localFrom
	currentPeriodStart := alignToRotationPeriod(localFrom, schedule, handoffHour, handoffMin, loc)

	var segments []model.OnCallSegment

	// Iterate by rotation period (Day or Week)
	for currentPeriodStart.Before(localUntil) {
		// Calculate Period End
		var nextPeriodStart time.Time
		if schedule.L1RotationType == model.RotationWeekly {
			nextPeriodStart = currentPeriodStart.AddDate(0, 0, 7)
		} else {
			nextPeriodStart = currentPeriodStart.AddDate(0, 0, 1)
		}

		segStart := currentPeriodStart
		segEnd := nextPeriodStart

		// Optimization: Check if this period overlaps with requested range
		if !segEnd.After(from) || !until.After(segStart) {
			currentPeriodStart = nextPeriodStart
			continue
		}

		// Find epochs that overlap with this Ideal Period
		var relevantEpochs []*model.RotationEpoch
		for _, e := range epochs {
			if e.StartTime.Before(segEnd) && (e.EndTime == nil || e.EndTime.After(segStart)) {
				relevantEpochs = append(relevantEpochs, e)
			}
		}

		if len(relevantEpochs) == 0 {
			currentPeriodStart = nextPeriodStart
			continue
		}

		// Generate sub-segments for each relevant epoch
		for _, epoch := range relevantEpochs {
			subStart := segStart
			if epoch.StartTime.After(subStart) {
				subStart = epoch.StartTime
			}

			subEnd := segEnd
			if epoch.EndTime != nil && epoch.EndTime.Before(subEnd) {
				subEnd = *epoch.EndTime
			}

			if !subEnd.After(subStart) {
				continue
			}

			subMidpoint := subStart.Add(subEnd.Sub(subStart) / 2)
			groupIDs := calculateRotationGroup(epoch, schedule.L1RotationType, schedule.L1HandoffDay, handoffHour, handoffMin, loc, subMidpoint)

			if len(groupIDs) == 0 {
				continue
			}

			subOverrides := g.getOverlappingOverrides(overrides, subStart.UTC(), subEnd.UTC())

			if len(subOverrides) == 0 {
				seg := model.OnCallSegment{
					UserIDs:   groupIDs,
					StartTime: subStart.UTC(),
					EndTime:   subEnd.UTC(),
					Layer:     "l1",
				}
				for _, id := range groupIDs {
					if u, ok := users[id]; ok {
						seg.Users = append(seg.Users, u)
					}
				}
				segments = append(segments, seg)
			} else {
				splitSegs := g.splitByOverrides(groupIDs, subStart.UTC(), subEnd.UTC(), subOverrides, users)
				segments = append(segments, splitSegs...)
			}
		}

		currentPeriodStart = nextPeriodStart
	}

	// Step 2: Merge adjacent segments for the same user
	// This ensures we return "Natural Shifts" (e.g. 2 weeks continuous)
	// rather than fragmented day/week blocks.
	var merged []model.OnCallSegment
	for _, seg := range segments {
		if len(merged) == 0 {
			merged = append(merged, seg)
			continue
		}

		last := &merged[len(merged)-1]

		// Check if we can merge
		// 1. Same User
		// 2. Same Layer
		// 3. Contiguous (Last.End == Seg.Start)
		// 4. Checking equality of pointers handles overrides/users correctly if they are the exact same object
		//    BUT, we also need to check IDs if they are different objects but represent the same override/user
		canMerge := sliceEqual(last.UserIDs, seg.UserIDs) &&
			last.Layer == seg.Layer &&
			!last.EndTime.Before(seg.StartTime) && !last.EndTime.After(seg.StartTime) // Contiguous

		if canMerge {
			// Special check for overrides: Don't merge if they are different overrides
			if last.Layer == "override" {
				if last.Override == nil || seg.Override == nil {
					// Should ideally not happen if layer is override, but safe fallback
					canMerge = last.Override == seg.Override
				} else if last.Override != seg.Override && last.Override.ID != seg.Override.ID {
					canMerge = false
				}
			}
		}

		if canMerge {
			// Merge
			last.EndTime = seg.EndTime
		} else {
			merged = append(merged, seg)
		}
	}

	// Step 3: Clamp segments to requested range [from, until]
	// This ensures we don't return segments that are partially outside the requested view (e.g. from lookahead)
	var clamped []model.OnCallSegment
	for _, seg := range merged {
		if seg.EndTime.Before(from) || seg.StartTime.After(until) {
			continue
		}
		// Clamp Start
		if seg.StartTime.Before(from) {
			seg.StartTime = from
		}
		// Clamp End
		if seg.EndTime.After(until) {
			seg.EndTime = until
		}
		// Only add if duration > 0
		if seg.EndTime.After(seg.StartTime) {
			clamped = append(clamped, seg)
		}
	}

	return clamped
}

// GenerateCurrentSegment calculates the single active segment for a specific time 'at'.
// It looks ahead (up to 31 days) to determine the segment's end time.
// If the segment extends to the horizon limit, IsForever is set to true.
func (g *SegmentGenerator) GenerateCurrentSegment(
	schedule *model.Schedule,
	epochs []*model.RotationEpoch,
	overrides []*model.ScheduleOverride,
	users map[string]*model.User,
	at time.Time,
) *model.OnCallSegment {
	from, horizon := CurrentOnCallWindow(at)

	segments := g.GenerateSegments(schedule, epochs, overrides, users, from, horizon)

	for i := range segments {
		seg := &segments[i]
		if !seg.StartTime.After(at) && seg.EndTime.After(at) {
			// Check if this segment extends to our horizon (minus safety buffer)
			if seg.EndTime.After(horizon.Add(-time.Hour)) {
				seg.IsForever = true
				seg.EndTime = horizon // Or keep merged time? Keep merged time, UI handles it. IsForever is the signal.
			}
			return seg
		}
	}

	return nil
}

// alignToRotationPeriod finds the start of the rotation period covering time t
func alignToRotationPeriod(t time.Time, schedule *model.Schedule, hHour, hMin int, loc *time.Location) time.Time {
	// Candidate: t's day at HandoffTime
	candidate := time.Date(t.Year(), t.Month(), t.Day(), hHour, hMin, 0, 0, loc)

	if schedule.L1RotationType == model.RotationWeekly {
		// Adjust to the specific weekday
		targetDay := 1 // Monday
		if schedule.L1HandoffDay != nil {
			targetDay = *schedule.L1HandoffDay
		}

		currentWD := int(candidate.Weekday())
		// Days to subtract to get to targetDay
		diff := (currentWD - targetDay + 7) % 7
		candidate = candidate.AddDate(0, 0, -diff)
	}

	// If t is before the candidate (e.g. valid day but before 11:00),
	// then the active period started in the PREVIOUS cycle.
	if t.Before(candidate) {
		if schedule.L1RotationType == model.RotationWeekly {
			candidate = candidate.AddDate(0, 0, -7)
		} else {
			candidate = candidate.AddDate(0, 0, -1)
		}
	}

	return candidate
}

// sliceEqual returns true if two string slices have the same elements in the same order.
func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// calculateRotationGroup determines which group is on-call based on rotation type and elapsed time.
//
// Architecture "Virtual Start":
// To ensure rotation always switches at the configured Handoff Time (e.g., Monday 11:00)
// regardless of when the schedule was actually created (Epoch Start), we calculate a
// "Virtual Start" time.
//
// Virtual Start is the theoretical start time of the rotation loop if it had begun perfectly
// aligned with the Handoff Grid. We then calculate the rotation position based on the time
// elapsed since this Virtual Start.
func calculateRotationGroup(
	epoch *model.RotationEpoch,
	rotationType model.RotationType,
	handoffDay *int,
	handoffHour, handoffMin int,
	loc *time.Location,
	at time.Time,
) []string {
	if len(epoch.Groups) == 0 {
		return nil
	}

	localAt := at.In(loc)
	localEpochStart := epoch.StartTime.In(loc)

	var position int

	// Correctly align rotation to the "Handoff Grid" defined by the Schedule.
	// Users expect rotation to switch at "Handoff Time" (e.g. Monday 11:00),
	// regardless of when the schedule was created (Epoch Start).
	//
	// To achieve this, we calculate a "Virtual Start" for the epoch:
	// The most recent Handoff Time that occurred at or before Epoch.StartTime.
	// We then measure elapsed time from this Virtual Start.

	// 1. Find the Handoff Time immediately preceding (or equal to) epoch.StartTime
	// Start with the day of epoch start
	epochDay := time.Date(localEpochStart.Year(), localEpochStart.Month(), localEpochStart.Day(), 0, 0, 0, 0, loc)
	epochHandoff := time.Date(epochDay.Year(), epochDay.Month(), epochDay.Day(), handoffHour, handoffMin, 0, 0, loc)

	// If epoch started before today's handoff, the "current" grid slot started yesterday (or last week)
	// But wait, we just need to align to the *Grid*.
	// For Daily: Virtual Start is simply epochHandoff (if epochStart >= epochHandoff) or epochHandoff-1day.
	// For Weekly: We need to find the previous "Handoff Day" (e.g. Previous Monday).

	var virtualStart time.Time

	switch rotationType {
	case model.RotationDaily:
		if localEpochStart.Before(epochHandoff) {
			virtualStart = epochHandoff.AddDate(0, 0, -1)
		} else {
			virtualStart = epochHandoff
		}

		// DST-Safe Day Calculation
		// Estimate days using 24h/day
		daysSinceStart := int(localAt.Sub(virtualStart).Hours() / 24)

		// Correct estimation using AddDate (which handles daylight saving shifts correctly)
		targetWithDays := virtualStart.AddDate(0, 0, daysSinceStart)

		// Check for undershoot: if we can add one more day and still be <= localAt
		if !virtualStart.AddDate(0, 0, daysSinceStart+1).After(localAt) {
			daysSinceStart++
		} else if targetWithDays.After(localAt) {
			// Check for overshoot
			daysSinceStart--
		}

		position = daysSinceStart % len(epoch.Groups)

	case model.RotationWeekly:
		// Find the weekday of the epoch start day
		weekday := int(epochDay.Weekday()) // Sun=0, Mon=1...

		// Guard against nil handoffDay for weekly rotation
		targetDay := 1 // Default to Monday
		if handoffDay != nil {
			targetDay = *handoffDay
		}

		// Calculate days to subtract to reach previous 'targetDay'
		daysBack := (weekday - targetDay + 7) % 7

		// Potential virtual start is epochDay - daysBack
		potentialStartDay := epochDay.AddDate(0, 0, -daysBack)
		potentialStart := time.Date(potentialStartDay.Year(), potentialStartDay.Month(), potentialStartDay.Day(), handoffHour, handoffMin, 0, 0, loc)

		if potentialStart.After(localEpochStart) {
			potentialStart = potentialStart.AddDate(0, 0, -7)
		}

		virtualStart = potentialStart

		// DST-Safe Week Calculation
		weeksSinceStart := int(localAt.Sub(virtualStart).Hours() / (24 * 7))

		// Correct estimation
		targetWithWeeks := virtualStart.AddDate(0, 0, weeksSinceStart*7)

		if !virtualStart.AddDate(0, 0, (weeksSinceStart+1)*7).After(localAt) {
			weeksSinceStart++
		} else if targetWithWeeks.After(localAt) {
			weeksSinceStart--
		}

		position = weeksSinceStart % len(epoch.Groups)

	default:
		position = 0
	}

	// Handle case where virtual calculation drifts (shouldn't happen with correct logic)
	if position < 0 {
		position = 0
	}

	return epoch.Groups[position]
}

// getOverlappingOverrides returns overrides that overlap with the given time range
func (g *SegmentGenerator) getOverlappingOverrides(overrides []*model.ScheduleOverride, start, end time.Time) []*model.ScheduleOverride {
	var result []*model.ScheduleOverride
	for _, o := range overrides {
		if o.StartTime.Before(end) && o.EndTime.After(start) {
			result = append(result, o)
		}
	}
	return result
}

// splitByOverrides splits a rotation segment by overlapping overrides
func (g *SegmentGenerator) splitByOverrides(
	rotationUserIDs []string,
	start, end time.Time,
	overrides []*model.ScheduleOverride,
	users map[string]*model.User,
) []model.OnCallSegment {
	type slot struct {
		start    time.Time
		end      time.Time
		userIDs  []string
		layer    string
		override *model.ScheduleOverride
	}

	// Start with rotation covering the whole segment
	slots := []slot{{start: start, end: end, userIDs: rotationUserIDs, layer: "l1"}}

	// Split by each override
	for _, override := range overrides {
		var newSlots []slot
		for _, s := range slots {
			if s.layer == "override" {
				newSlots = append(newSlots, s)
				continue
			}

			// Clamp override to slot boundaries
			overStart := override.StartTime
			overEnd := override.EndTime
			if overStart.Before(s.start) {
				overStart = s.start
			}
			if overEnd.After(s.end) {
				overEnd = s.end
			}

			// No overlap
			if !overStart.Before(s.end) || !overEnd.After(s.start) {
				newSlots = append(newSlots, s)
				continue
			}

			// Before override
			if overStart.After(s.start) {
				newSlots = append(newSlots, slot{
					start:   s.start,
					end:     overStart,
					userIDs: s.userIDs,
					layer:   s.layer,
				})
			}

			// The override itself (override replaces entire group with single user)
			newSlots = append(newSlots, slot{
				start:    overStart,
				end:      overEnd,
				userIDs:  []string{override.UserID},
				layer:    "override",
				override: override,
			})

			// After override
			if overEnd.Before(s.end) {
				newSlots = append(newSlots, slot{
					start:   overEnd,
					end:     s.end,
					userIDs: s.userIDs,
					layer:   s.layer,
				})
			}
		}
		slots = newSlots
	}

	// Convert to segments
	var segments []model.OnCallSegment
	for _, s := range slots {
		seg := model.OnCallSegment{
			UserIDs:   s.userIDs,
			StartTime: s.start,
			EndTime:   s.end,
			Layer:     s.layer,
			Override:  s.override,
		}
		for _, id := range s.userIDs {
			if u, ok := users[id]; ok {
				seg.Users = append(seg.Users, u)
			}
		}
		segments = append(segments, seg)
	}

	return segments
}

// parseHandoffTime parses "HH:MM" format into hour and minute
func parseHandoffTime(handoffTime string) (hour, minute int) {
	hour, minute = 11, 0 // Default
	if handoffTime == "" {
		return
	}
	parts := strings.Split(handoffTime, ":")
	if len(parts) >= 2 {
		if h, err := strconv.Atoi(parts[0]); err == nil {
			hour = h
		}
		if m, err := strconv.Atoi(parts[1]); err == nil {
			minute = m
		}
	}
	return
}

// splitByCalendarDays takes segments and splits them by calendar day boundaries.
// This is an internal helper used by RenderCalendarSchedule.
func (g *SegmentGenerator) splitByCalendarDays(segments []model.OnCallSegment, loc *time.Location) []model.OnCallSegment {
	if loc == nil {
		loc = time.UTC
	}

	var result []model.OnCallSegment

	for _, seg := range segments {
		localStart := seg.StartTime.In(loc)
		localEnd := seg.EndTime.In(loc)

		// Start from the day of segment start
		currentDayStart := time.Date(localStart.Year(), localStart.Month(), localStart.Day(), 0, 0, 0, 0, loc)

		for {
			nextDayStart := currentDayStart.AddDate(0, 0, 1)

			// Segment start for this day
			daySegStart := localStart
			if currentDayStart.After(localStart) {
				daySegStart = currentDayStart
			}

			// Segment end for this day
			daySegEnd := localEnd
			if nextDayStart.Before(localEnd) {
				daySegEnd = nextDayStart
			}

			// Only add if segment has positive duration
			if daySegEnd.After(daySegStart) {
				newSeg := model.OnCallSegment{
					UserIDs:   seg.UserIDs,
					Users:     seg.Users,
					StartTime: daySegStart.UTC(),
					EndTime:   daySegEnd.UTC(),
					Layer:     seg.Layer,
					Override:  seg.Override,
				}
				result = append(result, newSeg)
			}

			// Move to next day
			currentDayStart = nextDayStart
			if !currentDayStart.Before(localEnd) {
				break
			}
		}
	}

	return result
}

// RenderCalendarSchedule prepares segments for calendar UI rendering.
// It performs two operations:
// 1. Splits segments by calendar day boundaries (in the given timezone)
// 2. Merges adjacent segments for the same user on the same day
//
// Example: If Alice is on-call 11:00 day 1 → 11:00 day 3 (continuous):
// - Day 1: 11:00-24:00 Alice
// - Day 2: 00:00-24:00 Alice (merged from 00:00-11:00 + 11:00-24:00)
// - Day 3: 00:00-11:00 Alice
func (g *SegmentGenerator) RenderCalendarSchedule(segments []model.OnCallSegment, loc *time.Location) []model.OnCallSegment {
	if loc == nil {
		loc = time.UTC
	}

	// Step 1: Split by calendar days
	split := g.splitByCalendarDays(segments, loc)

	if len(split) == 0 {
		return split
	}

	// Step 2: Merge adjacent segments for same user on same day
	var result []model.OnCallSegment

	for _, seg := range split {
		// Check if we can merge with the last segment
		if len(result) > 0 {
			last := &result[len(result)-1]

			// Same user, same layer, same override, adjacent times, same day?
			if sliceEqual(last.UserIDs, seg.UserIDs) &&
				last.Layer == seg.Layer &&
				last.EndTime.Equal(seg.StartTime) &&
				sameCalendarDay(last.StartTime, seg.StartTime, loc) &&
				sameOverride(last.Override, seg.Override) {
				// Merge: extend the last segment
				last.EndTime = seg.EndTime
				continue
			}
		}
		// Can't merge, add as new
		result = append(result, seg)
	}

	return result
}

// sameCalendarDay checks if two times are on the same calendar day in the given timezone
func sameCalendarDay(t1, t2 time.Time, loc *time.Location) bool {
	l1 := t1.In(loc)
	l2 := t2.In(loc)
	return l1.Year() == l2.Year() && l1.YearDay() == l2.YearDay()
}

// sameOverride checks if two segments belong to the same override (or both have none)
func sameOverride(a, b *model.ScheduleOverride) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.ID == b.ID
}
