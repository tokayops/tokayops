package keys

import (
	"crypto/sha256"
	"sort"
)

// Operation is what a commitment asks a provider to do. Closed set: the verb is
// part of every identity it appears in, and one spelled differently would be a
// different key for the same act.
type Operation string

const (
	OperationSend    Operation = "send"
	OperationUpdate  Operation = "update"
	OperationResolve Operation = "resolve"
	OperationDeliver Operation = "deliver"
)

func (o Operation) validate() error {
	switch o {
	case OperationSend, OperationUpdate, OperationResolve, OperationDeliver:
		return nil
	default:
		return contractf("unknown operation %q", o)
	}
}

// CompletionMode says what acceptance by the provider proves.
type CompletionMode string

const (
	// CompletionOnAcceptance: the provider taking the message is the delivery.
	CompletionOnAcceptance CompletionMode = "on_acceptance"
	// CompletionOnProviderReceipt: acceptance only means queued, and the
	// delivery is confirmed later by the provider itself.
	CompletionOnProviderReceipt CompletionMode = "on_provider_receipt"
)

func (m CompletionMode) validate() error {
	switch m {
	case CompletionOnAcceptance, CompletionOnProviderReceipt:
		return nil
	default:
		return contractf("unknown completion mode %q", m)
	}
}

// AmbiguityPolicy is which way to be wrong when the outcome of a call is not
// known: risk a duplicate, or risk not delivering.
type AmbiguityPolicy string

const (
	PolicyRetry              AmbiguityPolicy = "retry"
	PolicyReconcileThenRetry AmbiguityPolicy = "reconcile_then_retry"
	PolicyManualReview       AmbiguityPolicy = "manual_review"
	PolicyAssumeAccepted     AmbiguityPolicy = "assume_accepted"
)

func (p AmbiguityPolicy) validate() error {
	switch p {
	case PolicyRetry, PolicyReconcileThenRetry, PolicyManualReview, PolicyAssumeAccepted:
		return nil
	default:
		return contractf("unknown ambiguity policy %q", p)
	}
}

// EscalationCommitment is one promise to notify one recipient: everything that
// says what it is, stated once.
//
// The recipient, the slot and the provider are given here and nowhere else.
// Everything derived from them - the business key that deduplicates the
// commitment, the fingerprint that decides whether two admissions propose the
// same thing, and the payload the handler executes - is built from these fields
// together, so the thing deduplicated and the thing sent cannot be two
// different things.
type EscalationCommitment struct {
	Slot     Slot
	Provider string
	Target   Target
	// Editable says whether the external effect can be brought to a later
	// revision - a card that will be updated, as opposed to a message that is
	// sent once.
	Editable bool
	// MessageOverride is the text a policy step supplies instead of the default
	// wording. Absent and empty are different: a step that deliberately sends
	// an empty message is not a step that supplies none.
	MessageOverride *string
	// Interactive says whether this message may carry action buttons. Frozen at
	// admission because it depends on the provider's configuration, and a
	// message whose buttons come and go between attempts is two different
	// external effects under one key.
	Interactive     bool
	Timing          TimingSpec
	Expiry          *TimingSpec
	CompletionMode  CompletionMode
	AmbiguityPolicy AmbiguityPolicy
}

func (c EscalationCommitment) validate() error {
	if err := c.Slot.validate(); err != nil {
		return err
	}
	if c.Provider == "" {
		return contractf("an escalation commitment with no provider")
	}
	if err := c.Target.addressedTo(TargetChannel, TargetUser); err != nil {
		return err
	}
	if err := c.Timing.validate(); err != nil {
		return err
	}
	if c.Expiry != nil {
		if err := c.Expiry.validate(); err != nil {
			return err
		}
	}
	if err := c.CompletionMode.validate(); err != nil {
		return err
	}
	return c.AmbiguityPolicy.validate()
}

// EscalationBatch is one admission as a producer proposes it: what content it
// is about, and every commitment it wants accepted.
//
// The content comes in as the snapshot itself rather than as a group id, a
// revision and a digest beside it. Those three are already inside the snapshot,
// and a batch free to state them again is a batch that can store the content of
// one group under the admission of another - with a digest belonging to
// neither. Here they are read off the snapshot, so the admission and the state
// it admits cannot describe different things.
//
// The kind lives on the batch, so a first admission and a re-admission cannot
// be mixed in one of them either.
type EscalationBatch struct {
	Kind Kind
	// ClientRequestID is the operator's idempotency key on a re-admission, and
	// empty otherwise.
	ClientRequestID string
	GrammarVersion  int
	// FingerprintVersion is the protocol the content fingerprint is taken
	// under. A new admission uses CurrentBatchFingerprintVersion; a repeat being
	// compared against a row already stored uses the version stored on it, or
	// the two would be compared across protocols.
	FingerprintVersion int
	Snapshot           RenderSnapshot
	Commitments        []EscalationCommitment
}

// AdmittedCommitment is one commitment with everything derived from it: the key
// it is deduplicated by and the payload it will be executed from.
type AdmittedCommitment struct {
	IdempotencyKey  string
	Provider        string
	Target          Target
	Slot            Slot
	Editable        bool
	Operation       Operation
	CompletionMode  CompletionMode
	AmbiguityPolicy AmbiguityPolicy
	Timing          TimingSpec
	Expiry          *TimingSpec
	// Payload is what the handler executes from, in the shape this kind of
	// claim has. Which shape it is follows from the kind, and the store keeps
	// PayloadSchemaVersion beside it so a later schema is readable rather than
	// guessed at.
	Payload              Payload
	PayloadSchemaVersion int
}

// Admission is a proposal reduced to what the database stores: one key for the
// claim, one fingerprint for its content, the snapshot that content is, and the
// commitments in the order they must be inserted.
//
// The snapshot travels back out with the admission for the same reason it
// travelled in: what is stored and what was hashed have to be the same value,
// and handing them to the caller separately is how they stop being.
type Admission struct {
	BatchKey              string
	Kind                  Kind
	AlertGroupID          string
	Revision              int64
	GrammarVersion        int
	Outcome               AdmissionOutcome
	Fingerprint           []byte
	FingerprintVersion    int
	Snapshot              RenderSnapshot
	SnapshotSchemaVersion int
	Commitments           []AdmittedCommitment

	// EventID is the alert event a webhook admission is about, and empty for
	// every other kind. It is the one durable link from an event to ALL of its
	// claims - the fan-out's and every replay's, whose keys differ - and the
	// store writes it beside the key so that the group's delivery history and
	// the retention of the event can find them without parsing a key.
	EventID string
}

// Admit derives every identity in one pass.
//
// The outcome is derived rather than declared: an admission with no commitments
// is an admission that found nobody to notify, and one with commitments is not.
// Two fields for one fact would eventually disagree.
//
// Commitments come back ordered by their key, which is the order they have to
// be inserted in: two producers racing on the same batch insert the same rows
// in the same order, so a violation of the key grammar surfaces as one
// deterministic unique-violation instead of a deadlock.
func (b EscalationBatch) Admit() (Admission, error) {
	if err := checkVersion(b.Kind, b.GrammarVersion); err != nil {
		return Admission{}, err
	}
	if err := checkBatchFingerprintVersion(b.FingerprintVersion); err != nil {
		return Admission{}, err
	}

	snapshot := b.Snapshot.content
	digest := b.Snapshot.digest
	if snapshot.AlertGroupID == "" || len(digest) != sha256.Size {
		// The zero value of a snapshot is the only invalid one that can reach
		// here - every other route goes through NewRenderSnapshot.
		return Admission{}, contractf("an admission with no snapshot to be about")
	}

	identity := BatchIdentity{
		Kind:            b.Kind,
		AlertGroupID:    snapshot.AlertGroupID,
		ClientRequestID: b.ClientRequestID,
	}
	batchKey, err := identity.BatchKey(b.GrammarVersion)
	if err != nil {
		return Admission{}, err
	}

	content := contentRef{
		AlertGroupID:   snapshot.AlertGroupID,
		Revision:       snapshot.Revision,
		SnapshotDigest: digest,
	}

	admitted := make([]AdmittedCommitment, 0, len(b.Commitments))
	encoded := make([][]byte, 0, len(b.Commitments))
	seen := make(map[string]bool, len(b.Commitments))

	for _, c := range b.Commitments {
		if err := c.validate(); err != nil {
			return Admission{}, err
		}

		key, err := escalationIntent{
			AlertGroupID:    snapshot.AlertGroupID,
			ClientRequestID: b.ClientRequestID,
			Slot:            c.Slot,
			Provider:        c.Provider,
			Target:          c.Target,
		}.key(b.Kind, b.GrammarVersion)
		if err != nil {
			return Admission{}, err
		}
		if seen[key] {
			// Two commitments with one key are one commitment proposed twice.
			// Accepting them would put a set with a duplicate into a
			// fingerprint that describes a set, and the repeat of this very
			// admission would then fail to match it.
			return Admission{}, contractf(
				"two commitments in one admission share the key %s", key)
		}
		seen[key] = true

		// Copied, not shared. A commitment whose override or expiry the caller
		// still holds a pointer to is a payload that can change after it was
		// fingerprinted - the executable half moving while the identity half
		// stands still.
		payload := EscalationPayloadV1{
			Slot:            c.Slot,
			Target:          c.Target,
			MessageOverride: cloneString(c.MessageOverride),
			Interactive:     c.Interactive,
		}
		expiry := cloneTiming(c.Expiry)

		material, err := submitIntent{
			Kind:            b.Kind,
			GrammarVersion:  b.GrammarVersion,
			Provider:        c.Provider,
			Target:          c.Target,
			Operation:       OperationSend,
			Editable:        c.Editable,
			Content:         content,
			Timing:          c.Timing,
			Expiry:          expiry,
			CompletionMode:  c.CompletionMode,
			AmbiguityPolicy: c.AmbiguityPolicy,
			Payload:         payload,
		}.encode()
		if err != nil {
			return Admission{}, err
		}

		admitted = append(admitted, AdmittedCommitment{
			IdempotencyKey:       key,
			Provider:             c.Provider,
			Target:               c.Target,
			Slot:                 c.Slot,
			Editable:             c.Editable,
			Operation:            OperationSend,
			CompletionMode:       c.CompletionMode,
			AmbiguityPolicy:      c.AmbiguityPolicy,
			Timing:               c.Timing,
			Expiry:               expiry,
			Payload:              payload,
			PayloadSchemaVersion: payload.SchemaVersion(),
		})
		encoded = append(encoded, material)
	}

	outcome := OutcomeAdmitted
	if len(admitted) == 0 {
		outcome = OutcomeNoTargets
	}

	fingerprint, err := batchFingerprint(b.FingerprintVersion, outcome, digest, encoded)
	if err != nil {
		return Admission{}, err
	}

	sort.Slice(admitted, func(i, j int) bool {
		return admitted[i].IdempotencyKey < admitted[j].IdempotencyKey
	})

	return Admission{
		BatchKey:              batchKey,
		Kind:                  b.Kind,
		AlertGroupID:          snapshot.AlertGroupID,
		Revision:              snapshot.Revision,
		GrammarVersion:        b.GrammarVersion,
		Outcome:               outcome,
		Fingerprint:           fingerprint,
		FingerprintVersion:    b.FingerprintVersion,
		Snapshot:              b.Snapshot,
		SnapshotSchemaVersion: RenderSnapshotSchemaV1,
		Commitments:           admitted,
	}, nil
}

// cloneTiming copies an optional timing spec rather than the pointer to it.
func cloneTiming(t *TimingSpec) *TimingSpec {
	if t == nil {
		return nil
	}
	copied := *t
	return &copied
}
