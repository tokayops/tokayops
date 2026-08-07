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
	// The database stores timestamps at microsecond resolution, so a range
	// bound carrying nanoseconds cannot be answered exactly. It is normalized
	// once, here, and reported back on the result: otherwise the renderer
	// would clip with one value while the query that fetched the revisions
	// used another, and a revision overlapping the range by less than a
	// microsecond would be fetched or dropped depending on which end it fell.
	from := scheduleconfig.NormalizeTimestamp(in.From)
	until := scheduleconfig.NormalizeTimestamp(in.Until)
	if !until.After(from) {
		return Result{}, fmt.Errorf("schedulerender: range %v..%v is empty or inverted at %v resolution",
			in.From, in.Until, scheduleconfig.TimestampResolution)
	}

	res := Result{
		From:                from,
		Until:               until,
		HistoryCompleteFrom: in.Root.HistoryCompleteFrom,
		DeletedAt:           in.Root.DeletedAt,
		HistoryComplete:     true,
	}
	queryRange := interval{Start: from, End: until}

	revisions := append([]scheduleconfig.ScheduleRevision(nil), in.Revisions...)
	sort.SliceStable(revisions, func(i, j int) bool {
		if !revisions[i].EffectiveFrom.Equal(revisions[j].EffectiveFrom) {
			return revisions[i].EffectiveFrom.Before(revisions[j].EffectiveFrom)
		}
		// Two revisions starting at the same instant is corruption, but the
		// order still has to be decided by the data rather than by however the
		// query returned them.
		return revisions[i].Version < revisions[j].Version
	})

	// History before the first recorded revision is not a gap in the chain,
	// it is the part of the past this schedule cannot speak about. Saying
	// "gap" there would cry damage on every schedule younger than the query.
	if hcf := in.Root.HistoryCompleteFrom; hcf == nil || from.Before(*hcf) {
		res.HistoryComplete = false
		incompleteUntil := until
		if hcf != nil {
			incompleteUntil = minTime(until, *hcf)
		}
		res.Warnings = append(res.Warnings, Warning{
			Code:  WarnHistoryIncomplete,
			From:  from,
			Until: incompleteUntil,
		})
	}

	// The coverage cursor is what makes the three states distinguishable.
	// Comparing only neighbouring pairs finds a revision lost in the middle
	// but not one lost at either end, nor an empty chain, and those cases
	// would come back as a silent empty stretch with HistoryComplete=true.
	//
	// It starts at the point from which this schedule's history is supposed
	// to be exact. When that point is unknown, leading and trailing coverage
	// cannot be asserted at all, so the cursor only starts tracking once the
	// first revision has been seen and only inner gaps are reported.
	cov := coverage{query: queryRange}
	if hcf := in.Root.HistoryCompleteFrom; hcf != nil {
		cov.start(maxTime(from, *hcf))
	}

	for _, rev := range revisions {
		revRange := revisionInterval(rev)
		revRange, res.Warnings = cov.advance(revRange, rev, res.Warnings)

		bound := revRange.intersect(queryRange)
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
	res.Warnings = cov.finish(in.Root.HistoryCompleteFrom != nil, res.Warnings)

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

// coverage walks the revision chain and reports where it fails to tile the
// part of the query range that is supposed to be covered.
//
// It exists because pairwise comparison is not enough. A revision lost in the
// middle shows up as a hole between two neighbours, but a lost first or last
// revision, or an entirely empty chain, leaves no pair to compare - and those
// come back as a silent empty stretch that the caller cannot tell from "the
// schedule genuinely had nobody on duty".
type coverage struct {
	query    interval
	cursor   time.Time
	tracking bool

	// lastID names the revision the cursor currently ends at, so a gap can
	// say what it sits between.
	lastID string
}

func (c *coverage) start(at time.Time) {
	c.cursor, c.tracking = at, true
}

// advance folds one revision into the cursor and returns its effective range,
// clipped if it overlapped what was already covered.
//
// An overlap is corruption - the exclusion constraint forbids it - but the
// renderer has to stay total, and two parallel assignments on one layer would
// break the assumption every consumer makes. The EARLIER revision keeps the
// disputed interval: an answer already given about the past must not change
// because a later row has a wrong boundary.
func (c *coverage) advance(revRange interval, rev scheduleconfig.ScheduleRevision, warnings []Warning) (interval, []Warning) {
	if c.tracking {
		switch {
		case revRange.Start.After(c.cursor):
			warnings = c.appendGap(warnings, c.cursor, revRange.Start, rev.ID)
		case revRange.Start.Before(c.cursor):
			if overlap := (interval{Start: revRange.Start, End: minTime(c.cursor, revRange.End)}).
				intersect(c.query); !overlap.empty() {
				warnings = append(warnings, Warning{
					Code:       WarnRevisionOverlap,
					From:       overlap.Start,
					Until:      overlap.End,
					RelatedIDs: relatedIDs(c.lastID, rev.ID),
				})
			}
			revRange.Start = maxTime(revRange.Start, c.cursor)
		}
	}

	if !c.tracking || revRange.End.After(c.cursor) {
		c.cursor = revRange.End
	}
	c.tracking = true
	c.lastID = rev.ID
	return revRange, warnings
}

// finish reports the stretch after the last revision. It only fires when the
// schedule knows from when its history is exact: without that, the absence of
// a tail revision cannot be distinguished from history that never started.
func (c *coverage) finish(historyCompleteKnown bool, warnings []Warning) []Warning {
	if !c.tracking || !historyCompleteKnown {
		return warnings
	}
	return c.appendGap(warnings, c.cursor, c.query.End, "")
}

func (c *coverage) appendGap(warnings []Warning, from, until time.Time, nextID string) []Warning {
	gap := interval{Start: from, End: until}.intersect(c.query)
	if gap.empty() {
		return warnings
	}
	return append(warnings, Warning{
		Code:       WarnRevisionGap,
		From:       gap.Start,
		Until:      gap.End,
		RelatedIDs: relatedIDs(c.lastID, nextID),
	})
}

// relatedIDs names the revisions on either side of a break, dropping the ends
// that do not exist: a gap before the first revision has nothing on its left.
func relatedIDs(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}
