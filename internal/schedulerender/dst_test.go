package schedulerender

import (
	"testing"
	"time"
)

// TestGridStaysContiguousAcrossDST checks the renderer's use of the grid, not
// the grid itself (that is rotation's own suite): the slots it walks must
// tile the range without a hole or an overlap even where a day is 23 or 25
// hours long.
func TestGridStaysContiguousAcrossDST(t *testing.T) {
	tests := []struct {
		name     string
		zone     string
		from     time.Time
		until    time.Time
		handoff  string
		wantSpan time.Duration // the one irregular slot
	}{
		{
			// Europe/Berlin springs forward on 2026-03-29 at 02:00 local.
			name: "spring forward",
			zone: "Europe/Berlin", handoff: "11:00",
			from:  time.Date(2026, 3, 27, 10, 0, 0, 0, time.UTC),
			until: time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC),
			// The day containing the transition is one hour shorter.
			wantSpan: 23 * time.Hour,
		},
		{
			// And falls back on 2026-10-25.
			name: "fall back",
			zone: "Europe/Berlin", handoff: "11:00",
			from:     time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC),
			until:    time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
			wantSpan: 25 * time.Hour,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			revs := chain(t, revisionStep{at: tc.from, cfg: config(tc.zone, dailyPolicy(tc.handoff), group(groupA, "alice"))})
			res := renderOf(t, Input{Root: root(tc.from), Revisions: revs, From: tc.from, Until: tc.until})

			l1 := assignmentsOf(res, LayerL1)
			if len(l1) < 3 {
				t.Fatalf("got %d assignments, want the range tiled by daily slots", len(l1))
			}

			var sawIrregular bool
			for i, a := range l1 {
				if i > 0 && !l1[i-1].AssignmentEnd.Equal(a.AssignmentStart) {
					t.Fatalf("assignments %d and %d are not contiguous: %v then %v",
						i-1, i, l1[i-1].AssignmentEnd, a.AssignmentStart)
				}
				if span := a.GridSlotEnd.Sub(a.GridSlotStart); span == tc.wantSpan {
					sawIrregular = true
				} else if span != 24*time.Hour {
					t.Fatalf("assignment %d spans %v, want 24h or the DST %v", i, span, tc.wantSpan)
				}
			}
			if !sawIrregular {
				t.Fatalf("no %v slot: the DST transition was not covered", tc.wantSpan)
			}
			if !l1[0].AssignmentStart.Equal(tc.from) || !l1[len(l1)-1].AssignmentEnd.Equal(tc.until) {
				t.Fatalf("range covered %v..%v, want %v..%v",
					l1[0].AssignmentStart, l1[len(l1)-1].AssignmentEnd, tc.from, tc.until)
			}
		})
	}
}
