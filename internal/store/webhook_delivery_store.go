package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The delivery routes of the API, over the webhook family's commitments.
//
// The routes and their form are the ones subscribers' operators have always
// had; the source is the journal. A "delivery" is one commitment addressed to
// one subscriber, its attempts are the commitment's attempt records, and a
// replay is a NEW commitment with its own admission - never a terminal one
// brought back to life.

var (
	// ErrWebhookDeliveryNotFound covers everything this route has no delivery
	// for: no such commitment, another subscriber's, another family's, or a
	// subscriber that no longer exists when a replay asks for one.
	ErrWebhookDeliveryNotFound = errors.New("webhook delivery not found")
	// ErrWebhookDeliveryNotTerminal is a replay of a commitment still being
	// worked on: a second live commitment beside it would be a guaranteed
	// duplicate, not a probable one.
	ErrWebhookDeliveryNotTerminal = errors.New("webhook delivery is still in progress")
	// ErrWebhookSubscriberDisabled is a replay to a subscriber that is switched
	// off. Switching it off withdrew what it was owed, and what was withdrawn
	// is terminal - so it can be asked for again, and a replay that obliged
	// would hand the work back through the other door. "Off" means "do not
	// send" at every door.
	ErrWebhookSubscriberDisabled = errors.New("webhook subscriber is disabled")
)

// WebhookReplayRequest is one operator asking for one event to be delivered to
// one subscriber again.
type WebhookReplayRequest struct {
	IntegrationID string
	DeliveryID    string
	// ClientRequestID is the operator's idempotency key: one per decision. A
	// repeat of the request with the same key finds the same commitment.
	ClientRequestID string
	Actor           string
}

// WebhookReplayResult is the commitment the replay stands for - created now, or
// found already admitted under the same key. The two are the same answer.
type WebhookReplayResult struct {
	Outcome    outbound.SubmitOutcome
	DeliveryID string
}

// deliveryColumns is the projection's read, one row per commitment. The three
// "last" fields come from ONE record - the last one in the journal, of any
// kind - so an error is never shown beside the success that followed it, and
// the reason a preparation refused for good is shown when nothing was ever
// sent. The attempt count and the attempt list take only records of kind
// attempt: a refusal before the call is not an HTTP request that happened.
const deliveryColumns = `
	i.id, i.payload->>'event_id', i.target_ref, i.status, i.next_attempt_at,
	i.payload->>'body', i.created_at,
	(SELECT count(*) FROM outbound_attempts a WHERE a.intent_id = i.id AND a.record_kind = 'attempt'),
	last.provider_status, last.error_class, last.response_summary, last.finished_at`

const deliveryFrom = `
	FROM outbound_intents i
	LEFT JOIN LATERAL (
		SELECT provider_status, error_class, response_summary, finished_at
		FROM outbound_attempts a WHERE a.intent_id = i.id
		ORDER BY attempt_no DESC LIMIT 1) last ON TRUE`

type deliveryRow struct {
	id, eventID, integrationID string
	status                     outbound.Status
	nextAttemptAt              time.Time
	body                       sql.NullString
	createdAt                  time.Time
	attempts                   int
	lastStatus, lastError      sql.NullString
	lastSummary                sql.NullString
	lastFinished               sql.NullTime
}

func scanDelivery(row interface{ Scan(...any) error }) (deliveryRow, error) {
	var r deliveryRow
	err := row.Scan(&r.id, &r.eventID, &r.integrationID, &r.status, &r.nextAttemptAt, &r.body,
		&r.createdAt, &r.attempts, &r.lastStatus, &r.lastError, &r.lastSummary, &r.lastFinished)
	return r, err
}

// ListWebhookDeliveries is the history of one subscriber, newest first.
func (s *Store) ListWebhookDeliveries(ctx context.Context, integrationID string,
	limit, offset int) ([]*model.OutboxDelivery, int, error) {

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM outbound_intents
		WHERE delivery_family = 'webhook' AND target_ref = $1`, integrationID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count the deliveries of %s: %w", integrationID, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryColumns+deliveryFrom+`
		WHERE i.delivery_family = 'webhook' AND i.target_ref = $1
		ORDER BY i.created_at DESC, i.id DESC
		LIMIT $2 OFFSET $3`, integrationID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list the deliveries of %s: %w", integrationID, err)
	}
	defer rows.Close()
	deliveries := []*model.OutboxDelivery{}
	for rows.Next() {
		r, err := scanDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		delivery, err := projectDelivery(r)
		if err != nil {
			return nil, 0, err
		}
		deliveries = append(deliveries, delivery)
	}
	return deliveries, total, rows.Err()
}

// WebhookDelivery is one commitment with its attempts. It answers not found
// for a delivery that is not this subscriber's, so an id learned elsewhere
// opens nothing.
func (s *Store) WebhookDelivery(ctx context.Context, integrationID, deliveryID string) (
	*model.OutboxDelivery, []*model.DeliveryAttempt, error) {

	r, err := scanDelivery(s.db.QueryRowContext(ctx, `SELECT `+deliveryColumns+deliveryFrom+`
		WHERE i.id = $1 AND i.delivery_family = 'webhook' AND i.target_ref = $2`, deliveryID, integrationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read delivery %s: %w", deliveryID, err)
	}
	delivery, err := projectDelivery(r)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, attempt_no, provider_status, error_class, response_summary, started_at, finished_at
		FROM outbound_attempts
		WHERE intent_id = $1 AND record_kind = 'attempt'
		ORDER BY attempt_no`, deliveryID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the attempts of %s: %w", deliveryID, err)
	}
	defer rows.Close()
	attempts := []*model.DeliveryAttempt{}
	for rows.Next() {
		var (
			attempt              model.DeliveryAttempt
			no                   int
			status, class, brief sql.NullString
			started, finished    sql.NullTime
		)
		if err := rows.Scan(&attempt.ID, &no, &status, &class, &brief, &started, &finished); err != nil {
			return nil, nil, err
		}
		// The old path numbered attempts from zero; the journal from one. The
		// form stays what it was.
		attempt.DeliveryID = deliveryID
		attempt.Attempt = no - 1
		attempt.HTTPStatus = httpStatusOf(status)
		attempt.Error = nullable(class)
		attempt.ResponseBodyTrunc = nullable(brief)
		if started.Valid {
			attempt.CreatedAt = started.Time
		} else if finished.Valid {
			attempt.CreatedAt = finished.Time
		}
		attempts = append(attempts, &attempt)
	}
	return delivery, attempts, rows.Err()
}

// projectDelivery is the old form over a commitment.
func projectDelivery(r deliveryRow) (*model.OutboxDelivery, error) {
	status, err := projectDeliveryStatus(r.status, r.attempts)
	if err != nil {
		return nil, outboundContractf("commitment %s: %v", r.id, err)
	}
	delivery := &model.OutboxDelivery{
		ID:                r.id,
		EventID:           r.eventID,
		IntegrationID:     r.integrationID,
		Status:            status,
		Attempts:          r.attempts,
		LastHTTPStatus:    httpStatusOf(r.lastStatus),
		LastError:         nullable(r.lastError),
		RequestPayload:    nullable(r.body),
		ResponseBodyTrunc: nullable(r.lastSummary),
		CreatedAt:         r.createdAt,
	}
	switch status {
	case model.OutboxDeliveryPending, model.OutboxDeliveryRetry:
		// The column is NOT NULL and a terminal commitment keeps a stale value
		// in it; only a live one has a next attempt.
		next := r.nextAttemptAt
		delivery.NextAttemptAt = &next
	case model.OutboxDeliverySent:
		if r.lastFinished.Valid {
			at := r.lastFinished.Time
			delivery.SentAt = &at
		}
	case model.OutboxDeliveryFailed:
		// Four domain endings read as one word here, and the detail has to say
		// which. When the last record names the error, that is the reason; when
		// nothing was ever attempted - expired before a call, withdrawn - the
		// ending itself is.
		if delivery.LastError == nil {
			reason := string(r.status)
			delivery.LastError = &reason
		}
	}
	return delivery, nil
}

// projectDeliveryStatus maps the domain's vocabulary onto the route's. Closed
// over every status the domain has: a new one is a compile-time question of
// how it should read, not an empty string on a screen.
func projectDeliveryStatus(status outbound.Status, attempts int) (model.OutboxDeliveryStatus, error) {
	switch status {
	case outbound.StatusPending, outbound.StatusSending:
		if attempts == 0 {
			return model.OutboxDeliveryPending, nil
		}
		return model.OutboxDeliveryRetry, nil
	case outbound.StatusSucceeded:
		return model.OutboxDeliverySent, nil
	case outbound.StatusPermanentFailed, outbound.StatusExpired, outbound.StatusCanceled,
		outbound.StatusManualReview:
		return model.OutboxDeliveryFailed, nil
	case outbound.StatusIdle:
		// A one-shot commitment is never idle: idle is the state of an editable
		// message between revisions.
		return "", fmt.Errorf("a webhook delivery cannot be %q", status)
	}
	return "", fmt.Errorf("status %q is not one this build projects", status)
}

func httpStatusOf(provider sql.NullString) *int {
	if !provider.Valid {
		return nil
	}
	code, err := strconv.Atoi(provider.String)
	if err != nil {
		return nil
	}
	return &code
}

func nullable(s sql.NullString) *string {
	if !s.Valid || s.String == "" {
		return nil
	}
	value := s.String
	return &value
}

// ReplayWebhookDelivery admits a new commitment for the same event to the same
// subscriber, in one transaction with the reads that decide whether it may.
//
// The original is read typed - this family, one of this family's two kinds -
// and without a lock: a webhook commitment's terminal state is final, because
// the operator's retries are refused for these kinds and the replay is their
// one door to a new effect. A terminal read now cannot come alive later.
//
// Before its body is copied, the original's payload is checked against the
// digest it was admitted with - the same check every attempt makes. Without it
// a payload swapped on the row, which an attempt would refuse, would be
// legitimised by a fresh digest on a fresh commitment and go out.
//
// The subscriber is read FOR SHARE, like the fan-out reads its audience, and
// it has to be a webhook subscriber that is switched on. Deletion and
// disabling take FOR UPDATE on the same row, so a replay that read a living,
// enabled subscriber cannot commit after either did - the command waits, and
// then withdraws the new commitment too. A deleted subscriber is not found; the
// tombstone lets its history be read and lets nothing be made for it. A
// switched-off one is refused: what the switch withdrew stays withdrawn.
func (s *Store) ReplayWebhookDelivery(ctx context.Context, req WebhookReplayRequest) (WebhookReplayResult, error) {
	policy, err := outbound.PolicyOf(outbound.FamilyWebhook)
	if err != nil {
		return WebhookReplayResult{}, err
	}
	grammar, err := keys.CurrentGrammarVersion(keys.KindWebhookReplay)
	if err != nil {
		return WebhookReplayResult{}, outboundContractf("%v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebhookReplayResult{}, err
	}
	defer tx.Rollback()
	if err := setLockTimeoutTx(ctx, tx, s.lockTimeout); err != nil {
		return WebhookReplayResult{}, err
	}

	original, _, err := scanIntent(tx.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1 AND delivery_family = 'webhook'`, req.DeliveryID))
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookReplayResult{}, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return WebhookReplayResult{}, fmt.Errorf("read delivery %s: %w", req.DeliveryID, err)
	}
	if original.TargetRef != req.IntegrationID ||
		(original.KeyKind != keys.KindWebhookEvent && original.KeyKind != keys.KindWebhookReplay) {
		return WebhookReplayResult{}, ErrWebhookDeliveryNotFound
	}
	switch original.Status {
	case outbound.StatusSucceeded, outbound.StatusPermanentFailed, outbound.StatusExpired, outbound.StatusCanceled:
	default:
		return WebhookReplayResult{}, ErrWebhookDeliveryNotTerminal
	}
	if _, err := admittedPayloadDigest(*original); err != nil {
		return WebhookReplayResult{}, err
	}
	payload, err := keys.DecodeWebhookPayloadV1(original.PayloadSchemaVersion, original.Payload)
	if err != nil {
		return WebhookReplayResult{}, outboundContractf("the payload of %s: %v", original.ID, err)
	}

	var enabled bool
	err = tx.QueryRowContext(ctx, `SELECT enabled FROM integrations WHERE id = $1 AND type = $2 FOR SHARE`,
		req.IntegrationID, model.IntegrationTypeGenericWebhook).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookReplayResult{}, ErrWebhookDeliveryNotFound
	}
	if err != nil {
		return WebhookReplayResult{}, busyOr(err)
	}
	if !enabled {
		return WebhookReplayResult{}, ErrWebhookSubscriberDisabled
	}

	admitted, err := admitWebhookTx(ctx, tx, keys.WebhookBatch{
		Kind:               keys.KindWebhookReplay,
		EventID:            payload.EventID,
		EventType:          payload.EventType,
		Body:               payload.Body,
		IntegrationIDs:     []string{req.IntegrationID},
		ClientRequestID:    req.ClientRequestID,
		Expiry:             policy.Expiry,
		GrammarVersion:     grammar,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
	}, req.Actor)
	if err != nil {
		return WebhookReplayResult{}, busyOr(err)
	}
	switch admitted.Outcome {
	case outbound.SubmitCreated, outbound.SubmitExisting:
	default:
		// A replay batch is one commitment derived from (event, subscriber,
		// key). Another composition under the same key is a defect, not a race.
		return WebhookReplayResult{}, outboundContractf(
			"replay %s of %s answered %q: another composition under the same key",
			req.ClientRequestID, req.DeliveryID, admitted.Outcome)
	}
	if len(admitted.IntentIDs) != 1 {
		return WebhookReplayResult{}, outboundContractf(
			"replay %s of %s admitted %d commitments, want one", req.ClientRequestID, req.DeliveryID,
			len(admitted.IntentIDs))
	}
	if err := tx.Commit(); err != nil {
		return WebhookReplayResult{}, err
	}
	// After the commit: created and existing are different series on purpose -
	// the share of existing is the visible rate of lost answers.
	metrics.OutboundAdmissionsTotal.WithLabelValues(outbound.FamilyWebhook,
		outbound.AdmissionLabel(admitted.Outcome, len(admitted.IntentIDs))).Inc()
	return WebhookReplayResult{Outcome: admitted.Outcome, DeliveryID: admitted.IntentIDs[0]}, nil
}
