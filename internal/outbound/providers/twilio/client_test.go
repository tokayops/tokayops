package twilio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// testAccountSID has the shape of a Twilio account id. It is put together
// here rather than written out, because a literal of that shape is what secret
// scanners look for, and a fixture that trips them blocks every push.
var testAccountSID = "AC" + strings.Repeat("0", 31) + "1"

var testConfig = model.TwilioConfig{
	AccountSID: testAccountSID, AuthToken: "token", FromNumber: "+15005550006",
}

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// The request is the one Twilio documents: the account in the path and in the
// basic auth, the call as a form, the TwiML inline.
func TestACallIsCreatedWithTheDocumentedRequest(t *testing.T) {
	var got *http.Request
	var form map[string]string
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		_ = r.ParseForm()
		form = map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"CA123","status":"queued"}`))
	})

	sid, err := client.CreateCall(context.Background(), testConfig, Call{
		To: "+14155550101", TwiML: "<Response/>", Ring: 30 * time.Second, Limit: time.Minute,
	})
	if err != nil || sid != "CA123" {
		t.Fatalf("CreateCall = %q, %v", sid, err)
	}
	if got.URL.Path != "/2010-04-01/Accounts/"+testConfig.AccountSID+"/Calls.json" {
		t.Errorf("path = %s", got.URL.Path)
	}
	if user, pass, ok := got.BasicAuth(); !ok || user != testConfig.AccountSID || pass != "token" {
		t.Errorf("basic auth = %q %q %v", user, pass, ok)
	}
	want := map[string]string{"To": "+14155550101", "From": "+15005550006", "Twiml": "<Response/>",
		"Timeout": "30", "TimeLimit": "60"}
	for k, v := range want {
		if form[k] != v {
			t.Errorf("form %s = %q, want %q", k, form[k], v)
		}
	}
}

// The one thing a caller must be able to tell: whether a call may be ringing.
func TestAFailureSaysWhetherACallMayExist(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		notCreated bool
		code       int
	}{
		{"invalid number", 400, `{"code":21211,"message":"Invalid 'To' Phone Number","status":400}`, true, 21211},
		{"too many requests", 429, `{"code":20429,"message":"Too Many Requests","status":429}`, true, 20429},
		{"bad credentials", 401, `{"code":20003,"message":"Authenticate","status":401}`, true, 20003},
		{"server error", 500, `oops`, false, 0},
		{"unavailable", 503, ``, false, 0},
		{"accepted without a sid", 201, `{}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := client.CreateCall(context.Background(), testConfig, Call{To: "+14155550101"})
			if got := errors.Is(err, ErrNotCreated); got != tc.notCreated {
				t.Fatalf("not created = %v, want %v (%v)", got, tc.notCreated, err)
			}
			if got := errors.Is(err, ErrUnknown); got == tc.notCreated {
				t.Fatalf("unknown = %v, want %v (%v)", got, !tc.notCreated, err)
			}
			var refusal *Refusal
			if tc.notCreated && (!errors.As(err, &refusal) || refusal.Code != tc.code) {
				t.Fatalf("refusal = %+v, want code %d", refusal, tc.code)
			}
		})
	}
}

// A request that got no answer may have been taken.
func TestNoAnswerMeansTheCallMayExist(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	})
	_, err := client.CreateCall(context.Background(), testConfig, Call{To: "+14155550101"})
	if !errors.Is(err, ErrUnknown) {
		t.Fatalf("a dropped connection = %v, want ErrUnknown", err)
	}
}

func TestTheTwiMLSpeaksEscapedText(t *testing.T) {
	got := Say("en-US", `Code 1, 2 & "3" <4>`)
	want := `<Response><Say language="en-US">Code 1, 2 &amp; &#34;3&#34; &lt;4&gt;</Say></Response>`
	if got != want {
		t.Fatalf("Say = %s\nwant %s", got, want)
	}
	if SpokenDigits("042") != "0, 4, 2" {
		t.Fatalf("SpokenDigits = %q", SpokenDigits("042"))
	}
}

func integrationFor(id, from string, priority int, coverage ...string) Integration {
	return Integration{ID: id, Config: model.TwilioConfig{
		AccountSID: "AC1", FromNumber: from, Priority: priority, Coverage: coverage, CPS: 1, MaxConcurrent: 1,
	}}
}

func TestTheProviderIsPickedByCoverageThenPinThenPriority(t *testing.T) {
	all := []Integration{
		integrationFor("world", "+15005550001", 10, "+"),
		integrationFor("ru", "+79990000001", 1, "+7"),
		integrationFor("us", "+15005550002", 5, "+1"),
	}
	for _, tc := range []struct {
		number, pinned, want string
		ok                   bool
	}{
		{"+79161234567", "", "ru", true},
		{"+79161234567", "world", "world", true},
		{"+79161234567", "us", "ru", true}, // a pin that does not cover the number is not used
		{"+14155550101", "", "us", true},
		{"+442071234567", "", "world", true},
	} {
		got, ok := Pick(all, tc.number, tc.pinned)
		if ok != tc.ok || got.ID != tc.want {
			t.Errorf("Pick(%s, pinned %q) = %q %v, want %q", tc.number, tc.pinned, got.ID, ok, tc.want)
		}
	}
	if _, ok := Pick(all[1:2], "+14155550101", ""); ok {
		t.Error("a number nobody covers was given a provider")
	}
}

func TestIntegrationsOnOneAccountShareTheLowestLimits(t *testing.T) {
	a := integrationFor("a", "+1", 0, "+")
	a.Config.CPS, a.Config.MaxConcurrent = 5, 2
	b := integrationFor("b", "+2", 0, "+")
	b.Config.CPS, b.Config.MaxConcurrent = 1, 9
	other := integrationFor("c", "+3", 0, "+")
	other.Config.AccountSID, other.Config.CPS, other.Config.MaxConcurrent = "AC2", 0, 0

	cps, concurrent := AccountLimits([]Integration{a, b, other}, "AC1")
	if cps != 1 || concurrent != 2 {
		t.Fatalf("limits = %d cps, %d concurrent; want 1 and 2", cps, concurrent)
	}
}

func TestAConfigThatDoesNotReadIsNamed(t *testing.T) {
	_, err := Decode([]*model.Integration{{ID: "broken", Type: model.IntegrationTypeTwilio, Enabled: true, Config: []byte(`{`)}})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("Decode of a damaged config = %v, want an error naming it", err)
	}
}
