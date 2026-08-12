package schedulerender

import (
	"fmt"
	"sort"
	"strings"
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

	// No horizon at all is damage, not a schedule with little history. The
	// create flow writes it in the same statement as the row, and nothing
	// clears it afterwards, so an empty value means a row written past the
	// code - the same conclusion the projection reaches (onCallOfRoot).
	//
	// This is deliberately not folded into the check below. Rendering an empty
	// calendar with history_complete=false would answer a corrupt row exactly
	// as it answers a schedule created yesterday, and the caller cannot tell
	// those apart.
	if err := scheduleconfig.RequireInitializedRoot(&in.Root); err != nil {
		return Result{}, err
	}
	hcf := in.Root.HistoryCompleteFrom

	// History before the first recorded revision is not a gap in the chain,
	// it is the part of the past this schedule cannot speak about. Saying
	// "gap" there would cry damage on every schedule younger than the query.
	if from.Before(*hcf) {
		res.HistoryComplete = false
		res.Warnings = append(res.Warnings, Warning{
			Code:  WarnHistoryIncomplete,
			From:  from,
			Until: minTime(until, *hcf),
		})
	}

	// The coverage cursor is what makes the three states distinguishable.
	// Comparing only neighbouring pairs finds a revision lost in the middle
	// but not one lost at either end, nor an empty chain, and those cases
	// would come back as a silent empty stretch with HistoryComplete=true.
	//
	// It starts at the point from which this schedule's history is supposed to
	// be exact, which is always known here.
	cov := coverage{query: queryRange, cursor: maxTime(from, *hcf)}

	for _, rev := range revisions {
		revRange := revisionInterval(rev)
		revRange, advErr := cov.advance(revRange, rev)
		if advErr != nil {
			return Result{}, advErr
		}

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
			assignments, err := renderLayer(rev, layer, bound, in.Overrides)
			if err != nil {
				return Result{}, err
			}
			res.Assignments = append(res.Assignments, assignments...)
		}
	}
	if err := cov.finish(); err != nil {
		return Result{}, err
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
	overrides []scheduleconfig.OverrideRevision) ([]Assignment, error) {

	state, err := resolveLayer(rev, layer)
	if err != nil {
		return nil, err
	}

	layerOverrides := overridesOfLayer(overrides, layer)
	if !state.active && len(layerOverrides) == 0 {
		return nil, nil
	}

	slots := state.grid.SlotsOverlapping(bound.Start, bound.End)
	if len(slots) == 0 {
		return nil, nil
	}

	// PositionAt walks the grid from the phase anchor, which can be years
	// back. It is called ONCE, for the first slot; every following slot is
	// the next one along the grid, so the position simply advances by one.
	// Calling it per slot would multiply a cost that already grows with the
	// age of the anchor.
	position := 0
	if state.active {
		position, _, err = rotation.PositionAt(state.grid, state.snapshot, slots[0].Start)
		if err != nil {
			return nil, layerError(rev, layer, err)
		}
	}

	var out []Assignment
	for _, slot := range slots {
		in := slotInput{
			RevisionID: rev.ID,
			Layer:      layer,
			Slot:       slot,
			Bound:      interval{Start: slot.Start, End: slot.End}.intersect(bound),
			Overrides:  layerOverrides,
		}
		if state.active {
			in.Base = state.groupAt(position)
			position = state.nextPosition(position)
		}

		slotAssignments, err := renderSlot(in)
		if err != nil {
			return nil, err
		}
		out = append(out, slotAssignments...)
	}
	return out, nil
}

// coverage walks the revision chain and reports where it fails to tile the
// part of the query range that is supposed to be covered.
//
// It exists because pairwise comparison is not enough. A revision lost in the
// middle shows up as a hole between two neighbours, but a lost first or last
// revision, or an entirely empty chain, leaves no pair to compare - and those
// come back as a silent empty stretch that the caller cannot tell from "the
// schedule genuinely had nobody on duty".
//
// The cursor is always started: every schedule knows the instant from which its
// history is exact, because a row without one is refused before the render
// begins. There used to be an untracked mode for pre-revision rows, and it is
// gone with them - leading and trailing coverage are now always assertable.
type coverage struct {
	query  interval
	cursor time.Time

	// lastID names the revision the cursor currently ends at, so a gap can
	// say what it sits between.
	lastID string
}

// advance folds one revision into the cursor and returns its effective range,
// clipped if it overlapped what was already covered.
//
// An overlap is corruption - the exclusion constraint forbids it - but the
// renderer has to stay total, and two parallel assignments on one layer would
// break the assumption every consumer makes. The EARLIER revision keeps the
// disputed interval: an answer already given about the past must not change
// because a later row has a wrong boundary.
func (c *coverage) advance(revRange interval, rev scheduleconfig.ScheduleRevision) (interval, error) {
	switch {
	case revRange.Start.After(c.cursor):
		if err := c.gap(c.cursor, revRange.Start, rev.ID); err != nil {
			return interval{}, err
		}
	case revRange.Start.Before(c.cursor):
		if overlap := (interval{Start: revRange.Start, End: minTime(c.cursor, revRange.End)}).
			intersect(c.query); !overlap.empty() {
			return interval{}, fmt.Errorf("%w: schedule %s, revisions %s over %s..%s",
				scheduleconfig.ErrRevisionOverlap, rev.ScheduleID,
				strings.Join(relatedIDs(c.lastID, rev.ID), " and "),
				overlap.Start.Format(time.RFC3339), overlap.End.Format(time.RFC3339))
		}
		revRange.Start = maxTime(revRange.Start, c.cursor)
	}

	// The cursor and the revision it ends at move together. A revision wholly
	// inside what is already covered - corruption, but corruption the renderer
	// has to survive - advances neither: it is not what the coverage now ends
	// at, so naming it as one side of the next gap would point at a row that
	// does not touch that gap. The assignments would still be right and only
	// the diagnosis wrong, which is the kind of error nobody catches while
	// reading a warning about damaged data.
	if revRange.End.After(c.cursor) {
		c.cursor = revRange.End
		c.lastID = rev.ID
	}
	return revRange, nil
}

// finish refuses the stretch after the last revision, if there is one.
func (c *coverage) finish() error {
	return c.gap(c.cursor, c.query.End, "")
}

// gap refuses a stretch the chain fails to cover.
//
// It used to be a warning attached to a rendered calendar. A chain with a hole
// in it is damage the write paths cannot produce, and drawing the rest of the
// calendar around the hole answers a question about the past with something
// that merely looks like an answer.
func (c *coverage) gap(from, until time.Time, nextID string) error {
	gap := interval{Start: from, End: until}.intersect(c.query)
	if gap.empty() {
		return nil
	}
	return fmt.Errorf("%w: %s..%s, between revisions %s",
		ErrRevisionGap, gap.Start.Format(time.RFC3339), gap.End.Format(time.RFC3339),
		strings.Join(relatedIDs(c.lastID, nextID), " and "))
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
