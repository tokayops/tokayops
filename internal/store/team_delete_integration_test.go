package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// These run against the real database because what they are about lives there:
// which foreign key fires, in what order, and what a cascade takes with it. A
// fake can be told to refuse; only Postgres can be wrong about it.

func TestDeleteTeamRefusesTeamScopedIntegrationWithTypedError(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice")

	// Straight INSERT rather than CreateIntegration: what holds the team is the
	// foreign key, and going through the store would drag in config encryption
	// and an ENCRYPTION_KEY this test has no opinion about.
	if _, err := s.db.Exec(
		`INSERT INTO integrations (id, type, direction, name, enabled, scope, team_id, config)
		 VALUES ($1, 'generic_webhook', 'outbound', 'team hook', true, 'team', $2, '')`,
		"int-team-hook", "devops"); err != nil {
		t.Fatalf("seed team-scoped integration: %v", err)
	}

	err := newTestScheduleService(s, time.Now().UTC()).DeleteTeam(context.Background(), "devops")
	if !errors.Is(err, scheduleconfig.ErrTeamHasIntegrations) {
		t.Fatalf("error = %v, want ErrTeamHasIntegrations", err)
	}
	// The point of mapping it: without one, this reaches the client as the
	// text of integrations_team_id_fkey inside a 500.
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		t.Fatalf("a raw SQL error leaked through the contract: %v", pqErr)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams WHERE id = 'devops'`); n != 1 {
		t.Fatal("a refused delete rolled back nothing: the team is gone")
	}
}

// The reason the schedule blocker is a read and not a constraint: with the
// cascade from teams, an uninitialized row has no revisions to protect it, so
// the constraint would never fire and the row would vanish.
func TestDeleteTeamRefusesUninitializedScheduleRatherThanCascadingIt(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice")

	// The shape a skipped upgrade reset leaves behind: a schedule row with no
	// history horizon and no revision chain.
	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, team_id, config_version) VALUES ($1, $2, 0)`,
		"legacy-1", "devops"); err != nil {
		t.Fatalf("seed pre-revision schedule: %v", err)
	}

	err := newTestScheduleService(s, time.Now().UTC()).DeleteTeam(context.Background(), "devops")
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("error = %v, want ErrInvariantViolation", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE id = 'legacy-1'`); n != 1 {
		t.Fatal("the pre-revision schedule was destroyed by the cascade - silently, which is the whole defect")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams WHERE id = 'devops'`); n != 1 {
		t.Fatal("the team was deleted despite the refusal")
	}
}

func TestDeleteTeamTakesMembershipsAndLeavesHistoryAlone(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	if err := newTestScheduleService(s, time.Now().UTC()).
		DeleteTeam(context.Background(), "devops"); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	// team_members -> teams has no ON DELETE action, so these rows only go if
	// the command takes them.
	if n := countRows(t, s, `SELECT COUNT(*) FROM team_members WHERE team_id = 'devops'`); n != 0 {
		t.Fatalf("%d memberships outlived the team", n)
	}
}

func TestDeleteTeamWithScheduleHistoryIsTypedNotAConstraintError(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	svc := newTestScheduleService(s, now)
	if _, err := svc.CreateSchedule(context.Background(), "devops", revTestConfig(), "alice", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	err := svc.DeleteTeam(context.Background(), "devops")
	var retained *scheduleconfig.TeamHasScheduleHistoryError
	if !errors.As(err, &retained) {
		t.Fatalf("error = %v, want TeamHasScheduleHistoryError", err)
	}
	if retained.ScheduleID == "" {
		t.Error("the refusal must name the schedule that retains the team")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("history was touched: %d revisions", n)
	}
}

// LockTeam is what serializes team deletion against anything that would give
// the team a new child row, and this is the mechanism itself rather than its
// consequences: inserting a row that references a team takes FOR KEY SHARE on
// the parent, and FOR UPDATE conflicts with it.
//
// Tested directly because the end-to-end race below cannot prove it - the
// delete is two statements and a create is a whole pipeline, so the delete
// wins every time on speed alone and the contended window never opens by
// chance. Without this test the lock could be deleted and every other test
// here would still pass.
func TestLockTeamBlocksConcurrentScheduleInsert(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice")

	locked := make(chan struct{})
	released := make(chan struct{})
	inserted := make(chan time.Time, 1)

	go func() {
		<-locked
		// A bare INSERT, not the service: what is under test is the lock the
		// foreign key takes on the parent row, with nothing else in the way.
		_, err := s.db.Exec(
			`INSERT INTO schedules (id, team_id, config_version) VALUES ($1, $2, 0)`,
			"sched-race", "devops")
		if err != nil {
			t.Errorf("insert: %v", err)
		}
		inserted <- time.Now()
	}()

	var releasedAt time.Time
	err := s.ScheduleConfigRepository().WithinTx(context.Background(),
		func(tx scheduleconfig.ScheduleConfigTx) error {
			if err := tx.LockTeam(context.Background(), "devops"); err != nil {
				return err
			}
			close(locked)
			// Long enough that an unblocked insert would land first by a wide
			// margin, so the assertion is not measuring scheduler noise.
			time.Sleep(200 * time.Millisecond)
			releasedAt = time.Now()
			close(released)
			return nil
		})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}

	<-released
	insertedAt := <-inserted
	if insertedAt.Before(releasedAt) {
		t.Fatalf("the insert went through while the team row was locked (%v before release)",
			releasedAt.Sub(insertedAt))
	}
}

// Both interleavings of "create the first schedule" and "delete the team",
// under real concurrency.
//
// What this pins is the outcome contract: exactly one wins, the loser gets a
// typed error, and nothing answers with the text of a constraint. That the
// lock is what buys it is the test above.
func TestDeleteTeamRacesFirstSaveDeterministically(t *testing.T) {
	s := setupTestDB(t)
	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	svc := newTestScheduleService(s, now)

	// Both goroutines released at once, on a fresh team each round.
	//
	// In practice the delete wins every round: it is two statements against a
	// whole create pipeline. So this does NOT demonstrate the lock - removing
	// LockTeam leaves it green, which is why the test above exists. What it
	// does hold is the outcome contract under real concurrency, including that
	// the losing create is answered in the contract's vocabulary rather than
	// with a constraint name.
	const rounds = 25
	for i := 0; i < rounds; i++ {
		teamID := fmt.Sprintf("devops-%d", i)
		seedTeam(t, s, teamID, "alice", "bob")

		var wg sync.WaitGroup
		var createErr, deleteErr error
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			deleteErr = svc.DeleteTeam(context.Background(), teamID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, createErr = svc.CreateSchedule(context.Background(), teamID, revTestConfig(), "alice", nil)
		}()
		close(start)
		wg.Wait()

		// Exactly one of them wins, and the loser says so in the contract's
		// own vocabulary.
		switch {
		case deleteErr == nil:
			// Two typed answers, one per sub-interleaving, and both are
			// legitimate readings of "the team is gone":
			//
			//   - the delete committed before the create read membership:
			//     memberships went with the team, so validation refuses first
			//     and says the users are not members;
			//   - it committed after that read but before the insert: the
			//     foreign key fires and the store maps it to ErrTeamNotFound.
			//
			// The second is why the foreign key mapping stays a backstop even
			// though the lock is what makes the common case deterministic.
			if !errors.Is(createErr, scheduleconfig.ErrUserNotTeamMember) &&
				!errors.Is(createErr, scheduleconfig.ErrTeamNotFound) {
				t.Fatalf("round %d: team deleted, so the create must lose with a typed error, got %v", i, createErr)
			}
		case createErr == nil:
			var retained *scheduleconfig.TeamHasScheduleHistoryError
			if !errors.As(deleteErr, &retained) {
				t.Fatalf("round %d: schedule created, so the delete must lose with TeamHasScheduleHistoryError, got %v", i, deleteErr)
			}
		default:
			t.Fatalf("round %d: both failed: create=%v delete=%v", i, createErr, deleteErr)
		}

		// Whatever the order, nothing leaked a driver error - that is the 500
		// this whole story is about.
		for _, err := range []error{createErr, deleteErr} {
			var pqErr *pq.Error
			if errors.As(err, &pqErr) {
				t.Fatalf("round %d: a raw SQL error escaped: %v", i, pqErr)
			}
		}
	}
}
