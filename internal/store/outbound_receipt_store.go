package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The store's half of a channel whose acceptance only means "queued".
//
// Three ways in, one way to an answer. An acceptance records the object the
// attempt made; a provider's event - a callback, or the answer to a poll -
// folds into that object; and either of them, or the wait running out, asks
// the same question of the same facts: what do the objects of this commitment
// add up to (outbound.GenerationVerdict), and what does that do to it
// (outbound.Decide, TriggerVerdict). reviewReceiptTx is that question, and it
// is asked from every door, so a callback and a poll can never settle the same
// call two different ways.

// recordAcceptedEffectTx writes the object an accepted attempt made, as far as
// the acceptance knows it: it exists and has not finished. An event may have
// overtaken the answer to the request and written it already, with more to
// say, and then this changes nothing.
func recordAcceptedEffectTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent,
	attemptID, externalRef string) error {

	if externalRef == "" {
		return outboundContractf("attempt %s was accepted into a provider's queue and named no object", attemptID)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_effects (attempt_id, intent_id, generation_no, external_ref, state, provider_status)
		VALUES ($1, $2, $3, $4, $5, 'accepted')
		ON CONFLICT (attempt_id) DO NOTHING`,
		attemptID, intent.ID, intent.GenerationNo, externalRef, string(outbound.EffectInProgress))
	if err != nil {
		return fmt.Errorf("record the object attempt %s made: %w", attemptID, err)
	}
	return nil
}

// RecordProviderEvent keeps what a provider said, before anything tries to
// understand it. A repeat of an event already kept is a no-op that answers with
// the stored row, so the provider can be told yes either way.
func (s *Store) RecordProviderEvent(ctx context.Context, event outbound.ProviderEvent) (string, error) {
	if event.Provider == "" || event.EventID == "" || event.ProviderStatus == "" {
		return "", outboundContractf("a provider event with no provider, id or status")
	}
	var occurred any
	if !event.OccurredAt.IsZero() {
		occurred = event.OccurredAt
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
		WITH kept AS (
			INSERT INTO outbound_provider_events (id, provider, account_scope, event_id, attempt_id,
				external_ref, sequence, provider_status, summary, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (provider, account_scope, event_id) DO NOTHING
			RETURNING id
		)
		SELECT id FROM kept
		UNION ALL
		SELECT id FROM outbound_provider_events
		WHERE provider = $2 AND account_scope = $3 AND event_id = $4
		LIMIT 1`,
		uuid.New().String(), event.Provider, event.AccountScope, event.EventID,
		nilIfEmpty(event.AttemptID), nilIfEmpty(event.ExternalRef), event.Sequence,
		event.ProviderStatus, nilIfEmpty(event.Summary), occurred).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("keep event %s of %s: %w", event.EventID, event.Provider, err)
	}
	return id, nil
}

// ApplyProviderEvent folds one kept event into the object it is about, and
// asks what the commitment's objects now add up to.
//
// The event names the attempt that made its object, and the attempt is written
// before the network is touched, so an event that arrives before the answer to
// the request still finds both the attempt and the commitment. An event whose
// attempt is not here stays in the inbox, unmatched.
//
// Lock order: the alert group, then the commitment, then its object - the
// order every door that can write to a group takes them in.
func (s *Store) ApplyProviderEvent(ctx context.Context, eventRowID string,
	translators map[string]outbound.EffectTranslator) (outbound.ApplyResult, error) {

	event, state, err := s.readKeptEvent(ctx, eventRowID)
	if err != nil {
		return outbound.ApplyResult{}, err
	}
	if state != string(outbound.ApplyUnmatched) {
		return outbound.ApplyResult{Outcome: outbound.ApplyAlreadyDone}, nil
	}
	if event.AttemptID == "" || event.ExternalRef == "" {
		return outbound.ApplyResult{Outcome: outbound.ApplyUnmatched}, nil
	}

	var intentID, groupID string
	err = s.db.QueryRowContext(ctx, `
		SELECT a.intent_id, COALESCE(i.alert_group_id, '')
		FROM outbound_attempts a JOIN outbound_intents i ON i.id = a.intent_id
		WHERE a.id = $1`, event.AttemptID).Scan(&intentID, &groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.ApplyResult{Outcome: outbound.ApplyUnmatched}, nil
	}
	if err != nil {
		return outbound.ApplyResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.ApplyResult{}, err
	}
	defer tx.Rollback()
	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
		return outbound.ApplyResult{}, err
	}
	if groupID != "" {
		if err := lockAlertGroupTx(ctx, tx, groupID); err != nil {
			return outbound.ApplyResult{}, err
		}
	}
	intent, _, err := lockIntentTx(ctx, tx, intentID)
	if err != nil {
		return outbound.ApplyResult{}, err
	}
	if intent == nil {
		return outbound.ApplyResult{Outcome: outbound.ApplyUnmatched}, nil
	}

	result := outbound.ApplyResult{Outcome: outbound.ApplyApplied, IntentID: intentID}
	translator, served := translators[intent.Provider]
	if !served {
		return outbound.ApplyResult{}, outboundContractf(
			"an event about commitment %s of %s, and nothing here reads that provider's events",
			intentID, intent.Provider)
	}
	next, class, known := translator.EffectStateOf(event)
	if !known || !next.Known() {
		// The object stays where it was. A guess here would decide whether
		// somebody was called.
		metrics.OutboundContractViolationsTotal.WithLabelValues("effect_state_of", "unknown_status").Inc()
		result.Outcome = outbound.ApplyIgnored
	} else if err := foldEventTx(ctx, tx, *intent, event, next, class); err != nil {
		return outbound.ApplyResult{}, err
	}

	if intent.Status.Terminal() {
		// The object is kept up to date; a commitment that ended does not
		// move. History, not news.
		result.Outcome = outbound.ApplyIgnored
	} else if result.Outcome == outbound.ApplyApplied {
		moved, err := reviewReceiptTx(ctx, tx, *intent, false)
		if err != nil {
			return outbound.ApplyResult{}, err
		}
		result.To, result.Row = moved.To, moved.Row
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE outbound_provider_events SET state = $2 WHERE id = $1`,
		eventRowID, string(result.Outcome)); err != nil {
		return outbound.ApplyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return outbound.ApplyResult{}, err
	}
	if result.To != "" {
		countTerminal(intent.Family, result.To)
	}
	metrics.OutboundProviderEventsTotal.WithLabelValues(intent.Provider, string(result.Outcome)).Inc()
	return result, nil
}

func (s *Store) readKeptEvent(ctx context.Context, id string) (outbound.ProviderEvent, string, error) {
	var (
		event                         outbound.ProviderEvent
		attempt, ref, summary, status sql.NullString
		sequence                      sql.NullInt64
		occurred                      sql.NullTime
		state                         string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT provider, account_scope, event_id, attempt_id, external_ref, sequence,
		       provider_status, summary, occurred_at, state
		FROM outbound_provider_events WHERE id = $1`, id).Scan(
		&event.Provider, &event.AccountScope, &event.EventID, &attempt, &ref, &sequence,
		&status, &summary, &occurred, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return event, "", outboundContractf("no kept event %s", id)
	}
	if err != nil {
		return event, "", err
	}
	event.AttemptID, event.ExternalRef = attempt.String, ref.String
	event.ProviderStatus, event.Summary = status.String, summary.String
	if sequence.Valid {
		value := int(sequence.Int64)
		event.Sequence = &value
	}
	if occurred.Valid {
		event.OccurredAt = occurred.Time
	}
	return event, state, nil
}

// foldEventTx brings one object up to what the event says, by the domain's
// fold: a terminal state absorbs, and between the others the provider's
// numbering decides.
func foldEventTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent,
	event outbound.ProviderEvent, next outbound.EffectState, class string) error {

	var (
		generation int
		current    outbound.Effect
		sequence   sql.NullInt64
		exists     = true
	)
	err := tx.QueryRowContext(ctx, `
		SELECT generation_no, external_ref, state, last_sequence
		FROM outbound_effects WHERE attempt_id = $1 FOR UPDATE`, event.AttemptID).
		Scan(&generation, &current.ExternalRef, &current.State, &sequence)
	if errors.Is(err, sql.ErrNoRows) {
		exists = false
		if err := tx.QueryRowContext(ctx,
			`SELECT generation_no FROM outbound_attempts WHERE id = $1`, event.AttemptID).
			Scan(&generation); err != nil {
			return fmt.Errorf("read the generation of attempt %s: %w", event.AttemptID, err)
		}
		current = outbound.Effect{ExternalRef: event.ExternalRef, State: outbound.EffectInProgress}
	} else if err != nil {
		return err
	}
	if sequence.Valid {
		value := int(sequence.Int64)
		current.LastSequence = &value
	}
	if exists && current.ExternalRef != event.ExternalRef {
		return outboundContractf("attempt %s made %s, and an event about it names %s",
			event.AttemptID, current.ExternalRef, event.ExternalRef)
	}

	folded, changed := outbound.FoldEffect(current, next, event.Sequence)
	if exists && !changed {
		return nil
	}
	status := class
	if status == "" {
		status = event.ProviderStatus
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbound_effects (attempt_id, intent_id, generation_no, external_ref, state,
			last_sequence, provider_status, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (attempt_id) DO UPDATE SET
			state = EXCLUDED.state, last_sequence = EXCLUDED.last_sequence,
			provider_status = EXCLUDED.provider_status, updated_at = now()`,
		event.AttemptID, intent.ID, generation, folded.ExternalRef, string(folded.State),
		folded.LastSequence, status)
	if err != nil {
		return fmt.Errorf("fold an event into the object of attempt %s: %w", event.AttemptID, err)
	}
	return nil
}

// reviewReceiptTx asks what a commitment's objects add up to, under the locks
// the caller holds, and applies what the machine says.
//
// polled says the caller is the poll: only then does "still going" move the
// commitment, by scheduling the next poll. Any other door leaves a commitment
// that is still going exactly where it was - an event that changes nothing
// must not push the next look further away.
func reviewReceiptTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent,
	polled bool) (outbound.Transition, error) {

	if intent.CompletionMode != outbound.CompletionOnProviderReceipt {
		return outbound.Transition{}, nil
	}
	switch intent.Status {
	case outbound.StatusAwaitingReceipt, outbound.StatusPending:
	default:
		return outbound.Transition{}, nil
	}

	facts, err := generationFactsTx(ctx, tx, intent)
	if err != nil {
		return outbound.Transition{}, err
	}
	verdict, err := outbound.GenerationVerdict(facts)
	if err != nil {
		return outbound.Transition{}, err
	}

	if intent.Status == outbound.StatusPending {
		// A call due to be repeated after a doubtful attempt. A call that did
		// happen ends it; one known to be ringing is waited for; one known not
		// to have happened, with no doubt left, is repeated as a new
		// generation. Doubt alone leaves the repeat as it was.
		switch verdict {
		case outbound.VerdictHappened, outbound.VerdictNotPlaced:
		case outbound.VerdictStillGoing:
			if !hasKnownLiveEffect(facts) {
				return outbound.Transition{}, nil
			}
		default:
			return outbound.Transition{}, nil
		}
	}
	if intent.Status == outbound.StatusAwaitingReceipt && verdict == outbound.VerdictStillGoing && !polled {
		return outbound.Transition{}, nil
	}

	transition, err := outbound.Decide(outbound.Input{
		Intent:  intent,
		Trigger: outbound.TriggerVerdict,
		Verdict: verdict,
	})
	if err != nil {
		return outbound.Transition{}, err
	}

	// A commitment that starts waiting, or ends well, from pending keeps the
	// object it is waiting on, or that took place, as its receipt.
	var receipt json.RawMessage
	var ref string
	if transition.Effects.StoreReceipt && !intent.HasReceipt {
		if effect, ok := receiptEffect(facts, verdict); ok {
			ref = effect.ExternalRef
			receipt, _ = json.Marshal(map[string]string{"external_ref": ref})
		}
	}

	if err := applyTransitionTx(ctx, tx, transitionWrite{
		Intent:     intent,
		Transition: transition,
		Receipt:    receipt,
		ReceiptRef: ref,
		Actor:      outbound.ActorWorker,
		Reason:     "the provider's word: " + string(verdict),
	}); err != nil {
		return outbound.Transition{}, err
	}
	return transition, nil
}

// dischargedByEarlierCallTx says whether a call of this commitment is already
// known to have taken place - an event about it came in while a request was
// open - and if so, the transition that ends the commitment as settled. Asked
// before a request's own outcome is allowed to end the commitment: a refusal
// of a repeat must not fail a commitment whose first call reached somebody,
// nor stop the escalation behind it.
func dischargedByEarlierCallTx(ctx context.Context, tx *sql.Tx,
	intent outbound.Intent) (outbound.Transition, bool, error) {

	if intent.CompletionMode != outbound.CompletionOnProviderReceipt || intent.Form != outbound.FormOneShot {
		return outbound.Transition{}, false, nil
	}
	facts, err := generationFactsTx(ctx, tx, intent)
	if err != nil {
		return outbound.Transition{}, false, err
	}
	verdict, err := outbound.GenerationVerdict(facts)
	if err != nil || verdict != outbound.VerdictHappened {
		return outbound.Transition{}, false, err
	}
	transition, err := outbound.Decide(outbound.Input{
		Intent: intent, Trigger: outbound.TriggerVerdict, Verdict: outbound.VerdictHappened,
	})
	if err != nil {
		return outbound.Transition{}, false, err
	}
	return transition, true, nil
}

// generationFactsTx reads what the verdict depends on.
func generationFactsTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent) (outbound.GenerationFacts, error) {
	facts := outbound.GenerationFacts{
		Generation:  intent.GenerationNo,
		AttemptOpen: intent.Status == outbound.StatusSending,
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT attempt_id, generation_no, external_ref, state, last_sequence
		FROM outbound_effects WHERE intent_id = $1 ORDER BY attempt_id`, intent.ID)
	if err != nil {
		return facts, err
	}
	for rows.Next() {
		var e outbound.Effect
		var sequence sql.NullInt64
		if err := rows.Scan(&e.AttemptID, &e.Generation, &e.ExternalRef, &e.State, &sequence); err != nil {
			rows.Close()
			return facts, err
		}
		if sequence.Valid {
			value := int(sequence.Int64)
			e.LastSequence = &value
		}
		facts.Effects = append(facts.Effects, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return facts, err
	}

	// An attempt of this generation that ended in doubt, or was cut off with
	// its lease, may have made an object nobody has named yet.
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM outbound_attempts a
		WHERE a.intent_id = $1 AND a.generation_no = $2 AND a.record_kind = 'attempt'
		  AND a.finished_at IS NOT NULL
		  AND (a.outcome = 'ambiguous' OR a.finish_reason = 'lease_lost')
		  AND NOT EXISTS (SELECT 1 FROM outbound_effects e WHERE e.attempt_id = a.id)`,
		intent.ID, intent.GenerationNo).Scan(&facts.Unaccounted); err != nil {
		return facts, fmt.Errorf("count the attempts of %s nobody has named an object for: %w", intent.ID, err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(receipt_deadline <= now(), FALSE) FROM outbound_intents WHERE id = $1`,
		intent.ID).Scan(&facts.DeadlinePassed); err != nil {
		return facts, err
	}
	return facts, nil
}

func hasKnownLiveEffect(f outbound.GenerationFacts) bool {
	for _, e := range f.Effects {
		if e.Generation == f.Generation && e.State == outbound.EffectInProgress {
			return true
		}
	}
	return false
}

// receiptEffect is the object a commitment keeps as its receipt: the one that
// took place, or the live one it waits on.
func receiptEffect(f outbound.GenerationFacts, verdict outbound.Verdict) (outbound.Effect, bool) {
	want := outbound.EffectInProgress
	if verdict == outbound.VerdictHappened {
		want = outbound.EffectHappened
	}
	for _, e := range f.Effects {
		if e.State == want && (want == outbound.EffectHappened || e.Generation == f.Generation) {
			return e, true
		}
	}
	return outbound.Effect{}, false
}

// ClaimDueReceipts takes the waits that came round, in one family.
//
// No lease. Moving the next look forward IS the claim: no other instance takes
// the row until that moment, and an instance that dies mid-poll leaves it to be
// taken again then. Asking a provider twice changes nothing at the provider.
// The moment is read with clock_timestamp(), after the row lock: now() is when
// the transaction began, and one that waited behind a neighbour would schedule
// from before the wait.
func (s *Store) ClaimDueReceipts(ctx context.Context, family string, limit int) ([]outbound.AwaitingReceipt, error) {
	if limit <= 0 {
		return nil, nil
	}
	policy, err := outbound.PolicyOf(family)
	if err != nil {
		return nil, err
	}
	if policy.Receipt == nil {
		return nil, outboundContractf("family %s does not wait for the provider's word", family)
	}

	rows, err := s.db.QueryContext(ctx, `
		UPDATE outbound_intents i
		SET receipt_timeout_at = clock_timestamp() + make_interval(secs => $3)
		FROM (
			SELECT id FROM outbound_intents
			WHERE delivery_family = $1 AND status = 'awaiting_receipt' AND receipt_timeout_at <= now()
			ORDER BY receipt_timeout_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		) due
		WHERE i.id = due.id
		RETURNING i.id, i.provider`, family, limit, policy.Receipt.PollInterval.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim the waits of %s: %w", family, err)
	}
	var due []outbound.AwaitingReceipt
	for rows.Next() {
		var a outbound.AwaitingReceipt
		if err := rows.Scan(&a.IntentID, &a.Provider); err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}

	ids := make([]string, len(due))
	index := make(map[string]int, len(due))
	for i, a := range due {
		ids[i], index[a.IntentID] = a.IntentID, i
	}
	effects, err := s.db.QueryContext(ctx, `
		SELECT intent_id, attempt_id, external_ref FROM outbound_effects
		WHERE intent_id = ANY($1) AND state = 'in_progress'
		ORDER BY intent_id, attempt_id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer effects.Close()
	for effects.Next() {
		var ref outbound.EffectRef
		if err := effects.Scan(&ref.IntentID, &ref.AttemptID, &ref.ExternalRef); err != nil {
			return nil, err
		}
		i := index[ref.IntentID]
		due[i].Effects = append(due[i].Effects, ref)
	}
	return due, effects.Err()
}

// ReviewReceiptWait asks a waiting commitment's question after a poll: what
// was heard, and whether the wait is over. The poll's own answers came in
// through ApplyProviderEvent first, like any callback.
func (s *Store) ReviewReceiptWait(ctx context.Context, intentID string) (outbound.ApplyResult, error) {
	var groupID string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(alert_group_id, '') FROM outbound_intents WHERE id = $1`, intentID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.ApplyResult{}, nil
	}
	if err != nil {
		return outbound.ApplyResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.ApplyResult{}, err
	}
	defer tx.Rollback()
	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
		return outbound.ApplyResult{}, err
	}
	if groupID != "" {
		if err := lockAlertGroupTx(ctx, tx, groupID); err != nil {
			return outbound.ApplyResult{}, err
		}
	}
	intent, _, err := lockIntentTx(ctx, tx, intentID)
	if err != nil || intent == nil || intent.Status != outbound.StatusAwaitingReceipt {
		return outbound.ApplyResult{}, err
	}
	moved, err := reviewReceiptTx(ctx, tx, *intent, true)
	if err != nil {
		return outbound.ApplyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return outbound.ApplyResult{}, err
	}
	if moved.To != "" {
		countTerminal(intent.Family, moved.To)
	}
	return outbound.ApplyResult{Outcome: outbound.ApplyApplied, IntentID: intentID,
		To: moved.To, Row: moved.Row}, nil
}

// UnmatchedProviderEvents are kept events still waiting for their attempt, the
// oldest first. An event whose attempt never turns up stays here, counted, for
// an operator: it passed the provider's signature and names nothing this
// system made.
func (s *Store) UnmatchedProviderEvents(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM outbound_provider_events
		WHERE state = 'unmatched' AND attempt_id IS NOT NULL
		  AND EXISTS (SELECT 1 FROM outbound_attempts a WHERE a.id = attempt_id)
		ORDER BY received_at, id LIMIT $1`, limit)
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
	return ids, rows.Err()
}
