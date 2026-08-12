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

	// OverrideRevisionID identifies the VERSION of the override in force, and
	// is set only for SourceOverride.
	//
	// It is what tells two arrivals of the same stand-in apart. Editing an
	// override does not move its valid_from, so neither the composition nor
	// AssignmentStart changes when the person on it is swapped and swapped
	// back - only the revision does.
	OverrideRevisionID string
}

// OnCall is the current-assignment projection. A nil layer means nobody is on
// duty there - the layer is off, has no groups, or the schedule did not exist
// at that instant.
type OnCall struct {
	At time.Time
	L1 *LayerOnCall
	L2 *LayerOnCall
}

// It carries no warnings. The two that remain - history_incomplete and
// schedule_inactive - are about a RANGE, and this type answers about one
// instant: at that instant a schedule is either readable or it is not, and
// unreadable is an error now rather than a note attached to a guess.

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
	overrides []scheduleconfig.OverrideRevision) (OnCall, error) {

	out := OnCall{At: at}
	for _, ls := range slots {
		assignments, err := renderSlot(slotInput{
			RevisionID: rev.ID,
			Layer:      ls.layer,
			Slot:       ls.slot,
			Bound:      ls.bound,
			Base:       ls.base,
			Overrides:  overridesOfLayer(overrides, ls.layer),
		})
		if err != nil {
			return OnCall{}, err
		}

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
			OverrideRevisionID: found.OverrideRevisionID,
		}
		if ls.layer == LayerL1 {
			out.L1 = layerOnCall
		} else {
			out.L2 = layerOnCall
		}
	}
	return out, nil
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

// TeamOnCallResult is a caller's answer to "who is on duty for this team", and
// it has three states rather than two: the team has a schedule, the team has
// none, or the question could not be answered at all.
//
// The third state has to survive the journey. Collapsed into the second - a
// zero TeamOnCall, as a bare value would be - a failed read looks like a team
// that never configured a schedule, and a consumer that treats "no schedule" as
// a reason to look elsewhere then acts on a state that was never observed.
//
// It lives beside TeamOnCall rather than with any one consumer: what it
// describes is a read of this projection, and two consumers - the alert engine
// and the escalation builder - pass it between them. Declaring it in either
// would make the other depend on its neighbour for a word about schedules.
//
// The zero value means "this team has no schedule". Carry a real read with
// TeamOnCallRead, which cannot drop the error because it takes it.
type TeamOnCallResult struct {
	onCall TeamOnCall
	err    error
}

// TeamOnCallRead wraps what a projection read returned, error included. It
// takes the pair so it can be applied directly to the call:
//
//	schedulerender.TeamOnCallRead(projection.CurrentTeamOnCallNow(ctx, teamID))
func TeamOnCallRead(onCall TeamOnCall, err error) TeamOnCallResult {
	return TeamOnCallResult{onCall: onCall, err: err}
}

// Err reports why the team's on-call state is unknown, if it is.
func (r TeamOnCallResult) Err() error { return r.err }

// OnCall is the projection that was read. It is only meaningful when Err is nil.
func (r TeamOnCallResult) OnCall() TeamOnCall { return r.onCall }
