package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// When Alertmanager last sent anything about an alert group.
//
// Every payload that reaches an open incident writes it, whatever the payload
// meant, and nothing else does. What it must not do is move the two things a
// repeat has never moved - the time the group last changed and the version a
// producer reads - or go back.

// notifiedAlert is the one alert the fixture holds, or another in the same
// words: a payload repeating it exactly is a repeat.
func notifiedAlert(fingerprint string, status model.AlertStatus) model.Alert {
	return model.Alert{
		Fingerprint: fingerprint, Status: status, StartsAt: time.Unix(1700000000, 0),
		Labels: map[string]string{"alertname": "DiskWillFill"},
	}
}

// notifiedFixture opens an incident the way a person does, so the column
// starts empty and a test can see the payload fill it.
func notifiedFixture(t *testing.T, s *Store, key string, status model.AlertGroupStatus) string {
	t.Helper()
	id := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: key, Status: status,
		Title: "Disk filling up", Severity: "critical", TeamID: "team-1",
		TeamNameSnapshot: "team-1",
		Alerts:           []model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)},
		CreatedAt:        time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the incident: %v", err)
	}
	if at := lastNotifiedAt(t, s, id); at != nil {
		t.Fatalf("an incident opened by hand says it was notified at %v", at)
	}
	return id
}

func lastNotifiedAt(t *testing.T, s *Store, id string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := s.db.QueryRow(`SELECT last_notified_at FROM alert_groups WHERE id = $1`, id).
		Scan(&at); err != nil {
		t.Fatalf("read last_notified_at: %v", err)
	}
	return at
}

// TestEveryPayloadForAnOpenIncidentRecordsTheNotification. The quiet outcomes
// are the point: a repeat that changes nothing is the one that says
// Alertmanager is still sending.
func TestEveryPayloadForAnOpenIncidentRecordsTheNotification(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	alert := notifiedAlert
	for _, c := range []struct {
		name     string
		incoming model.Alert
		want     alertgroup.MergeOutcome
	}{
		{"a resolution nobody here has seen", alert("stranger", model.AlertStatusResolved), alertgroup.MergeIgnored},
		{"the same payload again", alert("fp-0", model.AlertStatusFiring), alertgroup.MergeUnchanged},
		{"a new alert joins", alert("fp-1", model.AlertStatusFiring), alertgroup.MergeMerged},
		{"the last alert clears", alert("fp-0", model.AlertStatusResolved), alertgroup.MergeResolved},
	} {
		t.Run(c.name, func(t *testing.T) {
			key := "notified-" + uuid.New().String()
			id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)

			result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
				[]model.Alert{c.incoming}, "system")
			if err != nil {
				t.Fatalf("apply the payload: %v", err)
			}
			if result.Outcome != c.want {
				t.Fatalf("the payload came back %s, want %s", result.Outcome, c.want)
			}
			if lastNotifiedAt(t, s, id) == nil {
				t.Errorf("a payload that came back %s did not record that Alertmanager sent it", result.Outcome)
			}
		})
	}
}

// TestARepeatRecordsTheNotificationAndNothingElse. updated_at is when the
// group last changed and the render source version is what a producer's plan
// is checked against; a repeat moving either would make "Last update" mean
// "last repeat" again, and refuse every plan built between two repeats.
func TestARepeatRecordsTheNotificationAndNothingElse(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	key := "repeat-" + uuid.New().String()
	id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)

	var updatedBefore time.Time
	if err := s.db.QueryRow(`SELECT updated_at FROM alert_groups WHERE id = $1`, id).
		Scan(&updatedBefore); err != nil {
		t.Fatal(err)
	}
	versionBefore := renderSourceVersion(t, s, id)

	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		[]model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)}, "system")
	if err != nil || result.Outcome != alertgroup.MergeUnchanged {
		t.Fatalf("the repeat came back %s (%v)", result.Outcome, err)
	}

	if lastNotifiedAt(t, s, id) == nil {
		t.Error("the repeat was not recorded")
	}
	var updatedAfter time.Time
	if err := s.db.QueryRow(`SELECT updated_at FROM alert_groups WHERE id = $1`, id).
		Scan(&updatedAfter); err != nil {
		t.Fatal(err)
	}
	if !updatedAfter.Equal(updatedBefore) {
		t.Errorf("the repeat moved updated_at from %v to %v", updatedBefore, updatedAfter)
	}
	if after := renderSourceVersion(t, s, id); after != versionBefore {
		t.Errorf("the repeat moved the render source version from %d to %d", versionBefore, after)
	}
}

// TestAPayloadLeavesAFinishedIncidentAlone. The key names the alert, not the
// incident: the finished one keeps the time the last payload reached it while
// it was open, and the payload belongs to the one open now.
func TestAPayloadLeavesAFinishedIncidentAlone(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	key := "finished-" + uuid.New().String()
	finished := notifiedFixture(t, s, key, model.AlertGroupStatusResolved)
	open := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)

	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		[]model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)}, "system"); err != nil {
		t.Fatalf("apply the payload: %v", err)
	}

	if lastNotifiedAt(t, s, open) == nil {
		t.Error("the open incident was not recorded")
	}
	if at := lastNotifiedAt(t, s, finished); at != nil {
		t.Errorf("the finished incident was recorded at %v", at)
	}
}

// TestAPayloadThatWaitedForTheLockRecordsWhenItGotIt. now() is when a
// transaction began, and this one began before it waited: recorded that way,
// the column would say Alertmanager was last heard from before a payload that
// had already committed. The test holds the incident, lets the payload queue
// behind it, reads the clock and lets go without writing anything - so there
// is no later value in the row for GREATEST to keep, and only the instant the
// payload itself records can pass.
func TestAPayloadThatWaitedForTheLockRecordsWhenItGotIt(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	key := "waited-" + uuid.New().String()
	id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)

	holder, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	if _, err := holder.Exec(`SELECT 1 FROM alert_groups WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("hold the incident: %v", err)
	}

	applied := make(chan error, 1)
	go func() {
		_, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
			[]model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)}, "system")
		applied <- err
	}()
	waitForLockWaiter(t, s, "the payload never queued behind the held incident")

	var released time.Time
	if err := holder.QueryRow(`SELECT clock_timestamp()`).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatalf("apply the payload: %v", err)
	}

	at := lastNotifiedAt(t, s, id)
	if at == nil {
		t.Fatal("the payload was not recorded")
	}
	if at.Before(released) {
		t.Errorf("the payload was recorded at %v, before the incident was let go at %v", at, released)
	}
}

// TestTheNotificationTimeNeverGoesBack. The clock of the database can step
// back; the last time Alertmanager was heard from cannot.
func TestTheNotificationTimeNeverGoesBack(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	key := "back-" + uuid.New().String()
	id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)

	var ahead time.Time
	if err := s.db.QueryRow(`
		UPDATE alert_groups SET last_notified_at = now() + interval '1 hour'
		WHERE id = $1 RETURNING last_notified_at`, id).Scan(&ahead); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		[]model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)}, "system"); err != nil {
		t.Fatalf("apply the payload: %v", err)
	}

	if at := lastNotifiedAt(t, s, id); at == nil || !at.Equal(ahead) {
		t.Errorf("the payload moved last_notified_at from %v to %v", ahead, at)
	}
}

// TestOnlyTheIngesterOpensAGroupThatWasNotified. The payload that opens an
// incident is the first thing Alertmanager said about it; an incident opened
// by hand has heard from nobody.
func TestOnlyTheIngesterOpensAGroupThatWasNotified(t *testing.T) {
	s := setupTestDB(t)

	byHand := notifiedFixture(t, s, "manual:"+uuid.New().String(), model.AlertGroupStatusNew)
	if at := lastNotifiedAt(t, s, byHand); at != nil {
		t.Errorf("an incident opened by hand was notified at %v", at)
	}

	fromAlertmanager := &model.AlertGroup{
		ID: uuid.New().String(), AlertKey: "ingested-" + uuid.New().String(),
		Status: model.AlertGroupStatusNew, Title: "Disk filling up", Severity: "critical",
		TeamID: "team-1", TeamNameSnapshot: "team-1",
		Alerts:    []model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := s.CreateAlertGroupAtomic(fromAlertmanager, nil, nil); err != nil {
		t.Fatalf("open the incident: %v", err)
	}
	if lastNotifiedAt(t, s, fromAlertmanager.ID) == nil {
		t.Error("an incident opened by a payload does not say it was notified")
	}
}
