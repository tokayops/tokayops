package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	_ "github.com/lib/pq"
)

// Several instances coming up against one database at the same moment.
//
// Every instance calls InitDB on start-up, so a rollout IS this test. The
// statements in there look idempotent one at a time and are not safe together:
// `CREATE TABLE IF NOT EXISTS` run by two backends at once has both of them
// find the table missing, and the loser fails on a duplicate key in `pg_type`.
// Guarded ALTERs have the same check-then-act gap. What holds the "start the
// instances in any order" contract is the lock, not the guards.

// TestInstancesStartingTogetherBuildTheSchemaOnce runs the real InitDB, on its
// own empty database, which is the only place the race exists: against a
// database that is already built every statement is a no-op and the test would
// prove nothing.
func TestInstancesStartingTogetherBuildTheSchemaOnce(t *testing.T) {
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set")
	}
	freshDSN := emptyDatabase(t, dsn)

	const instances = 6
	stores := make([]*Store, instances)
	for i := range stores {
		s, err := NewStore(freshDSN)
		if err != nil {
			t.Fatalf("connect instance %d: %v", i, err)
		}
		t.Cleanup(func() { s.Close() })
		stores[i] = s
	}

	var wg sync.WaitGroup
	errs := make([]error, instances)
	start := make(chan struct{})
	for i, s := range stores {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			<-start
			errs[i] = s.InitDB()
		}(i, s)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d failed to start: %v", i, err)
		}
	}

	// And what they built is the schema, not a partial one: the last table
	// InitDB creates is the outbound one, under its own lock at the very end.
	var tables int
	if err := stores[0].db.QueryRow(`
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_name IN ('alert_groups', 'timeline_events', 'escalation_policies',
		                     'integrations', 'event_outbox', 'outbound_batches',
		                     'outbound_intents', 'outbound_group_snapshots')`).
		Scan(&tables); err != nil {
		t.Fatalf("read the catalogue: %v", err)
	}
	if tables != 8 {
		t.Fatalf("the schema has %d of the 8 tables looked for", tables)
	}

	// The version column arrives under its current name, once.
	var versionColumns int
	if err := stores[0].db.QueryRow(`
		SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'alert_groups'
		  AND column_name IN ('render_source_version', 'slack_update_generation')`).
		Scan(&versionColumns); err != nil {
		t.Fatalf("read the catalogue: %v", err)
	}
	if versionColumns != 1 {
		t.Fatalf("alert_groups has %d version columns", versionColumns)
	}
}

// emptyDatabase makes a database of its own for the test and drops it after.
//
// Not the suite's database: InitDB carries one-shot data migrations that would
// run over the other tests' rows, and against an already-built schema the race
// this test is about cannot happen at all.
func emptyDatabase(t *testing.T, dsn string) string {
	t.Helper()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Skipf("TEST_DB_DSN is not a URL this test can rewrite: %v", err)
	}
	name := fmt.Sprintf("schema_race_%d", os.Getpid())

	admin := *parsed
	admin.Path = "/postgres"
	server, err := sql.Open("postgres", admin.String())
	if err != nil {
		t.Fatalf("connect to the server: %v", err)
	}
	defer server.Close()

	if _, err := server.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name)); err != nil {
		t.Fatalf("drop a leftover database: %v", err)
	}
	if _, err := server.Exec(`CREATE DATABASE ` + quoteIdent(name)); err != nil {
		t.Fatalf("create the database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", admin.String())
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + quoteIdent(name) + ` WITH (FORCE)`)
	})

	fresh := *parsed
	fresh.Path = "/" + name
	return fresh.String()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
