package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// admitWebhookTx admits one webhook batch - a fan-out of an event, or a replay
// to one subscriber - inside the caller's transaction, and it is the ONLY door
// the webhook family has.
//
// It takes the provenance and derives the admission itself. Handed a ready
// Admission, it would have to trust that the keys, the payloads and the
// fingerprint in it were still the ones the grammar produced together, and
// nothing in the type system says so: the fields are exported, and a caller
// that changed the recipient in the payload and in the columns alike would pass
// every pairwise check with a key still naming the old one. Deriving here,
// there is no moment at which the parts exist apart.
//
// The caller owns the transaction, so the caller counts the admission - after
// its commit and not before. Counting here, inside a transaction that may yet
// roll back, would report work that never came to exist.
//
// Unlike an escalation's or a handover's door this one locks no recipients: a
// subscriber is an integration, and whether it still exists is the fan-out's
// business, decided under the share lock it takes on the audience.
func admitWebhookTx(ctx context.Context, tx *sql.Tx, batch keys.WebhookBatch,
	actor string) (outbound.SubmitResult, error) {

	admission, err := batch.Admit()
	if err != nil {
		return outbound.SubmitResult{}, outboundContractf("%v", err)
	}
	family, err := keys.FamilyOf(admission.Kind)
	if err != nil {
		return outbound.SubmitResult{}, outboundContractf("%v", err)
	}

	// The claim first, for the same reason every other door reads it first:
	// a repeat of an admission has to find the row the first one wrote.
	if held, found, err := existingAdmission(ctx, tx, admission); err != nil {
		return outbound.SubmitResult{}, err
	} else if found {
		return held, nil
	}

	batchID, admittedAt, won, err := claimBatchTx(ctx, tx, admission, string(family),
		"", nil, nil, 0, 0)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	if !won {
		return lostAdmission(ctx, tx, admission)
	}

	intentIDs, err := insertCommitmentsTx(ctx, tx, batchID, admission, string(family),
		admittedAt, actor)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	return outbound.SubmitResult{
		Outcome:   outbound.SubmitCreated,
		BatchID:   batchID,
		IntentIDs: intentIDs,
	}, nil
}

// FanOutNextEvent turns one pending event of the alert outbox into commitments
// to its subscribers, in one transaction, and reports Found=false when nothing
// is pending.
//
// The event is claimed with a row lock and nothing else - no lease, no
// attempt counter. Fan-out makes no network calls, so a process that dies at any
// point rolls the whole thing back and the event is pending for the next tick;
// and under the lock exactly one fan-out ever reaches the door for one event,
// which is why "existing" and "conflict" from the door are refused here as
// contract violations rather than handled: they mean a second path to the door
// exists, and that is fixed, not tolerated.
//
// The audience is read under FOR SHARE. Deleting or disabling a subscriber takes
// FOR UPDATE on the same row, so whichever gets there first, the other waits,
// and no live commitment to a subscriber that is gone can be left behind (D2,
// package 3).
//
// An event this build cannot read - an event type it does not know, an empty
// body - is refused with the event named and Refused set, and is left exactly
// as it was. It holds the head of the queue until a person fixes the row: the
// execution model builds no way around a damaged execution row, only a way to
// see one.
func (s *Store) FanOutNextEvent(ctx context.Context) (outbound.FanOutResult, error) {
	policy, err := outbound.PolicyOf(outbound.FamilyWebhook)
	if err != nil {
		return outbound.FanOutResult{}, err
	}
	grammar, err := keys.CurrentGrammarVersion(keys.KindWebhookEvent)
	if err != nil {
		return outbound.FanOutResult{}, outboundContractf("%v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.FanOutResult{}, err
	}
	defer tx.Rollback()
	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
		return outbound.FanOutResult{}, err
	}

	var eventID, teamID, eventType string
	var body []byte
	err = tx.QueryRowContext(ctx, `
		SELECT id, team_id, event_type, payload FROM event_outbox
		WHERE status IN ('pending', 'processing')
		ORDER BY created_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT 1`).Scan(&eventID, &teamID, &eventType, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.FanOutResult{}, tx.Commit()
	}
	if err != nil {
		return outbound.FanOutResult{}, fmt.Errorf("claim an event to fan out: %w", err)
	}
	found := outbound.FanOutResult{Found: true, EventID: eventID}

	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM integrations
		WHERE type = $1 AND enabled
		  AND (scope = $2 OR (scope = $3 AND team_id = $4))
		ORDER BY id
		FOR SHARE`, model.IntegrationTypeGenericWebhook,
		model.WebhookScopeGlobal, model.WebhookScopeTeam, teamID)
	if err != nil {
		return found, fmt.Errorf("resolve the audience of event %s: %w", eventID, err)
	}
	var audience []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return found, err
		}
		audience = append(audience, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return found, err
	}

	admitted, err := admitWebhookTx(ctx, tx, keys.WebhookBatch{
		Kind:               keys.KindWebhookEvent,
		EventID:            eventID,
		EventType:          keys.WebhookEventType(eventType),
		Body:               string(body),
		IntegrationIDs:     audience,
		Expiry:             policy.Expiry,
		GrammarVersion:     grammar,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
	}, "fan-out")
	if err != nil {
		if errors.Is(err, ErrOutboundContract) {
			found.Refused = true
		}
		return found, fmt.Errorf("event %s: %w", eventID, err)
	}
	if admitted.Outcome != outbound.SubmitCreated {
		// Under the row lock nobody else reaches the door for this event, so
		// its claim cannot already be held. That it is means a second path.
		found.Refused = true
		return found, outboundContractf(
			"event %s is already admitted as %q under a lock that allows one fan-out",
			eventID, admitted.Outcome)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE event_outbox SET status = $2 WHERE id = $1`,
		eventID, model.OutboxEventStatusFannedOut); err != nil {
		return found, fmt.Errorf("mark event %s fanned out: %w", eventID, err)
	}
	if err := tx.Commit(); err != nil {
		return found, err
	}
	found.Outcome = admitted.Outcome
	found.Commitments = len(admitted.IntentIDs)
	return found, nil
}
