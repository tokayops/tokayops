package scheduleconfig

import (
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
)

const (
	testGroupA = "9b0a1e2c-3333-4a3b-8c4d-000000000001"
	testGroupB = "9b0a1e2c-3333-4a3b-8c4d-000000000002"
)

var testEffectiveAt = time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)

// testSnapshot builds a valid snapshot through the planner rather than by
// hand, so the phase anchor is guaranteed to sit on a real grid boundary.
// L2 is left disabled, which is what makes its Groups nil - the exact shape
// storage coerces to [].
func testSnapshot(t *testing.T) rotation.ScheduleRevisionSnapshot {
	t.Helper()
	monday := 1
	weekly := rotation.RotationPolicy{
		SchemaVersion: rotation.PolicySchemaVersion,
		Cadence:       model.RotationWeekly,
		HandoffTime:   "11:00",
		HandoffDay:    &monday,
	}
	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Desired: rotation.ScheduleConfiguration{
			Timezone: "Europe/Amsterdam",
			L1: rotation.LayerConfiguration{
				Enabled: true,
				Policy:  weekly,
				Groups: []rotation.RotationGroup{
					{ID: testGroupA, Members: []string{"alice"}},
					{ID: testGroupB, Members: []string{"bob"}},
				},
			},
			L2:                      rotation.LayerConfiguration{Enabled: false, Policy: weekly},
			L2EscalationTimeoutMins: 5,
		},
		EffectiveAt: testEffectiveAt,
	})
	if err != nil {
		t.Fatalf("PlanTransition: %v", err)
	}
	return plan.Snapshot
}

func testRevision(t *testing.T) *ScheduleRevision {
	t.Helper()
	return &ScheduleRevision{
		ID:            "rev-1",
		ScheduleID:    "sched-1",
		Version:       1,
		Snapshot:      testSnapshot(t),
		EffectiveFrom: testEffectiveAt,
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
		{name: "unknown kind", mutate: func(r *ScheduleRevision) { r.Kind = "archived" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rev := testRevision(t)
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

func TestPrepareRevisionDefaultsKind(t *testing.T) {
	rev := testRevision(t)
	if err := PrepareRevision(rev); err != nil {
		t.Fatalf("PrepareRevision: %v", err)
	}
	if rev.Kind != RevisionActive {
		t.Fatalf("kind = %q, want %q", rev.Kind, RevisionActive)
	}

	// A deleted revision is a legitimate value on the plain path: only the
	// initial revision of a schedule is required to be active.
	rev = testRevision(t)
	rev.Kind = RevisionDeleted
	if err := PrepareRevision(rev); err != nil {
		t.Fatalf("PrepareRevision(deleted): %v", err)
	}
	if rev.Kind != RevisionDeleted {
		t.Fatalf("kind = %q, want %q", rev.Kind, RevisionDeleted)
	}
}

func TestPrepareRevisionNormalizesTimestamps(t *testing.T) {
	rev := testRevision(t)
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

// Storage does not store a snapshot verbatim: it coerces nil group slices to
// [] and anchors to UTC. PrepareRevision applies that transformation to the
// caller's revision, so whoever holds it afterwards holds what a read will
// return - and the fake and the database agree by construction rather than by
// each remembering to do the same thing.
func TestPrepareRevisionCanonicalizesSnapshot(t *testing.T) {
	rev := testRevision(t)
	if rev.Snapshot.L2.Groups != nil {
		t.Fatal("fixture no longer exercises the nil-groups case")
	}
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	local := rev.Snapshot.L1.PhaseAnchorSlotStart.In(amsterdam)
	rev.Snapshot.L1.PhaseAnchorSlotStart = &local

	if err := PrepareRevision(rev); err != nil {
		t.Fatalf("PrepareRevision: %v", err)
	}

	if rev.Snapshot.L2.Groups == nil {
		t.Fatal("nil group slice was not coerced to an empty slice")
	}
	if len(rev.Snapshot.L2.Groups) != 0 {
		t.Fatalf("empty layer gained %d groups", len(rev.Snapshot.L2.Groups))
	}
	if loc := rev.Snapshot.L1.PhaseAnchorSlotStart.Location(); loc != time.UTC {
		t.Fatalf("anchor location = %v, want UTC", loc)
	}
	if !rev.Snapshot.L1.PhaseAnchorSlotStart.Equal(local) {
		t.Fatal("canonicalizing the anchor moved the instant")
	}

	// Canonical form is a fixed point: preparing again changes nothing.
	before, err := rotation.EncodeSnapshot(rev.Snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if err := PrepareRevision(rev); err != nil {
		t.Fatalf("second PrepareRevision: %v", err)
	}
	after, err := rotation.EncodeSnapshot(rev.Snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("canonicalization is not idempotent:\n%s\n%s", before, after)
	}
}

func TestPrepareRevisionRejectsInvalidSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*rotation.ScheduleRevisionSnapshot)
	}{
		{
			name:   "zero snapshot",
			mutate: func(s *rotation.ScheduleRevisionSnapshot) { *s = rotation.ScheduleRevisionSnapshot{} },
		},
		{
			name:   "unknown timezone",
			mutate: func(s *rotation.ScheduleRevisionSnapshot) { s.Timezone = "Mars/Olympus" },
		},
		{
			name: "active layer without a phase pair",
			mutate: func(s *rotation.ScheduleRevisionSnapshot) {
				s.L1.PhaseAnchorSlotStart = nil
				s.L1.StartPosition = nil
			},
		},
		{
			name: "anchor off the grid",
			mutate: func(s *rotation.ScheduleRevisionSnapshot) {
				shifted := s.L1.PhaseAnchorSlotStart.Add(time.Minute)
				s.L1.PhaseAnchorSlotStart = &shifted
			},
		},
		{
			name: "start position out of range",
			mutate: func(s *rotation.ScheduleRevisionSnapshot) {
				pos := len(s.L1.Groups)
				s.L1.StartPosition = &pos
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rev := testRevision(t)
			tc.mutate(&rev.Snapshot)
			if err := PrepareRevision(rev); !errors.Is(err, ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
		})
	}
}

func TestPrepareInitialSchedule(t *testing.T) {
	root := &ScheduleRoot{ID: "sched-1", TeamID: "devops"}
	rev := testRevision(t)

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
		rev  func(*testing.T) *ScheduleRevision
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
			rev:  func(t *testing.T) *ScheduleRevision { r := testRevision(t); r.ID = ""; return r },
		},
		{
			name: "revision of another schedule",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev:  func(t *testing.T) *ScheduleRevision { r := testRevision(t); r.ScheduleID = "sched-2"; return r },
		},
		{
			name: "not version one",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev:  func(t *testing.T) *ScheduleRevision { r := testRevision(t); r.Version = 2; return r },
		},
		{
			name: "already closed",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev: func(t *testing.T) *ScheduleRevision {
				r := testRevision(t)
				to := r.EffectiveFrom.Add(time.Hour)
				r.EffectiveTo = &to
				return r
			},
		},
		{
			// A schedule cannot begin its history already deleted.
			name: "deleted kind",
			root: &ScheduleRoot{ID: "sched-1", TeamID: "devops"},
			rev:  func(t *testing.T) *ScheduleRevision { r := testRevision(t); r.Kind = RevisionDeleted; return r },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := PrepareInitialSchedule(tc.root, tc.rev(t)); !errors.Is(err, ErrInvariantViolation) {
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
