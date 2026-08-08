package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/rotation"
)

// AnonymizedUserName replaces the display name of an erased user. History
// keeps referring to the user ID, so something has to render in its place.
const AnonymizedUserName = "Deleted user"

// dbTimestampResolution is the resolution PostgreSQL stores TIMESTAMPTZ at.
// Go clocks carry nanoseconds, so a timestamp is truncated before it is
// written: otherwise the value read back differs from the value handed in.
//
// scheduleconfig states the same fact for its own contract. Erasure has
// nothing to do with schedule configuration, so it says it itself rather than
// importing that package for one helper - two one-line statements of an
// immutable property of the database cannot drift apart.
const dbTimestampResolution = time.Microsecond

func dbTimestamp(t time.Time) time.Time {
	return t.Truncate(dbTimestampResolution)
}

// ErasureRepository exposes the user erasure unit of work. Like the schedule
// configuration repository it stays out of StoreInterface.
func (s *Store) ErasureRepository() erasure.Repository {
	return &erasureRepo{db: s.db}
}

type erasureRepo struct {
	db *sql.DB
}

func (r *erasureRepo) WithinTx(ctx context.Context, fn func(erasure.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(&erasureTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

type erasureTx struct {
	tx *sql.Tx
}

// adminLifecycleLockKey is an arbitrary but fixed advisory-lock key. Every
// command that can change the number of active administrators takes it, so
// they serialize instead of racing on a count each of them reads separately.
// The legacy schedule reset uses the same mechanism with a different key.
const adminLifecycleLockKey int64 = 1953980276

func (t *erasureTx) LockAdminLifecycle(ctx context.Context) error {
	_, err := t.tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, adminLifecycleLockKey)
	return err
}

// LockUser takes FOR UPDATE on the user row. This row is the point every other
// command serializes against: they take a shared lock on it before they touch
// a schedule, so an erasure and an assignment cannot both commit.
func (t *erasureTx) LockUser(ctx context.Context, userID string) (*erasure.LockedUser, error) {
	var (
		user    erasure.LockedUser
		role    sql.NullString
		deleted sql.NullTime
	)
	err := t.tx.QueryRowContext(ctx,
		`SELECT id, role, deleted_at FROM users WHERE id = $1 FOR UPDATE`, userID).
		Scan(&user.ID, &role, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, erasure.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	user.Role = role.String
	if deleted.Valid {
		v := deleted.Time
		user.DeletedAt = &v
	}
	return &user, nil
}

func (t *erasureTx) CountActiveAdmins(ctx context.Context) (int, error) {
	var count int
	err := t.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).Scan(&count)
	return count, err
}

// ListScheduleTailsLocked returns the in-force revision of every live
// schedule, holding a SHARE lock on the schedule rows.
//
// The lock is the point: a save takes FOR UPDATE on the same row, so a
// rotation cannot acquire the user being erased between this scan and the
// commit. Deleted schedules are skipped - nobody is on duty there, and
// including them would block an erasure forever on a schedule the team
// already retired.
func (t *erasureTx) ListScheduleTailsLocked(ctx context.Context) ([]erasure.ScheduleTail, error) {
	rows, err := t.tx.QueryContext(ctx,
		`SELECT s.id, s.team_id, r.snapshot
		 FROM schedules s
		 JOIN schedule_revisions r ON r.schedule_id = s.id AND r.effective_to IS NULL
		 WHERE s.deleted_at IS NULL AND r.kind = 'active'
		 ORDER BY s.id
		 FOR SHARE OF s`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []erasure.ScheduleTail
	for rows.Next() {
		var (
			tail     erasure.ScheduleTail
			snapshot []byte
		)
		if err := rows.Scan(&tail.ScheduleID, &tail.TeamID, &snapshot); err != nil {
			return nil, err
		}
		if tail.Snapshot, err = rotation.DecodeSnapshot(snapshot); err != nil {
			return nil, fmt.Errorf("store: schedule %s has an undecodable tail snapshot: %w", tail.ScheduleID, err)
		}
		out = append(out, tail)
	}
	return out, rows.Err()
}

// ListLiveOverrideHeadsForUser returns the override heads aimed at the user
// that are still in force or yet to start.
//
// Tombstones are dropped only AFTER the head is picked, for the same reason
// the projection does it: filtering first would let the revision preceding a
// delete win the MAX and block an erasure on an override that was removed.
func (t *erasureTx) ListLiveOverrideHeadsForUser(ctx context.Context, userID string,
	at time.Time) ([]erasure.OverrideAssignment, error) {

	rows, err := t.tx.QueryContext(ctx,
		`SELECT o.schedule_id, s.team_id, o.override_id, o.valid_from, o.valid_to
		 FROM schedule_override_revisions o
		 JOIN (SELECT override_id, MAX(revision) AS revision
		       FROM schedule_override_revisions
		       GROUP BY override_id) last
		   ON last.override_id = o.override_id AND last.revision = o.revision
		 JOIN schedules s ON s.id = o.schedule_id
		 WHERE o.user_id = $1
		   AND NOT o.deleted
		   AND o.valid_to > $2
		   AND s.deleted_at IS NULL
		 ORDER BY o.schedule_id, o.override_id`, userID, dbTimestamp(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []erasure.OverrideAssignment
	for rows.Next() {
		var a erasure.OverrideAssignment
		if err := rows.Scan(&a.ScheduleID, &a.TeamID, &a.OverrideID, &a.ValidFrom, &a.ValidTo); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteUserTeamMemberships removes the user from every team. Membership is
// not history: it is a live grant, and an erased user must not keep one.
func (t *erasureTx) DeleteUserTeamMemberships(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM team_members WHERE user_id = $1`, userID)
	return err
}

// SetUserDeletedAt marks the user as erased. The row survives: every revision,
// override and event that names the user ID must stay explainable.
func (t *erasureTx) SetUserDeletedAt(ctx context.Context, userID string, at time.Time) error {
	_, err := t.tx.ExecContext(ctx,
		`UPDATE users SET deleted_at = $1 WHERE id = $2`,
		dbTimestamp(at), userID)
	return err
}

// AnonymizeUser strips the identifying columns. id and role are deliberately
// untouched: the ID is the join key history depends on, and role removal has
// its own invariant (the system must keep an administrator) that belongs to
// the command layer, not to erasure.
//
// email becomes NULL rather than an empty string so the UNIQUE constraint
// still admits further anonymizations and a lookup by the old address returns
// nothing at all.
func (t *erasureTx) AnonymizeUser(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx,
		`UPDATE users
		 SET name = $1, email = NULL, password_hash = NULL, auth_provider = NULL
		 WHERE id = $2`, AnonymizedUserName, userID)
	return err
}

func (t *erasureTx) DeleteUserAPITokens(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM api_tokens WHERE user_id = $1`, userID)
	return err
}

func (t *erasureTx) DeleteUserExternalIdentities(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM external_identities WHERE user_id = $1`, userID)
	return err
}

func (t *erasureTx) DeleteUserLinkTokens(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM link_tokens WHERE user_id = $1`, userID)
	return err
}

// NullifyOverrideRevisionReasons clears free text on override revisions where
// the user is the target or the author.
//
// This and NullifyScheduleRevisionChangeReasons are the only writes that
// mutate an append-only history row besides closing a revision: free text can
// name a person, so the reason columns are a declared exception to
// immutability. Known residual risk: a third party named inside someone
// else's text is not reachable this way.
func (t *erasureTx) NullifyOverrideRevisionReasons(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx,
		`UPDATE schedule_override_revisions SET reason = NULL
		 WHERE reason IS NOT NULL AND (user_id = $1 OR recorded_by = $1)`, userID)
	return err
}

// NullifyScheduleRevisionChangeReasons clears free text on schedule revisions
// the user authored.
func (t *erasureTx) NullifyScheduleRevisionChangeReasons(ctx context.Context, userID string) error {
	_, err := t.tx.ExecContext(ctx,
		`UPDATE schedule_revisions SET change_reason = NULL
		 WHERE change_reason IS NOT NULL AND created_by = $1`, userID)
	return err
}

var _ erasure.Tx = (*erasureTx)(nil)
