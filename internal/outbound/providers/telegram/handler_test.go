package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// The Telegram channel as the outbound worker uses it.

func handlerState(t *testing.T) keys.RenderSnapshot {
	t.Helper()
	groupURL := "https://tokay.example/#/ops/alert-groups/ag-1"
	state, err := keys.NewRenderSnapshot(keys.SnapshotInput{
		AlertGroupID: "ag-1", Status: keys.GroupTriggered, Title: "Disk filling up",
		Severity: "critical", TeamOnboarded: true, GroupURL: &groupURL,
		DisplayTimezone: "UTC",
		Alerts: []keys.AlertSnapshot{{
			Fingerprint: "fp-1", Status: keys.AlertFiring,
			StartsAt:  time.Unix(1700000000, 0).UTC(),
			AlertName: "DiskWillFill", Severity: "critical",
		}},
	})
	if err != nil {
		t.Fatalf("build the state: %v", err)
	}
	return state
}

func handlerCall(t *testing.T, kind keys.TargetKind, interactive bool) outbound.Call {
	t.Helper()
	payload, err := json.Marshal(keys.EscalationPayloadV1{
		Slot:   keys.Slot{Kind: keys.SlotFirehose},
		Target: keys.Target{Kind: kind, Ref: "-1001"},
		// Direct messages carry the escalation's own words.
		Interactive: interactive,
	})
	if err != nil {
		t.Fatalf("build the payload: %v", err)
	}
	return outbound.Call{
		IntentID: "intent-1", AttemptID: "attempt-1", Provider: "telegram",
		AttemptKind: outbound.AttemptCreate, Operation: outbound.OperationSend,
		Endpoint: "-1001", ProviderKey: "create-key",
		State: handlerState(t), Payload: payload,
		PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
	}
}

type botAPI struct {
	server *httptest.Server
	calls  []map[string]any
	answer func(body map[string]any) (int, map[string]any)
}

func newBotAPI(t *testing.T) *botAPI {
	t.Helper()
	api := &botAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		body["_method"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		api.calls = append(api.calls, body)

		w.Header().Set("Content-Type", "application/json")
		if api.answer != nil {
			if status, reply := api.answer(body); reply != nil {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(reply)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": 42,
				"chat":       map[string]any{"id": -1001},
			},
		})
	}))
	t.Cleanup(api.server.Close)
	return api
}

func handlerFor(api *botAPI) *Handler {
	return NewHandler(&mockTelegramTokenSource{token: "tok"}, nil,
		WithHandlerBaseURL(api.server.URL))
}

// TestOneAttemptIsOneCall: Telegram has no thread and no permalink, so an
// attempt is exactly one sendMessage and the answer is already the receipt.
func TestOneAttemptIsOneCall(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	result, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.TargetChannel, true))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(api.calls) != 1 || api.calls[0]["_method"] != "sendMessage" {
		t.Fatalf("the attempt made %d calls: %+v", len(api.calls), api.calls)
	}
	if result.Status != "ok" || !result.Receipt.Recorded() {
		t.Fatalf("the send answered %+v", result)
	}
	if ref := result.Receipt.Ref(); ref != "-1001/42" {
		t.Fatalf("the receipt names %q", ref)
	}

	text, _ := api.calls[0]["text"].(string)
	if !strings.Contains(text, "Disk filling up") {
		t.Fatalf("the card does not say what happened: %q", text)
	}
	if _, hasKeyboard := api.calls[0]["reply_markup"]; !hasKeyboard {
		t.Fatal("a delivery admitted with buttons sent none")
	}
}

// TestButtonsFollowTheAdmission. An empty keyboard is not the same as no
// keyboard, and the difference has to survive into the request.
func TestButtonsFollowTheAdmission(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	if _, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.TargetChannel, false)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	markup, ok := api.calls[0]["reply_markup"].(map[string]any)
	if !ok {
		t.Fatalf("expected an empty keyboard, got %v", api.calls[0]["reply_markup"])
	}
	if rows, _ := markup["inline_keyboard"].([]any); len(rows) != 0 {
		t.Fatalf("a delivery admitted without buttons sent %d rows", len(rows))
	}
}

// TestTelegramAnswersAreTranslatedNotInterpreted walks the capability matrix.
func TestTelegramAnswersAreTranslatedNotInterpreted(t *testing.T) {
	handler := &Handler{}

	cases := map[string]struct {
		want  outbound.Outcome
		known bool
	}{
		"ok":                                    {outbound.OutcomeAccepted, true},
		"429:Too Many Requests: retry after 30": {outbound.OutcomeRetryableRejection, true},
		"403:Forbidden: bot was blocked by the user": {outbound.OutcomePermanentRejection, true},
		"400:Bad Request: chat not found":            {outbound.OutcomePermanentRejection, true},
		"400:Bad Request: message is too long":       {outbound.OutcomePermanentRejection, true},
		"400:Bad Request: PEER_ID_INVALID":           {outbound.OutcomePermanentRejection, true},
		"500:Internal Server Error":                  {outbound.OutcomeAmbiguous, true},
		"502:Bad Gateway":                            {outbound.OutcomeAmbiguous, true},
		// A 400 nobody recognised is not proof of anything.
		"400:Bad Request: something new": {"", false},
		"418:I am a teapot":              {"", false},
		"":                               {"", false},
	}

	for status, want := range cases {
		outcome, _, known := handler.ClassifyResponse(outbound.Result{
			Evidence: outbound.ProviderResponse, Status: status,
		})
		if known != want.known {
			t.Errorf("status %q: known=%v, want %v", status, known, want.known)
			continue
		}
		if known && outcome != want.want {
			t.Errorf("status %q classified %q, want %q", status, outcome, want.want)
		}
	}
}

// TestARejectedSendCarriesWhatTelegramSaid: the code and the sentence both
// reach the journal, because the sentence is what the classification reads and
// what a person reads afterwards.
func TestARejectedSendCarriesWhatTelegramSaid(t *testing.T) {
	api := newBotAPI(t)
	api.answer = func(map[string]any) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"ok": false, "error_code": 403,
			"description": "Forbidden: bot was blocked by the user",
		}
	}
	handler := handlerFor(api)

	result, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.TargetChannel, true))
	if err == nil {
		t.Fatal("a refusal was reported as a success")
	}
	if result.Evidence != outbound.ProviderResponse {
		t.Fatalf("Telegram answered and it was recorded as %q", result.Evidence)
	}
	outcome, _, known := handler.ClassifyResponse(result)
	if !known || outcome != outbound.OutcomePermanentRejection {
		t.Fatalf("a blocked bot classified %q (known=%v)", outcome, known)
	}
	if !strings.Contains(result.Summary, "blocked") {
		t.Fatalf("the journal does not say why: %q", result.Summary)
	}
}

// TestPreparationResolvesAPersonEveryAttempt.
func TestPreparationResolvesAPersonEveryAttempt(t *testing.T) {
	handler := NewHandler(&mockTelegramTokenSource{token: "tok"},
		func(context.Context, string, string) (string, error) { return "5551", nil })

	prepared := handler.Prepare(context.Background(),
		intentFor(t, keys.Target{Kind: keys.TargetUser, Ref: "user-1"}))
	if prepared.Outcome() != outbound.PreparationReady {
		t.Fatalf("a linked person was refused: %+v", prepared)
	}
	if got := prepared.Request("i", "t", "w").BoundEndpoint; got != "5551" {
		t.Fatalf("the resolved chat is %q", got)
	}

	unlinked := NewHandler(&mockTelegramTokenSource{token: "tok"},
		func(context.Context, string, string) (string, error) {
			return "", providers.ErrNotLinked
		})
	if got := unlinked.Prepare(context.Background(),
		intentFor(t, keys.Target{Kind: keys.TargetUser, Ref: "user-1"})).Outcome(); got != outbound.PreparationPermanent {
		t.Fatalf("an unlinked person was %s", got)
	}
}

// TestTheBotTokenNeverReachesTheJournal.
//
// The Bot API puts the token in the path, and net/http puts the whole address
// into every transport error it returns. Those errors are written into a
// delivery's summary, which is a durable row people read - so one unreachable
// Telegram host would write the installation's bot token into the journal,
// where it would stay. Truncation does not help: the token is at the front.
func TestTheBotTokenNeverReachesTheJournal(t *testing.T) {
	const token = "0000000000:SECRET-BOT-TOKEN-DO-NOT-STORE"

	// A port that was listening a moment ago and is not any more.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	handler := NewHandler(&mockTelegramTokenSource{token: token}, nil,
		WithHandlerBaseURL("http://"+address))

	result, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.TargetChannel, true))
	if err == nil {
		t.Fatal("a dead address answered")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token is in the error the worker logs: %v", err)
	}
	if strings.Contains(result.Summary, token) {
		t.Fatalf("the token is in the summary the journal keeps: %q", result.Summary)
	}

	// And the cause survives, because what the cause IS decides whether the
	// message might have gone out.
	if result.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("a refused connection was classified %q", result.Evidence)
	}
}

func intentFor(t *testing.T, target keys.Target) outbound.Intent {
	t.Helper()
	payload, err := json.Marshal(keys.EscalationPayloadV1{
		Slot: keys.Slot{Kind: keys.SlotFirehose}, Target: target, Interactive: true,
	})
	if err != nil {
		t.Fatalf("build the payload: %v", err)
	}
	return outbound.Intent{
		ID: "intent-1", Provider: "telegram",
		TargetKind: target.Kind, TargetRef: target.Ref,
		PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		Payload:              payload,
	}
}

// TestAPayloadNobodyCanReadStopsBeforeTheNetwork: the same rule as the other
// channel, because the cost is the same - an attempt that never touched the
// network, retried for as long as the commitment lives.
func TestAPayloadNobodyCanReadStopsBeforeTheNetwork(t *testing.T) {
	handler := NewHandler(&mockTelegramTokenSource{token: "tok"}, nil)

	intent := intentFor(t, keys.Target{Kind: keys.TargetChannel, Ref: "-1001"})
	intent.Payload = []byte(`{"target":{"kind":"chan`)

	prepared := handler.Prepare(context.Background(), intent)
	if prepared.Outcome() != outbound.PreparationPermanent {
		t.Fatalf("an unreadable payload was %s, which retries forever", prepared.Outcome())
	}
	if got := prepared.Request("i", "t", "w").ErrorClass; got != "payload_unreadable" {
		t.Fatalf("the refusal says %q", got)
	}
}

// TestADirectMessageIsPlainText.
//
// The card is HTML this package built and escaped. A direct message is
// somebody's own words: marking those as HTML makes an ordinary "a < b", or an
// unclosed tag in an operator's note, unsendable - the message fails to go out
// for the shape of its text, which is not something the delivery can fix by
// retrying.
func TestADirectMessageIsPlainText(t *testing.T) {
	api := newBotAPI(t)
	handler := handlerFor(api)

	override := "disk on db-1 is at 95% <check the graph> & escalate"
	payload, err := json.Marshal(keys.EscalationPayloadV1{
		Slot:            keys.Slot{Kind: keys.SlotPolicy, Index: 1},
		Target:          keys.Target{Kind: keys.TargetUser, Ref: "user-1"},
		MessageOverride: &override,
		Interactive:     true,
	})
	if err != nil {
		t.Fatalf("build the payload: %v", err)
	}

	call := handlerCall(t, keys.TargetUser, true)
	call.Payload = payload
	call.Endpoint = "5551"
	if _, err := handler.ExecuteAttempt(context.Background(), call); err != nil {
		t.Fatalf("execute: %v", err)
	}

	sent := api.calls[0]
	if got, _ := sent["text"].(string); got != override {
		t.Fatalf("the message was rewritten: %q", got)
	}
	if mode, marked := sent["parse_mode"]; marked {
		t.Fatalf("a person's own words were sent as %v", mode)
	}
	if _, hasKeyboard := sent["reply_markup"]; hasKeyboard {
		t.Fatal("a direct message carried buttons")
	}
}
