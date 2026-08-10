package dispatcher

import (
	"context"
	"time"

	"github.com/tokayops/tokayops/internal/schedulerender"
)

// fakeOnCall is an onCallLister a test drives tick by tick.
//
// The runtime consumers depend on this narrow interface rather than on the
// renderer so that their unit tests need neither PostgreSQL nor revisions in
// the legacy MockStore.
type fakeOnCall struct {
	// bulk is what the next tick observes.
	bulk schedulerender.BulkOnCall

	// err, when set, is a failure of the call itself: nothing was read.
	err error

	// calls counts ticks, so a test can prove a tick happened at all.
	calls int
}

func (f *fakeOnCall) CurrentOnCallForAllNow(ctx context.Context) (schedulerender.BulkOnCall, error) {
	f.calls++
	if f.err != nil {
		return schedulerender.BulkOnCall{}, f.err
	}
	return f.bulk, nil
}

// set replaces what the projection reports from the next tick on.
func (f *fakeOnCall) set(schedules ...schedulerender.ScheduleOnCall) {
	f.bulk = schedulerender.BulkOnCall{Schedules: schedules}
}

// fakeTeamOnCall answers the single-schedule side of the projection, which is
// what the escalation builder reads. Anything not seeded answers "no schedule",
// the same as a team that never configured one.
type fakeTeamOnCall struct {
	teams     map[string]schedulerender.TeamOnCall
	schedules map[string]schedulerender.OnCall
	err       error
}

func (f *fakeTeamOnCall) CurrentTeamOnCallNow(ctx context.Context, teamID string) (schedulerender.TeamOnCall, error) {
	if f.err != nil {
		return schedulerender.TeamOnCall{}, f.err
	}
	return f.teams[teamID], nil
}

func (f *fakeTeamOnCall) CurrentOnCallNow(ctx context.Context, scheduleID string) (schedulerender.OnCall, error) {
	if f.err != nil {
		return schedulerender.OnCall{}, f.err
	}
	return f.schedules[scheduleID], nil
}

// dutySpec describes one schedule as the projection would report it. The zero
// source means nobody is on duty - a deleted schedule, a layer switched off, a
// rotation with no groups.
type dutySpec struct {
	scheduleID string
	teamID     string
	timezone   string
	usergroup  string
	deletedAt  *time.Time

	source    string
	groupID   string
	users     []string
	slotStart time.Time
	start     time.Time
	end       time.Time
}

// dutyBase is the shift boundary the specs default to: a fixed instant, because
// a detector that compares compositions must not depend on the wall clock.
var dutyBase = time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)

func duty(spec dutySpec) schedulerender.ScheduleOnCall {
	if spec.teamID == "" {
		spec.teamID = "team-1"
	}
	if spec.timezone == "" {
		spec.timezone = "UTC"
	}
	sc := schedulerender.ScheduleOnCall{
		ScheduleID:       spec.scheduleID,
		TeamID:           spec.teamID,
		Timezone:         spec.timezone,
		SlackUsergroupID: spec.usergroup,
		DeletedAt:        spec.deletedAt,
		OnCall:           schedulerender.OnCall{At: dutyBase},
	}
	if spec.source == "" {
		return sc
	}

	slotStart := spec.slotStart
	if slotStart.IsZero() {
		slotStart = dutyBase
	}
	start := spec.start
	if start.IsZero() {
		start = slotStart
	}
	end := spec.end
	if end.IsZero() {
		end = slotStart.Add(24 * time.Hour)
	}
	sc.OnCall.L1 = &schedulerender.LayerOnCall{
		GroupID:         spec.groupID,
		UserIDs:         spec.users,
		Source:          spec.source,
		GridSlotStart:   slotStart,
		GridSlotEnd:     slotStart.Add(24 * time.Hour),
		AssignmentStart: start,
		AssignmentEnd:   end,
	}
	if spec.source == schedulerender.SourceOverride {
		sc.OnCall.L1.OverrideID = spec.groupID
	}
	return sc
}

// rotationDuty is the common case: one rotation group on duty for its slot.
func rotationDuty(scheduleID, groupID string, users ...string) schedulerender.ScheduleOnCall {
	return duty(dutySpec{
		scheduleID: scheduleID,
		source:     schedulerender.SourceRotation,
		groupID:    groupID,
		users:      users,
	})
}

// overrideDuty is a stand-in named by an override, whose assignment starts
// inside the slot rather than at its boundary.
func overrideDuty(scheduleID, overrideID string, users ...string) schedulerender.ScheduleOnCall {
	return duty(dutySpec{
		scheduleID: scheduleID,
		source:     schedulerender.SourceOverride,
		groupID:    overrideID,
		users:      users,
		slotStart:  dutyBase,
		start:      dutyBase.Add(3 * time.Hour),
		end:        dutyBase.Add(7 * time.Hour),
	})
}

// emptyDuty is a schedule with nobody on call.
func emptyDuty(scheduleID string) schedulerender.ScheduleOnCall {
	return duty(dutySpec{scheduleID: scheduleID})
}
