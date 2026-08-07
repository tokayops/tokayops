package rotation

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// Canonical UUIDs for L1 test groups.
var gid = [...]string{
	"0c8f8f5e-4bda-4a44-9b8e-7f4a1f6de111",
	"1d9f8f5e-4bda-4a44-9b8e-7f4a1f6de222",
	"2e0f8f5e-4bda-4a44-9b8e-7f4a1f6de333",
	"3f1f8f5e-4bda-4a44-9b8e-7f4a1f6de444",
	"4a2f8f5e-4bda-4a44-9b8e-7f4a1f6de555",
	"5b3f8f5e-4bda-4a44-9b8e-7f4a1f6de666",
}

func intp(i int) *int { return &i }

func timep(t time.Time) *time.Time { return &t }

func dailyPolicy(hhmm string) RotationPolicy {
	return RotationPolicy{SchemaVersion: 1, Cadence: model.RotationDaily, HandoffTime: hhmm}
}

func weeklyPolicy(hhmm string, day int) RotationPolicy {
	return RotationPolicy{SchemaVersion: 1, Cadence: model.RotationWeekly, HandoffTime: hhmm, HandoffDay: intp(day)}
}

func mustGrid(t *testing.T, tz string, p RotationPolicy) Grid {
	t.Helper()
	g, err := NewGrid(tz, p)
	if err != nil {
		t.Fatalf("NewGrid(%q): %v", tz, err)
	}
	return g
}

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func inZone(t *testing.T, tz string, y int, m time.Month, d, hh, mm int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", tz, err)
	}
	return time.Date(y, m, d, hh, mm, 0, 0, loc)
}

// baseSnapshot: UTC, daily 11:00, three L1 groups [alice], [bob], [carol]
// with stable IDs gid[0..2], anchor Mon 2026-08-03 11:00 UTC, position 0;
// L2 disabled. Active group at 2026-08-04 12:00 UTC is gid[1] (bob).
func baseSnapshot() ScheduleRevisionSnapshot {
	return ScheduleRevisionSnapshot{
		SchemaVersion:    1,
		Timezone:         "UTC",
		SlackUsergroupID: "S0123",
		L1: RotationLayerSnapshot{
			Enabled: true,
			Policy:  dailyPolicy("11:00"),
			Groups: []RotationGroup{
				{ID: gid[0], Members: []string{"alice"}},
				{ID: gid[1], Members: []string{"bob"}},
				{ID: gid[2], Members: []string{"carol"}},
			},
			PhaseAnchorSlotStart: timep(utc(2026, time.August, 3, 11, 0)),
			StartPosition:        intp(0),
		},
		L2: RotationLayerSnapshot{
			Enabled: false,
			Policy:  weeklyPolicy("11:00", 1),
			Groups:  nil,
		},
		L2EscalationTimeoutMins: 5,
	}
}
