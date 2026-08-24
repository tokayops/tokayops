package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/model"
)

// Reading the state an escalation is planned from.
//
// The snapshot a producer freezes is what every message of that escalation is
// rendered from, forever. So it has to be a picture of ONE moment. A group read
// here, a history read there and a version read somewhere else can already be
// describing three different alerts by the time they are put together, and
// nothing downstream would ever notice: the digest would faithfully record the
// collage.
//
// Two things make it a picture. Everything is read inside one repeatable-read
// transaction, so the parts agree with each other. And the version the group
// was at is read WITH them, so the admission can refuse a plan built from state
// that has moved since - see SubmitEscalationBatch, which checks it again under
// the lock that decides the escalation.

// GetEscalationSources returns the alert groups nobody has been paged for, each
// with the alerts and the history a card is drawn from, and the version they
// were read at (AlertGroup.SlackUpdateGeneration).
//
// Read-only and repeatable read. Read-only because it is: the isolation level
// is what this is for, and saying so lets Postgres treat it as such. Repeatable
// read because the group and its history are two statements, and at the default
// level the second one would see writes the first one did not.
func (s *Store) GetEscalationSources(ctx context.Context) ([]*model.AlertGroup, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("read the escalation sources: %w", err)
	}
	defer tx.Rollback()

	groups, err := newAlertGroupsTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, nil
	}
	if err := historyForTx(ctx, tx, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// newAlertGroupsTx finds the groups whose escalation has not been admitted.
//
// It also picks up stale "processing" groups orphaned by a crash between the
// status change and the admission - but ONLY if nothing was admitted for them.
// An admission is the claim over a group's escalation and it is held forever,
// whatever became of the deliveries under it, so a group that has one has been
// escalated and must never be picked up again.
//
// It asks the admissions rather than the jobs, and that is not a cosmetic
// change of table: once the escalation job is gone, "no escalation job" becomes
// true of every group in the system, and every processing group would come back
// round every thirty seconds to be escalated again.
//
// Asked by alert_group_id rather than by a batch key: the question is what this
// group already holds, not what holds a particular claim.
func newAlertGroupsTx(ctx context.Context, tx *sql.Tx) ([]*model.AlertGroup, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+alertGroupColumns+` FROM alert_groups ag
		WHERE ag.status = $1
		   OR (ag.status = $2 AND ag.updated_at < now() - interval '30 seconds'
		       AND NOT EXISTS (
		           SELECT 1 FROM outbound_batches b WHERE b.alert_group_id = ag.id
		       ))`,
		model.AlertGroupStatusNew, model.AlertGroupStatusProcessing)
	if err != nil {
		return nil, fmt.Errorf("read the escalation sources: %w", err)
	}
	defer rows.Close()

	var groups []*model.AlertGroup
	for rows.Next() {
		ag, err := scanAlertGroupRow(rows)
		if err != nil {
			return nil, fmt.Errorf("read the escalation sources: %w", err)
		}
		groups = append(groups, ag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the escalation sources: %w", err)
	}
	return groups, nil
}

// historyForTx attaches each group's history, in one query rather than one per
// group. The order is the order a card shows it in, and the one the snapshot
// hashes: oldest first, ties broken by id.
func historyForTx(ctx context.Context, tx *sql.Tx, groups []*model.AlertGroup) error {
	ids := make([]string, 0, len(groups))
	byID := make(map[string]*model.AlertGroup, len(groups))
	for _, ag := range groups {
		ids = append(ids, ag.ID)
		byID[ag.ID] = ag
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, alert_group_id, type, message, actor, metadata, created_at
		FROM timeline_events
		WHERE alert_group_id = ANY($1)
		ORDER BY created_at ASC, id ASC`, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("read the escalation history: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e model.TimelineEvent
		var metadata sql.NullString
		if err := rows.Scan(&e.ID, &e.AlertGroupID, &e.Type, &e.Message, &e.Actor,
			&metadata, &e.CreatedAt); err != nil {
			return fmt.Errorf("read the escalation history: %w", err)
		}
		if metadata.Valid && metadata.String != "" {
			_ = json.Unmarshal([]byte(metadata.String), &e.Metadata)
		}
		if ag := byID[e.AlertGroupID]; ag != nil {
			ag.TimelineEvents = append(ag.TimelineEvents, &e)
		}
	}
	return rows.Err()
}
