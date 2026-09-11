package outbound

import (
	"errors"
	"testing"
)

// The actor is closed at the constructor, not at the type: Component is a
// string, so any package can spell one, and what keeps the set closed is that
// SystemActor refuses a spelling it does not know. A valid Actor is what
// cannot be made outside this package.

func TestAComponentOutsideTheSetIsRefused(t *testing.T) {
	if _, err := SystemActor(Component("other")); !errors.Is(err, ErrActorContract) {
		t.Fatalf("a component this system does not have was accepted: %v", err)
	}
	if _, err := SystemActor(Component("")); !errors.Is(err, ErrActorContract) {
		t.Fatalf("an empty component was accepted: %v", err)
	}
	for _, c := range Components() {
		actor, err := SystemActor(c)
		if err != nil {
			t.Fatalf("%s is a component and was refused: %v", c, err)
		}
		if actor.Kind() != ActorKindSystem || actor.Ref() != string(c) || actor.IsZero() {
			t.Fatalf("%s builds %s", c, actor)
		}
	}
	if len(Components()) != 7 {
		t.Fatalf("%d components, want the seven of S5-D3", len(Components()))
	}
}

func TestAUserActorNeedsAnID(t *testing.T) {
	if _, err := UserActor(""); !errors.Is(err, ErrActorContract) {
		t.Fatalf("a user with no id was accepted: %v", err)
	}
	actor, err := UserActor("u-1")
	if err != nil {
		t.Fatal(err)
	}
	if actor.Kind() != ActorKindUser || actor.Ref() != "u-1" || actor.String() != "user:u-1" {
		t.Fatalf("a user builds %s", actor)
	}
}

// TestNobodyBuildsALegacyActor: the zero value is the only Actor another
// package can make, and it is nobody - not a legacy line, which only the
// backfill writes.
func TestNobodyBuildsALegacyActor(t *testing.T) {
	var nobody Actor
	if !nobody.IsZero() || nobody.Kind() != "" || nobody.String() != "nobody" {
		t.Fatalf("the zero actor reads %s", nobody)
	}
	kinds := ActorKinds()
	if len(kinds) != 3 || kinds[2] != ActorKindLegacy {
		t.Fatalf("the kinds are %v", kinds)
	}
	for _, prebuilt := range []Actor{ActorEngine, ActorNotifier, ActorFanOut, ActorWorker, ActorRecovery, ActorErasure, ActorSystem} {
		if prebuilt.Kind() != ActorKindSystem {
			t.Fatalf("%s is not a component", prebuilt)
		}
	}
}
