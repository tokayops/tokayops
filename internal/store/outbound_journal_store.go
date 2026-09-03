package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The delivery journal, read from the outside: a list of commitments over a
// period, and one alert group's deliveries. Both are reads over the tables the
// domain already writes; neither adds a table or a second journal.

// IntentFilter is what the operational journal can be narrowed by. Every field
// is optional except the period, which the store defaults rather than the
// caller: the last day, by the database's clock, so that a client that sent
// nothing and a client in another time zone get the same window.
type IntentFilter struct {
	Family     string
	Provider   string
	Statuses   []outbound.Status
	TargetKind keys.TargetKind
	TargetRef  string
	// AlertGroupID narrows to the commitments the group OWNS - the paging
	// ones. A webhook commitment belongs to its subscriber and is found by
	// EventID, or by the group's own delivery view, which goes through the
	// group's events.
	AlertGroupID string
	// EventID narrows to every commitment of every claim on one alert event:
	// the fan-out's and the replays', which have different keys and the same
	// event.
	EventID string
	From    *time.Time
	To      *time.Time
}

// ListIntents is the operational journal: commitments admitted in a period,
// newest first, narrowed by whatever the filter names.
//
// The period is bounded on both sides in SQL, with the database's own now()
// as the default - not the process's, which would make "the last day" mean
// different things on two instances whose clocks disagree.
func (s *Store) ListIntents(ctx context.Context, filter IntentFilter, limit, offset int) ([]outbound.Intent, int, error) {
	where := []string{
		"created_at >= COALESCE($1, now() - interval '24 hours')",
		"created_at < COALESCE($2, now())",
	}
	args := []any{nullTime(filter.From), nullTime(filter.To)}
	next := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if filter.Family != "" {
		next("delivery_family = $%d", filter.Family)
	}
	if filter.Provider != "" {
		next("provider = $%d", filter.Provider)
	}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			statuses = append(statuses, string(status))
		}
		next("status = ANY($%d)", pq.Array(statuses))
	}
	if filter.TargetKind != "" {
		next("target_kind = $%d", string(filter.TargetKind))
	}
	if filter.TargetRef != "" {
		next("target_ref = $%d", filter.TargetRef)
	}
	if filter.AlertGroupID != "" {
		next("alert_group_id = $%d", filter.AlertGroupID)
	}
	if filter.EventID != "" {
		next("batch_id IN (SELECT id FROM outbound_batches WHERE event_id = $%d)", filter.EventID)
	}
	predicate := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbound_intents WHERE `+predicate, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count the journal: %w", err)
	}

	pageArgs := append(append([]any(nil), args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE `+predicate+
		fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2),
		pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("read the journal: %w", err)
	}
	defer rows.Close()

	intents := []outbound.Intent{}
	for rows.Next() {
		intent, _, err := scanIntent(rows)
		if err != nil {
			return nil, 0, err
		}
		intents = append(intents, *intent)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return intents, total, nil
}

func nullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// groupDeliveriesSeam is a test hook, called before each read of the group's
// deliveries with the read's number. Production leaves it nil. It exists so a
// test can put a fan-out between two of the reads and prove that the answer is
// one snapshot: an event still pending in the first read and already claimed
// in the second is a state the database was never in.
var groupDeliveriesSeam func(step int)

func groupDeliveriesStep(step int) {
	if groupDeliveriesSeam != nil {
		groupDeliveriesSeam(step)
	}
}

// AlertGroupDeliveries is one group's deliveries: the paging commitments it
// owns, and its alert events with every claim on them and the webhook
// commitments under those.
//
// Four reads, one snapshot. They run under REPEATABLE READ, read-only, the way
// the commitment journal does, because a fan-out or a replay committing between
// two of them would splice two states into one answer - a pending event with a
// batch under it - and this is the view people reach for to establish what
// happened. The webhook half starts from the events and goes through the
// claims because that is the only path that finds a replay (its own claim, its
// own key, the same event) and the only one that can show an event the fan-out
// found no subscriber for.
func (s *Store) AlertGroupDeliveries(ctx context.Context, alertGroupID string) (*outbound.GroupDeliveries, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	out := &outbound.GroupDeliveries{Paging: []outbound.Intent{}, Events: []outbound.EventDeliveries{}}

	groupDeliveriesStep(1)
	if out.Paging, err = intentsWhereTx(ctx, tx,
		`alert_group_id = $1 ORDER BY idempotency_key`, alertGroupID); err != nil {
		return nil, fmt.Errorf("read the group's paging: %w", err)
	}

	groupDeliveriesStep(2)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, event_type, status, created_at FROM event_outbox
		WHERE alert_group_id = $1 ORDER BY created_at, id`, alertGroupID)
	if err != nil {
		return nil, fmt.Errorf("read the group's events: %w", err)
	}
	var eventIDs []string
	for rows.Next() {
		var e outbound.EventDeliveries
		if err := rows.Scan(&e.EventID, &e.EventType, &e.Status, &e.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		e.Batches = []outbound.BatchDeliveries{}
		out.Events = append(out.Events, e)
		eventIDs = append(eventIDs, e.EventID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(eventIDs) == 0 {
		return out, tx.Commit()
	}

	groupDeliveriesStep(3)
	rows, err = tx.QueryContext(ctx, `
		SELECT id, event_id, key_kind, admission_outcome, intent_count, admitted_at
		FROM outbound_batches WHERE event_id = ANY($1) ORDER BY admitted_at, id`, pq.Array(eventIDs))
	if err != nil {
		return nil, fmt.Errorf("read the claims on the group's events: %w", err)
	}
	byEvent := map[string][]outbound.BatchDeliveries{}
	for rows.Next() {
		var b outbound.BatchDeliveries
		var eventID string
		if err := rows.Scan(&b.BatchID, &eventID, &b.Kind, &b.Outcome, &b.IntentCount, &b.AdmittedAt); err != nil {
			rows.Close()
			return nil, err
		}
		b.Deliveries = []outbound.Intent{}
		byEvent[eventID] = append(byEvent[eventID], b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	groupDeliveriesStep(4)
	for eventID, batches := range byEvent {
		for i := range batches {
			batches[i].Deliveries, err = intentsWhereTx(ctx, tx,
				`batch_id = $1 ORDER BY idempotency_key`, batches[i].BatchID)
			if err != nil {
				return nil, fmt.Errorf("read the commitments of claim %s: %w", batches[i].BatchID, err)
			}
		}
		byEvent[eventID] = batches
	}
	for i := range out.Events {
		if batches, ok := byEvent[out.Events[i].EventID]; ok {
			out.Events[i].Batches = batches
		}
	}
	return out, tx.Commit()
}

// intentsWhereTx reads commitments by a predicate over the commitment row.
func intentsWhereTx(ctx context.Context, tx *sql.Tx, predicate string, args ...any) ([]outbound.Intent, error) {
	rows, err := tx.QueryContext(ctx, outboundIntentColumns+` FROM outbound_intents WHERE `+predicate, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	intents := []outbound.Intent{}
	for rows.Next() {
		intent, _, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		intents = append(intents, *intent)
	}
	return intents, rows.Err()
}
