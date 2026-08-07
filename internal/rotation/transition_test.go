package rotation

import (
	"testing"
	"time"
)

// effAt is the canonical edit instant of most transition tests:
// Tue 2026-08-04 15:00 UTC. With baseSnapshot (anchor Mon 03 Aug 11:00,
// position 0) the active L1 group is gid[1] (bob) in slot [04 11:00, 05 11:00).
var effAt = utc(2026, time.August, 4, 15, 0)

func mustPlan(t *testing.T, current *ScheduleRevisionSnapshot, desired ScheduleConfiguration, at time.Time) TransitionPlan {
	t.Helper()
	plan, err := PlanTransition(TransitionInput{Current: current, Desired: desired, EffectiveAt: at})
	if err != nil {
		t.Fatalf("PlanTransition: %v", err)
	}
	return plan
}

func assertLayer(t *testing.T, lt LayerTransition, action, selection string, expectedID string, preserves bool) {
	t.Helper()
	if lt.PhaseAction != action || lt.GroupSelection != selection {
		t.Fatalf("transition = %s+%s, want %s+%s", lt.PhaseAction, lt.GroupSelection, action, selection)
	}
	if expectedID == "" {
		if lt.ExpectedActiveGroupID != nil {
			t.Fatalf("expected active group = %v, want nil", *lt.ExpectedActiveGroupID)
		}
	} else if lt.ExpectedActiveGroupID == nil || *lt.ExpectedActiveGroupID != expectedID {
		t.Fatalf("expected active group = %v, want %s", lt.ExpectedActiveGroupID, expectedID)
	}
	if lt.PreservesActiveGroup != preserves {
		t.Fatalf("preserves = %v, want %v", lt.PreservesActiveGroup, preserves)
	}
}

// activeAt resolves the active L1 group ID of a snapshot at an instant.
func activeAt(t *testing.T, s ScheduleRevisionSnapshot, at time.Time) string {
	t.Helper()
	g, err := NewGrid(s.Timezone, s.L1.Policy)
	if err != nil {
		t.Fatal(err)
	}
	group, _, ok, err := ActiveGroupAt(g, s.L1, at)
	if err != nil || !ok {
		t.Fatalf("ActiveGroupAt: ok=%v err=%v", ok, err)
	}
	return group.ID
}

func TestPlanTransition_Noop(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	plan := mustPlan(t, &snap, desired, effAt)
	if !plan.Noop {
		t.Fatalf("identical resave must be a no-op")
	}

	// Permuted members are the same canonical configuration.
	snap2 := baseSnapshot()
	snap2.L1.Groups[1].Members = []string{"bob", "dave"}
	desired2 := ConfigurationFromSnapshot(snap2)
	desired2.L1.Groups[1].Members = []string{"dave", "bob"}
	if plan := mustPlan(t, &snap2, desired2, effAt); !plan.Noop {
		t.Fatalf("member permutation must be a no-op")
	}
}

// The original bug scenario: [A],[B],[C] -> [A],[B,D],[C] with B active.
func TestPlanTransition_ActiveMembershipChange(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Groups[1].Members = []string{"bob", "dave"}

	plan := mustPlan(t, &snap, desired, effAt)
	if plan.Noop {
		t.Fatalf("membership change is not a no-op")
	}
	assertLayer(t, plan.L1, PhaseActionCarry, SelectionPreserve, gid[1], true)

	// Carry copies the phase pair verbatim: no recomputation.
	if !plan.Snapshot.L1.PhaseAnchorSlotStart.Equal(*snap.L1.PhaseAnchorSlotStart) {
		t.Fatalf("carry changed the anchor: %v", plan.Snapshot.L1.PhaseAnchorSlotStart)
	}
	if *plan.Snapshot.L1.StartPosition != *snap.L1.StartPosition {
		t.Fatalf("carry changed the position: %d", *plan.Snapshot.L1.StartPosition)
	}

	// B (now B,D) stays on duty; next handoff goes to C.
	if id := activeAt(t, plan.Snapshot, effAt); id != gid[1] {
		t.Fatalf("active after save = %s, want %s", id, gid[1])
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 12, 0)); id != gid[2] {
		t.Fatalf("next handoff goes to %s, want %s (C)", id, gid[2])
	}

	if !plan.Change.L1GroupsChanged || plan.Change.L1PolicyChanged || plan.Change.TimezoneChanged {
		t.Fatalf("change summary wrong: %+v", plan.Change)
	}
	if plan.Change.L1PhaseAction != PhaseActionCarry || plan.Change.L1GroupSelection != SelectionPreserve {
		t.Fatalf("summary must record carry+preserve: %+v", plan.Change)
	}
}

func TestPlanTransition_AddGroup_EveryActivePosition(t *testing.T) {
	for p := 0; p < 3; p++ {
		snap := baseSnapshot()
		// Active index at effAt is (sp+1)%3; choose sp so the active is p.
		snap.L1.StartPosition = intp((p + 2) % 3)
		if got := activeAt(t, snap, effAt); got != gid[p] {
			t.Fatalf("setup broken: active %s, want %s", got, gid[p])
		}
		desired := ConfigurationFromSnapshot(snap)
		desired.L1.Groups = append(desired.L1.Groups, RotationGroup{ID: gid[3], Members: []string{"dana"}})

		plan := mustPlan(t, &snap, desired, effAt)
		assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[p], true)
		if *plan.Snapshot.L1.StartPosition != p {
			t.Fatalf("active position %d: new start_position = %d", p, *plan.Snapshot.L1.StartPosition)
		}
		if !plan.Snapshot.L1.PhaseAnchorSlotStart.Equal(utc(2026, time.August, 4, 11, 0)) {
			t.Fatalf("anchor = %v, want slot containing effectiveAt", plan.Snapshot.L1.PhaseAnchorSlotStart)
		}
		if id := activeAt(t, plan.Snapshot, effAt); id != gid[p] {
			t.Fatalf("adding a group moved the duty: %s", id)
		}
	}
}

func TestPlanTransition_Reorder(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Groups = []RotationGroup{
		{ID: gid[2], Members: []string{"carol"}},
		{ID: gid[1], Members: []string{"bob"}},
		{ID: gid[0], Members: []string{"alice"}},
	}
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[1], true)
	if id := activeAt(t, plan.Snapshot, effAt); id != gid[1] {
		t.Fatalf("active after reorder = %s, want bob", id)
	}
	// Next handoff follows the NEW order: after bob comes alice.
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 12, 0)); id != gid[0] {
		t.Fatalf("next after reorder = %s, want alice", id)
	}
}

func TestPlanTransition_RemoveActive_CyclicSuccessor(t *testing.T) {
	// Active bob removed; old cycle after B is C, which survives.
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Groups = []RotationGroup{
		{ID: gid[0], Members: []string{"alice"}},
		{ID: gid[2], Members: []string{"carol"}},
	}
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionSuccessor, gid[2], false)
	if id := activeAt(t, plan.Snapshot, effAt); id != gid[2] {
		t.Fatalf("successor = %s, want carol", id)
	}

	// Wrap-around: active carol (last), removed; successor cycles to alice.
	snap = baseSnapshot()
	snap.L1.StartPosition = intp(1) // active at effAt: (1+1)%3 = 2 = carol
	desired = ConfigurationFromSnapshot(snap)
	desired.L1.Groups = []RotationGroup{
		{ID: gid[0], Members: []string{"alice"}},
		{ID: gid[1], Members: []string{"bob"}},
	}
	plan = mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionSuccessor, gid[0], false)
}

func TestPlanTransition_FullReplacement_First(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Groups = []RotationGroup{
		{ID: gid[3], Members: []string{"dana"}},
		{ID: gid[4], Members: []string{"erik"}},
	}
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionFirst, gid[3], false)
	if *plan.Snapshot.L1.StartPosition != 0 {
		t.Fatalf("full replacement starts at position 0")
	}
}

// §9.7 examples, edit at 15:00 with bob active.
func TestPlanTransition_HandoffLater_11to18(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Policy = dailyPolicy("18:00")
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[1], true)
	// B serves until 18:00 TODAY.
	if !plan.Snapshot.L1.PhaseAnchorSlotStart.Equal(utc(2026, time.August, 3, 18, 0)) {
		t.Fatalf("anchor = %v, want slot [03 18:00, 04 18:00)", plan.Snapshot.L1.PhaseAnchorSlotStart)
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 4, 17, 59)); id != gid[1] {
		t.Fatalf("before 18:00 = %s, want bob", id)
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 4, 18, 0)); id != gid[2] {
		t.Fatalf("at 18:00 = %s, want carol", id)
	}
}

func TestPlanTransition_HandoffEarlier_11to14(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Policy = dailyPolicy("14:00")
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[1], true)
	// Edit at 15:00 > 14:00, so B serves until 14:00 TOMORROW.
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 13, 59)); id != gid[1] {
		t.Fatalf("before tomorrow 14:00 = %s, want bob", id)
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 14, 0)); id != gid[2] {
		t.Fatalf("at tomorrow 14:00 = %s, want carol", id)
	}
}

func TestPlanTransition_DailyToWeekly(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Policy = weeklyPolicy("11:00", 1) // Monday
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[1], true)
	// B serves until next Monday 11:00.
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 10, 10, 59)); id != gid[1] {
		t.Fatalf("until Monday = %s, want bob", id)
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 10, 11, 0)); id != gid[2] {
		t.Fatalf("Monday 11:00 = %s, want carol", id)
	}
}

func TestPlanTransition_WeeklyToDaily(t *testing.T) {
	snap := baseSnapshot()
	snap.L1.Policy = weeklyPolicy("11:00", 1)
	// Anchor Mon 03 Aug 11:00 is a weekly Monday boundary too; alice active.
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Policy = dailyPolicy("11:00")
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[0], true)
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 12, 0)); id != gid[1] {
		t.Fatalf("next daily handoff = %s, want bob", id)
	}
}

func TestPlanTransition_TimezoneChange(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.Timezone = "Europe/Moscow"
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionPreserve, gid[1], true)
	// 11:00 Moscow = 08:00 UTC; B serves until the next Moscow boundary.
	if !plan.Snapshot.L1.PhaseAnchorSlotStart.Equal(utc(2026, time.August, 4, 8, 0)) {
		t.Fatalf("anchor = %v, want 2026-08-04T08:00:00Z", plan.Snapshot.L1.PhaseAnchorSlotStart)
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 7, 59)); id != gid[1] {
		t.Fatalf("before Moscow boundary = %s, want bob", id)
	}
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 8, 0)); id != gid[2] {
		t.Fatalf("at Moscow boundary = %s, want carol", id)
	}
	if !plan.Change.TimezoneChanged || plan.Change.L1PolicyChanged || plan.Change.L1GroupsChanged {
		t.Fatalf("summary must show tz-only change: %+v", plan.Change)
	}
}

// Composite edit: timezone changed AND the active group removed. A naive
// one-dimensional guard would reject this valid edit; the two-dimensional
// model classifies it as reanchor+successor.
func TestPlanTransition_TimezoneChangedAndActiveRemoved(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.Timezone = "Europe/Moscow"
	desired.L1.Groups = []RotationGroup{
		{ID: gid[0], Members: []string{"alice"}},
		{ID: gid[2], Members: []string{"carol"}},
	}
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionSuccessor, gid[2], false)
}

func l2Enabled(users ...string) LayerConfiguration {
	return LayerConfiguration{
		Enabled: true,
		Policy:  weeklyPolicy("11:00", 1),
		Groups:  L2GroupsFromUserIDs(users),
	}
}

func TestPlanTransition_L2DisableReenable(t *testing.T) {
	// Start with an active L2.
	snap := baseSnapshot()
	snap.L2 = RotationLayerSnapshot{
		Enabled:              true,
		Policy:               weeklyPolicy("11:00", 1),
		Groups:               L2GroupsFromUserIDs([]string{"xavier", "yulia"}),
		PhaseAnchorSlotStart: timep(utc(2026, time.August, 3, 11, 0)),
		StartPosition:        intp(0),
	}

	// Disable: assignments stop immediately, phase pair cleared.
	desired := ConfigurationFromSnapshot(snap)
	desired.L2.Enabled = false
	plan := mustPlan(t, &snap, desired, effAt)
	assertLayer(t, plan.L2, PhaseActionClear, SelectionNone, "", false)
	if plan.Snapshot.L2.PhaseAnchorSlotStart != nil || plan.Snapshot.L2.StartPosition != nil {
		t.Fatalf("disabled layer must have a nil phase pair")
	}

	// Re-enable: no phase to continue, start from position 0.
	disabled := plan.Snapshot
	desired2 := ConfigurationFromSnapshot(disabled)
	desired2.L2.Enabled = true
	plan2 := mustPlan(t, &disabled, desired2, utc(2026, time.August, 6, 12, 0))
	assertLayer(t, plan2.L2, PhaseActionReanchor, SelectionFirst, "xavier", false)
	if *plan2.Snapshot.L2.StartPosition != 0 {
		t.Fatalf("re-enable starts at position 0")
	}
}

// Re-enabling with UNCHANGED policy and group IDs must not
// classify as carry (that would copy a nil phase pair into an active layer).
func TestPlanTransition_ReenableUnchangedPolicy_NotCarry(t *testing.T) {
	snap := baseSnapshot()
	snap.L2 = RotationLayerSnapshot{
		Enabled: false,
		Policy:  weeklyPolicy("11:00", 1),
		Groups:  L2GroupsFromUserIDs([]string{"xavier", "yulia"}),
	}
	desired := ConfigurationFromSnapshot(snap)
	desired.L2.Enabled = true
	plan := mustPlan(t, &snap, desired, effAt)
	if plan.L2.PhaseAction == PhaseActionCarry {
		t.Fatalf("re-enable classified as carry: would copy a nil phase pair")
	}
	assertLayer(t, plan.L2, PhaseActionReanchor, SelectionFirst, "xavier", false)
	if plan.Snapshot.L2.PhaseAnchorSlotStart == nil || plan.Snapshot.L2.StartPosition == nil {
		t.Fatalf("re-enabled layer must get a fresh phase pair")
	}
}

// §9.8: edit exactly at the handoff instant. Half-open slots: at T the new
// slot's group is already active; it carries over without a double advance.
func TestPlanTransition_EditExactlyAtHandoff(t *testing.T) {
	snap := baseSnapshot()
	boundary := utc(2026, time.August, 4, 11, 0) // bob's slot starts here
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Groups[1].Members = []string{"bob", "dave"}

	plan := mustPlan(t, &snap, desired, boundary)
	assertLayer(t, plan.L1, PhaseActionCarry, SelectionPreserve, gid[1], true)
	if id := activeAt(t, plan.Snapshot, boundary); id != gid[1] {
		t.Fatalf("at handoff instant active = %s, want bob", id)
	}
	// No double advance: carol takes over only at the NEXT boundary.
	if id := activeAt(t, plan.Snapshot, utc(2026, time.August, 5, 11, 0)); id != gid[2] {
		t.Fatalf("next boundary active = %s, want carol", id)
	}
}

func TestPlanTransition_InitialRevision(t *testing.T) {
	desired := ConfigurationFromSnapshot(baseSnapshot())
	plan := mustPlan(t, nil, desired, effAt)
	if plan.Noop {
		t.Fatalf("initial revision is never a no-op")
	}
	assertLayer(t, plan.L1, PhaseActionReanchor, SelectionFirst, gid[0], false)
	assertLayer(t, plan.L2, PhaseActionClear, SelectionNone, "", false)
	if !plan.Snapshot.L1.PhaseAnchorSlotStart.Equal(utc(2026, time.August, 4, 11, 0)) {
		t.Fatalf("initial anchor = %v", plan.Snapshot.L1.PhaseAnchorSlotStart)
	}
	ch := plan.Change
	if !ch.L1GroupsChanged || !ch.L1PolicyChanged || !ch.L2Changed || !ch.TimezoneChanged || !ch.SlackUsergroupChanged {
		t.Fatalf("initial revision summary must mark everything changed: %+v", ch)
	}
}

func TestPlanTransition_RequiresEffectiveAt(t *testing.T) {
	snap := baseSnapshot()
	_, err := PlanTransition(TransitionInput{Current: &snap, Desired: ConfigurationFromSnapshot(snap)})
	if err == nil {
		t.Fatalf("zero effective_at must be rejected")
	}
}

// TestPlanTransition_ClassificationPartition enumerates current x desired
// states and checks that exactly the pair prescribed by the normative order
// comes out. The expectation function below is an INDEPENDENT restatement of
// the four rules; the test fails if implementation and rules diverge.
func TestPlanTransition_ClassificationPartition(t *testing.T) {
	type curKind int
	type desKind int
	const (
		curNil curKind = iota
		curDisabled
		curEmpty
		curActive
	)
	const (
		desDisabled desKind = iota
		desEmpty
		desMetadataOnly    // layers untouched (slack changes)
		desMembershipEdit  // members change, IDs unchanged
		desPolicyChanged   // handoff time changed
		desTZChanged       // timezone changed
		desAddGroup        // ordered ID list extended
		desRemoveActive    // active removed, others survive
		desReplaceAll      // no old ID survives
		desTZAndRemove     // composite: tz + active removed
	)
	curKinds := []curKind{curNil, curDisabled, curEmpty, curActive}
	desKinds := []desKind{desDisabled, desEmpty, desMetadataOnly, desMembershipEdit,
		desPolicyChanged, desTZChanged, desAddGroup, desRemoveActive, desReplaceAll, desTZAndRemove}

	expect := func(c curKind, d desKind) (action, selection string) {
		switch {
		case d == desDisabled || d == desEmpty:
			return PhaseActionClear, SelectionNone // rule 1
		case c != curActive:
			return PhaseActionReanchor, SelectionFirst // rule 2
		case d == desMetadataOnly || d == desMembershipEdit:
			return PhaseActionCarry, SelectionPreserve // rule 3
		case d == desRemoveActive || d == desTZAndRemove:
			return PhaseActionReanchor, SelectionSuccessor // rule 4
		case d == desReplaceAll:
			return PhaseActionReanchor, SelectionFirst // rule 4
		default:
			return PhaseActionReanchor, SelectionPreserve // rule 4
		}
	}

	makeCurrent := func(c curKind) *ScheduleRevisionSnapshot {
		if c == curNil {
			return nil
		}
		s := baseSnapshot()
		switch c {
		case curDisabled:
			s.L1.Enabled = false
			s.L1.PhaseAnchorSlotStart = nil
			s.L1.StartPosition = nil
		case curEmpty:
			s.L1.Groups = nil
			s.L1.PhaseAnchorSlotStart = nil
			s.L1.StartPosition = nil
		}
		return &s
	}

	makeDesired := func(d desKind) ScheduleConfiguration {
		c := ConfigurationFromSnapshot(baseSnapshot())
		c.SlackUsergroupID = "S-changed" // never a no-op, never affects layers
		switch d {
		case desDisabled:
			c.L1.Enabled = false
		case desEmpty:
			c.L1.Groups = nil
		case desMembershipEdit:
			c.L1.Groups[1].Members = []string{"bob", "dave"}
		case desPolicyChanged:
			c.L1.Policy = dailyPolicy("18:00")
		case desTZChanged:
			c.Timezone = "Europe/Moscow"
		case desAddGroup:
			c.L1.Groups = append(c.L1.Groups, RotationGroup{ID: gid[3], Members: []string{"dana"}})
		case desRemoveActive: // bob is active at effAt for curActive
			c.L1.Groups = []RotationGroup{
				{ID: gid[0], Members: []string{"alice"}},
				{ID: gid[2], Members: []string{"carol"}},
			}
		case desReplaceAll:
			c.L1.Groups = []RotationGroup{
				{ID: gid[3], Members: []string{"dana"}},
				{ID: gid[4], Members: []string{"erik"}},
			}
		case desTZAndRemove:
			c.Timezone = "Europe/Moscow"
			c.L1.Groups = []RotationGroup{
				{ID: gid[0], Members: []string{"alice"}},
				{ID: gid[2], Members: []string{"carol"}},
			}
		}
		return c
	}

	for _, ck := range curKinds {
		for _, dk := range desKinds {
			plan := mustPlan(t, makeCurrent(ck), makeDesired(dk), effAt)
			if plan.Noop {
				t.Fatalf("cur=%d des=%d: unexpected no-op", ck, dk)
			}
			wantA, wantS := expect(ck, dk)
			if plan.L1.PhaseAction != wantA || plan.L1.GroupSelection != wantS {
				t.Errorf("cur=%d des=%d: got %s+%s, want %s+%s",
					ck, dk, plan.L1.PhaseAction, plan.L1.GroupSelection, wantA, wantS)
			}
		}
	}
}
