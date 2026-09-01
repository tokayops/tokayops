package outbound

import (
	"fmt"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a kind of claim decides about its own deliveries, in two closed
// functions.
//
// Both are by KIND and not by channel, and the difference is the whole point. A
// channel that could declare "my acceptances name nothing" or "my commitments
// are revived by replay" would be declaring it in the one place nobody reviewing
// that channel would look for a rule about duplicates - and the cost of getting
// either wrong is a second message beside the first. Here they are stated once,
// beside every other thing a kind decides, and an unknown kind is refused
// rather than defaulted: a build meeting a row from a newer one has no business
// guessing how that row's deliveries behave.

// AcceptanceNamesObject says whether an accepted call has to come back with the
// coordinates of what it made.
//
// A message in Slack or Telegram is an object: it is edited later, compared
// against, found again. An acceptance that will not name it leaves a commitment
// that looks unsent, and the next revision creates a second message beside the
// first - which is why such an acceptance is a breach and is recorded as doubt.
// A POST to a subscriber makes nothing that can be named: there are no
// coordinates, nothing is edited later, and asking for them would turn every
// successful delivery into doubt and every doubt into a duplicate.
//
// Exported because the rule lives in three places, and the third is the store:
// the store refuses a finalisation that created something and did not say what,
// and it has to ask the same question of the same kind.
func AcceptanceNamesObject(kind keys.Kind) (bool, error) {
	switch kind {
	case keys.KindEscalation, keys.KindEscalationReplay, keys.KindHandoff:
		return true, nil
	case keys.KindWebhookEvent, keys.KindWebhookReplay:
		return false, nil
	default:
		return false, fmt.Errorf("outbound: %q is not a kind whose acceptances this build can judge", kind)
	}
}

// effectDoor is which command may start a NEW external effect for a commitment
// that has ended.
type effectDoor string

const (
	// doorResolveAmbiguity: an operator's retry_new_generation on the same
	// commitment, with the guards of the state machine.
	doorResolveAmbiguity effectDoor = "resolve_ambiguity"

	// doorReplay: a new commitment under the operator's request id. The old one
	// stays ended.
	doorReplay effectDoor = "replay"
)

// newEffectDoor says which door a kind has, and there is exactly one.
//
// Two doors to the same effect are two live commitments: a replay reads a
// terminal commitment, an operator revives it, the replay creates a second one
// beside it - and no lock on the original prevents that, because reviving it
// AFTER the replay committed would be perfectly legal. So the webhook kinds have
// replay and nothing else, and what follows is the property the replay relies
// on: a webhook commitment that has ended cannot come back to a state that
// makes network calls.
func newEffectDoor(kind keys.Kind) (effectDoor, error) {
	switch kind {
	case keys.KindEscalation, keys.KindEscalationReplay, keys.KindHandoff:
		return doorResolveAmbiguity, nil
	case keys.KindWebhookEvent, keys.KindWebhookReplay:
		return doorReplay, nil
	default:
		return "", fmt.Errorf("outbound: %q is not a kind whose effects this build can restart", kind)
	}
}

// CreateOperationOf is the verb a commitment's FIRST external effect is asked
// for, by kind.
//
// A message is sent; an event is delivered. The verb travels into the journal
// and into the operation label of the attempt metric, and "delivery to a
// subscriber" against "a message to a person" is the difference that label
// exists to show. A change to an existing message is never a first effect and
// is decided elsewhere, from the state the change applies.
func CreateOperationOf(kind keys.Kind) (Operation, error) {
	switch kind {
	case keys.KindEscalation, keys.KindEscalationReplay, keys.KindHandoff:
		return OperationSend, nil
	case keys.KindWebhookEvent, keys.KindWebhookReplay:
		return OperationDeliver, nil
	default:
		return "", fmt.Errorf("outbound: %q is not a kind whose deliveries this build can name", kind)
	}
}
