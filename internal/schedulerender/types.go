// Package schedulerender is the read side of schedule revisions: historical
// rendering and the current on-call projection.
//
// It is a package of its own because the rule it must obey - rotation math
// reads revisions, never a mutable schedule row - is worth having the compiler
// enforce. It imports internal/rotation and the envelope types of
// internal/scheduleconfig and nothing else; the store is out of reach from
// here, so the ban is structural rather than a comment someone has to
// remember.
package schedulerender

import (
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Layers an assignment can belong to.
const (
	LayerL1 = scheduleconfig.LayerL1
	LayerL2 = scheduleconfig.LayerL2
)

// Assignment sources.
const (
	// SourceRotation means the base rotation put this group on duty.
	SourceRotation = "rotation"

	// SourceOverride means an override names the person on duty, whatever the
	// rotation says.
	SourceOverride = "override"
)

// Assignment is one atomic answer to "who was on duty": exactly one grid slot
// of one revision, possibly cut short by an override.
//
// The two pairs of boundaries are deliberately separate:
//
//   - GridSlotStart/End are the mathematical handoff boundaries of the grid,
//     reported as they are. After a policy edit in the middle of a slot the
//     grid boundary can precede the revision that introduced that grid, and
//     clamping it would misreport when the shift itself began.
//   - AssignmentStart/End are the real boundaries after clipping to the
//     revision, the override and the query range. AssignmentStart is never
//     earlier than the revision: the composition it describes did not exist
//     before then.
//
// UserIDs are IDs, not user records. Resolving them to names belongs to the
// API layer, which is also where an anonymized user has to be presented.
type Assignment struct {
	ScheduleRevisionID string
	Layer              string
	Source             string

	// GroupID is the stable rotation group for SourceRotation and the logical
	// override ID for SourceOverride.
	GroupID string
	UserIDs []string

	GridSlotStart   time.Time
	GridSlotEnd     time.Time
	AssignmentStart time.Time
	AssignmentEnd   time.Time

	// OverrideID and OverrideRevisionID are set only for SourceOverride.
	OverrideID         string
	OverrideRevisionID string
}

// WarningCode identifies a condition the caller has to be able to branch on.
type WarningCode string

const (
	// WarnHistoryIncomplete means part of the queried range precedes the
	// point from which this schedule's history is exact. Inferred history is
	// never returned as if it were recorded.
	WarnHistoryIncomplete WarningCode = "history_incomplete"

	// WarnScheduleInactive marks the interval covered by a deleted-kind
	// revision. It is an expected state, not damage.
	WarnScheduleInactive WarningCode = "schedule_inactive"
)

// Both remaining codes describe states a user produced on purpose: a range
// that reaches before this schedule's history, and a period during which the
// schedule was deleted.
//
// Three codes are gone - revision_gap, revision_overlap, override_overlap.
// Each described damage the write paths cannot produce, and each came attached
// to a calendar the renderer had drawn around the damage. A plausible answer
// about who was on duty is worse than no answer: it is the same failure the
// epic refused when it stopped reporting "nobody" for a schedule it could not
// read. They are errors now.

// Warning is structured because both the API and the tests consume it: a
// message string would force either to parse prose.
type Warning struct {
	Code  WarningCode
	Layer string
	From  time.Time
	Until time.Time

	// RelatedIDs names whatever the code is about: the overrides that
	// overlapped, the revisions on either side of a gap.
	RelatedIDs []string
}

// interval is a half-open [Start, End) span used while clipping.
type interval struct {
	Start time.Time
	End   time.Time
}

func (i interval) empty() bool { return !i.End.After(i.Start) }

// intersect returns the overlap of two intervals; the result is empty when
// they do not overlap.
func (i interval) intersect(o interval) interval {
	out := i
	if o.Start.After(out.Start) {
		out.Start = o.Start
	}
	if o.End.Before(out.End) {
		out.End = o.End
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// revisionInterval is the effective span of a revision. A tail revision is
// open-ended, which is represented by the far-future sentinel below so that
// clipping needs no special case.
func revisionInterval(rev scheduleconfig.ScheduleRevision) interval {
	end := endOfTime
	if rev.EffectiveTo != nil {
		end = *rev.EffectiveTo
	}
	return interval{Start: rev.EffectiveFrom, End: end}
}

// endOfTime stands in for "no end yet". It is far past any date a schedule
// query can name, and it never leaves this package: every assignment is
// clipped to the caller's range before it is returned.
var endOfTime = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
