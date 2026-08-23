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
		// Set-based, so the machine cannot be asked per row - but the alert
		// still hears exactly what T20 says it hears.
		if e.AlertGroupID != "" {
			message, eventType, ok := timelineLine(outbound.TimelineExpired)
			if !ok {
				return nil, outboundContractf("an expiry has no wording for the alert")
			}
			if err := addTimelineTx(ctx, tx, e.AlertGroupID, eventType, message,
				"system"); err != nil {
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
func (s *Store) ClaimDueIntents(ctx context.Context,
	req outbound.ClaimRequest) ([]outbound.Leased, error) {

	if req.Limit <= 0 {
		return nil, nil
	}

	statement, err := claimStatement(req.Phase)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, statement,
		req.Family, req.Provider, req.Limit, req.Lease.Seconds(),
		nilIfEmpty(req.WorkerID))
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

// claimStatement is the claim for one phase.
//
// Two statements rather than one with a flag, because the flag is what stops
// the planner: a predicate hidden behind a parameter cannot be matched to an
// index, so the one statement that served both phases had to read and sort the
// whole due set of a provider to answer either of them. Spelled separately,
// each phase is a plain index scan.
//
// The phases are a closed set and an unrecognised one is refused rather than
// quietly treated as the broader of the two: handing out leases for work the
// caller did not ask for is not a default anybody would choose on purpose.
func claimStatement(phase outbound.ClaimPhase) (string, error) {
	const shape = `
		UPDATE outbound_intents i
		SET lease_token = gen_random_uuid()::text,
		    locked_until = statement_timestamp() + make_interval(secs => $4),
		    worker_id = $5,
		    updated_at = now()
		FROM (
			SELECT id FROM outbound_intents
			WHERE delivery_family = $1 AND provider = $2 AND status = 'pending'
			  AND next_attempt_at <= now()
			  AND (expires_at IS NULL OR expires_at > now())
			  AND (locked_until IS NULL OR locked_until <= now())
			  %s
			ORDER BY %s
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		) due
		WHERE i.id = due.id
		RETURNING i.id, i.lease_token, i.locked_until`

	switch phase {
	case outbound.ClaimFirstAttempts:
		return fmt.Sprintf(shape,
			"AND attempts_in_generation = 0", "next_attempt_at, id"), nil

	case outbound.ClaimRetriesFirst:
		// FALSE sorts first, so a commitment that has already been attempted
		// comes before one that has not - which is what makes the share the
		// scheduler reserves for retries actually reach them. Without it, a
		// backlog of untried work that is older than the retries takes that
		// share too, and the oldest retry never goes out at all.
		return fmt.Sprintf(shape,
			"", "(attempts_in_generation = 0), next_attempt_at, id"), nil

	default:
		return "", outboundContractf("claim phase %q is not one this build takes", phase)
	}
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

	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
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
	var attemptVersion int
	if err := tx.QueryRowContext(ctx, `
		SELECT applied_revision, COALESCE(completion_fingerprint_version, 0)
		FROM outbound_attempts WHERE id = $1`, attemptID).
		Scan(&attemptRevision, &attemptVersion); err != nil {
		return nil, err
	}

	// Closed under the protocol the attempt was opened with: the worker that
	// may yet come back will compute its own conclusion the same way, and the
	// two have to be comparable.
	conclusion, err := leaseLostFingerprint(attemptVersion)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE outbound_attempts
		SET finished_at = now(), outcome = 'ambiguous', finish_reason = 'lease_lost',
		    error_class = 'lease_lost',
		    completion_fingerprint = $2
		WHERE id = $1 AND finished_at IS NULL`, attemptID, conclusion)
	if err != nil {
		return nil, fmt.Errorf("close the abandoned attempt: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// Somebody closed it between the scan and the lock.
		return nil, nil
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
func leaseLostFingerprint(version int) ([]byte, error) {
	class := "lease_lost"
	return (keys.Completion{
		Outcome:    keys.OutcomeAmbiguous,
		ErrorClass: &class,
	}).Fingerprint(version)
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

	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
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

	// What the call has to be, decided before anything is written and from the
	// commitment alone. A refusal is recorded under the same shape: the journal
	// says which call did not happen.
	plan, err := planAttempt(*intent)
	if err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	if req.Preparation != outbound.PreparationReady {
		return s.recordPreparation(ctx, tx, req, *intent, plan)
	}

	stored, err := lockedSnapshotTx(ctx, tx, intent.AlertGroupID)
	if err != nil {
		if errors.Is(err, ErrUndeliverable) {
			return s.refuseAttempt(ctx, tx, req, *intent, plan, "state_unreadable", err.Error())
		}
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
	if err := beginEffectsUnderstood(transition.Effects); err != nil {
		return outbound.BeginAttemptResult{}, err
	}

	effect, err := bindGenerationTx(ctx, tx, *intent, req.BoundEndpoint,
		transition.Effects.OpenGeneration)
	if err != nil {
		if errors.Is(err, ErrUndeliverable) {
			return s.refuseAttempt(ctx, tx, req, *intent, plan, "binding_lost", err.Error())
		}
		return outbound.BeginAttemptResult{}, err
	}

	var payload json.RawMessage
	if err := tx.QueryRowContext(ctx,
		`SELECT payload FROM outbound_intents WHERE id = $1`, req.IntentID).
		Scan(&payload); err != nil {
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
	//
	// The attempt records the address and key it will ACTUALLY use, which are
	// the generation's, not the ones this worker proposed. Recording the
	// proposal would leave a journal claiming a message went somewhere it
	// never went.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_attempts (
			id, intent_id, attempt_no, record_kind, generation_no, attempt_kind,
			operation, applied_revision, provider, bound_endpoint, provider_key,
			request_fingerprint, lease_token, worker_id, started_at,
			completion_fingerprint_version)
		VALUES ($1, $2, $3, 'attempt', $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now(), $14)`,
		attemptID, req.IntentID, attemptNo, intent.GenerationNo, string(plan.Kind),
		string(plan.Operation), stored.Revision, intent.Provider, effect.Endpoint,
		effect.ProviderKey, stored.Snapshot.Digest(), req.LeaseToken,
		nilIfEmpty(req.WorkerID), fingerprintVersion,
	); err != nil {
		return outbound.BeginAttemptResult{}, fmt.Errorf("open the attempt: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_intents
		SET status = 'sending', current_attempt_id = $2,
		    attempts_in_generation = attempts_in_generation + 1,
		    updated_at = now()
		WHERE id = $1`, req.IntentID, attemptID); err != nil {
		return outbound.BeginAttemptResult{}, fmt.Errorf("mark the commitment as sending: %w", err)
	}

	if transition.Effects.OpenGeneration {
		if err := appendIntentEventTx(ctx, tx, req.IntentID, nextEventSeq,
			"effect_bound", "the address and key of this generation are settled",
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
		AttemptKind:                  plan.Kind,
		Operation:                    plan.Operation,
		BoundEndpoint:                effect.Endpoint,
		ProviderKey:                  effect.ProviderKey,
		AppliedRevision:              stored.Revision,
		Snapshot:                     stored.Snapshot,
		Payload:                      payload,
		PayloadSchemaVersion:         intent.PayloadSchemaVersion,
		CompletionFingerprintVersion: fingerprintVersion,
		Intent:                       *intent,
	}, nil
}

// beginEffectsUnderstood refuses a transition asking for something this path
// cannot write.
//
// Opening an attempt is not a status change: it inserts the journal row, points
// the commitment at it and counts it, none of which the shared writer can
// express - so this is the one path that applies its own transition, and the
// one place an effect could be dropped without anybody noticing. If T4 ever
// grows a second effect, this says so loudly instead of sending a message with
// half a rule applied.
func beginEffectsUnderstood(e outbound.Effects) error {
	rest := e
	rest.OpenGeneration = false
	if rest != (outbound.Effects{}) {
		return outboundContractf(
			"starting an attempt cannot write the effects %+v this transition asks for", rest)
	}
	return nil
}

// plannedAttempt is what a call has to be. Every field of it is derived from
// the commitment's own state, which is what makes the identity of the effect
// something the system decides rather than something a worker reports.
type plannedAttempt struct {
	Kind      outbound.AttemptKind
	Operation outbound.Operation
}

// planAttempt decides the shape of the next call.
//
// The rule is the receipt. Nothing out there yet means a create; an object that
// already exists is changed, never created a second time. The revision is not
// here at all - it comes from the state the attempt renders, read under the
// same lock, so the content and the key that names it cannot disagree.
//
// Changing an object that already exists is not something this build does, and
// it stops the caller rather than the commitment. Nothing in this build brings
// a commitment that has a receipt back into the queue, so the only way to be
// here is a deployment that is behind - and ending perfectly good work because
// this instance is the wrong one to do it is exactly the mistake the two error
// kinds above exist to keep apart.
//
// What Sprint 2 adds is the call itself and the choice between an update and a
// resolve, which is a question about the stored state - is this the last
// revision? - and so can only be answered after that state has been read and
// proved. Recording a guess here would put a call in the journal that the
// system never decided to make.
func planAttempt(intent outbound.Intent) (plannedAttempt, error) {
	if intent.HasReceipt {
		return plannedAttempt{}, outboundContractf(
			"commitment %s needs a change to an object that already exists, "+
				"and this build only creates them", intent.ID)
	}
	return plannedAttempt{
		Kind:      outbound.AttemptCreate,
		Operation: outbound.OperationSend,
	}, nil
}

// boundEffect is the external identity one attempt will use: where it goes and
// under what key the provider may deduplicate it.
type boundEffect struct {
	Endpoint    string
	ProviderKey string
}

// bindGenerationTx settles the address and the key of the current external
// effect - once, when it opens - and hands back what every later attempt of
// that effect must reuse.
//
// This is the invariant the whole generation exists for. A retry after a doubtful
// call must ask the provider to create the SAME thing at the SAME address: if a
// recipient's identity was relinked in between, sending to the new address would
// deliver twice, to two different people, with nobody able to tell which one
// got it. So the worker's freshly resolved address is a proposal, and a bound
// generation ignores it.
func bindGenerationTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent,
	proposed string, opening bool) (boundEffect, error) {

	if opening {
		if proposed == "" {
			return boundEffect{}, outboundContractf(
				"opening an effect for %s with no address to send to", intent.ID)
		}
		key, err := keys.CreateKey(intent.ID, intent.GenerationNo, intent.ProviderKeyCodecVersion)
		if err != nil {
			return boundEffect{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE outbound_intents SET bound_endpoint = $2, create_key = $3
			WHERE id = $1`, intent.ID, proposed, key); err != nil {
			return boundEffect{}, fmt.Errorf("bind the effect of %s: %w", intent.ID, err)
		}
		return boundEffect{Endpoint: proposed, ProviderKey: key}, nil
	}

	var storedEndpoint, storedKey sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT bound_endpoint, create_key FROM outbound_intents WHERE id = $1`,
		intent.ID).Scan(&storedEndpoint, &storedKey); err != nil {
		return boundEffect{}, err
	}
	if !storedEndpoint.Valid || storedEndpoint.String == "" || !storedKey.Valid {
		return boundEffect{}, undeliverablef(
			"commitment %s has a bound effect with no address or no key", intent.ID)
	}
	return boundEffect{Endpoint: storedEndpoint.String, ProviderKey: storedKey.String}, nil
}

// refuseAttempt ends a commitment the store itself cannot deliver.
//
// It is the same answer a worker gives when it finds the configuration broken -
// no call was made, and none will be until somebody changes something - and it
// is recorded the same way, because it is the same fact. What differs is only
// who found out.
//
// Leaving the commitment pending would be worse than the failure it is
// refusing. The queue would hand out the same broken row forever, and the one
// door that could withdraw it - an operator decision - opens only for a
// commitment that has stopped moving. A delivery that cannot happen has to end
// somewhere a person can see it, and permanent_failed is that place.
//
// The error is not returned: it is written down. A caller told "the state of
// this alert is corrupt" can do nothing with that, while the journal entry, the
// status and the line in the alert's history reach the people who can.
func (s *Store) refuseAttempt(ctx context.Context, tx *sql.Tx,
	req outbound.BeginAttemptRequest, intent outbound.Intent, plan plannedAttempt,
	class, detail string) (outbound.BeginAttemptResult, error) {

	req.Preparation = outbound.PreparationPermanent
	req.ErrorClass = class
	req.Summary = detail
	return s.recordPreparation(ctx, tx, req, intent, plan)
}

// recordPreparation writes the proof that no call was made.
//
// It is not an attempt and must never look like one: an attempt row means the
// network might have been reached, and inventing that doubt would turn a
// provable refusal into a possible duplicate.
func (s *Store) recordPreparation(ctx context.Context, tx *sql.Tx,
	req outbound.BeginAttemptRequest, intent outbound.Intent,
	plan plannedAttempt) (outbound.BeginAttemptResult, error) {

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
		recordID, req.IntentID, attemptNo, intent.GenerationNo, string(plan.Kind),
		string(plan.Operation), intent.Provider, nilIfEmpty(req.WorkerID),
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
	err := s.db.QueryRowContext(ctx, attemptFind, req.AttemptID).Scan(&intentID, &groupID, &policy)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.FinalizeResult{Outcome: outbound.FinalizeNotFound}, nil
	}
	if err != nil {
		return outbound.FinalizeResult{}, err
	}

	// The group is locked first by anything that could end up writing to it: a
	// success, or doubt that a policy turns into one.
	concluded := req.Conclusion.Completion()
	receipt := req.Conclusion.Receipt()

	lockGroup := groupID != "" &&
		(concluded.Outcome == keys.OutcomeAccepted ||
			(concluded.Outcome == keys.OutcomeAmbiguous && policy == outbound.PolicyAssumeAccepted))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.FinalizeResult{}, err
	}
	defer tx.Rollback()

	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
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
		attemptToken    sql.NullString
		finishedAt      sql.NullTime
		finishReason    sql.NullString
		storedPrint     []byte
		attemptRevision sql.NullInt64
		attemptVersion  int
		currentAttempt  sql.NullString
		intentToken     sql.NullString
	)
	var attemptKind string
	if err := tx.QueryRowContext(ctx, `
		SELECT a.lease_token, a.finished_at, a.finish_reason, a.completion_fingerprint,
		       a.applied_revision, COALESCE(a.completion_fingerprint_version, 0),
		       a.attempt_kind, i.current_attempt_id, i.lease_token
		FROM outbound_attempts a JOIN outbound_intents i ON i.id = a.intent_id
		WHERE a.id = $1`, req.AttemptID).
		Scan(&attemptToken, &finishedAt, &finishReason, &storedPrint,
			&attemptRevision, &attemptVersion, &attemptKind, &currentAttempt,
			&intentToken); err != nil {
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

	// Only now is what the caller SAID looked at, and the first thing looked at
	// is whether it said something it has no standing to say. A result reports
	// what the provider answered, never which revision it was answering about:
	// that is the attempt's own record, and a caller allowed to restate it
	// could file a real reply against a revision the message never carried.
	//
	// The order is the ladder's, not convenience: an unknown attempt is not
	// found and a stranger's token is a lost lease, whatever else their request
	// contains. Reading their claims first would answer a question about the
	// content of a request nobody has established the right to make.
	if concluded.AppliedRevision != nil {
		return outbound.FinalizeResult{}, outboundContractf(
			"a result for attempt %s names the revision it applied; only the attempt does that",
			req.AttemptID)
	}

	// What the result says about the object has to match what the attempt was.
	// The domain builds these together, so a mismatch here is a caller that
	// assembled its own - and the state it would produce is the one nothing
	// recovers from: a commitment settled as delivered with no coordinates,
	// which the next revision reads as "never sent" and creates a second
	// message for.
	//
	// Only a create is held to it. A change to an object that already exists
	// keeps the receipt the commitment already has, and Sprint 2 will need
	// exactly that.
	if (concluded.ReceiptRef != nil) != (len(receipt) > 0) {
		return outbound.FinalizeResult{}, outboundContractf(
			"the result of attempt %s names an object it did not record, or records "+
				"one it will not name", req.AttemptID)
	}
	if concluded.Outcome == keys.OutcomeAccepted &&
		attemptKind == string(outbound.AttemptCreate) && len(receipt) == 0 {
		return outbound.FinalizeResult{}, outboundContractf(
			"attempt %s created something and did not say what", req.AttemptID)
	}

	// The result is completed from the attempt's own row before anything is
	// computed from it: the revision it applied is a fact of the record, and
	// putting it in here is what makes the fingerprint, the observation and the
	// commitment's own state describe the same thing.
	completion := concluded
	if attemptRevision.Valid {
		applied := attemptRevision.Int64
		completion.AppliedRevision = &applied
	}

	// Only now the fingerprint, and under the protocol the ATTEMPT was opened
	// with rather than today's. An attempt can outlive a deployment, and a
	// repeat compared across two protocols would read as a conflict - the one
	// answer that turns a lost commit reply into an incident.
	fingerprint, err := completion.Fingerprint(attemptVersion)
	if err != nil {
		return outbound.FinalizeResult{}, err
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
			recorded, err := recordObservationTx(ctx, tx, observation{
				AttemptID:   req.AttemptID,
				Completion:  completion,
				Receipt:     receipt,
				Summary:     req.Conclusion.Summary(),
				Fingerprint: fingerprint,
				Version:     attemptVersion,
			})
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

	applied := attemptRevision.Int64

	// Whether that revision was the LAST one is a property of the stored state
	// too. Only a success needs the answer: the one doubtful outcome that
	// settles a commitment here is assume_accepted, which the machine allows
	// only for one-shot messages - and a one-shot has no later revision to be
	// waiting for.
	final := false
	if intent.GroupBound() && concluded.Outcome == keys.OutcomeAccepted {
		stored, err := lockedSnapshotTx(ctx, tx, intent.AlertGroupID)
		if err != nil {
			return outbound.FinalizeResult{}, err
		}
		final = stored.Final && stored.Revision == applied
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:          *intent,
		Trigger:         outbound.TriggerFinishAttempt,
		Outcome:         completion.Outcome,
		AttemptRevision: applied,
		AttemptIsFinal:  final,
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
		req.AttemptID, string(completion.Outcome), completion.ErrorClass,
		completion.ProviderStatus, nullableJSON(receipt), nilIfEmpty(req.Conclusion.Summary()),
		fingerprint,
	); err != nil {
		return outbound.FinalizeResult{}, fmt.Errorf("close the attempt: %w", err)
	}

	if err := applyTransitionTx(ctx, tx, transitionWrite{
		Intent:          *intent,
		Transition:      transition,
		Backoff:         outbound.Backoff(intent.FailureStreak + 1),
		AppliedRevision: applied,
		AttemptIsFinal:  final,
		Receipt:         receipt,
		Actor:           "worker",
	}); err != nil {
		return outbound.FinalizeResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return outbound.FinalizeResult{}, err
	}
	return outbound.FinalizeResult{
		Outcome: outbound.FinalizeFinalized, To: transition.To, Row: transition.Row,
	}, nil
}

// observation is a late result on its way to being kept: the conclusion as the
// record has it, and the fingerprint of exactly that.
type observation struct {
	AttemptID   string
	Completion  keys.Completion
	Receipt     json.RawMessage
	Summary     string
	Fingerprint []byte
	Version     int
}

// recordObservationTx keeps a late result for an attempt somebody else closed.
//
// Identity is the attempt and the kind; the fingerprint is content. Two
// contradicting late results are a conflict rather than two truths, and the
// first one stands.
//
// What is stored is the completion as canonicalised from the attempt, so the
// row and the fingerprint that indexes it cannot describe two different things.
func recordObservationTx(ctx context.Context, tx *sql.Tx, o observation) (bool, error) {
	var storedPrint []byte
	err := tx.QueryRowContext(ctx, `
		SELECT completion_fingerprint FROM outbound_attempt_observations
		WHERE attempt_id = $1 AND observation_kind = 'late_finalize'`, o.AttemptID).
		Scan(&storedPrint)
	switch {
	case err == nil:
		if bytes.Equal(storedPrint, o.Fingerprint) {
			return true, nil
		}
		return false, outboundContractf(
			"two different late results for attempt %s", o.AttemptID)
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
		uuid.New().String(), o.AttemptID, string(o.Completion.Outcome),
		o.Completion.ErrorClass, o.Completion.ProviderStatus,
		nullableJSON(o.Receipt), o.Completion.AppliedRevision,
		detailOf(o.Completion.ProviderResultDetail), nilIfEmpty(o.Summary),
		o.Fingerprint, o.Version,
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
