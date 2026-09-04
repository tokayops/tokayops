package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The channel, against real HTTP servers on the loopback - which is a private
// range, so every test that expects a delivery also exercises the allow list.

const (
	subscriberID = "int-a"
	eventID      = "evt-1"
	eventBody    = `{"event":"alert_group.firing","alert_group":{"id":"ag-1"}}`
)

// configs is the store as the channel sees it, and a test's hand on it.
type configs struct {
	cfg   model.GenericWebhookConfig
	found bool
	err   error
	reads atomic.Int32
	// block, when set, makes the read wait for the context: the budget test.
	block bool
}

func (c *configs) SubscriberConfig(ctx context.Context, id string) (model.GenericWebhookConfig, bool, error) {
	c.reads.Add(1)
	if c.block {
		<-ctx.Done()
		return model.GenericWebhookConfig{}, false, ctx.Err()
	}
	if c.err != nil {
		return model.GenericWebhookConfig{}, false, c.err
	}
	if !c.found {
		return model.GenericWebhookConfig{}, false, nil
	}
	return c.cfg, true, nil
}

func loopback(t *testing.T) []*net.IPNet {
	t.Helper()
	_, lo, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	return []*net.IPNet{lo}
}

func payloadRaw(t *testing.T, subscriber string) []byte {
	t.Helper()
	raw, err := json.Marshal(keys.WebhookPayloadV1{
		Target:    keys.Target{Kind: keys.TargetSubscriber, Ref: subscriber},
		EventID:   eventID,
		EventType: keys.WebhookEventFiring,
		Body:      eventBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func intentFor(t *testing.T, subscriber string) outbound.Intent {
	t.Helper()
	return outbound.Intent{
		ID: "intent-1", KeyKind: keys.KindWebhookEvent, Provider: keys.ProviderWebhook,
		TargetKind: keys.TargetSubscriber, TargetRef: subscriber, Form: outbound.FormOneShot,
		Payload: payloadRaw(t, subscriber), PayloadSchemaVersion: 1,
	}
}

func callTo(t *testing.T, endpoint string) outbound.Call {
	t.Helper()
	return outbound.Call{
		IntentID: "intent-1", AttemptID: "attempt-1", Provider: keys.ProviderWebhook,
		AttemptKind: outbound.AttemptCreate, Operation: outbound.OperationDeliver,
		Endpoint: endpoint, ProviderKey: "create-key",
		KeyKind: keys.KindWebhookEvent, Family: outbound.FamilyWebhook,
		Payload: payloadRaw(t, subscriberID), PayloadSchemaVersion: 1,
	}
}

// received is what a test server saw.
type received struct {
	calls   atomic.Int32
	body    atomic.Value
	headers atomic.Value
}

func serve(t *testing.T, status int, rec *received) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.calls.Add(1)
			body, _ := io.ReadAll(r.Body)
			rec.body.Store(string(body))
			rec.headers.Store(r.Header.Clone())
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("thanks"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAnEventIsPostedAsTheContractSays: the body is the payload's body byte for
// byte, the headers are ours, the signature verifies with the subscriber's
// secret over "<timestamp>.<body>", and the acceptance names no object - which
// the kind allows.
func TestAnEventIsPostedAsTheContractSays(t *testing.T) {
	rec := &received{}
	srv := serve(t, http.StatusOK, rec)
	store := &configs{found: true, cfg: model.GenericWebhookConfig{
		URL: srv.URL, Secret: "s3cret", CustomHeaders: map[string]string{"X-Team": "sre"}}}
	h := NewHandler(store, loopback(t))

	prepared := h.Prepare(context.Background(), intentFor(t, subscriberID))
	if prepared.Outcome() != outbound.PreparationReady {
		t.Fatalf("prepared as %+v", prepared)
	}
	call := callTo(t, srv.URL)
	result, err := h.ExecuteAttempt(context.Background(), call)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	concluded, breach := outbound.Conclude(h, call, result, err)
	if breach != outbound.BreachNone || concluded.Outcome() != outbound.OutcomeAccepted {
		t.Fatalf("a 200 concluded %q with breach %q", concluded.Outcome(), breach)
	}
	if concluded.Completion().ReceiptRefOrEmpty() != "" {
		t.Fatal("a POST named an object")
	}

	if rec.calls.Load() != 1 {
		t.Fatalf("the subscriber was called %d times", rec.calls.Load())
	}
	if got := rec.body.Load().(string); got != eventBody {
		t.Fatalf("the body arrived as\n  %s\nand was\n  %s", got, eventBody)
	}
	headers := rec.headers.Load().(http.Header)
	if headers.Get(HeaderEvent) != "alert_group.firing" || headers.Get(HeaderEventID) != eventID ||
		headers.Get(HeaderContentType) != "application/json" || headers.Get("X-Team") != "sre" {
		t.Fatalf("headers %v", headers)
	}
	ts := headers.Get(HeaderTimestamp)
	if _, err := strconv.ParseInt(ts, 10, 64); err != nil {
		t.Fatalf("timestamp %q", ts)
	}
	if want := "sha256=" + Sign(ts, []byte(eventBody), "s3cret"); headers.Get(HeaderSignature) != want {
		t.Fatalf("signature %q, want %q", headers.Get(HeaderSignature), want)
	}
}

// TestOurHeadersWinOverTheSubscribers: a configuration that names our headers -
// saved before the check on the way in existed, in any case - loses at the
// request. All five of them, in mixed case.
func TestOurHeadersWinOverTheSubscribers(t *testing.T) {
	rec := &received{}
	srv := serve(t, http.StatusOK, rec)
	store := &configs{found: true, cfg: model.GenericWebhookConfig{
		URL: srv.URL, Secret: "s3cret", CustomHeaders: map[string]string{
			"x-tokay-event-id":  "forged",
			"X-TOKAY-EVENT":     "forged",
			"X-Tokay-Timestamp": "0",
			"x-tokay-signature": "sha256=forged",
			"content-type":      "text/plain",
			"X-Real":            "kept",
		}}}
	h := NewHandler(store, loopback(t))
	if _, err := h.ExecuteAttempt(context.Background(), callTo(t, srv.URL)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	headers := rec.headers.Load().(http.Header)
	if headers.Get(HeaderEventID) != eventID || headers.Get(HeaderEvent) != "alert_group.firing" ||
		headers.Get(HeaderContentType) != "application/json" || headers.Get(HeaderTimestamp) == "0" ||
		headers.Get(HeaderSignature) == "sha256=forged" {
		t.Fatalf("a subscriber's configuration replaced our headers: %v", headers)
	}
	if headers.Get("X-Real") != "kept" {
		t.Fatal("an ordinary custom header was dropped")
	}
	for _, name := range []string{"x-tokay-event-id", "X-Tokay-Event-ID", "Content-Type", "content-type", "X-TOKAY-Anything"} {
		if !IsReservedHeader(name) {
			t.Errorf("%s is not reserved", name)
		}
	}
	if IsReservedHeader("X-Team") || IsReservedHeader("Authorization") {
		t.Error("an ordinary header is reserved")
	}
}

// TestTheSecretIsReadAgainAndTheAddressIsNot: between the preparation and the
// call the subscriber rotated its secret and moved its URL. The signature uses
// the new secret; the POST goes to the address the generation is bound to.
func TestTheSecretIsReadAgainAndTheAddressIsNot(t *testing.T) {
	bound := &received{}
	boundSrv := serve(t, http.StatusOK, bound)
	moved := &received{}
	movedSrv := serve(t, http.StatusOK, moved)

	store := &configs{found: true, cfg: model.GenericWebhookConfig{URL: boundSrv.URL, Secret: "old"}}
	h := NewHandler(store, loopback(t))
	prepared := h.Prepare(context.Background(), intentFor(t, subscriberID))
	if prepared.Outcome() != outbound.PreparationReady {
		t.Fatalf("prepared as %+v", prepared)
	}

	store.cfg = model.GenericWebhookConfig{URL: movedSrv.URL, Secret: "new"}
	if _, err := h.ExecuteAttempt(context.Background(), callTo(t, boundSrv.URL)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if bound.calls.Load() != 1 || moved.calls.Load() != 0 {
		t.Fatalf("the bound address was called %d times, the moved one %d",
			bound.calls.Load(), moved.calls.Load())
	}
	headers := bound.headers.Load().(http.Header)
	ts := headers.Get(HeaderTimestamp)
	if headers.Get(HeaderSignature) != "sha256="+Sign(ts, []byte(eventBody), "new") {
		t.Fatal("the request was signed with the secret the preparation saw, not the current one")
	}
	if store.reads.Load() != 2 {
		t.Fatalf("the configuration was read %d times, once per half is 2", store.reads.Load())
	}
}

// TestTheSubscribersAnswerIsClassifiedByRange: through Conclude, with the real
// handler, over the whole rule and not a list of examples - 409, 418 and 499 are
// refusals like 400 is, every 3xx is a redirect not followed, and 5xx is left
// to the domain, which answers doubt.
func TestTheSubscribersAnswerIsClassifiedByRange(t *testing.T) {
	cases := []struct {
		code    int
		outcome outbound.Outcome
		class   string
	}{
		{200, outbound.OutcomeAccepted, ""}, {201, outbound.OutcomeAccepted, ""}, {202, outbound.OutcomeAccepted, ""},
		{204, outbound.OutcomeAccepted, ""}, {226, outbound.OutcomeAccepted, ""},
		{429, outbound.OutcomeRetryableRejection, "rate_limited"},
		{408, outbound.OutcomeRetryableRejection, "request_timeout"},
		{301, outbound.OutcomePermanentRejection, "redirect_not_followed"},
		{302, outbound.OutcomePermanentRejection, "redirect_not_followed"},
		{307, outbound.OutcomePermanentRejection, "redirect_not_followed"},
		{308, outbound.OutcomePermanentRejection, "redirect_not_followed"},
		{400, outbound.OutcomePermanentRejection, "rejected_4xx"},
		{401, outbound.OutcomePermanentRejection, "rejected_4xx"},
		{404, outbound.OutcomePermanentRejection, "rejected_4xx"},
		{409, outbound.OutcomePermanentRejection, "rejected_4xx"},
		{418, outbound.OutcomePermanentRejection, "rejected_4xx"},
		{499, outbound.OutcomePermanentRejection, "rejected_4xx"},
		{500, outbound.OutcomeAmbiguous, "unknown_status"},
		{502, outbound.OutcomeAmbiguous, "unknown_status"},
		{599, outbound.OutcomeAmbiguous, "unknown_status"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			h := NewHandler(&configs{found: true}, loopback(t))
			result := outbound.Result{Evidence: outbound.ProviderResponse, Status: strconv.Itoa(tc.code)}
			concluded, breach := outbound.Conclude(h, callTo(t, "https://example.com"), result, nil)
			if breach != outbound.BreachNone {
				t.Fatalf("breach %q", breach)
			}
			if concluded.Outcome() != tc.outcome {
				t.Errorf("HTTP %d concluded %q, want %q", tc.code, concluded.Outcome(), tc.outcome)
			}
			class := ""
			if concluded.Completion().ErrorClass != nil {
				class = *concluded.Completion().ErrorClass
			}
			if class != tc.class {
				t.Errorf("HTTP %d classified %q, want %q", tc.code, class, tc.class)
			}
		})
	}
	// Every status there is, 100 through 599, and not the examples above: an
	// implementation that accepted 200, 202 and 204 by name would pass them and
	// refuse a 201, which is as good an acceptance as any. Nothing below 200 is
	// an answer to the request and nothing from 500 up is the channel's to say;
	// both are left to the domain, whose answer is doubt.
	h := NewHandler(&configs{found: true}, loopback(t))
	for code := 100; code < 600; code++ {
		got, known := h.ClassifyResponse(outbound.Result{Status: strconv.Itoa(code)})
		switch {
		case code < 200:
			if known {
				t.Errorf("HTTP %d was classified by the channel: %+v", code, got)
			}
		case code < 300:
			if !known || got.Outcome != outbound.OutcomeAccepted || got.Class != "" {
				t.Errorf("HTTP %d: %+v %v, want acceptance", code, got, known)
			}
		case code < 400:
			if !known || got.Outcome != outbound.OutcomePermanentRejection || got.Class != "redirect_not_followed" {
				t.Errorf("HTTP %d: %+v %v", code, got, known)
			}
		case code == 408 || code == 429:
			if !known || got.Outcome != outbound.OutcomeRetryableRejection {
				t.Errorf("HTTP %d: %+v %v", code, got, known)
			}
		case code < 500:
			if !known || got.Outcome != outbound.OutcomePermanentRejection || got.Class != "rejected_4xx" {
				t.Errorf("HTTP %d: %+v %v", code, got, known)
			}
		default:
			if known {
				t.Errorf("HTTP %d was classified by the channel: %+v", code, got)
			}
		}
	}
	if _, known := h.ClassifyResponse(outbound.Result{Status: "ok"}); known {
		t.Error("a status that is not a number was classified")
	}
}

// TestARedirectIsNotFollowed: the subscriber answers 301 to a working address;
// that address is never called, the 301 is the answer, and it is a refusal.
func TestARedirectIsNotFollowed(t *testing.T) {
	final := &received{}
	finalSrv := serve(t, http.StatusOK, final)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalSrv.URL, http.StatusMovedPermanently)
	}))
	t.Cleanup(redirect.Close)

	h := NewHandler(&configs{found: true, cfg: model.GenericWebhookConfig{URL: redirect.URL}}, loopback(t))
	call := callTo(t, redirect.URL)
	result, err := h.ExecuteAttempt(context.Background(), call)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Status != "301" {
		t.Fatalf("the answer recorded is %q, want the redirect itself", result.Status)
	}
	if final.calls.Load() != 0 {
		t.Fatalf("the redirect was followed: the final address was called %d times", final.calls.Load())
	}
	concluded, _ := outbound.Conclude(h, call, result, err)
	if concluded.Outcome() != outbound.OutcomePermanentRejection {
		t.Fatalf("a redirect concluded %q", concluded.Outcome())
	}
}

// TestPreparationRefusesWhatWillNotChangeAndWaitsForWhatMight: the closed set
// of preparation answers, each for its reason.
func TestPreparationRefusesWhatWillNotChangeAndWaitsForWhatMight(t *testing.T) {
	public := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
	mixed := func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.7")}, nil
	}
	nxdomain := func(_ context.Context, host string) ([]net.IP, error) {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}

	cases := []struct {
		name    string
		store   *configs
		resolve Resolver
		intent  func(outbound.Intent) outbound.Intent
		outcome outbound.PreparationOutcome
		class   string
	}{
		{"a subscriber that exists", &configs{found: true, cfg: model.GenericWebhookConfig{URL: "https://hooks.example.com/a"}},
			public, nil, outbound.PreparationReady, ""},
		{"no such subscriber", &configs{found: false}, public, nil, outbound.PreparationPermanent, "integration_missing"},
		{"the database failed", &configs{err: errors.New("connection reset")}, public, nil,
			outbound.PreparationTransient, "config_read_failed"},
		{"no URL", &configs{found: true}, public, nil, outbound.PreparationPermanent, "url_missing"},
		{"an unusable URL", &configs{found: true, cfg: model.GenericWebhookConfig{URL: "::not a url"}}, public, nil,
			outbound.PreparationPermanent, "url_invalid"},
		{"one private address among public ones", &configs{found: true, cfg: model.GenericWebhookConfig{URL: "https://hooks.example.com/a"}},
			mixed, nil, outbound.PreparationPermanent, "ip_policy"},
		{"a name that does not resolve", &configs{found: true, cfg: model.GenericWebhookConfig{URL: "https://hooks.example.com/a"}},
			nxdomain, nil, outbound.PreparationTransient, "dns"},
		{"a payload for another subscriber", &configs{found: true}, public,
			func(i outbound.Intent) outbound.Intent { i.TargetRef = "int-b"; return i },
			outbound.PreparationPermanent, "target_mismatch"},
		{"a payload that does not read", &configs{found: true}, public,
			func(i outbound.Intent) outbound.Intent { i.Payload = []byte(`{"body":`); return i },
			outbound.PreparationPermanent, "payload_unreadable"},
		{"a kind this channel does not deliver", &configs{found: true}, public,
			func(i outbound.Intent) outbound.Intent { i.KeyKind = keys.KindHandoff; return i },
			outbound.PreparationPermanent, "unsupported_kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandlerResolving(tc.store, nil, tc.resolve)
			intent := intentFor(t, subscriberID)
			if tc.intent != nil {
				intent = tc.intent(intent)
			}
			prepared := h.Prepare(context.Background(), intent)
			req := prepared.Request(intent.ID, "lease", "worker")
			if req.Preparation != tc.outcome || req.ErrorClass != tc.class {
				t.Fatalf("prepared as %s / %q, want %s / %q", req.Preparation, req.ErrorClass, tc.outcome, tc.class)
			}
			if tc.outcome == outbound.PreparationReady && req.BoundEndpoint != tc.store.cfg.URL {
				t.Fatalf("bound to %q", req.BoundEndpoint)
			}
		})
	}
}

// TestAConfigurationThatCannotBeReadIsARequestThatNeverLeft: inside the attempt,
// the database failing, the subscriber gone, or the read outliving its budget is
// evidence of absence - the request was never built - and not the doubt an
// unrecognised error would otherwise become.
func TestAConfigurationThatCannotBeReadIsARequestThatNeverLeft(t *testing.T) {
	rec := &received{}
	srv := serve(t, http.StatusOK, rec)
	for name, store := range map[string]*configs{
		"the database failed":      {err: errors.New("connection reset")},
		"the subscriber is gone":   {found: false},
		"the read outlives budget": {block: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := NewHandler(store, loopback(t))
			h.budget = 50 * time.Millisecond
			started := time.Now()
			result, err := h.ExecuteAttempt(context.Background(), callTo(t, srv.URL))
			if err == nil {
				t.Fatal("a call with no configuration went out")
			}
			if result.Evidence != outbound.DefinitelyNotSent {
				t.Fatalf("recorded as %q, want definitely not sent", result.Evidence)
			}
			if store.block && time.Since(started) > time.Second {
				t.Fatalf("the read was not bounded by its budget: %s", time.Since(started))
			}
		})
	}
	if rec.calls.Load() != 0 {
		t.Fatalf("the subscriber was called %d times without a configuration", rec.calls.Load())
	}
}

// TestAnAddressThatMovedIntoAPrivateRangeIsBlockedAtTheSocket: the preparation
// saw a public address and bound it; by the dial the name answers with a private
// one. The dial refuses, the request provably never left, and the next
// preparation will refuse for good.
func TestAnAddressThatMovedIntoAPrivateRangeIsBlockedAtTheSocket(t *testing.T) {
	rec := &received{}
	srv := serve(t, http.StatusOK, rec)
	var answers atomic.Int32
	flipping := func(context.Context, string) ([]net.IP, error) {
		if answers.Add(1) == 1 {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil // preparation: public
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil // dial: private
	}
	store := &configs{found: true, cfg: model.GenericWebhookConfig{URL: "http://moved.example.com/hook"}}
	h := NewHandlerResolving(store, nil, flipping)

	if prepared := h.Prepare(context.Background(), intentFor(t, subscriberID)); prepared.Outcome() != outbound.PreparationReady {
		t.Fatalf("prepared as %+v", prepared)
	}
	result, err := h.ExecuteAttempt(context.Background(), callTo(t, "http://moved.example.com"+srv.URL[strings.LastIndex(srv.URL, ":"):]+"/hook"))
	if err == nil {
		t.Fatal("a request to a blocked address went out")
	}
	if !errors.Is(err, ErrBlockedAddress) || result.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("recorded as %q: %v", result.Evidence, err)
	}
	if rec.calls.Load() != 0 {
		t.Fatal("the private address was reached")
	}
	if prepared := h.Prepare(context.Background(), intentFor(t, subscriberID)); prepared.Outcome() != outbound.PreparationPermanent {
		t.Fatalf("the next preparation did not refuse for good: %+v", prepared)
	}
}

// TestASubscriberIsGivenItsTimeoutAndNoMore: a subscriber that never answers is
// doubt after ITS timeout, not after the attempt's; and a saved timeout above
// the ceiling is clamped, at the read before the call.
func TestASubscriberIsGivenItsTimeoutAndNoMore(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body is read first: until it is, the server does not watch the
		// connection and would never see the client give up. Bounded either
		// way, so a test that is wrong cannot hang the run.
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hang.Close)
	store := &configs{found: true, cfg: model.GenericWebhookConfig{URL: hang.URL, TimeoutSeconds: 1}}
	h := NewHandler(store, loopback(t))
	started := time.Now()
	result, err := h.ExecuteAttempt(context.Background(), callTo(t, hang.URL))
	if err == nil {
		t.Fatal("a subscriber that never answered was accepted")
	}
	if result.Evidence != outbound.PossiblySent {
		t.Fatalf("a timeout after the request went out was recorded as %q", result.Evidence)
	}
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("the call took %s for a one-second timeout", elapsed)
	}

	for seconds, want := range map[int]time.Duration{
		0: 30 * time.Second, 10: 10 * time.Second, 30: 30 * time.Second,
		45: 30 * time.Second, 60: 30 * time.Second,
	} {
		if got := EffectiveTimeout(model.GenericWebhookConfig{TimeoutSeconds: seconds}); got != want {
			t.Errorf("timeout_seconds=%d gives %s, want %s", seconds, got, want)
		}
	}
}

// TestTheSignatureIsTheOneSubscribersAlreadyVerify pins the algorithm the
// documentation describes - HMAC-SHA256 over "<timestamp>.<body>", hex - so the
// move from the old worker changed nothing a subscriber checks. The vector was
// computed independently of this code.
func TestTheSignatureIsTheOneSubscribersAlreadyVerify(t *testing.T) {
	const want = "3defdef6e81edf86cf66b409ee5c4e4a970a624585ef5ab39c4f08187a091bcc"
	if got := Sign("1700000000", []byte(`{"event":"alert_group.firing"}`), "s3cret"); got != want {
		t.Fatalf("the signature is %s, subscribers verify %s", got, want)
	}
}

// TestOnlyAPublicAddressMayBePostedTo: the policy is what an address IS, not a
// list of what it is not. Every kind of address that is not globally reachable
// is refused - at the preparation, where it ends the commitment, and at the
// socket, where a name that moved is caught - and the allowlist is the one way
// through.
func TestOnlyAPublicAddressMayBePostedTo(t *testing.T) {
	blocked := []string{
		"10.0.0.7", "172.16.0.1", "192.168.1.1", // private
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "fe80::1", // link-local: where the metadata service lives
		"fc00::1", "fd12:3456::1", // unique-local
		"0.0.0.0", "::", "0.1.2.3", // unspecified, and "this" network
		"100.64.0.1",                                                          // shared address space
		"192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", // IETF, documentation, benchmarking
		"224.0.0.1", "239.255.255.250", "ff02::1", // multicast
		"240.0.0.1", "255.255.255.255", // reserved, and broadcast
		"::ffff:10.0.0.7", "::ffff:127.0.0.1", // IPv4 written as IPv6
		"64:ff9b::a00:7", // 10.0.0.7 through NAT64
		"2002:a00:7::",   // 10.0.0.7 through 6to4
		"100::1",         // discard-only
		// IPv6 outside the allocated global-unicast third: refused by the
		// positive rule, not by being named.
		"64:ff9b:1::1",       // local-use NAT64 (RFC 8215): embeds an IPv4 nobody can find
		"100:0:0:1::1",       // beside the discard prefix
		"5f00::1",            // SRv6 SID space
		"fec0::1",            // site-local, deprecated but routed by old gear
		"4000::1", "a000::1", // unallocated
		"::2", // beside the unspecified address
		// Inside IANA's own 2001::/23, which is allocated to no registry: only
		// its globally reachable assignments are public, and these are not them.
		"2001::1",     // Teredo
		"2001:2::1",   // benchmarking
		"2001:10::1",  // deprecated ORCHID
		"2001:5::1",   // no assignment at all
		"3fff::1",     // documentation, RESERVED in the unicast registry
		"2001:db8::1", // documentation, and it lives INSIDE an allocated block
		"192.88.99.1", // 6to4 relay anycast, deprecated
		// Inside 2000::/3 and NOT in the IANA unicast assignments registry:
		// the space allocations are made from is not the allocation.
		"3ffe::1",      // RESERVED - the retired 6bone
		"2200::1",      // unassigned
		"21ff:ffff::1", // unassigned, right below the first allocation
	}
	// Every one of these is a subscriber somebody could really have, and a
	// block dropped from the list is a public subscriber answered with
	// permanent_failed before a request is made - a support call, not an
	// outage anyone can see. So: one address per allocated block, and every
	// assignment the special-purpose registry marks globally reachable.
	public := []string{"93.184.216.34", "8.8.8.8", "2606:4700::1111", "2001:4860:4860::8888",
		"::ffff:93.184.216.34", "64:ff9b::5db8:d822", "2002:5db8:d822::",
		"2a00:1450:4001:80f::200e",
		// Globally reachable special-purpose, all of it.
		"2001:1::1", "2001:1::2", "2001:1::3", // PCP, TURN and DNS-SD SRP anycasts
		"2001:3::1", "2001:4:112::1", // AMT, AS112-v6
		"2001:20::1", "2001:30::1", // ORCHIDv2, drone remote ID
		"2620:4f:8000::1", // Direct Delegation AS112, inside ARIN's 2620::/23
		// One per allocated block of the unicast registry, in its order.
		"2001:200::1", "2001:400::1", "2001:600::1", "2001:800::1",
		"2001:c00::1", "2001:e00::1", "2001:1200::1", "2001:1400::1",
		"2001:1800::1", "2001:1a00::1", "2001:1c00::1", "2001:2000::1",
		"2001:3c00::1", // the /19 is allocated WHOLE: this corner is not free
		"2001:4000::1", "2001:4200::1", "2001:4400::1", "2001:4600::1",
		"2001:4800::1", "2001:4a00::1", "2001:4c00::1", "2001:5000::1",
		"2001:8000::1", "2001:a000::1", "2001:b000::1", "2003::1",
		"2400::1", "2410::1", // 2410::/12 is allocated to APNIC like 2400::/12
		"2600::1", "2610::1", "2620::1", "2630::1", "2800::1",
		"2a00::1", "2a10::1", "2c00::1"}
	none := ipPolicy{}
	for _, a := range blocked {
		if none.allowedIP(net.ParseIP(a)) {
			t.Errorf("%s was allowed", a)
		}
	}
	for _, a := range public {
		if !none.allowedIP(net.ParseIP(a)) {
			t.Errorf("%s was refused", a)
		}
	}
	if none.allowedIP(nil) {
		t.Error("no address at all was allowed")
	}

	// The allowlist is the one way through, judged over the address itself:
	// an allowed IPv4 range covers the address however it is written.
	allowing := ipPolicy{allowed: cidrs(t, "fc00::/7", "10.0.0.0/8")}
	for _, a := range []string{"fc00::1", "10.0.0.7", "::ffff:10.0.0.7", "64:ff9b::a00:7"} {
		if !allowing.allowedIP(net.ParseIP(a)) {
			t.Errorf("%s was refused with its range allowed", a)
		}
	}
	for _, a := range []string{"100.64.0.1", "172.16.0.1", "::"} {
		if allowing.allowedIP(net.ParseIP(a)) {
			t.Errorf("allowing two ranges opened %s", a)
		}
	}

	// A subscriber at an allocated address is PREPARED, not refused. This is
	// the point where a block missing from the list turns a working public
	// subscriber into permanent_failed with no request made - which is how
	// 2410::/12 and the recovered corner of 2001:2000::/19 were found.
	for _, a := range []string{"2410::1", "2410:1000:2::5", "2001:3c00::1", "2001:2000::1", "2001:1::3"} {
		address := a
		t.Run("a subscriber at "+address+" is prepared", func(t *testing.T) {
			resolve := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP(address)}, nil }
			cfg := model.GenericWebhookConfig{URL: "https://hooks.example.com/a"}
			h := NewHandlerResolving(&configs{found: true, cfg: cfg}, nil, resolve)
			intent := intentFor(t, subscriberID)
			req := h.Prepare(context.Background(), intent).Request(intent.ID, "lease", "worker")
			if req.Preparation != outbound.PreparationReady || req.BoundEndpoint != cfg.URL {
				t.Fatalf("prepared as %s / %q: a public subscriber was refused", req.Preparation, req.ErrorClass)
			}
		})
	}

	// Both points, for addresses one of the two lists this replaced let
	// through - the second batch is the space outside 2000::/3 that the first
	// rewrite still answered "public" for.
	for _, a := range []string{"fc00::1", "::", "100.64.0.1", "64:ff9b::a00:7", "::ffff:10.0.0.7",
		"64:ff9b:1::1", "5f00::1", "fec0::1", "4000::1",
		"3ffe::1", "2200::1"} {
		address := a
		t.Run("at the preparation: "+address, func(t *testing.T) {
			resolve := func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP(address)}, nil }
			h := NewHandlerResolving(&configs{found: true, cfg: model.GenericWebhookConfig{URL: "https://hooks.example.com/a"}}, nil, resolve)
			intent := intentFor(t, subscriberID)
			req := h.Prepare(context.Background(), intent).Request(intent.ID, "lease", "worker")
			if req.Preparation != outbound.PreparationPermanent || req.ErrorClass != "ip_policy" {
				t.Fatalf("prepared as %s / %q, want a permanent ip_policy refusal", req.Preparation, req.ErrorClass)
			}
		})
		t.Run("at the socket: "+address, func(t *testing.T) {
			rec := &received{}
			srv := serve(t, http.StatusOK, rec)
			var answers atomic.Int32
			flipping := func(context.Context, string) ([]net.IP, error) {
				if answers.Add(1) == 1 {
					return []net.IP{net.ParseIP("93.184.216.34")}, nil // preparation: public
				}
				return []net.IP{net.ParseIP(address)}, nil // dial: moved
			}
			store := &configs{found: true, cfg: model.GenericWebhookConfig{URL: "http://moved.example.com/hook"}}
			h := NewHandlerResolving(store, nil, flipping)
			if prepared := h.Prepare(context.Background(), intentFor(t, subscriberID)); prepared.Outcome() != outbound.PreparationReady {
				t.Fatalf("prepared as %+v", prepared)
			}
			result, err := h.ExecuteAttempt(context.Background(),
				callTo(t, "http://moved.example.com"+srv.URL[strings.LastIndex(srv.URL, ":"):]+"/hook"))
			if !errors.Is(err, ErrBlockedAddress) || result.Evidence != outbound.DefinitelyNotSent {
				t.Fatalf("recorded as %q: %v", result.Evidence, err)
			}
			if rec.calls.Load() != 0 {
				t.Fatal("the address was reached")
			}
		})
	}
}

func cidrs(t *testing.T, list ...string) []*net.IPNet {
	t.Helper()
	nets := make([]*net.IPNet, 0, len(list))
	for _, c := range list {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		nets = append(nets, n)
	}
	return nets
}
