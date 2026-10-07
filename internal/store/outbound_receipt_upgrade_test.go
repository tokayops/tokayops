package store

import "testing"

// TestAStartFromV030AddsTheWaitForTheProvidersWord is the upgrade of a v0.3.0
// installation, as far as the wait goes: one start adds the columns, the
// tables and the rule, with commitments already in the table - none of which
// waits, so the rule finds nothing to object to.
func TestAStartFromV030AddsTheWaitForTheProvidersWord(t *testing.T) {
	s := throwawayDatabase(t, "schema-v0.3.0.sql")
	if relationExists(t, s, "outbound_effects") || hasColumn(t, s, "outbound_intents", "receipt_deadline") {
		t.Fatal("the schema file is not v0.3.0's")
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused v0.3.0's database: %v", err)
	}

	for _, table := range []string{"outbound_effects", "outbound_provider_events"} {
		if !relationExists(t, s, table) {
			t.Errorf("the start did not add %s", table)
		}
	}
	for _, column := range []string{"receipt_deadline", "obligation_withdrawn_at",
		"obligation_withdrawn_reason", "obligation_withdrawn_actor"} {
		if !hasColumn(t, s, "outbound_intents", column) {
			t.Errorf("the start did not add outbound_intents.%s", column)
		}
	}
	if !hasColumn(t, s, "outbound_attempts", "bound_context") {
		t.Error("the start did not add outbound_attempts.bound_context")
	}
	var rules int
	if err := s.db.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname = $1`,
		outboundAwaitingHasWait).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != 1 {
		t.Fatalf("the start did not add %s", outboundAwaitingHasWait)
	}

	// And again: a second start changes nothing and fails on nothing.
	if err := s.InitDB(); err != nil {
		t.Fatalf("a second start: %v", err)
	}
}
