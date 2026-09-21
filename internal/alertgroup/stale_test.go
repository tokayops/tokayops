package alertgroup

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// What a notification says about the alerts Alertmanager has stopped
// reporting, row by row of the design's table.
//
// Absence means something only in a snapshot - a payload that named the group,
// carried alerts, and had nothing cut off by max_alerts. What it never means
// is that the alert cleared: an alert nobody reports still holds the incident
// open, which is the line between an observation and a resolution.

var (
	markedAt   = time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	silentFrom = time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
)

func alertNamed(fingerprint string, status model.AlertStatus) model.Alert {
	return model.Alert{
		Fingerprint: fingerprint, Status: status,
		StartsAt: time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC),
		Labels:   map[string]string{"alertname": fingerprint},
	}
}

func staleAlert(fingerprint string, since time.Time) model.Alert {
	a := alertNamed(fingerprint, model.AlertStatusFiring)
	at := since
	a.StaleSince = &at
	return a
}

func stateOf(t *testing.T, alerts []model.Alert, fingerprint string) model.AlertState {
	t.Helper()
	for _, a := range alerts {
		if a.Fingerprint == fingerprint {
			return a.State()
		}
	}
	t.Fatalf("%s is not in the incident any more: %v", fingerprint, alerts)
	return ""
}

func TestWhatANotificationSaysAboutWhatIsStillReported(t *testing.T) {
	firing, resolved := model.AlertStatusFiring, model.AlertStatusResolved

	for _, c := range []struct {
		name          string
		held          []model.Alert
		notification  Notification
		wantStates    map[string]model.AlertState
		wantResolving bool
		wantMarked    int
		wantBack      []Reported
		wantHeldOnly  bool
	}{
		{
			name: "a snapshot without an alert the incident holds marks it",
			held: []model.Alert{alertNamed("A", firing), alertNamed("B", firing)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("B", firing)}, Snapshot: true,
			},
			wantStates: map[string]model.AlertState{
				"A": model.AlertStateStale, "B": model.AlertStateFiring,
			},
			wantMarked: 1,
		},
		{
			name: "the same snapshot again leaves the mark where it was",
			held: []model.Alert{staleAlert("A", silentFrom), alertNamed("B", firing)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("B", firing)}, Snapshot: true,
			},
			wantStates: map[string]model.AlertState{
				"A": model.AlertStateStale, "B": model.AlertStateFiring,
			},
			wantMarked: 0,
		},
		{
			name: "the rest of the group clears and the marked alert holds the incident",
			held: []model.Alert{staleAlert("A", silentFrom), alertNamed("B", firing)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("B", resolved)}, Snapshot: true,
			},
			wantStates: map[string]model.AlertState{
				"A": model.AlertStateStale, "B": model.AlertStateResolved,
			},
			wantResolving: false,
			wantHeldOnly:  true,
		},
		{
			name: "a payload cut short marks nothing",
			held: []model.Alert{alertNamed("A", firing), alertNamed("B", firing)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("B", firing)}, Snapshot: false,
			},
			wantStates: map[string]model.AlertState{
				"A": model.AlertStateFiring, "B": model.AlertStateFiring,
			},
			wantMarked: 0,
		},
		{
			name: "a marked alert reported again is reported again",
			held: []model.Alert{staleAlert("A", silentFrom), alertNamed("B", firing)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("A", firing), alertNamed("B", firing)}, Snapshot: true,
			},
			wantStates: map[string]model.AlertState{
				"A": model.AlertStateFiring, "B": model.AlertStateFiring,
			},
			wantBack: []Reported{{Status: firing, Silent: time.Hour}},
		},
		{
			name: "a marked alert that comes back resolved ends the incident",
			held: []model.Alert{staleAlert("A", silentFrom)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("A", resolved)}, Snapshot: true,
			},
			wantStates:    map[string]model.AlertState{"A": model.AlertStateResolved},
			wantResolving: true,
			wantBack:      []Reported{{Status: resolved, Silent: time.Hour}},
		},
		{
			name: "a snapshot of alerts this incident cannot take still marks its own",
			held: []model.Alert{alertNamed("A", firing)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("X", resolved)}, Snapshot: true,
			},
			wantStates:   map[string]model.AlertState{"A": model.AlertStateStale},
			wantMarked:   1,
			wantHeldOnly: true,
		},
		{
			name: "an incident already held only by stale alerts is not a new one",
			held: []model.Alert{staleAlert("A", silentFrom)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("X", resolved)}, Snapshot: true,
			},
			wantStates:   map[string]model.AlertState{"A": model.AlertStateStale},
			wantHeldOnly: false,
		},
		{
			name: "a resolved alert is not marked when it goes",
			held: []model.Alert{alertNamed("A", firing), alertNamed("B", resolved)},
			notification: Notification{
				Alerts: []model.Alert{alertNamed("A", firing)}, Snapshot: true,
			},
			wantStates: map[string]model.AlertState{
				"A": model.AlertStateFiring, "B": model.AlertStateResolved,
			},
			wantMarked: 0,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			marksBefore := map[string]time.Time{}
			for _, a := range c.held {
				if a.StaleSince != nil {
					marksBefore[a.Fingerprint] = *a.StaleSince
				}
			}

			applied := Apply(c.held, c.notification, markedAt)

			for fingerprint, want := range c.wantStates {
				if got := stateOf(t, applied.Alerts, fingerprint); got != want {
					t.Errorf("%s is %s, want %s", fingerprint, got, want)
				}
			}
			if applied.Resolving != c.wantResolving {
				t.Errorf("resolving = %v, want %v", applied.Resolving, c.wantResolving)
			}
			if applied.Marked != c.wantMarked {
				t.Errorf("marked %d alerts, want %d", applied.Marked, c.wantMarked)
			}
			if applied.HeldOnlyByStale != c.wantHeldOnly {
				t.Errorf("held only by stale = %v, want %v",
					applied.HeldOnlyByStale, c.wantHeldOnly)
			}
			if len(applied.Back) != len(c.wantBack) {
				t.Fatalf("%d alerts came back, want %d: %v", len(applied.Back), len(c.wantBack), applied.Back)
			}
			for i, want := range c.wantBack {
				if applied.Back[i] != want {
					t.Errorf("the alert that came back is %v, want %v", applied.Back[i], want)
				}
			}

			// A mark says when the silence STARTED. A payload that found the
			// alert still missing is not a second silence, so a mark that was
			// already there stays where it was.
			for _, a := range applied.Alerts {
				before, had := marksBefore[a.Fingerprint]
				if !had || a.StaleSince == nil {
					continue
				}
				if !a.StaleSince.Equal(before) {
					t.Errorf("the mark on %s moved from %v to %v", a.Fingerprint, before, a.StaleSince)
				}
			}

			// An alert is marked only while it is firing: the mark says
			// "Alertmanager stopped saying this is on fire", and there is no
			// such thing for one that cleared.
			for _, a := range applied.Alerts {
				if a.StaleSince != nil && a.Status != firing {
					t.Errorf("%s is %s and marked since %v", a.Fingerprint, a.Status, a.StaleSince)
				}
			}
		})
	}
}

// TestMarkStaleLeavesWhatItWasGivenAlone. Apply hands it a slice the
// merge has just built, so marking in place is invisible from there; this
// holds the function itself to the contract, because the next caller may not
// be handing it a copy.
func TestMarkStaleLeavesWhatItWasGivenAlone(t *testing.T) {
	held := []model.Alert{alertNamed("A", model.AlertStatusFiring)}

	marked, n := MarkStale(held, map[string]bool{}, markedAt)

	if n != 1 || marked[0].StaleSince == nil {
		t.Fatalf("the alert nobody reported was not marked (%d)", n)
	}
	if held[0].StaleSince != nil {
		t.Error("MarkStale marked the alerts it was given")
	}
}

// TestApplyLeavesWhatItWasGivenAlone. The incident's alerts are read under its
// lock and written back by the caller; a function that marked them in place
// would have already changed the row the caller compares against.
func TestApplyLeavesWhatItWasGivenAlone(t *testing.T) {
	held := []model.Alert{alertNamed("A", model.AlertStatusFiring)}
	payload := Notification{Alerts: []model.Alert{alertNamed("B", model.AlertStatusFiring)}, Snapshot: true}

	applied := Apply(held, payload, markedAt)

	if held[0].StaleSince != nil {
		t.Error("Apply marked the alerts it was given")
	}
	if stateOf(t, applied.Alerts, "A") != model.AlertStateStale {
		t.Error("the alert the snapshot left out was not marked in the result")
	}
}

// TestAClockThatStepsBackIsNotANegativeSilence. Both instants come from the
// database, and a step back there would otherwise be recorded as an alert that
// was silent for minus ten minutes.
func TestAClockThatStepsBackIsNotANegativeSilence(t *testing.T) {
	held := []model.Alert{staleAlert("A", markedAt.Add(10*time.Minute))}
	payload := Notification{
		Alerts: []model.Alert{alertNamed("A", model.AlertStatusFiring)}, Snapshot: true,
	}

	applied := Apply(held, payload, markedAt)

	if len(applied.Back) != 1 {
		t.Fatalf("%d alerts came back, want one", len(applied.Back))
	}
	if applied.Back[0].Silent != 0 {
		t.Errorf("the alert was silent for %v, want none of it", applied.Back[0].Silent)
	}
}
