package rotation

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func baseConfiguration() ScheduleConfiguration {
	return ConfigurationFromSnapshot(baseSnapshot())
}

func TestNormalize_SortsL1MembersOnly(t *testing.T) {
	c := baseConfiguration()
	c.L1.Groups[1].Members = []string{"dave", "bob"}
	c.L2 = LayerConfiguration{
		Enabled: true,
		Policy:  weeklyPolicy("11:00", 1),
		Groups:  L2GroupsFromUserIDs([]string{"yulia", "xavier"}),
	}
	n, err := NormalizeConfiguration(c)
	if err != nil {
		t.Fatalf("NormalizeConfiguration: %v", err)
	}
	if got := n.L1.Groups[1].Members; !reflect.DeepEqual(got, []string{"bob", "dave"}) {
		t.Fatalf("L1 members not sorted: %v", got)
	}
	// L2 order is fully significant and must survive normalization.
	if n.L2.Groups[0].ID != "yulia" || n.L2.Groups[1].ID != "xavier" {
		t.Fatalf("L2 order changed: %v", n.L2.Groups)
	}
	// Input untouched.
	if !reflect.DeepEqual(c.L1.Groups[1].Members, []string{"dave", "bob"}) {
		t.Fatalf("NormalizeConfiguration mutated its input")
	}
}

func TestNormalize_DailyDayNil(t *testing.T) {
	c := baseConfiguration()
	c.L1.Policy.HandoffDay = intp(3) // daily + day: canonicalized away
	n, err := NormalizeConfiguration(c)
	if err != nil {
		t.Fatalf("NormalizeConfiguration: %v", err)
	}
	if n.L1.Policy.HandoffDay != nil {
		t.Fatalf("daily handoff day must canonicalize to nil")
	}
}

func TestValidateConfiguration(t *testing.T) {
	ok := baseConfiguration()
	if err := ValidateConfiguration(ok); err != nil {
		t.Fatalf("base configuration invalid: %v", err)
	}

	bad := baseConfiguration()
	bad.L1.Groups[0].ID = "not-a-uuid"
	if err := ValidateConfiguration(bad); err == nil {
		t.Fatalf("non-UUID L1 group id must fail")
	}

	bad = baseConfiguration()
	bad.L2EscalationTimeoutMins = 0
	if err := ValidateConfiguration(bad); err == nil {
		t.Fatalf("escalation timeout 0 must fail")
	}

	bad = baseConfiguration()
	bad.L2.Groups = []RotationGroup{{ID: "x", Members: []string{"x", "y"}}}
	if err := ValidateConfiguration(bad); err == nil {
		t.Fatalf("non-singleton L2 group must fail")
	}
}

func TestL2GroupsFromUserIDs(t *testing.T) {
	got := L2GroupsFromUserIDs([]string{"xavier", "yulia"})
	want := []RotationGroup{
		{ID: "xavier", Members: []string{"xavier"}},
		{ID: "yulia", Members: []string{"yulia"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("L2GroupsFromUserIDs = %v", got)
	}
	if L2GroupsFromUserIDs(nil) != nil {
		t.Fatalf("nil in, nil out")
	}
}

func TestConfigEqual_Noop(t *testing.T) {
	base := baseConfiguration()

	// Identical resave is a no-op.
	if !ConfigEqual(base, baseConfiguration()) {
		t.Fatalf("identical configuration must be equal")
	}

	// Permuted members inside an L1 group is a no-op.
	permuted := baseConfiguration()
	permuted.L1.Groups[1].Members = []string{"bob"}
	base2 := baseConfiguration()
	base2.L1.Groups[1].Members = []string{"bob"}
	permuted.L1.Groups[0].Members = []string{"alice"}
	multi := baseConfiguration()
	multi.L1.Groups[1].Members = []string{"dave", "bob"}
	multi2 := baseConfiguration()
	multi2.L1.Groups[1].Members = []string{"bob", "dave"}
	if !ConfigEqual(multi, multi2) {
		t.Fatalf("member permutation inside an L1 group must be a no-op")
	}

	// Daily handoff day nil vs set is a no-op after canonicalization.
	day := baseConfiguration()
	day.L1.Policy.HandoffDay = intp(5)
	if !ConfigEqual(base, day) {
		t.Fatalf("daily handoff day must not affect equality")
	}

	// Group reorder is NOT a no-op (L1 order is significant).
	reorder := baseConfiguration()
	reorder.L1.Groups[0], reorder.L1.Groups[1] = reorder.L1.Groups[1], reorder.L1.Groups[0]
	if ConfigEqual(base, reorder) {
		t.Fatalf("L1 group reorder must not be a no-op")
	}

	// Membership change is not a no-op.
	member := baseConfiguration()
	member.L1.Groups[1].Members = []string{"bob", "dave"}
	if ConfigEqual(base, member) {
		t.Fatalf("membership change must not be a no-op")
	}

	// L2 user reorder is not a no-op (L2 order fully significant).
	l2a := baseConfiguration()
	l2a.L2.Enabled = true
	l2a.L2.Groups = L2GroupsFromUserIDs([]string{"xavier", "yulia"})
	l2b := baseConfiguration()
	l2b.L2.Enabled = true
	l2b.L2.Groups = L2GroupsFromUserIDs([]string{"yulia", "xavier"})
	if ConfigEqual(l2a, l2b) {
		t.Fatalf("L2 order change must not be a no-op")
	}

	// Escalation timeout change is not a no-op.
	timeout := baseConfiguration()
	timeout.L2EscalationTimeoutMins = 10
	if ConfigEqual(base, timeout) {
		t.Fatalf("escalation timeout change must not be a no-op")
	}

	// Empty groups vs nil groups is a no-op (canonical form).
	empty := baseConfiguration()
	empty.L2.Groups = []RotationGroup{}
	if !ConfigEqual(base, empty) {
		t.Fatalf("nil vs empty groups must be equal")
	}
}

func TestConfigurationFromSnapshot_DropsGeneratedFields(t *testing.T) {
	c := ConfigurationFromSnapshot(baseSnapshot())
	// The configuration type has no phase fields at all; verify no aliasing
	// with the snapshot instead.
	s := baseSnapshot()
	c2 := ConfigurationFromSnapshot(s)
	c2.L1.Groups[0].Members[0] = "mutated"
	if s.L1.Groups[0].Members[0] == "mutated" {
		t.Fatalf("ConfigurationFromSnapshot aliased snapshot slices")
	}
	if c.Timezone != "UTC" || len(c.L1.Groups) != 3 {
		t.Fatalf("unexpected configuration: %+v", c)
	}
}

// TestPurity_InputsUnchanged covers the purity contract for the exported
// entry points: capture JSON images of the inputs, run, mutate outputs,
// compare the images.
func TestPurity_InputsUnchanged(t *testing.T) {
	snap := baseSnapshot()
	desired := ConfigurationFromSnapshot(snap)
	desired.L1.Groups[1].Members = []string{"dave", "bob"} // unsorted on purpose
	desired.SlackUsergroupID = "S0999"

	snapBefore, _ := json.Marshal(snap)
	desiredBefore, _ := json.Marshal(desired)

	n, err := NormalizeConfiguration(desired)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanTransition(TransitionInput{
		Current:     &snap,
		Desired:     desired,
		EffectiveAt: utc(2026, time.August, 4, 12, 0),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Aggressively mutate every reachable output slice/pointer.
	n.L1.Groups[0].Members[0] = "zzz"
	n.L1.Groups[1].Members[0] = "zzz"
	plan.Snapshot.L1.Groups[0].Members[0] = "zzz"
	plan.Snapshot.L1.Groups[1].Members[0] = "zzz"
	*plan.Snapshot.L1.PhaseAnchorSlotStart = time.Time{}
	*plan.Snapshot.L1.StartPosition = 99

	snapAfter, _ := json.Marshal(snap)
	desiredAfter, _ := json.Marshal(desired)
	if string(snapBefore) != string(snapAfter) {
		t.Fatalf("PlanTransition mutated the current snapshot:\nbefore %s\nafter  %s", snapBefore, snapAfter)
	}
	if string(desiredBefore) != string(desiredAfter) {
		t.Fatalf("inputs mutated:\nbefore %s\nafter  %s", desiredBefore, desiredAfter)
	}
}
