package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	webhookprovider "github.com/tokayops/tokayops/internal/outbound/providers/webhook"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// An alert event, end to end: written to the outbox, fanned out by the domain,
// delivered by the webhook family's worker through the real channel to a real
// HTTP server, and settled in the store - which is where the third place of
// the rule "an acceptance names an object" is proven, because nowhere else can
// a webhook commitment be finalised.

type webhookEnv struct {
	s      *store.Store
	worker *outbound.Worker
	fanOut *outbound.FanOut
	e      *echo.Echo
	admin  string
}

func setupWebhookEnv(t *testing.T) *webhookEnv {
	t.Helper()
	s := testutil.SetupDB(t)
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	worker, err := outbound.NewWorkerFor(outbound.FamilyWebhook, s, "integration-webhook-worker",
		map[string]outbound.Channel{
			keys.ProviderWebhook: webhookprovider.NewHandler(s, []*net.IPNet{loopback}),
		})
	if err != nil {
		t.Fatalf("build the webhook worker: %v", err)
	}
	fanOut, err := outbound.NewFanOut(s)
	if err != nil {
		t.Fatalf("build the fan-out: %v", err)
	}
	a := api.NewAPI(s, nil, nil, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)
	// The first user of a fresh database is its administrator.
	admin := testutil.SeedUser(t, s, "admin@example.com").ID
	return &webhookEnv{s: s, worker: worker, fanOut: fanOut, e: e, admin: admin}
}

// call is one authenticated request to the API, as the administrator.
func (e *webhookEnv) call(t *testing.T, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	token, err := auth.GenerateToken(e.admin)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: api.AuthCookieName, Value: token})
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.e.ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

type receiver struct {
	calls   atomic.Int32
	body    atomic.Value
	headers atomic.Value
	status  atomic.Int32

	mu       sync.Mutex
	queue    []int    // answers given in order before status takes over
	eventIDs []string // X-Tokay-Event-ID of every request, in order
}

// answers queues statuses for the next requests, in order; after them the
// receiver answers with status again.
func (r *receiver) answers(statuses ...int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = append(r.queue, statuses...)
}

func (r *receiver) seenEventIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.eventIDs...)
}

func (r *receiver) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.calls.Add(1)
		r.body.Store(string(body))
		r.headers.Store(req.Header.Clone())
		status := int(r.status.Load())
		r.mu.Lock()
		if len(r.queue) > 0 {
			status, r.queue = r.queue[0], r.queue[1:]
		}
		r.eventIDs = append(r.eventIDs, req.Header.Get(webhookprovider.HeaderEventID))
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *webhookEnv) subscribe(t *testing.T, url string) string {
	t.Helper()
	cfg, _ := json.Marshal(model.GenericWebhookConfig{URL: url, Secret: "s3cret"})
	scope := model.WebhookScopeGlobal
	integration := &model.Integration{Type: model.IntegrationTypeGenericWebhook, Name: "hooks",
		Enabled: true, Scope: &scope, Config: cfg}
	if err := e.s.CreateIntegration(integration); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return integration.ID
}

func (e *webhookEnv) event(t *testing.T) (string, string) {
	t.Helper()
	agID := uuid.New().String()
	if err := e.s.CreateAlertGroup(&model.AlertGroup{
		ID: agID, AlertKey: "webhook-" + agID, Status: model.AlertGroupStatusNew,
		Title: "Disk filling up", Severity: "critical", TeamID: "team-1",
		Alerts: []model.Alert{{Fingerprint: "fp-" + agID, Status: "firing",
			Labels: map[string]string{"alertname": "Disk"}}},
	}); err != nil {
		t.Fatalf("create the group: %v", err)
	}
	body := `{"event":"alert_group.firing","alert_group":{"id":"` + agID + `"}}`
	event := &model.OutboxEvent{ID: uuid.New().String(), EventType: model.OutboxEventFiring,
		AlertGroupID: agID, TeamID: "team-1", Payload: json.RawMessage(body)}
	if err := e.s.CreateOutboxEvent(event); err != nil {
		t.Fatalf("write the event: %v", err)
	}
	return event.ID, body
}

// deliverOnce runs one fan-out and one worker tick, and waits for what the tick
// started.
func (e *webhookEnv) deliverOnce(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if n := e.fanOut.Tick(ctx); n < 0 {
		t.Fatal("fan out")
	}
	e.worker.Tick(ctx)
	e.worker.Drain()
}

type settled struct {
	ID              string
	Status          string
	ReceiptRecorded bool
	Receipt         sql.NullString
	ReceiptRef      sql.NullString
	Attempts        int
	LastOperation   sql.NullString
	LastOutcome     sql.NullString
	LastStatus      sql.NullString
}

func (e *webhookEnv) commitmentOf(t *testing.T, subscriber string) settled {
	t.Helper()
	var got settled
	err := e.s.GetDB().QueryRow(`
		SELECT i.id, i.status, i.receipt_recorded, i.receipt::text, i.receipt_ref,
		       (SELECT count(*) FROM outbound_attempts a WHERE a.intent_id = i.id AND a.record_kind = 'attempt'),
		       (SELECT a.operation FROM outbound_attempts a WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1),
		       (SELECT a.outcome FROM outbound_attempts a WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1),
		       (SELECT a.provider_status FROM outbound_attempts a WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1)
		FROM outbound_intents i
		WHERE i.delivery_family = 'webhook' AND i.target_ref = $1`, subscriber).
		Scan(&got.ID, &got.Status, &got.ReceiptRecorded, &got.Receipt, &got.ReceiptRef,
			&got.Attempts, &got.LastOperation, &got.LastOutcome, &got.LastStatus)
	if err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	return got
}

// TestAnAlertEventReachesItsSubscriber: the whole path, and the store's half of
// the acceptance rule - a webhook commitment settles as succeeded with no
// receipt at all, which for a message would be a contract violation.
func TestAnAlertEventReachesItsSubscriber(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := &receiver{}
	rec.status.Store(http.StatusOK)
	srv := rec.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	eventID, body := env.event(t)

	env.deliverOnce(t)

	if rec.calls.Load() != 1 {
		t.Fatalf("the subscriber was called %d times", rec.calls.Load())
	}
	// The body is the event's payload AS THE DATABASE RETURNS IT: the column is
	// JSONB, so it comes back in Postgres's normal form, not in the producer's
	// spelling - which is what subscribers have always received. What has to
	// hold is that the bytes taken at fan-out are the bytes that went out, that
	// they are the bytes frozen in the commitment, and that they mean the same
	// as what the producer wrote.
	delivered := rec.body.Load().(string)
	var stored string
	if err := env.s.GetDB().QueryRow(`SELECT payload::text FROM event_outbox WHERE id = $1`, eventID).Scan(&stored); err != nil {
		t.Fatalf("read the event back: %v", err)
	}
	if delivered != stored {
		t.Fatalf("the body arrived as\n  %s\nand the event holds\n  %s", delivered, stored)
	}
	var frozen string
	if err := env.s.GetDB().QueryRow(`SELECT payload->>'body' FROM outbound_intents
		WHERE delivery_family = 'webhook' AND target_ref = $1`, subscriber).Scan(&frozen); err != nil {
		t.Fatalf("read the frozen body: %v", err)
	}
	if delivered != frozen {
		t.Fatalf("the body that went out is not the one frozen in the commitment")
	}
	var meant, wrote map[string]any
	if err := json.Unmarshal([]byte(delivered), &meant); err != nil {
		t.Fatalf("the delivered body is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(body), &wrote); err != nil {
		t.Fatal(err)
	}
	if meant["event"] != wrote["event"] || meant["alert_group"].(map[string]any)["id"] != wrote["alert_group"].(map[string]any)["id"] {
		t.Fatalf("the delivered body means %v, the producer wrote %v", meant, wrote)
	}
	headers := rec.headers.Load().(http.Header)
	if headers.Get(webhookprovider.HeaderEventID) != eventID ||
		headers.Get(webhookprovider.HeaderEvent) != "alert_group.firing" {
		t.Fatalf("headers %v", headers)
	}
	ts := headers.Get(webhookprovider.HeaderTimestamp)
	if headers.Get(webhookprovider.HeaderSignature) != "sha256="+webhookprovider.Sign(ts, []byte(delivered), "s3cret") {
		t.Fatal("the signature does not verify over the bytes that were delivered")
	}

	got := env.commitmentOf(t, subscriber)
	if got.Status != "succeeded" {
		t.Fatalf("the commitment is %q after a 200", got.Status)
	}
	if got.ReceiptRecorded || got.Receipt.Valid || got.ReceiptRef.Valid {
		t.Fatalf("a POST recorded a receipt: %+v", got)
	}
	if got.Attempts != 1 || got.LastOperation.String != "deliver" || got.LastOutcome.String != "accepted" ||
		got.LastStatus.String != "200" {
		t.Fatalf("the journal says %+v", got)
	}

	// Nothing more to do: another round changes nothing and calls nobody.
	env.deliverOnce(t)
	if rec.calls.Load() != 1 {
		t.Fatalf("a settled commitment was delivered again: %d calls", rec.calls.Load())
	}
}

// TestASubscriberThatFailsIsRetriedAndOneThatRefusesIsNot: 500 is doubt and the
// commitment waits for its retry; 404 is a refusal and the commitment ends where
// a person can see it.
func TestASubscriberThatFailsIsRetriedAndOneThatRefusesIsNot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		want    string
		outcome string
	}{
		{"a 500 is doubt, retried", http.StatusInternalServerError, "pending", "ambiguous"},
		{"a 404 is a refusal, final", http.StatusNotFound, "permanent_failed", "permanent_rejection"},
		{"a 429 is not processed, retried", http.StatusTooManyRequests, "pending", "retryable_rejection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupWebhookEnv(t)
			rec := &receiver{}
			rec.status.Store(int32(tc.status))
			srv := rec.serve(t)
			subscriber := env.subscribe(t, srv.URL)
			env.event(t)

			env.deliverOnce(t)

			got := env.commitmentOf(t, subscriber)
			if got.Status != tc.want || got.LastOutcome.String != tc.outcome {
				t.Fatalf("after HTTP %d the commitment is %q with outcome %q, want %q / %q",
					tc.status, got.Status, got.LastOutcome.String, tc.want, tc.outcome)
			}
			if got.ReceiptRecorded {
				t.Fatal("a failed POST recorded a receipt")
			}
		})
	}
}

// TestAnEventNobodySubscribedToIsDoneWithoutACall: no subscribers, no calls,
// and the worker has nothing to claim.
func TestAnEventNobodySubscribedToIsDoneWithoutACall(t *testing.T) {
	env := setupWebhookEnv(t)
	eventID, _ := env.event(t)
	env.deliverOnce(t)

	var status string
	if err := env.s.GetDB().QueryRow(`SELECT status FROM event_outbox WHERE id = $1`, eventID).Scan(&status); err != nil ||
		status != "fanned_out" {
		t.Fatalf("the event is %q (%v)", status, err)
	}
	var commitments int
	if err := env.s.GetDB().QueryRow(`SELECT count(*) FROM outbound_intents WHERE delivery_family = 'webhook'`).
		Scan(&commitments); err != nil || commitments != 0 {
		t.Fatalf("%d commitments for nobody (%v)", commitments, err)
	}
}

// gatedReceiver holds every request until it is released, so a commitment can
// be caught on the wire.
type gatedReceiver struct {
	receiver
	entered chan struct{}
	release chan struct{}
}

func newGatedReceiver(status int) *gatedReceiver {
	g := &gatedReceiver{entered: make(chan struct{}, 8), release: make(chan struct{})}
	g.status.Store(int32(status))
	return g
}

func (g *gatedReceiver) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		g.calls.Add(1)
		g.body.Store(string(body))
		g.headers.Store(req.Header.Clone())
		g.entered <- struct{}{}
		select {
		case <-g.release:
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(int(g.status.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *webhookEnv) flagOf(t *testing.T, subscriber string) bool {
	t.Helper()
	var flagged bool
	if err := e.s.GetDB().QueryRow(`SELECT cancellation_requested FROM outbound_intents
		WHERE delivery_family = 'webhook' AND target_ref = $1`, subscriber).Scan(&flagged); err != nil {
		t.Fatalf("read the flag: %v", err)
	}
	return flagged
}

// TestDeletingASubscriberMidFlightEndsTheCommitmentWithTheAttempt: a request
// already on the wire is not withdrawn by anything; the deletion flags the
// commitment and the attempt's outcome decides. A failure becomes a withdrawal
// where a retry would otherwise follow; a success is a success. Neither leaves
// anything live, and nothing is called again.
func TestDeletingASubscriberMidFlightEndsTheCommitmentWithTheAttempt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"the request fails: withdrawn instead of retried", http.StatusInternalServerError, "canceled"},
		{"the request lands: done", http.StatusOK, "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupWebhookEnv(t)
			rec := newGatedReceiver(tc.status)
			srv := rec.serve(t)
			subscriber := env.subscribe(t, srv.URL)
			env.event(t)
			ctx := context.Background()

			if n := env.fanOut.Tick(ctx); n != 1 {
				t.Fatalf("fan-out handled %d events", n)
			}
			env.worker.Tick(ctx)
			select {
			case <-rec.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the request never went out")
			}
			if got := env.commitmentOf(t, subscriber); got.Status != "sending" {
				t.Fatalf("on the wire the commitment is %q", got.Status)
			}

			change, err := env.s.DeleteIntegration(ctx, subscriber, "nina")
			if err != nil || change.Withdrawn != 0 {
				t.Fatalf("deleting mid-flight: %+v (%v), want nothing withdrawn outright", change, err)
			}
			if got := env.commitmentOf(t, subscriber); got.Status != "sending" || !env.flagOf(t, subscriber) {
				t.Fatalf("after the deletion the commitment is %q, flagged=%v", got.Status, env.flagOf(t, subscriber))
			}

			close(rec.release)
			env.worker.Drain()
			got := env.commitmentOf(t, subscriber)
			if got.Status != tc.want {
				t.Fatalf("after HTTP %d under a deletion the commitment is %q, want %q", tc.status, got.Status, tc.want)
			}
			if env.flagOf(t, subscriber) {
				t.Fatal("the flag was not consumed by the ending")
			}
			if got.ReceiptRecorded {
				t.Fatal("a webhook recorded a receipt")
			}
			env.deliverOnce(t)
			if rec.calls.Load() != 1 {
				t.Fatalf("a deleted subscriber was called again: %d calls", rec.calls.Load())
			}
		})
	}
}

// TestTheChannelDoesNotLookAtTheSwitch: withdrawal has one door, the explicit
// transition. A commitment that outlived the switch - the state a race can
// leave, constructed here directly because the command would have withdrawn it
// - is delivered; a channel that quietly refused would be a second rule with
// its own races and no record of why a commitment stopped.
func TestTheChannelDoesNotLookAtTheSwitch(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := &receiver{}
	rec.status.Store(http.StatusOK)
	srv := rec.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	env.event(t)
	ctx := context.Background()
	if n := env.fanOut.Tick(ctx); n != 1 {
		t.Fatalf("fan-out handled %d events", n)
	}
	if _, err := env.s.GetDB().Exec(`UPDATE integrations SET enabled = FALSE WHERE id = $1`, subscriber); err != nil {
		t.Fatal(err)
	}

	env.worker.Tick(ctx)
	env.worker.Drain()
	if rec.calls.Load() != 1 {
		t.Fatalf("the subscriber was called %d times", rec.calls.Load())
	}
	if got := env.commitmentOf(t, subscriber); got.Status != "succeeded" {
		t.Fatalf("the commitment is %q", got.Status)
	}
}

// The delivery routes over the journal, and the replay through the API.

func (e *webhookEnv) deliveries(t *testing.T, subscriber string) api.DeliveryListResponse {
	t.Helper()
	rec := e.call(t, http.MethodGet, "/api/v1/integrations/"+subscriber+"/deliveries", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var resp api.DeliveryListResponse
	decodeInto(t, rec, &resp)
	return resp
}

func (e *webhookEnv) delivery(t *testing.T, subscriber, id string) api.DeliveryDetailResponse {
	t.Helper()
	rec := e.call(t, http.MethodGet, "/api/v1/integrations/"+subscriber+"/deliveries/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var resp api.DeliveryDetailResponse
	decodeInto(t, rec, &resp)
	return resp
}

func (e *webhookEnv) replay(t *testing.T, subscriber, id, key string) *httptest.ResponseRecorder {
	t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return e.call(t, http.MethodPost, "/api/v1/integrations/"+subscriber+"/deliveries/"+id+"/replay", headers)
}

func (e *webhookEnv) dueNow(t *testing.T, deliveryID string) {
	t.Helper()
	if _, err := e.s.GetDB().Exec(`UPDATE outbound_intents SET next_attempt_at = now() WHERE id = $1`, deliveryID); err != nil {
		t.Fatal(err)
	}
}

func (e *webhookEnv) rowsOwedTo(t *testing.T, subscriber string) int {
	t.Helper()
	var n int
	if err := e.s.GetDB().QueryRow(`SELECT count(*) FROM outbound_intents
		WHERE delivery_family = 'webhook' AND target_ref = $1`, subscriber).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheDeliveryRoutesShowTheJournal: the form the routes always had, over
// real attempts. A failure then a success: the list says retry, then sent; the
// three "last" fields follow the last attempt, so no error stands beside a
// success; the failure stays visible in the attempts, numbered from zero. A
// refusal is failed with its reason; an ending with no attempt behind it names
// the ending.
func TestTheDeliveryRoutesShowTheJournal(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := &receiver{}
	rec.status.Store(http.StatusOK)
	rec.answers(http.StatusInternalServerError)
	srv := rec.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	eventID, _ := env.event(t)

	env.deliverOnce(t)
	list := env.deliveries(t, subscriber)
	if list.Total != 1 || len(list.Deliveries) != 1 {
		t.Fatalf("after one attempt the list is %+v", list)
	}
	d := list.Deliveries[0]
	if d.Status != model.OutboxDeliveryRetry || d.Attempts != 1 || d.LastHTTPStatus == nil || *d.LastHTTPStatus != 500 ||
		d.LastError == nil || d.NextAttemptAt == nil || d.SentAt != nil || d.EventID != eventID ||
		d.IntegrationID != subscriber || d.RequestPayload == nil || d.CreatedAt.IsZero() {
		t.Fatalf("after a 500 the delivery reads %+v", d)
	}
	var stored string
	if err := env.s.GetDB().QueryRow(`SELECT payload::text FROM event_outbox WHERE id = $1`, eventID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if *d.RequestPayload != stored {
		t.Fatalf("request_payload is not the bytes that went out: %s", *d.RequestPayload)
	}

	env.dueNow(t, d.ID)
	env.deliverOnce(t)
	detail := env.delivery(t, subscriber, d.ID)
	got := detail.Delivery
	if got.Status != model.OutboxDeliverySent || got.Attempts != 2 || got.LastHTTPStatus == nil || *got.LastHTTPStatus != 200 ||
		got.LastError != nil || got.ResponseBodyTrunc == nil || got.SentAt == nil || got.NextAttemptAt != nil {
		t.Fatalf("after a 500 and a 200 the delivery reads %+v", got)
	}
	if len(detail.Attempts) != 2 {
		t.Fatalf("attempts: %+v", detail.Attempts)
	}
	first, second := detail.Attempts[0], detail.Attempts[1]
	if first.Attempt != 0 || first.HTTPStatus == nil || *first.HTTPStatus != 500 || first.Error == nil ||
		first.DeliveryID != d.ID || first.CreatedAt.IsZero() {
		t.Fatalf("the first attempt reads %+v", first)
	}
	if second.Attempt != 1 || second.HTTPStatus == nil || *second.HTTPStatus != 200 || second.Error != nil {
		t.Fatalf("the second attempt reads %+v", second)
	}

	// A refusal: failed, with the reason.
	refusing := &receiver{}
	refusing.status.Store(http.StatusNotFound)
	other := env.subscribe(t, refusing.serve(t).URL)
	env.event(t)
	env.deliverOnce(t)
	for _, d := range env.deliveries(t, other).Deliveries {
		if d.Status != model.OutboxDeliveryFailed || d.LastError == nil || d.LastHTTPStatus == nil || *d.LastHTTPStatus != 404 {
			t.Fatalf("after a 404 the delivery reads %+v", d)
		}
	}

	// An ending nothing was ever tried for: failed, and the reason is the ending.
	env.event(t)
	if n := env.fanOut.Tick(context.Background()); n != 1 {
		t.Fatalf("fan-out handled %d", n)
	}
	var pending string
	if err := env.s.GetDB().QueryRow(`SELECT id FROM outbound_intents WHERE delivery_family = 'webhook'
		AND target_ref = $1 AND status = 'pending'`, other).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if _, err := env.s.GetDB().Exec(`UPDATE outbound_intents SET status = 'expired' WHERE id = $1`, pending); err != nil {
		t.Fatal(err)
	}
	expired := env.delivery(t, other, pending).Delivery
	if expired.Status != model.OutboxDeliveryFailed || expired.LastError == nil || *expired.LastError != "expired" ||
		expired.LastHTTPStatus != nil || expired.Attempts != 0 {
		t.Fatalf("an expired delivery reads %+v", expired)
	}
}

// TestAReplayThroughTheAPIIsOneDecisionUnderOneKey: no key is refused; one key
// is one new delivery, and its repeat is the same delivery with the same
// answer; another key is another delivery. The replays go out with the
// original event's id. A delivery still in progress is not replayed.
func TestAReplayThroughTheAPIIsOneDecisionUnderOneKey(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := &receiver{}
	rec.status.Store(http.StatusOK)
	srv := rec.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	eventID, _ := env.event(t)
	env.deliverOnce(t)
	original := env.deliveries(t, subscriber).Deliveries[0]
	if original.Status != model.OutboxDeliverySent {
		t.Fatalf("the original is %s", original.Status)
	}

	if got := env.replay(t, subscriber, original.ID, "").Code; got != http.StatusBadRequest {
		t.Fatalf("a replay without a key answered %d", got)
	}
	if got := env.replay(t, subscriber, original.ID, strings.Repeat("k", 129)).Code; got != http.StatusBadRequest {
		t.Fatalf("a replay with an oversized key answered %d", got)
	}
	if env.rowsOwedTo(t, subscriber) != 1 {
		t.Fatal("a refused replay made a delivery")
	}

	var first, again, another api.ReplayDeliveryResponse
	if r := env.replay(t, subscriber, original.ID, "press-1"); r.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", r.Code, r.Body.String())
	} else {
		decodeInto(t, r, &first)
	}
	if r := env.replay(t, subscriber, original.ID, "press-1"); r.Code != http.StatusOK {
		t.Fatalf("the repeat: %d %s", r.Code, r.Body.String())
	} else {
		decodeInto(t, r, &again)
	}
	if r := env.replay(t, subscriber, original.ID, "press-2"); r.Code != http.StatusOK {
		t.Fatalf("a second decision: %d %s", r.Code, r.Body.String())
	} else {
		decodeInto(t, r, &another)
	}
	if !first.OK || first.DeliveryID == "" || first.DeliveryID == original.ID {
		t.Fatalf("the replay answered %+v", first)
	}
	if again != first {
		t.Fatalf("the repeat answered %+v, the first %+v", again, first)
	}
	if another.DeliveryID == first.DeliveryID {
		t.Fatal("a second decision found the first's delivery")
	}
	if env.rowsOwedTo(t, subscriber) != 3 {
		t.Fatalf("%d deliveries, want the original and two replays", env.rowsOwedTo(t, subscriber))
	}
	if d := env.delivery(t, subscriber, first.DeliveryID).Delivery; d.Status != model.OutboxDeliveryPending || d.EventID != eventID {
		t.Fatalf("the new delivery reads %+v before the worker", d)
	}

	env.deliverOnce(t)
	if rec.calls.Load() != 3 {
		t.Fatalf("the subscriber was called %d times, want the original and two replays", rec.calls.Load())
	}
	for _, seen := range rec.seenEventIDs() {
		if seen != eventID {
			t.Fatalf("a replay went out as event %s, the original was %s", seen, eventID)
		}
	}
	if d := env.delivery(t, subscriber, first.DeliveryID).Delivery; d.Status != model.OutboxDeliverySent {
		t.Fatalf("the replay reads %+v after the worker", d)
	}

	// Still in progress: not replayed, nothing made.
	env.event(t)
	if n := env.fanOut.Tick(context.Background()); n != 1 {
		t.Fatalf("fan-out handled %d", n)
	}
	var live string
	if err := env.s.GetDB().QueryRow(`SELECT id FROM outbound_intents WHERE delivery_family = 'webhook'
		AND target_ref = $1 AND status = 'pending'`, subscriber).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if got := env.replay(t, subscriber, live, "press-3").Code; got != http.StatusConflict {
		t.Fatalf("a replay of a live delivery answered %d", got)
	}
	if env.rowsOwedTo(t, subscriber) != 4 {
		t.Fatal("a refused replay made a delivery")
	}
}

// TestAReplayGoesToTheCurrentAddressWhileAStuckOriginalKeepsItsOwn: the address
// is bound to a generation by its first open attempt, so a delivery already
// tried at the old address retries there after the URL is corrected; a replay
// is a new delivery and goes to the address as it is now.
func TestAReplayGoesToTheCurrentAddressWhileAStuckOriginalKeepsItsOwn(t *testing.T) {
	env := setupWebhookEnv(t)
	old := &receiver{}
	old.status.Store(http.StatusInternalServerError)
	old.answers(http.StatusOK)
	oldSrv := old.serve(t)
	current := &receiver{}
	current.status.Store(http.StatusOK)
	currentSrv := current.serve(t)

	subscriber := env.subscribe(t, oldSrv.URL)
	delivered, _ := env.event(t)
	env.deliverOnce(t) // 200: sent, at the old address
	env.event(t)
	env.deliverOnce(t) // 500: stuck, bound to the old address
	if old.calls.Load() != 2 {
		t.Fatalf("the old address was called %d times", old.calls.Load())
	}
	var sent, stuck string
	for _, d := range env.deliveries(t, subscriber).Deliveries {
		switch d.Status {
		case model.OutboxDeliverySent:
			sent = d.ID
		case model.OutboxDeliveryRetry:
			stuck = d.ID
		}
	}
	if sent == "" || stuck == "" {
		t.Fatalf("expected one sent and one stuck delivery: %+v", env.deliveries(t, subscriber).Deliveries)
	}

	moved, _ := json.Marshal(model.GenericWebhookConfig{URL: currentSrv.URL, Secret: "s3cret"})
	if _, err := env.s.UpdateIntegration(context.Background(), subscriber, store.IntegrationPatch{Config: moved}, "nina"); err != nil {
		t.Fatal(err)
	}
	var replayed api.ReplayDeliveryResponse
	if r := env.replay(t, subscriber, sent, "press-1"); r.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", r.Code, r.Body.String())
	} else {
		decodeInto(t, r, &replayed)
	}
	env.dueNow(t, stuck)

	env.deliverOnce(t)
	if old.calls.Load() != 3 || current.calls.Load() != 1 {
		t.Fatalf("old address called %d times and the current %d: want the stuck retry at the old, the replay at the current",
			old.calls.Load(), current.calls.Load())
	}
	if seen := current.seenEventIDs(); len(seen) != 1 || seen[0] != delivered {
		t.Fatalf("the current address received %v, want the replayed event %s", seen, delivered)
	}
	if d := env.delivery(t, subscriber, replayed.DeliveryID).Delivery; d.Status != model.OutboxDeliverySent {
		t.Fatalf("the replay reads %+v", d)
	}
	if d := env.delivery(t, subscriber, stuck).Delivery; d.Status != model.OutboxDeliveryRetry || d.Attempts != 2 {
		t.Fatalf("the stuck original reads %+v", d)
	}
}

// TestTheHistoryOfADeletedSubscriberIsReadAndNotReplayed: after the subscriber
// is deleted through the API its deliveries are still listed and opened, and a
// replay makes nothing; a deleted Slack integration has no history route.
func TestTheHistoryOfADeletedSubscriberIsReadAndNotReplayed(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := &receiver{}
	rec.status.Store(http.StatusOK)
	subscriber := env.subscribe(t, rec.serve(t).URL)
	env.event(t)
	env.deliverOnce(t)
	original := env.deliveries(t, subscriber).Deliveries[0]

	if r := env.call(t, http.MethodDelete, "/api/v1/integrations/"+subscriber, nil); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	list := env.deliveries(t, subscriber)
	if list.Total != 1 || list.Deliveries[0].ID != original.ID || list.Deliveries[0].Status != model.OutboxDeliverySent {
		t.Fatalf("the deleted subscriber's history: %+v", list)
	}
	if d := env.delivery(t, subscriber, original.ID); d.Delivery.ID != original.ID || len(d.Attempts) != 1 {
		t.Fatalf("the deleted subscriber's delivery: %+v", d)
	}
	if got := env.replay(t, subscriber, original.ID, "press-1").Code; got != http.StatusNotFound {
		t.Fatalf("a replay to a deleted subscriber answered %d", got)
	}
	if env.rowsOwedTo(t, subscriber) != 1 {
		t.Fatal("a deleted subscriber was promised a delivery")
	}

	slackCfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-1"})
	slack := &model.Integration{Type: model.IntegrationTypeSlack, Name: "slack", Enabled: true, Config: slackCfg}
	if err := env.s.CreateIntegration(slack); err != nil {
		t.Fatal(err)
	}
	if _, err := env.s.DeleteIntegration(context.Background(), slack.ID, "nina"); err != nil {
		t.Fatal(err)
	}
	if got := env.call(t, http.MethodGet, "/api/v1/integrations/"+slack.ID+"/deliveries", nil).Code; got != http.StatusNotFound {
		t.Fatalf("a deleted Slack integration's history route answered %d", got)
	}
}

// TestTheAttemptIsWrittenBeforeTheCall: while the request is on the wire the
// journal already holds an open attempt for it. A worker that died here would
// leave a record for recovery to close, not a call nobody knew was made.
func TestTheAttemptIsWrittenBeforeTheCall(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := newGatedReceiver(http.StatusOK)
	srv := rec.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	env.event(t)
	ctx := context.Background()
	env.fanOut.Tick(ctx)
	env.worker.Tick(ctx)
	select {
	case <-rec.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never went out")
	}

	var open int
	if err := env.s.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_attempts a JOIN outbound_intents i ON i.id = a.intent_id
		WHERE i.target_ref = $1 AND a.record_kind = 'attempt' AND a.started_at IS NOT NULL AND a.finished_at IS NULL`,
		subscriber).Scan(&open); err != nil || open != 1 {
		t.Fatalf("%d open attempts while the request is on the wire (%v), want 1", open, err)
	}
	close(rec.release)
	env.worker.Drain()
	if got := env.commitmentOf(t, subscriber); got.Status != "succeeded" || got.Attempts != 1 {
		t.Fatalf("after the answer the commitment reads %+v", got)
	}
}

// TestOneSubscribersFailureLeavesTheOthersDeliveryAlone: one event, two
// subscribers, two commitments; the one whose subscriber failed is retried, the
// other is done.
func TestOneSubscribersFailureLeavesTheOthersDeliveryAlone(t *testing.T) {
	env := setupWebhookEnv(t)
	failing := &receiver{}
	failing.status.Store(http.StatusInternalServerError)
	fine := &receiver{}
	fine.status.Store(http.StatusOK)
	unlucky := env.subscribe(t, failing.serve(t).URL)
	lucky := env.subscribe(t, fine.serve(t).URL)
	env.event(t)

	env.deliverOnce(t)
	if failing.calls.Load() != 1 || fine.calls.Load() != 1 {
		t.Fatalf("calls: failing %d, fine %d", failing.calls.Load(), fine.calls.Load())
	}
	if got := env.commitmentOf(t, unlucky); got.Status != "pending" || got.LastOutcome.String != "ambiguous" {
		t.Fatalf("the failing subscriber's commitment reads %+v", got)
	}
	if got := env.commitmentOf(t, lucky); got.Status != "succeeded" {
		t.Fatalf("the other subscriber's commitment reads %+v", got)
	}
}

// TestADeadSubscriberStaysOwedAndIsRetriedOnTheCurve: twenty answers of 500 in
// a row, and the commitment is still owed - no attempt limit ends it - with a
// failure streak the backoff curve is read from, and its next attempt no
// further away than the family's ceiling allows.
func TestADeadSubscriberStaysOwedAndIsRetriedOnTheCurve(t *testing.T) {
	env := setupWebhookEnv(t)
	rec := &receiver{}
	rec.status.Store(http.StatusInternalServerError)
	subscriber := env.subscribe(t, rec.serve(t).URL)
	env.event(t)

	for round := 1; round <= 20; round++ {
		if round > 1 {
			env.dueNow(t, env.commitmentOf(t, subscriber).ID)
		}
		env.deliverOnce(t)
		if rec.calls.Load() != int32(round) {
			t.Fatalf("round %d: the subscriber was called %d times", round, rec.calls.Load())
		}
	}
	got := env.commitmentOf(t, subscriber)
	if got.Status != "pending" || got.Attempts != 20 {
		t.Fatalf("after twenty failures the commitment reads %+v", got)
	}
	var streak int
	var wait float64
	if err := env.s.GetDB().QueryRow(`SELECT failure_streak, EXTRACT(EPOCH FROM (next_attempt_at - now()))
		FROM outbound_intents WHERE id = $1`, got.ID).Scan(&streak, &wait); err != nil {
		t.Fatal(err)
	}
	if streak != 20 {
		t.Fatalf("failure_streak = %d, want 20", streak)
	}
	ceiling := (outbound.WebhookBackoffCap + outbound.WebhookBackoffCap/5).Seconds()
	if wait <= 0 || wait > ceiling {
		t.Fatalf("the next attempt is %.0fs away; want within the 30 minute ceiling and its jitter", wait)
	}
}
