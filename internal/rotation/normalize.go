package rotation

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/tokayops/tokayops/internal/model"
)

// LayerConfiguration is the user-intent configuration of one layer: no
// generated fields, no phase pair.
type LayerConfiguration struct {
	Enabled bool
	Policy  RotationPolicy
	Groups  []RotationGroup
}

// ScheduleConfiguration is the full desired configuration of a schedule as
// submitted by one Save. Its canonical form is defined by
// NormalizeConfiguration; the no-op predicate ConfigEqual compares canonical
// forms only.
type ScheduleConfiguration struct {
	Timezone                string
	SlackUsergroupID        string
	L1                      LayerConfiguration
	L2                      LayerConfiguration
	L2EscalationTimeoutMins int
}

// L2GroupsFromUserIDs converts the ordered L2 user list into singleton
// groups whose stable identity is the user ID itself, so both layers share
// one rotation math.
func L2GroupsFromUserIDs(userIDs []string) []RotationGroup {
	if userIDs == nil {
		return nil
	}
	out := make([]RotationGroup, len(userIDs))
	for i, id := range userIDs {
		out[i] = RotationGroup{ID: id, Members: []string{id}}
	}
	return out
}

func (l LayerConfiguration) clone() LayerConfiguration {
	return LayerConfiguration{
		Enabled: l.Enabled,
		Policy:  l.Policy.clone(),
		Groups:  cloneGroups(l.Groups),
	}
}

func (c ScheduleConfiguration) clone() ScheduleConfiguration {
	return ScheduleConfiguration{
		Timezone:                c.Timezone,
		SlackUsergroupID:        c.SlackUsergroupID,
		L1:                      c.L1.clone(),
		L2:                      c.L2.clone(),
		L2EscalationTimeoutMins: c.L2EscalationTimeoutMins,
	}
}

// canonicalizeConfiguration returns the canonical deep copy: daily handoff
// day nil, L1 members sorted by user ID (the canonical STORAGE form, not
// just a compare-time trick), empty group slices coerced to nil. L2 group
// order and L2 members stay untouched: L2 order is fully significant.
func canonicalizeConfiguration(c ScheduleConfiguration) ScheduleConfiguration {
	n := c.clone()
	for _, l := range [2]*LayerConfiguration{&n.L1, &n.L2} {
		if l.Policy.Cadence == model.RotationDaily {
			l.Policy.HandoffDay = nil
		}
		if len(l.Groups) == 0 {
			l.Groups = nil
		}
	}
	for i := range n.L1.Groups {
		sort.Strings(n.L1.Groups[i].Members)
	}
	return n
}

// NormalizeConfiguration canonicalizes and validates. The input is never
// mutated.
func NormalizeConfiguration(c ScheduleConfiguration) (ScheduleConfiguration, error) {
	n := canonicalizeConfiguration(c)
	if err := ValidateConfiguration(n); err != nil {
		return ScheduleConfiguration{}, err
	}
	return n, nil
}

// ValidateConfiguration applies the same rules as snapshot validation minus
// the generated phase pair (configurations have none by construction).
func ValidateConfiguration(c ScheduleConfiguration) error {
	if _, err := validateTimezone(c.Timezone); err != nil {
		return err
	}
	if c.L2EscalationTimeoutMins < EscalationTimeoutMinMins || c.L2EscalationTimeoutMins > EscalationTimeoutMaxMins {
		return fmt.Errorf("rotation: l2_escalation_timeout_mins %d out of range %d..%d",
			c.L2EscalationTimeoutMins, EscalationTimeoutMinMins, EscalationTimeoutMaxMins)
	}
	if err := c.L1.Policy.Validate(); err != nil {
		return fmt.Errorf("l1: %w", err)
	}
	if err := c.L2.Policy.Validate(); err != nil {
		return fmt.Errorf("l2: %w", err)
	}
	for _, g := range c.L1.Groups {
		if err := validateL1Group(g); err != nil {
			return err
		}
	}
	for _, g := range c.L2.Groups {
		if err := validateL2Group(g); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{})
	for _, l := range [2][]RotationGroup{c.L1.Groups, c.L2.Groups} {
		for _, g := range l {
			if _, dup := seen[g.ID]; dup {
				return fmt.Errorf("rotation: duplicate group id %q in configuration", g.ID)
			}
			seen[g.ID] = struct{}{}
		}
	}
	return nil
}

// ConfigurationFromSnapshot extracts the user-intent configuration, dropping
// the generated phase pairs. The snapshot is not aliased.
func ConfigurationFromSnapshot(s ScheduleRevisionSnapshot) ScheduleConfiguration {
	return ScheduleConfiguration{
		Timezone:         s.Timezone,
		SlackUsergroupID: s.SlackUsergroupID,
		L1: LayerConfiguration{
			Enabled: s.L1.Enabled,
			Policy:  s.L1.Policy.clone(),
			Groups:  cloneGroups(s.L1.Groups),
		},
		L2: LayerConfiguration{
			Enabled: s.L2.Enabled,
			Policy:  s.L2.Policy.clone(),
			Groups:  cloneGroups(s.L2.Groups),
		},
		L2EscalationTimeoutMins: s.L2EscalationTimeoutMins,
	}
}

// ConfigEqual is the semantic no-op predicate: it compares
// canonical user configuration only. Generated fields (phase pairs, revision
// IDs, timestamps) never participate.
func ConfigEqual(a, b ScheduleConfiguration) bool {
	return reflect.DeepEqual(canonicalizeConfiguration(a), canonicalizeConfiguration(b))
}
