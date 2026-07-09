package outbox

import (
	"testing"
	"time"
)

func TestComputeBackoff_Bounds(t *testing.T) {
	// Expected raw values: 5s * 2^attempt, capped at 10m
	expectedRaw := []time.Duration{
		5 * time.Second,   // attempt 0
		10 * time.Second,  // attempt 1
		20 * time.Second,  // attempt 2
		40 * time.Second,  // attempt 3
		80 * time.Second,  // attempt 4
		160 * time.Second, // attempt 5
		320 * time.Second, // attempt 6
		600 * time.Second, // attempt 7 (capped at 10m)
		600 * time.Second, // attempt 8 (capped)
	}

	for attempt := 0; attempt <= 8; attempt++ {
		raw := expectedRaw[attempt]
		lo := time.Duration(float64(raw) * 0.8)
		hi := time.Duration(float64(raw) * 1.2)

		// Run multiple times to test jitter distribution
		for i := 0; i < 50; i++ {
			got := computeBackoff(attempt)
			if got < lo || got > hi {
				t.Errorf("attempt=%d: computeBackoff()=%v, want [%v, %v]", attempt, got, lo, hi)
			}
		}
	}
}

func TestComputeBackoff_CappedAt10Min(t *testing.T) {
	// Very high attempt should still be capped
	maxAllowed := time.Duration(float64(10*time.Minute) * 1.2)
	for i := 0; i < 20; i++ {
		got := computeBackoff(100)
		if got > maxAllowed {
			t.Errorf("attempt=100: computeBackoff()=%v exceeds cap+jitter %v", got, maxAllowed)
		}
	}
}
