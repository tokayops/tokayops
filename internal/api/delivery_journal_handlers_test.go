package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/store"
)

// The journal routes over a store that answers what it is told: what the
// routes refuse, how they read the query string, what they make of the answer,
// and who may ask. The reads themselves are tested against Postgres.

type journalStoreFake struct {
	store.StoreInterface
	filters []store.IntentFilter
	pages   [][2]int
	intents []outbound.Intent
	total   int
	journal *outbound.Journal
	group   *outbound.GroupDeliveries
}

func (f *journalStoreFake) ListIntents(_ context.Context, filter store.IntentFilter, limit, offset int) ([]outbound.Intent, int, error) {
	f.filters = append(f.filters, filter)
	f.pages = append(f.pages, [2]int{limit, offset})
	return f.intents, f.total, nil
}

func (f *journalStoreFake) IntentJournal(_ context.Context, id string) (*outbound.Journal, error) {
	if f.journal != nil && f.journal.Intent.ID == id {
		return f.journal, nil
	}
	return nil, nil
}

func (f *journalStoreFake) AlertGroupDeliveries(_ context.Context, _ string) (*outbound.GroupDeliveries, error) {
	return f.group, nil
}

type journalRoutes struct {
	fake *journalStoreFake
	e    *echo.Echo
}

// setupJournalRoutes wires the routes over the mock store's people and one
// alert group of team-a: denis is the mock's administrator, bob a member of
// team-a, and neither of them is more than that.
func setupJournalRoutes(t *testing.T) *journalRoutes {
	t.Helper()
	s := store.NewMockStore()
	if err := s.CreateTeam(&model.Team{ID: "team-a", Name: "Team A"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(&model.User{ID: "bob", Email: "bob@test.com", Role: model.UserRoleUser}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTeamMember("team-a", "bob", model.TeamMemberRoleMember); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag-a", AlertKey: "k-a", TeamID: "team-a", Severity: "critical",
		Status: model.AlertGroupStatusTriggered,
	}); err != nil {
		t.Fatal(err)
	}
	fake := &journalStoreFake{StoreInterface: s, group: &outbound.GroupDeliveries{
		Paging: []outbound.Intent{}, Events: []outbound.EventDeliveries{},
	}}
	a := NewAPI(fake, nil, nil, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)
	return &journalRoutes{fake: fake, e: e}
}

func (r *journalRoutes) get(path, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	addAuth(req, user)
	rec := httptest.NewRecorder()
	r.e.ServeHTTP(rec, req)
	return rec
}

func TestTheJournalRefusesWhatItDoesNotKnow(t *testing.T) {
	r := setupJournalRoutes(t)
	for _, query := range []string{
		"family=pigeon",
		"status=pending,bogus",
		"target_kind=robot",
		"from=yesterday",
		"to=2026-09-01T00:00:00Z&from=2026-09-02T00:00:00Z",
	} {
		rec := r.get("/api/v1/deliveries?"+query, "denis")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", query, rec.Code, rec.Body.String())
		}
	}
	if len(r.fake.filters) != 0 {
		t.Fatalf("the store was asked %d times about queries that were refused", len(r.fake.filters))
	}
}

func TestTheJournalReadsThePeriodAndTheFilters(t *testing.T) {
	r := setupJournalRoutes(t)
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	// Nothing given: the store applies the day by its own clock.
	if rec := r.get("/api/v1/deliveries", "denis"); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if f := r.fake.filters[0]; f.From != nil || f.To != nil {
		t.Errorf("no period given, the store was handed %v..%v", f.From, f.To)
	}
	if p := r.fake.pages[0]; p != [2]int{50, 0} {
		t.Errorf("the default page is %v, want limit 50 offset 0", p)
	}

	// Only an end: the day before it.
	r.get("/api/v1/deliveries?to="+at.Format(time.RFC3339), "denis")
	if f := r.fake.filters[1]; f.To == nil || !f.To.Equal(at) || f.From == nil || !f.From.Equal(at.Add(-24*time.Hour)) {
		t.Errorf("with only an end the window is %v..%v", f.From, f.To)
	}

	// Every filter, and the page clamped to what the route allows.
	r.get("/api/v1/deliveries?family=webhook&provider=webhook&status=pending,%20succeeded&target_kind=subscriber"+
		"&target_ref=int-a&alert_group_id=ag-a&event_id=evt-1&from="+at.Format(time.RFC3339)+
		"&page=3&limit=500", "denis")
	f := r.fake.filters[2]
	if f.Family != "webhook" || f.Provider != "webhook" || f.TargetKind != keys.TargetSubscriber ||
		f.TargetRef != "int-a" || f.AlertGroupID != "ag-a" || f.EventID != "evt-1" ||
		f.From == nil || !f.From.Equal(at) || f.To != nil {
		t.Errorf("the filters were read as %+v", f)
	}
	if len(f.Statuses) != 2 || f.Statuses[0] != outbound.StatusPending || f.Statuses[1] != outbound.StatusSucceeded {
		t.Errorf("the statuses were read as %v", f.Statuses)
	}
	if p := r.fake.pages[2]; p != [2]int{200, 400} {
		t.Errorf("page 3 of 500 reads as %v, want limit 200 offset 400", p)
	}
}

func TestTheJournalOfOneDelivery(t *testing.T) {
	r := setupJournalRoutes(t)
	if rec := r.get("/api/v1/deliveries/nothing", "denis"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown delivery answered %d", rec.Code)
	}

	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	revision := int64(3)
	r.fake.journal = &outbound.Journal{
		Intent: outbound.Intent{
			ID: "d-1", AlertGroupID: "ag-a", Family: outbound.FamilyNotification, KeyKind: keys.KindEscalation,
			Provider: "slack", TargetKind: outbound.TargetKind(keys.TargetUser), TargetRef: "U1",
			Form: outbound.FormEditable, Status: outbound.StatusIdle, GenerationNo: 1,
			AttemptsInGeneration: 2, DesiredRevision: 3, AppliedRevision: &revision, HasReceipt: true,
			ReceiptRef: "C1/1700000000.000100", NotBefore: at, NextAttemptAt: at, CreatedAt: at, UpdatedAt: at,
		},
		Attempts: []outbound.AttemptRecord{{
			ID: "a-1", AttemptNo: 1, RecordKind: outbound.RecordAttempt, GenerationNo: 1,
			AttemptKind: outbound.AttemptCreate, Operation: outbound.OperationSend, Provider: "slack",
			BoundEndpoint: "D-bob", StartedAt: &at, Outcome: outbound.OutcomeAccepted, ReceiptRecorded: true,
			Receipt: json.RawMessage(`{"channel":"C1","ts":"1700000000.000100"}`), Summary: "accepted",
		}},
		Events: []outbound.IntentEvent{{Seq: 1, Kind: "created", Actor: "engine", At: at}},
	}
	rec := r.get("/api/v1/deliveries/d-1", "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp DeliveryJournalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Delivery.ID != "d-1" || resp.Delivery.Status != "idle" || resp.Delivery.Kind != "escalation" ||
		resp.Delivery.TargetKind != "user" || !resp.Delivery.ReceiptRecorded || resp.Delivery.ReceiptRef == "" ||
		resp.Delivery.AppliedRevision == nil || *resp.Delivery.AppliedRevision != 3 {
		t.Errorf("the delivery reads %+v", resp.Delivery)
	}
	if len(resp.Attempts) != 1 || resp.Attempts[0].Outcome != "accepted" || resp.Attempts[0].BoundEndpoint != "D-bob" ||
		!resp.Attempts[0].ReceiptRecorded || len(resp.Attempts[0].Receipt) == 0 {
		t.Errorf("the attempts read %+v", resp.Attempts)
	}
	if len(resp.Events) != 1 || resp.Events[0].Kind != "created" || !resp.Events[0].At.Equal(at) {
		t.Errorf("the events read %+v", resp.Events)
	}
	if resp.Observations == nil {
		t.Error("observations are absent rather than empty")
	}
}

func TestTheGroupsDeliveriesRouteShapesBothHalves(t *testing.T) {
	r := setupJournalRoutes(t)
	at := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	r.fake.group = &outbound.GroupDeliveries{
		Paging: []outbound.Intent{{ID: "p-1", AlertGroupID: "ag-a", Family: outbound.FamilyNotification,
			Provider: "slack", Status: outbound.StatusSucceeded, CreatedAt: at, UpdatedAt: at, NotBefore: at, NextAttemptAt: at}},
		Events: []outbound.EventDeliveries{
			{EventID: "evt-1", EventType: "alert_group.firing", Status: "fanned_out", CreatedAt: at,
				Batches: []outbound.BatchDeliveries{
					{BatchID: "b-1", Kind: keys.KindWebhookEvent, Outcome: "admitted", IntentCount: 1, AdmittedAt: at,
						Deliveries: []outbound.Intent{{ID: "w-1", Family: outbound.FamilyWebhook, Provider: "webhook",
							TargetKind: outbound.TargetKind(keys.TargetSubscriber), TargetRef: "int-a", Status: outbound.StatusPending,
							CreatedAt: at, UpdatedAt: at, NotBefore: at, NextAttemptAt: at}}},
					{BatchID: "b-2", Kind: keys.KindWebhookReplay, Outcome: "admitted", IntentCount: 1, AdmittedAt: at,
						Deliveries: []outbound.Intent{{ID: "w-2", Family: outbound.FamilyWebhook, Provider: "webhook",
							TargetRef: "int-a", Status: outbound.StatusSucceeded, CreatedAt: at, UpdatedAt: at, NotBefore: at, NextAttemptAt: at}}},
				}},
			{EventID: "evt-2", EventType: "alert_group.resolved", Status: "pending", CreatedAt: at, Batches: []outbound.BatchDeliveries{}},
		},
	}
	rec := r.get("/api/v1/alert-groups/ag-a/deliveries", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp AlertGroupDeliveriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Paging) != 1 || resp.Paging[0].ID != "p-1" {
		t.Errorf("the paging reads %+v", resp.Paging)
	}
	if len(resp.Events) != 2 || len(resp.Events[0].Batches) != 2 || resp.Events[0].Batches[1].Kind != "webhook_replay" ||
		resp.Events[0].Batches[1].Deliveries[0].ID != "w-2" {
		t.Errorf("the events read %+v", resp.Events)
	}
	if resp.Events[1].Batches == nil || len(resp.Events[1].Batches) != 0 {
		t.Errorf("an event nobody reached reads %+v", resp.Events[1])
	}
	if rec := r.get("/api/v1/alert-groups/nope/deliveries", "bob"); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown group answered %d", rec.Code)
	}
}

// TestTheJournalIsTheAdministratorsAndTheGroupsIsItsReaders: the two journal
// routes are the administrator's; a member of the group's team reads the
// group's deliveries the way they read its timeline.
func TestTheJournalIsTheAdministratorsAndTheGroupsIsItsReaders(t *testing.T) {
	r := setupJournalRoutes(t)
	r.fake.journal = &outbound.Journal{Intent: outbound.Intent{ID: "d-1"}}
	for _, tt := range []struct {
		path string
		user string
		want int
	}{
		{"/api/v1/deliveries", "denis", http.StatusOK},
		{"/api/v1/deliveries", "bob", http.StatusForbidden},
		{"/api/v1/deliveries/d-1", "denis", http.StatusOK},
		{"/api/v1/deliveries/d-1", "bob", http.StatusForbidden},
		{"/api/v1/alert-groups/ag-a/deliveries", "denis", http.StatusOK},
		{"/api/v1/alert-groups/ag-a/deliveries", "bob", http.StatusOK},
	} {
		if rec := r.get(tt.path, tt.user); rec.Code != tt.want {
			t.Errorf("%s as %s: status %d, want %d: %s", tt.path, tt.user, rec.Code, tt.want, rec.Body.String())
		}
	}
}
