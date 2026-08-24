package outbound

import (
	"errors"
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// ErrNotAdmissible is what an admission is refused with.
//
// It is not retryable and it is not a race: it means the producer asked for a
// delivery this build cannot perform. Storing it anyway would create a
// commitment whose first attempt is guaranteed to fail, which is a promise the
// domain would have to break by design.
var ErrNotAdmissible = errors.New("outbound: not admissible")

func notAdmissiblef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotAdmissible, fmt.Sprintf(format, args...))
}

// notificationProviders is what the notification family can actually deliver
// through today.
//
// The list lives here rather than in the store because it is a property of the
// family, not of the schema, and it moves into the family policy when that
// arrives. What matters now is that it exists somewhere: without it a typo in a
// provider name becomes a durable commitment that fails on every attempt until
// somebody notices.
var notificationProviders = map[string]bool{
	"slack":    true,
	"telegram": true,
}

// DeliversThrough reports whether this build has a channel for a provider.
//
// Exported because the producer has to ask BEFORE it promises: a step naming a
// provider nothing delivers through is refused here, and refusing it refuses
// the whole admission - the alert would lose its firehose over one misspelled
// step, and retry that forever. Asked in advance, the step is dropped, the
// history says why, and the rest of the escalation goes out.
func DeliversThrough(provider string) bool { return notificationProviders[provider] }

// ValidateEscalationAdmission is the gate a commitment has to pass to become
// durable.
//
// Each rule here corresponds to a capability this build does not have. They are
// checked at admission rather than at delivery because that is the last moment
// a refusal costs nothing: afterwards the promise exists, and the only ways out
// of it are a failed delivery and an operator.
func ValidateEscalationAdmission(adm keys.Admission, now time.Time) error {
	if adm.BatchKey == "" {
		return notAdmissiblef("an admission with no claim")
	}
	if len(adm.Fingerprint) == 0 {
		return notAdmissiblef("an admission with no fingerprint")
	}

	for _, c := range adm.Commitments {
		if !notificationProviders[c.Provider] {
			return notAdmissiblef("provider %q is not one this build delivers through", c.Provider)
		}

		if c.CompletionMode != keys.CompletionOnAcceptance {
			// A channel whose acceptance only means "queued" needs somewhere to
			// wait for the provider's own confirmation, and there is nowhere to
			// wait yet. Admitting one would leave a commitment that can never
			// be completed.
			return notAdmissiblef(
				"completion mode %q needs provider receipts, which this build cannot receive",
				c.CompletionMode)
		}

		switch c.AmbiguityPolicy {
		case keys.PolicyRetry, keys.PolicyManualReview:
		case keys.PolicyReconcileThenRetry:
			// The policy promises a reconciliation before the retry, and no
			// provider here can be asked what happened. Admitting it would
			// promise a check that never runs.
			return notAdmissiblef(
				"reconcile_then_retry needs a provider that can be asked what happened")
		case keys.PolicyAssumeAccepted:
			if c.Editable {
				// Assuming delivery of a card leaves nothing to update
				// afterwards: it would be frozen at whatever it said when the
				// doubt began, with the domain claiming it is fine.
				return notAdmissiblef(
					"assume_accepted cannot be automatic for an editable commitment")
			}
		default:
			return notAdmissiblef("unknown ambiguity policy %q", c.AmbiguityPolicy)
		}

		if c.Expiry != nil {
			if err := c.Expiry.Validate(); err != nil {
				return notAdmissiblef("expiry: %v", err)
			}
			if c.Expiry.Kind == keys.TimingAbsolute && !c.Expiry.At.After(now) {
				// A deadline already in the past would make the commitment
				// expire on its first claim, which is a delivery that never
				// happens and a terminal state nobody asked for.
				return notAdmissiblef("a deadline in the past: %s", c.Expiry.At)
			}
		}
	}

	return nil
}
