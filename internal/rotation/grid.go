package rotation

import (
	"fmt"
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
	if occ, n := localOccurrences(loc, c, hh, mm); n > 0 {
		return occ[0]
	}
	// The local time does not exist: it falls into a spring-forward gap. A
	// gap can exceed a day when a zone skips an entire calendar date
	// (Pacific/Apia 2011-12-30); the shifted time then lands on the next
	// date and collapses with that date's own boundary - callers dedupe.
	gap := gapSize(loc, c, hh, mm)
	total := hh*60 + mm + int(gap/time.Minute)
	for total >= 24*60 {
		total -= 24 * 60
		c = c.addDays(1)
	}
	if occ, n := localOccurrences(loc, c, total/60, total%60); n > 0 {
		return occ[0]
	}
	// Unreachable with a correct gap size; keep a deterministic fallback
	// instead of a panic in domain code.
	return time.Date(c.y, c.m, c.d, total/60, total%60, 0, 0, loc).UTC()
}

// localOccurrences returns every UTC instant whose wall clock in loc reads
// exactly c hh:mm, ascending: none for a gap, one normally, two for a fold.
// It probes the zone offsets around the target instead of trusting
// time.Date's unspecified gap/fold normalization. Allocation-free: this is
// the hot inner call of boundary walking.
func localOccurrences(loc *time.Location, c civilDate, hh, mm int) (occ [2]time.Time, n int) {
	guess := time.Date(c.y, c.m, c.d, hh, mm, 0, 0, time.UTC)
	var seen [5]int
	ns := 0
	for _, dp := range [5]time.Duration{-30 * time.Hour, -12 * time.Hour, 0, 12 * time.Hour, 30 * time.Hour} {
		_, off := guess.Add(dp).In(loc).Zone()
		dup := false
		for i := 0; i < ns; i++ {
			if seen[i] == off {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		seen[ns] = off
		ns++
		cand := guess.Add(-time.Duration(off) * time.Second)
		lc := cand.In(loc)
		if lc.Year() == c.y && lc.Month() == c.m && lc.Day() == c.d && lc.Hour() == hh && lc.Minute() == mm && lc.Second() == 0 {
			if n < 2 {
				occ[n] = cand.UTC()
				n++
			}
		}
	}
	if n == 2 && occ[1].Before(occ[0]) {
		occ[0], occ[1] = occ[1], occ[0]
	}
	return occ, n
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

// boundaryCursor pairs a boundary instant with the SOURCE civil date it was
// resolved from. The source date is NOT recoverable from the instant: a DST
// gap can shift a boundary onto the NEXT calendar date without collapsing
// into that date's own boundary (Asia/Dhaka 2009-06-19 23:30 resolves to
// June 20 00:30 local, while June 20 has its own 23:30 boundary). Walking
// from a date re-derived off the instant would skip that boundary, so every
// walk carries the cursor instead.
type boundaryCursor struct {
	date    civilDate
	instant time.Time
}

// nextAfter returns the first grid boundary strictly after cur.instant,
// advancing calendar dates from cur.date. It skips dates whose boundary
// collapses into an earlier one: a timezone can skip an entire calendar
// date (Pacific/Apia 2011-12-30), making adjacent dates resolve to the same
// instant. Every method derives the boundary sequence through this dedupe,
// so the grid is the strictly increasing sequence of DISTINCT resolved
// instants.
func (g *grid) nextAfter(cur boundaryCursor) boundaryCursor {
	c := cur.date
	for {
		c = c.addDays(g.stepDays)
		if b := g.resolve(c); b.After(cur.instant) {
			return boundaryCursor{date: c, instant: b}
		}
	}
}

// slotContaining locates the slot around at and returns cursors for its
// start and end boundaries. The backward scan recovers the true source date
// of the start boundary (it steps back until the resolved boundary is <= at,
// which lands on the gap-shifted source date, not on the date the instant
// happens to fall on).
func (g *grid) slotContaining(at time.Time) (start, end boundaryCursor) {
	c := g.alignDate(at)
	b := g.resolve(c)
	for b.After(at) {
		c = c.addDays(-g.stepDays)
		b = g.resolve(c)
	}
	cur := boundaryCursor{date: c, instant: b}
	for {
		next := g.nextAfter(cur)
		if next.instant.After(at) {
			return cur, next
		}
		cur = next
	}
}

func (g *grid) SlotContaining(at time.Time) Slot {
	start, end := g.slotContaining(at)
	return Slot{Start: start.instant, End: end.instant}
}

func (g *grid) NextBoundary(after time.Time) time.Time {
	_, end := g.slotContaining(after)
	return end.instant
}

// SlotsBetween walks distinct boundaries from a to b, carrying the source
// civil date in the cursor the whole way. A calendar-step estimate is NOT
// usable here (skipped dates collapse boundaries and make the count
// ambiguous), and re-deriving the date from an instant is NOT usable either
// (gap-shifted boundaries live on a different date - see boundaryCursor).
// Consistency with NextBoundary holds by construction; the walk is O(days),
// fine for multi-year anchors (a few thousand cheap resolver calls).
func (g *grid) SlotsBetween(fromSlotStart, toSlotStart time.Time) (int, error) {
	fromCur, _ := g.slotContaining(fromSlotStart)
	if !fromCur.instant.Equal(fromSlotStart) {
		return 0, fmt.Errorf("rotation: %v is not a slot start of this grid", fromSlotStart)
	}
	toCur, _ := g.slotContaining(toSlotStart)
	if !toCur.instant.Equal(toSlotStart) {
		return 0, fmt.Errorf("rotation: %v is not a slot start of this grid", toSlotStart)
	}
	if fromSlotStart.Equal(toSlotStart) {
		return 0, nil
	}
	sign := 1
	cur, target := fromCur, toCur.instant
	if target.Before(cur.instant) {
		cur, target = toCur, fromCur.instant
		sign = -1
	}
	n := 0
	for cur.instant.Before(target) {
		cur = g.nextAfter(cur)
		n++
	}
	if !cur.instant.Equal(target) {
		// Defensive: both ends were validated as slot starts, so the walk
		// must land exactly on the target.
		return 0, fmt.Errorf("rotation: boundary walk overshot %v to %v", target, cur.instant)
	}
	return sign * n, nil
}

func (g *grid) SlotsOverlapping(from, until time.Time) []Slot {
	if !until.After(from) {
		return nil
	}
	start, end := g.slotContaining(from)
	var out []Slot
	for start.instant.Before(until) {
		out = append(out, Slot{Start: start.instant, End: end.instant})
		start, end = end, g.nextAfter(end)
	}
	return out
}
