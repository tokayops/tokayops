package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/tokayops/tokayops/internal/outbound"
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

	unit := &erasureTx{tx: tx}
	if err := fn(unit); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true

	// After the commit, never inside it: an erasure that rolled back would
	// otherwise report deliveries as withdrawn while they are still going out,
	// and this counter is the one an operator watches for pages that did not
	// happen.
	countWithdrawn(unit.withdrawn)
	return nil
}

type erasureTx struct {
	tx *sql.Tx

	// withdrawn is the commitments this erasure ended, by the family each ran
	// in, held until the transaction commits. See WithinTx.
	//
	// By family and not as one number: the counter is watched per partition,
	// and an announcement withdrawn from the handover queue counted against
	// paging reads as an escalation that did not go out - which is what
	// somebody is woken up for.
	withdrawn map[string]int
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

// CancelLiveOutboundIntentsForUser marks this person's commitments as erased
// and withdraws what is still owed to them.
//
// The MARKER comes first, and it is the point of the whole operation: erasure
// is not a sweep that cleans up once, it is a durable prohibition every later
// writer has to honour. A result already in flight, a late observation from a
// worker whose lease was reclaimed, an operator reviving a failed commitment -
// each of those would otherwise put the address back minutes after the person
// was erased.
//
// Then the withdrawal, and three states get three answers - the same split the
// acknowledgement path makes:
//
//   - pending and manual_review: nothing has gone out and nothing will.
//     Withdrawn outright, and the lease goes with them so a worker holding one
//     finds out at its next compare-and-set.
//   - sending: a call is in flight and may already have landed. FLAGGED, not
//     withdrawn - and the journal says cancellation_requested, because
//     "canceled" would be a claim about an external effect nobody knows the
//     fate of yet.
//   - anything with a receipt: left alone. Something exists out there, and
//     erasure removes the ability to CONTACT a person rather than unsending
//     what was sent. Its coordinates are redacted below.
//
// The rows are taken in one statement per state, ordered by id, and the
// commitments are locked before the attempts under them - the same order
// Finalize takes, because the opposite order is a deadlock waiting for a busy
// afternoon.
func (t *erasureTx) CancelLiveOutboundIntentsForUser(ctx context.Context, userID string) error {
	if _, err := t.tx.ExecContext(ctx, `
		UPDATE outbound_intents SET recipient_erased_at = now(), updated_at = now()
		WHERE target_kind = 'user' AND target_ref = $1 AND recipient_erased_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("mark the commitments of an erased user: %w", err)
	}

	withdrawn, err := t.withdrawOutbound(ctx, `
		UPDATE outbound_intents
		SET status = 'canceled', lease_token = NULL, locked_until = NULL,
		    worker_id = NULL, updated_at = now()
		WHERE target_kind = 'user' AND target_ref = $1
		  AND NOT receipt_recorded AND status IN ('pending', 'manual_review')
		RETURNING id, delivery_family`, userID, "canceled",
		"the recipient was erased")
	if err != nil {
		return err
	}

	if _, err := t.withdrawOutbound(ctx, `
		UPDATE outbound_intents
		SET cancellation_requested = TRUE, updated_at = now()
		WHERE target_kind = 'user' AND target_ref = $1
		  AND NOT receipt_recorded AND status = 'sending'
		RETURNING id, delivery_family`, userID, "cancellation_requested",
		"the recipient was erased; this send was already in flight"); err != nil {
		return err
	}

	// Counted, not returned: the caller is the erasure service, which has no
	// business knowing how many messages somebody was owed.
	if t.withdrawn == nil {
		t.withdrawn = map[string]int{}
	}
	for family, n := range withdrawn {
		t.withdrawn[family] += n
	}
	return nil
}

// withdrawOutbound runs one withdrawal and writes a line in each commitment's
// own journal, saying what actually happened to it. The reason names nobody: it
// is written into a row that survives the person it is about.
func (t *erasureTx) withdrawOutbound(ctx context.Context, query, userID, kind, reason string) (map[string]int, error) {
	rows, err := t.tx.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("withdraw the notifications of an erased user: %w", err)
	}
	type withdrawal struct{ id, family string }
	var withdrawn []withdrawal
	for rows.Next() {
		var w withdrawal
		if err := rows.Scan(&w.id, &w.family); err != nil {
			rows.Close()
			return nil, err
		}
		withdrawn = append(withdrawn, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byFamily := map[string]int{}
	for _, w := range withdrawn {
		if err := appendIntentEventTx(ctx, t.tx, w.id, nextEventSeq, kind,
			reason, outbound.ActorErasure); err != nil {
			return nil, err
		}
		byFamily[w.family]++
	}
	return byFamily, nil
}

// ScrubOutboundEndpointsForUser removes every personal coordinate the delivery
// domain holds for this person, in all three tables that can hold one.
//
// What goes: the address a message was sent to (bound_endpoint), the receipt
// that says where the message ended up, and the response summary - which reads
// "accepted with channel=D0123" and is therefore an address in prose.
//
// What stays, deliberately: the outcome, the applied revision and the completion
// fingerprint. The first two name nobody. The fingerprint is a SHA-256 whose
// inputs include the receipt reference, which makes it pseudonymous rather than
// irreversible - channel ids are enumerable, so the hash confirms a guess for
// anybody who already holds both this database and a candidate list. It is kept
// with that risk named, because it is what tells an idempotent repeat from a
// conflict: without it one delivery could be finalised twice with two different
// answers and nothing would notice.
//
// And the FACT of a receipt stays: receipt_recorded remains true with
// receipt_redacted_at set, so the state machine still knows a message exists out
// there rather than deciding it never happened.
//
// The commitments are updated before the attempts under them. That is the order
// Finalize takes - the commitment, then its attempt - and taking it the other
// way round here is what turns a busy afternoon into a deadlock.
func (t *erasureTx) ScrubOutboundEndpointsForUser(ctx context.Context, userID string) error {
	if _, err := t.tx.ExecContext(ctx, `
		UPDATE outbound_intents
		SET bound_endpoint = NULL,
		    receipt = NULL,
		    -- The name goes with the coordinates: it is a channel and a
		    -- timestamp, or a chat and a message, which is an address by
		    -- another spelling.
		    receipt_ref = NULL,
		    receipt_redacted_at = CASE
		        WHEN receipt_recorded THEN COALESCE(receipt_redacted_at, now())
		        ELSE NULL END,
		    updated_at = now()
		WHERE target_kind = 'user' AND target_ref = $1
		  AND (bound_endpoint IS NOT NULL OR receipt IS NOT NULL
		       OR receipt_ref IS NOT NULL)`, userID); err != nil {
		return fmt.Errorf("scrub the commitment endpoints of an erased user: %w", err)
	}

	if _, err := t.tx.ExecContext(ctx, `
		UPDATE outbound_attempts a
		SET bound_endpoint = NULL,
		    receipt = NULL,
		    receipt_redacted_at = CASE
		        WHEN a.receipt_recorded THEN COALESCE(a.receipt_redacted_at, now())
		        ELSE NULL END,
		    response_summary = NULL
		FROM outbound_intents i
		WHERE i.id = a.intent_id AND i.target_kind = 'user' AND i.target_ref = $1
		  AND (a.bound_endpoint IS NOT NULL OR a.receipt IS NOT NULL
		       OR a.response_summary IS NOT NULL)`, userID); err != nil {
		return fmt.Errorf("scrub the attempt endpoints of an erased user: %w", err)
	}

	if _, err := t.tx.ExecContext(ctx, `
		UPDATE outbound_attempt_observations o
		SET receipt = NULL,
		    receipt_redacted_at = CASE
		        WHEN o.receipt_recorded THEN COALESCE(o.receipt_redacted_at, now())
		        ELSE NULL END,
		    response_summary = NULL
		FROM outbound_attempts a JOIN outbound_intents i ON i.id = a.intent_id
		WHERE a.id = o.attempt_id AND i.target_kind = 'user' AND i.target_ref = $1
		  AND (o.receipt IS NOT NULL OR o.response_summary IS NOT NULL)`,
		userID); err != nil {
		return fmt.Errorf("scrub the observed endpoints of an erased user: %w", err)
	}
	return nil
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
