// Package schedulerender is the read side of schedule revisions: historical
// rendering and the current on-call projection.
//
// It is a package of its own rather than files inside internal/scheduler
// because the rule it must obey - rotation math reads revisions, never the
// mutable schedule row - is worth having the compiler enforce. This package
// imports internal/rotation and the envelope types of internal/scheduleconfig
// and nothing else; there is no way to reach a *model.Schedule or the store
// from here, so the ban is structural rather than a comment someone has to
// remember. The legacy rotation math in internal/scheduler stays live until
// the cutover removes it.
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

	// WarnRevisionGap means the revision chain fails to cover a stretch it
	// should have covered - between two revisions, before the first, or after
	// the last. Since a deleted period is itself a revision, this only ever
	// means data loss.
	WarnRevisionGap WarningCode = "revision_gap"

	// WarnRevisionOverlap means two revisions claim the same instant. The
	// exclusion constraint forbids it, so this is corruption; the renderer
	// resolves it in favour of the earlier revision and says so.
	WarnRevisionOverlap WarningCode = "revision_overlap"

	// WarnScheduleInactive marks the interval covered by a deleted-kind
	// revision. It is an expected state, not damage.
	WarnScheduleInactive WarningCode = "schedule_inactive"

	// WarnOverrideOverlap means two overrides of one layer claimed the same
	// instant. The renderer resolves it deterministically and says so rather
	// than picking one silently.
	WarnOverrideOverlap WarningCode = "override_overlap"
)

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
