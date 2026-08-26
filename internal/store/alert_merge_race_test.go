package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// What two webhooks for one alert may and may not do to each other.
//
// The decision - merge this in, or end the incident - used to be worked out
// from a read taken before anything was held, and then written. Two payloads
// starting from the same read reached different conclusions and the second to
// commit decided, so an alert was lost or an incident ended without it. The
// tests below start both sides at once, many times over, and check the only
// thing that matters: nothing anybody sent disappears.

func mergeRaceGroup(t *testing.T, s *Store) (string, string) {
	t.Helper()
	id := uuid.New().String()
	key := "race-" + id
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: key, Status: model.AlertGroupStatusProcessing,
		Title: "Disk filling up", Severity: "critical", TeamID: "team-1",
		Alerts: []model.Alert{{
			Fingerprint: "fp-0", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000000, 0),
			Labels:   map[string]string{"alertname": "DiskWillFill"},
		}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the incident: %v", err)
	}
	return id, key
}

func firingAlert(fingerprint string, at time.Time) model.Alert {
	return model.Alert{
		Fingerprint: fingerprint, Status: model.AlertStatusFiring, StartsAt: at,
		Labels: map[string]string{"alertname": fingerprint},
	}
}

func heldFingerprints(t *testing.T, s *Store, agID string) map[string]bool {
	t.Helper()
	group, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("read the incident: %v", err)
	}
	held := make(map[string]bool, len(group.Alerts))
	for _, a := range group.Alerts {
		held[a.Fingerprint] = true
	}
	return held
}

// TestTwoFiringPayloadsAtOnceKeepBothAlerts is AG-1.
//
// Both payloads add an alert the incident has never seen. Whichever order they
// take, the incident has to end up holding both: a read-modify-write worked out
// before the lock lets the second writer overwrite the first one's alert with a
// set that never contained it.
func TestTwoFiringPayloadsAtOnceKeepBothAlerts(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	for round := 0; round < raceRounds; round++ {
		agID, key := mergeRaceGroup(t, s)

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for _, fingerprint := range []string{"fp-a", "fp-b"} {
			wg.Add(1)
			go func(fingerprint string) {
				defer wg.Done()
				_, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
					[]model.Alert{firingAlert(fingerprint, time.Unix(1700000100, 0))}, "system")
				errs <- err
			}(fingerprint)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: apply the payload: %v", round, err)
			}
		}

		held := heldFingerprints(t, s, agID)
		for _, fingerprint := range []string{"fp-0", "fp-a", "fp-b"} {
			if !held[fingerprint] {
				t.Fatalf("round %d: %s is not in the incident: %v", round, fingerprint, held)
			}
		}
	}
}

// TestAFiringPayloadAgainstAResolutionLosesNothing. A person resolves the
// incident while Alertmanager sends an alert that is still firing.
//
// Both orders are legal and neither loses the alert. If the payload lands
// first, the alert is in the incident the person then ends. If the resolution
// lands first, the payload finds nothing open - and "no open incident" is what
// tells the caller this alert belongs to the NEXT one, which is the only answer
// that does not put a firing alert into an incident nobody will be paged for.
func TestAFiringPayloadAgainstAResolutionLosesNothing(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	measuringLockOrder(t, s)

	for round := 0; round < raceRounds; round++ {
		agID, key := mergeRaceGroup(t, s)

		var wg sync.WaitGroup
		var applied alertgroup.MergeResult
		var applyErr, resolveErr error
		var resolved bool

		wg.Add(2)
		go func() {
			defer wg.Done()
			applied, applyErr = s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
				[]model.Alert{firingAlert("fp-a", time.Unix(1700000100, 0))}, "system")
		}()
		go func() {
			defer wg.Done()
			resolved, resolveErr = s.ResolveAlertGroupAtomic(agID, "nina", nil, nil)
		}()
		wg.Wait()

		if applyErr != nil {
			t.Fatalf("round %d: apply the payload: %v", round, applyErr)
		}
		if resolveErr != nil {
			t.Fatalf("round %d: resolve: %v", round, resolveErr)
		}
		if !resolved {
			t.Fatalf("round %d: the resolution did not happen", round)
		}

		switch applied.Outcome {
		case alertgroup.MergeMerged, alertgroup.MergeUnchanged:
			// The payload went in first: the alert is in the incident that was
			// then ended, and the history of it is complete.
			if !heldFingerprints(t, s, agID)["fp-a"] {
				t.Fatalf("round %d: the merged alert is not in the incident", round)
			}
		case alertgroup.MergeNoActive:
			// The resolution went in first. The alert belongs to the next
			// incident, and this one was not rewritten after it ended.
			if heldFingerprints(t, s, agID)["fp-a"] {
				t.Fatalf("round %d: a firing alert was written into an incident that had ended", round)
			}
		default:
			t.Fatalf("round %d: the payload came back %s", round, applied.Outcome)
		}

		var status string
		if err := s.db.QueryRow(`SELECT status FROM alert_groups WHERE id = $1`, agID).
			Scan(&status); err != nil {
			t.Fatalf("round %d: read the incident: %v", round, err)
		}
		if status != string(model.AlertGroupStatusResolved) {
			t.Fatalf("round %d: the incident is %s", round, status)
		}
	}
}

// TestAResolvingPayloadAgainstAnAcknowledgementEndsTheIncidentOnce. The last
// alert clears while somebody acknowledges. Whichever order, the incident ends
// exactly once and the acknowledgement does not resurrect it.
func TestAResolvingPayloadAgainstAnAcknowledgementEndsTheIncidentOnce(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	measuringLockOrder(t, s)

	for round := 0; round < raceRounds; round++ {
		agID, key := mergeRaceGroup(t, s)

		var wg sync.WaitGroup
		var applied alertgroup.MergeResult
		var applyErr, ackErr error

		wg.Add(2)
		go func() {
			defer wg.Done()
			applied, applyErr = s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
				[]model.Alert{{
					Fingerprint: "fp-0", Status: model.AlertStatusResolved,
					StartsAt: time.Unix(1700000000, 0),
					Labels:   map[string]string{"alertname": "DiskWillFill"},
				}}, "system")
		}()
		go func() {
			defer wg.Done()
			_, ackErr = s.AckAlertGroupAtomic(agID, "nina", nil, nil)
		}()
		wg.Wait()

		if applyErr != nil {
			t.Fatalf("round %d: apply the payload: %v", round, applyErr)
		}
		if ackErr != nil {
			t.Fatalf("round %d: acknowledge: %v", round, ackErr)
		}

		var status string
		if err := s.db.QueryRow(`SELECT status FROM alert_groups WHERE id = $1`, agID).
			Scan(&status); err != nil {
			t.Fatalf("round %d: read the incident: %v", round, err)
		}

		switch applied.Outcome {
		case alertgroup.MergeResolved:
			if status != string(model.AlertGroupStatusResolved) {
				t.Fatalf("round %d: the incident ended and is now %s", round, status)
			}
		case alertgroup.MergeNoActive:
			t.Fatalf("round %d: an acknowledgement closed the incident to Alertmanager", round)
		default:
			// Acknowledged first, then the alert cleared: still resolved.
			if status != string(model.AlertGroupStatusResolved) {
				t.Fatalf("round %d: the last alert cleared and the incident is %s (%s)",
					round, status, applied.Outcome)
			}
		}
	}
}

// The two orders, settled rather than raced.
//
// The concurrent tests above prove the two sides cannot lose an alert between
// them, but nothing in them decides which side commits first - so neither can
// say what a given order has to produce. These do: the first transaction
// commits before the second begins.

// TestAMergeThatCommitsFirstIsInTheIncidentThatEnds.
func TestAMergeThatCommitsFirstIsInTheIncidentThatEnds(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID, key := mergeRaceGroup(t, s)

	applied, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		[]model.Alert{firingAlert("fp-a", time.Unix(1700000100, 0))}, "system")
	if err != nil || applied.Outcome != alertgroup.MergeMerged {
		t.Fatalf("the merge came back %s (%v)", applied.Outcome, err)
	}

	resolved, err := s.ResolveAlertGroupAtomic(agID, "nina", nil, nil)
	if err != nil || !resolved {
		t.Fatalf("resolve: %v %v", resolved, err)
	}

	held := heldFingerprints(t, s, agID)
	if !held["fp-a"] {
		t.Fatal("the alert that arrived first is not in the incident that ended")
	}
	// And the incident ended once, with the alert in it.
	var status string
	if err := s.db.QueryRow(`SELECT status FROM alert_groups WHERE id = $1`, agID).
		Scan(&status); err != nil {
		t.Fatalf("read the incident: %v", err)
	}
	if status != string(model.AlertGroupStatusResolved) {
		t.Fatalf("the incident is %s", status)
	}
}

// TestAResolutionThatCommitsFirstSendsTheAlertToTheNextIncident.
//
// The store's half is the answer "there is nothing open". What that answer is
// FOR is the other half, and it is asserted at the level that acts on it: the
// ingester opens the next incident, and the alert nobody could merge is in it.
func TestAResolutionThatCommitsFirstSendsTheAlertToTheNextIncident(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID, key := mergeRaceGroup(t, s)

	resolved, err := s.ResolveAlertGroupAtomic(agID, "nina", nil, nil)
	if err != nil || !resolved {
		t.Fatalf("resolve: %v %v", resolved, err)
	}

	applied, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key,
		[]model.Alert{firingAlert("fp-a", time.Unix(1700000100, 0))}, "system")
	if err != nil {
		t.Fatalf("apply the payload: %v", err)
	}
	if applied.Outcome != alertgroup.MergeNoActive {
		t.Fatalf("a payload for a finished incident came back %s", applied.Outcome)
	}
	if heldFingerprints(t, s, agID)["fp-a"] {
		t.Fatal("a firing alert was written into an incident that had ended")
	}

	// The alert key is free again, which is what lets the next incident open.
	var open int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM alert_groups
		WHERE alert_key = $1 AND status NOT IN ($2, $3)`,
		key, model.AlertGroupStatusResolved, model.AlertGroupStatusClosed).
		Scan(&open); err != nil {
		t.Fatalf("count the open incidents: %v", err)
	}
	if open != 0 {
		t.Fatalf("%d incidents are still open for the alert", open)
	}
}
