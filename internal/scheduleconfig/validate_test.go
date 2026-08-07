package scheduleconfig

import (
	"errors"
	"testing"
	"time"
)

func testRevision() *ScheduleRevision {
	return &ScheduleRevision{
		ID:            "rev-1",
		ScheduleID:    "sched-1",
		Version:       1,
		EffectiveFrom: time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC),
	}
}

func TestPrepareRevisionRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ScheduleRevision)
	}{
		{name: "no id", mutate: func(r *ScheduleRevision) { r.ID = "" }},
		{name: "no schedule id", mutate: func(r *ScheduleRevision) { r.ScheduleID = "" }},
		{name: "zero version", mutate: func(r *ScheduleRevision) { r.Version = 0 }},
		{name: "negative version", mutate: func(r *ScheduleRevision) { r.Version = -1 }},
		{name: "zero effective from", mutate: func(r *ScheduleRevision) { r.EffectiveFrom = time.Time{} }},
		{
			name: "zero-length interval",
			mutate: func(r *ScheduleRevision) {
				to := r.EffectiveFrom
				r.EffectiveTo = &to
			},
		},
		{
			name: "inverted interval",
			mutate: func(r *ScheduleRevision) {
				to := r.EffectiveFrom.Add(-time.Hour)
				r.EffectiveTo = &to
			},
		},
		{
			// Below database resolution the two timestamps are one value.
			name: "sub-resolution interval",
			mutate: func(r *ScheduleRevision) {
				to := r.EffectiveFrom.Add(500 * time.Nanosecond)
				r.EffectiveTo = &to
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rev := testRevision()
			tc.mutate(rev)
			if err := PrepareRevision(rev); !errors.Is(err, ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
		})
	}
	if err := PrepareRevision(nil); !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("nil revision error = %v, want ErrInvariantViolation", err)
	}
}

func TestPrepareRevisionNormalizesTimestamps(t *testing.T) {
	rev := testRevision()
	rev.EffectiveFrom = rev.EffectiveFrom.Add(123 * time.Nanosecond)
	to := rev.EffectiveFrom.Add(time.Hour + 456*time.Nanosecond)
	rev.EffectiveTo = &to

	if err := PrepareRevision(rev); err != nil {
		t.Fatalf("PrepareRevision: %v", err)
	}
	for name, ts := range map[string]time.Time{
		"effective_from": rev.EffectiveFrom,
		"effective_to":   *rev.EffectiveTo,
		"recorded_at":    rev.RecordedAt,
	} {
		if ts.Truncate(TimestampResolution) != ts {
			t.Fatalf("%s = %v carries sub-resolution precision", name, ts)
		}
	}
	// An unset recorded time is filled in rather than stored as year one.
	if rev.RecordedAt.IsZero() {
		t.Fatal("recorded_at was left zero")
	}
}

func TestPrepareInitialSchedule(t *testing.T) {
	root := &ScheduleRoot{ID: "sched-1", TeamID: "devops"}
	rev := testRevision()

	if err := PrepareInitialSchedule(root, rev); err != nil {
		t.Fatalf("PrepareInitialSchedule: %v", err)
	}
	if root.ConfigVersion != 1 {
		t.Fatalf("config version = %d, want 1", root.ConfigVersion)
	}
	if root.HistoryCompleteFrom == nil || !root.HistoryCompleteFrom.Equal(rev.EffectiveFrom) {
		t.Fatalf("history_complete_from = %v, want %v", root.HistoryCompleteFrom, rev.EffectiveFrom)
	}
	// The derived pointer must not alias the revision's own field.
	*root.HistoryCompleteFrom = time.Unix(0, 0).UTC()
	if rev.EffectiveFrom.Equal(time.Unix(0, 0).UTC()) {
		t.Fatal("history_complete_from aliases the revision's effective_from")
	}
}

func TestPrepareInitialScheduleRejects(t *testing.T) {
	tests := []struct {
		name string
		root *ScheduleRoot
		rev  func() *ScheduleRevision
	}{
		{name: "nil root", root: nil, rev: testRevision},
		{name: "no root id", root: &ScheduleRoot{TeamID: "devops"}, rev: testRevision},
		{name: "no team id", root: &ScheduleRoot{ID: "sched-1"}, rev: testRevision},
		{
			// The reason the two implementations must share this: an ID source
			// that runs dry yields a revision the fake once stored happily and
			// PostgreSQL always rejected.
			name: "revision without an id",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev:  func() *ScheduleRevision { r := testRevision(); r.ID = ""; return r },
		},
		{
			name: "revision of another schedule",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev:  func() *ScheduleRevision { r := testRevision(); r.ScheduleID = "sched-2"; return r },
		},
		{
			name: "not version one",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev:  func() *ScheduleRevision { r := testRevision(); r.Version = 2; return r },
		},
		{
			name: "already closed",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev: func() *ScheduleRevision {
				r := testRevision()
				to := r.EffectiveFrom.Add(time.Hour)
				r.EffectiveTo = &to
				return r
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := PrepareInitialSchedule(tc.root, tc.rev()); !errors.Is(err, ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
		})
	}
}

func testOverride() *OverrideRevision {
	from := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	return &OverrideRevision{
		OverrideID: "ovr-1",
		ScheduleID: "sched-1",
		Revision:   1,
		UserID:     "alice",
		ValidFrom:  from,
		ValidTo:    from.Add(time.Hour),
	}
}

func TestPrepareOverrideRevisionDefaultsAndRejects(t *testing.T) {
	rev := testOverride()
	if err := PrepareOverrideRevision(rev); err != nil {
		t.Fatalf("PrepareOverrideRevision: %v", err)
	}
	if rev.RevisionID == "" {
		t.Fatal("revision id was not generated")
	}
	if rev.Layer != LayerL1 {
		t.Fatalf("layer = %q, want the l1 default", rev.Layer)
	}
	if rev.RecordedAt.IsZero() {
		t.Fatal("recorded_at was left zero")
	}

	tests := []struct {
		name   string
		mutate func(*OverrideRevision)
	}{
		{name: "no override id", mutate: func(r *OverrideRevision) { r.OverrideID = "" }},
		{name: "no schedule id", mutate: func(r *OverrideRevision) { r.ScheduleID = "" }},
		{name: "no user id", mutate: func(r *OverrideRevision) { r.UserID = "" }},
		{name: "zero revision", mutate: func(r *OverrideRevision) { r.Revision = 0 }},
		{name: "unknown layer", mutate: func(r *OverrideRevision) { r.Layer = "l3" }},
		{name: "inverted interval", mutate: func(r *OverrideRevision) { r.ValidTo = r.ValidFrom.Add(-time.Hour) }},
		{name: "zero-length interval", mutate: func(r *OverrideRevision) { r.ValidTo = r.ValidFrom }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rev := testOverride()
			tc.mutate(rev)
			if err := PrepareOverrideRevision(rev); !errors.Is(err, ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
		})
	}
	if err := PrepareOverrideRevision(nil); !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("nil override error = %v, want ErrInvariantViolation", err)
	}
}

func TestPrepareScheduleEventDefaultsAndRejects(t *testing.T) {
	event := &ScheduleEvent{ScheduleID: "sched-1", EventType: "schedule.changed"}
	if err := PrepareScheduleEvent(event); err != nil {
		t.Fatalf("PrepareScheduleEvent: %v", err)
	}
	if event.ID == "" {
		t.Fatal("event id was not generated")
	}
	if string(event.Payload) != "{}" {
		t.Fatalf("payload = %q, want the empty object default", event.Payload)
	}
	if event.RecordedAt.IsZero() {
		t.Fatal("recorded_at was left zero")
	}

	tests := []struct {
		name  string
		event *ScheduleEvent
	}{
		{name: "nil", event: nil},
		{name: "no schedule id", event: &ScheduleEvent{EventType: "schedule.changed"}},
		{name: "no type", event: &ScheduleEvent{ScheduleID: "sched-1"}},
		{name: "malformed payload", event: &ScheduleEvent{
			ScheduleID: "sched-1", EventType: "schedule.changed", Payload: []byte(`{"broken"`),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := PrepareScheduleEvent(tc.event); !errors.Is(err, ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
		})
	}
}
