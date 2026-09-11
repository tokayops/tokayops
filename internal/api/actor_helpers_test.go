package api

import (
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/outbound"
)

// byUser is a person as the journal names them, for the tests that decide,
// admit or acknowledge as one.
func byUser(id string) outbound.Actor {
	actor, err := outbound.UserActor(id)
	if err != nil {
		panic(err)
	}
	return actor
}

// actorNamed is a person acting on an alert, with the same id and name.
func actorNamed(id string) alertgroup.Actor {
	return alertgroup.Actor{ID: id, Name: id}
}
