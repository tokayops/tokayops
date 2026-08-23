package outbound

import "context"

// Preparation is what a worker resolves before it asks to make a call: the
// address, the credentials, the configuration - everything that can fail
// without touching the provider.
//
// It happens outside the transaction and outside the domain, because the
// transaction that opens an attempt must not be waiting on anything but the
// database. What reaches the domain is the ANSWER.
//
// The fields are closed and the constructors below are the only way in. Held
// open they were an invitation to the two states this has no word for: ready
// with nowhere to send, and refused with no reason - the first sends a message
// into the void, the second leaves a journal entry that explains nothing.
type Preparation struct {
	outcome    PreparationOutcome
	endpoint   string
	errorClass string
	summary    string
}

// Preparer answers whether a call may be made, for one provider.
//
// It returns no error, and that is the contract rather than an omission: every
// way this can fail is one of the three outcomes. An infrastructure hiccup is
// transient, a missing integration or an unlinked identity is permanent, and a
// preparer that returned an error as well would be inventing a fourth state the
// journal has no word for.
type Preparer interface {
	Prepare(ctx context.Context, intent Intent) Preparation
}

// Ready is the preparation of a call that may go ahead.
//
// An empty address is NOT turned into a refusal here, and the difference
// matters. "This recipient has no address" is a fact about the world and a
// preparer says it with Impossible("identity_not_linked", ...), which ends the
// commitment. "Ready, and I forgot the address" is a bug in the adapter, and
// the store refuses it as a contract violation that changes nothing - so the
// work waits for a build that resolves addresses properly instead of being
// ended by one that does not.
//
// The same mistake had two different fates depending on which layer noticed it
// first. Now it has one, and it lives in the store where the refusal is
// already written.
func Ready(endpoint string) Preparation {
	return Preparation{outcome: PreparationReady, endpoint: endpoint}
}

// Impossible is a refusal nothing will fix on its own: no integration, an
// identity nobody linked, a configuration the provider rejects. It ends the
// commitment where a person will see it.
func Impossible(class, summary string) Preparation {
	return refusal(PreparationPermanent, class, summary)
}

// NotNow is a refusal that may resolve itself: a rate limit local to this
// instance, a configuration being reloaded, a dependency briefly unavailable.
// The commitment waits and tries again.
func NotNow(class, summary string) Preparation {
	return refusal(PreparationTransient, class, summary)
}

func refusal(outcome PreparationOutcome, class, summary string) Preparation {
	if class == "" {
		class = "unstated"
	}
	return Preparation{outcome: outcome, errorClass: class, summary: summary}
}

// Outcome is which of the three this is.
func (p Preparation) Outcome() PreparationOutcome { return p.outcome }

// Request turns a preparation into what the store is asked for. The mapping
// lives here so a caller cannot assemble half of it: an attempt begun with a
// preparation the worker forgot to report would be a call the journal describes
// as something else entirely.
func (p Preparation) Request(intentID, leaseToken, workerID string) BeginAttemptRequest {
	return BeginAttemptRequest{
		IntentID:      intentID,
		LeaseToken:    leaseToken,
		WorkerID:      workerID,
		Preparation:   p.outcome,
		BoundEndpoint: p.endpoint,
		ErrorClass:    p.errorClass,
		Summary:       p.summary,
	}
}
