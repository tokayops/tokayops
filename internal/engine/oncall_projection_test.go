package engine

import (
	"context"
	"time"

	"github.com/tokayops/tokayops/internal/schedulerender"
)

// fakeProjection is the on-call projection a test seeds directly. The engine and
// the escalation builder share it, which is the point: the snapshot stored on an
// alert group and the users escalated to must be one answer.
//
// A team that was not seeded answers with the zero TeamOnCall - an empty
// ScheduleID, exactly how a team with no schedule is reported.
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

func layer(groupID, source string, users ...string) *schedulerender.LayerOnCall {
	return &schedulerender.LayerOnCall{
		GroupID:         groupID,
		UserIDs:         users,
		Source:          source,
		GridSlotStart:   projectionBase,
		GridSlotEnd:     projectionBase.Add(24 * time.Hour),
		AssignmentStart: projectionBase,
		AssignmentEnd:   projectionBase.Add(24 * time.Hour),
	}
}

func onDuty(groupID string, users ...string) schedulerender.OnCall {
	return schedulerender.OnCall{At: projectionBase, L1: layer(groupID, schedulerender.SourceRotation, users...)}
}

func onDutyByOverride(overrideID string, users ...string) schedulerender.OnCall {
	oc := schedulerender.OnCall{At: projectionBase, L1: layer(overrideID, schedulerender.SourceOverride, users...)}
	oc.L1.OverrideID = overrideID
	return oc
}

func teamSchedule(scheduleID string, onCall schedulerender.OnCall) schedulerender.TeamOnCall {
	return schedulerender.TeamOnCall{ScheduleID: scheduleID, OnCall: onCall}
}
