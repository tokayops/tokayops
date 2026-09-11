package alertgroup_test

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// The arithmetic of putting an Alertmanager payload together with the incident
// it names. It is pure so it can run inside the transaction that holds the
// incident - which is the only place the answer is safe to work out.

func TestFilterMergeableAlerts(t *testing.T) {
	existing := map[string]model.AlertStatus{
		"known-firing":   model.AlertStatusFiring,
		"known-resolved": model.AlertStatusResolved,
	}

	tests := []struct {
		name     string
		incoming []model.Alert
		expected []string
	}{
		{
			name:     "unknown resolved alert is dropped",
			incoming: []model.Alert{{Fingerprint: "stranger", Status: model.AlertStatusResolved}},
			expected: nil,
		},
		{
			name:     "unknown firing alert joins the group",
			incoming: []model.Alert{{Fingerprint: "newcomer", Status: model.AlertStatusFiring}},
			expected: []string{"newcomer"},
		},
		{
			name:     "known alert resolving is kept",
			incoming: []model.Alert{{Fingerprint: "known-firing", Status: model.AlertStatusResolved}},
			expected: []string{"known-firing"},
		},
		{
			name:     "known alert re-firing is kept",
			incoming: []model.Alert{{Fingerprint: "known-resolved", Status: model.AlertStatusFiring}},
			expected: []string{"known-resolved"},
		},
		{
			name:     "known alert with unchanged status is kept",
			incoming: []model.Alert{{Fingerprint: "known-firing", Status: model.AlertStatusFiring}},
			expected: []string{"known-firing"},
		},
		{
			name: "mixed payload keeps everything but the unknown resolved alert",
			incoming: []model.Alert{
				{Fingerprint: "known-firing", Status: model.AlertStatusResolved},
				{Fingerprint: "stranger", Status: model.AlertStatusResolved},
				{Fingerprint: "newcomer", Status: model.AlertStatusFiring},
			},
			expected: []string{"known-firing", "newcomer"},
		},
		{
			name:     "empty payload stays empty",
			incoming: nil,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := alertgroup.FilterMergeable(tt.incoming, existing)
			if len(got) != len(tt.expected) {
				t.Fatalf("got %d alerts, want %d (%v)", len(got), len(tt.expected), got)
			}
			// Order is preserved, so index comparison is safe.
			for i, fp := range tt.expected {
				if got[i].Fingerprint != fp {
					t.Errorf("alert %d = %q, want %q", i, got[i].Fingerprint, fp)
				}
			}
		})
	}
}

// TestMergeAlertsIsOrderedAndLatestWins. Two payloads that say the same thing
// have to produce the same row, or an incident rewrites itself on every repeat
// for no reason anybody can see. The order is the one a message lists alerts
// in - by when they started - so the stored set, the snapshot and the card do
// not disagree.
func TestMergeAlertsIsOrderedAndLatestWins(t *testing.T) {
	early := time.Unix(1700000000, 0).UTC()
	late := time.Unix(1700000600, 0).UTC()

	existing := []model.Alert{
		{Fingerprint: "b", Status: model.AlertStatusFiring, StartsAt: late},
		{Fingerprint: "a", Status: model.AlertStatusFiring, StartsAt: early},
	}
	incoming := []model.Alert{
		{Fingerprint: "a", Status: model.AlertStatusResolved, StartsAt: early},
		{Fingerprint: "c", Status: model.AlertStatusFiring, StartsAt: early},
	}

	merged := alertgroup.MergeAlerts(existing, incoming)
	if len(merged) != 3 {
		t.Fatalf("the incident holds %d alerts, want 3", len(merged))
	}
	if merged[0].Fingerprint != "a" || merged[1].Fingerprint != "c" || merged[2].Fingerprint != "b" {
		t.Fatalf("the order is %s, %s, %s", merged[0].Fingerprint,
			merged[1].Fingerprint, merged[2].Fingerprint)
	}
	if merged[0].Status != model.AlertStatusResolved {
		t.Errorf("the newer state of a lost: %s", merged[0].Status)
	}

	// Same inputs in another order, same answer - byte for byte.
	shuffled := alertgroup.MergeAlerts(
		[]model.Alert{existing[1], existing[0]},
		[]model.Alert{incoming[1], incoming[0]})
	if !alertgroup.SameAlerts(merged, shuffled) {
		t.Fatal("the same payload in another order produced a different incident")
	}
}

// TestSameAlertsSeesWhatAMessageWouldShow. It is what tells a repeat from news,
// so it has to notice a status change and not notice a reordering.
func TestSameAlertsSeesWhatAMessageWouldShow(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	one := []model.Alert{{Fingerprint: "a", Status: model.AlertStatusFiring, StartsAt: at}}

	if !alertgroup.SameAlerts(one, []model.Alert{
		{Fingerprint: "a", Status: model.AlertStatusFiring, StartsAt: at}}) {
		t.Error("two identical sets were called different")
	}
	if alertgroup.SameAlerts(one, []model.Alert{
		{Fingerprint: "a", Status: model.AlertStatusResolved, StartsAt: at}}) {
		t.Error("an alert clearing went unnoticed")
	}
	if alertgroup.SameAlerts(one, nil) {
		t.Error("an empty set matched a set with an alert in it")
	}
}

// TestMergeTimelineEventsSaysWhatChanged, and says nothing when nothing did.
func TestMergeTimelineEventsSaysWhatChanged(t *testing.T) {
	base := time.Unix(1700000000, 0).UTC()
	held := map[string]model.AlertStatus{
		"known-firing":   model.AlertStatusFiring,
		"known-resolved": model.AlertStatusResolved,
	}

	events := alertgroup.MergeTimelineEvents("ag-1", []model.Alert{
		{Fingerprint: "newcomer", Status: model.AlertStatusFiring,
			Labels: map[string]string{"alertname": "New"}},
		{Fingerprint: "known-firing", Status: model.AlertStatusResolved,
			Labels: map[string]string{"alertname": "Cleared"}},
		{Fingerprint: "known-resolved", Status: model.AlertStatusFiring,
			Labels: map[string]string{"alertname": "Again"}},
	}, held, base)

	if len(events) != 3 {
		t.Fatalf("the history gained %d lines, want 3", len(events))
	}
	want := []model.TimelineEventType{
		model.TimelineEventAlertAdded,
		model.TimelineEventAlertResolved,
		model.TimelineEventAlertAdded,
	}
	for i, kind := range want {
		if events[i].Type != kind {
			t.Errorf("line %d is %s, want %s", i, events[i].Type, kind)
		}
	}
	// Written in one transaction, so the order has to come from the lines
	// themselves rather than from the instant they share.
	for i := 1; i < len(events); i++ {
		if !events[i].CreatedAt.After(events[i-1].CreatedAt) {
			t.Fatalf("line %d is not after line %d", i, i-1)
		}
	}

	if got := alertgroup.MergeTimelineEvents("ag-1", []model.Alert{
		{Fingerprint: "known-firing", Status: model.AlertStatusFiring}}, held, base); got != nil {
		t.Errorf("a repeat that changed nothing wrote %d lines", len(got))
	}
}

// TestAllResolvedEndsAnIncidentOnlyWhenNothingIsFiring.
func TestAllResolvedEndsAnIncidentOnlyWhenNothingIsFiring(t *testing.T) {
	if !alertgroup.AllResolved(nil) {
		t.Error("an incident holding nothing is not over")
	}
	if alertgroup.AllResolved([]model.Alert{
		{Status: model.AlertStatusResolved}, {Status: model.AlertStatusFiring}}) {
		t.Error("an incident with something still firing was called over")
	}
	if !alertgroup.AllResolved([]model.Alert{{Status: model.AlertStatusResolved}}) {
		t.Error("an incident whose only alert cleared is not over")
	}
}
