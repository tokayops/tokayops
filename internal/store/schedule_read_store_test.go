package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

func withSnapshot(t *testing.T, s *Store, fn func(scheduleconfig.ScheduleReadView) error) error {
	t.Helper()
	return s.ScheduleReadRepository().WithinSnapshot(context.Background(), fn)
}

func readRevisionsInRange(t *testing.T, s *Store, scheduleID string, from, until time.Time) []scheduleconfig.ScheduleRevision {
	t.Helper()
	var out []scheduleconfig.ScheduleRevision
	err := withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
		var err error
		out, err = view.GetRevisionsInRange(context.Background(), scheduleID, from, until)
		return err
	})
	if err != nil {
		t.Fatalf("GetRevisionsInRange: %v", err)
	}
	return out
}

// TestGetRevisionsInRangeOverlap walks every relative position a revision can
// have against a query range. The predicate is half-open on both ends, so a
// revision that merely touches a boundary is outside.
func TestGetRevisionsInRangeOverlap(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	initial := createSchedule(t, s, "devops", start)

	// Three closed revisions plus the open tail, one day apart.
	second := appendRevision(t, s, initial, start.Add(24*time.Hour))
	third := appendRevision(t, s, second, start.Add(48*time.Hour))

	tests := []struct {
		name        string
		from, until time.Time
		want        []string
	}{
		{
			name: "range inside one revision",
			from: start.Add(2 * time.Hour), until: start.Add(4 * time.Hour),
			want: []string{initial.ID},
		},
		{
			name: "range spanning all of them",
			from: start.Add(-time.Hour), until: start.Add(72 * time.Hour),
			want: []string{initial.ID, second.ID, third.ID},
		},
		{
			name: "range ending exactly at a revision start excludes it",
			from: start, until: start.Add(24 * time.Hour),
			want: []string{initial.ID},
		},
		{
			name: "range starting exactly at a revision start includes it",
			from: start.Add(24 * time.Hour), until: start.Add(30 * time.Hour),
			want: []string{second.ID},
		},
		{
			name: "range entirely before the history",
			from: start.Add(-48 * time.Hour), until: start,
			want: nil,
		},
		{
			name: "range in the far future is served by the open tail",
			from: start.Add(365 * 24 * time.Hour), until: start.Add(366 * 24 * time.Hour),
			want: []string{third.ID},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readRevisionsInRange(t, s, initial.ScheduleID, tc.from, tc.until)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d revisions, want %d", len(got), len(tc.want))
			}
			for i, id := range tc.want {
				if got[i].ID != id {
					t.Fatalf("revision %d = %s, want %s", i, got[i].ID, id)
				}
			}
			for i := 1; i < len(got); i++ {
				if got[i].EffectiveFrom.Before(got[i-1].EffectiveFrom) {
					t.Fatal("revisions are not ordered by effective_from")
				}
			}
		})
	}
}

// TestOverrideProjectionAsOf proves the as-of read and, more importantly, that
// the validity range is applied to the WINNING revision. Filtering inside the
// subquery would let an earlier version whose interval happens to overlap win
// the grouping.
func TestOverrideProjectionAsOf(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	early := start.Add(24 * time.Hour)
	late := start.Add(96 * time.Hour)

	recordedV1 := start.Add(time.Hour)
	recordedV2 := start.Add(2 * time.Hour)

	insert := func(revision int64, userID string, validFrom, validTo, recordedAt time.Time, deleted bool) {
		t.Helper()
		err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
			return tx.InsertOverrideRevision(context.Background(), &scheduleconfig.OverrideRevision{
				OverrideID: "ovr-1",
				ScheduleID: rev.ScheduleID,
				Revision:   revision,
				UserID:     userID,
				ValidFrom:  validFrom,
				ValidTo:    validTo,
				RecordedAt: recordedAt,
				Deleted:    deleted,
			})
		})
		if err != nil {
			t.Fatalf("insert override revision %d: %v", revision, err)
		}
	}

	// v1 covers the early window; v2 moves it to a later one entirely.
	insert(1, "alice", early, early.Add(4*time.Hour), recordedV1, false)
	insert(2, "bob", late, late.Add(4*time.Hour), recordedV2, false)

	project := func(from, until, asOf *time.Time) []scheduleconfig.OverrideRevision {
		t.Helper()
		var out []scheduleconfig.OverrideRevision
		err := withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
			var err error
			out, err = view.GetOverrideProjectionInRange(context.Background(), rev.ScheduleID, from, until, asOf)
			return err
		})
		if err != nil {
			t.Fatalf("GetOverrideProjectionInRange: %v", err)
		}
		return out
	}

	// Unbounded and current: only the latest version exists.
	if got := project(nil, nil, nil); len(got) != 1 || got[0].UserID != "bob" {
		t.Fatalf("current projection = %+v, want only v2", got)
	}

	// As of a moment between the two writes, v1 is the state of the world.
	asOf := recordedV1.Add(30 * time.Minute)
	if got := project(nil, nil, &asOf); len(got) != 1 || got[0].UserID != "alice" {
		t.Fatalf("as-of projection = %+v, want v1", got)
	}

	// The early window no longer holds the override: the winning revision v2
	// moved away from it. Returning v1 here would mean the range filter ran
	// before the winner was chosen.
	earlyEnd := early.Add(4 * time.Hour)
	if got := project(&early, &earlyEnd, nil); len(got) != 0 {
		t.Fatalf("early window projection = %+v, want nothing", got)
	}
	lateEnd := late.Add(4 * time.Hour)
	if got := project(&late, &lateEnd, nil); len(got) != 1 || got[0].UserID != "bob" {
		t.Fatalf("late window projection = %+v, want v2", got)
	}

	// A tombstone still wins the grouping and removes the override, and an
	// as-of before it still sees the live version.
	recordedV3 := start.Add(3 * time.Hour)
	insert(3, "bob", late, late.Add(4*time.Hour), recordedV3, true)
	if got := project(nil, nil, nil); len(got) != 0 {
		t.Fatalf("after the tombstone got %+v, want nothing", got)
	}
	beforeDelete := recordedV2.Add(30 * time.Minute)
	if got := project(nil, nil, &beforeDelete); len(got) != 1 || got[0].UserID != "bob" {
		t.Fatalf("as-of before the delete = %+v, want v2", got)
	}
}

// TestSnapshotHidesConcurrentCommit is the guarantee the read side exists for.
// Under READ COMMITTED the second read would see the committed revision and
// the rendered answer would describe a state that never existed.
func TestSnapshotHidesConcurrentCommit(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	initial := createSchedule(t, s, "devops", start)
	until := start.Add(72 * time.Hour)

	var wg sync.WaitGroup
	err := withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
		ctx := context.Background()
		before, err := view.GetRevisionsInRange(ctx, initial.ScheduleID, start, until)
		if err != nil {
			return err
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			appendRevision(t, s, initial, start.Add(24*time.Hour))
		}()
		wg.Wait()

		after, err := view.GetRevisionsInRange(ctx, initial.ScheduleID, start, until)
		if err != nil {
			return err
		}
		if len(before) != len(after) {
			t.Fatalf("the snapshot saw a concurrent commit: %d revisions became %d", len(before), len(after))
		}
		if after[0].EffectiveTo != nil {
			t.Fatal("the snapshot saw the tail being closed by another transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithinSnapshot: %v", err)
	}

	// Outside the snapshot the write is there: it was hidden, not blocked.
	if got := readRevisionsInRange(t, s, initial.ScheduleID, start, until); len(got) != 2 {
		t.Fatalf("got %d revisions after the snapshot closed, want 2", len(got))
	}
}

// TestSnapshotUsesRepeatableReadAndIsReadOnly asserts the two properties the
// contract rests on directly at the database, rather than trusting that the
// options passed to BeginTx were the ones that took effect. Isolation is the
// whole reason the read side hands out a snapshot; read-only is what keeps a
// future reader from quietly writing through it.
func TestSnapshotUsesRepeatableReadAndIsReadOnly(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	createSchedule(t, s, "devops", start)

	err := s.ScheduleReadRepository().WithinSnapshot(context.Background(), func(view scheduleconfig.ScheduleReadView) error {
		v, ok := view.(*scheduleReadView)
		if !ok {
			t.Fatalf("unexpected view type %T", view)
		}
		tx, ok := v.q.(*sql.Tx)
		if !ok {
			t.Fatalf("snapshot queryer is %T, want *sql.Tx", v.q)
		}

		var isolation, readOnly string
		if err := tx.QueryRowContext(context.Background(),
			`SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')`).
			Scan(&isolation, &readOnly); err != nil {
			return err
		}
		if isolation != "repeatable read" {
			t.Fatalf("isolation = %q, want repeatable read", isolation)
		}
		if readOnly != "on" {
			t.Fatalf("transaction_read_only = %q, want on", readOnly)
		}

		if _, err := tx.ExecContext(context.Background(), `UPDATE schedules SET config_version = 99`); err == nil {
			t.Fatal("the read-only snapshot accepted a write")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithinSnapshot: %v", err)
	}
}

// TestGetScheduleRootReadsSoftDeleted: history stays readable after a delete,
// so the root lookup must not filter deleted schedules out.
func TestGetScheduleRootReadsSoftDeleted(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	initial := createSchedule(t, s, "devops", start)

	deletedAt := start.Add(24 * time.Hour)
	if _, err := s.db.Exec(`UPDATE schedules SET deleted_at = $1 WHERE id = $2`, deletedAt, initial.ScheduleID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	var root *scheduleconfig.ScheduleRoot
	err := withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
		var err error
		root, err = view.GetScheduleRoot(context.Background(), initial.ScheduleID)
		return err
	})
	if err != nil {
		t.Fatalf("GetScheduleRoot: %v", err)
	}
	if root.DeletedAt == nil || !root.DeletedAt.Equal(deletedAt) {
		t.Fatalf("deleted_at = %v, want %v", root.DeletedAt, deletedAt)
	}

	byTeam := func() *scheduleconfig.ScheduleRoot {
		t.Helper()
		var out *scheduleconfig.ScheduleRoot
		err := withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
			var err error
			out, err = view.GetScheduleRootByTeam(context.Background(), "devops")
			return err
		})
		if err != nil {
			t.Fatalf("GetScheduleRootByTeam: %v", err)
		}
		return out
	}
	if byTeam().ID != initial.ScheduleID {
		t.Fatal("the team lookup lost the soft-deleted schedule")
	}

	err = withSnapshot(t, s, func(view scheduleconfig.ScheduleReadView) error {
		_, err := view.GetScheduleRoot(context.Background(), "no-such-schedule")
		return err
	})
	if !errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
		t.Fatalf("error = %v, want ErrScheduleNotFound", err)
	}
}
