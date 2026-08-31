package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// Applying what Alertmanager sent to the incident that is open.
//
// One command, one decision, one lock. The HTTP layer used to read the incident,
// work out whether the payload merged into it or ended it, and then write - and
// the read was taken before anything was held. Two webhooks for one alert could
// therefore decide differently from the same starting point: one merged a new
// alert in, the other resolved the incident without it, and whichever committed
// second won. A firing alert was lost, or written into an incident that was
// already over and would never page anybody for it.
//
// So the decision moved to where the row is held. What the caller supplies is
// the payload; what it gets back is what happened.
func (s *Store) ApplyAlertmanagerUpdateAtomic(ctx context.Context, alertKey string,
	incoming []model.Alert, actor string) (alertgroup.MergeResult, error) {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return alertgroup.MergeResult{}, err
	}
	defer tx.Rollback()

	// The incident that is OPEN, and only that one. A resolved one is over: the
	// same alert firing again belongs to the next incident, and the partial
	// unique index says there is at most one row here to take.
	//
	// Under Read Committed this is also what settles the race with an
	// acknowledgement or a resolution arriving at the same moment. A waiter
	// released by their commit re-checks the predicate against the row they
	// left behind, finds it no longer open, and comes back with no incident -
	// which is exactly right, because the alert it carries belongs to the next
	// one.
	group, err := scanAlertGroupRow(tx.QueryRowContext(ctx,
		`SELECT `+alertGroupColumns+` FROM alert_groups
		 WHERE alert_key = $1 AND status NOT IN ($2, $3) FOR UPDATE`,
		alertKey, model.AlertGroupStatusResolved, model.AlertGroupStatusClosed))
	if err != nil {
		return alertgroup.MergeResult{}, fmt.Errorf("read the open incident for %s: %w", alertKey, err)
	}
	if group == nil {
		return alertgroup.MergeResult{Outcome: alertgroup.MergeNoActive}, nil
	}

	held := alertgroup.FingerprintsOf(group.Alerts)
	relevant := alertgroup.FilterMergeable(incoming, held)
	if len(relevant) == 0 {
		return alertgroup.MergeResult{
			Outcome: alertgroup.MergeIgnored, AlertGroupID: group.ID,
		}, nil
	}

	merged := alertgroup.MergeAlerts(group.Alerts, relevant)
	resolving := alertgroup.AllResolved(merged)

	// A repeat that says exactly what the incident already says is not news:
	// no write, no revision, no edit of a message into what it already showed.
	// The end of an incident is the exception - a group whose alerts have all
	// cleared has to end even if this payload told us nothing new.
	if !resolving && alertgroup.SameAlerts(group.Alerts, merged) {
		return alertgroup.MergeResult{
			Outcome: alertgroup.MergeUnchanged, AlertGroupID: group.ID,
		}, nil
	}

	// Time comes from the database, like every other instant this system
	// records: the history of one incident cannot be ordered by two clocks.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return alertgroup.MergeResult{}, err
	}
	events := alertgroup.MergeTimelineEvents(group.ID, relevant, held, now)

	group.Alerts = merged
	if resolving {
		return s.resolveByAlertmanagerTx(ctx, tx, group, events, now, actor)
	}
	return s.mergeAlertsTx(ctx, tx, group, events, actor)
}

// mergeAlertsTx records a changed alert set and tells the incident's messages
// that what they show has moved.
func (s *Store) mergeAlertsTx(ctx context.Context, tx *sql.Tx, group *model.AlertGroup,
	events []*model.TimelineEvent, actor string) (alertgroup.MergeResult, error) {

	if err := writeAlertsTx(ctx, tx, group); err != nil {
		return alertgroup.MergeResult{}, err
	}
	if err := addTimelineEventsTx(ctx, tx, events); err != nil {
		return alertgroup.MergeResult{}, err
	}

	desired, err := setDesiredStateTx(ctx, tx, s.render, outbound.DesiredStateRequest{
		AlertGroupID: group.ID, Reason: outbound.DesiredMerge, Actor: actor,
	})
	if err != nil {
		return alertgroup.MergeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return alertgroup.MergeResult{}, err
	}

	countDesired(outbound.DesiredMerge, desired.Outcome)

	outcome := alertgroup.MergeMerged
	if desired.Outcome == outbound.DesiredUnchanged {
		// The alerts moved in a way no message shows - a resolved alert whose
		// end time changed, say. Recorded, and nothing was sent.
		outcome = alertgroup.MergeUnchanged
	}
	return alertgroup.MergeResult{Outcome: outcome, AlertGroupID: group.ID}, nil
}

// resolveByAlertmanagerTx ends the incident because everything in it cleared.
//
// The same commit as the alerts, the history, the webhook event and the
// withdrawal of what the incident still owed: "resolved" and "nobody is being
// paged any more" are one fact, and a crash between them pages somebody about
// an alert that has already stopped.
func (s *Store) resolveByAlertmanagerTx(ctx context.Context, tx *sql.Tx,
	group *model.AlertGroup, events []*model.TimelineEvent, now time.Time,
	actor string) (alertgroup.MergeResult, error) {

	alertsJSON, err := json.Marshal(group.Alerts)
	if err != nil {
		return alertgroup.MergeResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_groups
		SET alerts_data = $2, status = $3, resolved_at = now(), resolved_by = $4,
		    updated_at = now(), render_source_version = render_source_version + 1
		WHERE id = $1`,
		group.ID, string(alertsJSON), model.AlertGroupStatusResolved, actor,
	); err != nil {
		return alertgroup.MergeResult{}, fmt.Errorf("resolve %s: %w", group.ID, err)
	}
	group.Status = model.AlertGroupStatusResolved
	group.ResolvedBy = actor

	events = append(events, &model.TimelineEvent{
		ID:           uuid.New().String(),
		AlertGroupID: group.ID,
		Type:         model.TimelineEventResolved,
		Message:      "Alert group resolved: all alerts cleared",
		Actor:        actor,
		CreatedAt:    now.Add(time.Duration(len(events)+1) * time.Microsecond),
	})
	if err := addTimelineEventsTx(ctx, tx, events); err != nil {
		return alertgroup.MergeResult{}, err
	}
	// Everything this transaction writes to the history belongs to one
	// sequence. The withdrawal below is written last and must READ last: taking
	// now() for it would put it before the lines above, which carry microsecond
	// offsets from the same instant, and the history would say the
	// notifications were withdrawn before the alert cleared.
	withdrawalAt := now.Add(time.Duration(len(events)+1) * time.Microsecond)

	payload, err := model.BuildWebhookEventPayload(
		model.OutboxEventResolved, group, group.TeamNameSnapshot, actor, "", now)
	if err != nil {
		return alertgroup.MergeResult{}, fmt.Errorf("build the resolution event for %s: %w", group.ID, err)
	}
	if err := insertOutboxEventTx(tx, &model.OutboxEvent{
		EventType:    model.OutboxEventResolved,
		AlertGroupID: group.ID,
		TeamID:       group.TeamID,
		Actor:        actor,
		Payload:      payload,
	}); err != nil {
		return alertgroup.MergeResult{}, err
	}

	withdrawn, err := cancelIntentsAtTx(ctx, tx, group.ID, "the alert cleared", actor, withdrawalAt)
	if err != nil {
		return alertgroup.MergeResult{}, err
	}
	desired, err := setDesiredStateTx(ctx, tx, s.render, outbound.DesiredStateRequest{
		AlertGroupID: group.ID, Reason: outbound.DesiredResolve, Actor: actor,
	})
	if err != nil {
		return alertgroup.MergeResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return alertgroup.MergeResult{}, err
	}
	// By alert group, so every one of them is paging: a handover names no
	// alert group and cannot be among these.
	countWithdrawn(map[string]int{outbound.FamilyNotification: withdrawn})
	countDesired(outbound.DesiredResolve, desired.Outcome)
	return alertgroup.MergeResult{
		Outcome: alertgroup.MergeResolved, AlertGroupID: group.ID,
	}, nil
}

// writeAlertsTx records the alert set and moves the version a producer reads.
func writeAlertsTx(ctx context.Context, tx *sql.Tx, group *model.AlertGroup) error {
	alertsJSON, err := json.Marshal(group.Alerts)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_groups
		SET alerts_data = $2, updated_at = now(),
		    render_source_version = render_source_version + 1
		WHERE id = $1`, group.ID, string(alertsJSON)); err != nil {
		return fmt.Errorf("record the alerts of %s: %w", group.ID, err)
	}
	return nil
}

// addTimelineEventsTx writes the history a payload produced.
//
// Each line carries its own instant rather than taking now(): several written
// in one transaction would otherwise share a timestamp, and a history that
// cannot be ordered is not a history. The microsecond offsets come from the
// pure function that built them.
func addTimelineEventsTx(ctx context.Context, tx *sql.Tx, events []*model.TimelineEvent) error {
	for _, e := range events {
		metadata := []byte("{}")
		if len(e.Metadata) > 0 {
			encoded, err := json.Marshal(e.Metadata)
			if err != nil {
				return fmt.Errorf("record %s in the timeline: %w", e.Type, err)
			}
			metadata = encoded
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO timeline_events
				(id, alert_group_id, type, message, actor, metadata, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.ID, e.AlertGroupID, e.Type, e.Message, e.Actor, metadata, e.CreatedAt,
		); err != nil {
			return fmt.Errorf("record %s in the timeline: %w", e.Type, err)
		}
	}
	return nil
}
