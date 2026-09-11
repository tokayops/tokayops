package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

var (
	ErrOutboxEventNotFound = errors.New("outbox event not found")
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
