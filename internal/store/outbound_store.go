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

// The outbound delivery operations, as the transactions they have to be.
//
// Everything here follows two rules that are not local decisions:
//
//   - time comes from the database, never from the process. Two instances have
//     different clocks, and a lease or a deadline compared against the wrong one
//     is a lease two workers both believe they hold.
//   - when a unit of work needs both an alert group and a commitment, the group
//     is locked first. Every transaction that can end up writing to a group
//     takes them in that order, which is what keeps acknowledgement and delivery
//     from deadlocking against each other.

// The two ways this package refuses. They are disjoint on purpose - neither
// wraps the other - because they have opposite consequences and a caller has to
// be able to tell them apart in whichever order it asks.
//
// ErrOutboundContract is a valid row this build cannot handle - written under a
// protocol version it does not know, or needing a call it does not make - and
// an invariant that should have been impossible. It stops the caller and
// changes nothing: the same row read by the build it was written for is fine,
// and an old instance must not be able to end work a new format created.
var ErrOutboundContract = errors.New("store: outbound contract violation")

func outboundContractf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrOutboundContract, fmt.Sprintf(format, args...))
}

// ErrUndeliverable is the other one: the row itself is broken, for every build
// and forever. The state a commitment renders from is gone, or no longer
// produces the digest its keys were computed over, or describes another alert.
//
// The consequence differs because the cause does. Stopping the caller over a
// broken row would hand the same row out of the queue for as long as its
// deadline allows, so this one ends the commitment where a person can see it -
// see refuseAttempt. Nothing that could become readable again after a
// deployment belongs here.
//
// It is NOT a contract violation as well. A handler that asked about the
// contract first would read a broken row as an incompatible build and leave the
// commitment circling, which is the failure both of these exist to name.
var ErrUndeliverable = errors.New("store: this commitment cannot produce a call")

func undeliverablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUndeliverable, fmt.Sprintf(format, args...))
}

// SubmitEscalationBatch admits an escalation: the claim, its commitments, the
// state they are about, and the policy the group was escalated by, in one
// commit.
//
// The order inside is the whole design. The group is locked first and written
// LAST, and only by the producer whose claim was accepted: an admission that
// wrote the policy snapshot before knowing whether it won would let the loser
// overwrite the winner's, leaving a group describing one escalation and
// executing another.
func (s *Store) SubmitEscalationBatch(ctx context.Context,
	adm outbound.EscalationAdmission) (outbound.SubmitResult, error) {

	admission := adm.Admission
	if len(admission.Commitments) != 0 && admission.Outcome != keys.OutcomeAdmitted {
		return outbound.SubmitResult{}, outboundContractf(
			"an admission of %d commitments carrying outcome %q",
			len(admission.Commitments), admission.Outcome)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	defer tx.Rollback()

	// The group first, and its clock with it: everything this transaction
	// computes about time is computed from the database's now().
	var status string
	var now time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT status, now() FROM alert_groups WHERE id = $1 FOR UPDATE`,
		admission.AlertGroupID).Scan(&status, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.SubmitResult{}, fmt.Errorf("alert group %s not found", admission.AlertGroupID)
	}
	if err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("lock alert group %s: %w", admission.AlertGroupID, err)
	}

	if err := outbound.ValidateEscalationAdmission(admission, now); err != nil {
		return outbound.SubmitResult{}, err
	}

	// The user is ahead of us: they acknowledged or resolved before this
	// escalation was admitted, and nothing about the group is touched.
	if model.AlertGroupStatus(status) != model.AlertGroupStatusNew &&
		model.AlertGroupStatus(status) != model.AlertGroupStatusProcessing {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return outbound.SubmitResult{Outcome: outbound.SubmitGroupNotAdmitted}, nil
	}

	batchID := uuid.New().String()
	var admittedAt time.Time
	err = tx.QueryRowContext(ctx, `
		INSERT INTO outbound_batches
			(id, batch_key, key_kind, delivery_family, grammar_version, alert_group_id,
			 fingerprint, fingerprint_version, admission_outcome, intent_count)
		VALUES ($1, $2, $3, 'notification', $4, $5, $6, $7, $8, $9)
		ON CONFLICT (batch_key) DO NOTHING
		RETURNING id, admitted_at`,
		batchID, admission.BatchKey, string(admission.Kind), admission.GrammarVersion,
		admission.AlertGroupID, admission.Fingerprint, admission.FingerprintVersion,
		string(admission.Outcome), len(admission.Commitments),
	).Scan(&batchID, &admittedAt)

	if errors.Is(err, sql.ErrNoRows) {
		// Somebody else holds this claim. Nothing about the group is written on
		// this path - the winner already said what the group is escalating by.
		result, lostErr := lostAdmission(ctx, tx, admission)
		if lostErr != nil {
			return outbound.SubmitResult{}, lostErr
		}
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return result, nil
	}
	if err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("claim the admission: %w", err)
	}

	intentIDs, err := insertCommitmentsTx(ctx, tx, batchID, admission, admittedAt, adm.Actor)
	if err != nil {
		return outbound.SubmitResult{}, err
	}

	// The state the commitments are about. Written even when there is nobody to
	// notify: a group that was admitted has a snapshot, or a later re-admission
	// would start from nothing.
	snapshot, err := json.Marshal(admission.Snapshot)
	if err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("store the snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_group_snapshots
			(alert_group_id, revision, snapshot_schema_version, snapshot, snapshot_digest)
		VALUES ($1, $2, $3, $4, $5)`,
		admission.AlertGroupID, admission.Revision, admission.SnapshotSchemaVersion,
		snapshot, admission.Snapshot.Digest()); err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("store the snapshot: %w", err)
	}

	// Only now the group itself.
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_groups
		SET status = CASE WHEN status = $1 THEN $2 ELSE status END,
		    policy_id = $3, policy_snapshot = $4, updated_at = now()
		WHERE id = $5`,
		model.AlertGroupStatusNew, model.AlertGroupStatusProcessing,
		adm.PolicyID, nullableJSON(adm.PolicySnapshot), admission.AlertGroupID,
	); err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("record the escalation on the group: %w", err)
	}

	if err := admissionTimelineTx(ctx, tx, adm, admission); err != nil {
		return outbound.SubmitResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return outbound.SubmitResult{}, err
	}
	return outbound.SubmitResult{
		Outcome:   outbound.SubmitCreated,
		BatchID:   batchID,
		IntentIDs: intentIDs,
	}, nil
}

// lostAdmission answers for the producer that did not get the claim.
//
// The question is not "did somebody get there first" - that is already known -
// but "did they accept the same work". The same content is an idempotent
// repeat; different content is a conflict, and the first set stands. Merging
// them would page an audience nobody chose.
func lostAdmission(ctx context.Context, tx *sql.Tx,
	admission keys.Admission) (outbound.SubmitResult, error) {

	var existingID string
	var existingFingerprint []byte
	var existingVersion int
	err := tx.QueryRowContext(ctx, `
		SELECT id, fingerprint, fingerprint_version
		FROM outbound_batches WHERE batch_key = $1`, admission.BatchKey).
		Scan(&existingID, &existingFingerprint, &existingVersion)
	if err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("read the winning admission: %w", err)
	}

	if existingVersion != admission.FingerprintVersion {
		// The stored row was written by another protocol version. Comparing
		// digests across protocols would answer a question neither of them
		// asked, so the comparison is refused rather than guessed at.
		return outbound.SubmitResult{}, outboundContractf(
			"admission %s was fingerprinted under version %d, this build compares under %d",
			admission.BatchKey, existingVersion, admission.FingerprintVersion)
	}

	ids, err := intentIDsOfBatch(ctx, tx, existingID)
	if err != nil {
		return outbound.SubmitResult{}, err
	}

	outcome := outbound.SubmitConflict
	if bytes.Equal(existingFingerprint, admission.Fingerprint) {
		outcome = outbound.SubmitExisting
	}
	return outbound.SubmitResult{Outcome: outcome, BatchID: existingID, IntentIDs: ids}, nil
}

// insertCommitmentsTx writes the commitments in key order.
//
// The order is not cosmetic: two producers racing on one claim insert the same
// rows in the same sequence, so a violation of the key grammar surfaces as one
// deterministic unique-violation instead of as a deadlock nobody can read.
func insertCommitmentsTx(ctx context.Context, tx *sql.Tx, batchID string,
	admission keys.Admission, admittedAt time.Time, actor string) ([]string, error) {

	ids := make([]string, 0, len(admission.Commitments))
	for _, c := range admission.Commitments {
		payload, err := json.Marshal(c.Payload)
		if err != nil {
			return nil, fmt.Errorf("encode the payload of %s: %w", c.IdempotencyKey, err)
		}

		offset, err := timingOffset(c.Timing)
		if err != nil {
			return nil, err
		}
		expiresAt, err := expiryOf(c.Expiry, admittedAt)
		if err != nil {
			return nil, err
		}

		form := outbound.FormOneShot
		if c.Editable {
			form = outbound.FormEditable
		}

		id := uuid.New().String()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbound_intents (
				id, batch_id, idempotency_key, delivery_family, key_kind, grammar_version,
				provider, target_kind, target_ref, alert_group_id, form, completion_mode,
				ambiguity_policy, payload_schema_version, payload, provider_key_codec_version,
				status, desired_revision, not_before, next_attempt_at, expires_at)
			VALUES (
				$1, $2, $3, 'notification', $4, $5,
				$6, $7, $8, $9, $10, $11,
				$12, $13, $14, $15,
				'pending', $16,
				$17::timestamptz + make_interval(secs => $18),
				GREATEST(now(), $17::timestamptz + make_interval(secs => $18)),
				$19)`,
			id, batchID, c.IdempotencyKey,
			string(admission.Kind), admission.GrammarVersion,
			c.Provider, string(c.Target.Kind), c.Target.Ref, admission.AlertGroupID,
			string(form), string(c.CompletionMode),
			string(c.AmbiguityPolicy), c.PayloadSchemaVersion, payload, keys.ProviderKeyCodecV1,
			admission.Revision,
			admittedAt, offset.Seconds(),
			expiresAt,
		); err != nil {
			return nil, fmt.Errorf("write the commitment %s: %w", c.IdempotencyKey, err)
		}

		if err := appendIntentEventTx(ctx, tx, id, 1, "created", "", actor); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// timingOffset turns a timing spec into the delay from admission.
//
// Only the relative form has a meaning here: an escalation step is "so long
// after the alert was admitted", and the absolute form belongs to families
// whose time comes from the domain rather than from the claim.
func timingOffset(spec keys.TimingSpec) (time.Duration, error) {
	if err := spec.Validate(); err != nil {
		return 0, outboundContractf("timing: %v", err)
	}
	if spec.Kind != keys.TimingRelativeToAdmission {
		return 0, outboundContractf("an escalation step timed as %q", spec.Kind)
	}
	return spec.Offset, nil
}

func expiryOf(spec *keys.TimingSpec, admittedAt time.Time) (*time.Time, error) {
	if spec == nil {
		return nil, nil
	}
	if err := spec.Validate(); err != nil {
		return nil, outboundContractf("expiry: %v", err)
	}
	switch spec.Kind {
	case keys.TimingAbsolute:
		at := spec.At.UTC()
		return &at, nil
	case keys.TimingRelativeToAdmission:
		at := admittedAt.Add(spec.Offset)
		return &at, nil
	default:
		return nil, outboundContractf("unknown expiry kind %q", spec.Kind)
	}
}

// admissionTimelineTx records what the admission decided in the group's own
// history - including, and especially, that it found nobody to notify.
func admissionTimelineTx(ctx context.Context, tx *sql.Tx,
	adm outbound.EscalationAdmission, admission keys.Admission) error {

	if admission.Outcome == keys.OutcomeNoTargets {
		if err := addTimelineTx(ctx, tx, admission.AlertGroupID,
			model.TimelineEventNotificationFailed,
			"Escalation admitted with nobody to notify", adm.Actor); err != nil {
			return err
		}
	}
	for _, step := range adm.StepsWithoutRecipients {
		if err := addTimelineTx(ctx, tx, admission.AlertGroupID,
			model.TimelineEventNotificationFailed,
			fmt.Sprintf("Escalation step %s resolved to nobody on call", step),
			adm.Actor); err != nil {
			return err
		}
	}
	return nil
}

func addTimelineTx(ctx context.Context, tx *sql.Tx, alertGroupID string,
	eventType model.TimelineEventType, message, actor string) error {

	if actor == "" {
		actor = "system"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, '{}', now())`,
		uuid.New().String(), alertGroupID, eventType, message, actor)
	if err != nil {
		return fmt.Errorf("record %s in the timeline: %w", eventType, err)
	}
	return nil
}

// appendIntentEventTx writes one line of a commitment's own history: the things
// that happen to it without a network call - it was created, withdrawn, expired
// before anything was tried, decided on by a person.
func appendIntentEventTx(ctx context.Context, tx *sql.Tx,
	intentID string, seq int, kind, reason, actor string) error {

	// seq 0 means "whatever comes next": the caller of a lifecycle event knows
	// what happened, not how many things happened before it.
	sequence := any(seq)
	if seq <= 0 {
		sequence = nil
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_intent_events (id, intent_id, seq, kind, reason, actor)
		VALUES ($1, $2,
		        COALESCE($3::int,
		                 (SELECT COALESCE(max(seq), 0) + 1
		                  FROM outbound_intent_events WHERE intent_id = $2)),
		        $4, $5, $6)`,
		uuid.New().String(), intentID, sequence, kind, nilIfEmpty(reason), nilIfEmpty(actor))
	if err != nil {
		return fmt.Errorf("record %s for commitment %s: %w", kind, intentID, err)
	}
	return nil
}

func intentIDsOfBatch(ctx context.Context, tx *sql.Tx, batchID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM outbound_intents WHERE batch_id = $1 ORDER BY idempotency_key`, batchID)
	if err != nil {
		return nil, fmt.Errorf("read the commitments of %s: %w", batchID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// outboundIntentColumns is read by every door into a commitment - the plain
// read, the locking one and the journal - so the domain cannot end up seeing a
// different commitment depending on which one it came through.
//
// The last column is the only computed one: whether the deadline has passed as
// of this statement's clock. It is asked of the database rather than compared
// in Go, because the process clock is not the one the leases are written
// against.
const outboundIntentColumns = `
	SELECT id, COALESCE(alert_group_id, ''), provider, form, completion_mode,
	       ambiguity_policy, status, generation_no, attempts_in_generation,
	       failure_streak, desired_revision, applied_revision,
	       final_revision_applied, receipt, cancellation_requested,
	       accepted_duplicate_risk, not_before, next_attempt_at, expires_at,
	       create_key IS NOT NULL, payload_schema_version,
	       provider_key_codec_version,
	       COALESCE(expires_at <= now(), FALSE)`

// scanIntent turns one row of outboundIntentColumns into a commitment, and is
// the only place that mapping exists: two readers that disagreed about it would
// hand the domain two different commitments from the same row.
func scanIntent(row interface{ Scan(...any) error }) (*outbound.Intent, bool, error) {
	var (
		intent         outbound.Intent
		groupID        sql.NullString
		applied        sql.NullInt64
		expiresAt      sql.NullTime
		receipt        []byte
		deadlinePassed bool
	)
	if err := row.Scan(
		&intent.ID, &groupID, &intent.Provider, &intent.Form, &intent.CompletionMode,
		&intent.AmbiguityPolicy, &intent.Status, &intent.GenerationNo,
		&intent.AttemptsInGeneration, &intent.FailureStreak, &intent.DesiredRevision,
		&applied, &intent.FinalRevisionApplied, &receipt, &intent.CancellationRequested,
		&intent.AcceptedDuplicateRisk, &intent.NotBefore, &intent.NextAttemptAt, &expiresAt,
		&intent.GenerationBound, &intent.PayloadSchemaVersion,
		&intent.ProviderKeyCodecVersion, &deadlinePassed); err != nil {
		return nil, false, err
	}

	intent.AlertGroupID = groupID.String
	if applied.Valid {
		value := applied.Int64
		intent.AppliedRevision = &value
	}
	if expiresAt.Valid {
		at := expiresAt.Time
		intent.ExpiresAt = &at
	}
	intent.HasReceipt = len(receipt) > 0
	return &intent, deadlinePassed, nil
}

// GetIntent reads one commitment as the domain sees it.
func (s *Store) GetIntent(ctx context.Context, id string) (*outbound.Intent, error) {
	intent, _, err := scanIntent(s.db.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return intent, nil
}

// ListIntentsByAlertGroup reads a group's commitments in key order - what the
// history of one alert's delivery looks like from the outside.
func (s *Store) ListIntentsByAlertGroup(ctx context.Context, alertGroupID string) ([]outbound.Intent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM outbound_intents WHERE alert_group_id = $1 ORDER BY idempotency_key`,
		alertGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]outbound.Intent, 0, len(ids))
	for _, id := range ids {
		intent, err := s.GetIntent(ctx, id)
		if err != nil {
			return nil, err
		}
		if intent != nil {
			out = append(out, *intent)
		}
	}
	return out, nil
}

// nextEventSeq asks appendIntentEventTx to take the next number in this
// commitment's own history.
const nextEventSeq = 0

// transitionWrite is one decided transition plus the values the effects need.
type transitionWrite struct {
	Intent     outbound.Intent
	Transition outbound.Transition
	Backoff    time.Duration
	// AppliedRevision is the revision the settled attempt was applying, which
	// is not the same as the one desired now: the desired state may have moved
	// while the attempt was in flight, and recording that as applied would
	// claim the card shows something it does not.
	AppliedRevision int64
	// AttemptIsFinal says the settled attempt applied the last revision this
	// commitment will ever have, which is what makes an editable card done
	// rather than merely up to date.
	AttemptIsFinal bool
	Receipt        json.RawMessage
	NewExpires     *time.Time
	Actor          string
	Reason         string
}

// applyTransitionTx writes everything a transition means, in one call.
//
// One door on purpose. The effects of a transition are not only the
// commitment's own row: a success moves the alert group out of processing, and
// every ending writes a line in the alert's history. When those lived at the
// call sites, two of the three call sites forgot them - and the one that forgot
// the group left an alert saying nobody had been paged while the delivery said
// otherwise. A caller that can apply half a transition will eventually apply
// half a transition.
func applyTransitionTx(ctx context.Context, tx *sql.Tx, w transitionWrite) error {
	e := w.Transition.Effects

	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_intents SET
			status = $2,
			lease_token  = CASE WHEN $3 THEN NULL ELSE lease_token END,
			locked_until = CASE WHEN $3 THEN NULL ELSE locked_until END,
			worker_id    = CASE WHEN $3 THEN NULL ELSE worker_id END,
			current_attempt_id = CASE WHEN $4 THEN NULL ELSE current_attempt_id END,
			cancellation_requested = CASE WHEN $5 THEN FALSE ELSE cancellation_requested END,
			generation_no = generation_no + CASE WHEN $6 THEN 1 ELSE 0 END,
			attempts_in_generation = CASE WHEN $6 THEN 0 ELSE attempts_in_generation END,
			bound_endpoint = CASE WHEN $6 THEN NULL ELSE bound_endpoint END,
			create_key     = CASE WHEN $6 THEN NULL ELSE create_key END,
			receipt = CASE
				WHEN $6 THEN NULL
				WHEN $7 AND $8::jsonb IS NOT NULL THEN $8::jsonb
				ELSE receipt END,
			failure_streak = CASE
				WHEN $9  THEN 0
				WHEN $10 THEN failure_streak + 1
				ELSE failure_streak END,
			next_attempt_at = CASE
				WHEN $11 THEN now() + make_interval(secs => $12)
				WHEN $13 THEN now()
				ELSE next_attempt_at END,
			applied_revision = CASE WHEN $14 THEN $15 ELSE applied_revision END,
			final_revision_applied = CASE
				WHEN $14 AND $16 THEN TRUE ELSE final_revision_applied END,
			accepted_duplicate_risk = CASE WHEN $17 THEN TRUE ELSE accepted_duplicate_risk END,
			expires_at = COALESCE($18, expires_at),
			updated_at = now()
		WHERE id = $1`,
		w.Intent.ID, string(w.Transition.To),
		e.ClearLease, e.ClearCurrentAttempt, e.ConsumeCancellation,
		e.NewGeneration,
		e.StoreReceipt, nullableJSON(w.Receipt),
		e.ResetFailureStreak, e.BumpFailureStreak,
		e.ScheduleRetry, w.Backoff.Seconds(), e.ScheduleNow,
		e.ApplyRevision, w.AppliedRevision, w.AttemptIsFinal, e.RecordDuplicateRisk,
		w.NewExpires,
	); err != nil {
		return fmt.Errorf("apply %s to commitment %s: %w", w.Transition.Row, w.Intent.ID, err)
	}

	if err := appendTransitionEventTx(ctx, tx, w); err != nil {
		return err
	}
	if !w.Intent.GroupBound() {
		return nil
	}
	return groupEffectsTx(ctx, tx, w.Intent.AlertGroupID, w.Transition)
}

// appendTransitionEventTx records the lifecycle events a transition produces -
// the ones that are not attempts and therefore have no place in the attempt
// journal.
func appendTransitionEventTx(ctx context.Context, tx *sql.Tx, w transitionWrite) error {
	e := w.Transition.Effects

	if e.NewGeneration {
		// The decision to start over, which is not the same fact as the
		// binding of the effect that follows it: this one abandons whatever
		// the previous generation may have created, and is recorded even if no
		// attempt is ever made afterwards.
		if err := appendIntentEventTx(ctx, tx, w.Intent.ID, nextEventSeq,
			"generation_started", w.Reason, w.Actor); err != nil {
			return err
		}
	}
	if e.RecordDuplicateRisk {
		if err := appendIntentEventTx(ctx, tx, w.Intent.ID, nextEventSeq,
			"duplicate_risk_accepted",
			"a message may already have been delivered", w.Actor); err != nil {
			return err
		}
	}
	if w.Transition.To == outbound.StatusCanceled {
		return appendIntentEventTx(ctx, tx, w.Intent.ID, nextEventSeq,
			"canceled", w.Reason, w.Actor)
	}
	return nil
}

// groupEffectsTx writes what a transition means for the alert group: its
// history, and the one status move a delivery is allowed to make.
func groupEffectsTx(ctx context.Context, tx *sql.Tx, alertGroupID string,
	transition outbound.Transition) error {

	if message, eventType, ok := timelineFor(transition); ok {
		if err := addTimelineTx(ctx, tx, alertGroupID, eventType, message, "system"); err != nil {
			return err
		}
	}

	if transition.Effects.TriggerGroup {
		// Conditional on purpose: an acknowledgement that arrived while this
		// send was in flight has already moved the group, and a delivery must
		// not move it back.
		if _, err := tx.ExecContext(ctx, `
			UPDATE alert_groups SET status = $2, updated_at = now()
			WHERE id = $1 AND status = $3`,
			alertGroupID, model.AlertGroupStatusTriggered, model.AlertGroupStatusProcessing,
		); err != nil {
			return fmt.Errorf("move the group to triggered: %w", err)
		}
	}
	return nil
}

// timelineFor turns a transition into the line the alert's history gets.
func timelineFor(t outbound.Transition) (string, model.TimelineEventType, bool) {
	return timelineLine(t.Effects.Timeline)
}

// timelineLine is the wording of every effect, in one place. Set-based paths
// that end many commitments at once cannot ask the machine per row, and this is
// what keeps them saying the same thing it would have said.
//
// The wording of a success is deliberate: "sent" and "assumed delivered" are
// different claims, and only one of them is about the world.
func timelineLine(kind outbound.TimelineKind) (string, model.TimelineEventType, bool) {
	switch kind {
	case outbound.TimelineSent:
		return "Notification sent", model.TimelineEventNotificationSent, true
	case outbound.TimelineDelivered:
		return "Notification delivered", model.TimelineEventNotificationSent, true
	case outbound.TimelineAssumedAccepted:
		return "Notification assumed delivered: the provider never confirmed, and the risk was accepted",
			model.TimelineEventNotificationSent, true
	case outbound.TimelineSentAlongsideAck:
		return "Notification went out at the same moment the alert was acknowledged",
			model.TimelineEventNotificationSent, true
	case outbound.TimelineFailed:
		return "Notification failed permanently", model.TimelineEventNotificationFailed, true
	case outbound.TimelineExpired:
		return "A notification expired before it could be sent",
			model.TimelineEventNotificationFailed, true
	case outbound.TimelineCanceled:
		return "A notification was withdrawn", model.TimelineEventNotificationFailed, true
	case outbound.TimelineLeaseLost:
		return "A notification was interrupted mid-flight; whether it arrived is unknown",
			model.TimelineEventNotificationFailed, true
	default:
		return "", "", false
	}
}

// lockAlertGroupTx takes the group's row, and always before any commitment of
// it. Every transaction that can write to a group takes them in this order,
// which is what keeps acknowledgement and delivery from deadlocking.
func lockAlertGroupTx(ctx context.Context, tx *sql.Tx, alertGroupID string) error {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM alert_groups WHERE id = $1 FOR UPDATE`, alertGroupID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock alert group %s: %w", alertGroupID, err)
	}
	return nil
}

// setLockTimeoutTx bounds how long a point mutation waits for a row somebody
// else holds. Waiting longer than the lease it is protecting would mean
// applying a decision that has already been handed to another worker; a
// timeout, by contrast, is a retry of a mutation that classifies itself.
func setLockTimeoutTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx,
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'",
			outbound.NotificationLockTimeout.Milliseconds()))
	return err
}

// lockIntentTx takes one commitment and reads it as the domain sees it,
// together with whether its own deadline has passed as of this transaction's
// clock.
func lockIntentTx(ctx context.Context, tx *sql.Tx, id string) (*outbound.Intent, bool, error) {
	intent, expired, err := scanIntent(tx.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock commitment %s: %w", id, err)
	}
	return intent, expired, nil
}

// nextAttemptNoTx is the next number in this commitment's journal, taken under
// the lock the caller already holds. It orders the journal: two records written
// in one transaction share a timestamp, and "the last attempt" has to mean
// something.
func nextAttemptNoTx(ctx context.Context, tx *sql.Tx, intentID string) (int, error) {
	var next int
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(max(attempt_no), 0) + 1 FROM outbound_attempts WHERE intent_id = $1`,
		intentID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("number the next journal record of %s: %w", intentID, err)
	}
	return next, nil
}

// storedSnapshot is the state a revision is rendered from, as it came back out
// of the database and after it has been proved to be the same thing that went
// in.
type storedSnapshot struct {
	Snapshot keys.RenderSnapshot
	Revision int64
	Final    bool
}

// lockedSnapshotTx reads the state an attempt will render from, and checks that
// it is still what was admitted.
//
// From the domain's own table, never from the live alert group: a retry has to
// send what was accepted, and two instances have to send the same thing.
//
// Four checks, and none of them is defensive programming. The schema version
// says whether this build can read the row at all. The digest says the content
// is the content the commitments were keyed against - a row edited by hand
// would otherwise be sent under the identity of the admission it replaced. The
// group and the revision say the row is about the right thing: a snapshot moved
// between groups, or one revision stored under another's number, would be a
// message about the wrong alert with a key that says it is about the right one.
//
// The first refusal is about this build and leaves the commitment alone; the
// other three are about the row and end it.
func lockedSnapshotTx(ctx context.Context, tx *sql.Tx, alertGroupID string) (storedSnapshot, error) {
	var (
		raw           []byte
		revision      int64
		schemaVersion int
		digest        []byte
		final         bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT snapshot, revision, snapshot_schema_version, snapshot_digest, final
		FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		alertGroupID).Scan(&raw, &revision, &schemaVersion, &digest, &final)
	if errors.Is(err, sql.ErrNoRows) {
		return storedSnapshot{}, undeliverablef(
			"alert group %s has commitments but no state for them to render from", alertGroupID)
	}
	if err != nil {
		return storedSnapshot{}, err
	}

	// A version this build does not know is a deployment that is behind, not a
	// broken alert: the instance that wrote it renders it perfectly well. It
	// stops here and changes nothing, so the work waits for a build that can
	// do it instead of being ended by one that cannot.
	if schemaVersion != keys.RenderSnapshotSchemaV1 {
		return storedSnapshot{}, outboundContractf(
			"the state of %s was written under schema version %d, which this build cannot render",
			alertGroupID, schemaVersion)
	}

	var snapshot keys.RenderSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		// A stored snapshot that no longer canonicalises is refused rather than
		// rendered: the message it would produce is not the one its key
		// describes.
		return storedSnapshot{}, undeliverablef(
			"the stored state of %s cannot be read back: %v", alertGroupID, err)
	}

	if !bytes.Equal(snapshot.Digest(), digest) {
		return storedSnapshot{}, undeliverablef(
			"the stored state of %s no longer matches the digest its commitments were keyed against",
			alertGroupID)
	}
	content := snapshot.Content()
	if content.AlertGroupID != alertGroupID || content.Revision != revision {
		return storedSnapshot{}, undeliverablef(
			"the state stored for %s at revision %d describes %s at revision %d",
			alertGroupID, revision, content.AlertGroupID, content.Revision)
	}

	return storedSnapshot{Snapshot: snapshot, Revision: revision, Final: final}, nil
}

// IntentJournal reads everything known about one commitment, in the order it
// happened.
//
// One repeatable-read transaction, and read-only. Four separate reads would be
// four different instants: with a delivery finishing between them the answer
// could hold a commitment that is still sending and the attempt that finished
// it, which is not a state the system was ever in - and this is the read people
// reach for precisely when they are trying to establish what really happened.
func (s *Store) IntentJournal(ctx context.Context, intentID string) (*outbound.Journal, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	intent, _, err := scanIntent(tx.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1`, intentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	journal := &outbound.Journal{Intent: *intent}

	if journal.Attempts, err = journalAttemptsTx(ctx, tx, intentID); err != nil {
		return nil, err
	}
	if journal.Observations, err = journalObservationsTx(ctx, tx, intentID); err != nil {
		return nil, err
	}
	if journal.Events, err = journalEventsTx(ctx, tx, intentID); err != nil {
		return nil, err
	}
	return journal, tx.Commit()
}

func journalAttemptsTx(ctx context.Context, tx *sql.Tx,
	intentID string) ([]outbound.AttemptRecord, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, attempt_no, record_kind, generation_no, attempt_kind, operation,
		       provider, COALESCE(bound_endpoint, ''), COALESCE(provider_key, ''),
		       applied_revision, started_at, finished_at, COALESCE(outcome, ''),
		       COALESCE(error_class, ''), COALESCE(provider_status, ''), receipt,
		       COALESCE(response_summary, ''), COALESCE(finish_reason, ''),
		       COALESCE(completion_fingerprint_version, 0)
		FROM outbound_attempts WHERE intent_id = $1 ORDER BY attempt_no`, intentID)
	if err != nil {
		return nil, fmt.Errorf("read the journal of %s: %w", intentID, err)
	}
	defer rows.Close()

	var attempts []outbound.AttemptRecord
	for rows.Next() {
		var (
			record   outbound.AttemptRecord
			revision sql.NullInt64
			started  sql.NullTime
			finished sql.NullTime
			receipt  []byte
		)
		if err := rows.Scan(&record.ID, &record.AttemptNo, &record.RecordKind,
			&record.GenerationNo, &record.AttemptKind, &record.Operation, &record.Provider,
			&record.BoundEndpoint, &record.ProviderKey, &revision, &started, &finished,
			&record.Outcome, &record.ErrorClass, &record.ProviderStatus, &receipt,
			&record.Summary, &record.FinishReason,
			&record.CompletionFingerprintVersion); err != nil {
			return nil, err
		}
		if len(receipt) > 0 {
			record.Receipt = json.RawMessage(receipt)
		}
		if revision.Valid {
			value := revision.Int64
			record.AppliedRevision = &value
		}
		if started.Valid {
			at := started.Time
			record.StartedAt = &at
		}
		if finished.Valid {
			at := finished.Time
			record.FinishedAt = &at
		}
		attempts = append(attempts, record)
	}
	return attempts, rows.Err()
}

func journalObservationsTx(ctx context.Context, tx *sql.Tx,
	intentID string) ([]outbound.Observation, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT o.attempt_id, o.observation_kind, o.observed_at, o.outcome,
		       COALESCE(o.error_class, ''), COALESCE(o.provider_status, ''),
		       COALESCE(o.provider_result_detail, ''), o.applied_revision, o.receipt,
		       COALESCE(o.response_summary, ''),
		       COALESCE(o.completion_fingerprint_version, 0)
		FROM outbound_attempt_observations o
		JOIN outbound_attempts a ON a.id = o.attempt_id
		WHERE a.intent_id = $1 ORDER BY a.attempt_no, o.observed_at`, intentID)
	if err != nil {
		return nil, fmt.Errorf("read the late results of %s: %w", intentID, err)
	}
	defer rows.Close()

	var observations []outbound.Observation
	for rows.Next() {
		var (
			o        outbound.Observation
			revision sql.NullInt64
			receipt  []byte
		)
		if err := rows.Scan(&o.AttemptID, &o.Kind, &o.ObservedAt, &o.Outcome,
			&o.ErrorClass, &o.ProviderStatus, &o.ProviderResultDetail, &revision,
			&receipt, &o.Summary, &o.CompletionFingerprintVersion); err != nil {
			return nil, err
		}
		if revision.Valid {
			value := revision.Int64
			o.AppliedRevision = &value
		}
		if len(receipt) > 0 {
			o.Receipt = json.RawMessage(receipt)
		}
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

func journalEventsTx(ctx context.Context, tx *sql.Tx,
	intentID string) ([]outbound.IntentEvent, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT seq, kind, COALESCE(reason, ''), COALESCE(actor, ''),
		       COALESCE(from_status, ''), COALESCE(to_status, '')
		FROM outbound_intent_events WHERE intent_id = $1 ORDER BY seq`, intentID)
	if err != nil {
		return nil, fmt.Errorf("read the events of %s: %w", intentID, err)
	}
	defer rows.Close()

	var events []outbound.IntentEvent
	for rows.Next() {
		var e outbound.IntentEvent
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Reason, &e.Actor,
			&e.FromStatus, &e.ToStatus); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
