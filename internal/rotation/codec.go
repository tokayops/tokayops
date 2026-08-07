package rotation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrSnapshotDecode is wrapped by every DecodeSnapshot failure so callers can
// route all decode problems to one operational alert. A decode failure is
// never converted into an empty rotation: empty is a valid explicit state,
// not a fallback for corrupt data.
var ErrSnapshotDecode = errors.New("rotation: snapshot decode failed")

// EncodeSnapshot serializes a snapshot in the canonical persistence form:
// anchors normalized to UTC, nil group slices coerced to [], all fields
// always present (nil pointers serialize as explicit null). Encoding
// validates first; an invalid snapshot is never written.
func EncodeSnapshot(s ScheduleRevisionSnapshot) ([]byte, error) {
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
		return nil, err
	}
	return json.Marshal(c)
}

// DecodeSnapshot is the single strict codec for snapshot bytes.
//
// Two passes: a schema_version probe WITHOUT DisallowUnknownFields (a strict
// probe would reject every other snapshot field), then a full decode WITH it.
// Both passes require EOF afterwards. null, {}, a missing/zero schema_version
// and a schema_version newer than this binary are all errors.
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

	var s ScheduleRevisionSnapshot
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrSnapshotDecode, err)
	}
	if err := requireEOF(dec); err != nil {
		return zero, err
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
