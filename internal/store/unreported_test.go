package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// The alerts Alertmanager has stopped reporting, as the database keeps them.
//
// The mark is an observation and nothing else: it does not end an incident,
// does not reach a message, and does not move when the same snapshot arrives
// again. What it does is make the difference visible - in the alert, in the
// counts a list answers with, and in what an operator can measure.

// twoAlertIncident opens an incident holding two firing alerts, the way a
// person does, so a payload can leave one of them out.
func twoAlertIncident(t *testing.T, s *Store) (id, key string) {
	t.Helper()
	id = uuid.New().String()
	key = "unreported-" + id
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: key, Status: model.AlertGroupStatusProcessing,
		Title: "Disk filling up", Severity: "critical", TeamID: "team-1",
		TeamNameSnapshot: "team-1",
		Alerts: []model.Alert{
			notifiedAlert("fp-0", model.AlertStatusFiring),
			notifiedAlert("fp-1", model.AlertStatusFiring),
		},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the incident: %v", err)
	}
	return id, key
}

// storedSnapshotRevision is the revision of the state this incident's messages
// are drawn from. It moves when what a message would say moves.
func storedSnapshotRevision(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	var revision int64
	if err := s.db.QueryRow(
		`SELECT revision FROM outbound_group_snapshots WHERE alert_group_id = $1`, id).
		Scan(&revision); err != nil {
		t.Fatalf("read the stored state of %s: %v", id, err)
	}
	return revision
}

// amSnapshot is a payload of Alertmanager's that carries the whole group.
func amSnapshot(alerts ...model.Alert) alertgroup.Notification {
	return alertgroup.Notification{Alerts: alerts, Snapshot: true}
}

func heldAlerts(t *testing.T, s *Store, id string) map[string]model.Alert {
	t.Helper()
	group, err := s.GetAlertGroupByID(id)
	if err != nil || group == nil {
		t.Fatalf("read the incident: %v", err)
	}
	held := make(map[string]model.Alert, len(group.Alerts))
	for _, a := range group.Alerts {
		held[a.Fingerprint] = a
	}
	return held
}

// TestASnapshotMarksTheAlertAlertmanagerLeftOut, and says so without telling
// any message: the mark is not in the render snapshot, so the payload comes
// back "unchanged" - recorded, nothing sent.
//
// The first payload is there to give the incident a desired state to compare
// against. Without it the mark would come back "merged" because the state was
// built for the first time, which says nothing about whether a message shows
// the mark.
func TestASnapshotMarksTheAlertAlertmanagerLeftOut(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	id, key := twoAlertIncident(t, s)

	// An escalation was admitted, so the incident has a state its messages are
	// drawn from - the thing a payload is compared against. The repeat after it
	// settles that state: the admission in this test carries a snapshot of the
	// test's own making, and the first payload after it is what brings the
	// stored state to what the incident actually says.
	mustSubmit(t, s, outboundAdmission(t, id, "first", channelCommitment("C0001", 0)))
	opening, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, amSnapshot(
		notifiedAlert("fp-0", model.AlertStatusFiring),
		notifiedAlert("fp-1", model.AlertStatusFiring),
		notifiedAlert("fp-2", model.AlertStatusFiring),
	), "system")
	if err != nil || opening.Outcome != alertgroup.MergeMerged {
		t.Fatalf("the alert that joined came back %s (%v)", opening.Outcome, err)
	}
	revision := storedSnapshotRevision(t, s, id)

	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, amSnapshot(
		notifiedAlert("fp-1", model.AlertStatusFiring),
		notifiedAlert("fp-2", model.AlertStatusFiring),
	), "system")
	if err != nil {
		t.Fatalf("apply the snapshot: %v", err)
	}
	if result.Outcome != alertgroup.MergeUnchanged {
		t.Errorf("the snapshot came back %s, want unchanged - no message shows the mark",
			result.Outcome)
	}

	if after := storedSnapshotRevision(t, s, id); after != revision {
		t.Errorf("the mark moved the state the messages are drawn from, %d to %d", revision, after)
	}

	held := heldAlerts(t, s, id)
	if state := held["fp-0"].State(); state != model.AlertStateUnreported {
		t.Errorf("the alert the snapshot left out is %s", state)
	}
	if state := held["fp-1"].State(); state != model.AlertStateFiring {
		t.Errorf("the alert the snapshot carried is %s", state)
	}
}

// TestTheMarkDoesNotMoveOnTheNextSnapshot. It says when the silence started,
// and Alertmanager repeats a notification for as long as the group lives.
func TestTheMarkDoesNotMoveOnTheNextSnapshot(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	id, key := twoAlertIncident(t, s)
	snapshot := amSnapshot(notifiedAlert("fp-1", model.AlertStatusFiring))

	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, snapshot, "system"); err != nil {
		t.Fatalf("apply the snapshot: %v", err)
	}
	first := heldAlerts(t, s, id)["fp-0"].UnreportedSince
	if first == nil {
		t.Fatal("the alert the snapshot left out was not marked")
	}

	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, snapshot, "system")
	if err != nil {
		t.Fatalf("apply the snapshot again: %v", err)
	}
	if result.Outcome != alertgroup.MergeUnchanged {
		t.Errorf("the repeated snapshot came back %s, want unchanged", result.Outcome)
	}
	again := heldAlerts(t, s, id)["fp-0"].UnreportedSince
	if again == nil || !again.Equal(*first) {
		t.Errorf("the mark moved from %v to %v", first, again)
	}
}

// TestAnUnreportedAlertHoldsTheIncidentOpen. This is the whole line between an
// observation and a resolution: the rest of the group clearing does not end an
// incident whose remaining alert was silenced rather than fixed.
func TestAnUnreportedAlertHoldsTheIncidentOpen(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	id, key := twoAlertIncident(t, s)

	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		amSnapshot(notifiedAlert("fp-1", model.AlertStatusFiring)), "system"); err != nil {
		t.Fatalf("apply the snapshot: %v", err)
	}
	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		amSnapshot(notifiedAlert("fp-1", model.AlertStatusResolved)), "system")
	if err != nil {
		t.Fatalf("apply the resolution: %v", err)
	}
	if result.Outcome == alertgroup.MergeResolved {
		t.Fatal("the incident ended while an alert nobody reports was still firing")
	}

	group, err := s.GetAlertGroupByID(id)
	if err != nil || group == nil {
		t.Fatalf("read the incident: %v", err)
	}
	if group.Status == model.AlertGroupStatusResolved {
		t.Errorf("the incident is %s", group.Status)
	}
	if state := group.Alerts[0].State(); state != model.AlertStateUnreported {
		t.Errorf("the alert nobody reports is %s", state)
	}
}

// TestAnAlertReportedAgainLosesItsMark, and the incident ends when it comes
// back resolved: a silence that ends is not a state of its own.
func TestAnAlertReportedAgainLosesItsMark(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	id, key := twoAlertIncident(t, s)

	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		amSnapshot(notifiedAlert("fp-1", model.AlertStatusFiring)), "system"); err != nil {
		t.Fatalf("apply the snapshot: %v", err)
	}

	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		amSnapshot(
			notifiedAlert("fp-0", model.AlertStatusFiring),
			notifiedAlert("fp-1", model.AlertStatusFiring),
		), "system")
	if err != nil {
		t.Fatalf("apply the snapshot that brought it back: %v", err)
	}
	if result.Outcome != alertgroup.MergeUnchanged && result.Outcome != alertgroup.MergeMerged {
		t.Fatalf("the snapshot came back %s", result.Outcome)
	}
	if held := heldAlerts(t, s, id); held["fp-0"].UnreportedSince != nil {
		t.Errorf("the alert Alertmanager reports again is still marked since %v",
			held["fp-0"].UnreportedSince)
	}
}

// TestTheListCountsTheThreeStates holds the SQL that counts for a list to the
// same answer as model.Alert.State, which is what everything else counts by.
func TestTheListCountsTheThreeStates(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	id := uuid.New().String()
	key := "counted-" + id
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: key, Status: model.AlertGroupStatusProcessing,
		Title: "Disk filling up", Severity: "critical", TeamID: "team-1",
		TeamNameSnapshot: "team-1",
		Alerts: []model.Alert{
			notifiedAlert("fp-0", model.AlertStatusFiring),
			notifiedAlert("fp-1", model.AlertStatusFiring),
			notifiedAlert("fp-2", model.AlertStatusFiring),
		},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the incident: %v", err)
	}

	// fp-1 clears, fp-2 stops being reported, fp-0 keeps firing.
	snapshot := amSnapshot(
		notifiedAlert("fp-0", model.AlertStatusFiring),
		notifiedAlert("fp-1", model.AlertStatusResolved),
	)
	snapshot.QuietAfterSeconds = 900
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, snapshot, "system"); err != nil {
		t.Fatalf("apply the snapshot: %v", err)
	}

	group, err := s.GetAlertGroupByID(id)
	if err != nil || group == nil {
		t.Fatalf("read the incident: %v", err)
	}
	inGo := map[model.AlertState]int{}
	for _, a := range group.Alerts {
		inGo[a.State()]++
	}
	if inGo[model.AlertStateFiring] != 1 || inGo[model.AlertStateUnreported] != 1 ||
		inGo[model.AlertStateResolved] != 1 {
		t.Fatalf("the incident holds %v, want one of each", inGo)
	}

	summaries, err := s.ListAlertGroupSummaries(nil, nil, 0, 50, 0, "", "")
	if err != nil {
		t.Fatalf("list the incidents: %v", err)
	}
	var summary *model.AlertGroupSummary
	for _, candidate := range summaries {
		if candidate.ID == id {
			summary = candidate
		}
	}
	if summary == nil {
		t.Fatal("the incident is not in the list")
	}
	if summary.AlertsCount != 3 ||
		summary.FiringCount != inGo[model.AlertStateFiring] ||
		summary.UnreportedCount != inGo[model.AlertStateUnreported] {
		t.Errorf("the list counts %d alerts, %d firing and %d unreported; the alerts say %v",
			summary.AlertsCount, summary.FiringCount, summary.UnreportedCount, inGo)
	}
	if resolved := summary.AlertsCount - summary.FiringCount - summary.UnreportedCount; resolved != 1 {
		t.Errorf("what is left over is %d alerts, want the one that resolved", resolved)
	}
	// The card is drawn from this row alone, so it also carries what says
	// whether Alertmanager has gone quiet about the group.
	if summary.LastNotifiedAt == nil {
		t.Error("the list does not say when Alertmanager last sent")
	}
	if summary.QuietAfterSeconds == nil || *summary.QuietAfterSeconds != 900 {
		t.Errorf("the list says quiet after %v, want the declared 900", summary.QuietAfterSeconds)
	}
}

// TestWhatIsMeasuredAboutSilence. The three counters are what a decision about
// resolving an incident without its unreported alerts would be made from, so
// each of them has to mean what it says: a mark, a return with the status it
// came back as, and the moment an incident came to be held by nothing else.
func TestWhatIsMeasuredAboutSilence(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	key := func() string {
		_, key := twoAlertIncident(t, s)
		return key
	}()

	marked := counterValue(t, "alerts_unreported_total", nil)
	held := counterValue(t, "alert_groups_held_by_unreported_total", nil)
	back := histogramCount(t, "alert_unreported_duration_seconds", "firing")

	// fp-0 stops being reported.
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		amSnapshot(notifiedAlert("fp-1", model.AlertStatusFiring)), "system"); err != nil {
		t.Fatal(err)
	}
	if got := counterValue(t, "alerts_unreported_total", nil) - marked; got != 1 {
		t.Errorf("one alert stopped being reported, counted %v", got)
	}
	if got := counterValue(t, "alert_groups_held_by_unreported_total", nil) - held; got != 0 {
		t.Errorf("the incident still has a firing alert, counted %v as held by silence", got)
	}

	// The alert that was still firing clears: now only the silent one holds it.
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		amSnapshot(notifiedAlert("fp-1", model.AlertStatusResolved)), "system"); err != nil {
		t.Fatal(err)
	}
	if got := counterValue(t, "alert_groups_held_by_unreported_total", nil) - held; got != 1 {
		t.Errorf("the incident came to be held by silence, counted %v", got)
	}

	// And the silent one is reported again, firing.
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, amSnapshot(
		notifiedAlert("fp-0", model.AlertStatusFiring),
		notifiedAlert("fp-1", model.AlertStatusResolved),
	), "system"); err != nil {
		t.Fatal(err)
	}
	if got := histogramCount(t, "alert_unreported_duration_seconds", "firing") - back; got != 1 {
		t.Errorf("one alert came back firing, measured %v", got)
	}
	if got := counterValue(t, "alert_groups_held_by_unreported_total", nil) - held; got != 1 {
		t.Errorf("the incident left the state and was counted %v times into it", got)
	}
}

// counterValue and histogramCount read what the registry gathers, not what a
// variable in this process holds: a counter that is incremented and never
// registered reads as nothing here, which is what Prometheus would see.
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			if !hasLabels(m.GetLabel(), labels) {
				continue
			}
			total += m.GetCounter().GetValue()
		}
		return total
	}
	t.Fatalf("%s is not exported", name)
	return 0
}

func histogramCount(t *testing.T, name, status string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		var total float64
		for _, m := range family.GetMetric() {
			if !hasLabels(m.GetLabel(), map[string]string{"status": status}) {
				continue
			}
			total += float64(m.GetHistogram().GetSampleCount())
		}
		return total
	}
	// Nothing observed yet: the vector has no series until the first one.
	return 0
}

func hasLabels(pairs []*dto.LabelPair, want map[string]string) bool {
	for name, value := range want {
		found := false
		for _, p := range pairs {
			if p.GetName() == name && p.GetValue() == value {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
