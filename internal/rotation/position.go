package rotation

import (
	"fmt"
	"time"
)

// floorMod is the mathematical modulo: the result is always in [0, n) even
// for negative a. A carry anchor may lie AFTER the probed target (historical
// queries), so elapsed slot counts go negative; clamping negatives to zero,
// as the legacy scheduler did, silently picks the wrong group.
func floorMod(a, n int) int {
	r := a % n
	if r < 0 {
		r += n
	}
	return r
}

// PositionAt returns the group index serving the grid slot that contains at:
//
//	originSlot = grid.SlotContaining(PhaseAnchorSlotStart)
//	elapsed    = grid.SlotsBetween(originSlot.Start, targetSlot.Start)
//	position   = floorMod(StartPosition + elapsed, len(Groups))
//
// The layer must be active with a phase pair; use ActiveGroupAt for the
// ok-shaped variant that treats a disabled or empty layer as "no group".
func PositionAt(g Grid, layer RotationLayerSnapshot, at time.Time) (int, Slot, error) {
	if !layer.Enabled || len(layer.Groups) == 0 {
		return 0, Slot{}, fmt.Errorf("rotation: layer is disabled or has no groups")
	}
	if layer.PhaseAnchorSlotStart == nil || layer.StartPosition == nil {
		return 0, Slot{}, fmt.Errorf("rotation: active layer has no phase pair")
	}
	// A corrupted phase pair is a data error, never silently repaired: a
	// wrapped position or a rounded anchor would produce a plausible but
	// wrong on-call instead of surfacing the corruption.
	if *layer.StartPosition < 0 || *layer.StartPosition >= len(layer.Groups) {
		return 0, Slot{}, fmt.Errorf("rotation: start_position %d out of range [0..%d)",
			*layer.StartPosition, len(layer.Groups))
	}
	origin := g.SlotContaining(*layer.PhaseAnchorSlotStart)
	if !origin.Start.Equal(*layer.PhaseAnchorSlotStart) {
		return 0, Slot{}, fmt.Errorf("rotation: phase_anchor_slot_start %v is not a slot boundary of this grid",
			*layer.PhaseAnchorSlotStart)
	}
	target := g.SlotContaining(at)
	elapsed, err := g.SlotsBetween(origin.Start, target.Start)
	if err != nil {
		return 0, Slot{}, err
	}
	return floorMod(*layer.StartPosition+elapsed, len(layer.Groups)), target, nil
}

// ActiveGroupAt resolves the group on duty at the given instant. A disabled
// or empty layer yields ok=false and no error; errors are reserved for
// invalid input (broken phase pair, foreign slot starts).
func ActiveGroupAt(g Grid, layer RotationLayerSnapshot, at time.Time) (RotationGroup, Slot, bool, error) {
	if !layer.Enabled || len(layer.Groups) == 0 {
		return RotationGroup{}, Slot{}, false, nil
	}
	pos, slot, err := PositionAt(g, layer, at)
	if err != nil {
		return RotationGroup{}, Slot{}, false, err
	}
	src := layer.Groups[pos]
	return RotationGroup{ID: src.ID, Members: append([]string(nil), src.Members...)}, slot, true, nil
}
