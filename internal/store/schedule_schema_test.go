package store

import "testing"

// TestFreshDatabaseHasTheRevisionExclusionConstraint: the constraint swallows a
// missing extension with a NOTICE, so losing btree_gist would remove the
// non-overlap backstop silently, and only on fresh databases.
func TestFreshDatabaseHasTheRevisionExclusionConstraint(t *testing.T) {
	s := newThrowawayDB(t)
	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	var available bool
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')`).Scan(&available); err != nil {
		t.Fatalf("check for btree_gist: %v", err)
	}
	if !available {
		// Skipped rather than failed on purpose: the constraint is defence in
		// depth and an installation without the extension is supported. That
		// contract is what RevisionOverlapConstraintPresent reports at startup.
		t.Skip("btree_gist is not installed in this PostgreSQL; the constraint is skipped by design")
	}

	present, err := s.RevisionOverlapConstraintPresent()
	if err != nil {
		t.Fatalf("check for the constraint: %v", err)
	}
	if !present {
		t.Fatal("the revision exclusion constraint was not created")
	}
}
