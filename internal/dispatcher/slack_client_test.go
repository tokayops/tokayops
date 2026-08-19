package dispatcher

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// TestNewSlackClient_TimesOutOnAHangingServer: the client gives up on a call
// that never answers.
//
// What can actually go wrong is the wiring - a client built without the option
// keeps slack-go's own &http.Client{}, whose Timeout is zero, and waits for as
// long as the connection stays open. So the assertion is on behaviour against a
// server that never replies, not on the constant: that &http.Client{Timeout: x}
// honours x is the standard library's business.
//
// Three things are asserted rather than one, because "an error arrived quickly"
// is true of several ways of being wrong: a helper that dropped OptionAPIURL
// would fail fast against the real Slack, and one that ignored its timeout
// argument for a hardcoded larger value would still beat a one-second watchdog.
func TestNewSlackClient_TimesOutOnAHangingServer(t *testing.T) {
	const timeout = 50 * time.Millisecond

	hang := make(chan struct{})
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-hang
	}))
	// Order matters and is the whole reason these are two lines: Close waits for
	// the handler to return, and the handler only returns once hang is closed.
	// Deferred calls run last-registered-first, so this pair releases the
	// handler and only then shuts the server down.
	defer server.Close()
	defer close(hang)

	client := newSlackClient("xoxb-test", timeout, slack.OptionAPIURL(server.URL+"/"))

	// A context with no deadline of its own: the only thing that can end this
	// call is the client's timeout, which is the point.
	//
	// The wait is bounded by the test rather than by the call, because a client
	// built without the option would not return at all, and a test about calls
	// that hang forever must not hang forever itself.
	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		_, err := client.AuthTestContext(context.Background())
		done <- outcome{err: err, elapsed: time.Since(start)}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(20 * timeout):
		t.Fatalf("the call had not returned after %v against a %v timeout - the client was built without one",
			20*timeout, timeout)
	}

	// The request reached OUR server, so the timeout that ended it was ours.
	select {
	case <-entered:
	default:
		t.Fatal("the hanging server was never called - the client did not use the test endpoint")
	}

	var netErr net.Error
	if !errors.As(got.err, &netErr) || !netErr.Timeout() {
		t.Fatalf("the call ended with %v, want a timeout", got.err)
	}

	// Loose enough for a loaded machine, tight enough to catch a timeout that
	// ignores its argument: anything hardcoded above 250ms fails here.
	if got.elapsed > 5*timeout {
		t.Errorf("the call took %v against a %v timeout - the argument was not the bound", got.elapsed, timeout)
	}
}
