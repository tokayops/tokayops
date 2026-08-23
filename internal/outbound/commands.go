package outbound

import (
	"encoding/json"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The commands the domain offers, and what each of them can answer.
//
// Every result is a closed set rather than a boolean or an error string: the
// caller of an admission has to be able to tell "somebody already accepted this
// exact work" from "somebody accepted different work under the same claim", and
// a bool cannot say that. The same goes for every other operation here - the
// outcomes are the vocabulary the workflows above are written in.

// EscalationAdmission is what a producer hands the store: the identities the
// grammar derived, plus the two things about the alert group that are settled
// at the same moment.
type EscalationAdmission struct {
	// Admission carries the batch key, the fingerprint, the snapshot and every
	// commitment with its key and payload. It is produced in one pass by the
	// grammar so the parts cannot disagree.
	Admission keys.Admission

	// PolicyID and PolicySnapshot are the escalation policy this admission was
	// built against, recorded on the group by the winner of the admission and
	// by nobody else.
	PolicyID       string
	PolicySnapshot json.RawMessage

	// StepsWithoutRecipients names the plan steps that resolved to nobody.
	// They produce no commitment - a commitment that must fail is an alert
	// about a failure the system knew about in advance - so the history is
	// where they are recorded.
	StepsWithoutRecipients []string

	Actor string
}

// SubmitOutcome is what happened to an admission.
type SubmitOutcome string

const (
	// SubmitCreated: this producer's set was accepted.
	SubmitCreated SubmitOutcome = "created"

	// SubmitExisting: the same set was already accepted, by this producer or
	// another. An idempotent repeat, and the normal answer to a retry after a
	// lost reply.
	SubmitExisting SubmitOutcome = "existing"

	// SubmitConflict: a DIFFERENT set was already accepted under this claim.
	// The first one stands; this one is refused rather than merged, because
	// merging two audiences is how somebody gets paged who was never chosen.
	SubmitConflict SubmitOutcome = "conflict"

	// SubmitGroupNotAdmitted: the alert group moved on - acknowledged, resolved
	// - before this admission got there. The user won.
	SubmitGroupNotAdmitted SubmitOutcome = "group_not_admitted"
)

// SubmitResult is the answer to an admission, with what was accepted.
type SubmitResult struct {
	Outcome SubmitOutcome
	BatchID string
	// IntentIDs are the commitments this claim holds, in key order, whether
	// they were created now or found already there.
	IntentIDs []string
}

// Expired is one commitment whose deadline passed before anything was sent.
type Expired struct {
	IntentID     string
	AlertGroupID string
}

// Recovered is one commitment whose worker died with an attempt open.
type Recovered struct {
	IntentID  string
	AttemptID string
	To        Status
	Row       string
}

// ProviderDue is the demand of one provider, as the scheduler and the metric
// need it - which is not the same number.
type ProviderDue struct {
	Provider string
	// ClaimableDue and ClaimableFresh count what a claim could actually take:
	// the deadline is alive and no other instance holds a lease. Handing a
	// share of the pool to work that cannot be taken is how a worker ends up
	// idle with a queue in front of it.
	ClaimableDue   int
	ClaimableFresh int
	// LatenessSeconds is measured over ALL due work, leased or not, and is
	// computed by the database in the same statement. A row claimed and never
	// begun is exactly what a health signal must not hide.
	LatenessSeconds float64
}

// ClaimPhase is which half of a provider's queue a claim is asking for.
type ClaimPhase string

const (
	// ClaimFirstAttempts takes commitments nobody has tried yet, so a freshly
	// admitted page does not queue behind a pile of old retries.
	ClaimFirstAttempts ClaimPhase = "first_attempts"

	// ClaimAny takes whatever is due. Retries are guaranteed a share of every
	// claim, or a steady stream of new work would starve them for good.
	ClaimAny ClaimPhase = "any"
)

// Leased is a commitment this worker now holds, with the token that proves it.
type Leased struct {
	Intent      Intent
	LeaseToken  string
	LockedUntil time.Time
}
