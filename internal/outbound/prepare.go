package outbound

import "context"

// Preparation is what a worker resolves before it asks to make a call: the
// address, the credentials, the configuration - everything that can fail
// without touching the provider.
//
// It happens outside the transaction and outside the domain, because the
// transaction that opens an attempt must not be waiting on anything but the
// database. What reaches the domain is the ANSWER.
type Preparation struct {
	Outcome PreparationOutcome

	// Endpoint is the provider's own address for this recipient, as resolved
	// now. It is a proposal: an effect that is already bound keeps the address
	// it opened with, and this one is ignored.
	Endpoint string

	// ErrorClass and Summary explain a refusal, and are what the journal keeps
	// in place of an attempt. They are the whole record of a call that provably
	// did not happen, so a refusal without them is a mystery later.
	ErrorClass string
	Summary    string
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
func Ready(endpoint string) Preparation {
	return Preparation{Outcome: PreparationReady, Endpoint: endpoint}
}

// Impossible is a refusal nothing will fix on its own: no integration, an
// identity nobody linked, a configuration the provider rejects. It ends the
// commitment where a person will see it.
func Impossible(class, summary string) Preparation {
	return Preparation{Outcome: PreparationPermanent, ErrorClass: class, Summary: summary}
}

// NotNow is a refusal that may resolve itself: a rate limit local to this
// instance, a configuration being reloaded, a dependency briefly unavailable.
// The commitment waits and tries again.
func NotNow(class, summary string) Preparation {
	return Preparation{Outcome: PreparationTransient, ErrorClass: class, Summary: summary}
}

// Request turns a preparation into what the store is asked for. The mapping
// lives here so a caller cannot assemble half of it: an attempt begun with a
// preparation the worker forgot to report would be a call the journal describes
// as something else entirely.
func (p Preparation) Request(intentID, leaseToken, workerID string) BeginAttemptRequest {
	return BeginAttemptRequest{
		IntentID:      intentID,
		LeaseToken:    leaseToken,
		WorkerID:      workerID,
		Preparation:   p.Outcome,
		BoundEndpoint: p.Endpoint,
		ErrorClass:    p.ErrorClass,
		Summary:       p.Summary,
	}
}
