package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
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
