package builders

import (
	"context"
	"time"

	"github.com/tokayops/tokayops/internal/schedulerender"
)

// fakeProjection is an OnCallProjection a test seeds directly.
//
// The builder depends on this narrow interface rather than on the renderer for
// one concrete reason: the revision model is deliberately absent from the legacy
// MockStore, so seeding on-call state through the store is not possible and
// these tests would otherwise need PostgreSQL.
//
// A team that was not seeded answers with the zero TeamOnCall - an empty
// ScheduleID, which is exactly how the projection reports a team with no
// schedule in this model.
type fakeProjection struct {
	teams     map[string]schedulerender.TeamOnCall
	schedules map[string]schedulerender.OnCall
	err       error
}

func (f *fakeProjection) CurrentTeamOnCallNow(ctx context.Context, teamID string) (schedulerender.TeamOnCall, error) {
	if f.err != nil {
		return schedulerender.TeamOnCall{}, f.err
	}
	return f.teams[teamID], nil
}

func (f *fakeProjection) CurrentOnCallNow(ctx context.Context, scheduleID string) (schedulerender.OnCall, error) {
	if f.err != nil {
		return schedulerender.OnCall{}, f.err
	}
	return f.schedules[scheduleID], nil
}

var projectionBase = time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)

// onDuty is a projected L1 assignment for a rotation group.
func onDuty(groupID string, users ...string) schedulerender.OnCall {
	return schedulerender.OnCall{
		At: projectionBase,
		L1: &schedulerender.LayerOnCall{
			GroupID:         groupID,
			UserIDs:         users,
			Source:          schedulerender.SourceRotation,
			GridSlotStart:   projectionBase,
			GridSlotEnd:     projectionBase.Add(24 * time.Hour),
			AssignmentStart: projectionBase,
			AssignmentEnd:   projectionBase.Add(24 * time.Hour),
		},
	}
}

// onDutyByOverride is the same for a stand-in named by an override. The
// projection has already overlaid it onto the layer, which is why the builder
// has no override branch of its own.
func onDutyByOverride(overrideID string, users ...string) schedulerender.OnCall {
	oc := onDuty(overrideID, users...)
	oc.L1.Source = schedulerender.SourceOverride
	oc.L1.OverrideID = overrideID
	return oc
}

// nobodyOnDuty is a schedule that exists with no assignment at this instant.
func nobodyOnDuty() schedulerender.OnCall {
	return schedulerender.OnCall{At: projectionBase}
}

// teamSchedule is a team whose schedule answers for it.
func teamSchedule(scheduleID string, onCall schedulerender.OnCall) schedulerender.TeamOnCall {
	return schedulerender.TeamOnCall{ScheduleID: scheduleID, OnCall: onCall}
}

// deletedTeamSchedule is a team whose schedule is soft-deleted: it still answers
// for the team, and its answer is nobody.
func deletedTeamSchedule(scheduleID string) schedulerender.TeamOnCall {
	deletedAt := projectionBase
	return schedulerender.TeamOnCall{
		ScheduleID: scheduleID,
		DeletedAt:  &deletedAt,
		OnCall:     nobodyOnDuty(),
	}
}
