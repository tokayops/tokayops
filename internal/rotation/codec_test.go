package rotation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// goldenSnapshotJSON is the frozen persistence contract for schema version 1.
// Changing it after merge requires a snapshot schema_version bump - the
// top-level one; the policy no longer carries a version of its own.
const goldenSnapshotJSON = `{"schema_version":1,"timezone":"Europe/Moscow","slack_usergroup_id":"S0123",` +
	`"l1":{"enabled":true,"policy":{"cadence":"daily","handoff_time":"11:00","handoff_day":null},` +
	`"groups":[{"id":"0c8f8f5e-4bda-4a44-9b8e-7f4a1f6de111","members":["alice"]},` +
	`{"id":"1d9f8f5e-4bda-4a44-9b8e-7f4a1f6de222","members":["bob","dave"]}],` +
	`"phase_anchor_slot_start":"2026-08-07T08:00:00Z","start_position":1},` +
	`"l2":{"enabled":false,"policy":{"cadence":"weekly","handoff_time":"11:00","handoff_day":1},` +
	`"groups":[],"phase_anchor_slot_start":null,"start_position":null},` +
	`"l2_escalation_timeout_mins":5}`

func goldenSnapshot() ScheduleRevisionSnapshot {
	return ScheduleRevisionSnapshot{
		SchemaVersion:    SnapshotSchemaVersion,
		Timezone:         "Europe/Moscow",
		SlackUsergroupID: "S0123",
		L1: RotationLayerSnapshot{
			Enabled: true,
			Policy:  dailyPolicy("11:00"),
			Groups: []RotationGroup{
				{ID: gid[0], Members: []string{"alice"}},
				{ID: gid[1], Members: []string{"bob", "dave"}},
			},
			// 11:00 Moscow (UTC+3) = 08:00 UTC.
			PhaseAnchorSlotStart: timep(utc(2026, time.August, 7, 8, 0)),
			StartPosition:        intp(1),
		},
		L2: RotationLayerSnapshot{
			Enabled: false,
			Policy:  weeklyPolicy("11:00", 1),
			Groups:  []RotationGroup{},
		},
		L2EscalationTimeoutMins: 5,
	}
}

func TestSnapshotCodec_RoundTripByteStable(t *testing.T) {
	encoded, err := EncodeSnapshot(goldenSnapshot())
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if string(encoded) != goldenSnapshotJSON {
		t.Fatalf("encode does not match golden contract:\n got: %s\nwant: %s", encoded, goldenSnapshotJSON)
	}
	decoded, err := DecodeSnapshot(encoded)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	re, err := EncodeSnapshot(decoded)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(re) != goldenSnapshotJSON {
		t.Fatalf("round trip is not byte-stable:\n got: %s\nwant: %s", re, goldenSnapshotJSON)
	}
}

// Second golden: both layers active, weekly policies, non-UTC timezone.
const goldenSnapshot2JSON = `{"schema_version":1,"timezone":"America/New_York","slack_usergroup_id":"",` +
	`"l1":{"enabled":true,"policy":{"cadence":"weekly","handoff_time":"09:00","handoff_day":0},` +
	`"groups":[{"id":"2e0f8f5e-4bda-4a44-9b8e-7f4a1f6de333","members":["carol","erik"]}],` +
	`"phase_anchor_slot_start":"2026-08-02T13:00:00Z","start_position":0},` +
	`"l2":{"enabled":true,"policy":{"cadence":"weekly","handoff_time":"09:00","handoff_day":0},` +
	`"groups":[{"id":"xavier","members":["xavier"]},{"id":"yulia","members":["yulia"]}],` +
	`"phase_anchor_slot_start":"2026-08-02T13:00:00Z","start_position":1},` +
	`"l2_escalation_timeout_mins":30}`

func TestSnapshotCodec_RoundTripByteStable_ActiveL2(t *testing.T) {
	// Sun 2026-08-02 09:00 America/New_York (EDT, UTC-4) = 13:00Z.
	snap := ScheduleRevisionSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Timezone:      "America/New_York",
		L1: RotationLayerSnapshot{
			Enabled: true,
			Policy:  weeklyPolicy("09:00", 0),
			Groups: []RotationGroup{
				{ID: gid[2], Members: []string{"carol", "erik"}},
			},
			PhaseAnchorSlotStart: timep(utc(2026, time.August, 2, 13, 0)),
			StartPosition:        intp(0),
		},
		L2: RotationLayerSnapshot{
			Enabled: true,
			Policy:  weeklyPolicy("09:00", 0),
			Groups: []RotationGroup{
				{ID: "xavier", Members: []string{"xavier"}},
				{ID: "yulia", Members: []string{"yulia"}},
			},
			PhaseAnchorSlotStart: timep(utc(2026, time.August, 2, 13, 0)),
			StartPosition:        intp(1),
		},
		L2EscalationTimeoutMins: 30,
	}
	encoded, err := EncodeSnapshot(snap)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if string(encoded) != goldenSnapshot2JSON {
		t.Fatalf("encode does not match golden 2:\n got: %s\nwant: %s", encoded, goldenSnapshot2JSON)
	}
	decoded, err := DecodeSnapshot(encoded)
	if err != nil {
		t.Fatalf("DecodeSnapshot: %v", err)
	}
	re, err := EncodeSnapshot(decoded)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(re) != goldenSnapshot2JSON {
		t.Fatalf("round trip not byte-stable:\n got: %s", re)
	}
}

func TestEncodeSnapshot_NormalizesAnchorToUTC(t *testing.T) {
	s := goldenSnapshot()
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatal(err)
	}
	local := s.L1.PhaseAnchorSlotStart.In(msk)
	s.L1.PhaseAnchorSlotStart = &local
	encoded, err := EncodeSnapshot(s)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if string(encoded) != goldenSnapshotJSON {
		t.Fatalf("anchor in non-UTC location must encode identically:\n got: %s", encoded)
	}
	if !s.L1.PhaseAnchorSlotStart.Equal(local) || s.L1.PhaseAnchorSlotStart.Location() != msk {
		t.Fatalf("EncodeSnapshot mutated its input")
	}
}

func TestEncodeSnapshot_RejectsInvalid(t *testing.T) {
	s := goldenSnapshot()
	s.L1.StartPosition = intp(5)
	if _, err := EncodeSnapshot(s); err == nil {
		t.Fatalf("invalid snapshot must not encode")
	}
}

func TestDecodeSnapshot_Errors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "null", in: `null`, want: "missing schema_version"},
		{name: "empty object", in: `{}`, want: "missing schema_version"},
		{name: "zero schema version", in: `{"schema_version":0}`, want: "missing schema_version"},
		{name: "negative schema version", in: `{"schema_version":-1}`, want: "invalid schema_version"},
		{
			name: "newer schema version",
			in:   `{"schema_version":2}`,
			want: "newer than supported",
		},
		{
			name: "unknown field",
			in:   strings.Replace(goldenSnapshotJSON, `"timezone"`, `"time_zone"`, 1),
			want: "unknown field",
		},
		{name: "malformed json", in: `{"schema_version":1,`, want: "snapshot decode"},
		{name: "trailing data", in: goldenSnapshotJSON + `{}`, want: "trailing data"},
		{name: "trailing scalar", in: `{"schema_version":1} 7`, want: "trailing data"},
		{
			// Well-formed JSON, valid schema version, but violates a snapshot
			// invariant: must be an error, never an empty rotation.
			name: "invariant violation",
			in:   strings.Replace(goldenSnapshotJSON, `"start_position":1`, `"start_position":9`, 1),
			want: "out of range",
		},
		{name: "array instead of object", in: `[1,2]`, want: "snapshot decode"},
		// Presence-aware contract: an ABSENT field is an error even where
		// its zero value or null would be legal - null and missing stay
		// distinguishable.
		{
			name: "missing slack_usergroup_id",
			in:   strings.Replace(goldenSnapshotJSON, `"slack_usergroup_id":"S0123",`, ``, 1),
			want: `missing field "slack_usergroup_id"`,
		},
		{
			name: "missing phase field in disabled layer",
			in:   strings.Replace(goldenSnapshotJSON, `"phase_anchor_slot_start":null,"start_position":null`, `"start_position":null`, 1),
			want: `missing field "phase_anchor_slot_start"`,
		},
		{
			name: "missing enabled",
			in:   strings.Replace(goldenSnapshotJSON, `"l2":{"enabled":false,`, `"l2":{`, 1),
			want: `missing field "enabled"`,
		},
		{
			name: "null groups",
			in:   strings.Replace(goldenSnapshotJSON, `"groups":[],`, `"groups":null,`, 1),
			want: "groups must not be null",
		},
		{
			name: "null timezone",
			in:   strings.Replace(goldenSnapshotJSON, `"timezone":"Europe/Moscow"`, `"timezone":null`, 1),
			want: "must not be null",
		},
		{
			name: "missing handoff_day",
			in:   strings.Replace(goldenSnapshotJSON, `"handoff_time":"11:00","handoff_day":null`, `"handoff_time":"11:00"`, 1),
			want: `missing field "handoff_day"`,
		},
		{
			name: "unknown nested field",
			in:   strings.Replace(goldenSnapshotJSON, `"l1":{"enabled":true,`, `"l1":{"enabled":true,"extra":1,`, 1),
			want: `unknown field "extra"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeSnapshot([]byte(tt.in))
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !errors.Is(err, ErrSnapshotDecode) {
				t.Fatalf("error %v must wrap ErrSnapshotDecode", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}
