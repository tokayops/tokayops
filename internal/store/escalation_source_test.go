package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The read a plan is built from has to be a picture of one moment, and it has
// to say which moment. Everything an escalation ever shows is frozen out of it,
// once, and no later revision corrects a card that was drawn from a collage.

// TestEscalationSourcesReadTheWholeAlert. The group, the alerts on it, the
// history behind it and the version they were read at, in one read. The history
// is the part that used to be missing, and it was missing silently: the cards
// simply had an empty history section forever.
func TestEscalationSourcesReadTheWholeAlert(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	agID := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "source-" + agID, Status: model.AlertGroupStatusNew,
		Title: "Disk filling up", Severity: "critical",
		Alerts: []model.Alert{{
			Fingerprint: "fp-1", Status: "firing", StartsAt: time.Unix(1700000000, 0),
			Labels: map[string]string{"alertname": "DiskWillFill"},
		}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}

	for _, event := range []*model.TimelineEvent{
		{ID: "ev-2", AlertGroupID: agID, Type: model.TimelineEventAlertAdded,
			Message: "A second alert joined", Actor: "system",
			CreatedAt: time.Unix(1700000200, 0)},
		{ID: "ev-1", AlertGroupID: agID, Type: model.TimelineEventCreated,
			Message: "Alert group created", Actor: "system",
			CreatedAt: time.Unix(1700000100, 0)},
	} {
		if err := s.AddTimelineEvent(event); err != nil {
			t.Fatalf("record the history: %v", err)
		}
	}

	// The group has been updated once since it was created, so its version is
	// not the default and a producer that never read one would disagree.
	if err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(agID, []model.Alert{{
		Fingerprint: "fp-1", Status: "firing", StartsAt: time.Unix(1700000000, 0),
		Labels: map[string]string{"alertname": "DiskWillFill"},
	}}); err != nil {
		t.Fatalf("update the alerts: %v", err)
	}

	sources, err := s.GetEscalationSources(ctx)
	if err != nil {
		t.Fatalf("read the escalation sources: %v", err)
	}

	var source *model.AlertGroup
	for _, candidate := range sources {
		if candidate.ID == agID {
			source = candidate
		}
	}
	if source == nil {
		t.Fatalf("the group waiting to be escalated was not returned")
	}

	if len(source.Alerts) != 1 || source.Alerts[0].Fingerprint != "fp-1" {
		t.Errorf("the alerts read back as %+v", source.Alerts)
	}
	if len(source.TimelineEvents) != 2 {
		t.Fatalf("the history read back with %d lines, want 2", len(source.TimelineEvents))
	}
	// Oldest first, which is the order a card shows and the snapshot hashes.
	if source.TimelineEvents[0].ID != "ev-1" || source.TimelineEvents[1].ID != "ev-2" {
		t.Errorf("the history reads %s then %s",
			source.TimelineEvents[0].ID, source.TimelineEvents[1].ID)
	}
	if source.TimelineEvents[0].Message != "Alert group created" {
		t.Errorf("the history line says %q", source.TimelineEvents[0].Message)
	}

	var version int64
	if err := s.db.QueryRow(
		`SELECT slack_update_generation FROM alert_groups WHERE id = $1`, agID).
		Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if source.SlackUpdateGeneration != version {
		t.Errorf("the projection says version %d, the group is at %d",
			source.SlackUpdateGeneration, version)
	}
	if version == 0 {
		t.Error("the fixture did not move the version, so the check above proves nothing")
	}
}

// TestSubmitRefusesAPlanBuiltFromStateThatMoved. Between the read and the
// admission, an alert joined the group. The plan in hand describes the alert as
// it was, and a snapshot is what every message of the escalation renders from
// for as long as it lives - so this one is refused whole, nothing is claimed,
// and the next tick plans it again from what is now there.
func TestSubmitRefusesAPlanBuiltFromStateThatMoved(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))

	// The producer read version 0; the group is now at 1.
	if err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(agID, []model.Alert{{
		Fingerprint: "fp-late", Status: "firing", StartsAt: time.Now(),
	}}); err != nil {
		t.Fatalf("change the alert group: %v", err)
	}

	result := mustSubmit(t, s, adm)
	if result.Outcome != outbound.SubmitSourceChanged {
		t.Fatalf("a plan built from state that moved answered %q", result.Outcome)
	}

	// Nothing claimed, nothing promised, nothing said about the group - so the
	// next tick picks it up and plans it again.
	var batches, intents, snapshots int
	if err := s.db.QueryRow(`
		SELECT (SELECT count(*) FROM outbound_batches WHERE alert_group_id = $1),
		       (SELECT count(*) FROM outbound_intents WHERE alert_group_id = $1),
		       (SELECT count(*) FROM outbound_group_snapshots WHERE alert_group_id = $1)`,
		agID).Scan(&batches, &intents, &snapshots); err != nil {
		t.Fatalf("count what was written: %v", err)
	}
	if batches != 0 || intents != 0 || snapshots != 0 {
		t.Fatalf("a refused plan left %d claims, %d commitments and %d snapshots",
			batches, intents, snapshots)
	}

	var policyID string
	if err := s.db.QueryRow(
		`SELECT coalesce(policy_id, '') FROM alert_groups WHERE id = $1`, agID).
		Scan(&policyID); err != nil {
		t.Fatalf("read the group: %v", err)
	}
	if policyID != "" {
		t.Fatalf("a refused plan recorded policy %q on the group", policyID)
	}

	// And the same plan, rebuilt against the version that is actually there, is
	// admitted - the refusal is a "not yet", not a dead end.
	adm.SourceVersion = 1
	if got := mustSubmit(t, s, adm).Outcome; got != outbound.SubmitCreated {
		t.Fatalf("the replanned admission answered %q", got)
	}
}

// TestSubmitAnswersTheUserBeforeTheVersion. Both are true - the alert changed
// AND the user acknowledged it - and the answers point opposite ways: one says
// try again next tick, the other says this group is finished with. The user
// wins, or the engine would keep replanning an alert nobody needs paging for.
func TestSubmitAnswersTheUserBeforeTheVersion(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	agID := outboundGroup(t, s)

	adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))

	if _, err := s.db.ExecContext(ctx, `
		UPDATE alert_groups
		SET status = $1, slack_update_generation = slack_update_generation + 1
		WHERE id = $2`, model.AlertGroupStatusAcknowledged, agID); err != nil {
		t.Fatalf("acknowledge the group: %v", err)
	}

	if got := mustSubmit(t, s, adm).Outcome; got != outbound.SubmitGroupNotAdmitted {
		t.Fatalf("an acknowledged group whose version also moved answered %q", got)
	}
}
