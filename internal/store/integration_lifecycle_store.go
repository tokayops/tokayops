package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The two commands that mean "this subscriber is gone": deleting an integration
// and switching it off. Each is one transaction over the integration's row and
// the webhook commitments addressed to it, and each says afterwards what it
// read and what it withdrew - which its caller needs for the effects that only
// make sense once the commit is durable.
//
// The row is re-read under FOR UPDATE inside the command, and a patch is
// applied to THAT row: a model fetched by the caller a moment earlier and
// written back whole would overwrite whatever somebody else changed in between.
// FOR UPDATE rather than FOR SHARE because these commands write the row; the
// fan-out, which only reads it, takes the shared lock, and the two wait for
// each other in either order - no longer than the lock timeout.

// ErrIntegrationBusy is the row held by another command for longer than this
// one waits. The caller retries; the state is whatever the other command left.
var ErrIntegrationBusy = errors.New("integration is held by another command")

// IntegrationPatch is what an edit may change. A nil field is left as it
// stands. Scope and team are not here: an integration is not moved between
// scopes, and the audience of an event is fixed when it is fanned out.
type IntegrationPatch struct {
	Name    *string
	Enabled *bool
	// Config replaces the configuration, except that an empty or masked secret
	// keeps its current value.
	Config []byte
}

// IntegrationChange is what a lifecycle command did.
type IntegrationChange struct {
	// Before is the row as the command read it under the lock, decrypted.
	Before *model.Integration
	// After is the row as written; nil once deleted.
	After *model.Integration
	// Withdrawn is how many webhook commitments the command ended outright.
	// Commitments in flight are flagged instead and are not in this number:
	// their ending is written, and counted, by the attempt that finishes.
	Withdrawn int
}

const integrationColumns = `id, type, direction, name, enabled, scope, team_id, config, created_at, updated_at`

// UpdateIntegration applies a patch to the row as it stands. Switching the
// integration off - a real true -> false, not a PUT repeating "false" - is the
// second door into withdrawal, and it is the same withdrawal deletion does:
// "off" means "do not send", and a backlog that keeps going out for a day
// after the switch would be a defect report, not a nuance. Changing the
// address, the secret, the headers or the timeout withdraws nothing; the
// commitments were made to the subscriber, and the subscriber is still there.
func (s *Store) UpdateIntegration(ctx context.Context, id string, patch IntegrationPatch,
	actor string) (IntegrationChange, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IntegrationChange{}, err
	}
	defer tx.Rollback()
	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
		return IntegrationChange{}, err
	}

	before, err := lockIntegrationTx(ctx, tx, id)
	if err != nil {
		return IntegrationChange{}, err
	}
	after := *before
	if patch.Name != nil {
		after.Name = *patch.Name
	}
	if patch.Enabled != nil {
		after.Enabled = *patch.Enabled
	}
	if patch.Config != nil {
		after.Config = mergeSecrets(before.Type, before.Config, patch.Config)
	}

	withdrawn := 0
	if before.Enabled && !after.Enabled {
		withdrawn, err = withdrawSubscriberTx(ctx, tx, id, "the subscriber was disabled", actor)
		if err != nil {
			return IntegrationChange{}, busyOr(err)
		}
	}

	encrypted, err := encryptConfig(after.Config)
	if err != nil {
		return IntegrationChange{}, err
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE integrations SET name = $1, enabled = $2, config = $3, updated_at = now()
		WHERE id = $4
		RETURNING updated_at`, after.Name, after.Enabled, encrypted, id).Scan(&after.UpdatedAt); err != nil {
		return IntegrationChange{}, busyOr(err)
	}

	if err := tx.Commit(); err != nil {
		return IntegrationChange{}, err
	}
	// After the commit and not before: a transaction that then rolled back
	// would have reported endings that never happened.
	countWithdrawn(map[string]int{outbound.FamilyWebhook: withdrawn})
	return IntegrationChange{Before: before, After: &after, Withdrawn: withdrawn}, nil
}

// DeleteIntegration removes the integration and withdraws every webhook
// commitment still owed to it, in one transaction, and leaves a tombstone.
//
// The history stays: a commitment names its subscriber by value, not by
// reference, and "did we ever try" is a question the journal has to keep
// answering after the subscriber is gone. The tombstone is what lets the
// history be read - the routes that show it resolve their scope through the
// integration, and after the row is gone they resolve it through this. It is
// written for every type, because the command is one and a rule about which
// types get one would eventually be wrong; who may act on it is the reader's
// decision.
//
// After both this and a fan-out have run, in whichever order, the deleted
// integration has no live commitment: if the fan-out won, its commitments are
// committed and visible here under the lock, and this withdraws them; if this
// won, the fan-out does not find the subscriber in the audience.
func (s *Store) DeleteIntegration(ctx context.Context, id, actor string) (IntegrationChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IntegrationChange{}, err
	}
	defer tx.Rollback()
	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
		return IntegrationChange{}, err
	}

	before, err := lockIntegrationTx(ctx, tx, id)
	if err != nil {
		return IntegrationChange{}, err
	}
	withdrawn, err := withdrawSubscriberTx(ctx, tx, id, "the subscriber was deleted", actor)
	if err != nil {
		return IntegrationChange{}, busyOr(err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO integration_tombstones (id, type, scope, team_id)
		VALUES ($1, $2, $3, $4)`,
		id, before.Type, scopeToNullString(before.Scope), stringPtrToNullString(before.TeamID)); err != nil {
		return IntegrationChange{}, fmt.Errorf("leave the tombstone of %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM integrations WHERE id = $1`, id); err != nil {
		return IntegrationChange{}, busyOr(err)
	}

	if err := tx.Commit(); err != nil {
		return IntegrationChange{}, err
	}
	countWithdrawn(map[string]int{outbound.FamilyWebhook: withdrawn})
	return IntegrationChange{Before: before, Withdrawn: withdrawn}, nil
}

// lockIntegrationTx takes the row for writing and reads it as it stands.
func lockIntegrationTx(ctx context.Context, tx *sql.Tx, id string) (*model.Integration, error) {
	integration, err := scanIntegration(tx.QueryRowContext(ctx,
		`SELECT `+integrationColumns+` FROM integrations WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, busyOr(err)
	}
	return integration, nil
}

// withdrawSubscriberTx ends what is still owed to one subscriber, by the same
// rules the alert's own withdrawal follows: nothing sent yet is withdrawn
// outright; a send in flight is flagged and the outcome decides; a commitment
// waiting for a person about an unknown outcome is withdrawn, and the history
// keeps the doubt. It returns how many it withdrew outright.
func withdrawSubscriberTx(ctx context.Context, tx *sql.Tx, integrationID, reason, actor string) (int, error) {
	notSent, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', lease_token = NULL, locked_until = NULL,
		    worker_id = NULL, updated_at = now()
		WHERE delivery_family = 'webhook' AND target_ref = $1 AND status = 'pending'
		RETURNING id`, integrationID)
	if err != nil {
		return 0, err
	}
	inFlight, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET cancellation_requested = TRUE, updated_at = now()
		WHERE delivery_family = 'webhook' AND target_ref = $1 AND status = 'sending'
		RETURNING id`, integrationID)
	if err != nil {
		return 0, err
	}
	waiting, err := cancelRowsTx(ctx, tx, `
		UPDATE outbound_intents
		SET status = 'canceled', updated_at = now()
		WHERE delivery_family = 'webhook' AND target_ref = $1 AND status = 'manual_review'
		RETURNING id`, integrationID)
	if err != nil {
		return 0, err
	}

	for _, id := range notSent {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled", reason, actor); err != nil {
			return 0, err
		}
	}
	for _, id := range inFlight {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "cancellation_requested",
			reason, actor); err != nil {
			return 0, err
		}
	}
	for _, id := range waiting {
		if err := appendIntentEventTx(ctx, tx, id, nextEventSeq, "canceled",
			reason+"; the outcome of the previous attempt stays unknown", actor); err != nil {
			return 0, err
		}
	}
	return len(notSent) + len(waiting), nil
}

// busyOr names the one failure the caller can act on - the lock timeout - and
// hands everything else back as it is.
func busyOr(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code.Name() == "lock_not_available" {
		return ErrIntegrationBusy
	}
	return err
}

// integrationEffectsLockClass is the first half of the two-key advisory lock
// the post-commit effects of a lifecycle command run under. The second half is
// the integration's id. Two keys rather than one so it shares no namespace with
// the single-key locks the schema build and the erasure take.
const integrationEffectsLockClass int32 = 1201

// WithIntegrationLocked runs fn with the integration's row as it stands - nil
// when there is none - under an advisory lock on the integration, held until fn
// returns.
//
// This is for the effects of a lifecycle command on the outside world. The
// commands themselves are serialised by the row lock; their effects are not,
// and two instances can run them in the reverse order of their commits. An
// effect that acted on the arguments of its own command would then undo the
// later one. Under this lock, an effect reads the current row and acts on
// that, and one command's read and actions finish before another's begin - so
// the last effect to run sees the newest row. The transaction stays open while
// fn runs; fn is bounded by whatever it calls.
func (s *Store) WithIntegrationLocked(ctx context.Context, id string,
	fn func(current *model.Integration) error) error {

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		integrationEffectsLockClass, id); err != nil {
		return fmt.Errorf("lock integration %s: %w", id, err)
	}
	current, err := scanIntegration(tx.QueryRowContext(ctx,
		`SELECT `+integrationColumns+` FROM integrations WHERE id = $1`, id))
	if errors.Is(err, ErrIntegrationNotFound) {
		current = nil
	} else if err != nil {
		return err
	}
	return fn(current)
}

// IntegrationTombstone is what remains of a deleted integration, if it was
// deleted by this build's command.
func (s *Store) IntegrationTombstone(ctx context.Context, id string) (model.IntegrationTombstone, bool, error) {
	var (
		tombstone     model.IntegrationTombstone
		scope, teamID sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, type, scope, team_id, deleted_at FROM integration_tombstones WHERE id = $1`, id).
		Scan(&tombstone.ID, &tombstone.Type, &scope, &teamID, &tombstone.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.IntegrationTombstone{}, false, nil
	}
	if err != nil {
		return model.IntegrationTombstone{}, false, err
	}
	if scope.Valid {
		s := model.WebhookScope(scope.String)
		tombstone.Scope = &s
	}
	if teamID.Valid {
		tombstone.TeamID = &teamID.String
	}
	return tombstone, true, nil
}
