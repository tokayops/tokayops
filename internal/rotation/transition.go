package rotation

import (
	"fmt"
	"reflect"
	"time"
)

// Phase actions: what happens to the layer's phase pair.
const (
	PhaseActionCarry    = "carry"
	PhaseActionReanchor = "reanchor"
	PhaseActionClear    = "clear"
)

// Group selections: which group serves the slot containing effectiveAt.
const (
	SelectionPreserve  = "preserve"
	SelectionSuccessor = "successor"
	SelectionFirst     = "first"
	SelectionNone      = "none"
)

// TransitionInput feeds the pure planner. Current is the effective revision
// SNAPSHOT (not the revision envelope: revision ID, version, effective
// interval and audit metadata play no role in rotation math); nil means the
// initial revision is being created.
type TransitionInput struct {
	Current     *ScheduleRevisionSnapshot
	Desired     ScheduleConfiguration
	EffectiveAt time.Time
}

// LayerTransition describes one layer's classified transition.
//
// It used to carry ExpectedActiveGroupID and PreservesActiveGroup as well, for
// a commit-time guard to check the planner against itself. The guard is gone
// now, and with it the only reader outside tests: both fields were
// still being computed on every save and looked at by nobody. What remains is
// what the audit trail records - the phase action and the group selection go
// into change_summary, where a person can ask later what a save decided.
type LayerTransition struct {
	PhaseAction    string
	GroupSelection string
}

// ChangeSummary is the diagnostic diff persisted next to a revision. It never
// participates in computation. timezone_changed is recorded separately:
// without it a tz-only reanchor is indistinguishable from a policy reanchor
// in the audit trail.
type ChangeSummary struct {
	L1GroupsChanged       bool   `json:"l1_groups_changed"`
	L1PolicyChanged       bool   `json:"l1_policy_changed"`
	L1PhaseAction         string `json:"l1_phase_action"`
	L1GroupSelection      string `json:"l1_group_selection"`
	L2Changed             bool   `json:"l2_changed"`
	L2PhaseAction         string `json:"l2_phase_action"`
	L2GroupSelection      string `json:"l2_group_selection"`
	TimezoneChanged       bool   `json:"timezone_changed"`
	SlackUsergroupChanged bool   `json:"slack_usergroup_changed"`
}

// TransitionPlan is the pure output of PlanTransition. When Noop is true no
// other field is meaningful and nothing must be written.
type TransitionPlan struct {
	Noop     bool
	Snapshot ScheduleRevisionSnapshot
	L1       LayerTransition
	L2       LayerTransition
	Change   ChangeSummary
}

// PlanTransition classifies the transition of each layer independently and
// assembles the new revision snapshot. It reads no database, no clock and no
// overrides (an active override never becomes a phase anchor).
//
// Normative classification order per layer (the order matters: an
// enabled-transition must be classified before the carry check, otherwise
// re-enabling a layer with unchanged policy/IDs would "carry" a nil phase
// pair into an active layer):
//
//  1. desired disabled or desired groups empty        -> clear + none
//  2. no current, current disabled or current empty   -> reanchor + first
//  3. both active, grid (policy+timezone) and ordered
//     group ID list unchanged                         -> carry + preserve
//  4. both active, anything of grid/IDs changed       -> reanchor +
//     preserve | successor | first
func PlanTransition(in TransitionInput) (TransitionPlan, error) {
	var zero TransitionPlan
	if in.EffectiveAt.IsZero() {
		return zero, fmt.Errorf("rotation: effective_at is required")
	}
	desired, err := NormalizeConfiguration(in.Desired)
	if err != nil {
		return zero, err
	}
	var currentCfg *ScheduleConfiguration
	if in.Current != nil {
		if err := in.Current.Validate(); err != nil {
			return zero, fmt.Errorf("rotation: current snapshot invalid: %w", err)
		}
		c := ConfigurationFromSnapshot(*in.Current)
		currentCfg = &c
		if ConfigEqual(c, desired) {
			return TransitionPlan{Noop: true}, nil
		}
	}

	var curL1, curL2 *RotationLayerSnapshot
	var curTZ string
	if in.Current != nil {
		curL1, curL2, curTZ = &in.Current.L1, &in.Current.L2, in.Current.Timezone
	}
	l1Snap, l1T, err := planLayer(curL1, curTZ, desired.L1, desired.Timezone, in.EffectiveAt)
	if err != nil {
		return zero, fmt.Errorf("l1: %w", err)
	}
	l2Snap, l2T, err := planLayer(curL2, curTZ, desired.L2, desired.Timezone, in.EffectiveAt)
	if err != nil {
		return zero, fmt.Errorf("l2: %w", err)
	}

	snap := ScheduleRevisionSnapshot{
		SchemaVersion:           SnapshotSchemaVersion,
		Timezone:                desired.Timezone,
		SlackUsergroupID:        desired.SlackUsergroupID,
		L1:                      l1Snap,
		L2:                      l2Snap,
		L2EscalationTimeoutMins: desired.L2EscalationTimeoutMins,
	}
	if err := snap.Validate(); err != nil {
		return zero, fmt.Errorf("rotation: planned snapshot invalid: %w", err)
	}
	return TransitionPlan{
		Snapshot: snap,
		L1:       l1T,
		L2:       l2T,
		Change:   summarize(currentCfg, desired, l1T, l2T),
	}, nil
}

// planLayer implements the normative classification order for one layer.
// current is nil when there is no current revision; currentTZ accompanies it.
func planLayer(current *RotationLayerSnapshot, currentTZ string, desired LayerConfiguration, desiredTZ string, effectiveAt time.Time) (RotationLayerSnapshot, LayerTransition, error) {
	newLayer := RotationLayerSnapshot{
		Enabled: desired.Enabled,
		Policy:  desired.Policy.clone(),
		Groups:  cloneGroups(desired.Groups),
	}

	// Step 1: the desired layer produces no assignments.
	if !desired.Enabled || len(desired.Groups) == 0 {
		return newLayer, LayerTransition{
			PhaseAction:    PhaseActionClear,
			GroupSelection: SelectionNone,
		}, nil
	}

	reanchor := func(selection string, position int) (RotationLayerSnapshot, LayerTransition, error) {
		ng, err := NewGrid(desiredTZ, desired.Policy)
		if err != nil {
			return RotationLayerSnapshot{}, LayerTransition{}, err
		}
		anchor := ng.SlotContaining(effectiveAt).Start
		newLayer.PhaseAnchorSlotStart = &anchor
		newLayer.StartPosition = &position
		return newLayer, LayerTransition{
			PhaseAction:    PhaseActionReanchor,
			GroupSelection: selection,
		}, nil
	}

	// Step 2: no current phase to continue from (initial revision, re-enable
	// after disable, or previously empty layer): start at position 0.
	if current == nil || !current.Enabled || len(current.Groups) == 0 {
		return reanchor(SelectionFirst, 0)
	}

	sameGrid := currentTZ == desiredTZ && equalPolicy(current.Policy, desired.Policy)
	sameIDs := equalOrderedGroupIDs(current.Groups, desired.Groups)

	og, err := NewGrid(currentTZ, current.Policy)
	if err != nil {
		return RotationLayerSnapshot{}, LayerTransition{}, err
	}
	oldPos, _, err := PositionAt(og, *current, effectiveAt)
	if err != nil {
		return RotationLayerSnapshot{}, LayerTransition{}, err
	}
	oldActiveID := current.Groups[oldPos].ID

	// Step 3: carry. The phase pair is copied by value, byte-identical after
	// encoding: no recomputation. Membership INSIDE groups may differ (the
	// original bug scenario [B] -> [B,D]); only the ordered ID list matters.
	if sameGrid && sameIDs {
		anchor := *current.PhaseAnchorSlotStart
		position := *current.StartPosition
		newLayer.PhaseAnchorSlotStart = &anchor
		newLayer.StartPosition = &position
		return newLayer, LayerTransition{
			PhaseAction:    PhaseActionCarry,
			GroupSelection: SelectionPreserve,
		}, nil
	}

	// Step 4: reanchor. The base active group is computed with the OLD grid
	// and OLD snapshot at effectiveAt, then group selection maps it onto the
	// desired groups.
	if idx := groupIndexByID(desired.Groups, oldActiveID); idx >= 0 {
		return reanchor(SelectionPreserve, idx)
	}
	for i := 1; i < len(current.Groups); i++ {
		candidate := current.Groups[(oldPos+i)%len(current.Groups)].ID
		if idx := groupIndexByID(desired.Groups, candidate); idx >= 0 {
			return reanchor(SelectionSuccessor, idx)
		}
	}
	return reanchor(SelectionFirst, 0)
}

func equalOrderedGroupIDs(a, b []RotationGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func groupIndexByID(gs []RotationGroup, id string) int {
	for i, g := range gs {
		if g.ID == id {
			return i
		}
	}
	return -1
}

// summarize builds the diagnostic diff. For the initial revision every
// "changed" flag is true: there is nothing to diff against.
func summarize(current *ScheduleConfiguration, desired ScheduleConfiguration, l1T, l2T LayerTransition) ChangeSummary {
	ch := ChangeSummary{
		L1PhaseAction:    l1T.PhaseAction,
		L1GroupSelection: l1T.GroupSelection,
		L2PhaseAction:    l2T.PhaseAction,
		L2GroupSelection: l2T.GroupSelection,
	}
	if current == nil {
		ch.L1GroupsChanged = true
		ch.L1PolicyChanged = true
		ch.L2Changed = true
		ch.TimezoneChanged = true
		ch.SlackUsergroupChanged = true
		return ch
	}
	cur := canonicalizeConfiguration(*current)
	des := canonicalizeConfiguration(desired)
	ch.L1GroupsChanged = !reflect.DeepEqual(cur.L1.Groups, des.L1.Groups)
	ch.L1PolicyChanged = !equalPolicy(cur.L1.Policy, des.L1.Policy) || cur.L1.Enabled != des.L1.Enabled
	ch.L2Changed = !reflect.DeepEqual(cur.L2, des.L2) ||
		cur.L2EscalationTimeoutMins != des.L2EscalationTimeoutMins
	ch.TimezoneChanged = cur.Timezone != des.Timezone
	ch.SlackUsergroupChanged = cur.SlackUsergroupID != des.SlackUsergroupID
	return ch
}
