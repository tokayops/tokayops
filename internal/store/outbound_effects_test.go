package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound"
)

// The machine answers in effects, and every one of them has to be written by
// somebody. This is the bookkeeping that keeps that true as the domain grows:
// an effect added to the struct and forgotten in the store fails here rather
// than in a delivery, months later, as a notification that half happened.
//
// Each entry says where the effect is written, or why it is not the store's to
// write. Adding a field to outbound.Effects without deciding which of the two
// it is, is the failure this test exists for.
func TestEveryEffectIsAccountedFor(t *testing.T) {
	written := map[string]string{
		"NewGeneration":       "applyTransitionTx: bumps generation_no and releases the address, key and receipt with it",
		"ClearLease":          "applyTransitionTx: lease_token, locked_until, worker_id",
		"ClearCurrentAttempt": "applyTransitionTx: current_attempt_id",
		"ConsumeCancellation": "applyTransitionTx: cancellation_requested",
		"ScheduleRetry":       "applyTransitionTx: next_attempt_at, after the family's backoff",
		"ScheduleNow":         "applyTransitionTx: next_attempt_at = now()",
		"ResetFailureStreak":  "applyTransitionTx: failure_streak",
		"BumpFailureStreak":   "applyTransitionTx: failure_streak",
		"ApplyRevision":       "applyTransitionTx: applied_revision, final_revision_applied",
		"StoreReceipt":        "applyTransitionTx: receipt",
		"RecordDuplicateRisk": "applyTransitionTx: accepted_duplicate_risk, and an event saying so",
		"TriggerGroup":        "groupEffectsTx: the alert leaves processing",
		"Timeline":            "groupEffectsTx: the line the alert's history gets",
		"OpenGeneration":      "BeginAttempt: binds the address and key, guarded by beginEffectsUnderstood",
	}

	notDurable := map[string]string{
		// Proof is how the machine decided the wording and the risk flag. Both
		// of those ARE written; the label itself would be a third copy of the
		// same fact, disagreeing with the other two the first time somebody
		// edited one of them.
		"Proof": "already expressed by Timeline and RecordDuplicateRisk",

		// A signal for the metrics, not a row. The store has nothing to write
		// for it, and the failure is visible in the commitment's status and in
		// the alert's history either way.
		"RaiseFailureSignal": "observability, raised by the caller rather than stored",
	}

	fields := reflect.TypeOf(outbound.Effects{})
	for i := 0; i < fields.NumField(); i++ {
		name := fields.Field(i).Name
		_, isWritten := written[name]
		_, isSkipped := notDurable[name]

		switch {
		case isWritten && isSkipped:
			t.Errorf("effect %s is claimed both written and not durable", name)
		case !isWritten && !isSkipped:
			t.Errorf("effect %s is produced by the machine and nothing in the store writes it "+
				"- decide where it lands, or say here why it does not", name)
		}
	}

	for name := range written {
		if _, ok := fields.FieldByName(name); !ok {
			t.Errorf("%s is listed as written but the machine no longer produces it", name)
		}
	}
	for name := range notDurable {
		if _, ok := fields.FieldByName(name); !ok {
			t.Errorf("%s is listed as not durable but the machine no longer produces it", name)
		}
	}
}

// TestBeginRefusesAnEffectItCannotWrite is the guard behind the one transition
// the store applies by hand. It cannot be reached through the machine today,
// which is the point: it is what makes tomorrow's added effect impossible to
// miss.
func TestBeginRefusesAnEffectItCannotWrite(t *testing.T) {
	if err := beginEffectsUnderstood(outbound.Effects{OpenGeneration: true}); err != nil {
		t.Fatalf("opening an attempt was refused its own effect: %v", err)
	}
	for name, effects := range map[string]outbound.Effects{
		"a history line":   {Timeline: outbound.TimelineSent},
		"moving the alert": {TriggerGroup: true},
		"a receipt":        {StoreReceipt: true},
	} {
		if err := beginEffectsUnderstood(effects); err == nil {
			t.Errorf("starting an attempt silently swallowed %s", name)
		}
	}
}

// TestTheTwoRefusalsDoNotOverlap. The store refuses in two ways with opposite
// consequences: one ends the commitment, the other leaves it for a build that
// can serve it. A caller decides which by asking, and if an error answered yes
// to both then whichever question it asked first would decide the fate of the
// commitment - the reader, not the fault.
func TestTheTwoRefusalsDoNotOverlap(t *testing.T) {
	broken := undeliverablef("the state of %s no longer matches its digest", "ag-1")
	if !errors.Is(broken, ErrUndeliverable) {
		t.Fatal("a broken row does not say so")
	}
	if errors.Is(broken, ErrOutboundContract) {
		t.Fatal("a broken row also reads as a build that cannot handle a good one, " +
			"so a caller checking the contract first would leave it circling the queue")
	}

	behind := outboundContractf("schema version %d is not one this build renders", 2)
	if !errors.Is(behind, ErrOutboundContract) {
		t.Fatal("a build that cannot handle a row does not say so")
	}
	if errors.Is(behind, ErrUndeliverable) {
		t.Fatal("a build that is behind also reads as a broken row, " +
			"so one old instance could end work the rest of the fleet can do")
	}
}
