package schedulerender

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkRender measures the cost of rendering a range against an anchor
// five years old, which is where the anchor of a schedule nobody has edited
// ends up.
//
// The shape of the result is the point. PositionAt is called once per
// (revision, layer) and every following slot advances the position by one, so
// the anchor walk is paid once and the range length only adds cheap slot
// steps. Calling PositionAt per slot - the literal reading of the rendering
// algorithm - would multiply the two instead, and a 90-day daily range would
// cost ninety anchor walks rather than one.
func BenchmarkRender(b *testing.B) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(-5, 0, 0)

	revs := chain(b, revisionStep{
		at:  created,
		cfg: config("Europe/Berlin", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob")),
	})
	rootValue := root(created)

	for _, days := range []int{7, 30, 90} {
		b.Run(fmt.Sprintf("daily/%dd", days), func(b *testing.B) {
			in := Input{
				Root:      rootValue,
				Revisions: revs,
				From:      now,
				Until:     now.AddDate(0, 0, days),
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Render(in); err != nil {
					b.Fatalf("Render: %v", err)
				}
			}
		})
	}
}

// BenchmarkCurrentOnCallProjection is the hot path: one revision, one instant.
// It is dominated by the same anchor walk, and nothing in the range length
// enters into it.
func BenchmarkCurrentOnCallProjection(b *testing.B) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(-5, 0, 0)

	revs := chain(b, revisionStep{
		at:  created,
		cfg: config("Europe/Berlin", dailyPolicy("11:00"), group(groupA, "alice"), group(groupB, "bob")),
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		slots, err := onCallSlots(revs[0], now)
		if err != nil {
			b.Fatalf("onCallSlots: %v", err)
		}
		projectOnCall(revs[0], now, slots, nil)
	}
}
