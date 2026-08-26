package store

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// TestWhatCameOfARevisionIsCounted.
//
// Who raised it and what came of it are two different questions, and the second
// is the one that says whether a message is going to change. Counting them in
// one label - the mistake this replaced - makes "an acknowledgement happened"
// and "a card was aimed at a new revision" indistinguishable.
//
// Counted at the doors, after the transaction commits: a revision the database
// rolled back is not a revision.
func TestWhatCameOfARevisionIsCounted(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	count := func(reason outbound.DesiredReason, outcome outbound.DesiredOutcome) float64 {
		return testutil.ToFloat64(metrics.OutboundDesiredRevisionsTotal.
			WithLabelValues(string(reason), string(outcome)))
	}
	rose := func(t *testing.T, reason outbound.DesiredReason,
		outcome outbound.DesiredOutcome, before float64) {
		t.Helper()
		if after := count(reason, outcome); after <= before {
			t.Errorf("%s/%s was not counted", reason, outcome)
		}
	}

	t.Run("an acknowledgement that aims the card", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk filling up")
		changeableCard(t, s, agID)
		before := count(outbound.DesiredAck, outbound.DesiredApplied)
		changed, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil)
		if err != nil || !changed {
			t.Fatalf("acknowledge: changed=%v err=%v", changed, err)
		}
		rose(t, outbound.DesiredAck, outbound.DesiredApplied, before)
	})

	t.Run("an alert group nobody was told about", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk quiet")
		moveGroup(t, s, agID, model.AlertGroupStatusTriggered)
		before := count(outbound.DesiredAck, outbound.DesiredNoSnapshot)
		changed, err := s.AckAlertGroupAtomic(agID, "nina", nil, nil)
		if err != nil || !changed {
			t.Fatalf("acknowledge: changed=%v err=%v", changed, err)
		}
		rose(t, outbound.DesiredAck, outbound.DesiredNoSnapshot, before)
	})

	// The alert set moved and the card would look identical: an end time on an
	// alert that is still firing is recorded and shown nowhere. Recording it is
	// right; editing three messages to say the same thing is not.
	t.Run("alerts that change nothing a message shows", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk slow")
		changeableCard(t, s, agID)

		// A real revision first, so what is stored is what this group renders
		// to. Without it the comparison is against the state the fixture
		// admitted from, and everything differs from that.
		arrived := []model.Alert{{
			Fingerprint: "fp-2", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000600, 0),
			Labels:   map[string]string{"alertname": "DiskSlow"},
		}}
		if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(),
			"desired-"+agID, arrived, "ingester"); err != nil {
			t.Fatalf("the first payload: %v", err)
		}

		invisible := []model.Alert{{
			Fingerprint: "fp-2", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000600, 0),
			EndsAt:   time.Unix(1700009999, 0),
			Labels:   map[string]string{"alertname": "DiskSlow"},
		}}
		before := count(outbound.DesiredMerge, outbound.DesiredUnchanged)
		result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(),
			"desired-"+agID, invisible, "ingester")
		if err != nil {
			t.Fatalf("the payload: %v", err)
		}
		if result.Outcome != alertgroup.MergeUnchanged {
			t.Fatalf("the payload came back %s", result.Outcome)
		}
		rose(t, outbound.DesiredMerge, outbound.DesiredUnchanged, before)
	})

	// stale_after_final has no subtest, and that is the honest answer: no door
	// that counts can reach it. A merge into a resolved incident is refused
	// before the desired state is touched, and neither transition moves a group
	// that has already ended. The outcome exists because the command must
	// answer something when the snapshot is already final, and it is proven
	// where it is produced - see the command's own tests.
	t.Run("a resolution", func(t *testing.T) {
		agID := desiredGroup(t, s, "Disk noisy")
		changeableCard(t, s, agID)

		before := count(outbound.DesiredResolve, outbound.DesiredApplied)
		changed, err := s.ResolveAlertGroupAtomic(agID, "nina", nil, nil)
		if err != nil || !changed {
			t.Fatalf("resolve: changed=%v err=%v", changed, err)
		}
		rose(t, outbound.DesiredResolve, outbound.DesiredApplied, before)
	})
}
