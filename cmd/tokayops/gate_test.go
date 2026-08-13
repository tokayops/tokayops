//go:build integration

package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// The startup gate is the safety mechanism of the schedule cutover, and it is
// the one part of it that no other test can reach.
//
// It is tempting to say the e2e suite covers it: if the wiring were wrong the
// stack would not come up. It would. e2e starts against a database created
// moments earlier, which InitDB puts straight into the final shape, so the gate
// passes there whether it is called or not. That suite stays green if the call
// is forgotten entirely, if it is placed above the `migrate` branch - where it
// would refuse the very command that repairs the database - or if it
// accidentally gates the maintenance subcommands an operator needs mid-cutover.
//
// Each of those is a separate case below, run against a real binary and a real
// legacy database, because a process is the only thing that can answer "does
// this binary refuse to start".
func TestStartupGate(t *testing.T) {
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set")
	}
	bin := buildBinary(t)
	cfg := repoFile(t, "tokay.yaml")

	t.Run("refuses a legacy database and says which step was skipped", func(t *testing.T) {
		env := newGateDB(t, dsn, cfg)
		env.makeLegacy(t, bin)

		out, err := env.run(t, bin)
		if err == nil {
			t.Fatalf("the server started against a pre-revision schema:\n%s", out)
		}
		// The refusal is for an operator mid-cutover. A bare DDL or scan error
		// would name a column and leave them with nothing to act on.
		if !strings.Contains(out, "migrate reset-schedules") {
			t.Errorf("the refusal does not name the skipped step:\n%s", out)
		}
		if !strings.Contains(out, "epic10-upgrade-checklist") {
			t.Errorf("the refusal does not point at the checklist:\n%s", out)
		}
	})

	t.Run("does not gate the migrate subcommand that repairs it", func(t *testing.T) {
		env := newGateDB(t, dsn, cfg)
		env.makeLegacy(t, bin)

		out, err := env.run(t, bin, "migrate", "reset-schedules")
		if err != nil {
			t.Fatalf("the reset was refused on the database it exists to repair: %v\n%s", err, out)
		}
		if !strings.Contains(out, "legacy schema removed") {
			t.Errorf("the reset did not report the cleanup:\n%s", out)
		}
	})

	t.Run("does not gate the maintenance subcommands", func(t *testing.T) {
		// An operator halfway through a cutover window may well need to create
		// a user. None of these touch schedules, and refusing them would turn
		// one skipped step into a locked toolbox.
		env := newGateDB(t, dsn, cfg)
		env.makeLegacy(t, bin)

		out, err := env.run(t, bin, "user", "create", "gate@example.com", "Passw0rd!", "Gate")
		if err != nil {
			t.Fatalf("`user create` was refused on a legacy database: %v\n%s", err, out)
		}
	})

	t.Run("starts once the reset has run", func(t *testing.T) {
		env := newGateDB(t, dsn, cfg)
		env.makeLegacy(t, bin)
		if out, err := env.run(t, bin, "migrate", "reset-schedules"); err != nil {
			t.Fatalf("reset: %v\n%s", err, out)
		}

		env.requireServes(t, bin)
	})
}

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tokayops")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the binary under test: %v\n%s", err, out)
	}
	return bin
}

// gateEnv is one throwaway database plus the environment that points the binary
// at it.
type gateEnv struct {
	dsn  string
	name string
	env  []string
}

// repoFile resolves a path relative to the repository root. The test binary
// runs with cmd/tokayops as its working directory, and the binary under test
// looks for tokay.yaml relative to its own - which is not the same place.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("stat %s: %v", abs, err)
	}
	return abs
}

func newGateDB(t *testing.T, adminDSN string, configPath string) *gateEnv {
	t.Helper()

	admin, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatalf("open the admin connection: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("gate_%x", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("cannot create a database for the gate test (%v); "+
			"TEST_DB_DSN must name a user allowed to CREATE DATABASE", err)
	}
	t.Cleanup(func() {
		dropper, err := sql.Open("postgres", adminDSN)
		if err != nil {
			t.Errorf("reopen to drop %s: %v", name, err)
			return
		}
		defer dropper.Close()
		if _, err := dropper.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse TEST_DB_DSN: %v", err)
	}
	password, _ := u.User.Password()
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "5432"
	}
	u.Path = "/" + name

	// The binary reads discrete DB_* variables rather than a DSN
	// (cmd/tokayops/main.go), so a test that only exported TEST_DB_DSN would
	// silently point the subprocess at localhost:5432 and prove nothing.
	return &gateEnv{
		dsn:  u.String(),
		name: name,
		env: append(os.Environ(),
			"DB_HOST="+host,
			"DB_PORT="+port,
			"DB_USER="+u.User.Username(),
			"DB_PASSWORD="+password,
			"DB_NAME="+name,
			"DB_SSLMODE=disable",
			"CONFIG_FILE="+configPath,
			"JWT_SECRET=gate-test-secret",
			"ENCRYPTION_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		),
	}
}

// makeLegacy leaves the database in the shape an installation has before the
// destructive upgrade: the pre-revision schema, with a row in it.
func (g *gateEnv) makeLegacy(t *testing.T, bin string) {
	t.Helper()

	db, err := sql.Open("postgres", g.dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", g.name, err)
	}
	defer db.Close()

	// The schema has to exist before it can be made legacy, and the binary's
	// own InitDB is what creates it - running the real thing rather than a
	// copy of the DDL is the point. The markers it records go again: this
	// database is pretending to have never been upgraded.
	if out, err := g.run(t, bin, "migrate", "reset-schedules"); err != nil {
		t.Fatalf("prepare the schema: %v\n%s", err, out)
	}
	if _, err := db.Exec(`DELETE FROM migration_markers`); err != nil {
		t.Fatalf("clear the markers: %v", err)
	}
	if _, err := db.Exec(gateLegacyDDL); err != nil {
		t.Fatalf("build the pre-revision schema: %v", err)
	}
}

// gateLegacyDDL is the smallest shape RequireCutoverSchema has to refuse: one
// legacy table and the nullable horizon. The full pre-revision schema is
// exercised in internal/store; what is under test here is the wiring.
const gateLegacyDDL = `
ALTER TABLE schedules ALTER COLUMN history_complete_from DROP NOT NULL;
ALTER TABLE schedules ADD COLUMN IF NOT EXISTS l1_rotation_start TIMESTAMPTZ;
CREATE TABLE IF NOT EXISTS rotation_epochs (
	id          TEXT PRIMARY KEY,
	schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
	layer       TEXT NOT NULL,
	user_ids    TEXT NOT NULL,
	start_time  TIMESTAMPTZ NOT NULL,
	end_time    TIMESTAMPTZ,
	created_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);
`

// run executes the binary to completion and returns everything it printed.
func (g *gateEnv) run(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(g.env, "INTERNAL_PORT="+freePort(t))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// requireServes starts the binary as a server and waits for it to answer.
//
// The assertion is an answer rather than "it did not exit within N seconds":
// a timer would pass for a process stuck before ever listening. The health
// endpoint is on INTERNAL_PORT, which is configurable; the main port is
// hardcoded to 8080 and its being taken is only logged, so a busy 8080 on the
// build machine cannot make this flap.
func (g *gateEnv) requireServes(t *testing.T, bin string) {
	t.Helper()

	port := freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(g.env, "INTERNAL_PORT="+port)
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:" + port + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			t.Fatalf("the server exited on a database that had been reset:\n%s", stderr)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the server never answered on /health:\n%s", stderr)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}
