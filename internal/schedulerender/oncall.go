package schedulerender

import (
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// LayerOnCall is who is on duty on one layer at one instant.
//
// It carries both pairs of boundaries for the reason the old API could not:
// one field cannot mean both "the handoff grid says the shift started at
// 11:00" and "this composition only took effect at 15:00, when it was saved".
type LayerOnCall struct {
	GroupID string
	UserIDs []string

	GridSlotStart   time.Time
	GridSlotEnd     time.Time
	AssignmentStart time.Time
	AssignmentEnd   time.Time

	ScheduleRevisionID string
	Source             string
	OverrideID         string
}

// OnCall is the current-assignment projection. A nil layer means nobody is on
// duty there - the layer is off, has no groups, or the schedule did not exist
// at that instant.
type OnCall struct {
	At time.Time
	L1 *LayerOnCall
	L2 *LayerOnCall

	// Warnings surface here too: an override overlap is no less wrong for
	// being observed through the current view rather than the history.
	Warnings []Warning
}

// layerSlot is the grid slot of one layer at the queried instant, already
// clipped to the revision.
type layerSlot struct {
	layer string
	slot  rotation.Slot
	bound interval
	base  *baseGroup
}

// onCallSlots resolves the slot each layer of a revision is in at `at`.
//
// The slot is found here, from the grid, and never inherited from a rendered
// segment: rendering merges adjacent slots into natural shifts, and the merged
// boundaries answer a different question than "which slot is `at` in".
func onCallSlots(rev scheduleconfig.ScheduleRevision, at time.Time) ([]layerSlot, error) {
	revBound := revisionInterval(rev)
	out := make([]layerSlot, 0, 2)

	for _, layer := range []string{LayerL1, LayerL2} {
		state, err := resolveLayer(rev, layer)
		if err != nil {
			return nil, err
		}
		slot := state.grid.SlotContaining(at)

		ls := layerSlot{
			layer: layer,
			slot:  slot,
			bound: interval{Start: slot.Start, End: slot.End}.intersect(revBound),
		}
		// An inactive rotation still yields a slot: an override on a layer
		// that was switched off mid-shift is still in force, and it reports
		// the grid slot it fell in.
		if state.active {
			position, _, err := rotation.PositionAt(state.grid, state.snapshot, at)
			if err != nil {
				return nil, layerError(rev, layer, err)
			}
			ls.base = state.groupAt(position)
		}
		out = append(out, ls)
	}
	return out, nil
}

// projectOnCall overlays the overrides onto each layer's slot and picks the
// assignment covering `at`.
//
// The overlay goes through renderSlot, the same primitive the historical
// renderer uses. Reimplementing it here as "the rotation, unless an override
// starts later in this slot" is the tempting shortcut and it is wrong in the
// other direction: after an override ends, the rotation resumes at the
// override's valid_to, not at the start of the grid slot.
func projectOnCall(rev scheduleconfig.ScheduleRevision, at time.Time, slots []layerSlot,
	overrides []scheduleconfig.OverrideRevision) OnCall {

	out := OnCall{At: at}
	for _, ls := range slots {
		assignments, warnings := renderSlot(slotInput{
			RevisionID: rev.ID,
			Layer:      ls.layer,
			Slot:       ls.slot,
			Bound:      ls.bound,
			Base:       ls.base,
			Overrides:  overridesOfLayer(overrides, ls.layer),
		})
		out.Warnings = append(out.Warnings, warnings...)

		found := assignmentAt(assignments, at)
		if found == nil {
			continue
		}
		layerOnCall := &LayerOnCall{
			GroupID:            found.GroupID,
			UserIDs:            found.UserIDs,
			GridSlotStart:      found.GridSlotStart,
			GridSlotEnd:        found.GridSlotEnd,
			AssignmentStart:    found.AssignmentStart,
			AssignmentEnd:      found.AssignmentEnd,
			ScheduleRevisionID: found.ScheduleRevisionID,
			Source:             found.Source,
			OverrideID:         found.OverrideID,
		}
		if ls.layer == LayerL1 {
			out.L1 = layerOnCall
		} else {
			out.L2 = layerOnCall
		}
	}
	return out
}

func assignmentAt(assignments []Assignment, at time.Time) *Assignment {
	for i := range assignments {
		if !assignments[i].AssignmentStart.After(at) && assignments[i].AssignmentEnd.After(at) {
			return &assignments[i]
		}
	}
	return nil
}

// onCallOverrideRange is the span of overrides the projection needs: the union
// of the layer slots. Anything outside cannot affect who is on duty now.
func onCallOverrideRange(slots []layerSlot) (from, until time.Time, ok bool) {
	for _, ls := range slots {
		if ls.bound.empty() {
			continue
		}
		if !ok {
			from, until, ok = ls.bound.Start, ls.bound.End, true
			continue
		}
		from = minTime(from, ls.bound.Start)
		until = maxTime(until, ls.bound.End)
	}
	return from, until, ok
}
