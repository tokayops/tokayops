package scheduler

import "time"

// CurrentOnCallLookback is how far back GenerateCurrentSegment looks
// to find the rotation segment covering "now". 8 days covers weekly handoffs.
const CurrentOnCallLookback = 8 * 24 * time.Hour

// CurrentOnCallLookahead is how far forward GenerateCurrentSegment looks
// to determine the segment's end time.
const CurrentOnCallLookahead = 31 * 24 * time.Hour

// CurrentOnCallWindow returns the [from, until) time range used by
// GenerateCurrentSegment and expected by callers that fetch data for it.
func CurrentOnCallWindow(at time.Time) (from, until time.Time) {
	return at.Add(-CurrentOnCallLookback), at.Add(CurrentOnCallLookahead)
}
