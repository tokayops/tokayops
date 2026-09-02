package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

// The metrics snapshot runs ten statements, and the scrape's deadline has to
// reach every one of them. A deadline that only the first statement honours is
// a database holding the connection pool in the name of observability: the
// scrape Prometheus gave up on keeps running, the next scrape starts another,
// and API and workers queue behind them.
//
// The proof is per statement, through the seam: a table lock taken in another
// transaction just BEFORE step N, after steps before it have run, so that the
// statement that hangs is exactly step N. A lock taken in advance would stop
// the first statement on that table instead, and a step reading the same table
// later would never be reached - which is why several steps share a table and
// the test does not.

// hold takes an ACCESS EXCLUSIVE lock on a table in its own transaction, which
// blocks every SELECT on it until the transaction ends.
func hold(t *testing.T, s *Store, table string) *sql.Tx {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin the holding transaction: %v", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE", table)); err != nil {
		tx.Rollback()
		t.Fatalf("lock %s: %v", table, err)
	}
	return tx
}

// snapshotUnder runs the snapshot with the given deadline in a goroutine and
// waits for it a bounded time, so that a statement which ignores its context
// fails the test instead of hanging it.
func snapshotUnder(t *testing.T, s *Store, deadline time.Duration) (error, time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := s.GetMetricsSnapshot(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		return err, time.Since(started)
	case <-time.After(deadline + 5*time.Second):
		return nil, time.Since(started)
	}
}

func TestEveryStepOfTheSnapshotHonoursTheDeadline(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { snapshotSeam = nil })

	for n := 1; n <= len(snapshotSteps); n++ {
		step := snapshotSteps[n-1]
		t.Run(fmt.Sprintf("step %d: %s", n, step.name), func(t *testing.T) {
			var holder *sql.Tx
			snapshotSeam = func(reached int) {
				if reached == n {
					holder = hold(t, s, step.table)
				}
			}
			t.Cleanup(func() {
				if holder != nil {
					holder.Rollback()
				}
			})

			err, elapsed := snapshotUnder(t, s, 1500*time.Millisecond)
			if holder == nil {
				t.Fatalf("the snapshot never reached step %d", n)
			}
			if err == nil && elapsed > 1500*time.Millisecond {
				t.Fatalf("step %d ignored its deadline: still running after %s", n, elapsed)
			}
			if err == nil {
				t.Fatalf("step %d answered under a lock that should have blocked it", n)
			}
			if elapsed > 4*time.Second {
				t.Fatalf("step %d took %s to give up on a 1.5s deadline", n, elapsed)
			}
		})
	}
}

// TestACancelledSnapshotGivesItsConnectionBack: cancelling the statement is
// only half of it - the connection has to come back to the pool, or ten
// abandoned scrapes are ten connections nobody will ever get. With the pool
// capped at two, one held by the lock and one by each cancelled snapshot, a
// leak shows up as the second iteration failing to begin at all; and the
// ordinary query at the end must get a connection at once.
func TestACancelledSnapshotGivesItsConnectionBack(t *testing.T) {
	s := setupTestDB(t)
	t.Cleanup(func() { snapshotSeam = nil })
	s.db.SetMaxOpenConns(2)
	t.Cleanup(func() { s.db.SetMaxOpenConns(0) })

	for i := 0; i < 10; i++ {
		var holder *sql.Tx
		snapshotSeam = func(reached int) {
			if reached == 7 {
				holder = hold(t, s, "outbound_intents")
			}
		}
		err, elapsed := snapshotUnder(t, s, 500*time.Millisecond)
		if holder == nil {
			t.Fatalf("iteration %d: the snapshot never reached the locked step (a connection leaked?)", i)
		}
		if err == nil {
			t.Fatalf("iteration %d: the locked step answered", i)
		}
		if elapsed > 4*time.Second {
			t.Fatalf("iteration %d: %s to give up", i, elapsed)
		}
		holder.Rollback()
	}
	snapshotSeam = nil

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var one int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("an ordinary query after ten cancelled snapshots: %v", err)
	}
	if _, err := s.GetMetricsSnapshot(ctx); err != nil {
		t.Fatalf("a snapshot after ten cancelled ones: %v", err)
	}
}
