package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// The channel as the outbound worker uses it: one call per attempt, an address
// it does not choose, and an answer it translates rather than interprets.

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
		Timeline: []keys.TimelineEventSnapshot{{
			ID: "e1", Type: keys.EventCreated, Message: "Alert group created",
			CreatedAt: time.Unix(1700000000, 0).UTC(),
		}},
	})
	if err != nil {
		t.Fatalf("build the state: %v", err)
	}
	return state
}

func handlerCall(t *testing.T, target keys.Target, interactive bool) outbound.Call {
	t.Helper()
	payload, err := json.Marshal(keys.EscalationPayloadV1{
		Slot: keys.Slot{Kind: keys.SlotFirehose}, Target: target, Interactive: interactive,
	})
	if err != nil {
		t.Fatalf("build the payload: %v", err)
	}
	return outbound.Call{
		IntentID: "intent-1", AttemptID: "attempt-1", Provider: "slack",
		AttemptKind: outbound.AttemptCreate, Operation: outbound.OperationSend,
		Endpoint: "C0001", ProviderKey: "create-key", Revision: 0,
		State: handlerState(t), Payload: payload,
		PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
	}
}

// slackAPI is a fake Slack that records what it was asked and answers however
// the test says.
type slackAPI struct {
	server   *httptest.Server
	posts    []map[string]any
	permalks int
	answer   func(path string, form map[string]any) map[string]any
}

func newSlackAPI(t *testing.T) *slackAPI {
	t.Helper()
	api := &slackAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]any{}
		for k := range r.Form {
			form[k] = r.Form.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(r.URL.Path, "chat.postMessage"):
			api.posts = append(api.posts, form)
		case strings.HasSuffix(r.URL.Path, "chat.getPermalink"):
			api.permalks++
		}

		if api.answer != nil {
			if reply := api.answer(r.URL.Path, form); reply != nil {
				_ = json.NewEncoder(w).Encode(reply)
				return
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "chat.postMessage"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "channel": "C0001", "ts": "1700000000.000100",
			})
		case strings.HasSuffix(r.URL.Path, "chat.getPermalink"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "permalink": "https://slack.example/archives/C0001/p1700000000000100",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

// handlerFor points a handler at the fake by giving it a token source whose
// client the test can steer.
func handlerFor(api *slackAPI) *Handler {
	return &Handler{tokens: &mockTokenSource{token: "tok", interactive: true},
		newClient: func(token string) *slackapi.Client {
			return slackapi.New(token, slackapi.OptionAPIURL(api.server.URL+"/"))
		}}
}

// TestOneAttemptIsOneCall is the promise the journal rests on: what an attempt
// records as possibly-having-happened is one external effect, not three.
func TestOneAttemptIsOneCall(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	result, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}, true))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Evidence != outbound.ProviderResponse || result.Status != "ok" {
		t.Fatalf("the call answered %+v", result)
	}

	// The card, then the timeline in its thread. The thread post is enrichment
	// AFTER the message exists, which is why it is allowed to be a second call.
	if len(api.posts) != 2 {
		t.Fatalf("the attempt made %d posts", len(api.posts))
	}
	if api.posts[0]["channel"] != "C0001" {
		t.Fatalf("the card went to %v", api.posts[0]["channel"])
	}
	if api.posts[1]["thread_ts"] != "1700000000.000100" {
		t.Fatalf("the timeline was not posted in the card's thread: %v", api.posts[1])
	}

	if !result.Receipt.Recorded() {
		t.Fatal("the message was accepted and nothing says where it is")
	}
	if ref := result.Receipt.Ref(); ref != "C0001/1700000000.000100" {
		t.Fatalf("the receipt names %q", ref)
	}
	var data Data
	if err := json.Unmarshal(result.Receipt.Raw(), &data); err != nil {
		t.Fatalf("read the receipt: %v", err)
	}
	if data.Permalink == "" || data.TimelineTimestamp == "" {
		t.Fatalf("the enrichment was not recorded: %+v", data)
	}
}

// TestEnrichmentCannotFailAnAttempt. The message exists; calling the attempt
// failed because a permalink could not be fetched would send it again.
func TestEnrichmentCannotFailAnAttempt(t *testing.T) {
	api := newSlackAPI(t)
	api.answer = func(path string, _ map[string]any) map[string]any {
		if strings.HasSuffix(path, "chat.getPermalink") {
			return map[string]any{"ok": false, "error": "channel_not_found"}
		}
		return nil
	}
	handler := handlerFor(api)

	result, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}, true))
	if err != nil {
		t.Fatalf("a failed permalink failed the attempt: %v", err)
	}
	if result.Status != "ok" || !result.Receipt.Recorded() {
		t.Fatalf("the delivery was lost with the enrichment: %+v", result)
	}
	if !strings.Contains(result.Summary, "no permalink") {
		t.Fatalf("the journal says nothing about what failed: %q", result.Summary)
	}
}

// TestADirectMessageIsOneCallToo. conversations.open is gone: chat.postMessage
// takes the user id in `channel` and opens the conversation itself, so a DM has
// no second place to fail before the message exists.
func TestADirectMessageIsOneCallToo(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	call := handlerCall(t, keys.Target{Kind: keys.TargetUser, Ref: "user-1"}, true)
	call.Endpoint = "U0001"
	result, err := handler.ExecuteAttempt(context.Background(), call)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if len(api.posts) != 1 {
		t.Fatalf("a direct message made %d posts", len(api.posts))
	}
	if api.posts[0]["channel"] != "U0001" {
		t.Fatalf("the message went to %v rather than the user", api.posts[0]["channel"])
	}
	text, _ := api.posts[0]["text"].(string)
	if !strings.Contains(text, "Disk filling up") {
		t.Fatalf("the message does not say what happened: %q", text)
	}
	// The link is the alert in TokayOps, not a permalink read from a
	// neighbouring card: that one does not exist yet on the first attempt and
	// does on the retry, which is two different messages under one key.
	if !strings.Contains(text, "https://tokay.example/#/ops/alert-groups/ag-1") {
		t.Fatalf("the message does not link to the alert: %q", text)
	}
	// A one-shot message still records where it went.
	if !result.Receipt.Recorded() {
		t.Fatal("a direct message was sent and nothing says where")
	}
}

// TestSlackAnswersAreTranslatedNotInterpreted walks the capability matrix. The
// point of every line is which way the uncertainty falls.
func TestSlackAnswersAreTranslatedNotInterpreted(t *testing.T) {
	handler := &Handler{}

	cases := map[string]struct {
		want  outbound.Outcome
		known bool
	}{
		"ok":                              {outbound.OutcomeAccepted, true},
		"ratelimited":                     {outbound.OutcomeRetryableRejection, true},
		"request_timeout":                 {outbound.OutcomeRetryableRejection, true},
		"internal_error":                  {outbound.OutcomeAmbiguous, true},
		"fatal_error":                     {outbound.OutcomeAmbiguous, true},
		"service_unavailable":             {outbound.OutcomeAmbiguous, true},
		"http_503":                        {outbound.OutcomeAmbiguous, true},
		"channel_not_found":               {outbound.OutcomePermanentRejection, true},
		"token_revoked":                   {outbound.OutcomePermanentRejection, true},
		"msg_too_long":                    {outbound.OutcomePermanentRejection, true},
		"some_code_slack_added_last_week": {"", false},
		"":                                {"", false},
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

// TestPreparationRefusesWhatCannotBeSent. Each of these is a refusal that
// leaves proof rather than a call whose fate is unknown.
func TestPreparationRefusesWhatCannotBeSent(t *testing.T) {
	linked := providers.IdentityLookup(func(context.Context, string, string) (string, error) {
		return "U0001", nil
	})

	t.Run("a channel needs nothing resolved", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"}, linked)
		prepared := handler.Prepare(context.Background(), outbound.Intent{
			TargetKind: keys.TargetChannel, TargetRef: "C0001",
			PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		})
		if prepared.Outcome() != outbound.PreparationReady {
			t.Fatalf("a channel send was refused: %+v", prepared)
		}
	})

	t.Run("a person is looked up every attempt", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"}, linked)
		prepared := handler.Prepare(context.Background(), outbound.Intent{
			TargetKind: keys.TargetUser, TargetRef: "user-1",
			PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		})
		if prepared.Outcome() != outbound.PreparationReady {
			t.Fatalf("a linked person was refused: %+v", prepared)
		}
		request := prepared.Request("intent-1", "token-1", "worker-1")
		if request.BoundEndpoint != "U0001" {
			t.Fatalf("the resolved address is %q", request.BoundEndpoint)
		}
	})

	t.Run("nobody's account links itself", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"},
			func(context.Context, string, string) (string, error) {
				return "", providers.ErrNotLinked
			})
		prepared := handler.Prepare(context.Background(), outbound.Intent{
			TargetKind: keys.TargetUser, TargetRef: "user-1",
			PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		})
		if prepared.Outcome() != outbound.PreparationPermanent {
			t.Fatalf("an unlinked person was %s", prepared.Outcome())
		}
	})

	t.Run("a lookup that failed says nothing about the person", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"},
			func(context.Context, string, string) (string, error) {
				return "", errors.New("database is down")
			})
		prepared := handler.Prepare(context.Background(), outbound.Intent{
			TargetKind: keys.TargetUser, TargetRef: "user-1",
			PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		})
		if prepared.Outcome() != outbound.PreparationTransient {
			t.Fatalf("a failing lookup was %s", prepared.Outcome())
		}
	})

	t.Run("an installation with no Slack", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: ""}, linked)
		prepared := handler.Prepare(context.Background(), outbound.Intent{
			TargetKind: keys.TargetChannel, TargetRef: "C0001",
			PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		})
		if prepared.Outcome() != outbound.PreparationPermanent {
			t.Fatalf("a missing integration was %s", prepared.Outcome())
		}
	})

	t.Run("a payload this build cannot render", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"}, linked)
		prepared := handler.Prepare(context.Background(), outbound.Intent{
			TargetKind: keys.TargetChannel, TargetRef: "C0001",
			PayloadSchemaVersion: 99,
		})
		if prepared.Outcome() != outbound.PreparationPermanent {
			t.Fatalf("an unreadable payload was %s", prepared.Outcome())
		}
	})
}
