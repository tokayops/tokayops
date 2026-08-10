package rotation

import (
	"testing"
	"time"
)

func TestGrid_SlotContaining_HalfOpen(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	boundary := utc(2026, time.August, 4, 11, 0)

	s := g.SlotContaining(boundary)
	if !s.Start.Equal(boundary) {
		t.Fatalf("SlotContaining(boundary).Start = %v, want %v (slot STARTS at boundary)", s.Start, boundary)
	}
	if !s.End.Equal(utc(2026, time.August, 5, 11, 0)) {
		t.Fatalf("slot end = %v", s.End)
	}

	before := g.SlotContaining(boundary.Add(-time.Second))
	if !before.End.Equal(boundary) {
		t.Fatalf("slot before boundary must end at boundary, got %v", before.End)
	}
	if !before.Start.Equal(utc(2026, time.August, 3, 11, 0)) {
		t.Fatalf("slot before boundary starts at %v", before.Start)
	}
}

func TestGrid_NextBoundary_AtBoundary(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	boundary := utc(2026, time.August, 4, 11, 0)
	next := g.NextBoundary(boundary)
	if !next.Equal(utc(2026, time.August, 5, 11, 0)) {
		t.Fatalf("NextBoundary(atBoundary) = %v, want strictly next boundary", next)
	}
	if got := g.NextBoundary(boundary.Add(-time.Second)); !got.Equal(boundary) {
		t.Fatalf("NextBoundary just before boundary = %v, want %v", got, boundary)
	}
}

func TestGrid_Weekly_DayConvention(t *testing.T) {
	// HandoffDay 0 = Sunday, matching time.Weekday.
	g := mustGrid(t, "UTC", weeklyPolicy("11:00", 0))
	// 2026-08-05 is a Wednesday; previous Sunday is 2026-08-02.
	s := g.SlotContaining(utc(2026, time.August, 5, 15, 0))
	if !s.Start.Equal(utc(2026, time.August, 2, 11, 0)) {
		t.Fatalf("weekly Sunday slot start = %v, want Sun 2026-08-02 11:00", s.Start)
	}
	if !s.End.Equal(utc(2026, time.August, 9, 11, 0)) {
		t.Fatalf("weekly slot end = %v", s.End)
	}
	if s.Start.Weekday() != time.Sunday {
		t.Fatalf("weekly boundary weekday = %v, want Sunday", s.Start.Weekday())
	}
}

func TestGrid_SlotsBetween_Antisymmetric(t *testing.T) {
	for _, tc := range []struct {
		tz     string
		policy RotationPolicy
		a, b   time.Time
	}{
		{"UTC", dailyPolicy("11:00"), utc(2026, time.August, 3, 11, 0), utc(2026, time.August, 20, 11, 0)},
		// Across the Berlin spring-forward transition (2026-03-29).
		{"Europe/Berlin", dailyPolicy("09:00"), utc(2026, time.March, 25, 8, 0), utc(2026, time.April, 2, 7, 0)},
	} {
		g := mustGrid(t, tc.tz, tc.policy)
		ab, err := g.SlotsBetween(tc.a, tc.b)
		if err != nil {
			t.Fatalf("SlotsBetween(a,b): %v", err)
		}
		ba, err := g.SlotsBetween(tc.b, tc.a)
		if err != nil {
			t.Fatalf("SlotsBetween(b,a): %v", err)
		}
		if ab != -ba {
			t.Fatalf("%s: SlotsBetween not antisymmetric: %d vs %d", tc.tz, ab, ba)
		}
		if zero, _ := g.SlotsBetween(tc.a, tc.a); zero != 0 {
			t.Fatalf("SlotsBetween(a,a) = %d", zero)
		}
	}
}

func TestGrid_SlotsBetween_RejectsNonSlotStart(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	a := utc(2026, time.August, 3, 11, 0)
	if _, err := g.SlotsBetween(a.Add(time.Minute), a); err == nil {
		t.Fatalf("non-slot-start first arg must error")
	}
	if _, err := g.SlotsBetween(a, a.Add(-time.Second)); err == nil {
		t.Fatalf("non-slot-start second arg must error")
	}
}

func TestGrid_SlotsOverlapping_Range(t *testing.T) {
	g := mustGrid(t, "UTC", dailyPolicy("11:00"))
	from := utc(2026, time.August, 3, 12, 0)
	until := utc(2026, time.August, 6, 10, 0)
	slots := g.SlotsOverlapping(from, until)
	if len(slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(slots))
	}
	if !slots[0].Start.Equal(utc(2026, time.August, 3, 11, 0)) {
		t.Fatalf("first slot start %v", slots[0].Start)
	}
	for i := 1; i < len(slots); i++ {
		if !slots[i].Start.Equal(slots[i-1].End) {
			t.Fatalf("slots not contiguous at %d", i)
		}
	}
	if slots[2].End.Before(until) {
		t.Fatalf("last slot must cover until")
	}

	if got := g.SlotsOverlapping(until, from); got != nil {
		t.Fatalf("until <= from must yield nil, got %d slots", len(got))
	}
	if got := g.SlotsOverlapping(from, from); got != nil {
		t.Fatalf("empty range must yield nil")
	}
}

func TestGrid_DST_SpringForward(t *testing.T) {
	// Europe/Berlin 2026-03-29: 02:00 CET -> 03:00 CEST. The daily 09:00
	// slot crossing the transition is 23 hours long; that is correct.
	g := mustGrid(t, "Europe/Berlin", dailyPolicy("09:00"))
	s := g.SlotContaining(utc(2026, time.March, 28, 12, 0))
	if !s.Start.Equal(utc(2026, time.March, 28, 8, 0)) { // 09:00 CET
		t.Fatalf("slot start %v", s.Start)
	}
	if !s.End.Equal(utc(2026, time.March, 29, 7, 0)) { // 09:00 CEST
		t.Fatalf("slot end %v", s.End)
	}
	if d := s.End.Sub(s.Start); d != 23*time.Hour {
		t.Fatalf("spring-forward slot length = %v, want 23h", d)
	}
}

func TestGrid_DST_FallBack(t *testing.T) {
	// Europe/Berlin 2026-10-25: 03:00 CEST -> 02:00 CET; the slot is 25h.
	g := mustGrid(t, "Europe/Berlin", dailyPolicy("09:00"))
	s := g.SlotContaining(utc(2026, time.October, 24, 12, 0))
	if !s.Start.Equal(utc(2026, time.October, 24, 7, 0)) { // 09:00 CEST
		t.Fatalf("slot start %v", s.Start)
	}
	if !s.End.Equal(utc(2026, time.October, 25, 8, 0)) { // 09:00 CET
		t.Fatalf("slot end %v", s.End)
	}
	if d := s.End.Sub(s.Start); d != 25*time.Hour {
		t.Fatalf("fall-back slot length = %v, want 25h", d)
	}
}

func TestGrid_HandoffInDSTGap(t *testing.T) {
	// America/New_York 2026-03-08: 02:00 EST -> 03:00 EDT. Local 02:30 does
	// not exist. D9: shift forward by the gap size preserving minutes, so
	// the boundary is 03:30 EDT = 07:30 UTC.
	g := mustGrid(t, "America/New_York", dailyPolicy("02:30"))
	s := g.SlotContaining(utc(2026, time.March, 8, 12, 0))
	if !s.Start.Equal(utc(2026, time.March, 8, 7, 30)) {
		t.Fatalf("gap boundary = %v (UTC), want 2026-03-08T07:30:00Z (03:30 EDT)", s.Start)
	}
	loc, _ := time.LoadLocation("America/New_York")
	if lt := s.Start.In(loc); lt.Hour() != 3 || lt.Minute() != 30 {
		t.Fatalf("gap boundary local clock = %02d:%02d, want 03:30", lt.Hour(), lt.Minute())
	}
	// Neighboring boundaries stay at 02:30 local.
	prev := g.SlotContaining(s.Start.Add(-time.Second))
	if lt := prev.Start.In(loc); lt.Hour() != 2 || lt.Minute() != 30 {
		t.Fatalf("previous boundary local = %02d:%02d, want 02:30", lt.Hour(), lt.Minute())
	}
	if lt := s.End.In(loc); lt.Hour() != 2 || lt.Minute() != 30 {
		t.Fatalf("next boundary local = %02d:%02d, want 02:30", lt.Hour(), lt.Minute())
	}
}

func TestGrid_HandoffInDSTFold(t *testing.T) {
	// America/New_York 2026-11-01: 02:00 EDT -> 01:00 EST. Local 01:30
	// happens twice: 05:30 UTC (EDT) and 06:30 UTC (EST). D9: the first,
	// earlier UTC occurrence wins.
	g := mustGrid(t, "America/New_York", dailyPolicy("01:30"))
	s := g.SlotContaining(utc(2026, time.November, 1, 12, 0))
	if !s.Start.Equal(utc(2026, time.November, 1, 5, 30)) {
		t.Fatalf("fold boundary = %v (UTC), want 2026-11-01T05:30:00Z (first occurrence)", s.Start)
	}
	// The second occurrence (06:30 UTC) belongs to the SAME slot, not to a
	// new one: SlotContaining must not cut a boundary there.
	inside := g.SlotContaining(utc(2026, time.November, 1, 6, 30))
	if !inside.Start.Equal(s.Start) {
		t.Fatalf("second fold occurrence started a new slot at %v", inside.Start)
	}
}

func TestGrid_NonHourOffset(t *testing.T) {
	// Asia/Kathmandu is UTC+05:45, no DST: 11:00 local = 05:15 UTC.
	g := mustGrid(t, "Asia/Kathmandu", dailyPolicy("11:00"))
	s := g.SlotContaining(utc(2026, time.August, 4, 12, 0))
	if !s.Start.Equal(utc(2026, time.August, 4, 5, 15)) {
		t.Fatalf("Kathmandu slot start = %v, want 05:15 UTC", s.Start)
	}

	// Australia/Lord_Howe has a 30-minute DST shift: +10:30 -> +11:00 on
	// 2026-10-04 (02:00 -> 02:30). Local 02:15 falls into the 30-minute
	// gap; D9 shifts it to 02:45 (+11:00) = 2026-10-03T15:45:00Z.
	lh := mustGrid(t, "Australia/Lord_Howe", dailyPolicy("02:15"))
	s = lh.SlotContaining(utc(2026, time.October, 4, 0, 0))
	if !s.Start.Equal(utc(2026, time.October, 3, 15, 45)) {
		t.Fatalf("Lord Howe gap boundary = %v, want 2026-10-03T15:45:00Z", s.Start)
	}
	loc, _ := time.LoadLocation("Australia/Lord_Howe")
	if lt := s.Start.In(loc); lt.Hour() != 2 || lt.Minute() != 45 {
		t.Fatalf("Lord Howe boundary local = %02d:%02d, want 02:45", lt.Hour(), lt.Minute())
	}
}

// Pacific/Apia skipped the entire calendar date 2011-12-30 (jump across the
// date line). The boundary for the skipped date collapses into the next
// date's boundary; the grid must dedupe: no zero-length slots, and
// SlotsBetween must count DISTINCT boundaries consistently with
// NextBoundary. Ordinary DST tests cannot catch this.
func TestGrid_SkippedCalendarDay_Apia(t *testing.T) {
	g := mustGrid(t, "Pacific/Apia", dailyPolicy("08:00"))
	loc, err := time.LoadLocation("Pacific/Apia")
	if err != nil {
		t.Fatal(err)
	}
	x := time.Date(2011, time.December, 29, 8, 0, 0, 0, loc).UTC() // exists
	z := time.Date(2011, time.December, 31, 8, 0, 0, 0, loc).UTC() // exists; Dec 30 does not
	w := time.Date(2012, time.January, 1, 8, 0, 0, 0, loc).UTC()

	// One long slot spans the skipped date.
	s := g.SlotContaining(z.Add(-time.Hour))
	if !s.Start.Equal(x) || !s.End.Equal(z) {
		t.Fatalf("slot across skipped day = [%v, %v), want [%v, %v)", s.Start, s.End, x, z)
	}
	if !s.Start.Before(s.End) {
		t.Fatalf("zero-length slot: [%v, %v)", s.Start, s.End)
	}
	if nb := g.NextBoundary(x); !nb.Equal(z) {
		t.Fatalf("NextBoundary across skip = %v, want %v", nb, z)
	}

	// Distinct-boundary counting: x -> z is ONE slot, x -> w is two.
	if n, err := g.SlotsBetween(x, z); err != nil || n != 1 {
		t.Fatalf("SlotsBetween(x,z) = %d (%v), want 1", n, err)
	}
	if n, err := g.SlotsBetween(x, w); err != nil || n != 2 {
		t.Fatalf("SlotsBetween(x,w) = %d (%v), want 2", n, err)
	}

	// No zero-length or overlapping slots across the skip.
	slots := g.SlotsOverlapping(utc(2011, time.December, 27, 0, 0), utc(2012, time.January, 3, 0, 0))
	for i, sl := range slots {
		if !sl.Start.Before(sl.End) {
			t.Fatalf("zero-length slot %d: [%v, %v)", i, sl.Start, sl.End)
		}
		if i > 0 && !sl.Start.Equal(slots[i-1].End) {
			t.Fatalf("gap/overlap at slot %d", i)
		}
	}
}

// Asia/Dhaka started DST on 2009-06-19 at 23:00 (clocks jumped to June 20
// 00:00). A daily 23:30 handoff on June 19 is gap-shifted onto the NEXT
// calendar date (June 20 00:30 local) WITHOUT collapsing into June 20's own
// 23:30 boundary. The boundary walk must carry the source civil date: a
// walk restarted from the date re-derived off the instant (June 20) would
// skip the June 20 23:30 boundary and undercount SlotsBetween by one -
// shifting every later on-call position.
func TestGrid_GapShiftedToNextDate_Dhaka(t *testing.T) {
	g := mustGrid(t, "Asia/Dhaka", dailyPolicy("23:30"))
	loc, err := time.LoadLocation("Asia/Dhaka")
	if err != nil {
		t.Fatal(err)
	}
	// All three instants exist unambiguously in local time.
	b19 := time.Date(2009, time.June, 20, 0, 30, 0, 0, loc).UTC()  // shifted boundary of June 19
	b20 := time.Date(2009, time.June, 20, 23, 30, 0, 0, loc).UTC() // June 20's own boundary
	b21 := time.Date(2009, time.June, 21, 23, 30, 0, 0, loc).UTC()

	s := g.SlotContaining(utc(2009, time.June, 20, 10, 0))
	if !s.Start.Equal(b19) || !s.End.Equal(b20) {
		t.Fatalf("slot = [%v, %v), want [%v, %v)", s.Start, s.End, b19, b20)
	}
	if nb := g.NextBoundary(b19); !nb.Equal(b20) {
		t.Fatalf("NextBoundary(shifted) = %v, want %v", nb, b20)
	}
	// The regression case: walking FROM the gap-shifted boundary must not
	// skip the same-date boundary that follows it.
	if n, err := g.SlotsBetween(b19, b20); err != nil || n != 1 {
		t.Fatalf("SlotsBetween(b19,b20) = %d (%v), want 1", n, err)
	}
	if n, err := g.SlotsBetween(b19, b21); err != nil || n != 2 {
		t.Fatalf("SlotsBetween(b19,b21) = %d (%v), want 2", n, err)
	}
	if n, err := g.SlotsBetween(b21, b19); err != nil || n != -2 {
		t.Fatalf("SlotsBetween(b21,b19) = %d (%v), want -2", n, err)
	}
	// Tiling across the transition has no gaps and hits all three.
	slots := g.SlotsOverlapping(utc(2009, time.June, 18, 0, 0), utc(2009, time.June, 22, 0, 0))
	found := 0
	for i, sl := range slots {
		if !sl.Start.Before(sl.End) {
			t.Fatalf("zero-length slot %d", i)
		}
		if i > 0 && !sl.Start.Equal(slots[i-1].End) {
			t.Fatalf("gap at slot %d", i)
		}
		if sl.Start.Equal(b19) || sl.Start.Equal(b20) || sl.Start.Equal(b21) {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("tiling found %d of the 3 expected boundaries", found)
	}
}

func TestGrid_SlotsBetween_MultiYear(t *testing.T) {
	g := mustGrid(t, "Europe/Berlin", dailyPolicy("09:00"))
	a := g.SlotContaining(inZone(t, "Europe/Berlin", 2020, time.January, 1, 12, 0)).Start
	b := g.SlotContaining(inZone(t, "Europe/Berlin", 2026, time.August, 7, 12, 0)).Start
	n, err := g.SlotsBetween(a, b)
	if err != nil {
		t.Fatalf("SlotsBetween: %v", err)
	}
	// Calendar days between 2020-01-01 and 2026-08-07 (12 DST transitions
	// in between; duration division alone would be off).
	if n != 2410 {
		t.Fatalf("multi-year SlotsBetween = %d, want 2410", n)
	}
}
