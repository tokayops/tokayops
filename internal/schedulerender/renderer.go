package schedulerender

import (
	"fmt"
	"sort"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Input is everything Render needs. It is passed in rather than fetched:
// Render is pure, and the consistency of the three collections is the
// snapshot's responsibility, not the renderer's.
type Input struct {
	Root      scheduleconfig.ScheduleRoot
	Revisions []scheduleconfig.ScheduleRevision

	// Overrides is the override projection for the range, both layers mixed.
	Overrides []scheduleconfig.OverrideRevision

	From  time.Time
	Until time.Time
}

// Result is the rendered range.
type Result struct {
	From  time.Time
	Until time.Time

	// Assignments are atomic: one grid slot of one revision each, cut where
	// an override takes over. Use MergeAdjacent to present natural shifts.
	Assignments []Assignment

	// HistoryComplete is false when part of the range precedes the point from
	// which this schedule's history is exact.
	HistoryComplete     bool
	HistoryCompleteFrom *time.Time

	// DeletedAt is the current soft-delete stamp of the schedule, if any.
	DeletedAt *time.Time

	Warnings []Warning
}

// Render answers "who was on duty across this range" from revisions alone.
//
// It never consults the mutable schedule row: two revisions may disagree on
// timezone, cadence and handoff time, and only the one in force at a given
// instant may decide what the grid looked like then. That is why the grid is
// rebuilt per revision and per layer instead of once per query.
func Render(in Input) (Result, error) {
	if !in.Until.After(in.From) {
		return Result{}, fmt.Errorf("schedulerender: range %v..%v is empty or inverted", in.From, in.Until)
	}

	res := Result{
		From:                in.From,
		Until:               in.Until,
		HistoryCompleteFrom: in.Root.HistoryCompleteFrom,
		DeletedAt:           in.Root.DeletedAt,
		HistoryComplete:     true,
	}
	queryRange := interval{Start: in.From, End: in.Until}

	revisions := append([]scheduleconfig.ScheduleRevision(nil), in.Revisions...)
	sort.SliceStable(revisions, func(i, j int) bool {
		return revisions[i].EffectiveFrom.Before(revisions[j].EffectiveFrom)
	})

	// History before the first recorded revision is not a gap in the chain,
	// it is the part of the past this schedule cannot speak about. Saying
	// "gap" there would cry damage on every schedule younger than the query.
	if from := in.Root.HistoryCompleteFrom; from == nil || in.From.Before(*from) {
		res.HistoryComplete = false
		until := in.Until
		if from != nil {
			until = minTime(in.Until, *from)
		}
		res.Warnings = append(res.Warnings, Warning{
			Code:  WarnHistoryIncomplete,
			From:  in.From,
			Until: until,
		})
	}

	for i, rev := range revisions {
		if i > 0 {
			prev := revisions[i-1]
			res.Warnings = appendGapWarning(res.Warnings, prev, rev, queryRange)
		}

		bound := revisionInterval(rev).intersect(queryRange)
		if bound.empty() {
			continue
		}

		// A deleted period is a recorded fact, not a hole: nobody was on duty
		// and the chain is intact. Deriving assignments from the snapshot it
		// carries would put a rotation back on a schedule that did not exist.
		if rev.Kind == scheduleconfig.RevisionDeleted {
			res.Warnings = append(res.Warnings, Warning{
				Code:       WarnScheduleInactive,
				From:       bound.Start,
				Until:      bound.End,
				RelatedIDs: []string{rev.ID},
			})
			continue
		}

		for _, layer := range []string{LayerL1, LayerL2} {
			assignments, warnings, err := renderLayer(rev, layer, bound, in.Overrides)
			if err != nil {
				return Result{}, err
			}
			res.Assignments = append(res.Assignments, assignments...)
			res.Warnings = append(res.Warnings, warnings...)
		}
	}

	sort.SliceStable(res.Assignments, func(i, j int) bool {
		if res.Assignments[i].Layer != res.Assignments[j].Layer {
			return res.Assignments[i].Layer < res.Assignments[j].Layer
		}
		return res.Assignments[i].AssignmentStart.Before(res.Assignments[j].AssignmentStart)
	})
	return res, nil
}

// renderLayer walks the grid slots one revision's layer covers inside bound.
//
// The layer is walked even when its rotation is inactive. Skipping it whole -
// the obvious reading of the algorithm - would silently delete an override
// that outlives the edit which disabled the layer, and an override names the
// person on duty until its own valid_to whatever the rotation is doing.
func renderLayer(rev scheduleconfig.ScheduleRevision, layer string, bound interval,
	overrides []scheduleconfig.OverrideRevision) ([]Assignment, []Warning, error) {

	snapshot := rev.Snapshot
	layerSnapshot := snapshot.L1
	if layer == LayerL2 {
		layerSnapshot = snapshot.L2
	}

	// The grid comes from the revision's own timezone and policy. Even a
	// disabled layer carries a valid policy, which is what lets an override
	// on a switched-off layer still report the grid slot it fell in.
	grid, err := rotation.NewGrid(snapshot.Timezone, layerSnapshot.Policy)
	if err != nil {
		return nil, nil, fmt.Errorf("schedulerender: revision %s layer %s: %w", rev.ID, layer, err)
	}

	layerOverrides := overridesOfLayer(overrides, layer)
	active := layerSnapshot.Enabled && len(layerSnapshot.Groups) > 0
	if !active && len(layerOverrides) == 0 {
		return nil, nil, nil
	}

	slots := grid.SlotsOverlapping(bound.Start, bound.End)
	if len(slots) == 0 {
		return nil, nil, nil
	}

	// PositionAt walks the grid from the phase anchor, which can be years
	// back. It is called ONCE, for the first slot; every following slot is
	// the next one along the grid, so the position simply advances by one.
	// Calling it per slot would multiply a cost that already grows with the
	// age of the anchor.
	position := 0
	if active {
		position, _, err = rotation.PositionAt(grid, layerSnapshot, slots[0].Start)
		if err != nil {
			return nil, nil, fmt.Errorf("schedulerender: revision %s layer %s: %w", rev.ID, layer, err)
		}
	}

	var (
		out      []Assignment
		warnings []Warning
	)
	for _, slot := range slots {
		in := slotInput{
			RevisionID: rev.ID,
			Layer:      layer,
			Slot:       slot,
			Bound:      interval{Start: slot.Start, End: slot.End}.intersect(bound),
			Overrides:  layerOverrides,
		}
		if active {
			group := layerSnapshot.Groups[position]
			in.Base = &baseGroup{GroupID: group.ID, UserIDs: group.Members}

			position++
			if position == len(layerSnapshot.Groups) {
				position = 0
			}
		}

		slotAssignments, slotWarnings := renderSlot(in)
		out = append(out, slotAssignments...)
		warnings = append(warnings, slotWarnings...)
	}
	return out, warnings, nil
}

func overridesOfLayer(overrides []scheduleconfig.OverrideRevision, layer string) []scheduleconfig.OverrideRevision {
	var out []scheduleconfig.OverrideRevision
	for _, o := range overrides {
		if o.Layer == layer {
			out = append(out, o)
		}
	}
	return out
}

// appendGapWarning reports a break between two revisions that should be
// adjacent. With deleted periods recorded as revisions of their own, the only
// remaining cause is a lost row.
func appendGapWarning(out []Warning, prev, next scheduleconfig.ScheduleRevision, queryRange interval) []Warning {
	if prev.EffectiveTo == nil || !next.EffectiveFrom.After(*prev.EffectiveTo) {
		return out
	}
	gap := interval{Start: *prev.EffectiveTo, End: next.EffectiveFrom}.intersect(queryRange)
	if gap.empty() {
		return out
	}
	return append(out, Warning{
		Code:       WarnRevisionGap,
		From:       gap.Start,
		Until:      gap.End,
		RelatedIDs: []string{prev.ID, next.ID},
	})
}
