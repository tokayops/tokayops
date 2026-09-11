package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

// newThrowawayDB gives the caller a database of its own, created empty.
//
// Two different needs land here. Tests that assert what a database looks like
// when InitDB runs on it for the FIRST time cannot use the shared one, where
// TestMain has already run InitDB (main_test.go) and any neighbour may have
// reshaped it since - "fresh" there is a fiction. And tests that reshape the
// schema must not do it to a database anything else will use afterwards: a
// dropped column keeps its slot forever, and on a shared database that damage
// accumulates across runs.
func newThrowawayDB(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open the admin connection: %v", err)
	}
	defer admin.Close()

	// One database per test. The name carries a common prefix so a leftover
	// from a killed run is recognizable as test scaffolding.
	name := fmt.Sprintf("tokay_tmp_%x", time.Now().UnixNano())
	// nosemgrep: string-formatted-query - the name is generated here, not user input
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("drop a stale %s: %v", name, err)
	}
	// nosemgrep: string-formatted-query - the name is generated here, not user input
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Skipf("cannot create a throwaway database (%v); "+
			"TEST_DB_DSN must name a user allowed to CREATE DATABASE", err)
	}

	s, err := NewStore(replaceDBName(t, dsn, name))
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	t.Cleanup(func() {
		s.Close()
		dropper, err := sql.Open("postgres", dsn)
		if err != nil {
			t.Errorf("reopen to drop %s: %v", name, err)
			return
		}
		defer dropper.Close()
		// nosemgrep: string-formatted-query - the name is generated here, not user input
		if _, err := dropper.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	return s
}

func replaceDBName(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DB_DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}
