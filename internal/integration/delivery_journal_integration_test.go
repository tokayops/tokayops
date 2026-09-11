//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/auth"
	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/testutil"
)

// The journal routes over the real store and a real delivery: one event to
// one subscriber, delivered through the webhook channel to a real HTTP server,
// then read back three ways - the operational log, the journal of the one
// delivery, and the group's deliveries - and once more through the routes the
// subscriber's contract keeps, whose bytes must not have moved.

// callAs is one authenticated request as an arbitrary user.
func (e *webhookEnv) callAs(t *testing.T, userID, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	token, err := auth.GenerateToken(userID)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: api.AuthCookieName, Value: token})
	rec := httptest.NewRecorder()
	e.e.ServeHTTP(rec, req)
	return rec
}

func TestTheJournalReadsARealDeliveryThreeWays(t *testing.T) {
	env := setupWebhookEnv(t)
	r := &receiver{}
	r.status.Store(http.StatusOK)
	srv := r.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	eventID, body := env.event(t)
	env.deliverOnce(t)

	var event struct {
		AlertGroup struct{ ID string } `json:"alert_group"`
	}
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		t.Fatal(err)
	}

	// The operational log, narrowed to the family.
	rec := env.call(t, http.MethodGet, "/api/v1/deliveries?family=webhook", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list api.DeliveriesResponse
	decodeInto(t, rec, &list)
	if list.Total != 1 || len(list.Deliveries) != 1 {
		t.Fatalf("the log lists %d deliveries: %+v", list.Total, list.Deliveries)
	}
	delivery := list.Deliveries[0]
	if delivery.Status != "succeeded" || delivery.TargetRef != subscriber || delivery.Family != "webhook" ||
		delivery.Kind != "webhook_event" || delivery.CreatedAt.IsZero() {
		t.Errorf("the delivery reads %+v", delivery)
	}

	// The journal of that one delivery: the attempt with its outcome, and the
	// events with their time. A webhook acceptance names no object, and the
	// journal says so rather than pretending to a receipt.
	rec = env.call(t, http.MethodGet, "/api/v1/deliveries/"+delivery.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("journal: %d %s", rec.Code, rec.Body.String())
	}
	var journal api.DeliveryJournalResponse
	decodeInto(t, rec, &journal)
	if len(journal.Attempts) != 1 || journal.Attempts[0].Outcome != "accepted" ||
		journal.Attempts[0].FinishedAt == nil || journal.Attempts[0].ReceiptRecorded {
		t.Errorf("the attempts read %+v", journal.Attempts)
	}
	if len(journal.Events) == 0 {
		t.Fatal("no events in the journal")
	}
	for _, e := range journal.Events {
		if e.At.IsZero() {
			t.Errorf("event %d (%s) has no time", e.Seq, e.Kind)
		}
	}

	// The group's deliveries: the event, the fan-out's claim, the delivery
	// under it - the same id.
	rec = env.call(t, http.MethodGet, "/api/v1/alert-groups/"+event.AlertGroup.ID+"/deliveries", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("group: %d %s", rec.Code, rec.Body.String())
	}
	var group api.AlertGroupDeliveriesResponse
	decodeInto(t, rec, &group)
	if len(group.Events) != 1 || group.Events[0].EventID != eventID || len(group.Events[0].Batches) != 1 ||
		len(group.Events[0].Batches[0].Deliveries) != 1 || group.Events[0].Batches[0].Deliveries[0].ID != delivery.ID {
		t.Errorf("the group's deliveries read %+v", group)
	}

	// And who may ask: the log and the journal are the administrator's; the
	// group's deliveries are its readers'.
	bob := testutil.SeedUser(t, env.s, "bob@example.com").ID
	if rec := env.callAs(t, bob, http.MethodGet, "/api/v1/deliveries"); rec.Code != http.StatusForbidden {
		t.Errorf("a user read the log: %d", rec.Code)
	}
	if rec := env.callAs(t, bob, http.MethodGet, "/api/v1/deliveries/"+delivery.ID); rec.Code != http.StatusForbidden {
		t.Errorf("a user read a journal: %d", rec.Code)
	}
	if rec := env.callAs(t, bob, http.MethodGet, "/api/v1/alert-groups/"+event.AlertGroup.ID+"/deliveries"); rec.Code != http.StatusOK {
		t.Errorf("a user could not read the group's deliveries: %d %s", rec.Code, rec.Body.String())
	}
}

var (
	uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)
)

// normalized is a response with every id and every timestamp replaced by a
// placeholder, so that two runs over the same shape produce the same bytes.
func normalized(body []byte) []byte {
	out := uuidPattern.ReplaceAll(body, []byte("<uuid>"))
	out = timePattern.ReplaceAll(out, []byte("<time>"))
	return out
}

// TestTheWebhookRoutesKeepTheirBytes: the subscriber's contract - the list and
// the detail under /integrations - is compared byte for byte, ids and times
// aside, against what it answered before the journal existed. The golden files
// are regenerated only on purpose (UPDATE_GOLDEN=1), which is the point of
// them: a change here is a change to a contract, and has to be one.
func TestTheWebhookRoutesKeepTheirBytes(t *testing.T) {
	env := setupWebhookEnv(t)
	r := &receiver{}
	r.status.Store(http.StatusOK)
	srv := r.serve(t)
	subscriber := env.subscribe(t, srv.URL)
	env.event(t)
	env.deliverOnce(t)

	list := env.call(t, http.MethodGet, "/api/v1/integrations/"+subscriber+"/deliveries", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	var listed struct {
		Deliveries []struct{ ID string } `json:"deliveries"`
	}
	decodeInto(t, list, &listed)
	if len(listed.Deliveries) != 1 {
		t.Fatalf("%d deliveries listed", len(listed.Deliveries))
	}
	detail := env.call(t, http.MethodGet, "/api/v1/integrations/"+subscriber+"/deliveries/"+listed.Deliveries[0].ID, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", detail.Code, detail.Body.String())
	}

	for name, got := range map[string][]byte{
		"webhook_deliveries_list.golden.json":   normalized(list.Body.Bytes()),
		"webhook_deliveries_detail.golden.json": normalized(detail.Body.Bytes()),
	} {
		path := filepath.Join("testdata", name)
		if os.Getenv("UPDATE_GOLDEN") != "" {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v (regenerate on purpose with UPDATE_GOLDEN=1)", path, err)
		}
		if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
			t.Errorf("%s moved:\n--- golden\n%s\n--- now\n%s", name, want, got)
		}
	}
}

// TestTheJournalShowsTheThreeStatesOfAReceipt: a paging delivery whose first
// attempt was refused in a way that is tried again names no receipt; the
// attempt that was accepted names one; and after the recipient is erased the
// fact of the receipt stays while its coordinates are gone. The journal shows
// each as what it is, never an erased receipt as one that never existed.
func TestTheJournalShowsTheThreeStatesOfAReceipt(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	// The API over the same store, and an administrator to ask it.
	a := api.NewAPI(env.S, nil, nil, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)
	if err := env.S.CreateUser(&model.User{
		ID: "root", Email: "root@pipeline.test", Name: "Root", Role: model.UserRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	getAs := func(user, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		token, err := auth.GenerateToken(user)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: api.AuthCookieName, Value: token})
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s as %s: %d %s", path, user, rec.Code, rec.Body.String())
		}
		return rec
	}
	journalOf := func(id string) api.DeliveryJournalResponse {
		t.Helper()
		var resp api.DeliveryJournalResponse
		decodeInto(t, getAs("root", "/api/v1/deliveries/"+id), &resp)
		return resp
	}

	env.Channel.FailWith("S_TEST", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "rate_limited",
	}, nil)
	sendWebhook(t, env.Echo, criticalAlert("receipt_states", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(ctx)
	startOutboundWorker(t, env.Worker)
	until(t, "the first attempt to be refused", func() bool {
		return len(env.Channel.SentTo("S_TEST")) >= 1
	})
	env.Channel.StopFailing("S_TEST")
	waitForNothingOwed(t, env.S, "receipt_states")

	// The critical policy pages one person; the group's card is the other
	// commitment, and not the one under test.
	var id string
	for _, intent := range intentsOf(t, env, "receipt_states") {
		if intent.TargetRef == "U_TEST" {
			id = intent.ID
		}
	}
	if id == "" {
		t.Fatal("no commitment to U_TEST")
	}

	before := journalOf(id)
	if len(before.Attempts) < 2 {
		t.Fatalf("%d attempts, want the refused one and the accepted one", len(before.Attempts))
	}
	refused, accepted := before.Attempts[0], before.Attempts[len(before.Attempts)-1]
	if refused.Outcome == "accepted" || refused.ReceiptRecorded || len(refused.Receipt) != 0 || refused.ReceiptRedactedAt != nil {
		t.Errorf("the refused attempt reads %+v", refused)
	}
	if accepted.Outcome != "accepted" || !accepted.ReceiptRecorded || len(accepted.Receipt) == 0 || accepted.ReceiptRedactedAt != nil {
		t.Errorf("the accepted attempt reads %+v", accepted)
	}
	if !before.Delivery.ReceiptRecorded || before.Delivery.ReceiptRef == "" || before.Delivery.RecipientErased {
		t.Errorf("the delivery reads %+v", before.Delivery)
	}

	// The group's readers see the same delivery with its receipt recorded and
	// without the reference: a user who is not an administrator may read the
	// group, and the reference is an address at the provider.
	bob := testutil.SeedUser(t, env.S, "bob@pipeline.test").ID
	rec := getAs(bob, "/api/v1/alert-groups/"+before.Delivery.AlertGroupID+"/deliveries")
	if bytes.Contains(rec.Body.Bytes(), []byte("receipt_ref")) {
		t.Errorf("the group's deliveries name a receipt reference: %s", rec.Body.String())
	}
	var group api.AlertGroupDeliveriesResponse
	decodeInto(t, rec, &group)
	shown := false
	for _, d := range group.Paging {
		if d.ID == id {
			shown = d.ReceiptRecorded
		}
	}
	if !shown {
		t.Errorf("the group's readers do not see the delivery with its receipt recorded: %+v", group.Paging)
	}

	if err := erasure.NewService(env.S.ErasureRepository()).Erase(ctx, "U_TEST"); err != nil {
		t.Fatalf("erase the recipient: %v", err)
	}
	after := journalOf(id)
	redacted := after.Attempts[len(after.Attempts)-1]
	if !redacted.ReceiptRecorded || len(redacted.Receipt) != 0 || redacted.ReceiptRedactedAt == nil {
		t.Errorf("after the erasure the accepted attempt reads %+v", redacted)
	}
	if !after.Delivery.ReceiptRecorded || after.Delivery.ReceiptRef != "" || !after.Delivery.RecipientErased {
		t.Errorf("after the erasure the delivery reads %+v", after.Delivery)
	}
}
