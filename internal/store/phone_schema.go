package store

import "fmt"

// A person's phone, and what it takes to call it.
//
// user_contacts is a table rather than a column on users so that a second
// number, or a second kind of contact, is a row and not a migration. One phone
// per person is a partial unique index, and it is the only thing that has to
// go when that changes.
//
// The number is not unique across people: a shared on-call handset is an
// organisation some teams choose.
//
// phone_dnd_checks records that a call from one sender number reached one of
// the person's numbers through Do Not Disturb. It is keyed by both numbers, so a
// change of either leaves no row that matches, and the profile asks for the
// check again without anything having to clear a flag.
//
// phone_calls_log is every call a person asked for - a code or a test call -
// whether or not the provider took it: a call whose outcome is unknown may have
// rung, and it counts against the person's allowance like one that did.
//
// call_capacity and call_reservations are the limiter in front of a provider
// account. The provider queues what it cannot place at once and can hold it
// for a day, outside every deadline and withdrawal this system has, so the
// account's rate and concurrency are spent here, before the request goes out.
// account_scope is the provider's account id rather than an integration id: two
// integrations on one account share its limits, and deleting and recreating an
// integration must not forget what the account is doing.
func (s *Store) applyPhoneSchema() error {
	if _, err := s.db.Exec(`
	CREATE TABLE IF NOT EXISTS user_contacts (
		id                    TEXT PRIMARY KEY,
		user_id               TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		kind                  TEXT NOT NULL CHECK (kind IN ('phone')),
		value                 TEXT NOT NULL,
		verified_at           TIMESTAMPTZ,
		pinned_integration_id TEXT REFERENCES integrations(id) ON DELETE SET NULL,
		created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_user_contacts_one_phone
		ON user_contacts (user_id) WHERE kind = 'phone';

	CREATE TABLE IF NOT EXISTS phone_dnd_checks (
		user_id       TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		phone_value   TEXT NOT NULL,
		sender_number TEXT NOT NULL,
		confirmed_at  TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (user_id, phone_value, sender_number)
	);

	CREATE TABLE IF NOT EXISTS phone_calls_log (
		id             TEXT PRIMARY KEY,
		user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		purpose        TEXT NOT NULL CHECK (purpose IN ('verify', 'dnd_check')),
		to_number      TEXT,
		from_number    TEXT NOT NULL,
		integration_id TEXT,
		created_at     TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_phone_calls_log_user
		ON phone_calls_log (user_id, created_at);

	CREATE TABLE IF NOT EXISTS call_capacity (
		account_scope TEXT PRIMARY KEY,
		next_free_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	);

	CREATE TABLE IF NOT EXISTS call_reservations (
		id            TEXT PRIMARY KEY,
		account_scope TEXT NOT NULL,
		purpose       TEXT NOT NULL CHECK (purpose IN ('verify', 'dnd_check', 'delivery')),
		user_id       TEXT REFERENCES users(id) ON DELETE SET NULL,
		from_number   TEXT,
		attempt_id    TEXT,
		created_at    TIMESTAMPTZ NOT NULL,
		hold_until    TIMESTAMPTZ NOT NULL,
		released_at   TIMESTAMPTZ
	);
	CREATE INDEX IF NOT EXISTS idx_call_reservations_live
		ON call_reservations (account_scope, hold_until) WHERE released_at IS NULL;
	CREATE INDEX IF NOT EXISTS idx_call_reservations_user
		ON call_reservations (user_id, hold_until) WHERE released_at IS NULL;
	`); err != nil {
		return fmt.Errorf("build the phone tables: %w", err)
	}
	return nil
}
