package schedulerender

import (
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Stable group identities. Rotation group IDs must be canonical UUIDs; user
// IDs are opaque by contract, so plain names are correct here.
const (
	groupA = "aaaaaaaa-1111-4111-8111-000000000001"
	groupB = "bbbbbbbb-1111-4111-8111-000000000002"
	groupC = "cccccccc-1111-4111-8111-000000000003"
)

const testScheduleID = "sched-1"

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func dailyPolicy(handoff string) rotation.RotationPolicy {
	return rotation.RotationPolicy{
		Cadence:       model.RotationDaily,
		HandoffTime:   handoff,
	}
}

func weeklyPolicy(handoff string, day int) rotation.RotationPolicy {
	return rotation.RotationPolicy{
		Cadence:       model.RotationWeekly,
		HandoffTime:   handoff,
		HandoffDay:    &day,
	}
}

func group(id string, members ...string) rotation.RotationGroup {
	return rotation.RotationGroup{ID: id, Members: members}
}

// config builds a schedule configuration with an L1 rotation and no L2.
func config(tz string, policy rotation.RotationPolicy, groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	return rotation.ScheduleConfiguration{
		Timezone: tz,
		L1: rotation.LayerConfiguration{
			Enabled: true,
			Policy:  policy,
			Groups:  groups,
		},
		L2:                      rotation.LayerConfiguration{Enabled: false, Policy: policy},
		L2EscalationTimeoutMins: 5,
	}
}

// snapshotFrom plans the transition from an optional predecessor, which is
// what makes the phase pair of the result honest: a successor revision keeps
// serving the group that was on duty.
func snapshotFrom(t testing.TB, current *rotation.ScheduleRevisionSnapshot,
	cfg rotation.ScheduleConfiguration, at time.Time) rotation.ScheduleRevisionSnapshot {
	t.Helper()
	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Current:     current,
		Desired:     cfg,
		EffectiveAt: at,
	})
	if err != nil {
		t.Fatalf("PlanTransition at %v: %v", at, err)
	}
	if plan.Noop {
		t.Fatalf("PlanTransition at %v is a no-op; the test intends a change", at)
	}
	return plan.Snapshot
}

// revisionChain builds a contiguous chain of revisions from a list of
// configurations, each taking effect at its own instant. The result is what
// the store would return for a range covering all of them.
type revisionStep struct {
	at   time.Time
	cfg  rotation.ScheduleConfiguration
	kind string
}

func chain(t testing.TB, steps ...revisionStep) []scheduleconfig.ScheduleRevision {
	t.Helper()
	var (
		out     []scheduleconfig.ScheduleRevision
		current *rotation.ScheduleRevisionSnapshot
	)
	for i, step := range steps {
		kind := step.kind
		if kind == "" {
			kind = scheduleconfig.RevisionActive
		}

		var snapshot rotation.ScheduleRevisionSnapshot
		if kind == scheduleconfig.RevisionDeleted {
			// A deleted period carries the last valid snapshot; no reader may
			// derive assignments from it.
			if current == nil {
				t.Fatal("a schedule cannot start deleted")
			}
			snapshot = *current
		} else {
			snapshot = snapshotFrom(t, current, step.cfg, step.at)
		}

		rev := scheduleconfig.ScheduleRevision{
			ID:            revisionID(i),
			ScheduleID:    testScheduleID,
			Version:       int64(i + 1),
			Kind:          kind,
			Snapshot:      snapshot,
			EffectiveFrom: step.at,
			RecordedAt:    step.at,
		}
		if i > 0 {
			end := step.at
			out[i-1].EffectiveTo = &end
		}
		out = append(out, rev)

		if kind == scheduleconfig.RevisionActive {
			s := snapshot
			current = &s
		} else {
			current = nil
		}
	}
	return out
}

func revisionID(i int) string {
	return string(rune('a'+i)) + "-rev"
}

func root(historyFrom time.Time) scheduleconfig.ScheduleRoot {
	return scheduleconfig.ScheduleRoot{
		ID:                  testScheduleID,
		TeamID:              "devops",
		ConfigVersion:       1,
		HistoryCompleteFrom: historyFrom,
	}
}

func override(id, layer, user string, from, to time.Time, recordedAt time.Time) scheduleconfig.OverrideRevision {
	return scheduleconfig.OverrideRevision{
		RevisionID: id + "-r1",
		OverrideID: id,
		ScheduleID: testScheduleID,
		Revision:   1,
		Layer:      layer,
		UserID:     user,
		ValidFrom:  from,
		ValidTo:    to,
		RecordedAt: recordedAt,
	}
}

// renderOf runs Render and fails the test on error.
func renderOf(t testing.TB, in Input) Result {
	t.Helper()
	res, err := Render(in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return res
}

// assignmentsOf keeps only the assignments of one layer, in order.
func assignmentsOf(res Result, layer string) []Assignment {
	var out []Assignment
	for _, a := range res.Assignments {
		if a.Layer == layer {
			out = append(out, a)
		}
	}
	return out
}

func warningCodes(res Result) []WarningCode {
	var out []WarningCode
	for _, w := range res.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func hasWarning(res Result, code WarningCode) bool {
	for _, w := range res.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// renderRefuses asserts that a render of damaged data fails with a specific
// sentinel instead of returning a calendar drawn around the damage.
func renderRefuses(t testing.TB, in Input, want error) error {
	t.Helper()
	res, err := Render(in)
	if err == nil {
		t.Fatalf("Render returned a calendar for damaged data: %+v", res)
	}
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	return err
}
