package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The mock store answers the engine's tests, so where it and PostgreSQL
// disagree the engine is being tested against a contract that does not exist.
// Admission is where that matters most: the order its questions are asked in is
// the difference between a lost reply being answered and a lost reply becoming
// a refusal over commitments that are already being delivered.
//
// These run the same scenarios against both.

// admitting is the part of a store an admission scenario needs.
type admitting interface {
	CreateAlertGroup(ag *model.AlertGroup) error
	TransitionAlertGroupStatus(id string, from, to model.AlertGroupStatus) (bool, error)
	UpdateAlertGroupAlertsAndRaiseSlackUpdate(id string, alerts []model.Alert) error
	SubmitEscalationBatch(ctx context.Context,
		adm outbound.EscalationAdmission) (outbound.SubmitResult, error)
}

func TestBothStoresAdmitTheSameWay(t *testing.T) {
	scenarios := []struct {
		name string
		// run returns what the scenario got out of the store, in words the two
		// implementations can be compared in.
		run func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome
	}{
		{
			name: "a repeat after the user acknowledged is still the same work",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
				first := submitTo(t, s, adm)
				acknowledge(t, s, agID)
				repeat := submitTo(t, s, adm)
				sameClaim(t, first, repeat)
				return []outbound.SubmitOutcome{first.Outcome, repeat.Outcome}
			},
		},
		{
			name: "a repeat after an alert joined is still the same work",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
				first := submitTo(t, s, adm)
				if err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(agID, []model.Alert{{
					Fingerprint: "fp-late", Status: "firing", StartsAt: time.Now(),
				}}); err != nil {
					t.Fatalf("change the alert group: %v", err)
				}
				repeat := submitTo(t, s, adm)
				sameClaim(t, first, repeat)
				return []outbound.SubmitOutcome{first.Outcome, repeat.Outcome}
			},
		},
		{
			name: "a plan built before an alert joined is refused",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
				if err := s.UpdateAlertGroupAlertsAndRaiseSlackUpdate(agID, []model.Alert{{
					Fingerprint: "fp-late", Status: "firing", StartsAt: time.Now(),
				}}); err != nil {
					t.Fatalf("change the alert group: %v", err)
				}
				stale := submitTo(t, s, adm)

				// And replanned against what is there now, it is admitted.
				adm.SourceVersion = 1
				return []outbound.SubmitOutcome{stale.Outcome, submitTo(t, s, adm).Outcome}
			},
		},
		{
			name: "a fresh plan for a group the user finished with is not admitted",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				acknowledge(t, s, agID)
				adm := outboundAdmission(t, agID, "first", channelCommitment("C0001", 0))
				return []outbound.SubmitOutcome{submitTo(t, s, adm).Outcome}
			},
		},
		{
			name: "different work under one claim is a conflict",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				first := submitTo(t, s, outboundAdmission(t, agID, "first",
					channelCommitment("C0001", 0)))
				second := submitTo(t, s, outboundAdmission(t, agID, "second",
					channelCommitment("C0009", 0)))
				if second.BatchID != first.BatchID {
					t.Errorf("the conflict names claim %q, the work is under %q",
						second.BatchID, first.BatchID)
				}
				return []outbound.SubmitOutcome{first.Outcome, second.Outcome}
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			mock := NewMockStore()
			fromMock := scenario.run(t, mock, conformanceGroup(t, mock))

			// Skips without a database, which is what makes this file runnable
			// in the unit suite: the mock half still gets exercised.
			s := setupTestDB(t)
			fromPostgres := scenario.run(t, s, conformanceGroup(t, s))

			if len(fromMock) != len(fromPostgres) {
				t.Fatalf("the two stores answered %v and %v", fromMock, fromPostgres)
			}
			for i := range fromMock {
				if fromMock[i] != fromPostgres[i] {
					t.Errorf("answer %d: the mock said %q, PostgreSQL said %q",
						i+1, fromMock[i], fromPostgres[i])
				}
			}
		})
	}
}

func conformanceGroup(t *testing.T, s admitting) string {
	t.Helper()
	id := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: id, AlertKey: "conformance-" + id, Status: model.AlertGroupStatusProcessing,
		Title: "conformance fixture", Severity: "critical",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}
	return id
}

func submitTo(t *testing.T, s admitting, adm outbound.EscalationAdmission) outbound.SubmitResult {
	t.Helper()
	result, err := s.SubmitEscalationBatch(context.Background(), adm)
	if err != nil {
		t.Fatalf("submit the admission: %v", err)
	}
	return result
}

func acknowledge(t *testing.T, s admitting, agID string) {
	t.Helper()
	moved, err := s.TransitionAlertGroupStatus(agID,
		model.AlertGroupStatusProcessing, model.AlertGroupStatusAcknowledged)
	if err != nil {
		t.Fatalf("acknowledge the group: %v", err)
	}
	if !moved {
		t.Fatal("the group did not move to acknowledged")
	}
}

// sameClaim is what a repeat has to answer with: the work that was accepted,
// named by the same claim and the same commitments.
func sameClaim(t *testing.T, first, repeat outbound.SubmitResult) {
	t.Helper()
	if repeat.BatchID != first.BatchID {
		t.Errorf("the repeat found claim %q, the work is under %q", repeat.BatchID, first.BatchID)
	}
	if len(repeat.IntentIDs) != len(first.IntentIDs) {
		t.Fatalf("the repeat named %d commitments, the claim holds %d",
			len(repeat.IntentIDs), len(first.IntentIDs))
	}
	for i := range first.IntentIDs {
		if repeat.IntentIDs[i] != first.IntentIDs[i] {
			t.Errorf("the repeat named commitment %s, the claim holds %s",
				repeat.IntentIDs[i], first.IntentIDs[i])
		}
	}
}
