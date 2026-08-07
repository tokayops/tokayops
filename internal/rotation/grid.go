package rotation

import (
	"fmt"
	"sort"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// Slot is one half-open handoff interval [Start, End) in UTC instants.
type Slot struct {
	Start time.Time
	End   time.Time
}

// Grid is the daily/weekly handoff grid of one layer in one timezone.
//
// Contract:
//   - slots are half-open [Start, End); SlotContaining(boundary) returns the
//     slot STARTING at that boundary;
//   - NextBoundary(at) returns the strictly next boundary, even when at is
//     itself a boundary;
//   - SlotsBetween is signed and antisymmetric; both arguments must be exact
//     slot starts;
//   - SlotsOverlapping covers [from, until); until <= from yields nil.
type Grid interface {
	SlotContaining(at time.Time) Slot
	SlotsBetween(fromSlotStart, toSlotStart time.Time) (int, error)
	SlotsOverlapping(from, until time.Time) []Slot
	NextBoundary(after time.Time) time.Time
}

// NewGrid builds a grid from the snapshot-level timezone and a layer policy.
// Timezone is deliberately not part of the policy (one timezone per revision).
func NewGrid(timezone string, policy RotationPolicy) (Grid, error) {
	loc, err := validateTimezone(timezone)
	if err != nil {
		return nil, err
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	hh, mm, _ := ParseHandoffTime(policy.HandoffTime)
	g := &grid{loc: loc, hh: hh, mm: mm, stepDays: 1}
	if policy.Cadence == model.RotationWeekly {
		g.stepDays = 7
		g.weekday = time.Weekday(*policy.HandoffDay)
	}
	return g, nil
}

type grid struct {
	loc      *time.Location
	hh, mm   int
	stepDays int          // 1 = daily, 7 = weekly
	weekday  time.Weekday // weekly only; 0 = Sunday
}

// civilDate is a pure calendar date; arithmetic on it never touches DST.
type civilDate struct {
	y int
	m time.Month
	d int
}

func (c civilDate) addDays(n int) civilDate {
	// UTC here is used purely as a calendar, never as a clock.
	t := time.Date(c.y, c.m, c.d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n)
	return civilDate{t.Year(), t.Month(), t.Day()}
}

// resolve maps the grid's local handoff time on a calendar date to a single
// UTC instant using fixed product DST semantics:
//   - nonexistent local time (spring-forward gap): shift forward by the gap
//     size, preserving minutes (02:30 with a one-hour gap becomes 03:30);
//   - ambiguous local time (fall-back fold): the first, earlier UTC
//     occurrence.
//
// All Grid methods go through this resolver. Raw time.Date on handoff times
// is forbidden here: its result for gap/fold local times is unspecified and
// varies across Go/tzdata versions and zones.
func (g *grid) resolve(c civilDate) time.Time {
	return resolveLocalBoundary(g.loc, c, g.hh, g.mm)
}

func resolveLocalBoundary(loc *time.Location, c civilDate, hh, mm int) time.Time {
	if occ := localOccurrences(loc, c, hh, mm); len(occ) > 0 {
		return occ[0]
	}
	// The local time does not exist: it falls into a spring-forward gap.
	gap := gapSize(loc, c, hh, mm)
	total := hh*60 + mm + int(gap/time.Minute)
	for total >= 24*60 {
		total -= 24 * 60
		c = c.addDays(1)
	}
	if occ := localOccurrences(loc, c, total/60, total%60); len(occ) > 0 {
		return occ[0]
	}
	// Unreachable with a correct gap size; keep a deterministic fallback
	// instead of a panic in domain code.
	return time.Date(c.y, c.m, c.d, total/60, total%60, 0, 0, loc).UTC()
}

// localOccurrences returns every UTC instant whose wall clock in loc reads
// exactly c hh:mm, sorted ascending: none for a gap, one normally, two for a
// fold. It probes the zone offsets around the target instead of trusting
// time.Date's unspecified gap/fold normalization.
func localOccurrences(loc *time.Location, c civilDate, hh, mm int) []time.Time {
	guess := time.Date(c.y, c.m, c.d, hh, mm, 0, 0, time.UTC)
	seen := make(map[int]struct{}, 3)
	var out []time.Time
	for _, dp := range [5]time.Duration{-30 * time.Hour, -12 * time.Hour, 0, 12 * time.Hour, 30 * time.Hour} {
		_, off := guess.Add(dp).In(loc).Zone()
		if _, ok := seen[off]; ok {
			continue
		}
		seen[off] = struct{}{}
		cand := guess.Add(-time.Duration(off) * time.Second)
		lc := cand.In(loc)
		if lc.Year() == c.y && lc.Month() == c.m && lc.Day() == c.d && lc.Hour() == hh && lc.Minute() == mm && lc.Second() == 0 {
			out = append(out, cand.UTC())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// gapSize returns the width of the offset jump of the transition nearest to
// the (nonexistent) local target time, found by binary search over UTC.
func gapSize(loc *time.Location, c civilDate, hh, mm int) time.Duration {
	guess := time.Date(c.y, c.m, c.d, hh, mm, 0, 0, time.UTC)
	lo, hi := guess.Add(-30*time.Hour), guess.Add(30*time.Hour)
	_, offLo := lo.In(loc).Zone()
	if _, offHi := hi.In(loc).Zone(); offHi == offLo {
		return 0
	}
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if _, off := mid.In(loc).Zone(); off == offLo {
			lo = mid
		} else {
			hi = mid
		}
	}
	_, offAfter := hi.In(loc).Zone()
	if d := time.Duration(offAfter-offLo) * time.Second; d > 0 {
		return d
	}
	return 0
}

// alignDate returns the grid calendar date at or before at: for daily the
// local date itself, for weekly the previous date with the handoff weekday.
func (g *grid) alignDate(at time.Time) civilDate {
	lt := at.In(g.loc)
	c := civilDate{lt.Year(), lt.Month(), lt.Day()}
	if g.stepDays == 7 {
		back := (int(lt.Weekday()) - int(g.weekday) + 7) % 7
		c = c.addDays(-back)
	}
	return c
}

func (g *grid) SlotContaining(at time.Time) Slot {
	c := g.alignDate(at)
	b := g.resolve(c)
	for b.After(at) {
		c = c.addDays(-g.stepDays)
		b = g.resolve(c)
	}
	for {
		nc := c.addDays(g.stepDays)
		nb := g.resolve(nc)
		if nb.After(at) {
			return Slot{Start: b, End: nb}
		}
		c, b = nc, nb
	}
}

func (g *grid) NextBoundary(after time.Time) time.Time {
	return g.SlotContaining(after).End
}

func (g *grid) SlotsBetween(fromSlotStart, toSlotStart time.Time) (int, error) {
	if s := g.SlotContaining(fromSlotStart); !s.Start.Equal(fromSlotStart) {
		return 0, fmt.Errorf("rotation: %v is not a slot start of this grid", fromSlotStart)
	}
	if s := g.SlotContaining(toSlotStart); !s.Start.Equal(toSlotStart) {
		return 0, fmt.Errorf("rotation: %v is not a slot start of this grid", toSlotStart)
	}
	from := g.alignDate(fromSlotStart)
	// Duration division is only ever an ESTIMATE here; the loop below
	// brackets to the exact calendar answer, so multi-year spans, DST and
	// historical timezone rule changes cannot skew the result.
	n := int(toSlotStart.Sub(fromSlotStart) / (time.Duration(g.stepDays) * 24 * time.Hour))
	for {
		b := g.resolve(from.addDays(n * g.stepDays))
		switch {
		case b.After(toSlotStart):
			n--
		case b.Before(toSlotStart):
			n++
		default:
			return n, nil
		}
	}
}

func (g *grid) SlotsOverlapping(from, until time.Time) []Slot {
	if !until.After(from) {
		return nil
	}
	cur := g.SlotContaining(from)
	c := g.alignDate(cur.Start)
	var out []Slot
	for cur.Start.Before(until) {
		out = append(out, cur)
		c = c.addDays(g.stepDays)
		cur = Slot{Start: cur.End, End: g.resolve(c.addDays(g.stepDays))}
	}
	return out
}
