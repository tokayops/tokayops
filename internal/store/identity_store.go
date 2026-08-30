package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
)

// ErrExternalIdentityAlreadyLinked means the (provider, external_id) is already
// bound to a different user. Returned by BindExternalIdentity / BindExternalIdentityIfAbsent
// / ConfirmIdentityLink so callers can surface HTTP 409 or fall through to OTP rather
// than handling a raw unique-violation.
var ErrExternalIdentityAlreadyLinked = errors.New("external identity already linked to another user")

// ErrLinkTokenInvalid means the submitted token did not match the pending link
// (wrong code or no pending link); ErrLinkTokenExpired means it expired or hit
// the attempts limit. Both are surfaced as HTTP 400 by the API.
var (
	ErrLinkTokenInvalid = errors.New("invalid link token")
	ErrLinkTokenExpired = errors.New("link token expired")
)

// hashLinkToken hashes a plaintext token (e.g. Slack OTP 6-digit code, or an opaque
// deep-link token) for at-rest storage. The plaintext is never persisted.
func hashLinkToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// scanIdentity scans one external_identities row.
func scanIdentity(scanner interface {
	Scan(dest ...any) error
}) (*model.ExternalIdentity, error) {
	var ei model.ExternalIdentity
	var chatID, displayName sql.NullString
	if err := scanner.Scan(&ei.ID, &ei.UserID, &ei.Provider, &ei.ExternalID, &chatID, &displayName, &ei.CreatedAt, &ei.UpdatedAt); err != nil {
		return nil, err
	}
	ei.ChatID = chatID.String
	ei.DisplayName = displayName.String
	return &ei, nil
}

// BindExternalIdentity upserts an identity for (user_id, provider). A
// (provider, external_id) collision against a different user is translated to
// ErrExternalIdentityAlreadyLinked rather than a raw unique-violation.
func (s *Store) BindExternalIdentity(ei *model.ExternalIdentity) error {
	if ei.ID == "" {
		ei.ID = uuid.New().String()
	}
	now := time.Now()
	if ei.CreatedAt.IsZero() {
		ei.CreatedAt = now
	}
	ei.UpdatedAt = now

	query := activeUserCTE + `
		INSERT INTO external_identities (id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at)
		SELECT $2, active.id, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8 FROM active
		ON CONFLICT (user_id, provider) DO UPDATE SET
			external_id  = EXCLUDED.external_id,
			chat_id      = EXCLUDED.chat_id,
			display_name = EXCLUDED.display_name,
			updated_at   = EXCLUDED.updated_at
	`
	res, err := s.db.Exec(query, ei.UserID, ei.ID, ei.Provider, ei.ExternalID, ei.ChatID, ei.DisplayName, ei.CreatedAt, ei.UpdatedAt)
	if err != nil {
		if isProviderExternalConflict(err) {
			return ErrExternalIdentityAlreadyLinked
		}
		return err
	}
	// An erased user matches nothing in the CTE, so no row is written and the
	// caller is told the identity has nobody to belong to.
	return requireOneRow(res, ErrUserNotFound)
}

// BindExternalIdentityIfAbsent is the safe auto-link path for SSO / interactive
// button flows. Semantics mirror the old UpdateUserSlackID guard:
//   - If the user already has an identity for (user_id, provider) → (false, nil).
//   - If (provider, external_id) is bound to another user → (false, ErrExternalIdentityAlreadyLinked).
//   - Otherwise insert → (true, nil).
//
// Atomic via INSERT ... ON CONFLICT (user_id, provider) DO NOTHING: a concurrent
// SSO/interactive auto-link cannot be overwritten between the check and the bind
// (the previous check-then-bind sequence had this TOCTOU window). The
// idx_external_identities_provider_external unique index is NOT named in the
// ON CONFLICT clause, so a cross-user collision still raises a unique violation -
// we translate it to ErrExternalIdentityAlreadyLinked.
//
// Callers can ignore (false, nil) silently and decide whether to surface the
// already-linked conflict.
func (s *Store) BindExternalIdentityIfAbsent(userID, provider, externalID, displayName string) (bool, error) {
	// KNOWN LIMITATION. ON CONFLICT DO NOTHING makes zero affected rows mean
	// two things - "already linked" and "the user has been erased" - and this
	// statement cannot tell them apart. Both come back as (false, nil).
	//
	// The security property holds either way: no identity is created for an
	// erased user. What is lost is the ability to say why, and callers of this
	// variant are documented to ignore (false, nil) anyway. Separating the two
	// would cost a transaction - lock the user, then upsert - which is what a
	// caller should reach for if it ever needs the distinction.
	res, err := s.db.Exec(activeUserCTE+`
		INSERT INTO external_identities (id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at)
		SELECT $2, active.id, $3, $4, NULL, NULLIF($5, ''), NOW(), NOW() FROM active
		ON CONFLICT (user_id, provider) DO NOTHING
	`, userID, uuid.New().String(), provider, externalID, displayName)
	if err != nil {
		if isProviderExternalConflict(err) {
			return false, ErrExternalIdentityAlreadyLinked
		}
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetExternalIdentity loads the identity for (user_id, provider). Absence is
// signalled by sql.ErrNoRows; a channel preparing an attempt maps it to a
// refusal that no retry can improve.
func (s *Store) GetExternalIdentity(userID, provider string) (*model.ExternalIdentity, error) {
	return s.GetExternalIdentityContext(context.Background(), userID, provider)
}

// GetExternalIdentityContext is the same lookup under a caller's deadline.
//
// It exists because preparation has one. Resolving an address is local work
// measured in milliseconds, and the deadline around it
// (outbound.NotificationPrepareDeadline) is there so a database that hangs
// costs one delivery slot for five seconds rather than for the length of a
// lease. Handed a context the query ignores, that deadline was a comment.
func (s *Store) GetExternalIdentityContext(ctx context.Context, userID, provider string) (
	*model.ExternalIdentity, error) {

	query := `SELECT id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at
	          FROM external_identities WHERE user_id = $1 AND provider = $2`
	row := s.db.QueryRowContext(ctx, query, userID, provider)
	return scanIdentity(row)
}

// GetUserByExternalID is the reverse lookup (Slack button click → TokayOps user)
// that replaces GetUserBySlackID.
func (s *Store) GetUserByExternalID(provider, externalID string) (*model.User, error) {
	// Active only. This is how an inbound Slack or Telegram message finds who
	// it came from, and an erased user must not be found by it - not even if
	// an identity row outlived them by a race.
	query := `SELECT u.id, u.email, u.name, u.role, u.password_hash, u.auth_provider, u.created_at
	          FROM users u JOIN external_identities ei ON ei.user_id = u.id
	          WHERE ei.provider = $1 AND ei.external_id = $2 AND u.deleted_at IS NULL`
	row := s.db.QueryRow(query, provider, externalID)

	var u model.User
	var email, role, passwordHash, authProvider sql.NullString
	if err := row.Scan(&u.ID, &email, &u.Name, &role, &passwordHash, &authProvider, &u.CreatedAt); err != nil {
		return nil, err
	}
	u.Email = email.String
	u.Role = model.UserRole(role.String)
	u.PasswordHash = passwordHash.String
	u.AuthProvider = authProvider.String
	return &u, nil
}

// GetIdentitiesForUsers batches identities by user - used to populate User.Identities
// in the users-list response and to build the slackByUser map in handoff/syncer.
func (s *Store) GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error) {
	out := make(map[string][]*model.ExternalIdentity)
	if len(userIDs) == 0 {
		return out, nil
	}
	query := `SELECT id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at
	          FROM external_identities WHERE user_id = ANY($1)`
	rows, err := s.db.Query(query, pq.Array(userIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ei, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out[ei.UserID] = append(out[ei.UserID], ei)
	}
	return out, rows.Err()
}

// ListUserIdentities returns every identity for a single user.
func (s *Store) ListUserIdentities(userID string) ([]*model.ExternalIdentity, error) {
	query := `SELECT id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at
	          FROM external_identities WHERE user_id = $1 ORDER BY provider`
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ExternalIdentity
	for rows.Next() {
		ei, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ei)
	}
	return out, rows.Err()
}

// UnbindExternalIdentity removes the (user_id, provider) row. Absence is not an error.
func (s *Store) UnbindExternalIdentity(userID, provider string) error {
	_, err := s.db.Exec(`DELETE FROM external_identities WHERE user_id = $1 AND provider = $2`, userID, provider)
	return err
}

// IssueLinkToken upserts a pending link by (user_id, provider). The caller
// supplies the plaintext token (e.g. a freshly generated 6-digit OTP); the store
// persists only the SHA-256 hash. On the rare (provider, token_hash) collision
// (~1/1M for 6-digit codes), the caller is expected to regenerate the token and
// retry - the API layer wraps this in a bounded loop.
func (s *Store) IssueLinkToken(userID, provider, externalID, token string, expiresAt time.Time) error {
	if token == "" {
		return errors.New("token is required")
	}
	id := uuid.New().String()
	query := activeUserCTE + `
		INSERT INTO link_tokens (id, user_id, provider, token_hash, external_id, attempts, expires_at, created_at)
		SELECT $2, active.id, $3, $4, NULLIF($5, ''), 0, $6, NOW() FROM active
		ON CONFLICT (user_id, provider) DO UPDATE SET
			token_hash  = EXCLUDED.token_hash,
			external_id = EXCLUDED.external_id,
			attempts    = 0,
			expires_at  = EXCLUDED.expires_at,
			created_at  = NOW()
	`
	res, err := s.db.Exec(query, userID, id, provider, hashLinkToken(token), externalID, expiresAt)
	if err != nil {
		return err
	}
	// An erased user matches nothing in the CTE, so no row is written - and
	// silence here would be worse than a plain failure: the caller goes on to
	// send an OTP or a deep link for a token that does not exist.
	return requireOneRow(res, ErrUserNotFound)
}

// ConfirmIdentityLink consumes a pending link token and binds the external identity
// in one transaction - generalises the old ConfirmSlackOTP. Returns
// ErrLinkTokenInvalid (wrong code), ErrLinkTokenExpired (timed out / too many
// attempts), or ErrExternalIdentityAlreadyLinked (target external_id taken by
// another user). Raw DB errors stay transient and are not wrapped.
func (s *Store) ConfirmIdentityLink(userID, provider, token string) (*model.ExternalIdentity, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Users before the rows that belong to them, and an erased user has no
	// link to confirm.
	if err := lockActiveUserTx(tx, userID); err != nil {
		return nil, err
	}

	var storedHash string
	var externalIDNull sql.NullString
	var expiresAt time.Time
	var attempts int
	row := tx.QueryRow(
		`SELECT token_hash, external_id, expires_at, attempts FROM link_tokens
		 WHERE user_id = $1 AND provider = $2 FOR UPDATE`,
		userID, provider)
	if err := row.Scan(&storedHash, &externalIDNull, &expiresAt, &attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLinkTokenInvalid
		}
		return nil, err
	}

	if time.Now().After(expiresAt) {
		_, _ = tx.Exec(`DELETE FROM link_tokens WHERE user_id = $1 AND provider = $2`, userID, provider)
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrLinkTokenExpired
	}
	if attempts >= 3 {
		_, _ = tx.Exec(`DELETE FROM link_tokens WHERE user_id = $1 AND provider = $2`, userID, provider)
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrLinkTokenExpired
	}

	if hashLinkToken(token) != storedHash {
		newAttempts := attempts + 1
		if newAttempts >= 3 {
			_, _ = tx.Exec(`DELETE FROM link_tokens WHERE user_id = $1 AND provider = $2`, userID, provider)
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return nil, ErrLinkTokenExpired
		}
		if _, err := tx.Exec(`UPDATE link_tokens SET attempts = $1 WHERE user_id = $2 AND provider = $3`,
			newAttempts, userID, provider); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrLinkTokenInvalid
	}

	// Token matched - bind external_identity and delete the link token.
	externalID := externalIDNull.String
	if externalID == "" {
		// Defensive: Slack OTP path requires external_id at issue; deep-link supplies
		// it via a different consume entry-point.
		return nil, fmt.Errorf("link token has no external_id; nothing to bind")
	}

	// Uniqueness check: another user already linked to this external_id?
	var otherUserID string
	switch err := tx.QueryRow(
		`SELECT user_id FROM external_identities WHERE provider = $1 AND external_id = $2 AND user_id != $3`,
		provider, externalID, userID).Scan(&otherUserID); {
	case err == nil:
		return nil, ErrExternalIdentityAlreadyLinked
	case errors.Is(err, sql.ErrNoRows):
		// no conflict, continue
	default:
		return nil, err
	}

	ei := &model.ExternalIdentity{
		ID:         uuid.New().String(),
		UserID:     userID,
		Provider:   provider,
		ExternalID: externalID,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if _, err := tx.Exec(`
		INSERT INTO external_identities (id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULL, NULL, $5, $6)
		ON CONFLICT (user_id, provider) DO UPDATE SET
			external_id = EXCLUDED.external_id,
			updated_at  = EXCLUDED.updated_at
	`, ei.ID, ei.UserID, ei.Provider, ei.ExternalID, ei.CreatedAt, ei.UpdatedAt); err != nil {
		if isProviderExternalConflict(err) {
			return nil, ErrExternalIdentityAlreadyLinked
		}
		return nil, err
	}

	if _, err := tx.Exec(`DELETE FROM link_tokens WHERE user_id = $1 AND provider = $2`, userID, provider); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ei, nil
}

// ConsumeLinkToken consumes a deep-link token (e.g. Telegram `/start <token>`)
// and binds the external identity in one transaction. Unlike ConfirmIdentityLink
// it is keyed by (provider, token_hash) and resolves the user FROM the token row -
// the bot side has no logged-in user. The token is a high-entropy bearer secret,
// so a wrong token simply matches no row (ErrLinkTokenInvalid); there is no
// attempt-counting (nothing to lock), and security rests on entropy + TTL +
// one-time delete. externalID/chatID/displayName come from the inbound update and
// are written onto the identity (ConfirmIdentityLink leaves chat_id/display_name NULL).
func (s *Store) ConsumeLinkToken(provider, token, externalID, chatID, displayName string) (*model.ExternalIdentity, error) {
	if token == "" || externalID == "" {
		return nil, ErrLinkTokenInvalid
	}
	tokenHash := hashLinkToken(token)

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Who the token belongs to has to be known before the user can be locked,
	// and the user has to be locked before the token row - erasure goes user
	// first and would deadlock against the opposite order. So the token is
	// read once without a lock to learn the owner, and then again under its
	// own lock, because anything could have happened to it while this
	// transaction waited on the user.
	var owner string
	switch err := tx.QueryRow(
		`SELECT user_id FROM link_tokens WHERE provider = $1 AND token_hash = $2`,
		provider, tokenHash).Scan(&owner); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrLinkTokenInvalid
	case err != nil:
		return nil, err
	}
	if err := lockActiveUserTx(tx, owner); err != nil {
		return nil, err
	}

	var userID string
	var expiresAt time.Time
	row := tx.QueryRow(
		`SELECT user_id, expires_at FROM link_tokens
		 WHERE provider = $1 AND token_hash = $2 FOR UPDATE`,
		provider, tokenHash)
	if err := row.Scan(&userID, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLinkTokenInvalid
		}
		return nil, err
	}
	// Load-bearing, not a formality. The row was read once without a lock and
	// again with one, and between those two reads it can be deleted and a new
	// token issued under the same hash for somebody else. Binding then would
	// give user B an identity while this transaction holds user A's lock -
	// which is to say, with no protection for B at all. Refuse instead: the
	// caller retries with a token whose owner is the one that got locked.
	if userID != owner {
		return nil, ErrLinkTokenInvalid
	}

	if time.Now().After(expiresAt) {
		_, _ = tx.Exec(`DELETE FROM link_tokens WHERE provider = $1 AND token_hash = $2`, provider, tokenHash)
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrLinkTokenExpired
	}

	// Uniqueness check: another user already linked to this external_id?
	var otherUserID string
	switch err := tx.QueryRow(
		`SELECT user_id FROM external_identities WHERE provider = $1 AND external_id = $2 AND user_id != $3`,
		provider, externalID, userID).Scan(&otherUserID); {
	case err == nil:
		return nil, ErrExternalIdentityAlreadyLinked
	case errors.Is(err, sql.ErrNoRows):
		// no conflict, continue
	default:
		return nil, err
	}

	ei := &model.ExternalIdentity{
		ID:          uuid.New().String(),
		UserID:      userID,
		Provider:    provider,
		ExternalID:  externalID,
		ChatID:      chatID,
		DisplayName: displayName,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if _, err := tx.Exec(`
		INSERT INTO external_identities (id, user_id, provider, external_id, chat_id, display_name, created_at, updated_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''), $7, $8)
		ON CONFLICT (user_id, provider) DO UPDATE SET
			external_id  = EXCLUDED.external_id,
			chat_id      = EXCLUDED.chat_id,
			display_name = EXCLUDED.display_name,
			updated_at   = EXCLUDED.updated_at
	`, ei.ID, ei.UserID, ei.Provider, ei.ExternalID, ei.ChatID, ei.DisplayName, ei.CreatedAt, ei.UpdatedAt); err != nil {
		if isProviderExternalConflict(err) {
			return nil, ErrExternalIdentityAlreadyLinked
		}
		return nil, err
	}

	if _, err := tx.Exec(`DELETE FROM link_tokens WHERE provider = $1 AND token_hash = $2`, provider, tokenHash); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ei, nil
}

// isProviderExternalConflict matches the unique violation on
// idx_external_identities_provider_external. We pattern-match the error message
// to avoid pulling pgx-specific types; the index name is stable.
func isProviderExternalConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "idx_external_identities_provider_external") ||
		(strings.Contains(msg, "external_identities") && strings.Contains(msg, "provider") && strings.Contains(msg, "external_id"))
}
