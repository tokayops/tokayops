package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The worker's side of delivery: what is overdue, what was abandoned, what can
// be claimed, and how one attempt is opened and closed.
//
// The cycle is three transactions rather than one, and that is not a style
// choice. now() is frozen at the start of a transaction, so a long recovery
// pass inside the same transaction as a claim would hand out leases that were
// already half spent by the time anyone looked at them.

// ExpireDueIntents ends the commitments whose deadline passed with nothing
// sent.
//
// A leased row is taken too. The lease is data, not a lock: its holder finds
// out at its next compare-and-set, which is the same way it finds out about
// every other decision made while it was away.
func (s *Store) ExpireDueIntents(ctx context.Context, family string, limit int) ([]outbound.Expired, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		UPDATE outbound_intents
		SET status = 'expired', lease_token = NULL, locked_until = NULL,
		    worker_id = NULL, updated_at = now()
		WHERE id IN (
			SELECT id FROM outbound_intents
			WHERE delivery_family = $1 AND status = 'pending' AND expires_at <= now()
			ORDER BY expires_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, COALESCE(alert_group_id, '')`, family, limit)
	if err != nil {
		return nil, fmt.Errorf("expire what is due: %w", err)
	}

	var expired []outbound.Expired
	for rows.Next() {
		var e outbound.Expired
		if err := rows.Scan(&e.IntentID, &e.AlertGroupID); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, e := range expired {
		// A commitment that ended without a single attempt leaves nothing in
		// the journal but a changed status, which is not an explanation.
		if err := appendIntentEventTx(ctx, tx, e.IntentID, nextEventSeq, "expired",
			"the deadline passed before anything was sent", ""); err != nil {
			return nil, err
		}
		if e.AlertGroupID != "" {
			if err := addTimelineTx(ctx, tx, e.AlertGroupID,
				model.TimelineEventNotificationFailed,
				"A notification expired before it could be sent", "system"); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return expired, nil
}

// DueSnapshot answers two different questions in one pass over the queue.
//
// The scheduler needs the demand it can actually take: work whose deadline is
// alive and whose lease is free. The health signal needs the lateness of ALL
// due work, leased or not - a row claimed by an instance that then hung is
// exactly the failure a queue metric must not hide. Answering both from one
// number would either idle the pool or hide the outage.
func (s *Store) DueSnapshot(ctx context.Context, family string) ([]outbound.ProviderDue, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider,
		       count(*) FILTER (
		           WHERE (expires_at IS NULL OR expires_at > now())
		             AND (locked_until IS NULL OR locked_until <= now())) AS claimable_due,
		       count(*) FILTER (
		           WHERE (expires_at IS NULL OR expires_at > now())
		             AND (locked_until IS NULL OR locked_until <= now())
		             AND attempts_in_generation = 0) AS claimable_fresh,
		       EXTRACT(EPOCH FROM (now() - min(next_attempt_at))) AS lateness_seconds
		FROM outbound_intents
		WHERE delivery_family = $1 AND status = 'pending' AND next_attempt_at <= now()
		GROUP BY provider`, family)
	if err != nil {
		return nil, fmt.Errorf("read the queue: %w", err)
	}
	defer rows.Close()

	var out []outbound.ProviderDue
	for rows.Next() {
		var due outbound.ProviderDue
		if err := rows.Scan(&due.Provider, &due.ClaimableDue, &due.ClaimableFresh,
			&due.LatenessSeconds); err != nil {
			return nil, err
		}
		out = append(out, due)
	}
	return out, rows.Err()
}

// ClaimDueIntents takes work for one provider, under a fresh lease each time.
//
// Every claim mints a NEW token. That is the fencing: a worker that comes back
// from the dead still holds the old one, and every mutation it attempts is
// compared against the token the row carries now.
func (s *Store) ClaimDueIntents(ctx context.Context, family, provider string,
	phase outbound.ClaimPhase, limit int, lease time.Duration) ([]outbound.Leased, error) {

	if limit <= 0 {
		return nil, nil
	}

	freshOnly := phase == outbound.ClaimFirstAttempts
	rows, err := s.db.QueryContext(ctx, `
		UPDATE outbound_intents i
		SET lease_token = gen_random_uuid()::text,
		    locked_until = statement_timestamp() + make_interval(secs => $5),
		    worker_id = $6,
		    updated_at = now()
		FROM (
			SELECT id FROM outbound_intents
			WHERE delivery_family = $1 AND provider = $2 AND status = 'pending'
			  AND next_attempt_at <= now()
			  AND (expires_at IS NULL OR expires_at > now())
			  AND (locked_until IS NULL OR locked_until <= now())
			  AND (NOT $3::boolean OR attempts_in_generation = 0)
			ORDER BY next_attempt_at, id
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		) due
		WHERE i.id = due.id
		RETURNING i.id, i.lease_token, i.locked_until`,
		family, provider, freshOnly, limit, lease.Seconds(), workerIDOf(ctx))
	if err != nil {
		return nil, fmt.Errorf("claim due work: %w", err)
	}
	defer rows.Close()

	type claimed struct {
		id    string
		token string
		until time.Time
	}
	var taken []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.token, &c.until); err != nil {
			return nil, err
		}
		taken = append(taken, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]outbound.Leased, 0, len(taken))
	for _, c := range taken {
		intent, err := s.GetIntent(ctx, c.id)
		if err != nil {
			return nil, err
		}
		if intent == nil {
			continue
		}
		out = append(out, outbound.Leased{
			Intent: *intent, LeaseToken: c.token, LockedUntil: c.until,
		})
	}
	return out, nil
}

// workerIDContextKey carries the identity of the process that is claiming. It
// is audit only - the lease is the token, never the name.
type workerIDContextKey struct{}

// WithWorkerID labels the claims made under this context.
func WithWorkerID(ctx context.Context, workerID string) context.Context {
	return context.WithValue(ctx, workerIDContextKey{}, workerID)
}

func workerIDOf(ctx context.Context) any {
	if id, ok := ctx.Value(workerIDContextKey{}).(string); ok && id != "" {
		return id
	}
	return nil
}

// RecoverStaleAttempts closes the attempts whose worker never came back.
//
// One short unit of work per candidate, never one transaction for the batch: a
// batch would hold the alert groups of every commitment in it, and the
// acknowledgement of an unrelated alert would wait behind a recovery it has
// nothing to do with.
func (s *Store) RecoverStaleAttempts(ctx context.Context, family string, limit int) ([]outbound.Recovered, error) {
	type candidate struct {
		intentID  string
		attemptID string
		groupID   string
		policy    outbound.AmbiguityPolicy
	}

	// An unlocked read: the guards are re-checked under the lock below, so a
	// candidate that stops qualifying between the two is skipped rather than
	// mishandled.
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, current_attempt_id, COALESCE(alert_group_id, ''), ambiguity_policy
		FROM outbound_intents
		WHERE delivery_family = $1 AND status = 'sending' AND locked_until <= now()
		ORDER BY locked_until, id
		LIMIT $2`, family, limit)
	if err != nil {
		return nil, fmt.Errorf("look for abandoned attempts: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.intentID, &c.attemptID, &c.groupID, &c.policy); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var recovered []outbound.Recovered
	for _, c := range candidates {
		// The group is locked first for anything that could end up writing to
		// it - here, a policy that turns doubt into a success.
		lockGroup := c.groupID != "" && c.policy == outbound.PolicyAssumeAccepted

		result, err := s.recoverOne(ctx, c.intentID, c.attemptID, c.groupID, lockGroup)
		if err != nil {
			return recovered, err
		}
		if result != nil {
			recovered = append(recovered, *result)
		}
	}
	return recovered, nil
}

func (s *Store) recoverOne(ctx context.Context, intentID, attemptID, groupID string,
	lockGroup bool) (*outbound.Recovered, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := setLockTimeoutTx(ctx, tx); err != nil {
		return nil, err
	}
	if lockGroup {
		if err := lockAlertGroupTx(ctx, tx, groupID); err != nil {
			return nil, err
		}
	}

	intent, expired, err := lockIntentTx(ctx, tx, intentID)
	if err != nil {
		return nil, err
	}
	if intent == nil {
		return nil, nil
	}

	// Re-checked under the lock: the worker may have come back and finished it,
	// or another recovery may have got here first. Either way this one has
	// nothing left to do.
	var currentAttempt sql.NullString
	var stale bool
	if err := tx.QueryRowContext(ctx, `
		SELECT current_attempt_id, COALESCE(locked_until <= now(), FALSE)
		FROM outbound_intents WHERE id = $1`, intentID).Scan(&currentAttempt, &stale); err != nil {
		return nil, err
	}
	if intent.Status != outbound.StatusSending || !stale ||
		!currentAttempt.Valid || currentAttempt.String != attemptID {
		return nil, nil
	}

	// The history of a call whose fate is unknown is written BEFORE anything
	// else is decided: a deadline that arrives in the same moment must not be
	// able to erase the fact that somebody may have received a message.
	var attemptRevision sql.NullInt64
	res, err := tx.ExecContext(ctx, `
		UPDATE outbound_attempts
		SET finished_at = now(), outcome = 'ambiguous', finish_reason = 'lease_lost',
		    error_class = 'lease_lost',
		    completion_fingerprint = $2
		WHERE id = $1 AND finished_at IS NULL`, attemptID, leaseLostFingerprint())
	if err != nil {
		return nil, fmt.Errorf("close the abandoned attempt: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// Somebody closed it between the scan and the lock.
		return nil, nil
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT applied_revision FROM outbound_attempts WHERE id = $1`, attemptID).
		Scan(&attemptRevision); err != nil {
		return nil, err
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:          *intent,
		Trigger:         outbound.TriggerRecoverStale,
		AttemptRevision: attemptRevision.Int64,
		Expired:         expired,
	})
	if err != nil {
		return nil, err
	}

	if err := applyTransitionTx(ctx, tx, transitionWrite{
		Intent:          *intent,
		Transition:      transition,
		Backoff:         outbound.Backoff(intent.FailureStreak + 1),
		AppliedRevision: attemptRevision.Int64,
		Actor:           "recovery",
		Reason:          "the lease expired with an attempt in flight",
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &outbound.Recovered{
		IntentID: intentID, AttemptID: attemptID,
		To: transition.To, Row: transition.Row,
	}, nil
}

// leaseLostFingerprint is the conclusion recovery records: an ambiguous outcome
// and nothing else known about it.
func leaseLostFingerprint() []byte {
	class := "lease_lost"
	sum, err := (keys.Completion{
		Outcome:    keys.OutcomeAmbiguous,
		ErrorClass: &class,
	}).Fingerprint(keys.CurrentCompletionFingerprintVersion())
	if err != nil {
		// The inputs are constants; a failure here is a broken build, not a
		// runtime condition.
		panic(fmt.Sprintf("outbound: the lease-lost conclusion cannot be fingerprinted: %v", err))
	}
	return sum
}

// BeginAttempt opens one attempt, or records why none was made.
//
// The attempt row is written BEFORE the network is touched, which is the
// promise the whole domain rests on: every call that might have happened has a
// record that says so. The converse does not hold - a record whose process died
// before the call is legitimate, and is exactly why an abandoned attempt can
// only be resolved as ambiguous.
func (s *Store) BeginAttempt(ctx context.Context,
	req outbound.BeginAttemptRequest) (outbound.BeginAttemptResult, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}
	defer tx.Rollback()

	if err := setLockTimeoutTx(ctx, tx); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	intent, _, err := lockIntentTx(ctx, tx, req.IntentID)
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}
	if intent == nil {
		return outbound.BeginAttemptResult{Outcome: outbound.BeginNotFound}, nil
	}

	var (
		token          sql.NullString
		lockedUntil    sql.NullTime
		leaseAlive     bool
		currentAttempt sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT lease_token, locked_until,
		       COALESCE(locked_until > now(), FALSE), current_attempt_id
		FROM outbound_intents WHERE id = $1`, req.IntentID).
		Scan(&token, &lockedUntil, &leaseAlive, &currentAttempt); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	switch {
	case intent.Status == outbound.StatusSending && token.String == req.LeaseToken &&
		currentAttempt.Valid:
		// The reply to a previous begin was lost. The network is not authorised
		// twice: nothing here can prove whether the provider was already
		// called, and recovery closing the open attempt as ambiguous is the
		// conservative answer.
		return outbound.BeginAttemptResult{
			Outcome: outbound.BeginUncertain, AttemptID: currentAttempt.String,
		}, nil

	case intent.Status.Terminal() || intent.Status == outbound.StatusManualReview:
		return outbound.BeginAttemptResult{Outcome: outbound.BeginIntentFinalized}, nil

	case intent.Status != outbound.StatusPending || !token.Valid ||
		token.String != req.LeaseToken || !leaseAlive:
		return outbound.BeginAttemptResult{Outcome: outbound.BeginLeaseLost}, nil
	}

	if req.Preparation != outbound.PreparationReady {
		return s.recordPreparation(ctx, tx, req, *intent)
	}

	snapshot, revision, err := lockedSnapshotTx(ctx, tx, intent.AlertGroupID)
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	var payload json.RawMessage
	if err := tx.QueryRowContext(ctx,
		`SELECT payload FROM outbound_intents WHERE id = $1`, req.IntentID).
		Scan(&payload); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:      *intent,
		Trigger:     outbound.TriggerPreparation,
		Preparation: outbound.PreparationReady,
	})
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	attemptNo, err := nextAttemptNoTx(ctx, tx, req.IntentID)
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	attemptID := uuid.New().String()
	fingerprintVersion := keys.CurrentCompletionFingerprintVersion()

	// The attempt first, then the commitment that points at it: the composite
	// foreign key insists the target exists, and this is the order that
	// satisfies it without deferring anything.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_attempts (
			id, intent_id, attempt_no, record_kind, generation_no, attempt_kind,
			operation, applied_revision, provider, bound_endpoint, provider_key,
			request_fingerprint, lease_token, worker_id, started_at,
			completion_fingerprint_version)
		VALUES ($1, $2, $3, 'attempt', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now(), $14)`,
		attemptID, req.IntentID, attemptNo, intent.GenerationNo, string(req.AttemptKind),
		string(req.Operation), revision, intent.Provider, nilIfEmpty(req.BoundEndpoint),
		nilIfEmpty(req.ProviderKey), snapshot.Digest(), req.LeaseToken,
		nilIfEmpty(req.WorkerID), fingerprintVersion,
	); err != nil {
		return outbound.BeginAttemptResult{}, fmt.Errorf("open the attempt: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_intents
		SET status = 'sending', current_attempt_id = $2,
		    attempts_in_generation = attempts_in_generation + 1,
		    bound_endpoint = COALESCE(bound_endpoint, $3),
		    create_key = COALESCE(create_key, $4),
		    updated_at = now()
		WHERE id = $1`,
		req.IntentID, attemptID, nilIfEmpty(req.BoundEndpoint), nilIfEmpty(req.ProviderKey),
	); err != nil {
		return outbound.BeginAttemptResult{}, fmt.Errorf("mark the commitment as sending: %w", err)
	}

	if transition.Effects.OpenGeneration {
		if err := appendIntentEventTx(ctx, tx, req.IntentID, nextEventSeq,
			"generation_opened", "the external effect was bound to an address",
			req.WorkerID); err != nil {
			return outbound.BeginAttemptResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	return outbound.BeginAttemptResult{
		Outcome:                      outbound.BeginStarted,
		AttemptID:                    attemptID,
		AttemptNo:                    attemptNo,
		GenerationNo:                 intent.GenerationNo,
		AppliedRevision:              revision,
		Snapshot:                     snapshot,
		Payload:                      payload,
		CompletionFingerprintVersion: fingerprintVersion,
		Intent:                       *intent,
	}, nil
}

// recordPreparation writes the proof that no call was made.
//
// It is not an attempt and must never look like one: an attempt row means the
// network might have been reached, and inventing that doubt would turn a
// provable refusal into a possible duplicate.
func (s *Store) recordPreparation(ctx context.Context, tx *sql.Tx,
	req outbound.BeginAttemptRequest, intent outbound.Intent) (outbound.BeginAttemptResult, error) {

	transition, err := outbound.Decide(outbound.Input{
		Intent:      intent,
		Trigger:     outbound.TriggerPreparation,
		Preparation: req.Preparation,
	})
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	attemptNo, err := nextAttemptNoTx(ctx, tx, req.IntentID)
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	outcome := string(outbound.OutcomePermanentRejection)
	if req.Preparation == outbound.PreparationTransient {
		outcome = string(outbound.OutcomeRetryableRejection)
	}

	recordID := uuid.New().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_attempts (
			id, intent_id, attempt_no, record_kind, generation_no, attempt_kind,
			operation, provider, worker_id, finished_at, outcome, error_class,
			response_summary, finish_reason)
		VALUES ($1, $2, $3, 'preparation', $4, $5, $6, $7, $8, now(), $9, $10, $11, 'preparation')`,
		recordID, req.IntentID, attemptNo, intent.GenerationNo, string(req.AttemptKind),
		string(req.Operation), intent.Provider, nilIfEmpty(req.WorkerID),
		outcome, nilIfEmpty(req.ErrorClass), nilIfEmpty(req.Summary),
	); err != nil {
		return outbound.BeginAttemptResult{}, fmt.Errorf("record the refusal: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE outbound_intents SET attempts_in_generation = attempts_in_generation + 1
		 WHERE id = $1`, req.IntentID); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	if err := applyTransitionTx(ctx, tx, transitionWrite{
		Intent:     intent,
		Transition: transition,
		Backoff:    outbound.Backoff(intent.FailureStreak + 1),
		Actor:      req.WorkerID,
		Reason:     req.ErrorClass,
	}); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	result := outbound.BeginAttemptResult{
		Outcome: outbound.BeginPreparedRetry, AttemptID: recordID, AttemptNo: attemptNo,
	}
	if req.Preparation == outbound.PreparationPermanent {
		result.Outcome = outbound.BeginPreparedPermanent
	}
	return result, nil
}

// FinalizeDeliveryAttempt closes an attempt and moves the commitment with it,
// in one commit that also writes to the alert group.
//
// The classification below starts from the ATTEMPT, not from the commitment's
// lease token, and the order is load-bearing: a successful finalisation clears
// the commitment's token, so a repeat after a lost commit reply has no token to
// match and would look like a stranger. What proves ownership is the token
// stored on the attempt, which nothing rewrites.
func (s *Store) FinalizeDeliveryAttempt(ctx context.Context,
	req outbound.FinalizeRequest) (outbound.FinalizeResult, error) {

	fingerprint, err := req.Completion.Fingerprint(keys.CurrentCompletionFingerprintVersion())
	if err != nil {
		return outbound.FinalizeResult{}, err
	}

	// Read before the transaction: both are immutable, and the lock order
	// depends on them.
	var (
		intentID    string
		groupID     string
		policy      outbound.AmbiguityPolicy
		attemptFind = `SELECT intent_id, COALESCE(i.alert_group_id, ''), i.ambiguity_policy
		               FROM outbound_attempts a JOIN outbound_intents i ON i.id = a.intent_id
		               WHERE a.id = $1`
	)
	err = s.db.QueryRowContext(ctx, attemptFind, req.AttemptID).Scan(&intentID, &groupID, &policy)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.FinalizeResult{Outcome: outbound.FinalizeNotFound}, nil
	}
	if err != nil {
		return outbound.FinalizeResult{}, err
	}

	// The group is locked first by anything that could end up writing to it: a
	// success, or doubt that a policy turns into one.
	lockGroup := groupID != "" &&
		(req.Completion.Outcome == keys.OutcomeAccepted ||
			(req.Completion.Outcome == keys.OutcomeAmbiguous && policy == outbound.PolicyAssumeAccepted))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.FinalizeResult{}, err
	}
	defer tx.Rollback()

	if err := setLockTimeoutTx(ctx, tx); err != nil {
		return outbound.FinalizeResult{}, err
	}
	if lockGroup {
		if err := lockAlertGroupTx(ctx, tx, groupID); err != nil {
			return outbound.FinalizeResult{}, err
		}
	}

	intent, _, err := lockIntentTx(ctx, tx, intentID)
	if err != nil {
		return outbound.FinalizeResult{}, err
	}
	if intent == nil {
		return outbound.FinalizeResult{Outcome: outbound.FinalizeNotFound}, nil
	}

	var (
		attemptToken   sql.NullString
		finishedAt     sql.NullTime
		finishReason   sql.NullString
		storedPrint    []byte
		currentAttempt sql.NullString
		intentToken    sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT a.lease_token, a.finished_at, a.finish_reason, a.completion_fingerprint,
		       i.current_attempt_id, i.lease_token
		FROM outbound_attempts a JOIN outbound_intents i ON i.id = a.intent_id
		WHERE a.id = $1`, req.AttemptID).
		Scan(&attemptToken, &finishedAt, &finishReason, &storedPrint,
			&currentAttempt, &intentToken); err != nil {
		return outbound.FinalizeResult{}, err
	}

	// The token gate comes first. A caller that cannot prove the result is
	// theirs does not get to record a transport observation either: a false
	// "accepted" from a stranger would be indistinguishable from a real one.
	if !attemptToken.Valid || attemptToken.String != req.LeaseToken {
		if err := tx.Commit(); err != nil {
			return outbound.FinalizeResult{}, err
		}
		return outbound.FinalizeResult{Outcome: outbound.FinalizeLeaseLost}, nil
	}

	if finishedAt.Valid {
		switch finishReason.String {
		case "worker":
			if bytes.Equal(storedPrint, fingerprint) {
				if err := tx.Commit(); err != nil {
					return outbound.FinalizeResult{}, err
				}
				return outbound.FinalizeResult{Outcome: outbound.FinalizeIdempotentRepeat}, nil
			}
			if err := tx.Commit(); err != nil {
				return outbound.FinalizeResult{}, err
			}
			return outbound.FinalizeResult{Outcome: outbound.FinalizeConflict}, nil

		case "lease_lost":
			// A genuine late result from the worker that owned this attempt.
			// It may be the only evidence that the effect happened, so it is
			// kept - durably, not as a log line.
			recorded, err := recordObservationTx(ctx, tx, req, fingerprint)
			if err != nil {
				return outbound.FinalizeResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return outbound.FinalizeResult{}, err
			}
			return outbound.FinalizeResult{
				Outcome: outbound.FinalizeLeaseLost, ObservationRecorded: recorded,
			}, nil

		default:
			return outbound.FinalizeResult{}, outboundContractf(
				"attempt %s was finished by %q", req.AttemptID, finishReason.String)
		}
	}

	if !currentAttempt.Valid || currentAttempt.String != req.AttemptID ||
		!intentToken.Valid || intentToken.String != attemptToken.String {
		return outbound.FinalizeResult{}, outboundContractf(
			"attempt %s is open but its commitment points elsewhere", req.AttemptID)
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:          *intent,
		Trigger:         outbound.TriggerFinishAttempt,
		Outcome:         req.Completion.Outcome,
		AttemptRevision: revisionOf(req.Completion.AppliedRevision),
		AttemptIsFinal:  req.AttemptIsFinal,
	})
	if err != nil {
		return outbound.FinalizeResult{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_attempts
		SET finished_at = now(), outcome = $2, error_class = $3, provider_status = $4,
		    receipt = $5, response_summary = $6, finish_reason = 'worker',
		    completion_fingerprint = $7
		WHERE id = $1 AND finished_at IS NULL`,
		req.AttemptID, string(req.Completion.Outcome), req.Completion.ErrorClass,
		req.Completion.ProviderStatus, nullableJSON(req.Receipt), nilIfEmpty(req.Summary),
		fingerprint,
	); err != nil {
		return outbound.FinalizeResult{}, fmt.Errorf("close the attempt: %w", err)
	}

	if err := applyTransitionTx(ctx, tx, transitionWrite{
		Intent:          *intent,
		Transition:      transition,
		Backoff:         outbound.Backoff(intent.FailureStreak + 1),
		AppliedRevision: revisionOf(req.Completion.AppliedRevision),
		AttemptIsFinal:  req.AttemptIsFinal,
		Receipt:         req.Receipt,
		Actor:           "worker",
	}); err != nil {
		return outbound.FinalizeResult{}, err
	}

	// The alert group learns about the delivery in the same commit, including
	// its first successful send moving it out of processing. Split in two, a
	// crash between them would leave a group that looks unpaged and a delivery
	// that says otherwise.
	if intent.GroupBound() {
		if err := groupEffectsTx(ctx, tx, intent.AlertGroupID, transition); err != nil {
			return outbound.FinalizeResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return outbound.FinalizeResult{}, err
	}
	return outbound.FinalizeResult{
		Outcome: outbound.FinalizeFinalized, To: transition.To, Row: transition.Row,
	}, nil
}

// recordObservationTx keeps a late result for an attempt somebody else closed.
//
// Identity is the attempt and the kind; the fingerprint is content. Two
// contradicting late results are a conflict rather than two truths, and the
// first one stands.
func recordObservationTx(ctx context.Context, tx *sql.Tx,
	req outbound.FinalizeRequest, fingerprint []byte) (bool, error) {

	var storedPrint []byte
	err := tx.QueryRowContext(ctx, `
		SELECT completion_fingerprint FROM outbound_attempt_observations
		WHERE attempt_id = $1 AND observation_kind = 'late_finalize'`, req.AttemptID).
		Scan(&storedPrint)
	switch {
	case err == nil:
		if bytes.Equal(storedPrint, fingerprint) {
			return true, nil
		}
		return false, outboundContractf(
			"two different late results for attempt %s", req.AttemptID)
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_attempt_observations (
			id, attempt_id, observation_kind, outcome, error_class, provider_status,
			receipt, applied_revision, provider_result_detail, response_summary,
			completion_fingerprint, completion_fingerprint_version)
		VALUES ($1, $2, 'late_finalize', $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		uuid.New().String(), req.AttemptID, string(req.Completion.Outcome),
		req.Completion.ErrorClass, req.Completion.ProviderStatus,
		nullableJSON(req.Receipt), req.Completion.AppliedRevision,
		detailOf(req.Completion.ProviderResultDetail), nilIfEmpty(req.Summary),
		fingerprint, keys.CurrentCompletionFingerprintVersion(),
	); err != nil {
		return false, fmt.Errorf("keep the late result: %w", err)
	}
	return true, nil
}

func detailOf(detail *keys.ProviderResultDetail) any {
	if detail == nil {
		return nil
	}
	return string(*detail)
}

func revisionOf(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
