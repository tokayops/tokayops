package handoff

import (
	"context"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/schedulerender"
)

// The projection as these tests drive it: one schedule per tick, described by
// hand, with no database and no revisions behind it.

// fakeOnCall is an onCallLister a test drives tick by tick.
//
// The runtime consumers depend on this narrow interface rather than on the
// renderer so that their unit tests need neither PostgreSQL nor revisions in
// MockStore.
//
// It is guarded because the real thing is: a syncer manager can leave two
// syncers alive at once, and each runs its own goroutine against this fake.
// An unsynchronised double would report that as a data race in the test rather
// than in the code under test.
type fakeOnCall struct {
	mu sync.Mutex

	// bulk is what the next tick observes.
	bulk schedulerender.BulkOnCall

	// err, when set, is a failure of the call itself: nothing was read.
	err error

	// calls counts ticks, so a test can prove a tick happened at all.
	calls int
}

func (f *fakeOnCall) CurrentOnCallForAllNow(ctx context.Context) (schedulerender.BulkOnCall, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return schedulerender.BulkOnCall{}, f.err
	}
	return f.bulk, nil
}

// set replaces what the projection reports from the next tick on.
func (f *fakeOnCall) set(schedules ...schedulerender.ScheduleOnCall) {
	f.setBulk(schedulerender.BulkOnCall{Schedules: schedules})
}

// setBulk is the same for a projection that also reports failures.
func (f *fakeOnCall) setBulk(bulk schedulerender.BulkOnCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bulk = bulk
}

// fail makes the next tick a failure of the call itself; nil clears it.
func (f *fakeOnCall) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeOnCall) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

	// revisionID is the version of the configuration that put this composition
	// on duty - the override revision for an override, the schedule revision
	// otherwise. It defaults to a constant, so an unchanged configuration
	// observed twice really is unchanged.
	revisionID string
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
	revisionID := spec.revisionID
	if revisionID == "" {
		revisionID = "rev-1"
	}
	sc.OnCall.L1 = &schedulerender.LayerOnCall{
		GroupID:            spec.groupID,
		UserIDs:            spec.users,
		Source:             spec.source,
		GridSlotStart:      slotStart,
		GridSlotEnd:        slotStart.Add(24 * time.Hour),
		AssignmentStart:    start,
		AssignmentEnd:      end,
		ScheduleRevisionID: revisionID,
	}
	if spec.source == schedulerender.SourceOverride {
		sc.OnCall.L1.OverrideID = spec.groupID
		sc.OnCall.L1.OverrideRevisionID = revisionID
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
