package schedulerender

import (
	"fmt"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// baseGroup is the rotation group serving a slot. It is absent when the layer
// carries no rotation.
type baseGroup struct {
	GroupID string
	UserIDs []string
}

// layerState is one layer of one revision resolved into what both readers
// need: the snapshot slice, the grid it defines, and whether a rotation is
// actually running on it.
//
// Both the historical renderer and the current on-call projection start from
// exactly this, and having them derive it separately is how they would come
// to disagree about which layer is active or which timezone its grid is in -
// the same reason the overlay itself lives in one place.
type layerState struct {
	layer    string
	snapshot rotation.RotationLayerSnapshot
	grid     rotation.Grid

	// active reports whether the layer has a rotation to serve slots with.
	// An inactive layer is NOT skipped: an override on a layer that was
	// switched off mid-shift still names the person on duty, and it still
	// reports the grid slot it fell in - which is why the grid is built even
	// here. A disabled layer always carries a valid policy, so it can be.
	active bool
}

func resolveLayer(rev scheduleconfig.ScheduleRevision, layer string) (layerState, error) {
	snapshot := rev.Snapshot.L1
	if layer == LayerL2 {
		snapshot = rev.Snapshot.L2
	}
	grid, err := rotation.NewGrid(rev.Snapshot.Timezone, snapshot.Policy)
	if err != nil {
		return layerState{}, layerError(rev, layer, err)
	}
	return layerState{
		layer:    layer,
		snapshot: snapshot,
		grid:     grid,
		active:   snapshot.Enabled && len(snapshot.Groups) > 0,
	}, nil
}

// groupAt returns the group at a rotation position.
//
// Members are BORROWED from the snapshot, not copied. The copy belongs at the
// one boundary where a slice actually escapes - renderSlot, building the
// assignment it returns - and doing it here as well would allocate once per
// slot to hand the copy straight to another copy. baseGroup never leaves this
// package and is only ever consumed by renderSlot.
func (s layerState) groupAt(position int) *baseGroup {
	group := s.snapshot.Groups[position]
	return &baseGroup{GroupID: group.ID, UserIDs: group.Members}
}

// nextPosition advances one slot along the grid. Consecutive slots differ by
// exactly one step, so the position never has to be recomputed from the phase
// anchor - which is the expensive part.
func (s layerState) nextPosition(position int) int {
	position++
	if position == len(s.snapshot.Groups) {
		return 0
	}
	return position
}

// layerError wraps a rotation-math failure in ErrRotation as well as the
// original: the runtime classifies by the sentinel, a person reads the rest.
func layerError(rev scheduleconfig.ScheduleRevision, layer string, err error) error {
	return fmt.Errorf("%w: revision %s layer %s: %w", ErrRotation, rev.ID, layer, err)
}

// overridesOfLayer keeps the overrides belonging to one layer. An override
// belongs to exactly one, and the renderer applies it to that one only.
func overridesOfLayer(overrides []scheduleconfig.OverrideRevision, layer string) []scheduleconfig.OverrideRevision {
	var out []scheduleconfig.OverrideRevision
	for _, o := range overrides {
		if o.Layer == layer {
			out = append(out, o)
		}
	}
	return out
}
