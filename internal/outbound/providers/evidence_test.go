package providers

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
)

// What a transport failure proves, and which way the doubt falls.
//
// Getting this wrong is expensive in one direction only. A request that may
// have gone out, called a clean failure, is retried - and the retry is a second
// page with nothing in the journal to explain it. A request that never left,
// called doubtful, costs one ambiguous record and a look. So only the failures
// that happen before anything is written count as proof, and the errors are
// produced here by actually failing rather than by hand: a hand-built error
// matches whatever the check looks for, which is how the previous version of
// this rule passed review while never firing once.

func TestFailuresBeforeAnythingIsWritten(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"a name that does not resolve", dialError(t, "http://tokay.invalid.")},
		{"a port with nothing behind it", dialError(t, refusedAddress(t))},
		{"a certificate nobody signed", tlsError(t)},
		{"a refused connection, bare", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}},
		{"an unreachable host", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EvidenceOf(tc.err); got != outbound.DefinitelyNotSent {
				t.Fatalf("%v was classified %q, and a retry of it is free", tc.err, got)
			}
		})
	}
}

func TestFailuresAfterTheRequestMayHaveGoneOut(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"a server that never answered", timeoutError(t)},
		{"a connection reset mid-flight", &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		{"a deadline that passed", context.DeadlineExceeded},
		{"something nobody has seen before", errors.New("the modem caught fire")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EvidenceOf(tc.err); got != outbound.PossiblySent {
				t.Fatalf("%v was classified %q, and a retry of it may page somebody twice",
					tc.err, got)
			}
		})
	}
}

func TestAnAnswerIsNotATransportFailure(t *testing.T) {
	if got := EvidenceOf(nil); got != outbound.ProviderResponse {
		t.Fatalf("a call that returned no error was classified %q", got)
	}
}

// dialError produces a real failure to reach an address.
func dialError(t *testing.T, address string) error {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	_, err := client.Get(address)
	if err == nil {
		t.Fatalf("%s answered", address)
	}
	return err
}

// refusedAddress is a port that was listening and is not any more, which is the
// closest thing to a guaranteed connection refusal.
func refusedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return "http://" + address
}

// tlsError produces a real handshake failure: a server with a certificate the
// client has no reason to trust.
func tlsError(t *testing.T) error {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
	}
	_, err := client.Get(server.URL)
	if err == nil {
		t.Fatal("an untrusted certificate was accepted")
	}

	// The check is only worth having if this is the shape a real handshake
	// failure has.
	var verification *tls.CertificateVerificationError
	if !errors.As(err, &verification) {
		t.Fatalf("a failed handshake is not a tls.CertificateVerificationError any more: %#v", err)
	}
	return err
}

// timeoutError produces a real timeout after the request was written.
func timeoutError(t *testing.T) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 50 * time.Millisecond}
	_, err := client.Get(server.URL)
	if err == nil {
		t.Fatal("a hanging server answered")
	}
	return fmt.Errorf("sending: %w", err)
}
