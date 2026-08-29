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
		Endpoint: "C0001", ProviderKey: "create-key",
		KeyKind: keys.KindEscalation, Family: outbound.FamilyNotification,
		Content: snapshotContent(t, handlerState(t)), Payload: payload,
		PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
	}
}

// snapshotContent wraps a frozen state the way BeginAttempt does, so a test
// call carries what a real one carries.
func snapshotContent(t *testing.T, state keys.RenderSnapshot) outbound.AttemptContent {
	t.Helper()
	content, err := outbound.NewSnapshotContent(state, false)
	if err != nil {
		t.Fatalf("build the content: %v", err)
	}
	return content
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

// TestAnAttemptEndsWhenTheCardExists.
//
// One call, and the handler returns. Anything done after the card exists but
// before the attempt is closed - a permalink, a message in its thread - widens
// the window where a crash leaves a delivered message beside an attempt that
// says it might not have been. Recovery closes that as ambiguous and the retry
// posts a second card, so the window is not something to handle carefully; it
// is something to not open.
func TestAnAttemptEndsWhenTheCardExists(t *testing.T) {
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

	if len(api.posts) != 1 {
		t.Fatalf("the attempt made %d posts; the second one is another message, "+
			"and another message is another commitment", len(api.posts))
	}
	if api.posts[0]["channel"] != "C0001" {
		t.Fatalf("the card went to %v", api.posts[0]["channel"])
	}
	if api.permalks != 0 {
		t.Fatalf("the attempt made %d permalink calls after the card already existed",
			api.permalks)
	}

	// What proves the acceptance is the coordinates, and they are recorded the
	// moment they arrive.
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
	if data.ChannelID != "C0001" || data.Timestamp != "1700000000.000100" {
		t.Fatalf("the receipt holds %+v", data)
	}
}

// TestTheCardGoesOutAsBlockKit. The wire shape Slack needs, and the shape a
// message-link unfurl needs: a section block inside the coloured attachment,
// the title in the top-level blocks, and a plain fallback text for the places
// that render no blocks at all - the notification, and a screen reader.
func TestTheCardGoesOutAsBlockKit(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	if _, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}, true)); err != nil {
		t.Fatalf("execute: %v", err)
	}

	post := api.posts[0]
	attachments, _ := post["attachments"].(string)
	if !strings.Contains(attachments, `"type":"section"`) {
		t.Errorf("the attachment carries no Block Kit section: %s", attachments)
	}
	blocks, _ := post["blocks"].(string)
	if !strings.Contains(blocks, "Disk filling up") {
		t.Errorf("the top-level blocks do not carry the title: %s", blocks)
	}
	text, _ := post["text"].(string)
	if !strings.Contains(text, "Disk filling up") {
		t.Errorf("the fallback text does not carry the title: %s", text)
	}
}

// TestAnAcceptanceThatNamesNothingIsDoubt. Slack answered ok and would not say
// what it made.
//
// The card may well exist, so this is not a clean failure: calling it one would
// send the retry to post a second card beside the first. It is doubt, and the
// breach names what was wrong with the answer - an acceptance with nothing to
// address afterwards. A person decides what happens next.
func TestAnAcceptanceThatNamesNothingIsDoubt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel string
		ts      string
	}{
		{name: "no timestamp", channel: "C0001", ts: ""},
		{name: "no channel", channel: "", ts: "1700000000.000100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newSlackAPI(t)
			api.answer = func(path string, _ map[string]any) map[string]any {
				if !strings.HasSuffix(path, "chat.postMessage") {
					return nil
				}
				return map[string]any{"ok": true, "channel": tc.channel, "ts": tc.ts}
			}
			handler := handlerFor(api)

			result, err := handler.ExecuteAttempt(context.Background(),
				handlerCall(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}, true))
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if result.Receipt.Recorded() {
				t.Fatalf("coordinates were recorded from %+v", result)
			}

			concluded, breach := outbound.Conclude(handler,
				outbound.Call{AttemptKind: outbound.AttemptCreate}, result, err)
			if concluded.Outcome() != outbound.OutcomeAmbiguous {
				t.Fatalf("an acceptance naming nothing concluded %q", concluded.Outcome())
			}
			if breach != outbound.BreachAcceptanceWithoutReceipt {
				t.Fatalf("the breach recorded is %q", breach)
			}
			if concluded.Receipt() != nil {
				t.Fatal("something was recorded as the card's coordinates")
			}
		})
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
	if api.permalks != 0 {
		t.Fatal("a direct message fetched a permalink nobody reads")
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
		"ok":                  {outbound.OutcomeAccepted, true},
		"ratelimited":         {outbound.OutcomeRetryableRejection, true},
		"request_timeout":     {outbound.OutcomeRetryableRejection, true},
		"internal_error":      {outbound.OutcomeAmbiguous, true},
		"fatal_error":         {outbound.OutcomeAmbiguous, true},
		"service_unavailable": {outbound.OutcomeAmbiguous, true},
		"http_503":            {outbound.OutcomeAmbiguous, true},
		"channel_not_found":   {outbound.OutcomePermanentRejection, true},
		"token_revoked":       {outbound.OutcomePermanentRejection, true},
		"msg_too_long":        {outbound.OutcomePermanentRejection, true},
		// The statuses a change gets back. The first is the one an operator's
		// hands are tied by: it is the only answer that proves the message is
		// not there any more.
		"message_not_found":               {outbound.OutcomePermanentRejection, true},
		"cant_update_message":             {outbound.OutcomePermanentRejection, true},
		"edit_window_closed":              {outbound.OutcomePermanentRejection, true},
		"some_code_slack_added_last_week": {"", false},
		"":                                {"", false},
	}

	for status, want := range cases {
		answer, known := handler.ClassifyResponse(outbound.Result{
			Evidence: outbound.ProviderResponse, Status: status,
		})
		if known != want.known {
			t.Errorf("status %q: known=%v, want %v", status, known, want.known)
			continue
		}
		if known && answer.Outcome != want.want {
			t.Errorf("status %q classified %q, want %q", status, answer.Outcome, want.want)
		}
	}
}

// TestOnlyAMissingMessageProvesAnythingAboutIt. Two refusals that read alike
// and mean opposite things: the message is gone, and the message is there but
// will not change. Only the first may licence a second message.
func TestOnlyAMissingMessageProvesAnythingAboutIt(t *testing.T) {
	handler := NewHandler(nil, nil)

	gone, known := handler.ClassifyResponse(outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "message_not_found",
	})
	if !known || gone.Detail == nil || *gone.Detail != keys.DetailDefinitelyAbsent {
		t.Fatalf("a missing message proved %v", gone.Detail)
	}

	for _, status := range []string{"cant_update_message", "edit_window_closed"} {
		stuck, known := handler.ClassifyResponse(outbound.Result{
			Evidence: outbound.ProviderResponse, Status: status,
		})
		if !known {
			t.Fatalf("%s was not recognised", status)
		}
		if stuck.Detail != nil {
			t.Errorf("%s claimed %v about a message that is still there",
				status, *stuck.Detail)
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
		prepared := handler.Prepare(context.Background(),
			intentFor(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}))
		if prepared.Outcome() != outbound.PreparationReady {
			t.Fatalf("a channel send was refused: %+v", prepared)
		}
	})

	t.Run("a person is looked up every attempt", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"}, linked)
		prepared := handler.Prepare(context.Background(),
			intentFor(t, keys.Target{Kind: keys.TargetUser, Ref: "user-1"}))
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
		prepared := handler.Prepare(context.Background(),
			intentFor(t, keys.Target{Kind: keys.TargetUser, Ref: "user-1"}))
		if prepared.Outcome() != outbound.PreparationPermanent {
			t.Fatalf("an unlinked person was %s", prepared.Outcome())
		}
	})

	t.Run("a lookup that failed says nothing about the person", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"},
			func(context.Context, string, string) (string, error) {
				return "", errors.New("database is down")
			})
		prepared := handler.Prepare(context.Background(),
			intentFor(t, keys.Target{Kind: keys.TargetUser, Ref: "user-1"}))
		if prepared.Outcome() != outbound.PreparationTransient {
			t.Fatalf("a failing lookup was %s", prepared.Outcome())
		}
	})

	t.Run("an installation with no Slack", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: ""}, linked)
		prepared := handler.Prepare(context.Background(),
			intentFor(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}))
		if prepared.Outcome() != outbound.PreparationPermanent {
			t.Fatalf("a missing integration was %s", prepared.Outcome())
		}
	})

	t.Run("a payload this build cannot render", func(t *testing.T) {
		handler := NewHandler(&mockTokenSource{token: "tok"}, linked)
		aged := intentFor(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"})
		aged.PayloadSchemaVersion = 99
		if got := handler.Prepare(context.Background(), aged).Outcome(); got != outbound.PreparationPermanent {
			t.Fatalf("a payload schema this build cannot read was %s", got)
		}
	})
}

// intentFor is a commitment as the store hands one to preparation: the target
// it names, and the payload it was admitted with.
func intentFor(t *testing.T, target keys.Target) outbound.Intent {
	t.Helper()
	payload, err := json.Marshal(keys.EscalationPayloadV1{
		Slot: keys.Slot{Kind: keys.SlotFirehose}, Target: target, Interactive: true,
	})
	if err != nil {
		t.Fatalf("build the payload: %v", err)
	}
	return outbound.Intent{
		ID: "intent-1", Provider: "slack",
		TargetKind: target.Kind, TargetRef: target.Ref,
		PayloadSchemaVersion: (keys.EscalationPayloadV1{}).SchemaVersion(),
		Payload:              payload,
	}
}

// TestAPayloadNobodyCanReadStopsBeforeTheNetwork.
//
// Read inside the call instead, a corrupt payload becomes an attempt that never
// touched the network: recorded as a call whose fate is unknown, retried on the
// family's backoff, and repeated for as long as the commitment lives. It is a
// refusal, and refusals leave proof instead of doubt.
func TestAPayloadNobodyCanReadStopsBeforeTheNetwork(t *testing.T) {
	handler := NewHandler(&mockTokenSource{token: "tok"}, nil)

	for name, payload := range map[string][]byte{
		"nothing at all":             nil,
		"truncated json":             []byte(`{"target":{"kind":"chan`),
		"a target nobody has":        []byte(`{"slot":{"kind":"firehose"},"target":{"kind":"carrier_pigeon","ref":"x"}}`),
		"a target with no recipient": []byte(`{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":""}}`),
		"a slot nobody has":          []byte(`{"slot":{"kind":"whenever"},"target":{"kind":"channel","ref":"C0001"}}`),
		// A build that knows more than this one wrote an instruction here. It
		// is refused rather than dropped: rendering the rest would carry out
		// half of what was admitted.
		"a field this build does not know": []byte(`{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"quiet_hours":true}`),
	} {
		t.Run(name, func(t *testing.T) {
			intent := intentFor(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"})
			intent.Payload = payload

			prepared := handler.Prepare(context.Background(), intent)
			if prepared.Outcome() != outbound.PreparationPermanent {
				t.Fatalf("an unreadable payload was %s, which retries forever",
					prepared.Outcome())
			}
			request := prepared.Request("intent-1", "token-1", "worker-1")
			if request.ErrorClass != "payload_unreadable" || request.Summary == "" {
				t.Fatalf("the refusal says %q/%q", request.ErrorClass, request.Summary)
			}
		})
	}
}

// TestARedirectIsAnAnswerNotADeadEnd.
//
// The provider accepted the POST and answered 3xx. Followed, the client would
// go somewhere else, and if THAT hop cannot resolve or handshake the error is
// indistinguishable from a request that never left - so the attempt would be
// called a clean failure and the retry would post a second card. The first
// answer is the one that counts, and an answer nobody recognises is doubt.
func TestARedirectIsAnAnswerNotADeadEnd(t *testing.T) {
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The message may already exist at this point; all we know is that
		// Slack answered by pointing somewhere.
		http.Redirect(w, r, "https://tokay.invalid./chat.postMessage", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirected.Close)

	handler := &Handler{
		tokens: &mockTokenSource{token: "tok"},
		newClient: func(token string) *slackapi.Client {
			return NewClient(token, HTTPTimeout, slackapi.OptionAPIURL(redirected.URL+"/"))
		},
	}

	result, err := handler.ExecuteAttempt(context.Background(),
		handlerCall(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}, true))
	if err == nil {
		t.Fatal("a redirect was taken for a delivery")
	}
	if result.Evidence == outbound.DefinitelyNotSent {
		t.Fatalf("a message that may exist was recorded as never sent: %+v", result)
	}

	concluded, _ := outbound.Conclude(handler, outbound.Call{AttemptKind: outbound.AttemptCreate}, result, err)
	if concluded.Outcome() == outbound.OutcomeRetryableRejection {
		t.Fatal("the attempt was called a clean failure, so the retry posts a second card")
	}
	if concluded.Outcome() != outbound.OutcomeAmbiguous {
		t.Fatalf("a redirect concluded %q", concluded.Outcome())
	}
}

// TestACommitmentAddressedTwiceHasToAgree.
//
// The commitment names its recipient in its own columns, which decide where the
// message goes, and again in the payload, which decides what is written. A row
// where those disagree does not produce a mangled journal entry: it delivers
// what was composed for a person into the channel named beside it.
func TestACommitmentAddressedTwiceHasToAgree(t *testing.T) {
	handler := NewHandler(&mockTokenSource{token: "tok"},
		func(context.Context, string, string) (string, error) { return "U0001", nil })

	cases := map[string]struct {
		column keys.Target
		inside keys.Target
	}{
		"a private message addressed to a channel": {
			column: keys.Target{Kind: keys.TargetChannel, Ref: "C0001"},
			inside: keys.Target{Kind: keys.TargetUser, Ref: "user-1"},
		},
		"a card addressed to a person": {
			column: keys.Target{Kind: keys.TargetUser, Ref: "user-1"},
			inside: keys.Target{Kind: keys.TargetChannel, Ref: "C0001"},
		},
		"the right kind, the wrong recipient": {
			column: keys.Target{Kind: keys.TargetChannel, Ref: "C0001"},
			inside: keys.Target{Kind: keys.TargetChannel, Ref: "C9999"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			intent := intentFor(t, tc.inside)
			intent.TargetKind, intent.TargetRef = tc.column.Kind, tc.column.Ref

			prepared := handler.Prepare(context.Background(), intent)
			if prepared.Outcome() != outbound.PreparationPermanent {
				t.Fatalf("a commitment that disagrees with itself was %s", prepared.Outcome())
			}
			if got := prepared.Request("i", "t", "w").ErrorClass; got != "target_mismatch" {
				t.Fatalf("the refusal says %q", got)
			}
		})
	}
}

// TestACallWithNoStateToRenderIsRefused.
//
// This channel draws an alert card from a frozen state. A commitment that
// carries none is one the delivery domain routed here by mistake, and the
// answer is a deterministic refusal in front of a person - not an empty card.
//
// The check asks the content whether it HAS a state rather than looking at
// whether the state is empty: a zero snapshot renders a message about no alert,
// with a title of nothing and no link, and it would go out.
func TestACallWithNoStateToRenderIsRefused(t *testing.T) {
	api := newSlackAPI(t)
	handler := handlerFor(api)

	call := handlerCall(t, keys.Target{Kind: keys.TargetChannel, Ref: "C0001"}, true)
	payloadOnly, err := outbound.NewPayloadContent(handlerState(t).Digest())
	if err != nil {
		t.Fatalf("build the content: %v", err)
	}
	call.Content = payloadOnly
	call.KeyKind = keys.Kind("handoff")

	result, err := handler.ExecuteAttempt(context.Background(), call)
	if err == nil {
		t.Fatal("a commitment with no state to render was sent anyway")
	}
	if result.Evidence != outbound.DefinitelyNotSent {
		t.Fatalf("a call that never went out is recorded as %q", result.Evidence)
	}
	if !strings.Contains(result.Summary, "handoff") {
		t.Fatalf("the refusal does not say what it was: %q", result.Summary)
	}
	if len(api.posts) != 0 {
		t.Fatal("the channel was called anyway")
	}
}
