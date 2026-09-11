package outbound

import (
	"errors"
	"fmt"
)

// Who wrote a line of a commitment's journal. Three kinds, one column beside
// the reference: a person, named by the id of the user; a component of this
// system, named from a closed set; and the text a build before this one wrote,
// which is read as text and never as either of the other two - a person whose
// display name was "system" was written by an acknowledgement, and the journal
// must not promote them to a component because of what they were called.

// ErrActorContract is an actor that cannot be built: a user with no id, or a
// component this system does not have.
var ErrActorContract = errors.New("outbound: actor contract violation")

// ActorKind is the form of an actor, and the vocabulary of actor_kind.
type ActorKind string

const (
	// ActorKindUser is a person; the reference is the id of the user.
	ActorKindUser ActorKind = "user"
	// ActorKindSystem is a component of this system; the reference is its name.
	ActorKindSystem ActorKind = "system"
	// ActorKindLegacy is a line written before the kind existed. The reference
	// is whatever that build wrote, and nothing in this build writes it: it is
	// the backfill's answer for every line it cannot attribute by its write
	// path.
	ActorKindLegacy ActorKind = "legacy"
)

// ActorKinds is the closed set, in one place, for the schema and the tests.
func ActorKinds() []ActorKind {
	return []ActorKind{ActorKindUser, ActorKindSystem, ActorKindLegacy}
}

// Component is a part of this system that writes journal lines on its own.
type Component string

const (
	ComponentEngine   Component = "engine"
	ComponentNotifier Component = "notifier"
	ComponentFanOut   Component = "fanout"
	ComponentWorker   Component = "worker"
	ComponentRecovery Component = "recovery"
	ComponentErasure  Component = "erasure"
	// ComponentSystem is the ingester acting on an alert - a merge, a clearing
	// - which the alert domain has always signed "system".
	ComponentSystem Component = "system"
)

// Components is the closed set.
func Components() []Component {
	return []Component{
		ComponentEngine, ComponentNotifier, ComponentFanOut, ComponentWorker,
		ComponentRecovery, ComponentErasure, ComponentSystem,
	}
}

// Actor is who wrote a journal line. It is built only by UserActor and
// SystemActor, and its fields are its own: a value of it made anywhere else is
// the zero value, which every writer refuses.
type Actor struct {
	kind ActorKind
	ref  string
}

// UserActor is a person, by the id of the user. An empty id is a contract
// violation: a line signed by nobody is not a line signed by a person.
func UserActor(id string) (Actor, error) {
	if id == "" {
		return Actor{}, fmt.Errorf("%w: a user actor needs the id of the user", ErrActorContract)
	}
	return Actor{kind: ActorKindUser, ref: id}, nil
}

// SystemActor is a component, from the closed set and no other.
func SystemActor(c Component) (Actor, error) {
	for _, known := range Components() {
		if c == known {
			return Actor{kind: ActorKindSystem, ref: string(c)}, nil
		}
	}
	return Actor{}, fmt.Errorf("%w: %q is not a component of this system", ErrActorContract, c)
}

// The components, as actors, for the writers that are one.
var (
	ActorEngine   = mustSystemActor(ComponentEngine)
	ActorNotifier = mustSystemActor(ComponentNotifier)
	ActorFanOut   = mustSystemActor(ComponentFanOut)
	ActorWorker   = mustSystemActor(ComponentWorker)
	ActorRecovery = mustSystemActor(ComponentRecovery)
	ActorErasure  = mustSystemActor(ComponentErasure)
	ActorSystem   = mustSystemActor(ComponentSystem)
)

func mustSystemActor(c Component) Actor {
	a, err := SystemActor(c)
	if err != nil {
		panic(err)
	}
	return a
}

// Kind is the form: user, system or legacy.
func (a Actor) Kind() ActorKind { return a.kind }

// Ref is the id of the user, the name of the component, or the legacy text.
func (a Actor) Ref() string { return a.ref }

// IsZero is an actor nobody built.
func (a Actor) IsZero() bool { return a.kind == "" }

func (a Actor) String() string {
	if a.IsZero() {
		return "nobody"
	}
	return string(a.kind) + ":" + a.ref
}
