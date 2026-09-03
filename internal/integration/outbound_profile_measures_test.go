//go:build integration

package integration

import (
	"testing"
	"time"
)

// The two measures the profile tests stand on, checked on synthetic samples:
// a wrong measure would be green about the wrong thing for eight minutes at a
// time, which is the most expensive kind of wrong.

func at(base time.Time, seconds float64) time.Time {
	return base.Add(time.Duration(seconds * float64(time.Second)))
}

// TestLongestOverIsOneContiguousStretch: a rule's `for` clause is satisfied by
// one continuous excursion, so two excursions with a dip between them are two
// short ones however long they add up to - and a measure that summed them
// would call a harmless burst an outage.
func TestLongestOverIsOneContiguousStretch(t *testing.T) {
	base := time.Now()
	samples := []webhookSample{
		{at: at(base, 0), late: 10, reported: true},
		{at: at(base, 100), late: 310, reported: true}, // over: 100..300 = 200s
		{at: at(base, 200), late: 350, reported: true},
		{at: at(base, 300), late: 290, reported: true}, // dip
		{at: at(base, 350), late: 305, reported: true}, // over: 350..500 = 150s
		{at: at(base, 500), late: 100, reported: true},
		{at: at(base, 600), late: 400, reported: true}, // over to the end: 600..640 = 40s
		{at: at(base, 640), late: 400, reported: true},
	}
	if got := longestOver(samples, 300); got != 200*time.Second {
		t.Fatalf("the longest stretch over 300 is %s, want 200s (the sum would be 390s)", got)
	}
	if got := longestOver(samples, 500); got != 0 {
		t.Fatalf("nothing was over 500 and the measure says %s", got)
	}
}

// TestLongestOverIgnoresScrapesThatReportedNothing: a scrape that did not
// carry the series is not a reading of zero, and must neither end an
// excursion nor start one.
func TestLongestOverIgnoresScrapesThatReportedNothing(t *testing.T) {
	base := time.Now()
	samples := []webhookSample{
		{at: at(base, 0), late: 310, reported: true},
		{at: at(base, 50), late: 0, reported: false},
		{at: at(base, 100), late: 310, reported: true},
		{at: at(base, 150), late: 0, reported: true},
	}
	if got := longestOver(samples, 300); got != 150*time.Second {
		t.Fatalf("an unreported scrape broke the stretch: %s, want 150s", got)
	}
}

// TestOccupancyIntegratesWhatWasHeldOverTheWindow: the pool's occupancy is
// slot-seconds held against slot-seconds available, over the window and
// nothing outside it. A pool that held eight for one sample and idled for
// the rest has a maximum of eight and an occupancy near zero, and the second
// is the number the isolation claim stands on.
func TestOccupancyIntegratesWhatWasHeldOverTheWindow(t *testing.T) {
	base := time.Now()
	samples := []webhookSample{
		{at: at(base, -5), inFlight: 8}, // before the window: ignored
		{at: at(base, 0), inFlight: 8},
		{at: at(base, 10), inFlight: 8},  // 8 * 10 = 80
		{at: at(base, 20), inFlight: 0},  // 8 * 10 = 80
		{at: at(base, 30), inFlight: 0},  // 0
		{at: at(base, 40), inFlight: 4},  // 0
		{at: at(base, 50), inFlight: 4},  // 4 * 10 = 40
		{at: at(base, 60), inFlight: 4},  // 4 * 10 = 40
		{at: at(base, 70), inFlight: 8},  // 4 * 10 = 40
		{at: at(base, 100), inFlight: 8}, // 8 * 30 = 240
		{at: at(base, 105), inFlight: 8}, // after the window: ignored
	}
	held, available, maxInFlight, counted := occupancy(samples, at(base, 0), at(base, 100), 8)
	if held != 520 {
		t.Errorf("held = %.0f slot-seconds, want 520", held)
	}
	if available != 800 {
		t.Errorf("available = %.0f slot-seconds, want 800", available)
	}
	if maxInFlight != 8 {
		t.Errorf("maximum in flight = %d, want 8", maxInFlight)
	}
	if counted != 9 {
		t.Errorf("%d samples counted inside the window, want 9", counted)
	}
	if share := held / available; share > 0.7 {
		t.Errorf("a pool that idled for half the window shows %.0f%% occupancy", share*100)
	}
}
