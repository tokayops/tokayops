package outbound

import (
	"testing"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The three things a kind decides, over every kind there is - and a refusal,
// not a default, for one there is not.
func TestWhatAKindDecidesIsClosed(t *testing.T) {
	cases := []struct {
		kind      keys.Kind
		names     bool
		door      effectDoor
		operation Operation
	}{
		{keys.KindEscalation, true, doorResolveAmbiguity, OperationSend},
		{keys.KindEscalationReplay, true, doorResolveAmbiguity, OperationSend},
		{keys.KindHandoff, true, doorResolveAmbiguity, OperationSend},
		{keys.KindWebhookEvent, false, doorReplay, OperationDeliver},
		{keys.KindWebhookReplay, false, doorReplay, OperationDeliver},
	}
	for _, tc := range cases {
		names, err := AcceptanceNamesObject(tc.kind)
		if err != nil || names != tc.names {
			t.Errorf("%s: names %v (%v), want %v", tc.kind, names, err, tc.names)
		}
		door, err := newEffectDoor(tc.kind)
		if err != nil || door != tc.door {
			t.Errorf("%s: door %q (%v), want %q", tc.kind, door, err, tc.door)
		}
		op, err := CreateOperationOf(tc.kind)
		if err != nil || op != tc.operation {
			t.Errorf("%s: operation %q (%v), want %q", tc.kind, op, err, tc.operation)
		}
	}

	for _, unknown := range []keys.Kind{"", "something_newer"} {
		if _, err := AcceptanceNamesObject(unknown); err == nil {
			t.Errorf("%q: acceptances judged for a kind nobody knows", unknown)
		}
		if _, err := newEffectDoor(unknown); err == nil {
			t.Errorf("%q: a door given to a kind nobody knows", unknown)
		}
		if _, err := CreateOperationOf(unknown); err == nil {
			t.Errorf("%q: an operation named for a kind nobody knows", unknown)
		}
	}
}
