package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// The mock and the database have to answer a payload the same way.
//
// Tests all over the tree drive the ingester through the mock, and every one of
// them is reading its answers as though they were the store's. Where the two
// disagree, those tests prove something that does not happen in production -
// which is worse than not testing it, because it reads as coverage.
//
// What is compared here is the OUTCOME, because that is what the ingester
// branches on. What the mock deliberately does not model - the withdrawal of
// what an incident still owes, and the revision its messages are brought to -
// is stated on the mock itself: no test may assert those against it.

type applying interface {
	CreateAlertGroup(ag *model.AlertGroup) error
	ApplyAlertmanagerUpdateAtomic(ctx context.Context, alertKey string,
		incoming []model.Alert, actor string) (alertgroup.MergeResult, error)
}

func TestTheMockAndTheDatabaseAnswerAPayloadAlike(t *testing.T) {
	firing := func(fingerprint string) model.Alert {
		return model.Alert{
			Fingerprint: fingerprint, Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000000, 0),
			Labels:   map[string]string{"alertname": fingerprint},
		}
	}
	resolved := func(fingerprint string) model.Alert {
		a := firing(fingerprint)
		a.Status = model.AlertStatusResolved
		return a
	}
	described := func(fingerprint, description string) model.Alert {
		a := firing(fingerprint)
		a.Annotations = map[string]string{"description": description}
		return a
	}

	cases := []struct {
		name     string
		held     []model.Alert
		status   model.AlertGroupStatus
		incoming []model.Alert
		want     alertgroup.MergeOutcome
	}{
		{
			name: "a new alert joins", held: []model.Alert{firing("fp-0")},
			status:   model.AlertGroupStatusProcessing,
			incoming: []model.Alert{firing("fp-1")}, want: alertgroup.MergeMerged,
		},
		{
			name: "the same payload again", held: []model.Alert{firing("fp-0")},
			status:   model.AlertGroupStatusProcessing,
			incoming: []model.Alert{firing("fp-0")}, want: alertgroup.MergeUnchanged,
		},
		{
			name: "a resolution for an alert nobody here has seen",
			held: []model.Alert{firing("fp-0")}, status: model.AlertGroupStatusProcessing,
			incoming: []model.Alert{resolved("stranger")}, want: alertgroup.MergeIgnored,
		},
		{
			name: "the last alert clears", held: []model.Alert{firing("fp-0")},
			status:   model.AlertGroupStatusProcessing,
			incoming: []model.Alert{resolved("fp-0")}, want: alertgroup.MergeResolved,
		},
		{
			name: "the incident is already over", held: []model.Alert{firing("fp-0")},
			status:   model.AlertGroupStatusResolved,
			incoming: []model.Alert{firing("fp-1")}, want: alertgroup.MergeNoActive,
		},
		{
			// A description is not a fingerprint and not a status, and it is on
			// the card. A repeat that changes one is news.
			name: "an alert says something new about itself",
			held: []model.Alert{firing("fp-0")}, status: model.AlertGroupStatusProcessing,
			incoming: []model.Alert{described("fp-0", "12% left")}, want: alertgroup.MergeMerged,
		},
		{
			// And one that changes a field no message shows is still news to
			// the incident: whether it reaches a message is decided by the
			// digest of the render snapshot, not here.
			name: "an alert changes something nobody renders",
			held: []model.Alert{firing("fp-0")}, status: model.AlertGroupStatusProcessing,
			incoming: []model.Alert{func() model.Alert {
				a := firing("fp-0")
				a.GeneratorURL = "https://prometheus.example/graph?g0.expr=up"
				return a
			}()}, want: alertgroup.MergeMerged,
		},
	}

	run := func(t *testing.T, s applying, tc int) alertgroup.MergeOutcome {
		t.Helper()
		c := cases[tc]
		id := uuid.New().String()
		key := "conformance-merge-" + id
		if err := s.CreateAlertGroup(&model.AlertGroup{
			ID: id, AlertKey: key, Status: c.status,
			Title: "conformance fixture", Severity: "critical", TeamID: "team-1",
			TeamNameSnapshot: "team-1", Alerts: c.held,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("create the incident: %v", err)
		}
		result, err := s.ApplyAlertmanagerUpdateAtomic(
			context.Background(), key, c.incoming, "system")
		if err != nil {
			t.Fatalf("apply the payload: %v", err)
		}
		return result.Outcome
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := setupTestDB(t)
			store.SetRenderEnvironment("https://tokay.example", "UTC")

			fromStore := run(t, store, i)
			fromMock := run(t, NewMockStore(), i)

			if fromStore != c.want {
				t.Errorf("the database answered %s, want %s", fromStore, c.want)
			}
			if fromMock != fromStore {
				t.Errorf("the mock answered %s and the database %s", fromMock, fromStore)
			}
		})
	}
}
