package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// InsertScheduleEvent appends a schedule domain event inside the caller's
// transaction, so the event and the change it describes commit together or
// not at all.
func (t *scheduleConfigTx) InsertScheduleEvent(ctx context.Context, event *scheduleconfig.ScheduleEvent) error {
	return insertScheduleEventTx(ctx, t.tx, event)
}

// insertScheduleEventTx normalizes defaults here so callers cannot forget
// them. Unlike insertOutboxEventTx it does NOT treat nil as a no-op: an event
// that must be part of a configuration change would otherwise turn a failure
// to assemble it into a successful commit with no audit trail.
//
// The table carries no delivery columns. Consumers of schedule events are
// internal (handoff timers, usergroup sync), unlike the customer webhooks
// event_outbox fans out to, and event_outbox is bound to alert groups anyway.
func insertScheduleEventTx(ctx context.Context, tx *sql.Tx, event *scheduleconfig.ScheduleEvent) error {
	if event == nil {
		return fmt.Errorf("%w: nil schedule event", scheduleconfig.ErrInvariantViolation)
	}
	if event.ScheduleID == "" {
		return fmt.Errorf("%w: schedule event needs a schedule id", scheduleconfig.ErrInvariantViolation)
	}
	if event.EventType == "" {
		return fmt.Errorf("%w: schedule event needs a type", scheduleconfig.ErrInvariantViolation)
	}
	if event.ID == "" {
		event.ID = generateUUID()
	}
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage("{}")
	}
	// Checked here so malformed payloads read as a contract violation rather
	// than a raw PostgreSQL JSON parse error.
	if !json.Valid(event.Payload) {
		return fmt.Errorf("%w: schedule event payload is not valid JSON", scheduleconfig.ErrInvariantViolation)
	}
	event.RecordedAt = normalizeRecordedAt(event.RecordedAt)

	_, err := tx.ExecContext(ctx,
		`INSERT INTO schedule_events (id, schedule_id, event_type, payload, recorded_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		event.ID, event.ScheduleID, event.EventType, []byte(event.Payload), event.RecordedAt)
	if err != nil {
		return mapScheduleWriteError(err)
	}
	return nil
}
