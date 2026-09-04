//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/testutil"
)

// The operator's door over the real store and real deliveries: the guard's
// words travel from the machine through the store and the door to the body a
// person reads; the alert being over refuses a retry; and every line the
// decision writes is signed by the person, by id.

// deciding is the API over the pipeline's store, with an administrator to
// knock on its doors.
type deciding struct {
	t   *testing.T
	env *IntegrationTestEnv
	e   *echo.Echo
}

func setupDeciding(t *testing.T, env *IntegrationTestEnv) *deciding {
	t.Helper()
	a := api.NewAPI(env.S, nil, nil, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)
	if err := env.S.CreateUser(&model.User{
		ID: "root", Email: "root@pipeline.test", Name: "Root", Role: model.UserRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	return &deciding{t: t, env: env, e: e}
}

func (d *deciding) as(user, method, path, body string) *httptest.ResponseRecorder {
	d.t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	token, err := auth.GenerateToken(user)
	if err != nil {
		d.t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: api.AuthCookieName, Value: token})
	rec := httptest.NewRecorder()
	d.e.ServeHTTP(rec, req)
	return rec
}

func (d *deciding) decide(user, id, body string) (int, api.DecisionResponse) {
	d.t.Helper()
	rec := d.as(user, http.MethodPost, "/api/v1/deliveries/"+id+"/decisions", body)
	var resp api.DecisionResponse
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			d.t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec.Code, resp
}

func (d *deciding) journal(id string) api.DeliveryJournalResponse {
	d.t.Helper()
	rec := d.as("root", http.MethodGet, "/api/v1/deliveries/"+id, "")
	if rec.Code != http.StatusOK {
		d.t.Fatalf("journal of %s: %d %s", id, rec.Code, rec.Body.String())
	}
	var resp api.DeliveryJournalResponse
	decodeInto(d.t, rec, &resp)
	return resp
}

// pageOf is the commitment an alert made to the person the critical policy
// pages.
func pageOf(t *testing.T, env *IntegrationTestEnv, alertKey string) string {
	t.Helper()
	for _, intent := range intentsOf(t, env, alertKey) {
		if intent.TargetRef == "U_TEST" {
			return intent.ID
		}
	}
	t.Fatalf("%s made no commitment to U_TEST", alertKey)
	return ""
}

func untilStatus(t *testing.T, env *IntegrationTestEnv, intentID string, want outbound.Status) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := env.S.GetDB().QueryRow(`SELECT status FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&status); err == nil && outbound.Status(status) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never became %s", intentID, want)
}

// signedIn is the last line of a kind in the journal, as the door shows it.
func signedIn(journal api.DeliveryJournalResponse, kind string) (actor, actorKind string, found bool) {
	for i := len(journal.Events) - 1; i >= 0; i-- {
		if journal.Events[i].Kind == kind {
			return journal.Events[i].Actor, journal.Events[i].ActorKind, true
		}
	}
	return "", "", false
}

func TestADecisionThroughTheDoor(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()
	d := setupDeciding(t, env)

	// Two alerts page one person. The first call about each is answered by
	// silence the provider may or may not have acted on; the second is refused
	// for good. Both commitments end failed, with a doubt in their generation.
	env.Channel.FailWith("S_TEST", outbound.Result{Evidence: outbound.PossiblySent}, nil)
	sendWebhook(t, env.Echo, criticalAlert("decide_a", "DiskFilling"))
	sendWebhook(t, env.Echo, criticalAlert("decide_b", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(ctx)
	stop := startOutboundWorker(t, env.Worker)
	until(t, "the first attempt about each alert", func() bool {
		return len(env.Channel.SentTo("S_TEST")) >= 2
	})
	env.Channel.FailWith("S_TEST", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "channel_not_found",
	}, nil)
	failedA, failedB := pageOf(t, env, "decide_a"), pageOf(t, env, "decide_b")
	untilStatus(t, env, failedA, outbound.StatusPermanentFailed)
	untilStatus(t, env, failedB, outbound.StatusPermanentFailed)
	stop()

	// A new generation after a doubtful attempt needs the risk of a duplicate
	// accepted, and the refusal says so in the guard's own words - from the
	// machine, through the store, to the body.
	code, resp := d.decide("root", failedA, `{"decision":"retry_new_generation","reason":"the channel is back"}`)
	if code != http.StatusUnprocessableEntity || resp.Outcome != "invalid_decision" ||
		resp.Status != "permanent_failed" ||
		resp.Detail != "a new generation after an ambiguous attempt needs the duplicate risk accepted" {
		t.Fatalf("without the flag: %d %+v", code, resp)
	}
	code, resp = d.decide("root", failedA,
		`{"decision":"retry_new_generation","reason":"the channel is back","accepted_duplicate_risk":true}`)
	if code != http.StatusOK || resp.Outcome != "resolved" || resp.Row == "" ||
		resp.Delivery == nil || resp.Delivery.ID != failedA || resp.Delivery.Status != "pending" {
		t.Fatalf("with the flag: %d %+v", code, resp)
	}

	// Every line the decision wrote is the person's, by id.
	journal := d.journal(failedA)
	for _, kind := range []string{"operator_decision", "duplicate_risk_accepted", "generation_started"} {
		actor, actorKind, found := signedIn(journal, kind)
		if !found {
			t.Errorf("the decision left no %s line", kind)
		} else if actor != "root" || actorKind != "user" {
			t.Errorf("%s is signed %s:%s, want user:root", kind, actorKind, actor)
		}
	}

	// The alert is over: nothing is retried for it, and the body says why. A
	// withdrawal still applies - it ends the commitment rather than reviving
	// it.
	agB := agIDForKey(t, env.S, "decide_b")
	if rec := d.as("root", http.MethodPatch, "/api/v1/alert-groups/"+agB+"/resolve", ""); rec.Code != http.StatusOK {
		t.Fatalf("resolve the alert: %d %s", rec.Code, rec.Body.String())
	}
	code, resp = d.decide("root", failedB, `{"decision":"retry_current_generation","reason":"once more"}`)
	if code != http.StatusConflict || resp.Outcome != "business_closed" || resp.Detail == "" {
		t.Fatalf("a retry for an alert that is over: %d %+v", code, resp)
	}
	code, resp = d.decide("root", failedB, `{"decision":"cancel","reason":"the incident is over"}`)
	if code != http.StatusOK || resp.Status != "canceled" {
		t.Fatalf("cancel for an alert that is over: %d %+v", code, resp)
	}
	code, resp = d.decide("root", failedB, `{"decision":"cancel","reason":"again"}`)
	if code != http.StatusConflict || resp.Outcome != "already_resolved" || resp.Status != "canceled" {
		t.Fatalf("a second decision: %d %+v", code, resp)
	}

	// An acknowledgement through the door signs the withdrawal it makes as the
	// person, by id - not by the name the timeline shows.
	sendWebhook(t, env.Echo, criticalAlert("decide_c", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(ctx)
	owedByC := pageOf(t, env, "decide_c")
	agC := agIDForKey(t, env.S, "decide_c")
	if rec := d.as("root", http.MethodPatch, "/api/v1/alert-groups/"+agC+"/ack", ""); rec.Code != http.StatusOK {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	if actor, actorKind, found := signedIn(d.journal(owedByC), "canceled"); !found || actor != "root" || actorKind != "user" {
		t.Errorf("the acknowledgement's withdrawal is signed %s:%s (found %v), want user:root", actorKind, actor, found)
	}

	// And who may decide: not a user.
	bob := testutil.SeedUser(t, env.S, "bob@pipeline.test").ID
	if code, _ := d.decide(bob, failedA, `{"decision":"cancel","reason":"no"}`); code != http.StatusForbidden {
		t.Errorf("a user decided: %d", code)
	}
	if code, _ := d.decide("root", "nothing", `{"decision":"cancel","reason":"no"}`); code != http.StatusNotFound {
		t.Errorf("an unknown delivery answered %d", code)
	}
}

// TestAWebhookDeliveryHasOneDoorToANewEffect: a webhook delivery that failed
// for good is withdrawn by a decision, and never retried by one - its door to
// a new effect is the replay, and the refusal names it.
func TestAWebhookDeliveryHasOneDoorToANewEffect(t *testing.T) {
	env := setupWebhookEnv(t)
	r := &receiver{}
	r.status.Store(http.StatusNotFound)
	srv := r.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	env.event(t)
	env.deliverOnce(t)

	var id, status string
	if err := env.s.GetDB().QueryRow(`SELECT id, status FROM outbound_intents WHERE target_ref = $1`, subscriber).
		Scan(&id, &status); err != nil {
		t.Fatal(err)
	}
	if status != "permanent_failed" {
		t.Fatalf("the refused delivery is %s", status)
	}

	decide := func(body string) (int, api.DecisionResponse) {
		t.Helper()
		rec := env.callJSON(t, http.MethodPost, "/api/v1/deliveries/"+id+"/decisions", body)
		var resp api.DecisionResponse
		decodeInto(t, rec, &resp)
		return rec.Code, resp
	}
	for _, decision := range []string{"retry_current_generation", "retry_new_generation"} {
		code, resp := decide(`{"decision":"` + decision + `","reason":"once more","accepted_duplicate_risk":true}`)
		if code != http.StatusUnprocessableEntity || resp.Outcome != "invalid_decision" ||
			!strings.Contains(resp.Detail, "its door to a new effect is replay") {
			t.Errorf("%s for a webhook delivery: %d %+v", decision, code, resp)
		}
	}
	// Assuming a refusal was an acceptance is refused before the family is
	// even asked: it would claim something known to be false.
	if code, resp := decide(`{"decision":"assume_accepted","reason":"it arrived"}`); code != http.StatusUnprocessableEntity ||
		resp.Outcome != "invalid_decision" || resp.Detail == "" {
		t.Errorf("assume_accepted for a refused webhook delivery: %d %+v", code, resp)
	}
	code, resp := decide(`{"decision":"cancel","reason":"the subscriber is gone"}`)
	if code != http.StatusOK || resp.Status != "canceled" {
		t.Errorf("cancel for a webhook delivery: %d %+v", code, resp)
	}
}

// TestTwoOperatorsAtOneDoor: two people decide about one commitment at the
// same moment through the door. One decision applies; the other is answered
// with the state the first left, and the journal holds one decision. Which
// of them wins is the database's to say.
func TestTwoOperatorsAtOneDoor(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()
	d := setupDeciding(t, env)
	if err := env.S.CreateUser(&model.User{
		ID: "root-2", Email: "root2@pipeline.test", Name: "Root 2", Role: model.UserRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}

	env.Channel.FailWith("S_TEST", outbound.Result{Evidence: outbound.PossiblySent}, nil)
	sendWebhook(t, env.Echo, criticalAlert("decide_twice", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(ctx)
	stop := startOutboundWorker(t, env.Worker)
	until(t, "the first attempt", func() bool { return len(env.Channel.SentTo("S_TEST")) >= 1 })
	env.Channel.FailWith("S_TEST", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "channel_not_found",
	}, nil)
	failed := pageOf(t, env, "decide_twice")
	untilStatus(t, env, failed, outbound.StatusPermanentFailed)
	stop()

	bodies := []string{
		`{"decision":"cancel","reason":"nobody is listening"}`,
		`{"decision":"retry_new_generation","reason":"the channel is back","accepted_duplicate_risk":true}`,
	}
	users := []string{"root", "root-2"}
	codes := make([]int, 2)
	answers := make([]api.DecisionResponse, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i], answers[i] = d.decide(users[i], failed, bodies[i])
		}(i)
	}
	close(start)
	wg.Wait()

	applied, refused := 0, 0
	for i := range codes {
		switch {
		case codes[i] == http.StatusOK && answers[i].Outcome == "resolved":
			applied++
		case codes[i] == http.StatusConflict && answers[i].Outcome == "already_resolved" && answers[i].Status != "":
			refused++
		default:
			t.Errorf("operator %d got %d %+v", i, codes[i], answers[i])
		}
	}
	if applied != 1 || refused != 1 {
		t.Fatalf("two decisions produced %d applied and %d refused", applied, refused)
	}
	var decisions int
	if err := env.S.GetDB().QueryRow(`SELECT count(*) FROM outbound_intent_events
		WHERE intent_id = $1 AND kind = 'operator_decision'`, failed).Scan(&decisions); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 {
		t.Errorf("the journal holds %d decisions, want 1", decisions)
	}
}
