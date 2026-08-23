package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Family is the execution partition a delivery is claimed under. Claims are
// taken per family so one kind of backlog cannot delay another; the only family
// with a grammar today is the notification one.
type Family string

const FamilyNotification Family = "notification"

// Kind is the identity grammar a key is written in.
type Kind string

const (
	// KindEscalation is the first admission of an alert group's escalation.
	KindEscalation Kind = "escalation"

	// KindEscalationReplay is an operator re-admitting a group whose first
	// admission found nobody to notify. It carries the operator's request id,
	// so two decisions about one group are two admissions rather than one.
	//
	// There is no producer for it yet. The grammar exists now because the
	// alternative - one that cannot express a re-admission - is what would make
	// the promise of one impossible to keep later.
	KindEscalationReplay Kind = "escalation_replay"
)

// GrammarV1 is the only version either escalation grammar has.
//
// It is stored on every row it identifies and read back before those rows are
// compared: a digest does not say which encoder produced it, so the version has
// to travel as data. Introducing a second version is a stop-the-world protocol
// change, not a rolling deploy - two instances writing different versions would
// each fail to find the other's rows and admit the same work twice.
const GrammarV1 = 1

// supportedVersions answers per KIND rather than globally.
//
// A version belongs to the grammar of one kind: changing how escalations are
// identified has nothing to do with how a re-admission is. A single global list
// would hand every other kind a version it never defined the moment one of them
// gained a second.
func supportedVersions(kind Kind) ([]int, error) {
	switch kind {
	case KindEscalation:
		return []int{GrammarV1}, nil
	case KindEscalationReplay:
		return []int{GrammarV1}, nil
	default:
		return nil, contractf("unknown key kind %q", kind)
	}
}

// checkVersion refuses a version the given kind does not define.
//
// Reading a stored version and quietly encoding with the current one would
// compare bytes from two different grammars and call the difference a conflict
// - or, worse, call two different things the same.
func checkVersion(kind Kind, version int) error {
	versions, err := supportedVersions(kind)
	if err != nil {
		return err
	}
	for _, supported := range versions {
		if version == supported {
			return nil
		}
	}
	return contractf("%s has no grammar version %d, only %v", kind, version, versions)
}

// CurrentGrammarVersion is the version a new key of this kind is written
// under. Callers ask rather than write the number: a literal 1 spread across
// producers is a literal that will still be there when there is a 2.
func CurrentGrammarVersion(kind Kind) (int, error) {
	versions, err := supportedVersions(kind)
	if err != nil {
		return 0, err
	}
	return versions[0], nil
}

// MaxClientRequestID bounds an operator's idempotency key. The limit exists so
// the key stays inside a b-tree entry; past it the insert fails and the request
// it identifies cannot be recorded at all, so it is refused up front instead.
const MaxClientRequestID = 128

// familyOf answers which worker partition a kind is executed in. It is derived
// rather than passed in: a caller free to name both is a caller free to write a
// notification key into the webhook partition.
func familyOf(kind Kind) (Family, error) {
	switch kind {
	case KindEscalation, KindEscalationReplay:
		return FamilyNotification, nil
	default:
		return "", contractf("unknown key kind %q", kind)
	}
}

// versionLiteral is how a version number is spelled inside a key. Derived from
// the stored integer so the column and the bytes cannot drift apart.
func versionLiteral(version int) string {
	return "v" + strconv.Itoa(version)
}

// opening writes the three fields every business key starts with: the partition
// it executes in, the grammar it is written in, and that grammar's version.
func opening(buf *bytes.Buffer, kind Kind, version int) error {
	family, err := familyOf(kind)
	if err != nil {
		return err
	}
	encStr(buf, string(family))
	encStr(buf, string(kind))
	encStr(buf, versionLiteral(version))
	return nil
}

// digestKey is the shape every key in this package has: a readable prefix, a
// colon, and the hex digest of the material.
//
// The prefix is not decoration. The row this key lands in outlives everything
// else about the delivery, and when somebody finds it years later it has to say
// whose it is without a join. The digest is what keeps the key bounded: a
// spelled-out roster has no upper length, and a b-tree entry does.
func digestKey(prefix string, material []byte) string {
	sum := sha256.Sum256(material)
	return prefix + ":" + hex.EncodeToString(sum[:])
}

// SlotKind says which part of the escalation plan a step came from.
type SlotKind string

const (
	// SlotFirehose is the channel every alert of a severity goes to. It has no
	// index: there is one of it.
	SlotFirehose SlotKind = "firehose"

	// SlotPolicy is a numbered step of the escalation policy, numbered by the
	// policy snapshot frozen at admission rather than by anything the runtime
	// counts. Runtime numbering restarts per fan-out, and two steps aimed at one
	// person would collapse into a single commitment under it.
	SlotPolicy SlotKind = "policy"
)

// Slot identifies the step of the plan a commitment came from.
//
// Index belongs to the policy variant only, and a firehose slot carrying one is
// refused rather than ignored: a value the encoding drops is a value the caller
// believes is part of the identity, and the two beliefs would differ in silence.
type Slot struct {
	Kind  SlotKind
	Index int
}

func (s Slot) validate() error {
	switch s.Kind {
	case SlotFirehose:
		if s.Index != 0 {
			return contractf("the firehose slot has no index, got %d", s.Index)
		}
	case SlotPolicy:
		if s.Index < 0 {
			return contractf("policy slot index %d is negative", s.Index)
		}
	default:
		return contractf("unknown slot kind %q", s.Kind)
	}
	return nil
}

func (s Slot) encode(buf *bytes.Buffer) error {
	if err := s.validate(); err != nil {
		return err
	}
	encStr(buf, string(s.Kind))
	if s.Kind == SlotFirehose {
		encStr(buf, "")
		return nil
	}
	enc(buf, int64Bytes(int64(s.Index)))
	return nil
}

// TargetKind is what sort of recipient a commitment is aimed at.
type TargetKind string

const (
	TargetChannel TargetKind = "channel"
	TargetUser    TargetKind = "user"
)

// Target is who a commitment is for, in one place.
//
// One place matters more than it looks. The recipient appears in the business
// key, in the fingerprint and in the payload a handler executes; three
// independently supplied copies would let a commitment be deduplicated as one
// delivery and executed as another.
type Target struct {
	Kind TargetKind
	// Ref is the recipient as this system names them - a user id, a channel id
	// - never the address the provider will be handed. That address belongs to
	// the attempt and can change without changing what was promised.
	Ref string
}

// Validate is the same check the grammar applies at admission, exported
// because a channel reading a STORED payload has to be able to make it: a row
// written by a build with a wider set of targets must be refused rather than
// half understood.
func (t Target) Validate() error { return t.validate() }

func (t Target) validate() error {
	switch t.Kind {
	case TargetChannel, TargetUser:
	default:
		return contractf("unknown target kind %q", t.Kind)
	}
	if t.Ref == "" {
		return contractf("a %s target with no recipient", t.Kind)
	}
	return nil
}

// escalationIntent is the identity of one commitment.
//
// Unexported on purpose: nothing outside this package builds a key on its own.
// Keys, fingerprints and payloads are derived together from one batch (see
// escalation.go), because a caller able to build them separately is a caller
// able to make them disagree - and then idempotency protects one external
// effect while the worker performs another.
type escalationIntent struct {
	AlertGroupID    string
	ClientRequestID string
	Slot            Slot
	Provider        string
	Target          Target
}

func (i escalationIntent) key(kind Kind, version int) (string, error) {
	if err := checkVersion(kind, version); err != nil {
		return "", err
	}
	if i.AlertGroupID == "" {
		return "", contractf("an escalation commitment with no alert group")
	}
	if i.Provider == "" {
		return "", contractf("an escalation commitment with no provider")
	}
	if err := i.Target.validate(); err != nil {
		return "", err
	}
	if err := requestIDFor(kind, i.ClientRequestID); err != nil {
		return "", err
	}

	var material bytes.Buffer
	if err := opening(&material, kind, version); err != nil {
		return "", err
	}
	encStr(&material, i.AlertGroupID)
	if kind == KindEscalationReplay {
		encStr(&material, i.ClientRequestID)
	}
	if err := i.Slot.encode(&material); err != nil {
		return "", err
	}
	encStr(&material, i.Provider)
	encStr(&material, string(i.Target.Kind))
	encStr(&material, i.Target.Ref)

	return digestKey(i.AlertGroupID, material.Bytes()), nil
}

// BatchIdentity names an admission - the claim a producer takes before any
// network call, and the row that makes taking it twice a no-op.
type BatchIdentity struct {
	Kind         Kind
	AlertGroupID string
	// ClientRequestID is the operator's idempotency key on a re-admission, and
	// empty otherwise.
	ClientRequestID string
}

func (b BatchIdentity) validate() error {
	if b.AlertGroupID == "" {
		return contractf("an admission with no alert group")
	}
	return requestIDFor(b.Kind, b.ClientRequestID)
}

// BatchKey returns the admission key under one version of the grammar.
func (b BatchIdentity) BatchKey(version int) (string, error) {
	if err := checkVersion(b.Kind, version); err != nil {
		return "", err
	}
	if err := b.validate(); err != nil {
		return "", err
	}

	var material bytes.Buffer
	if err := opening(&material, b.Kind, version); err != nil {
		return "", err
	}
	encStr(&material, b.AlertGroupID)
	if b.Kind == KindEscalationReplay {
		encStr(&material, b.ClientRequestID)
	}

	return digestKey(b.AlertGroupID, material.Bytes()), nil
}

// CandidateBatchKeys returns every key this admission could already be written
// under, newest grammar first.
//
// A repeat of an admission has to find the row the first attempt wrote, and
// that row may have been written by an encoder this build no longer treats as
// current. Looking only under the current version would create a second
// admission for work already accepted - the one failure this whole grammar
// exists to prevent.
func CandidateBatchKeys(b BatchIdentity) ([]string, error) {
	versions, err := supportedVersions(b.Kind)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(versions))
	for _, version := range versions {
		key, err := b.BatchKey(version)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, nil
}

// requestIDFor states in one place which kinds carry an operator's request id
// and which are refused for carrying one.
func requestIDFor(kind Kind, id string) error {
	switch kind {
	case KindEscalation:
		if id != "" {
			return contractf("a first admission carries no client request id")
		}
		return nil
	case KindEscalationReplay:
		if id == "" {
			return contractf("a re-admission with no client request id")
		}
		if len(id) > MaxClientRequestID {
			return contractf("client request id is %d bytes, the limit is %d",
				len(id), MaxClientRequestID)
		}
		return nil
	default:
		return contractf("unknown key kind %q", kind)
	}
}

// ProviderKeyCodecV1 is the version of the provider-key spelling below. It is
// stored on the commitment and for its whole lifetime, not per external effect:
// a mutation key has to stay stable across a recreated effect, and a codec that
// changed in between would hand the provider a different key for the same
// revision - which is exactly the mistake the key exists to expose.
const ProviderKeyCodecV1 = 1

// CreateKey identifies the external effect a commitment is trying to produce.
//
// Stable for as long as that effect is being attempted, so a retry after an
// ambiguous result asks the provider to create the same thing rather than a
// second one. A new effect - after a confirmed non-delivery, or an operator's
// decision - is a new generation and therefore a new key.
func CreateKey(intentID string, generation int, codecVersion int) (string, error) {
	if err := checkProviderCodec(codecVersion); err != nil {
		return "", err
	}
	if intentID == "" {
		return "", contractf("a create key with no commitment")
	}
	if generation < 0 {
		return "", contractf("generation %d is negative", generation)
	}

	var material bytes.Buffer
	encStr(&material, "outbound_create_key/"+versionLiteral(codecVersion))
	encStr(&material, intentID)
	enc(&material, int64Bytes(int64(generation)))
	return digestKey(intentID, material.Bytes()), nil
}

// MutationKey identifies one change to an effect that already exists.
//
// Keyed by the revision rather than by the generation on purpose: revisions are
// monotonic across the whole commitment, so applying one twice is the same key
// twice and the provider can refuse it. Adding the generation would make the
// second application look new, which is precisely the defect this key is meant
// to reveal.
func MutationKey(intentID string, operation Operation, revision int64, codecVersion int) (string, error) {
	if err := checkProviderCodec(codecVersion); err != nil {
		return "", err
	}
	if intentID == "" {
		return "", contractf("a mutation key with no commitment")
	}
	if err := operation.validate(); err != nil {
		return "", err
	}
	if revision < 0 {
		return "", contractf("revision %d is negative", revision)
	}

	var material bytes.Buffer
	encStr(&material, "outbound_mutation_key/"+versionLiteral(codecVersion))
	encStr(&material, intentID)
	encStr(&material, string(operation))
	enc(&material, int64Bytes(revision))
	return digestKey(intentID, material.Bytes()), nil
}

func checkProviderCodec(version int) error {
	if version != ProviderKeyCodecV1 {
		return contractf("provider key codec version %d is not one this build can encode",
			version)
	}
	return nil
}
