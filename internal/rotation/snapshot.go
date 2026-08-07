package rotation

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SnapshotSchemaVersion is the only snapshot schema version this binary can
// encode. Decoding dispatches on the top-level schema_version (see codec.go).
const SnapshotSchemaVersion = 1

// Escalation timeout bounds in minutes. Legacy rows predate this validation
// and may hold out-of-range values; code importing them must coerce.
const (
	EscalationTimeoutMinMins = 1
	EscalationTimeoutMaxMins = 1440
)

// RotationGroup is a stable-identity group of user IDs. For L1 the ID is a
// canonical UUID owned by user intent (editor row identity); for L2 the group
// is a singleton and its stable identity is the user ID itself.
type RotationGroup struct {
	ID      string   `json:"id"`
	Members []string `json:"members"` // user IDs
}

// RotationLayerSnapshot is the full per-layer configuration plus the phase
// pair owned by the layer. The pair is either both nil (disabled or empty
// layer) or both set; on carry transitions it is copied byte-identical.
type RotationLayerSnapshot struct {
	Enabled              bool            `json:"enabled"`
	Policy               RotationPolicy  `json:"policy"`
	Groups               []RotationGroup `json:"groups"`
	PhaseAnchorSlotStart *time.Time      `json:"phase_anchor_slot_start"`
	StartPosition        *int            `json:"start_position"`
}

// ScheduleRevisionSnapshot is the self-contained configuration snapshot of
// one schedule revision. Timezone lives here once for both layers.
type ScheduleRevisionSnapshot struct {
	SchemaVersion           int                   `json:"schema_version"`
	Timezone                string                `json:"timezone"`
	SlackUsergroupID        string                `json:"slack_usergroup_id"`
	L1                      RotationLayerSnapshot `json:"l1"`
	L2                      RotationLayerSnapshot `json:"l2"`
	L2EscalationTimeoutMins int                   `json:"l2_escalation_timeout_mins"`
}

// isCanonicalUUID accepts only the dashed lowercase canonical form. Group
// identity is compared as a plain string everywhere (transition planner,
// duplicate checks), so there must be exactly ONE spelling of an ID:
// uppercase variants are rejected, not normalized. uuid.Parse alone is too
// permissive: it also accepts raw hex, urn:uuid:, braced and uppercase forms.
func isCanonicalUUID(s string) bool {
	u, err := uuid.Parse(s)
	if err != nil {
		return false
	}
	return u.String() == s
}

func validateL1Group(g RotationGroup) error {
	if !isCanonicalUUID(g.ID) {
		return fmt.Errorf("rotation: l1 group id %q is not a canonical UUID", g.ID)
	}
	return validateGroupMembers(g)
}

// validateL2Group enforces the singleton shape: the group's stable identity
// is the user ID. User IDs are NOT UUIDs by contract (the admin API accepts
// arbitrary IDs or derives them from the email prefix), so the ID is opaque.
func validateL2Group(g RotationGroup) error {
	if len(g.Members) != 1 {
		return fmt.Errorf("rotation: l2 group %q must be a singleton, got %d members", g.ID, len(g.Members))
	}
	if g.ID == "" {
		return fmt.Errorf("rotation: l2 group has empty id")
	}
	if g.ID != g.Members[0] {
		return fmt.Errorf("rotation: l2 group id %q must equal its member %q", g.ID, g.Members[0])
	}
	return validateGroupMembers(g)
}

func validateGroupMembers(g RotationGroup) error {
	if len(g.Members) == 0 {
		return fmt.Errorf("rotation: group %q is empty", g.ID)
	}
	seen := make(map[string]struct{}, len(g.Members))
	for _, m := range g.Members {
		if m == "" {
			return fmt.Errorf("rotation: group %q has empty member id", g.ID)
		}
		if _, dup := seen[m]; dup {
			return fmt.Errorf("rotation: group %q has duplicate member %q", g.ID, m)
		}
		seen[m] = struct{}{}
	}
	return nil
}

// validateLayer checks group shape, phase pair invariants and, when the pair
// is set, that the anchor lies exactly on a grid slot boundary.
func validateLayer(name string, l RotationLayerSnapshot, timezone string) error {
	if err := l.Policy.Validate(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for _, g := range l.Groups {
		var err error
		if name == "l1" {
			err = validateL1Group(g)
		} else {
			err = validateL2Group(g)
		}
		if err != nil {
			return err
		}
	}

	hasAnchor := l.PhaseAnchorSlotStart != nil
	hasPosition := l.StartPosition != nil
	if hasAnchor != hasPosition {
		return fmt.Errorf("rotation: %s phase pair must be both nil or both set", name)
	}
	active := l.Enabled && len(l.Groups) > 0
	if active && !hasAnchor {
		return fmt.Errorf("rotation: %s is active but has no phase pair", name)
	}
	if !active && hasAnchor {
		return fmt.Errorf("rotation: %s is disabled or empty but has a phase pair", name)
	}
	if !hasAnchor {
		return nil
	}

	if *l.StartPosition < 0 || *l.StartPosition >= len(l.Groups) {
		return fmt.Errorf("rotation: %s start_position %d out of range [0..%d)", name, *l.StartPosition, len(l.Groups))
	}
	g, err := NewGrid(timezone, l.Policy)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if !g.SlotContaining(*l.PhaseAnchorSlotStart).Start.Equal(*l.PhaseAnchorSlotStart) {
		return fmt.Errorf("rotation: %s phase_anchor_slot_start %v is not a grid slot boundary",
			name, *l.PhaseAnchorSlotStart)
	}
	return nil
}

// Validate checks every snapshot invariant. Decoded snapshots that fail here
// must surface as errors, never as an empty rotation.
func (s ScheduleRevisionSnapshot) Validate() error {
	if s.SchemaVersion != SnapshotSchemaVersion {
		return fmt.Errorf("rotation: unsupported snapshot schema_version %d", s.SchemaVersion)
	}
	if _, err := validateTimezone(s.Timezone); err != nil {
		return err
	}
	if s.L2EscalationTimeoutMins < EscalationTimeoutMinMins || s.L2EscalationTimeoutMins > EscalationTimeoutMaxMins {
		return fmt.Errorf("rotation: l2_escalation_timeout_mins %d out of range %d..%d",
			s.L2EscalationTimeoutMins, EscalationTimeoutMinMins, EscalationTimeoutMaxMins)
	}
	if err := validateLayer("l1", s.L1, s.Timezone); err != nil {
		return err
	}
	if err := validateLayer("l2", s.L2, s.Timezone); err != nil {
		return err
	}
	// Group IDs are unique across the whole revision, not just per layer.
	seen := make(map[string]struct{})
	for _, l := range [2][]RotationGroup{s.L1.Groups, s.L2.Groups} {
		for _, g := range l {
			if _, dup := seen[g.ID]; dup {
				return fmt.Errorf("rotation: duplicate group id %q in revision", g.ID)
			}
			seen[g.ID] = struct{}{}
		}
	}
	return nil
}

func cloneGroups(gs []RotationGroup) []RotationGroup {
	if gs == nil {
		return nil
	}
	out := make([]RotationGroup, len(gs))
	for i, g := range gs {
		out[i] = RotationGroup{ID: g.ID, Members: append([]string(nil), g.Members...)}
	}
	return out
}

func (l RotationLayerSnapshot) clone() RotationLayerSnapshot {
	c := RotationLayerSnapshot{
		Enabled: l.Enabled,
		Policy:  l.Policy.clone(),
		Groups:  cloneGroups(l.Groups),
	}
	if l.PhaseAnchorSlotStart != nil {
		t := *l.PhaseAnchorSlotStart
		c.PhaseAnchorSlotStart = &t
	}
	if l.StartPosition != nil {
		p := *l.StartPosition
		c.StartPosition = &p
	}
	return c
}

func (s ScheduleRevisionSnapshot) clone() ScheduleRevisionSnapshot {
	return ScheduleRevisionSnapshot{
		SchemaVersion:           s.SchemaVersion,
		Timezone:                s.Timezone,
		SlackUsergroupID:        s.SlackUsergroupID,
		L1:                      s.L1.clone(),
		L2:                      s.L2.clone(),
		L2EscalationTimeoutMins: s.L2EscalationTimeoutMins,
	}
}
