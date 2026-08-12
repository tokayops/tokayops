package rotation

import (
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// PositionAt walks the grid from the phase anchor, so its cost grows with the
// age of that anchor: a schedule nobody has edited for years keeps the anchor
// it was created with. These benchmarks measure that growth, because the
// current on-call read sits on this path and is evaluated per dispatch and
// once a minute per schedule by the handoff notifier.
//
// The walk is what makes the answer correct across DST and skipped calendar
// dates (see SlotsBetween), so the question is not whether to walk but
// whether the walk is affordable at realistic anchor ages.
func BenchmarkPositionAt(b *testing.B) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)

	zones := []string{"UTC", "Europe/Berlin"}
	cadences := []struct {
		name   string
		policy RotationPolicy
	}{
		{"daily", RotationPolicy{Cadence: model.RotationDaily, HandoffTime: "11:00"}},
		{"weekly", weeklyBenchPolicy()},
	}
	ages := []struct {
		name  string
		years int
	}{{"1y", 1}, {"3y", 3}, {"5y", 5}}

	for _, zone := range zones {
		for _, cadence := range cadences {
			for _, age := range ages {
				b.Run(fmt.Sprintf("%s/%s/%s", zone, cadence.name, age.name), func(b *testing.B) {
					grid, err := NewGrid(zone, cadence.policy)
					if err != nil {
						b.Fatalf("NewGrid: %v", err)
					}
					anchor := grid.SlotContaining(now.AddDate(-age.years, 0, 0)).Start
					position := 0
					layer := RotationLayerSnapshot{
						Enabled: true,
						Policy:  cadence.policy,
						Groups: []RotationGroup{
							{ID: "11111111-1111-4111-8111-000000000001", Members: []string{"alice"}},
							{ID: "22222222-1111-4111-8111-000000000002", Members: []string{"bob"}},
						},
						PhaseAnchorSlotStart: &anchor,
						StartPosition:        &position,
					}

					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, _, err := PositionAt(grid, layer, now); err != nil {
							b.Fatalf("PositionAt: %v", err)
						}
					}
				})
			}
		}
	}
}

func weeklyBenchPolicy() RotationPolicy {
	monday := 1
	return RotationPolicy{
		Cadence:       model.RotationWeekly,
		HandoffTime:   "11:00",
		HandoffDay:    &monday,
	}
}
