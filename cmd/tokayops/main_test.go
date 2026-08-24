package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// buildBinary compiles the binary under test.
//
// It lives here rather than beside the gate tests because it is needed without a
// database: whether this binary refuses a command it cannot run is answerable
// with no Postgres anywhere, and gating that answer behind TEST_DB_DSN would
// leave it unrun in ordinary CI. The integration-tagged file still sees it -
// untagged files are compiled either way.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tokayops")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the binary under test: %v\n%s", err, out)
	}
	return bin
}

func TestCheckCommand(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantErr  bool
		contains []string
	}{
		{name: "no arguments runs the server", args: []string{"tokayops"}},
		{name: "a known command passes", args: []string{"tokayops", "seed"}},
		{name: "every listed command passes", args: []string{"tokayops", "migrate-slack-identities"}},
		{
			name:     "a typo is refused and named",
			args:     []string{"tokayops", "seeed"},
			wantErr:  true,
			contains: []string{`"seeed"`, "known commands", "seed"},
		},
		{
			// The mistake this check exists for: ENTRYPOINT already runs the
			// binary, so `docker compose run tokay /app/tokayops migrate` makes
			// the path the command.
			name:     "a path gets the entrypoint hint",
			args:     []string{"tokayops", "/app/tokayops", "seed"},
			wantErr:  true,
			contains: []string{`"/app/tokayops"`, "the image already runs the binary"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCommand(tt.args)
			if tt.wantErr != (err != nil) {
				t.Fatalf("checkCommand(%q) = %v, wantErr %v", tt.args[1:], err, tt.wantErr)
			}
			if err == nil {
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// TestUnknownCommandRefusedBeforeTheDatabase: the binary refuses a command it
// cannot run, and refuses it before touching anything.
//
// A process is the only thing that can answer this. The defect was that an
// unrecognised argument fell out of the dispatch switch and started the server,
// which no unit test over checkCommand can see - it would still pass with the
// call to it deleted from main.
//
// The exit code proves nothing on its own: a binary that had regressed would
// also exit non-zero here, on the database it cannot reach. What separates the
// two is the text, and the absence of the line main prints before connecting.
func TestUnknownCommandRefusedBeforeTheDatabase(t *testing.T) {
	bin := buildBinary(t)

	cfg, err := filepath.Abs(filepath.Join("..", "..", "tokay.yaml"))
	if err != nil {
		t.Fatalf("resolve tokay.yaml: %v", err)
	}

	// A real config so a regression gets as far as the database, and a port
	// nothing listens on so it fails there at once: NewStore pings eagerly, so
	// this cannot wander into a Postgres the developer happens to be running.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "migrat")
	cmd.Env = []string{"CONFIG_FILE=" + cfg, "DB_HOST=127.0.0.1", "DB_PORT=1"}

	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the binary was still running after 20s - an unknown command started the server:\n%s", out)
	}
	if err == nil {
		t.Fatalf("the binary exited 0 on an unknown command:\n%s", out)
	}

	output := string(out)
	if !strings.Contains(output, `unknown command "migrat"`) {
		t.Errorf("the refusal does not name the command:\n%s", output)
	}
	if !strings.Contains(output, "known commands") {
		t.Errorf("the refusal does not list what it would accept:\n%s", output)
	}
	// main logs this immediately before store.NewStore. Seeing it means the
	// refusal came from the database, not from the argument.
	if strings.Contains(output, "Using DB:") {
		t.Errorf("the binary reached the database before refusing the command:\n%s", output)
	}
}

// TestShutdownWaitsForTheWorkersItWasGiven.
//
// The outbound worker holds calls that have BEEN MADE. A process that cancels
// its context and exits leaves an answer arriving a moment later with nowhere
// to go, and the delivery becomes ambiguous - a message that may or may not
// have been sent, with nothing saying which. That is what this waits for, and
// it was missing: the workers were started fire-and-forget.
func TestShutdownWaitsForTheWorkersItWasGiven(t *testing.T) {
	quit := make(chan os.Signal, 1)
	stopped := make(chan struct{})

	var cancelled atomic.Bool
	cancelledFirst := make(chan struct{})
	cancel := func() {
		cancelled.Store(true)
		close(cancelledFirst)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		awaitShutdown(quit, cancel, nil, stopped)
	}()

	// Nothing happens until the signal.
	select {
	case <-returned:
		t.Fatal("the process gave up before it was told to")
	case <-time.After(20 * time.Millisecond):
	}

	quit <- syscall.SIGTERM

	// The worker is told to stop before it is waited for, or the wait would be
	// for something that has no reason to end.
	select {
	case <-cancelledFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("the background context was never cancelled")
	}

	// And the process stays while the worker is still finishing.
	select {
	case <-returned:
		t.Fatal("the process exited while a worker was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(stopped)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the process did not exit after its workers were done")
	}
	if !cancelled.Load() {
		t.Error("the workers were waited for without being told to stop")
	}
}

// TestShutdownStopsAcceptingBeforeItWaits.
//
// Waiting for the delivery worker can take a minute. A process that spends that
// minute still serving its API and its ingestion endpoint is taking on alerts,
// acknowledgements and webhook deliveries that nothing behind them is running
// any more: the engine and the worker are stopping. Before the join was added
// this could not happen, because the process simply exited - which is how the
// window got here.
func TestShutdownStopsAcceptingBeforeItWaits(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	url := "http://" + ln.Addr().String() + "/"
	if resp, err := http.Get(url); err != nil {
		t.Fatalf("the server was not serving to begin with: %v", err)
	} else {
		resp.Body.Close()
	}

	quit := make(chan os.Signal, 1)
	// The worker is still draining, and stays that way for the whole test.
	stopped := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		awaitShutdown(quit, func() {}, []listener{srv}, stopped)
	}()

	quit <- syscall.SIGTERM

	// The port has to be refusing while the worker is still running. That is
	// the window this test is about; the wait itself is the previous test.
	refused := false
	for i := 0; i < 200; i++ {
		resp, err := http.Get(url)
		if err != nil {
			refused = true
			break
		}
		resp.Body.Close()
		time.Sleep(10 * time.Millisecond)
	}
	if !refused {
		t.Fatal("the API was still accepting requests while the process was draining")
	}

	select {
	case <-returned:
		t.Fatal("the process exited while a worker was still running")
	default:
	}

	close(stopped)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the process did not exit after its workers were done")
	}
}
