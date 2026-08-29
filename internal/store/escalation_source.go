package store

import (
	"context"
	"fmt"

	"github.com/tokayops/tokayops/internal/model"
)

// Reading the state an escalation is planned from.
//
// The snapshot a producer freezes is what every message of that escalation is
// rendered from, forever. So it has to be a picture of ONE moment. A group read
// here and a version read somewhere else can already be describing two
// different alerts by the time they are put together, and nothing downstream
// would ever notice: the digest would faithfully record the collage.
//
// One statement makes it a picture. Everything a card is drawn from - the
// alerts included - lives on the group's own row, and the version it was at is
// read WITH it, so the admission can refuse a plan built from state that has
// moved since: see SubmitBatch, which checks it again under the lock
// that decides the escalation.
//
// The group's history used to be read here too, inside a repeatable-read
// transaction that existed to make the two reads agree with each other. It left
// the snapshot with tag 14 on 2026-08-25, and the transaction left with it: one
// statement is already one instant, and a transaction around it would only
// claim to be doing something.

// GetEscalationSources returns the alert groups nobody has been paged for, each
// with the alerts a card is drawn from and the version they were read at
// (AlertGroup.RenderSourceVersion).
func (s *Store) GetEscalationSources(ctx context.Context) ([]*model.AlertGroup, error) {
	return newAlertGroups(ctx, s.db)
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
func newAlertGroups(ctx context.Context, q sqlQueryer) ([]*model.AlertGroup, error) {
	rows, err := q.QueryContext(ctx, `
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
