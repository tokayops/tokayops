package outbound

import (
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// GroupDeliveries is one alert group's deliveries as they look from the
// outside: the paging commitments the group owns, and the group's alert events
// with the claims each was turned into and the webhook commitments under them.
//
// The two halves are shaped differently because they ARE different. A paging
// commitment belongs to the group - its row says so, and acknowledging the
// group withdraws it. A webhook commitment belongs to the subscriber and
// carries the group only through its event: the event is written by the
// group's transition, the fan-out turns it into one claim, and a replay turns
// it into another claim with its own key. So the webhook half starts from the
// events and goes through the claims, which is the only path that finds a
// replay and the only one that can show an event nobody subscribed to.
type GroupDeliveries struct {
	Paging []Intent
	Events []EventDeliveries
}

// EventDeliveries is one alert event with every claim taken on it.
//
// An event with no batches is one the fan-out has not reached yet. A batch
// with no deliveries is a fan-out that found no subscriber: the claim exists,
// says so, and promised nobody.
type EventDeliveries struct {
	EventID   string
	EventType string
	Status    string
	CreatedAt time.Time
	Batches   []BatchDeliveries
}

// BatchDeliveries is one claim on an event - the fan-out's, or a replay's -
// and the commitments it made.
type BatchDeliveries struct {
	BatchID     string
	Kind        keys.Kind
	Outcome     string
	IntentCount int
	AdmittedAt  time.Time
	Deliveries  []Intent
}
