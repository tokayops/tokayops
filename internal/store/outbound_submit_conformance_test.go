package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
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
	// eraseUser is how each store is told a recipient is gone. The two do it
	// differently - one writes deleted_at, the other keeps a set - so the
	// scenario asks the store rather than the database.
	eraseUser(t *testing.T, userID string)
	AckAlertGroupAtomic(id string, actor alertgroup.Actor, meta map[string]string,
		outboxEvent *model.OutboxEvent) (bool, error)
	ApplyAlertmanagerUpdateAtomic(ctx context.Context, alertKey string,
		incoming []model.Alert, actor string) (alertgroup.MergeResult, error)
	SubmitBatch(ctx context.Context, batch outbound.Batch) (outbound.SubmitResult, error)
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
				if err := alertJoins(s, agID); err != nil {
					t.Fatalf("an alert joining the group: %v", err)
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
				if err := alertJoins(s, agID); err != nil {
					t.Fatalf("an alert joining the group: %v", err)
				}
				stale := submitTo(t, s, adm)

				// And replanned against what is there now, it is admitted.
				adm = withEscalation(adm, func(about *outbound.EscalationContext) { about.SourceVersion = 1 })
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
			name: "an admission promising an erased person is refused",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				s.eraseUser(t, "erased-conformance")
				adm := outboundAdmission(t, agID, "first",
					dmCommitment("erased-conformance"))
				return []outbound.SubmitOutcome{submitTo(t, s, adm).Outcome}
			},
		},
		{
			// And the claim still answers first. An admission accepted before
			// the erasure is work that exists and is being delivered; telling
			// its producer "that person is gone" would turn a lost reply into
			// a rejected plan.
			name: "a repeat of an admission accepted before the erasure",
			run: func(t *testing.T, s admitting, agID string) []outbound.SubmitOutcome {
				adm := outboundAdmission(t, agID, "first",
					dmCommitment("erased-after-admission"))
				first := submitTo(t, s, adm)
				s.eraseUser(t, "erased-after-admission")
				repeat := submitTo(t, s, adm)
				sameClaim(t, first, repeat)
				return []outbound.SubmitOutcome{first.Outcome, repeat.Outcome}
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

// alertJoins is a new alert arriving for the group, through the door a payload
// actually comes in by. Both implementations answer it, which is what makes the
// conformance meaningful.
func alertJoins(s admitting, agID string) error {
	_, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), "conformance-"+agID,
		[]model.Alert{{
			Fingerprint: "fp-late", Status: model.AlertStatusFiring,
			StartsAt: time.Now(), Labels: map[string]string{"alertname": "Late"},
		}}, "system")
	return err
}

func submitTo(t *testing.T, s admitting, adm outbound.Batch) outbound.SubmitResult {
	t.Helper()
	result, err := s.SubmitBatch(context.Background(), adm)
	if err != nil {
		t.Fatalf("submit the admission: %v", err)
	}
	return result
}

func acknowledge(t *testing.T, s admitting, agID string) {
	t.Helper()
	changed, err := s.AckAlertGroupAtomic(agID, actorNamed("nina"), nil, nil)
	if err != nil {
		t.Fatalf("acknowledge the group: %v", err)
	}
	if !changed {
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

// The two stores are told about an erased user in their own way, so the
// scenarios can say "this person is gone" without knowing which store they are
// talking to.
func (m *MockStore) eraseUser(t *testing.T, userID string) {
	t.Helper()
	m.EraseUser(userID)
}

func (s *Store) eraseUser(t *testing.T, userID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO users (id, email, name, role, created_at, deleted_at)
		 VALUES ($1, $1 || '@example.com', $1, 'user', now(), now())
		 ON CONFLICT (id) DO UPDATE SET deleted_at = now()`, userID); err != nil {
		t.Fatalf("erase %s: %v", userID, err)
	}
}
