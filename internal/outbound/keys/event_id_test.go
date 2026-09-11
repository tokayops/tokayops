package keys

import (
	"strings"
	"testing"
	"time"
)

// The event id is the prefix of every webhook key - <event_id>:<hex> - and the
// only thing an upgrade has to read out of a key that was written before the
// claim carried its event beside it. A colon in the id would make that prefix
// ambiguous, so the grammar refuses it at the door rather than letting a key
// be written that nothing can read back.

func webhookBatchFor(eventID string) WebhookBatch {
	return WebhookBatch{
		Kind: KindWebhookEvent, EventID: eventID, EventType: "alert_group.firing",
		Body: `{"event":"alert_group.firing"}`, IntegrationIDs: []string{"int-a"},
		Expiry: 24 * time.Hour, GrammarVersion: GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
	}
}

func TestAnEventIDWithAColonIsRefusedAtTheDoor(t *testing.T) {
	_, err := webhookBatchFor("evt:1").Admit()
	if err == nil {
		t.Fatal("an event id with a colon was admitted")
	}
	if !strings.Contains(err.Error(), "reserves") {
		t.Fatalf("refused, but not by the grammar: %v", err)
	}
	if _, err := webhookBatchFor("").Admit(); err == nil {
		t.Fatal("an empty event id was admitted")
	}
}

// TestTheAdmissionNamesItsEvent: the claim carries the event id for both
// kinds, so the store can write it beside the key. Every other kind leaves it
// empty - the schema insists on exactly that pairing.
func TestTheAdmissionNamesItsEvent(t *testing.T) {
	fanOut, err := webhookBatchFor("evt-1").Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if fanOut.EventID != "evt-1" {
		t.Fatalf("the fan-out's admission names event %q, want evt-1", fanOut.EventID)
	}

	replay := webhookBatchFor("evt-1")
	replay.Kind = KindWebhookReplay
	replay.ClientRequestID = "key-1"
	admitted, err := replay.Admit()
	if err != nil {
		t.Fatalf("admit the replay: %v", err)
	}
	if admitted.EventID != "evt-1" {
		t.Fatalf("the replay's admission names event %q, want evt-1", admitted.EventID)
	}
	if admitted.BatchKey == fanOut.BatchKey {
		t.Fatal("a replay and a fan-out took the same claim")
	}
	if !strings.HasPrefix(admitted.BatchKey, "evt-1:") || !strings.HasPrefix(fanOut.BatchKey, "evt-1:") {
		t.Fatalf("the keys do not start with the event id: %q, %q", fanOut.BatchKey, admitted.BatchKey)
	}
}
