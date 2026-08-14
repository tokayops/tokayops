package scheduleconfig

import (
	"testing"
	"time"
)

func TestNextEffectiveAt(t *testing.T) {
	base := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	tail := base.Add(1234 * time.Microsecond)

	tests := []struct {
		name string
		tail *time.Time
		now  time.Time
		want time.Time
	}{
		{
			name: "no tail: now truncated to database resolution",
			tail: nil,
			now:  base.Add(700 * time.Nanosecond),
			want: base,
		},
		{
			name: "now well after tail: now wins",
			tail: &tail,
			now:  tail.Add(5 * time.Second),
			want: tail.Add(5 * time.Second),
		},
		{
			name: "same microsecond: advance one unit",
			tail: &tail,
			now:  tail,
			want: tail.Add(TimestampResolution),
		},
		{
			// The whole point of normalizing: after the round-trip both
			// values are the same stored timestamp, so "later" is a lie.
			name: "sub-resolution difference counts as the same instant",
			tail: &tail,
			now:  tail.Add(500 * time.Nanosecond),
			want: tail.Add(TimestampResolution),
		},
		{
			name: "clock stepped backwards: still advances",
			tail: &tail,
			now:  tail.Add(-2 * time.Hour),
			want: tail.Add(TimestampResolution),
		},
		{
			name: "exactly one unit later is already monotonic",
			tail: &tail,
			now:  tail.Add(TimestampResolution),
			want: tail.Add(TimestampResolution),
		},
		{
			name: "unnormalized tail does not leak sub-resolution digits",
			tail: ptrTime(tail.Add(400 * time.Nanosecond)),
			now:  tail,
			want: tail.Add(TimestampResolution),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NextEffectiveAt(tc.tail, tc.now)
			if !got.Equal(tc.want) {
				t.Fatalf("NextEffectiveAt = %v, want %v", got, tc.want)
			}
			if got.Truncate(TimestampResolution) != got {
				t.Fatalf("result %v carries sub-resolution precision", got)
			}
			if tc.tail != nil && !got.After(NormalizeTimestamp(*tc.tail)) {
				t.Fatalf("result %v does not strictly follow tail %v", got, *tc.tail)
			}
		})
	}
}

func TestNextEffectiveAtDoesNotMutateTail(t *testing.T) {
	tail := time.Date(2026, 3, 14, 9, 0, 0, 500, time.UTC)
	before := tail
	NextEffectiveAt(&tail, tail.Add(time.Hour))
	if !tail.Equal(before) {
		t.Fatalf("tail mutated: %v -> %v", before, tail)
	}
}

func TestNormalizeTimestampIsIdempotent(t *testing.T) {
	raw := time.Date(2026, 3, 14, 9, 0, 0, 123456789, time.UTC)
	once := NormalizeTimestamp(raw)
	twice := NormalizeTimestamp(once)
	if !once.Equal(twice) {
		t.Fatalf("normalize is not idempotent: %v vs %v", once, twice)
	}
	if once.Nanosecond()%1000 != 0 {
		t.Fatalf("normalized value %v is not at microsecond resolution", once)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
