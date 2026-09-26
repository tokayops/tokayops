package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// linkProviderPhone is the link_tokens provider of the codes that verify a
// phone number. The token's external_id is the number the code was spoken to.
const linkProviderPhone = "phone"

var (
	// ErrPhoneContactChanged means the person's number is not the one the
	// request was about: it changed, or was removed, in between.
	ErrPhoneContactChanged = errors.New("the phone number changed")
	// ErrPhoneNotVerified means the request needs a verified number.
	ErrPhoneNotVerified = errors.New("the phone number is not verified")
	// ErrPhoneNoTestCall means a Do Not Disturb check was marked for a sender
	// that has not called the number recently.
	ErrPhoneNoTestCall = errors.New("no recent test call from that number")
)

// Why a call was not reserved.
const (
	PhoneRefusedQuota    = "quota"
	PhoneRefusedInFlight = "in_flight"
	PhoneRefusedBusy     = "busy"
)

// PhoneCallRefused is a call the limiter or the person's allowance did not
// let through, and when asking again can succeed.
type PhoneCallRefused struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *PhoneCallRefused) Error() string {
	return fmt.Sprintf("call refused (%s), retry after %s", e.Reason, e.RetryAfter)
}

// How much a person may make the account call. A code call is the one they
// can repeat at will, so it has its own, smaller allowance.
const (
	phoneVerifyCallsPerHour = 3
	phoneCallsPerDay        = 10
)

// phoneTestCallFreshness is how recent a test call from a sender must be for
// the person to mark that it came through.
const phoneTestCallFreshness = time.Hour

// PhoneCallRequest is one call a person asks for, and what it is spent against.
type PhoneCallRequest struct {
	UserID        string
	Purpose       string // "verify" or "dnd_check"
	ToNumber      string
	FromNumber    string
	IntegrationID string
	// AccountScope is the provider account the call is made on.
	AccountScope  string
	CPS           int
	MaxConcurrent int
	// Hold is the longest the call can live at the provider: ringing, talking
	// and being put through. The capacity it takes is spent for that long
	// unless it is given back sooner.
	Hold time.Duration
	// RequireVerified refuses the call unless the number is verified.
	RequireVerified bool
}

// PhoneReservation is the capacity a call was given.
type PhoneReservation struct {
	ID        string
	HoldUntil time.Time
}

// GetPhoneContact returns the person's phone, or nil if they have none.
func (s *Store) GetPhoneContact(ctx context.Context, userID string) (*model.PhoneContact, error) {
	return scanPhoneContact(s.db.QueryRowContext(ctx, `
		SELECT value, verified_at, pinned_integration_id, updated_at
		FROM user_contacts WHERE user_id = $1 AND kind = 'phone'`, userID))
}

func scanPhoneContact(row *sql.Row) (*model.PhoneContact, error) {
	var c model.PhoneContact
	var verifiedAt sql.NullTime
	var pinned sql.NullString
	if err := row.Scan(&c.Value, &verifiedAt, &pinned, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if verifiedAt.Valid {
		at := verifiedAt.Time
		c.VerifiedAt = &at
	}
	c.PinnedIntegrationID = pinned.String
	return &c, nil
}

// SetPhoneContact writes the person's number. A different number starts
// unverified and unpinned - in the same statement, so there is no moment when a
// new number carries the old one's proof, and no caller that can forget to
// clear it. The pin goes too: it was checked against the old number's coverage.
// Writing the same number again changes nothing.
func (s *Store) SetPhoneContact(ctx context.Context, userID, value string) (*model.PhoneContact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := lockActiveUserTx(tx, userID); err != nil {
		return nil, err
	}
	contact, err := scanPhoneContactTx(tx.QueryRowContext(ctx, `
		INSERT INTO user_contacts (id, user_id, kind, value, created_at, updated_at)
		VALUES ($1, $2, 'phone', $3, now(), now())
		ON CONFLICT (user_id) WHERE kind = 'phone' DO UPDATE SET
			value = EXCLUDED.value,
			verified_at = CASE WHEN user_contacts.value = EXCLUDED.value
				THEN user_contacts.verified_at END,
			pinned_integration_id = CASE WHEN user_contacts.value = EXCLUDED.value
				THEN user_contacts.pinned_integration_id END,
			updated_at = CASE WHEN user_contacts.value = EXCLUDED.value
				THEN user_contacts.updated_at ELSE now() END
		RETURNING value, verified_at, pinned_integration_id, updated_at`,
		uuid.New().String(), userID, value))
	if err != nil {
		return nil, err
	}
	return contact, tx.Commit()
}

// scanPhoneContactTx is scanPhoneContact for a row that must exist.
func scanPhoneContactTx(row *sql.Row) (*model.PhoneContact, error) {
	contact, err := scanPhoneContact(row)
	if err == nil && contact == nil {
		return nil, sql.ErrNoRows
	}
	return contact, err
}

// DeletePhoneContact removes the person's phone, what they confirmed about it
// and any code still waiting to be entered for it.
func (s *Store) DeletePhoneContact(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := lockActiveUserTx(tx, userID); err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM user_contacts WHERE user_id = $1 AND kind = 'phone'`,
		`DELETE FROM phone_dnd_checks WHERE user_id = $1`,
		`DELETE FROM link_tokens WHERE user_id = $1 AND provider = '` + linkProviderPhone + `'`,
	} {
		if _, err := tx.ExecContext(ctx, statement, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PinPhoneIntegration sets the provider the person prefers, or clears it with
// "". The caller has checked that it covers number; the write only lands if
// number is still the person's, so a pin checked against one number is never
// stored against another.
func (s *Store) PinPhoneIntegration(ctx context.Context, userID, number, integrationID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_contacts SET pinned_integration_id = NULLIF($3, ''), updated_at = now()
		WHERE user_id = $1 AND kind = 'phone' AND value = $2
		  AND EXISTS (SELECT 1 FROM users WHERE id = $1 AND deleted_at IS NULL)`,
		userID, number, integrationID)
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrPhoneContactChanged)
}

// ReservePhoneCall spends the person's allowance and the account's capacity on
// one call, or says why not and when to ask again. It is the only way to a
// call a person asks for.
//
// Lock order: the user, then the account's capacity row.
//
// The user is locked FOR NO KEY UPDATE and not with lockActiveUserTx's FOR
// SHARE. A shared lock does not keep a second request out, and two tabs or a
// double click would each count the allowance before the other wrote to it and
// both get through. NO KEY UPDATE still conflicts with every FOR SHARE taken on
// the user - erasure's included - and not with foreign key checks.
//
// The capacity row is the serialization of the account: every instance takes it
// before counting what the account is doing. Time is read with clock_timestamp()
// after both locks are held. now() is when the transaction started, and one
// that waited on the lock behind a neighbour would reckon the rate and the
// holds from before the wait - the neighbour's next free moment would look like
// the future, and its own hold would end too early.
func (s *Store) ReservePhoneCall(ctx context.Context, req PhoneCallRequest) (*PhoneReservation, error) {
	if req.CPS < 1 || req.MaxConcurrent < 1 {
		return nil, fmt.Errorf("account %s has no call capacity configured", req.AccountScope)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL FOR NO KEY UPDATE`, req.UserID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO call_capacity (account_scope) VALUES ($1) ON CONFLICT DO NOTHING`, req.AccountScope); err != nil {
		return nil, err
	}
	var nextFree, now time.Time
	if err := tx.QueryRowContext(ctx,
		`SELECT next_free_at FROM call_capacity WHERE account_scope = $1 FOR UPDATE`, req.AccountScope).Scan(&nextFree); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return nil, err
	}

	contact, err := scanPhoneContact(tx.QueryRowContext(ctx, `
		SELECT value, verified_at, pinned_integration_id, updated_at
		FROM user_contacts WHERE user_id = $1 AND kind = 'phone'`, req.UserID))
	if err != nil {
		return nil, err
	}
	if contact == nil || contact.Value != req.ToNumber {
		return nil, ErrPhoneContactChanged
	}
	if req.RequireVerified && !contact.Verified() {
		return nil, ErrPhoneNotVerified
	}

	if refused, err := phoneAllowanceTx(ctx, tx, req, now); err != nil || refused != nil {
		if refused != nil {
			return nil, refused
		}
		return nil, err
	}
	if refused, err := accountCapacityTx(ctx, tx, req, now, nextFree); err != nil || refused != nil {
		if refused != nil {
			return nil, refused
		}
		return nil, err
	}

	reservation := &PhoneReservation{ID: uuid.New().String(), HoldUntil: now.Add(req.Hold)}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO call_reservations (id, account_scope, purpose, user_id, from_number, created_at, hold_until)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		reservation.ID, req.AccountScope, req.Purpose, req.UserID, req.FromNumber, now, reservation.HoldUntil); err != nil {
		return nil, err
	}
	interval := time.Second / time.Duration(req.CPS)
	if _, err := tx.ExecContext(ctx,
		`UPDATE call_capacity SET next_free_at = $2 WHERE account_scope = $1`,
		req.AccountScope, now.Add(interval)); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO phone_calls_log (id, user_id, purpose, to_number, from_number, integration_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uuid.New().String(), req.UserID, req.Purpose, req.ToNumber, req.FromNumber, req.IntegrationID, now); err != nil {
		return nil, err
	}
	return reservation, tx.Commit()
}

// phoneAllowanceTx is the person's side: how many calls they asked for, and
// whether one of this kind is still going. A call asked for counts whether or
// not it was placed - one whose outcome is unknown may have rung.
//
// One code call at a time: link_tokens keeps one code per person, and a new
// code would invalidate the one they are listening to. A test call is one at a
// time per sender, so testing two numbers does not wait for the first.
func phoneAllowanceTx(ctx context.Context, tx *sql.Tx, req PhoneCallRequest, now time.Time) (*PhoneCallRefused, error) {
	var inFlightUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT max(hold_until) FROM call_reservations
		WHERE user_id = $1 AND purpose = $2 AND released_at IS NULL AND hold_until > $3
		  AND ($2 <> 'dnd_check' OR from_number = $4)`,
		req.UserID, req.Purpose, now, req.FromNumber).Scan(&inFlightUntil); err != nil {
		return nil, err
	}
	if inFlightUntil.Valid {
		return &PhoneCallRefused{Reason: PhoneRefusedInFlight, RetryAfter: inFlightUntil.Time.Sub(now)}, nil
	}

	for _, window := range []struct {
		purpose string
		span    time.Duration
		limit   int
	}{
		{"verify", time.Hour, phoneVerifyCallsPerHour},
		{"", 24 * time.Hour, phoneCallsPerDay},
	} {
		if window.purpose != "" && window.purpose != req.Purpose {
			continue
		}
		var count int
		var oldest sql.NullTime
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*), min(created_at) FROM phone_calls_log
			WHERE user_id = $1 AND created_at > $2 AND ($3 = '' OR purpose = $3)`,
			req.UserID, now.Add(-window.span), window.purpose).Scan(&count, &oldest); err != nil {
			return nil, err
		}
		if count >= window.limit {
			return &PhoneCallRefused{Reason: PhoneRefusedQuota, RetryAfter: oldest.Time.Add(window.span).Sub(now)}, nil
		}
	}
	return nil, nil
}

// accountCapacityTx is the account's side, under the capacity row's lock: no
// more calls alive at once than the account allows, and none sooner than its
// rate. A reservation is alive until it is given back or its hold ends; one
// that is never given back still ends.
func accountCapacityTx(ctx context.Context, tx *sql.Tx, req PhoneCallRequest, now, nextFree time.Time) (*PhoneCallRefused, error) {
	var live int
	var soonest sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*), min(hold_until) FROM call_reservations
		WHERE account_scope = $1 AND released_at IS NULL AND hold_until > $2`,
		req.AccountScope, now).Scan(&live, &soonest); err != nil {
		return nil, err
	}
	if live >= req.MaxConcurrent {
		return &PhoneCallRefused{Reason: PhoneRefusedBusy, RetryAfter: soonest.Time.Sub(now)}, nil
	}
	if nextFree.After(now) {
		return &PhoneCallRefused{Reason: PhoneRefusedBusy, RetryAfter: nextFree.Sub(now)}, nil
	}
	return nil, nil
}

// ReleasePhoneReservation gives capacity back early, when the call is known not
// to exist. Nothing depends on it happening: a reservation nobody releases
// ends at its hold.
func (s *Store) ReleasePhoneReservation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE call_reservations SET released_at = clock_timestamp() WHERE id = $1 AND released_at IS NULL`, id)
	return err
}

// IssuePhoneCode stores the code spoken to number, replacing any earlier one.
// Only the hash is kept. On the rare collision with another person's code the
// caller generates a new one and tries again, as for Slack.
func (s *Store) IssuePhoneCode(ctx context.Context, userID, number, code string, ttl time.Duration) error {
	res, err := s.db.ExecContext(ctx, activeUserCTE+`
		INSERT INTO link_tokens (id, user_id, provider, token_hash, external_id, attempts, expires_at, created_at)
		SELECT $2, active.id, $3, $4, $5, 0, now() + make_interval(secs => $6), now() FROM active
		ON CONFLICT (user_id, provider) DO UPDATE SET
			token_hash  = EXCLUDED.token_hash,
			external_id = EXCLUDED.external_id,
			attempts    = 0,
			expires_at  = EXCLUDED.expires_at,
			created_at  = now()`,
		userID, uuid.New().String(), linkProviderPhone, hashLinkToken(code), number, ttl.Seconds())
	if err != nil {
		return err
	}
	return requireOneRow(res, ErrUserNotFound)
}

// phoneCodeAttempts is how many wrong codes end a code.
const phoneCodeAttempts = 3

// ConfirmPhoneCode marks the person's number verified if code is the one
// spoken to it. A code spoken to an earlier number verifies nothing: the
// number is part of the match, checked in the statement that writes the proof.
func (s *Store) ConfirmPhoneCode(ctx context.Context, userID, code string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := lockActiveUserTx(tx, userID); err != nil {
		return err
	}
	var storedHash, number string
	var attempts int
	var expired bool
	err = tx.QueryRowContext(ctx, `
		SELECT token_hash, COALESCE(external_id, ''), attempts, expires_at <= now()
		FROM link_tokens WHERE user_id = $1 AND provider = $2 FOR UPDATE`,
		userID, linkProviderPhone).Scan(&storedHash, &number, &attempts, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLinkTokenInvalid
	}
	if err != nil {
		return err
	}

	drop := func(result error) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM link_tokens WHERE user_id = $1 AND provider = $2`, userID, linkProviderPhone); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return result
	}
	if expired || attempts >= phoneCodeAttempts {
		return drop(ErrLinkTokenExpired)
	}
	if hashLinkToken(code) != storedHash {
		if attempts+1 >= phoneCodeAttempts {
			return drop(ErrLinkTokenExpired)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE link_tokens SET attempts = attempts + 1 WHERE user_id = $1 AND provider = $2`,
			userID, linkProviderPhone); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrLinkTokenInvalid
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE user_contacts SET verified_at = now(), updated_at = now()
		WHERE user_id = $1 AND kind = 'phone' AND value = $2`, userID, number)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		// The code was spoken to a number the person no longer has.
		return drop(ErrLinkTokenInvalid)
	}
	return drop(nil)
}

// ConfirmDNDCheck records the person's word that a test call from sender came
// through Do Not Disturb to their current number. It needs a verified number
// and a test call from that sender to it within the last hour: the mark is an
// answer to a call, not a setting.
func (s *Store) ConfirmDNDCheck(ctx context.Context, userID, sender string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := lockActiveUserTx(tx, userID); err != nil {
		return err
	}
	contact, err := scanPhoneContact(tx.QueryRowContext(ctx, `
		SELECT value, verified_at, pinned_integration_id, updated_at
		FROM user_contacts WHERE user_id = $1 AND kind = 'phone'`, userID))
	if err != nil {
		return err
	}
	if !contact.Verified() {
		return ErrPhoneNotVerified
	}
	var called bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM phone_calls_log
			WHERE user_id = $1 AND purpose = 'dnd_check' AND to_number = $2 AND from_number = $3
			  AND created_at > now() - make_interval(secs => $4))`,
		userID, contact.Value, sender, phoneTestCallFreshness.Seconds()).Scan(&called); err != nil {
		return err
	}
	if !called {
		return ErrPhoneNoTestCall
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO phone_dnd_checks (user_id, phone_value, sender_number, confirmed_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id, phone_value, sender_number) DO UPDATE SET confirmed_at = now()`,
		userID, contact.Value, sender); err != nil {
		return err
	}
	return tx.Commit()
}

// PhoneDNDChecks returns, for the person's current number, when each sender was
// confirmed to come through. A check made for an earlier number is not
// returned: it was about a different phone.
func (s *Store) PhoneDNDChecks(ctx context.Context, userID string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.sender_number, d.confirmed_at
		FROM phone_dnd_checks d
		JOIN user_contacts c ON c.user_id = d.user_id AND c.kind = 'phone' AND c.value = d.phone_value
		WHERE d.user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var sender string
		var at time.Time
		if err := rows.Scan(&sender, &at); err != nil {
			return nil, err
		}
		out[sender] = at
	}
	return out, rows.Err()
}
