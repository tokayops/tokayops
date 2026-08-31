package store

import (
	"os"
	"path/filepath"
	"testing"
)

// The cutover file is not run by the application, so nothing else would notice
// if it stopped applying - or if a start-up put back what it removes.
//
// Both halves are asserted here against a database this build has already
// created: the statements run, and a start afterwards leaves the tables gone.
// The second half is the one that matters. Every one of those CREATE TABLE IF
// NOT EXISTS statements was harmless while the tables were in use and would
// silently undo this file the moment somebody restarted the process.

func TestTheCutoverLeavesTheJobEngineGone(t *testing.T) {
	s := setupTestDB(t)

	path := filepath.Join("..", "..", "migrations", "drop-job-engine.sql")
	statements, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if _, err := s.db.Exec(string(statements)); err != nil {
		t.Fatalf("run the cutover: %v", err)
	}

	// A start on the build that ships this file - the whole of it, not the
	// outbound half: every one of these tables was created by the main schema
	// block, and a check that skipped it would pass while a start put them
	// straight back.
	if err := s.InitDB(); err != nil {
		t.Fatalf("start after the cutover: %v", err)
	}

	for _, table := range []string{"jobs", "job_stages", "job_steps",
		"notification_deliveries", "job_dedup_policies"} {
		var present bool
		if err := s.db.QueryRow(`
			SELECT EXISTS (SELECT 1 FROM information_schema.tables
			               WHERE table_name = $1)`, table).Scan(&present); err != nil {
			t.Fatalf("look for %s: %v", table, err)
		}
		if present {
			t.Errorf("%s is back after a start; the schema still creates it", table)
		}
	}

	for _, column := range []string{"slack_update_pending", "ack_processed_at"} {
		if hasColumn(t, s, "alert_groups", column) {
			t.Errorf("alert_groups.%s is back after a start; the schema still adds it", column)
		}
	}

	// And the product still works on the result: an alert group is read
	// through the same statement every reader uses, which is what would break
	// if a dropped column were still in its column list.
	agID := outboundGroup(t, s)
	group, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("read an alert group after the cutover: %v", err)
	}
	if group == nil {
		t.Fatal("the alert group is gone")
	}
}
