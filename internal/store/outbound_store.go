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

// ErrOutboundContract is a statement the schema or the grammar does not have -
// a stored row written by a protocol version this build cannot read, or an
// invariant that should have been impossible. It is never retryable.
var ErrOutboundContract = errors.New("store: outbound contract violation")

func outboundContractf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrOutboundContract, fmt.Sprintf(format, args...))
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

	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_intent_events (id, intent_id, seq, kind, reason, actor)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New().String(), intentID, seq, kind, nilIfEmpty(reason), nilIfEmpty(actor))
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

// GetIntent reads one commitment as the domain sees it.
func (s *Store) GetIntent(ctx context.Context, id string) (*outbound.Intent, error) {
	var (
		intent    outbound.Intent
		groupID   sql.NullString
		applied   sql.NullInt64
		expiresAt sql.NullTime
		receipt   []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(alert_group_id, ''), provider, form, completion_mode,
		       ambiguity_policy, status, generation_no, attempts_in_generation,
		       failure_streak, desired_revision, applied_revision,
		       final_revision_applied, receipt, cancellation_requested,
		       accepted_duplicate_risk, not_before, next_attempt_at, expires_at
		FROM outbound_intents WHERE id = $1`, id).Scan(
		&intent.ID, &groupID, &intent.Provider, &intent.Form, &intent.CompletionMode,
		&intent.AmbiguityPolicy, &intent.Status, &intent.GenerationNo,
		&intent.AttemptsInGeneration, &intent.FailureStreak, &intent.DesiredRevision,
		&applied, &intent.FinalRevisionApplied, &receipt, &intent.CancellationRequested,
		&intent.AcceptedDuplicateRisk, &intent.NotBefore, &intent.NextAttemptAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
	return &intent, nil
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
