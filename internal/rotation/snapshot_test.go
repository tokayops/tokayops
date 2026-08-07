package rotation

import (
	"strings"
	"testing"
	"time"
)

func TestIsCanonicalUUID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "0c8f8f5e-4bda-4a44-9b8e-7f4a1f6de111", want: true},
		// Exactly ONE spelling of an identity: transition compares IDs as
		// plain strings, so uppercase forms are rejected, not normalized.
		{in: "0C8F8F5E-4BDA-4A44-9B8E-7F4A1F6DE111", want: false},
		{in: "0c8f8f5e-4bda-4a44-9B8E-7f4a1f6de111", want: false}, // mixed case
		{in: "0c8f8f5e4bda4a449b8e7f4a1f6de111", want: false},     // raw hex
		{in: "urn:uuid:0c8f8f5e-4bda-4a44-9b8e-7f4a1f6de111", want: false},
		{in: "{0c8f8f5e-4bda-4a44-9b8e-7f4a1f6de111}", want: false},
		{in: "not-a-uuid", want: false},
		{in: "", want: false},
	}
	for _, tt := range tests {
		if got := isCanonicalUUID(tt.in); got != tt.want {
			t.Errorf("isCanonicalUUID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSnapshotValidate(t *testing.T) {
	valid := baseSnapshot()
	if err := valid.Validate(); err != nil {
		t.Fatalf("base snapshot must be valid: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*ScheduleRevisionSnapshot)
		wantErr string
	}{
		{
			name:    "bad snapshot schema version",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.SchemaVersion = 2 },
			wantErr: "schema_version",
		},
		{
			name:    "empty timezone",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.Timezone = "" },
			wantErr: "empty timezone",
		},
		{
			name:    "invalid timezone",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.Timezone = "Mars/Olympus" },
			wantErr: "invalid timezone",
		},
		{
			name:    "escalation timeout zero",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L2EscalationTimeoutMins = 0 },
			wantErr: "out of range",
		},
		{
			name:    "escalation timeout too big",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L2EscalationTimeoutMins = 1441 },
			wantErr: "out of range",
		},
		{
			name:    "l1 group id not canonical uuid",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.Groups[0].ID = "0c8f8f5e4bda4a449b8e7f4a1f6de111" },
			wantErr: "canonical UUID",
		},
		{
			name:    "empty group",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.Groups[1].Members = nil },
			wantErr: "is empty",
		},
		{
			name:    "duplicate member in group",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.Groups[1].Members = []string{"bob", "bob"} },
			wantErr: "duplicate member",
		},
		{
			name: "duplicate group id",
			mutate: func(s *ScheduleRevisionSnapshot) {
				s.L1.Groups[2].ID = s.L1.Groups[0].ID
			},
			wantErr: "duplicate group id",
		},
		{
			name:    "half phase pair: only anchor",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.StartPosition = nil },
			wantErr: "both nil or both set",
		},
		{
			name:    "half phase pair: only position",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.PhaseAnchorSlotStart = nil },
			wantErr: "both nil or both set",
		},
		{
			name: "active layer without phase pair",
			mutate: func(s *ScheduleRevisionSnapshot) {
				s.L1.PhaseAnchorSlotStart = nil
				s.L1.StartPosition = nil
			},
			wantErr: "no phase pair",
		},
		{
			name: "disabled layer with phase pair",
			mutate: func(s *ScheduleRevisionSnapshot) {
				s.L2.PhaseAnchorSlotStart = timep(utc(2026, time.August, 3, 11, 0))
				s.L2.StartPosition = intp(0)
			},
			wantErr: "disabled or empty",
		},
		{
			name:    "start position out of range",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.StartPosition = intp(3) },
			wantErr: "out of range",
		},
		{
			name:    "start position negative",
			mutate:  func(s *ScheduleRevisionSnapshot) { s.L1.StartPosition = intp(-1) },
			wantErr: "out of range",
		},
		{
			name: "anchor not on grid boundary",
			mutate: func(s *ScheduleRevisionSnapshot) {
				s.L1.PhaseAnchorSlotStart = timep(utc(2026, time.August, 3, 11, 30))
			},
			wantErr: "not a grid slot boundary",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := baseSnapshot()
			tt.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestSnapshotValidate_L2Singleton(t *testing.T) {
	l2Active := func() ScheduleRevisionSnapshot {
		s := baseSnapshot()
		s.L2 = RotationLayerSnapshot{
			Enabled: true,
			Policy:  weeklyPolicy("11:00", 1),
			Groups: []RotationGroup{
				{ID: "xavier", Members: []string{"xavier"}},
				{ID: "yulia", Members: []string{"yulia"}},
			},
			// Weekly Monday 11:00 UTC boundary: 2026-08-03 is a Monday.
			PhaseAnchorSlotStart: timep(utc(2026, time.August, 3, 11, 0)),
			StartPosition:        intp(0),
		}
		return s
	}
	if err := l2Active().Validate(); err != nil {
		t.Fatalf("valid active L2: %v", err)
	}

	s := l2Active()
	s.L2.Groups[0].Members = []string{"xavier", "yulia"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "singleton") {
		t.Fatalf("multi-member L2 group must fail as singleton violation, got %v", err)
	}

	s = l2Active()
	s.L2.Groups[0].ID = "other"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "must equal its member") {
		t.Fatalf("L2 id != member must fail, got %v", err)
	}

	// L2 IDs are opaque user IDs: non-UUID is fine (user IDs are not
	// guaranteed UUIDs in this system).
	s = l2Active()
	s.L2.Groups[0] = RotationGroup{ID: "denis.puchkin", Members: []string{"denis.puchkin"}}
	if err := s.Validate(); err != nil {
		t.Fatalf("non-UUID L2 user id must be valid: %v", err)
	}
}
