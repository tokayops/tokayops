package schedulerender

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// slotInput is one grid slot of one layer of one revision, plus everything
// needed to decide who is actually on duty inside it.
type slotInput struct {
	RevisionID string
	Layer      string

	// Slot is the raw grid slot. Its boundaries are reported unclamped.
	Slot rotation.Slot

	// Bound is the part of the slot the answer covers: the slot intersected
	// with the revision's effective interval and the caller's range.
	Bound interval

	// Base is the rotation group serving this slot, or nil when the layer is
	// disabled or has no groups.
	Base *baseGroup

	// Overrides are the effective override revisions of THIS layer. They need
	// not be sorted and need not be free of overlap.
	Overrides []scheduleconfig.OverrideRevision
}

// renderSlot is the only place where overrides are laid over a rotation.
//
// Both readers go through it: the historical renderer calls it per slot, and
// the current on-call projection finds its own slot and calls it once. A
// second implementation for the "simple" current case is exactly how the two
// would come to disagree - the on-call answer around an override needs the
// same boundaries in both directions, not just the next override ahead.
//
// A nil Base does NOT suppress overrides. An override keeps naming the person
// on duty until its own valid_to, so switching a layer off or emptying its
// groups in the middle of one must not make it vanish; the interval the
// override does not cover is then simply nobody's.
func renderSlot(in slotInput) ([]Assignment, []Warning, error) {
	if in.Bound.empty() {
		return nil, nil, nil
	}

	covering := overridesForSlot(in.Overrides, in.Bound)
	points := elementaryPoints(covering, in.Bound)

	var (
		out      []Assignment
		warnings []Warning
	)
	for i := 0; i+1 < len(points); i++ {
		span := interval{Start: points[i], End: points[i+1]}
		if span.empty() {
			continue
		}

		active := coveringAt(covering, span.Start)
		if len(active) > 1 {
			// Refused rather than resolved by "the last one wins". The command
			// side cannot create this pair, so finding it means the data was
			// edited past the code - and picking a winner would put an
			// arbitrary person on duty and say nothing.
			ids := make([]string, 0, len(active))
			for _, o := range active {
				ids = append(ids, o.OverrideID)
			}
			sort.Strings(ids)
			return nil, nil, fmt.Errorf("%w: layer %s over %s..%s, overrides %s",
				scheduleconfig.ErrOverrideCollision, in.Layer,
				span.Start.Format(time.RFC3339), span.End.Format(time.RFC3339),
				strings.Join(ids, " and "))
		}

		switch {
		case len(active) > 0:
			winner := active[len(active)-1]
			out = appendAssignment(out, Assignment{
				ScheduleRevisionID: in.RevisionID,
				Layer:              in.Layer,
				Source:             SourceOverride,
				GroupID:            winner.OverrideID,
				UserIDs:            []string{winner.UserID},
				GridSlotStart:      in.Slot.Start,
				GridSlotEnd:        in.Slot.End,
				AssignmentStart:    span.Start,
				AssignmentEnd:      span.End,
				OverrideID:         winner.OverrideID,
				OverrideRevisionID: winner.RevisionID,
			})
		case in.Base != nil:
			out = appendAssignment(out, Assignment{
				ScheduleRevisionID: in.RevisionID,
				Layer:              in.Layer,
				Source:             SourceRotation,
				GroupID:            in.Base.GroupID,
				// The only place a group's members escape the package, so the
				// only place they are copied: the base still borrows them from
				// the revision snapshot.
				UserIDs:         append([]string(nil), in.Base.UserIDs...),
				GridSlotStart:   in.Slot.Start,
				GridSlotEnd:     in.Slot.End,
				AssignmentStart: span.Start,
				AssignmentEnd:   span.End,
			})
		}
		// No base and no override: nobody is on duty for this span. The gap
		// is left as a gap rather than filled with the surrounding group.
	}
	return out, warnings, nil
}

// overridesForSlot keeps the overrides that reach the bound and sorts them by
// priority ASCENDING, so that the last covering entry always wins. Their own
// validity intervals are left intact: the bound decides which pieces are
// reported, not what an override covers.
//
// The sort is what makes the result independent of the order the caller
// supplied: overlapping overrides must resolve the same way no matter how the
// projection happened to return them. Priority is the later recorded_at, and
// for two recorded in the same microsecond the greater override_id - an
// arbitrary but total rule, which is what determinism requires.
func overridesForSlot(overrides []scheduleconfig.OverrideRevision, bound interval) []scheduleconfig.OverrideRevision {
	out := make([]scheduleconfig.OverrideRevision, 0, len(overrides))
	for _, o := range overrides {
		if !o.ValidTo.After(bound.Start) || !o.ValidFrom.Before(bound.End) {
			continue
		}
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].RecordedAt.Equal(out[j].RecordedAt) {
			return out[i].RecordedAt.Before(out[j].RecordedAt)
		}
		return out[i].OverrideID < out[j].OverrideID
	})
	return out
}

// elementaryPoints returns the sorted distinct instants at which the answer
// can change inside the bound.
func elementaryPoints(overrides []scheduleconfig.OverrideRevision, bound interval) []time.Time {
	points := []time.Time{bound.Start, bound.End}
	for _, o := range overrides {
		if o.ValidFrom.After(bound.Start) && o.ValidFrom.Before(bound.End) {
			points = append(points, o.ValidFrom)
		}
		if o.ValidTo.After(bound.Start) && o.ValidTo.Before(bound.End) {
			points = append(points, o.ValidTo)
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Before(points[j]) })

	distinct := points[:0:0]
	for i, p := range points {
		if i > 0 && p.Equal(points[i-1]) {
			continue
		}
		distinct = append(distinct, p)
	}
	return distinct
}

// coveringAt returns the overrides covering an instant, keeping the priority
// order of its input, so the last element is the winner.
func coveringAt(overrides []scheduleconfig.OverrideRevision, at time.Time) []scheduleconfig.OverrideRevision {
	var out []scheduleconfig.OverrideRevision
	for _, o := range overrides {
		if !o.ValidFrom.After(at) && o.ValidTo.After(at) {
			out = append(out, o)
		}
	}
	return out
}

// appendAssignment adds a piece, merging it into the previous one when they
// are the same assignment split only by a boundary that turned out not to
// change anything.
func appendAssignment(out []Assignment, next Assignment) []Assignment {
	if n := len(out); n > 0 {
		last := &out[n-1]
		if last.AssignmentEnd.Equal(next.AssignmentStart) && sameAssignment(*last, next) {
			last.AssignmentEnd = next.AssignmentEnd
			return out
		}
	}
	return append(out, next)
}

// sameAssignment reports whether two pieces describe the same duty. It
// deliberately ignores the revision: two pieces of one slot separated by a
// metadata-only save are the same assignment.
func sameAssignment(a, b Assignment) bool {
	return a.Layer == b.Layer &&
		a.Source == b.Source &&
		a.GroupID == b.GroupID &&
		a.OverrideID == b.OverrideID &&
		a.OverrideRevisionID == b.OverrideRevisionID &&
		equalIDs(a.UserIDs, b.UserIDs)
}

func equalIDs(a, b []string) bool {
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
