package rotation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/tokayops/tokayops/internal/model"
)

// ErrSnapshotDecode is wrapped by every DecodeSnapshot failure so callers can
// route all decode problems to one operational alert. A decode failure is
// never converted into an empty rotation: empty is a valid explicit state,
// not a fallback for corrupt data.
var ErrSnapshotDecode = errors.New("rotation: snapshot decode failed")

// CanonicalizeSnapshot returns the canonical persistence form of a snapshot:
// anchors normalized to UTC and nil group slices coerced to []. It validates
// afterwards, so an invalid snapshot never reaches a canonical form.
//
// This is the single definition of what storage does to a snapshot, and it is
// exported for exactly that reason: a writer that canonicalizes before storing
// keeps its in-memory copy equal to what a later read returns. Leaving the
// step inside EncodeSnapshot made the transformation visible only to whoever
// serialized, so an in-memory snapshot and its persisted twin could differ in
// nil-vs-empty groups and in the anchor's location.
//
// The input is never mutated.
func CanonicalizeSnapshot(s ScheduleRevisionSnapshot) (ScheduleRevisionSnapshot, error) {
	c := s.clone()
	for _, l := range [2]*RotationLayerSnapshot{&c.L1, &c.L2} {
		if l.PhaseAnchorSlotStart != nil {
			u := l.PhaseAnchorSlotStart.UTC()
			l.PhaseAnchorSlotStart = &u
		}
		if l.Groups == nil {
			l.Groups = []RotationGroup{}
		}
	}
	if err := c.Validate(); err != nil {
		return ScheduleRevisionSnapshot{}, err
	}
	return c, nil
}

// EncodeSnapshot serializes a snapshot in its canonical form, with all fields
// always present (nil pointers serialize as explicit null). An invalid
// snapshot is never written.
func EncodeSnapshot(s ScheduleRevisionSnapshot) ([]byte, error) {
	c, err := CanonicalizeSnapshot(s)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// DecodeSnapshot is the single strict codec for snapshot bytes.
//
// Pass 1 probes schema_version leniently (a strict probe would reject every
// other field) and requires EOF. Pass 2 is presence-aware: every contract
// field of every object must be PRESENT - an absent field is an error even
// when its zero value or explicit null would be legal. This keeps "null" and
// "missing" distinguishable, so truncated or hand-edited persistence JSON
// cannot masquerade as a valid snapshot.
func DecodeSnapshot(data []byte) (ScheduleRevisionSnapshot, error) {
	var zero ScheduleRevisionSnapshot

	var envelope struct {
		SchemaVersion *int `json:"schema_version"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&envelope); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrSnapshotDecode, err)
	}
	if err := requireEOF(dec); err != nil {
		return zero, err
	}
	if envelope.SchemaVersion == nil || *envelope.SchemaVersion == 0 {
		return zero, fmt.Errorf("%w: missing schema_version", ErrSnapshotDecode)
	}
	if v := *envelope.SchemaVersion; v > SnapshotSchemaVersion {
		return zero, fmt.Errorf("%w: schema_version %d is newer than supported %d",
			ErrSnapshotDecode, v, SnapshotSchemaVersion)
	} else if v < 1 {
		return zero, fmt.Errorf("%w: invalid schema_version %d", ErrSnapshotDecode, v)
	}

	s, err := decodeSnapshotV1(data)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrSnapshotDecode, err)
	}
	if err := s.Validate(); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrSnapshotDecode, err)
	}
	return s, nil
}

func requireEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data after snapshot", ErrSnapshotDecode)
	}
	return nil
}

// strictObject rejects null, non-objects, unknown keys and missing keys.
func strictObject(raw json.RawMessage, what string, keys []string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", what, err)
	}
	if m == nil {
		return nil, fmt.Errorf("%s must not be null", what)
	}
	for k := range m {
		known := false
		for _, want := range keys {
			if k == want {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("%s: unknown field %q", what, k)
		}
	}
	for _, want := range keys {
		if _, ok := m[want]; !ok {
			return nil, fmt.Errorf("%s: missing field %q", what, want)
		}
	}
	return m, nil
}

func fieldString(m map[string]json.RawMessage, what, key string) (string, error) {
	var v *string
	if err := json.Unmarshal(m[key], &v); err != nil {
		return "", fmt.Errorf("%s.%s: %v", what, key, err)
	}
	if v == nil {
		return "", fmt.Errorf("%s.%s must not be null", what, key)
	}
	return *v, nil
}

func fieldInt(m map[string]json.RawMessage, what, key string) (int, error) {
	var v *int
	if err := json.Unmarshal(m[key], &v); err != nil {
		return 0, fmt.Errorf("%s.%s: %v", what, key, err)
	}
	if v == nil {
		return 0, fmt.Errorf("%s.%s must not be null", what, key)
	}
	return *v, nil
}

func fieldBool(m map[string]json.RawMessage, what, key string) (bool, error) {
	var v *bool
	if err := json.Unmarshal(m[key], &v); err != nil {
		return false, fmt.Errorf("%s.%s: %v", what, key, err)
	}
	if v == nil {
		return false, fmt.Errorf("%s.%s must not be null", what, key)
	}
	return *v, nil
}

func decodeSnapshotV1(data []byte) (ScheduleRevisionSnapshot, error) {
	var zero ScheduleRevisionSnapshot
	root, err := strictObject(data, "snapshot", []string{
		"schema_version", "timezone", "slack_usergroup_id", "l1", "l2",
		"l2_escalation_timeout_mins",
	})
	if err != nil {
		return zero, err
	}
	var s ScheduleRevisionSnapshot
	if s.SchemaVersion, err = fieldInt(root, "snapshot", "schema_version"); err != nil {
		return zero, err
	}
	if s.Timezone, err = fieldString(root, "snapshot", "timezone"); err != nil {
		return zero, err
	}
	if s.SlackUsergroupID, err = fieldString(root, "snapshot", "slack_usergroup_id"); err != nil {
		return zero, err
	}
	if s.L2EscalationTimeoutMins, err = fieldInt(root, "snapshot", "l2_escalation_timeout_mins"); err != nil {
		return zero, err
	}
	if s.L1, err = decodeLayerV1(root["l1"], "l1"); err != nil {
		return zero, err
	}
	if s.L2, err = decodeLayerV1(root["l2"], "l2"); err != nil {
		return zero, err
	}
	return s, nil
}

func decodeLayerV1(raw json.RawMessage, what string) (RotationLayerSnapshot, error) {
	var zero RotationLayerSnapshot
	m, err := strictObject(raw, what, []string{
		"enabled", "policy", "groups", "phase_anchor_slot_start", "start_position",
	})
	if err != nil {
		return zero, err
	}
	var l RotationLayerSnapshot
	if l.Enabled, err = fieldBool(m, what, "enabled"); err != nil {
		return zero, err
	}
	if l.Policy, err = decodePolicyV1(m["policy"], what+".policy"); err != nil {
		return zero, err
	}

	// groups: must be present and non-null (the contract writes [] for an
	// empty layer, never null).
	var groupsRaw *[]json.RawMessage
	if err := json.Unmarshal(m["groups"], &groupsRaw); err != nil {
		return zero, fmt.Errorf("%s.groups: %v", what, err)
	}
	if groupsRaw == nil {
		return zero, fmt.Errorf("%s.groups must not be null", what)
	}
	l.Groups = make([]RotationGroup, 0, len(*groupsRaw))
	for i, groupRaw := range *groupsRaw {
		gm, err := strictObject(groupRaw, fmt.Sprintf("%s.groups[%d]", what, i), []string{"id", "members"})
		if err != nil {
			return zero, err
		}
		var g RotationGroup
		if g.ID, err = fieldString(gm, what+".group", "id"); err != nil {
			return zero, err
		}
		var members *[]string
		if err := json.Unmarshal(gm["members"], &members); err != nil {
			return zero, fmt.Errorf("%s.groups[%d].members: %v", what, i, err)
		}
		if members == nil {
			return zero, fmt.Errorf("%s.groups[%d].members must not be null", what, i)
		}
		g.Members = *members
		l.Groups = append(l.Groups, g)
	}

	// Phase pair: explicit null is meaningful (disabled/empty layer), so
	// these decode into pointers; absence was already rejected above.
	if err := json.Unmarshal(m["phase_anchor_slot_start"], &l.PhaseAnchorSlotStart); err != nil {
		return zero, fmt.Errorf("%s.phase_anchor_slot_start: %v", what, err)
	}
	if err := json.Unmarshal(m["start_position"], &l.StartPosition); err != nil {
		return zero, fmt.Errorf("%s.start_position: %v", what, err)
	}
	return l, nil
}

func decodePolicyV1(raw json.RawMessage, what string) (RotationPolicy, error) {
	var zero RotationPolicy
	m, err := strictObject(raw, what, []string{"cadence", "handoff_time", "handoff_day"})
	if err != nil {
		return zero, err
	}
	var p RotationPolicy
	cadence, err := fieldString(m, what, "cadence")
	if err != nil {
		return zero, err
	}
	p.Cadence = model.RotationType(cadence) // unknown values rejected by Validate
	if p.HandoffTime, err = fieldString(m, what, "handoff_time"); err != nil {
		return zero, err
	}
	if err := json.Unmarshal(m["handoff_day"], &p.HandoffDay); err != nil {
		return zero, fmt.Errorf("%s.handoff_day: %v", what, err)
	}
	return p, nil
}
