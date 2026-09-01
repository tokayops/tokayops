package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
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
	return &webhookEnv{s: s, worker: worker, fanOut: fanOut}
}

type receiver struct {
	calls   atomic.Int32
	body    atomic.Value
	headers atomic.Value
	status  atomic.Int32
}

func (r *receiver) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.calls.Add(1)
		r.body.Store(string(body))
		r.headers.Store(req.Header.Clone())
		w.WriteHeader(int(r.status.Load()))
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
		SELECT i.status, i.receipt_recorded, i.receipt::text, i.receipt_ref,
		       (SELECT count(*) FROM outbound_attempts a WHERE a.intent_id = i.id AND a.record_kind = 'attempt'),
		       (SELECT a.operation FROM outbound_attempts a WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1),
		       (SELECT a.outcome FROM outbound_attempts a WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1),
		       (SELECT a.provider_status FROM outbound_attempts a WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1)
		FROM outbound_intents i
		WHERE i.delivery_family = 'webhook' AND i.target_ref = $1`, subscriber).
		Scan(&got.Status, &got.ReceiptRecorded, &got.Receipt, &got.ReceiptRef,
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
