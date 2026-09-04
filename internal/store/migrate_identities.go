package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SlackIdentityConflict records a legacy Slack user ID that could not be migrated
// because the same external ID is already bound to a different TokayOps user.
type SlackIdentityConflict struct {
	UserID      string
	SlackUserID string
}

// SlackIdentityMigrationResult summarises a MigrateLegacySlackIdentities run.
type SlackIdentityMigrationResult struct {
	DryRun              bool
	LegacyColumnPresent bool // false on fresh installs - nothing to migrate
	Candidates          int  // users with a non-empty legacy users.slack_user_id
	Migrated            int  // identities newly bound (or would-be, when DryRun)
	AlreadySatisfied    int  // users that already had a slack external identity
	Conflicts           []SlackIdentityConflict
}

// MigrateLegacySlackIdentities backfills external_identities(provider='slack') from the
// legacy users.slack_user_id column. The model dropped that column, but InitDB
// does NOT remove it from existing databases, so the values survive and can be
// migrated in place - the only per-user data such an upgrade has to carry
// forward.
//
// Behaviour:
//   - Idempotent: users that already have a slack identity are left untouched.
//   - Conflict-safe: a slack_user_id already bound to a different user is reported in
//     Conflicts and skipped (never overwrites another user's link).
//   - Fresh install (no legacy column): no-op, LegacyColumnPresent=false.
//   - DryRun: classifies every candidate but writes nothing.
func (s *Store) MigrateLegacySlackIdentities(dryRun bool) (SlackIdentityMigrationResult, error) {
	res := SlackIdentityMigrationResult{DryRun: dryRun}

	var present bool
	if err := s.db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'users' AND column_name = 'slack_user_id'
		)`).Scan(&present); err != nil {
		return res, fmt.Errorf("check legacy slack_user_id column: %w", err)
	}
	res.LegacyColumnPresent = present
	if !present {
		return res, nil
	}

	rows, err := s.db.Query(`SELECT id, slack_user_id FROM users WHERE COALESCE(slack_user_id, '') <> ''`)
	if err != nil {
		return res, fmt.Errorf("read legacy slack ids: %w", err)
	}
	type legacy struct{ userID, slackID string }
	var legacies []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.userID, &l.slackID); err != nil {
			rows.Close()
			return res, fmt.Errorf("scan legacy row: %w", err)
		}
		legacies = append(legacies, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()
	res.Candidates = len(legacies)

	for _, l := range legacies {
		if dryRun {
			outcome, err := s.classifySlackBackfill(l.userID, l.slackID)
			if err != nil {
				return res, err
			}
			switch outcome {
			case "migrate":
				res.Migrated++
			case "already":
				res.AlreadySatisfied++
			case "conflict":
				res.Conflicts = append(res.Conflicts, SlackIdentityConflict{UserID: l.userID, SlackUserID: l.slackID})
			}
			continue
		}

		created, err := s.BindExternalIdentityIfAbsent(l.userID, "slack", l.slackID, "")
		switch {
		case errors.Is(err, ErrExternalIdentityAlreadyLinked):
			res.Conflicts = append(res.Conflicts, SlackIdentityConflict{UserID: l.userID, SlackUserID: l.slackID})
		case err != nil:
			return res, fmt.Errorf("bind slack identity for user %s: %w", l.userID, err)
		case created:
			res.Migrated++
		default:
			res.AlreadySatisfied++
		}
	}
	return res, nil
}

// classifySlackBackfill reports what the backfill would do for one (userID, slackID)
// pair without writing - "already", "conflict", or "migrate". Used by the dry-run path.
func (s *Store) classifySlackBackfill(userID, slackID string) (string, error) {
	var existsForUser bool
	if err := s.db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM external_identities WHERE user_id = $1 AND provider = 'slack')`,
		userID).Scan(&existsForUser); err != nil {
		return "", err
	}
	if existsForUser {
		return "already", nil
	}
	var owner string
	switch err := s.db.QueryRow(
		`SELECT user_id FROM external_identities WHERE provider = 'slack' AND external_id = $1`,
		slackID).Scan(&owner); {
	case err == nil && owner != userID:
		return "conflict", nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	return "migrate", nil
}
