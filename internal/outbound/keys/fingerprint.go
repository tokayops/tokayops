package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	batchFingerprintProtocol      = "outbound_batch_fingerprint/v1"
	completionFingerprintProtocol = "outbound_completion_fingerprint/v1"
)

// BatchFingerprintV1 is the version of the batch protocol below, stored beside
// every fingerprint it produces: a digest does not say which encoder made it.
const BatchFingerprintV1 = 1

// CompletionFingerprintV1 is the same for an attempt's conclusion. It is
// stamped on the attempt when it starts, not when it finishes: an attempt can
// outlive a deployment, and the encoder that closes it has to be the one that
// opened it.
const CompletionFingerprintV1 = 1

// CurrentBatchFingerprintVersion and CurrentCompletionFingerprintVersion are
// what a new row is stamped with. They exist so no caller has to write the
// number itself - a literal 1 spread across the codebase is a literal that will
// still be there when there is a 2.
func CurrentBatchFingerprintVersion() int { return BatchFingerprintV1 }

func CurrentCompletionFingerprintVersion() int { return CompletionFingerprintV1 }

func checkBatchFingerprintVersion(version int) error {
	if version != BatchFingerprintV1 {
		return contractf("batch fingerprint version %d is not one this build can encode", version)
	}
	return nil
}

func checkCompletionFingerprintVersion(version int) error {
	if version != CompletionFingerprintV1 {
		return contractf("completion fingerprint version %d is not one this build can encode",
			version)
	}
	return nil
}

// AdmissionOutcome is what a producer is proposing: to admit this work, or to
// record that there is nobody to send it to.
//
// It is part of the proposal and therefore part of its fingerprint. Two
// producers that disagree about whether anyone can be reached disagree about
// the work, and the second one has to be told so rather than be answered
// "already accepted".
type AdmissionOutcome string

const (
	OutcomeAdmitted  AdmissionOutcome = "admitted"
	OutcomeNoTargets AdmissionOutcome = "no_targets"
)

var admissionOutcomes = map[AdmissionOutcome]bool{
	OutcomeAdmitted: true, OutcomeNoTargets: true,
}

// TimingKind says what a delay is measured from.
type TimingKind string

const (
	// TimingRelativeToAdmission is an offset from the moment the work was
	// admitted, and the admitting transaction turns it into a wall-clock time
	// using the database's own clock.
	//
	// The offset is what enters the fingerprint, never the resulting instant: a
	// repeat of the same admission a minute later would otherwise propose a
	// different absolute time and be told it conflicts with itself.
	TimingRelativeToAdmission TimingKind = "relative_to_admission"

	// TimingAbsolute is a time that comes from the domain and cannot move - the
	// start of an on-call assignment, for instance. Never a process clock.
	TimingAbsolute TimingKind = "absolute"

	// TimingBounded is a deadline that is the earlier of two things: an instant
	// the domain supplies, and an age measured from admission.
	//
	// It exists because neither half alone is a deadline for an announcement
	// nobody can acknowledge. The domain instant answers "this is about a shift
	// that has ended"; the age answers "this has been true for long enough that
	// saying it is no longer news". Both atoms enter the fingerprint and the
	// RESULT does not: the result is computed from the database's clock at
	// admission and differs between two attempts at the same admission, so a
	// repeat carrying it would be told it conflicts with itself.
	TimingBounded TimingKind = "bounded"
)

// TimingSpec is when a commitment becomes due, in the form that stays the same
// across repeats.
//
// Each variant uses one field and refuses the other. A relative spec carrying
// an instant, or an absolute one carrying an offset, is a caller who believes
// something is part of the identity that the encoding would drop - and the two
// beliefs would differ without anyone finding out.
type TimingSpec struct {
	Kind   TimingKind
	Offset time.Duration
	At     time.Time
	// MaxAge is the second half of a bounded deadline, and is used by no other
	// kind. Absent from the encoding of the other two, so their bytes are what
	// they always were.
	MaxAge time.Duration
}

// Validate reports whether this spec is one the grammar can encode. Exported
// because the admission gate has to refuse a malformed deadline before it
// becomes durable, and re-stating the rule there would be a second copy of it.
func (t TimingSpec) Validate() error { return t.validate() }

func (t TimingSpec) validate() error {
	switch t.Kind {
	case TimingRelativeToAdmission:
		if t.Offset < 0 {
			return contractf("a negative admission offset %s", t.Offset)
		}
		if !t.At.IsZero() {
			return contractf("a relative timing spec also carrying an instant")
		}
		if t.MaxAge != 0 {
			return contractf("a relative timing spec also carrying a maximum age")
		}
	case TimingAbsolute:
		if t.At.IsZero() {
			return contractf("an absolute timing spec with no time")
		}
		if t.Offset != 0 {
			return contractf("an absolute timing spec also carrying an offset")
		}
		if t.MaxAge != 0 {
			return contractf("an absolute timing spec also carrying a maximum age")
		}
	case TimingBounded:
		if t.At.IsZero() {
			return contractf("a bounded deadline with no domain instant")
		}
		if t.MaxAge <= 0 {
			return contractf("a bounded deadline with a maximum age of %s", t.MaxAge)
		}
		if t.Offset != 0 {
			return contractf("a bounded deadline also carrying an offset")
		}
	default:
		return contractf("unknown timing kind %q", t.Kind)
	}
	return nil
}

func (t TimingSpec) encode(buf *bytes.Buffer) error {
	if err := t.validate(); err != nil {
		return err
	}
	encStr(buf, string(t.Kind))
	switch t.Kind {
	case TimingRelativeToAdmission:
		enc(buf, int64Bytes(int64(t.Offset)))
	case TimingBounded:
		// Both atoms, in a fixed order. The two older kinds keep exactly the
		// bytes they had: this is an extension of the field, not a new shape
		// for it, and every existing escalation fingerprint has to survive it.
		enc(buf, int64Bytes(t.At.UTC().UnixNano()))
		enc(buf, int64Bytes(int64(t.MaxAge)))
	default:
		enc(buf, int64Bytes(t.At.UTC().UnixNano()))
	}
	return nil
}

// contentRef says what a commitment is about, without saying it twice.
//
// For an escalation it is the group's revision plus the digest of that
// revision's snapshot. The revision alone is not enough: it identifies the
// content only once a winner has been picked, and a fingerprint compares
// proposals before that. The digest is what tells two proposals for the same
// revision apart.
//
// Unexported because it is derived from the batch that carries all three
// values; a caller able to supply it per commitment could put commitments about
// two different revisions into one admission.
type contentRef struct {
	AlertGroupID   string
	Revision       int64
	SnapshotDigest []byte
}

func (c contentRef) encode(buf *bytes.Buffer) error {
	if c.AlertGroupID == "" {
		return contractf("a content reference with no alert group")
	}
	if c.Revision < 0 {
		return contractf("revision %d is negative", c.Revision)
	}
	if len(c.SnapshotDigest) != sha256.Size {
		return contractf("a snapshot digest of %d bytes, expected %d",
			len(c.SnapshotDigest), sha256.Size)
	}
	encStr(buf, "revision")
	encStr(buf, c.AlertGroupID)
	enc(buf, int64Bytes(c.Revision))
	enc(buf, c.SnapshotDigest)
	return nil
}

// EscalationPayloadV1 is what an escalation handler executes from.
//
// It is derived from the commitment rather than supplied beside it: the target
// and the slot appear in the key as well, and two independently written copies
// would let a delivery be deduplicated as one message and sent as another.
type EscalationPayloadV1 struct {
	Slot            Slot    `json:"slot"`
	Target          Target  `json:"target"`
	MessageOverride *string `json:"message_override,omitempty"`
	Interactive     bool    `json:"interactive"`
}

// SchemaVersion is the payload schema this shape belongs to. It is stored on
// the commitment, so a later schema is readable rather than guessed at.
func (p EscalationPayloadV1) SchemaVersion() int { return 1 }

// DecodeEscalationPayloadV1 reads a STORED payload, and is the only way to do
// it.
//
// Strict on both halves, because both halves have already gone wrong somewhere
// else. Unknown fields are refused rather than dropped: a payload written by a
// build that knows more than this one would otherwise be rendered as if the
// extra instruction was not there. And the closed sets are checked rather than
// assumed valid because they were validated at ADMISSION, by a different build,
// possibly years ago - what is on the row now is what decides what goes out.
func DecodeEscalationPayloadV1(schemaVersion int, raw []byte) (EscalationPayloadV1, error) {
	var payload EscalationPayloadV1
	if schemaVersion != payload.SchemaVersion() {
		return payload, contractf("payload schema %d is not one this build renders",
			schemaVersion)
	}
	if len(raw) == 0 {
		return payload, contractf("a commitment with no payload")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return payload, contractf("the payload cannot be read: %v", err)
	}
	// A second Decode rather than More(): outside an array or an object, More()
	// answers "is the next token something other than ] or }", so a stray
	// closing brace after a perfectly good payload reads as "nothing follows".
	// What has to be true is that the input ENDED.
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload, contractf("the payload does not end where the value does")
	}

	if err := payload.Slot.validate(); err != nil {
		return payload, err
	}
	// A person or a channel. The grammar also knows subscribers, and an
	// escalation aimed at one would be a message nothing in Slack or Telegram
	// can be handed.
	if err := payload.Target.addressedTo(TargetChannel, TargetUser); err != nil {
		return payload, err
	}
	return payload, nil
}

func (p EscalationPayloadV1) encode(buf *bytes.Buffer) error {
	if err := p.Target.addressedTo(TargetChannel, TargetUser); err != nil {
		return err
	}
	if err := p.Slot.encode(buf); err != nil {
		return err
	}
	encStr(buf, string(p.Target.Kind))
	encStr(buf, p.Target.Ref)
	encOpt(buf, p.MessageOverride)
	encBool(buf, p.Interactive)
	return nil
}

// submitIntent is one commitment as it is proposed - everything that decides
// whether two proposals are the same one - in the frozen wire form its
// fingerprint is taken over.
//
// Unexported: it is built by Admit from an EscalationCommitment, so the values
// it shares with the business key and the payload are the same values.
type submitIntent struct {
	Kind            Kind
	GrammarVersion  int
	Provider        string
	Target          Target
	Operation       Operation
	Editable        bool
	Content         contentReference
	Timing          TimingSpec
	Expiry          *TimingSpec
	CompletionMode  CompletionMode
	AmbiguityPolicy AmbiguityPolicy
	Payload         Payload
}

// contentReference is what a commitment is about, in whichever shape its kind
// has one. Both shapes are TAGGED with a literal of their own, so the bytes of
// one can never be read as the bytes of the other.
type contentReference interface {
	encode(buf *bytes.Buffer) error
}

// occurrenceRef is what a handover announcement is about: the shift change,
// and nothing else. There is no revision and no snapshot to point at.
type occurrenceRef struct {
	Digest []byte
}

func (o occurrenceRef) encode(buf *bytes.Buffer) error {
	if len(o.Digest) != sha256.Size {
		return contractf("an occurrence reference of %d bytes, expected %d",
			len(o.Digest), sha256.Size)
	}
	encStr(buf, "occurrence")
	enc(buf, o.Digest)
	return nil
}

// encode writes the frozen wire form of one proposed commitment.
//
// Numbered fields in a fixed order, because a fingerprint has to survive the
// struct being reorganised: adding a durable field is a new version with its
// own encoder, never an edit to this one.
func (s submitIntent) encode() ([]byte, error) {
	if err := checkVersion(s.Kind, s.GrammarVersion); err != nil {
		return nil, err
	}
	if s.Provider == "" {
		return nil, contractf("a proposal with no provider")
	}
	if err := s.Target.validate(); err != nil {
		return nil, err
	}
	if err := s.Operation.validate(); err != nil {
		return nil, err
	}
	if err := s.CompletionMode.validate(); err != nil {
		return nil, err
	}
	if err := s.AmbiguityPolicy.validate(); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	tagged(&buf, 1, func(b *bytes.Buffer) { encStr(b, string(s.Kind)) })
	tagged(&buf, 2, func(b *bytes.Buffer) { encStr(b, versionLiteral(s.GrammarVersion)) })
	tagged(&buf, 3, func(b *bytes.Buffer) { encStr(b, s.Provider) })
	tagged(&buf, 4, func(b *bytes.Buffer) { encStr(b, string(s.Target.Kind)) })
	tagged(&buf, 5, func(b *bytes.Buffer) { encStr(b, s.Target.Ref) })
	tagged(&buf, 6, func(b *bytes.Buffer) { encStr(b, string(s.Operation)) })
	tagged(&buf, 7, func(b *bytes.Buffer) { encBool(b, s.Editable) })

	var failed error
	tagged(&buf, 8, func(b *bytes.Buffer) { failed = s.Content.encode(b) })
	if failed != nil {
		return nil, failed
	}
	tagged(&buf, 9, func(b *bytes.Buffer) { failed = s.Timing.encode(b) })
	if failed != nil {
		return nil, failed
	}
	tagged(&buf, 10, func(b *bytes.Buffer) {
		if s.Expiry == nil {
			encStr(b, "0")
			return
		}
		encStr(b, "1")
		failed = s.Expiry.encode(b)
	})
	if failed != nil {
		return nil, failed
	}

	tagged(&buf, 11, func(b *bytes.Buffer) { encStr(b, string(s.CompletionMode)) })
	tagged(&buf, 12, func(b *bytes.Buffer) { encStr(b, string(s.AmbiguityPolicy)) })
	tagged(&buf, 13, func(b *bytes.Buffer) { enc(b, int64Bytes(int64(s.Payload.SchemaVersion()))) })
	tagged(&buf, 14, func(b *bytes.Buffer) { failed = s.Payload.encode(b) })
	if failed != nil {
		return nil, failed
	}

	return buf.Bytes(), nil
}

// batchFingerprint is the identity of the proposed content of an admission.
//
// The outcome and the content reference are hashed BEFORE the list, and that
// order is what makes an empty admission identifiable: with nothing but the
// list, two proposals that found nobody to notify would hash the same however
// different the content behind them was, and the second producer would be told
// its proposal was already accepted.
func batchFingerprint(version int, outcome AdmissionOutcome, content []byte, encoded [][]byte) ([]byte, error) {
	if err := checkBatchFingerprintVersion(version); err != nil {
		return nil, err
	}
	if !admissionOutcomes[outcome] {
		return nil, contractf("admission outcome %q is not one this protocol knows", outcome)
	}
	if outcome == OutcomeNoTargets && len(encoded) > 0 {
		return nil, contractf("an admission with no targets carrying %d commitments", len(encoded))
	}
	if outcome == OutcomeAdmitted && len(encoded) == 0 {
		return nil, contractf("an admission of nothing")
	}
	// The content reference is optional in the protocol - encOptBytes below
	// writes absence as its own marker - and a webhook admission has none: what
	// its commitments are about is the event, which is already the claim. When
	// one IS given it is a digest, and only a whole one.
	if content != nil && len(content) != sha256.Size {
		return nil, contractf("a content reference of %d bytes, expected %d",
			len(content), sha256.Size)
	}

	// Sorted by bytes so the fingerprint describes the SET that was proposed:
	// the order two commitments came out of a plan is not part of what was
	// promised, and letting it in would make a reordered repeat a conflict.
	ordered := append([][]byte(nil), encoded...)
	sortBytewise(ordered)

	var material bytes.Buffer
	encStr(&material, batchFingerprintProtocol)
	encStr(&material, string(outcome))
	encOptBytes(&material, content)
	encList(&material, ordered)

	sum := sha256.Sum256(material.Bytes())
	return sum[:], nil
}

// AttemptOutcome is the transport result of one network call - what is known
// about the call, not what it means for the commitment.
type AttemptOutcome string

const (
	OutcomeAccepted           AttemptOutcome = "accepted"
	OutcomeRetryableRejection AttemptOutcome = "retryable_rejection"
	OutcomePermanentRejection AttemptOutcome = "permanent_rejection"
	OutcomeAmbiguous          AttemptOutcome = "ambiguous"
	OutcomeCanceled           AttemptOutcome = "canceled"
)

var attemptOutcomes = map[AttemptOutcome]bool{
	OutcomeAccepted: true, OutcomeRetryableRejection: true,
	OutcomePermanentRejection: true, OutcomeAmbiguous: true, OutcomeCanceled: true,
}

// ProviderResultDetail is the reserved set of things a reconciliation can
// prove. It is closed for the same reason every other enum here is: the value
// travels inside a fingerprint, and one spelled from outside the set would give
// two different conclusions the same identity.
//
// Nothing produces these yet - no provider this build talks to can be asked
// what happened - but the field is part of the protocol, so its values are
// stated rather than left open.
type ProviderResultDetail string

const (
	DetailAcceptanceProven ProviderResultDetail = "acceptance_proven"
	DetailDeliveryProven   ProviderResultDetail = "delivery_proven"
	DetailDefinitelyAbsent ProviderResultDetail = "definitely_absent"
	DetailInconclusive     ProviderResultDetail = "inconclusive"
)

var providerResultDetails = map[ProviderResultDetail]bool{
	DetailAcceptanceProven: true, DetailDeliveryProven: true,
	DetailDefinitelyAbsent: true, DetailInconclusive: true,
}

// Completion is the result of an attempt, in the form its fingerprint is taken
// over.
//
// Timings and the free-text summary are deliberately absent: a repeated
// finalisation after a lost commit reply has to match the first one exactly,
// and anything that moves between the two would turn an idempotent repeat into
// a conflict.
type Completion struct {
	Outcome              AttemptOutcome
	ErrorClass           *string
	ProviderStatus       *string
	ReceiptRef           *string
	AppliedRevision      *int64
	ProviderResultDetail *ProviderResultDetail
}

// ReceiptRefOrEmpty is the name of the object this attempt settled, or nothing
// when it settled none.
func (c Completion) ReceiptRefOrEmpty() string {
	if c.ReceiptRef == nil {
		return ""
	}
	return *c.ReceiptRef
}

// Fingerprint identifies what an attempt concluded, under the protocol version
// the attempt was started with.
//
// The version is a parameter rather than an assumption: it is stamped on the
// attempt at the moment it starts, and a finalisation that quietly used
// today's encoder on an attempt opened by yesterday's would compare bytes from
// two protocols - and call an idempotent repeat a conflict.
func (c Completion) Fingerprint(version int) ([]byte, error) {
	if err := checkCompletionFingerprintVersion(version); err != nil {
		return nil, err
	}
	if !attemptOutcomes[c.Outcome] {
		return nil, contractf("attempt outcome %q is not one this protocol knows", c.Outcome)
	}
	if c.AppliedRevision != nil && *c.AppliedRevision < 0 {
		return nil, contractf("applied revision %d is negative", *c.AppliedRevision)
	}
	if c.ProviderResultDetail != nil && !providerResultDetails[*c.ProviderResultDetail] {
		return nil, contractf("provider result detail %q is not one this protocol knows",
			*c.ProviderResultDetail)
	}

	var material bytes.Buffer
	encStr(&material, completionFingerprintProtocol)
	encStr(&material, string(c.Outcome))
	encOpt(&material, c.ErrorClass)
	encOpt(&material, c.ProviderStatus)
	encOpt(&material, c.ReceiptRef)
	encOptInt64(&material, c.AppliedRevision)
	if c.ProviderResultDetail == nil {
		encOpt(&material, nil)
	} else {
		detail := string(*c.ProviderResultDetail)
		encOpt(&material, &detail)
	}

	sum := sha256.Sum256(material.Bytes())
	return sum[:], nil
}

// Clone is a completion with none of its pointers shared.
//
// Every optional field here is a pointer, so passing one of these by value
// hands out the insides as well: the receiver can empty the receipt reference
// or move the revision of a completion somebody else is about to fingerprint.
// The values are small and the copy is cheap; the alternative is a value object
// that is not one.
func (c Completion) Clone() Completion {
	clone := Completion{Outcome: c.Outcome}
	if c.ErrorClass != nil {
		value := *c.ErrorClass
		clone.ErrorClass = &value
	}
	if c.ProviderStatus != nil {
		value := *c.ProviderStatus
		clone.ProviderStatus = &value
	}
	if c.ReceiptRef != nil {
		value := *c.ReceiptRef
		clone.ReceiptRef = &value
	}
	if c.AppliedRevision != nil {
		value := *c.AppliedRevision
		clone.AppliedRevision = &value
	}
	if c.ProviderResultDetail != nil {
		value := *c.ProviderResultDetail
		clone.ProviderResultDetail = &value
	}
	return clone
}
