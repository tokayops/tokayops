package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// AnonymizedUserName replaces the display name of an erased user. History
// keeps referring to the user ID, so something has to render in its place.
const AnonymizedUserName = "Deleted user"

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

// SetUserDeletedAt marks the user as erased. The row survives: every revision,
// override and event that names the user ID must stay explainable.
func (t *erasureTx) SetUserDeletedAt(ctx context.Context, userID string, at time.Time) error {
	_, err := t.tx.ExecContext(ctx,
		`UPDATE users SET deleted_at = $1 WHERE id = $2`,
		scheduleconfig.NormalizeTimestamp(at), userID)
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
