package erasure_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
)

var erasedAt = time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)

func newEraser(t *testing.T) (*erasure.Service, *fakes.ErasureRepo) {
	t.Helper()
	repo := fakes.NewErasureRepo()
	repo.AddUser("alice", string(model.UserRoleUser))
	repo.AddUser("root", string(model.UserRoleAdmin))
	svc := erasure.NewService(repo, erasure.WithClock(func() time.Time { return erasedAt }))
	return svc, repo
}

// tailNaming builds a live schedule whose rotation includes the named user.
func tailNaming(scheduleID, teamID string, members ...string) erasure.ScheduleTail {
	return erasure.ScheduleTail{
		ScheduleID: scheduleID,
		TeamID:     teamID,
		Snapshot: rotation.ScheduleRevisionSnapshot{
			L1: rotation.RotationLayerSnapshot{
				Groups: []rotation.RotationGroup{{ID: "g1", Members: members}},
			},
		},
	}
}

func TestEraseBlockedWhenUserInActiveTail(t *testing.T) {
	svc, repo := newEraser(t)
	repo.Tails = []erasure.ScheduleTail{
		tailNaming("sched-1", "devops", "alice", "bob"),
		tailNaming("sched-2", "platform", "bob"),
	}

	err := svc.Erase(context.Background(), "alice")
	var onCall *erasure.UserOnCallError
	if !errors.As(err, &onCall) {
		t.Fatalf("error = %v, want a user-on-call refusal", err)
	}
	if len(onCall.Schedules) != 1 || onCall.Schedules[0].ScheduleID != "sched-1" {
		t.Fatalf("refusal must name the blocking schedule, got %+v", onCall.Schedules)
	}
	if _, erased := repo.DeletedAt("alice"); erased {
		t.Fatal("a refused erasure must not have written anything")
	}
}

// A deleted schedule has nobody on duty. Counting it would make the erasure
// impossible forever on a schedule the team already retired.
func TestEraseAllowsUserOnlyInDeletedScheduleTail(t *testing.T) {
	svc, repo := newEraser(t)
	// The repository only ever returns live schedules; a deleted one simply
	// is not in the list.
	repo.Tails = nil

	if err := svc.Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if _, erased := repo.DeletedAt("alice"); !erased {
		t.Fatal("the user was not erased")
	}
}

// An override can name someone who is in no rotation group at all, so the
// snapshot alone would miss them.
func TestEraseBlockedByOverrideTarget(t *testing.T) {
	for _, tc := range []struct {
		name      string
		validFrom time.Time
		validTo   time.Time
		blocked   bool
	}{
		{"current", erasedAt.Add(-time.Hour), erasedAt.Add(time.Hour), true},
		{"future", erasedAt.Add(24 * time.Hour), erasedAt.Add(25 * time.Hour), true},
		{"expired", erasedAt.Add(-48 * time.Hour), erasedAt.Add(-24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newEraser(t)
			repo.Overrides = []erasure.OverrideAssignment{{
				ScheduleID: "sched-1",
				TeamID:     "devops",
				OverrideID: "ovr-1",
				ValidFrom:  tc.validFrom,
				ValidTo:    tc.validTo,
			}}

			err := svc.Erase(context.Background(), "alice")
			if tc.blocked {
				if !errors.Is(err, erasure.ErrUserOnCall) {
					t.Fatalf("error = %v, want a user-on-call refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("an expired override must not block an erasure: %v", err)
			}
		})
	}
}

// The scan of the schedule tails is the last lock this transaction takes, and
// it waits on any save holding a schedule. An instant captured before it is an
// instant from before somebody else's commit: history would then claim the
// user was gone during a stretch in which the revision in force still had them
// on call.
func TestEraseStampsTimeAfterTheLastLock(t *testing.T) {
	repo := fakes.NewErasureRepo()
	repo.AddUser("alice", string(model.UserRoleUser))

	// The clock records what had already happened when it was read.
	var callsAtClockRead int
	read := false
	svc := erasure.NewService(repo, erasure.WithClock(func() time.Time {
		if !read {
			callsAtClockRead = len(repo.Calls)
			read = true
		}
		return erasedAt
	}))

	if err := svc.Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	scan := -1
	for i, call := range repo.Calls {
		if strings.HasPrefix(call, "ListScheduleTailsLocked") {
			scan = i
			break
		}
	}
	if scan < 0 {
		t.Fatalf("the schedule scan never ran: %v", repo.Calls)
	}
	if callsAtClockRead <= scan {
		t.Fatalf("the clock was read after %d calls, before the scan at %d: %v",
			callsAtClockRead, scan, repo.Calls)
	}
}

func TestEraseBlockedForLastActiveAdmin(t *testing.T) {
	svc, repo := newEraser(t)

	if err := svc.Erase(context.Background(), "root"); !errors.Is(err, erasure.ErrLastAdmin) {
		t.Fatalf("error = %v, want ErrLastAdmin", err)
	}
	if _, erased := repo.DeletedAt("root"); erased {
		t.Fatal("the last admin was erased")
	}
}

func TestEraseAllowsAdminWhenAnotherRemains(t *testing.T) {
	svc, repo := newEraser(t)
	repo.AddUser("second-root", string(model.UserRoleAdmin))

	if err := svc.Erase(context.Background(), "root"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	// And now the remaining one is the last: the count must see that the
	// first admin is gone, or the invariant collapses after one erasure.
	if err := svc.Erase(context.Background(), "second-root"); !errors.Is(err, erasure.ErrLastAdmin) {
		t.Fatalf("error = %v, want ErrLastAdmin for the remaining admin", err)
	}
}

// Advisory lock, then the user row, then the schedules: any other order is
// half of a deadlock with the commands that assign work.
func TestEraseLockOrderAdvisoryThenUserThenGuards(t *testing.T) {
	svc, repo := newEraser(t)
	if err := svc.Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	want := []string{
		"LockAdminLifecycle:",
		"LockUser:alice",
		"ListScheduleTailsLocked:",
		"ListLiveOverrideHeadsForUser:alice",
	}
	if len(repo.Calls) < len(want) {
		t.Fatalf("calls = %v", repo.Calls)
	}
	for i, expected := range want {
		if repo.Calls[i] != expected {
			t.Fatalf("call %d = %q, want %q (all: %v)", i, repo.Calls[i], expected, repo.Calls)
		}
	}
}

// Every source of data about a user has to be reached, in an order where the
// row is marked erased before anything else touches it.
func TestEraseCallsEveryPrimitiveInOrder(t *testing.T) {
	svc, repo := newEraser(t)
	if err := svc.Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	want := []string{
		"SetUserDeletedAt",
		"AnonymizeUser",
		"DeleteUserAPITokens",
		"DeleteUserExternalIdentities",
		"DeleteUserLinkTokens",
		"NullifyOverrideRevisionReasons",
		"NullifyScheduleRevisionChangeReasons",
		"DeleteUserTeamMemberships",
	}
	var got []string
	for _, call := range repo.Calls {
		name := strings.SplitN(call, ":", 2)[0]
		for _, w := range want {
			if name == w {
				got = append(got, name)
				break
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("primitives called = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("primitive %d = %q, want %q", i, got[i], want[i])
		}
	}
	if !repo.Anonymized("alice") {
		t.Fatal("the user was not anonymized")
	}
}

func TestEraseRollsBackOnPrimitiveFailure(t *testing.T) {
	boom := errors.New("injected failure")
	for _, step := range []string{"AnonymizeUser", "DeleteUserLinkTokens", "DeleteUserTeamMemberships"} {
		t.Run(step, func(t *testing.T) {
			svc, repo := newEraser(t)
			repo.FailOn[step] = boom

			if err := svc.Erase(context.Background(), "alice"); !errors.Is(err, boom) {
				t.Fatalf("error = %v, want the injected failure", err)
			}
			if _, erased := repo.DeletedAt("alice"); erased {
				t.Fatal("a partial erasure committed")
			}
			if repo.Anonymized("alice") {
				t.Fatal("anonymization survived the rollback")
			}
		})
	}
}

func TestEraseUnknownUserIsNotFound(t *testing.T) {
	svc, _ := newEraser(t)
	if err := svc.Erase(context.Background(), "nobody"); !errors.Is(err, erasure.ErrUserNotFound) {
		t.Fatalf("error = %v, want ErrUserNotFound", err)
	}
}

// Erasing someone who is already erased is the state the caller asked for, so
// it succeeds without writing again.
func TestEraseIsIdempotent(t *testing.T) {
	svc, repo := newEraser(t)
	if err := svc.Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("first Erase: %v", err)
	}
	before := len(repo.Wiped("alice"))

	if err := svc.Erase(context.Background(), "alice"); err != nil {
		t.Fatalf("second Erase: %v", err)
	}
	if got := len(repo.Wiped("alice")); got != before {
		t.Fatalf("the second erasure repeated %d primitives", got-before)
	}
}
