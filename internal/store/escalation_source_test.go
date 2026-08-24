package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
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
		`SELECT render_source_version FROM alert_groups WHERE id = $1`, agID).
		Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if source.RenderSourceVersion != version {
		t.Errorf("the projection says version %d, the group is at %d",
			source.RenderSourceVersion, version)
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
		SET status = $1, render_source_version = render_source_version + 1
		WHERE id = $2`, model.AlertGroupStatusAcknowledged, agID); err != nil {
		t.Fatalf("acknowledge the group: %v", err)
	}

	if got := mustSubmit(t, s, adm).Outcome; got != outbound.SubmitGroupNotAdmitted {
		t.Fatalf("an acknowledged group whose version also moved answered %q", got)
	}
}

// TestAnUnreadableAlertIsNotAnAlertWithNothingWrong. The alerts are what a
// message about the group says and what the escalation is planned from. Read as
// "no alerts", a damaged row would be frozen into a snapshot describing an
// alert with nothing firing - once, for every message of that escalation, with
// no revision that ever corrects it.
func TestAnUnreadableAlertIsNotAnAlertWithNothingWrong(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	agID := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "damaged-" + agID, Status: model.AlertGroupStatusNew,
		Title: "Disk filling up", Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_groups SET alerts_data = 'not json at all' WHERE id = $1`,
		agID); err != nil {
		t.Fatalf("damage the alerts: %v", err)
	}

	before := storageContractFailures(t, "alerts_data")

	if _, err := s.GetEscalationSources(ctx); err == nil {
		t.Fatal("a group whose alerts cannot be read was returned as one with none")
	}

	// The refusal can stop the admission scan until somebody fixes the row, so
	// it is counted rather than only returned: a risk taken deliberately has to
	// be visible before an operator notices the silence.
	if after := storageContractFailures(t, "alerts_data"); after <= before {
		t.Errorf("the storage-contract counter stayed at %v", before)
	}
}

func storageContractFailures(t *testing.T, field string) float64 {
	t.Helper()
	var m dto.Metric
	counter, err := metrics.StorageContractFailuresTotal.GetMetricWithLabelValues(field)
	if err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	if err := counter.Write(&m); err != nil {
		t.Fatalf("read the counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// TestTheVersionColumnKeepsItsValuesWhenRenamed. The column was called
// slack_update_generation, which was one of its two jobs under the name of a
// loop that is on its way out. A database that predates the rename must arrive
// at the new name with the same numbers in it: added instead of renamed, every
// group would come back at version zero and every plan built against a real
// version would be refused as stale, forever.
func TestTheVersionColumnKeepsItsValuesWhenRenamed(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	agID := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "renamed-" + agID, Status: model.AlertGroupStatusNew,
		Title: "Disk filling up", Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}

	// An older database: the column under its old name, with a group that has
	// been updated four times.
	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE alert_groups RENAME COLUMN render_source_version TO slack_update_generation`,
	); err != nil {
		t.Fatalf("build the old schema: %v", err)
	}
	t.Cleanup(func() {
		// Whatever this test proves, the suite continues against the new name.
		_ = s.applyLegacyColumnMigrations()
	})
	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_groups SET slack_update_generation = 4 WHERE id = $1`, agID); err != nil {
		t.Fatalf("seed the old version: %v", err)
	}

	if err := s.applyLegacyColumnMigrations(); err != nil {
		t.Fatalf("run the migrations: %v", err)
	}

	var version int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT render_source_version FROM alert_groups WHERE id = $1`, agID).
		Scan(&version); err != nil {
		t.Fatalf("read the version under its new name: %v", err)
	}
	if version != 4 {
		t.Fatalf("the group came out of the migration at version %d, want 4", version)
	}

	// And running it again changes nothing, which is what every start does.
	if err := s.applyLegacyColumnMigrations(); err != nil {
		t.Fatalf("run the migrations again: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT render_source_version FROM alert_groups WHERE id = $1`, agID).
		Scan(&version); err != nil {
		t.Fatalf("read the version again: %v", err)
	}
	if version != 4 {
		t.Fatalf("a second run left the group at version %d", version)
	}
}

// TestInstancesStartingTogetherMigrateOnce. Guarded steps are enough to run the
// block twice in a row and not enough to run it twice at once: the guard and
// the ALTER it protects are two statements, so a rename committing between them
// leaves the other instance adding a column that now exists. That start fails,
// the container restarts, and on a bad day it is a crash loop over a schema
// that is already correct.
func TestInstancesStartingTogetherMigrateOnce(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	agID := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "racing-" + agID, Status: model.AlertGroupStatusNew,
		Title: "Disk filling up", Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`ALTER TABLE alert_groups RENAME COLUMN render_source_version TO slack_update_generation`,
	); err != nil {
		t.Fatalf("build the old schema: %v", err)
	}
	t.Cleanup(func() { _ = s.applyLegacyColumnMigrations() })
	if _, err := s.db.ExecContext(ctx,
		`UPDATE alert_groups SET slack_update_generation = 7 WHERE id = $1`, agID); err != nil {
		t.Fatalf("seed the old version: %v", err)
	}

	// Eight instances coming up against one database, which is what a rollout
	// looks like.
	const instances = 8
	var wg sync.WaitGroup
	errs := make([]error, instances)
	start := make(chan struct{})
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.applyLegacyColumnMigrations()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d failed to start: %v", i, err)
		}
	}

	var version int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT render_source_version FROM alert_groups WHERE id = $1`, agID).
		Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	if version != 7 {
		t.Fatalf("the group came out of the rollout at version %d, want 7", version)
	}

	// One column, not two: an instance that added instead of renaming would
	// leave the old name behind with the numbers still in it.
	var columns int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'alert_groups'
		  AND column_name IN ('render_source_version', 'slack_update_generation')`).
		Scan(&columns); err != nil {
		t.Fatalf("read the catalogue: %v", err)
	}
	if columns != 1 {
		t.Fatalf("the table has %d version columns", columns)
	}
}
