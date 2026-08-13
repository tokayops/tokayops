package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

var (
	ErrOutboxEventNotFound       = errors.New("outbox event not found")
	ErrOutboxDeliveryNotFound    = errors.New("outbox delivery not found")
	ErrOutboxDeliveryNotTerminal = errors.New("outbox delivery is not in a terminal state")
)

// CreateOutboxEvent inserts a new outbox event.
func (s *Store) CreateOutboxEvent(event *model.OutboxEvent) error {
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.Status == "" {
		event.Status = model.OutboxEventStatusPending
	}
	if event.Actor == "" {
		event.Actor = "system"
	}
	event.CreatedAt = time.Now()

	query := `INSERT INTO event_outbox (id, event_type, alert_group_id, team_id, actor, payload, status, attempts, next_attempt_at, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	_, err := s.db.Exec(query, event.ID, event.EventType, event.AlertGroupID, event.TeamID,
		event.Actor, event.Payload, event.Status, event.Attempts, event.NextAttemptAt, event.CreatedAt)
	return err
}

// GetOutboxEventByID retrieves an outbox event by ID.
func (s *Store) GetOutboxEventByID(id string) (*model.OutboxEvent, error) {
	query := `SELECT id, event_type, alert_group_id, team_id, actor, payload, status, attempts,
			  next_attempt_at, locked_until, locked_by, last_error, created_at, sent_at
			  FROM event_outbox WHERE id = $1`
	row := s.db.QueryRow(query, id)
	return scanOutboxEvent(row)
}

// GetPendingOutboxEvents retrieves outbox events ready for processing.
func (s *Store) GetPendingOutboxEvents(limit int) ([]*model.OutboxEvent, error) {
	query := `SELECT id, event_type, alert_group_id, team_id, actor, payload, status, attempts,
			  next_attempt_at, locked_until, locked_by, last_error, created_at, sent_at
			  FROM event_outbox
			  WHERE status IN ('pending', 'processing')
			    AND (locked_until IS NULL OR locked_until < NOW())
			    AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
			  ORDER BY next_attempt_at NULLS FIRST
			  LIMIT $1`
	rows, err := s.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutboxEvents(rows)
}

// UpdateOutboxEvent updates mutable fields of an outbox event.
func (s *Store) UpdateOutboxEvent(event *model.OutboxEvent) error {
	query := `UPDATE event_outbox SET status = $1, attempts = $2, next_attempt_at = $3,
			  locked_until = $4, locked_by = $5, last_error = $6, sent_at = $7
			  WHERE id = $8`
	result, err := s.db.Exec(query, event.Status, event.Attempts, event.NextAttemptAt,
		event.LockedUntil, event.LockedBy, event.LastError, event.SentAt, event.ID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrOutboxEventNotFound
	}
	return nil
}

// UpdateOutboxEventIfOwned updates mutable fields only if locked_by matches workerID.
// Returns (true, nil) if the row was updated, (false, nil) if ownership was lost.
func (s *Store) UpdateOutboxEventIfOwned(event *model.OutboxEvent, workerID string) (bool, error) {
	query := `UPDATE event_outbox SET status = $1, attempts = $2, next_attempt_at = $3,
			  locked_until = $4, locked_by = $5, last_error = $6, sent_at = $7
			  WHERE id = $8 AND locked_by = $9`
	result, err := s.db.Exec(query, event.Status, event.Attempts, event.NextAttemptAt,
		event.LockedUntil, event.LockedBy, event.LastError, event.SentAt, event.ID, workerID)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

// CreateOutboxDelivery inserts a new outbox delivery.
func (s *Store) CreateOutboxDelivery(delivery *model.OutboxDelivery) error {
	if delivery.ID == "" {
		delivery.ID = uuid.New().String()
	}
	if delivery.Status == "" {
		delivery.Status = model.OutboxDeliveryPending
	}
	delivery.CreatedAt = time.Now()

	query := `INSERT INTO event_outbox_deliveries (id, event_id, integration_id, status, attempts, next_attempt_at, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7)`

	_, err := s.db.Exec(query, delivery.ID, delivery.EventID, delivery.IntegrationID,
		delivery.Status, delivery.Attempts, delivery.NextAttemptAt, delivery.CreatedAt)
	return err
}

// GetOutboxDeliveryByID retrieves a delivery by its ID.
func (s *Store) GetOutboxDeliveryByID(id string) (*model.OutboxDelivery, error) {
	query := `SELECT id, event_id, integration_id, status, attempts, next_attempt_at,
			  last_http_status, last_error, request_payload, response_body_trunc, created_at, sent_at
			  FROM event_outbox_deliveries WHERE id = $1`
	row := s.db.QueryRow(query, id)
	return scanOutboxDelivery(row)
}

// GetOutboxDelivery retrieves a delivery by event_id and integration_id.
func (s *Store) GetOutboxDelivery(eventID, integrationID string) (*model.OutboxDelivery, error) {
	query := `SELECT id, event_id, integration_id, status, attempts, next_attempt_at,
			  last_http_status, last_error, request_payload, response_body_trunc, created_at, sent_at
			  FROM event_outbox_deliveries WHERE event_id = $1 AND integration_id = $2`
	row := s.db.QueryRow(query, eventID, integrationID)
	return scanOutboxDelivery(row)
}

// GetDeliveriesByEventID retrieves all deliveries for an event.
func (s *Store) GetDeliveriesByEventID(eventID string) ([]*model.OutboxDelivery, error) {
	query := `SELECT id, event_id, integration_id, status, attempts, next_attempt_at,
			  last_http_status, last_error, request_payload, response_body_trunc, created_at, sent_at
			  FROM event_outbox_deliveries WHERE event_id = $1 ORDER BY created_at`
	rows, err := s.db.Query(query, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutboxDeliveries(rows)
}

// GetDeliveriesByIntegrationID retrieves deliveries for an integration with pagination.
func (s *Store) GetDeliveriesByIntegrationID(integrationID string, limit, offset int) ([]*model.OutboxDelivery, int, error) {
	var total int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM event_outbox_deliveries WHERE integration_id = $1`, integrationID).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	query := `SELECT id, event_id, integration_id, status, attempts, next_attempt_at,
			  last_http_status, last_error, request_payload, response_body_trunc, created_at, sent_at
			  FROM event_outbox_deliveries WHERE integration_id = $1
			  ORDER BY created_at DESC LIMIT $2 OFFSET $3`
	rows, err := s.db.Query(query, integrationID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	deliveries, err := scanOutboxDeliveries(rows)
	if err != nil {
		return nil, 0, err
	}
	return deliveries, total, nil
}

// UpdateOutboxDelivery updates mutable fields of an outbox delivery.
func (s *Store) UpdateOutboxDelivery(delivery *model.OutboxDelivery) error {
	query := `UPDATE event_outbox_deliveries SET status = $1, attempts = $2, next_attempt_at = $3,
			  last_http_status = $4, last_error = $5, request_payload = $6, response_body_trunc = $7, sent_at = $8
			  WHERE id = $9`
	result, err := s.db.Exec(query, delivery.Status, delivery.Attempts, delivery.NextAttemptAt,
		delivery.LastHTTPStatus, delivery.LastError, delivery.RequestPayload, delivery.ResponseBodyTrunc,
		delivery.SentAt, delivery.ID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return ErrOutboxDeliveryNotFound
	}
	return nil
}

// ReplayOutboxDelivery atomically resets a delivery to pending and re-opens
// its parent event if the event is in a terminal state. Both writes happen
// inside a single transaction to avoid a delivery stuck under a terminal event.
func (s *Store) ReplayOutboxDelivery(deliveryID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Reset delivery to pending (CAS: only if terminal)
	res, err := tx.Exec(`UPDATE event_outbox_deliveries
		SET status = $1, attempts = 0, next_attempt_at = NULL,
			last_http_status = NULL, last_error = NULL, response_body_trunc = NULL, sent_at = NULL
		WHERE id = $2 AND status IN ($3, $4)`,
		model.OutboxDeliveryPending, deliveryID, model.OutboxDeliverySent, model.OutboxDeliveryFailed)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		// Distinguish not-found from not-terminal
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM event_outbox_deliveries WHERE id = $1)`, deliveryID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrOutboxDeliveryNotFound
		}
		return ErrOutboxDeliveryNotTerminal
	}

	// Get event_id for this delivery
	var eventID string
	err = tx.QueryRow(`SELECT event_id FROM event_outbox_deliveries WHERE id = $1`, deliveryID).Scan(&eventID)
	if err != nil {
		return err
	}

	// Re-open parent event if terminal
	_, err = tx.Exec(`UPDATE event_outbox
		SET status = 'processing', next_attempt_at = NULL, locked_until = NULL,
			locked_by = NULL, last_error = NULL, sent_at = NULL
		WHERE id = $1 AND status IN ('completed', 'failed')`,
		eventID)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ClaimOutboxEvents atomically claims a batch of events for processing.
func (s *Store) ClaimOutboxEvents(workerID string, limit int, leaseDuration time.Duration) ([]*model.OutboxEvent, error) {
	leaseSeconds := int(leaseDuration.Seconds())
	query := `
	WITH claimed AS (
		SELECT id FROM event_outbox
		WHERE status IN ('pending', 'processing')
		  AND (locked_until IS NULL OR locked_until < NOW())
		  AND (next_attempt_at IS NULL OR next_attempt_at <= NOW())
		ORDER BY next_attempt_at NULLS FIRST
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	)
	UPDATE event_outbox
	SET status = 'processing', locked_by = $2, locked_until = NOW() + $3 * interval '1 second'
	WHERE id IN (SELECT id FROM claimed)
	RETURNING id, event_type, alert_group_id, team_id, actor, payload, status, attempts,
		next_attempt_at, locked_until, locked_by, last_error, created_at, sent_at`

	rows, err := s.db.Query(query, limit, workerID, leaseSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOutboxEvents(rows)
}

// ExtendOutboxEventLease atomically extends a lease only if the caller still owns it.
func (s *Store) ExtendOutboxEventLease(eventID, workerID string, until time.Time) (bool, error) {
	query := `UPDATE event_outbox SET locked_until = $3
			  WHERE id = $1 AND locked_by = $2 AND status = 'processing'`
	result, err := s.db.Exec(query, eventID, workerID, until)
	if err != nil {
		return false, err
	}
	rows, _ := result.RowsAffected()
	return rows > 0, nil
}

// CreateDeliveryAttempt inserts an append-only attempt record.
func (s *Store) CreateDeliveryAttempt(attempt *model.DeliveryAttempt) error {
	if attempt.ID == "" {
		attempt.ID = uuid.New().String()
	}
	attempt.CreatedAt = time.Now()

	query := `INSERT INTO event_outbox_delivery_attempts (id, delivery_id, attempt, http_status, error, response_body_trunc, created_at)
			  VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err := s.db.Exec(query, attempt.ID, attempt.DeliveryID, attempt.Attempt,
		attempt.HTTPStatus, attempt.Error, attempt.ResponseBodyTrunc, attempt.CreatedAt)
	return err
}

// GetDeliveryAttempts retrieves all attempts for a delivery, ordered by attempt number.
func (s *Store) GetDeliveryAttempts(deliveryID string) ([]*model.DeliveryAttempt, error) {
	query := `SELECT id, delivery_id, attempt, http_status, error, response_body_trunc, created_at
			  FROM event_outbox_delivery_attempts WHERE delivery_id = $1 ORDER BY attempt`
	rows, err := s.db.Query(query, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var attempts []*model.DeliveryAttempt
	for rows.Next() {
		var a model.DeliveryAttempt
		if err := rows.Scan(&a.ID, &a.DeliveryID, &a.Attempt, &a.HTTPStatus,
			&a.Error, &a.ResponseBodyTrunc, &a.CreatedAt); err != nil {
			return nil, err
		}
		attempts = append(attempts, &a)
	}
	return attempts, nil
}

// scanOutboxEvent scans a single outbox event row.
func scanOutboxEvent(row *sql.Row) (*model.OutboxEvent, error) {
	var e model.OutboxEvent
	err := row.Scan(&e.ID, &e.EventType, &e.AlertGroupID, &e.TeamID, &e.Actor, &e.Payload,
		&e.Status, &e.Attempts, &e.NextAttemptAt, &e.LockedUntil, &e.LockedBy, &e.LastError,
		&e.CreatedAt, &e.SentAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOutboxEventNotFound
		}
		return nil, err
	}
	return &e, nil
}

// scanOutboxEvents scans multiple outbox event rows.
func scanOutboxEvents(rows *sql.Rows) ([]*model.OutboxEvent, error) {
	var events []*model.OutboxEvent
	for rows.Next() {
		var e model.OutboxEvent
		if err := rows.Scan(&e.ID, &e.EventType, &e.AlertGroupID, &e.TeamID, &e.Actor, &e.Payload,
			&e.Status, &e.Attempts, &e.NextAttemptAt, &e.LockedUntil, &e.LockedBy, &e.LastError,
			&e.CreatedAt, &e.SentAt); err != nil {
			return nil, err
		}
		events = append(events, &e)
	}
	return events, nil
}

// scanOutboxDelivery scans a single outbox delivery row.
func scanOutboxDelivery(row *sql.Row) (*model.OutboxDelivery, error) {
	var d model.OutboxDelivery
	err := row.Scan(&d.ID, &d.EventID, &d.IntegrationID, &d.Status, &d.Attempts, &d.NextAttemptAt,
		&d.LastHTTPStatus, &d.LastError, &d.RequestPayload, &d.ResponseBodyTrunc, &d.CreatedAt, &d.SentAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrOutboxDeliveryNotFound
		}
		return nil, err
	}
	return &d, nil
}

// scanOutboxDeliveries scans multiple outbox delivery rows.
func scanOutboxDeliveries(rows *sql.Rows) ([]*model.OutboxDelivery, error) {
	var deliveries []*model.OutboxDelivery
	for rows.Next() {
		var d model.OutboxDelivery
		if err := rows.Scan(&d.ID, &d.EventID, &d.IntegrationID, &d.Status, &d.Attempts, &d.NextAttemptAt,
			&d.LastHTTPStatus, &d.LastError, &d.RequestPayload, &d.ResponseBodyTrunc, &d.CreatedAt, &d.SentAt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, &d)
	}
	return deliveries, nil
}
