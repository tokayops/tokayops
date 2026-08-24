package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// Who owns the render source version.
//
// It is the token an admission is checked against, so what moves it decides
// which plans are refused as stale. Two rules, and both are easy to break by
// adding a writer and not thinking about it:
//
// a write that changes what a message about the alert would say moves it, and a
// write that only RECORDS WORK does not. The second is not a detail. Deliveries
// write history and the admission moves the group to "processing", so a version
// moved by either would leave nothing ever current: the update gate would never
// come down, and every snapshot would be stale the moment it was accepted.

func TestWhatMovesTheRenderSourceVersion(t *testing.T) {
	s := setupTestDB(t)

	owners := []struct {
		name string
		// moves says whether this unit of work changes what a message about
		// the alert would say.
		moves bool
		do    func(t *testing.T, agID string)
	}{
		{
			name: "an alert joins the group", moves: true,
			do: func(t *testing.T, agID string) {
				if err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(agID, []model.Alert{{
					Fingerprint: "fp-late", Status: "firing", StartsAt: time.Now(),
				}}); err != nil {
					t.Fatalf("update the alerts: %v", err)
				}
			},
		},
		{
			name: "the alerts are rewritten without raising the gate", moves: true,
			do: func(t *testing.T, agID string) {
				if err := s.UpdateAlertGroupAlerts(agID, []model.Alert{{
					Fingerprint: "fp-late", Status: "firing", StartsAt: time.Now(),
				}}); err != nil {
					t.Fatalf("update the alerts: %v", err)
				}
			},
		},
		{
			name: "a user acknowledges", moves: true,
			do: func(t *testing.T, agID string) {
				moved, err := s.AckAlertGroupAtomic(agID, "denis", nil, nil)
				if err != nil || !moved {
					t.Fatalf("acknowledge: moved=%v err=%v", moved, err)
				}
			},
		},
		{
			name: "a user resolves", moves: true,
			do: func(t *testing.T, agID string) {
				moved, err := s.ResolveAlertGroupAtomic(agID, "denis", nil, nil)
				if err != nil || !moved {
					t.Fatalf("resolve: moved=%v err=%v", moved, err)
				}
			},
		},
		{
			name: "the alerts stop firing and the group resolves", moves: true,
			do: func(t *testing.T, agID string) {
				moved, err := s.ResolveAlertGroupWithAlertsAtomic(agID, []model.Alert{{
					Fingerprint: "fp-1", Status: "resolved", StartsAt: time.Now(),
				}}, nil, nil)
				if err != nil || !moved {
					t.Fatalf("resolve with alerts: moved=%v err=%v", moved, err)
				}
			},
		},
		{
			name: "the group changes status", moves: true,
			do: func(t *testing.T, agID string) {
				moved, err := s.TransitionAlertGroupStatus(agID,
					model.AlertGroupStatusProcessing, model.AlertGroupStatusTriggered)
				if err != nil || !moved {
					t.Fatalf("transition: moved=%v err=%v", moved, err)
				}
			},
		},
		{
			name: "an escalation is admitted", moves: false,
			do: func(t *testing.T, agID string) {
				adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
				mustSubmit(t, s, adm)
			},
		},
		{
			name: "a line is added to the history", moves: false,
			do: func(t *testing.T, agID string) {
				if err := s.AddTimelineEvent(&model.TimelineEvent{
					ID: uuid.New().String(), AlertGroupID: agID,
					Type: model.TimelineEventNotificationSent, Message: "Notification sent",
					Actor: "system", CreatedAt: time.Now(),
				}); err != nil {
					t.Fatalf("record the history: %v", err)
				}
			},
		},
		{
			name: "the policy the group escalates by is recorded", moves: false,
			do: func(t *testing.T, agID string) {
				if err := s.UpdateAlertGroupPolicy(agID, "policy-1",
					&model.EscalationPolicySnapshot{PolicyID: "policy-1"}); err != nil {
					t.Fatalf("record the policy: %v", err)
				}
			},
		},
		{
			name: "who was on duty is recorded", moves: false,
			do: func(t *testing.T, agID string) {
				if err := s.UpdateAlertGroupOnCall(agID, &model.OnCallResult{}); err != nil {
					t.Fatalf("record the on-call: %v", err)
				}
			},
		},
		{
			name: "the group is touched", moves: false,
			do: func(t *testing.T, agID string) {
				if err := s.TouchAlertGroup(agID); err != nil {
					t.Fatalf("touch the group: %v", err)
				}
			},
		},
	}

	for _, owner := range owners {
		t.Run(owner.name, func(t *testing.T) {
			agID := versionedGroup(t, s)
			before := renderSourceVersion(t, s, agID)

			owner.do(t, agID)

			after := renderSourceVersion(t, s, agID)
			switch {
			case owner.moves && after == before:
				t.Fatalf("the version stayed at %d, so a plan built before this "+
					"would still be admitted against state that has changed", before)
			case !owner.moves && after != before:
				t.Fatalf("the version moved from %d to %d for a write that only "+
					"records what the system did", before, after)
			}
		})
	}
}

func versionedGroup(t *testing.T, s *Store) string {
	t.Helper()
	id := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: "versioned-" + id, Status: model.AlertGroupStatusProcessing,
		Title: "Disk filling up", Severity: "critical",
		Alerts: []model.Alert{{
			Fingerprint: "fp-1", Status: "firing", StartsAt: time.Unix(1700000000, 0),
		}},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}
	return id
}

func renderSourceVersion(t *testing.T, s *Store, agID string) int64 {
	t.Helper()
	var version int64
	if err := s.db.QueryRow(
		`SELECT render_source_version FROM alert_groups WHERE id = $1`, agID).
		Scan(&version); err != nil {
		t.Fatalf("read the version: %v", err)
	}
	return version
}
