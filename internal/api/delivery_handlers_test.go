package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/store"
)

// The delivery routes keep their form over the new source. What the store
// decides - the projection, the replay's admission - is tested against
// Postgres; what the routes do with the answer is tested here, over a double
// that answers what it is told.

type deliveryStoreFake struct {
	store.StoreInterface
	deliveries []*model.OutboxDelivery
	attempts   map[string][]*model.DeliveryAttempt
	replay     func(store.WebhookReplayRequest) (store.WebhookReplayResult, error)
	replayed   []store.WebhookReplayRequest
}

func (f *deliveryStoreFake) ListWebhookDeliveries(_ context.Context, integrationID string, limit, offset int) ([]*model.OutboxDelivery, int, error) {
	var mine []*model.OutboxDelivery
	for _, d := range f.deliveries {
		if d.IntegrationID == integrationID {
			mine = append(mine, d)
		}
	}
	total := len(mine)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return mine[offset:end], total, nil
}

func (f *deliveryStoreFake) WebhookDelivery(_ context.Context, integrationID, deliveryID string) (*model.OutboxDelivery, []*model.DeliveryAttempt, error) {
	for _, d := range f.deliveries {
		if d.ID == deliveryID && d.IntegrationID == integrationID {
			return d, f.attempts[deliveryID], nil
		}
	}
	return nil, nil, store.ErrWebhookDeliveryNotFound
}

func (f *deliveryStoreFake) ReplayWebhookDelivery(_ context.Context, req store.WebhookReplayRequest) (store.WebhookReplayResult, error) {
	f.replayed = append(f.replayed, req)
	return f.replay(req)
}

type deliveryRoutes struct {
	fake *deliveryStoreFake
	e    *echo.Echo
	hook string
}

func setupDeliveryRoutes(t *testing.T) *deliveryRoutes {
	t.Helper()
	s := store.NewMockStore()
	scope := model.WebhookScopeGlobal
	for _, name := range []string{"hooks", "other"} {
		if err := s.CreateIntegration(&model.Integration{
			ID: name, Type: model.IntegrationTypeGenericWebhook, Name: name, Enabled: true, Scope: &scope,
			Config: json.RawMessage(`{"url":"https://example.com/` + name + `","secret":"s"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	fake := &deliveryStoreFake{StoreInterface: s, attempts: map[string][]*model.DeliveryAttempt{}}
	a := NewAPI(fake, nil, nil, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)
	return &deliveryRoutes{fake: fake, e: e, hook: "hooks"}
}

func (r *deliveryRoutes) call(method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	addAuth(req, "denis")
	rec := httptest.NewRecorder()
	r.e.ServeHTTP(rec, req)
	return rec
}

func ptr[T any](v T) *T { return &v }

func TestTheDeliveryListKeepsItsForm(t *testing.T) {
	r := setupDeliveryRoutes(t)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	r.fake.deliveries = []*model.OutboxDelivery{
		{ID: "d-3", EventID: "evt-3", IntegrationID: "hooks", Status: model.OutboxDeliveryRetry, Attempts: 1,
			NextAttemptAt: ptr(at.Add(time.Minute)), LastHTTPStatus: ptr(500), LastError: ptr("ambiguous"),
			RequestPayload: ptr(`{"event":"alert_group.firing"}`), CreatedAt: at.Add(2 * time.Second)},
		{ID: "d-2", EventID: "evt-2", IntegrationID: "hooks", Status: model.OutboxDeliveryFailed, Attempts: 1,
			LastHTTPStatus: ptr(404), LastError: ptr("permanent_rejection"), CreatedAt: at.Add(time.Second)},
		{ID: "d-1", EventID: "evt-1", IntegrationID: "hooks", Status: model.OutboxDeliverySent, Attempts: 1,
			LastHTTPStatus: ptr(200), ResponseBodyTrunc: ptr("HTTP 200: ok"), CreatedAt: at, SentAt: ptr(at.Add(time.Second))},
		{ID: "d-x", EventID: "evt-1", IntegrationID: "other", Status: model.OutboxDeliverySent, CreatedAt: at},
	}

	rec := r.call(http.MethodGet, "/api/v1/integrations/hooks/deliveries?page=1&limit=2", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp DeliveryListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 3 || len(resp.Deliveries) != 2 || !resp.HasNext || resp.HasPrev || resp.TotalPages != 2 {
		t.Fatalf("page 1 of 2: %+v", resp)
	}
	if resp.Deliveries[0].ID != "d-3" || resp.Deliveries[1].ID != "d-2" {
		t.Fatalf("the other integration's delivery leaked, or the order changed: %+v", resp.Deliveries)
	}
	// The wire names, as the UI reads them.
	body := rec.Body.String()
	for _, field := range []string{`"status":"retry"`, `"attempts":1`, `"next_attempt_at":"2026-09-01T10:01:00Z"`,
		`"last_http_status":500`, `"last_error":"ambiguous"`, `"request_payload":"{\"event\":\"alert_group.firing\"}"`,
		`"event_id":"evt-3"`, `"integration_id":"hooks"`} {
		if !strings.Contains(body, field) {
			t.Errorf("the list does not carry %s: %s", field, body)
		}
	}
	rec = r.call(http.MethodGet, "/api/v1/integrations/hooks/deliveries?page=2&limit=2", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Deliveries) != 1 || resp.Deliveries[0].ID != "d-1" || resp.HasNext || !resp.HasPrev {
		t.Fatalf("page 2 of 2: %+v", resp)
	}
	if !strings.Contains(rec.Body.String(), `"sent_at":"2026-09-01T10:00:01Z"`) ||
		strings.Contains(rec.Body.String(), `"next_attempt_at"`) {
		t.Fatalf("a sent delivery carries sent_at and no next attempt: %s", rec.Body.String())
	}
}

func TestTheDeliveryDetailKeepsItsForm(t *testing.T) {
	r := setupDeliveryRoutes(t)
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	r.fake.deliveries = []*model.OutboxDelivery{
		{ID: "d-1", EventID: "evt-1", IntegrationID: "hooks", Status: model.OutboxDeliverySent, Attempts: 2, CreatedAt: at},
	}
	r.fake.attempts["d-1"] = []*model.DeliveryAttempt{
		{ID: "a-1", DeliveryID: "d-1", Attempt: 0, HTTPStatus: ptr(500), Error: ptr("ambiguous"), CreatedAt: at},
		{ID: "a-2", DeliveryID: "d-1", Attempt: 1, HTTPStatus: ptr(200), ResponseBodyTrunc: ptr("HTTP 200: ok"), CreatedAt: at.Add(time.Minute)},
	}

	rec := r.call(http.MethodGet, "/api/v1/integrations/hooks/deliveries/d-1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp DeliveryDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Delivery == nil || resp.Delivery.ID != "d-1" || len(resp.Attempts) != 2 {
		t.Fatalf("detail: %+v", resp)
	}
	if resp.Attempts[0].Attempt != 0 || *resp.Attempts[0].HTTPStatus != 500 || resp.Attempts[1].Attempt != 1 ||
		*resp.Attempts[1].HTTPStatus != 200 || resp.Attempts[1].Error != nil {
		t.Fatalf("attempts: %+v %+v", resp.Attempts[0], resp.Attempts[1])
	}
	if !strings.Contains(rec.Body.String(), `"http_status":500`) || !strings.Contains(rec.Body.String(), `"attempt":0`) {
		t.Fatalf("the attempt form changed: %s", rec.Body.String())
	}

	// Another integration's id, or no such delivery: not found, whoever asks.
	if rec := r.call(http.MethodGet, "/api/v1/integrations/other/deliveries/d-1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a delivery reached through another integration answered %d", rec.Code)
	}
	if rec := r.call(http.MethodGet, "/api/v1/integrations/hooks/deliveries/nobody", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown delivery answered %d", rec.Code)
	}
}

func TestAReplayNeedsAKeyAndAnswersWithTheNewDelivery(t *testing.T) {
	r := setupDeliveryRoutes(t)
	r.fake.replay = func(req store.WebhookReplayRequest) (store.WebhookReplayResult, error) {
		return store.WebhookReplayResult{Outcome: outbound.SubmitCreated, DeliveryID: "new-" + req.ClientRequestID}, nil
	}
	path := "/api/v1/integrations/hooks/deliveries/d-1/replay"

	// No key, or too long a key: refused before the store is asked.
	if rec := r.call(http.MethodPost, path, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("a replay without a key answered %d", rec.Code)
	}
	if rec := r.call(http.MethodPost, path, strings.Repeat("k", 129)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a replay with a 129-byte key answered %d", rec.Code)
	}
	if len(r.fake.replayed) != 0 {
		t.Fatalf("the store was asked %d times for refused requests", len(r.fake.replayed))
	}

	// The key and the person reach the store; the answer names the new delivery.
	key := strings.Repeat("k", 128)
	rec := r.call(http.MethodPost, path, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp ReplayDeliveryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || resp.DeliveryID != "new-"+key {
		t.Fatalf("response %+v", resp)
	}
	if !strings.Contains(rec.Body.String(), `"delivery_id":"new-`) {
		t.Fatalf("the wire name of the new delivery: %s", rec.Body.String())
	}
	got := r.fake.replayed[0]
	if got.IntegrationID != "hooks" || got.DeliveryID != "d-1" || got.ClientRequestID != key || got.Actor != "denis" {
		t.Fatalf("the store was asked %+v", got)
	}

	// A repeat found already admitted is the same answer.
	r.fake.replay = func(req store.WebhookReplayRequest) (store.WebhookReplayResult, error) {
		return store.WebhookReplayResult{Outcome: outbound.SubmitExisting, DeliveryID: "new-" + req.ClientRequestID}, nil
	}
	rec = r.call(http.MethodPost, path, key)
	var again ReplayDeliveryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again != resp {
		t.Fatalf("the repeat answered %d %+v, the first %+v", rec.Code, again, resp)
	}

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"no such delivery", store.ErrWebhookDeliveryNotFound, http.StatusNotFound},
		{"still in progress", store.ErrWebhookDeliveryNotTerminal, http.StatusConflict},
		{"the subscriber is being changed", store.ErrIntegrationBusy, http.StatusConflict},
		{"anything else", errors.New("the database is unwell"), http.StatusInternalServerError},
	} {
		r.fake.replay = func(store.WebhookReplayRequest) (store.WebhookReplayResult, error) {
			return store.WebhookReplayResult{}, tc.err
		}
		if rec := r.call(http.MethodPost, path, "k"); rec.Code != tc.want {
			t.Errorf("%s: answered %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
