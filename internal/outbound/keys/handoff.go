package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"
)

// The identity of a handover announcement.
//
// Inherited whole from the job engine that used to produce these, down to the
// domain separator the material opens with: the digest a running installation
// has already announced under has to stay the same digest, or the first upgrade
// announces every shift change a second time. The golden vector is what holds
// that, and it was carried across rather than recomputed.

// occurrenceProtocol is the domain separator of the occurrence material. It is
// the namespace the previous implementation wrote, kept for that reason alone.
const occurrenceProtocol = "handoff_occurrence"

// HandoffKind says which kind of shift change this is.
type HandoffKind string

const (
	// HandoffShiftChange is the ordinary one: a shift ended and another began.
	HandoffShiftChange HandoffKind = "handoff"

	// HandoffAddedToActiveShift is somebody joining a shift already in
	// progress. A different event about the same schedule, and deliberately a
	// different occurrence: the people it addresses are not the same people.
	HandoffAddedToActiveShift HandoffKind = "added_to_active_shift"
)

func (k HandoffKind) validate() error {
	switch k {
	case HandoffShiftChange, HandoffAddedToActiveShift:
		return nil
	default:
		return contractf("unknown handover kind %q", k)
	}
}

// Occurrence is the shift change an announcement is about.
//
// Every field is identity. Source and GroupID are both here because a rotation
// and an override that put the same people on duty at the same instant are two
// different events, and announcing one is not announcing the other.
type Occurrence struct {
	Kind       HandoffKind
	ScheduleID string
	// Source says which part of the schedule produced this - the rotation
	// itself, or an override laid over it.
	Source string
	// GroupID is the rotation group or override group the people came from.
	GroupID string
	// UserIDs is who is on duty. Sorted before encoding, so the order a
	// projection happened to return them in is not part of the identity.
	UserIDs []string
	// AssignmentStart is when the assignment takes effect - a domain instant,
	// never a process clock.
	AssignmentStart time.Time
	// RevisionID is the schedule revision the observation was made against.
	RevisionID string
}

func (o Occurrence) validate() error {
	if err := o.Kind.validate(); err != nil {
		return err
	}
	if o.ScheduleID == "" {
		return contractf("an occurrence with no schedule")
	}
	if o.AssignmentStart.IsZero() {
		return contractf("an occurrence with no assignment start")
	}
	for _, id := range o.UserIDs {
		if id == "" {
			return contractf("an occurrence naming an empty user")
		}
	}
	return nil
}

// Digest is the occurrence as bytes: sha256 over the material below, and the
// value the previous implementation produced for the same shift change.
func (o Occurrence) Digest() ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}

	var material bytes.Buffer
	encStr(&material, occurrenceProtocol)
	encStr(&material, string(o.Kind))
	encStr(&material, o.ScheduleID)
	encStr(&material, o.Source)
	encStr(&material, o.GroupID)

	users := make([][]byte, 0, len(o.UserIDs))
	for _, id := range o.UserIDs {
		users = append(users, []byte(id))
	}
	sortBytewise(users)
	encList(&material, users)

	enc(&material, int64Bytes(o.AssignmentStart.UTC().UnixNano()))
	encStr(&material, o.RevisionID)

	digest := sha256.Sum256(material.Bytes())
	return digest[:], nil
}

// Key is the occurrence as a string: the schedule, a colon and the digest.
//
// This string is the batch key, not the raw digest. The two are easy to
// confuse and are not interchangeable: the string is what a running
// installation already has stored, and it is also the one that says whose row
// it is without a join.
func (o Occurrence) Key() (string, error) {
	digest, err := o.Digest()
	if err != nil {
		return "", err
	}
	return o.ScheduleID + ":" + hex.EncodeToString(digest), nil
}

// handoffIntent is one announcement to one person through one channel.
//
// The recipient is the INTERNAL user id, not the address. One person linked to
// two channels gets two commitments; the same person relinking a channel does
// not get a third announcement about the same shift.
type handoffIntent struct {
	ScheduleID       string
	OccurrenceDigest []byte
	Provider         string
	UserID           string
}

func (i handoffIntent) key(version int) (string, error) {
	if err := checkVersion(KindHandoff, version); err != nil {
		return "", err
	}
	if i.ScheduleID == "" {
		return "", contractf("a handover commitment with no schedule")
	}
	if len(i.OccurrenceDigest) != sha256.Size {
		return "", contractf("a handover commitment whose occurrence digest is %d bytes, expected %d",
			len(i.OccurrenceDigest), sha256.Size)
	}
	if i.Provider == "" {
		return "", contractf("a handover commitment with no provider")
	}
	if i.UserID == "" {
		return "", contractf("a handover commitment with no recipient")
	}

	var material bytes.Buffer
	if err := opening(&material, KindHandoff, version); err != nil {
		return "", err
	}
	enc(&material, i.OccurrenceDigest)
	encStr(&material, i.Provider)
	encStr(&material, i.UserID)

	return digestKey(i.ScheduleID, material.Bytes()), nil
}

// HandoffPayloadV1 is what a channel draws a handover announcement from.
//
// Data, not text. The previous implementation composed one string in Slack's
// markup and sent it to every channel, so a Telegram reader saw `:mega:` and
// `*Backend*` literally. What a message looks like is the channel's business;
// what it says is this.
//
// The instants are instants. Formatted here they would carry this process's
// locale and time zone into a value two builds have to agree on byte for byte.
type HandoffPayloadV1 struct {
	Kind       HandoffKind `json:"kind"`
	TeamName   string      `json:"team_name"`
	ScheduleID string      `json:"schedule_id"`
	// Timezone is the IANA name the times are shown in. "Local" is refused for
	// the same reason it is in a render snapshot: it is a question about the
	// machine asking, and two machines would render one announcement
	// differently.
	Timezone string `json:"timezone"`
	// GridSlotStart, AssignmentStart and AssignmentEnd are stored as RFC 3339
	// in UTC and encoded as nanoseconds. The durable spelling and the canonical
	// material are two different wire forms of one value, and neither is
	// derived from the other at read time.
	GridSlotStart   time.Time `json:"grid_slot_start"`
	AssignmentStart time.Time `json:"assignment_start"`
	AssignmentEnd   time.Time `json:"assignment_end"`
	Target          Target    `json:"target"`
}

// SchemaVersion is the payload schema this shape belongs to.
func (p HandoffPayloadV1) SchemaVersion() int { return 1 }

func (p HandoffPayloadV1) validate() error {
	if err := p.Kind.validate(); err != nil {
		return err
	}
	if p.ScheduleID == "" {
		return contractf("a handover payload with no schedule")
	}
	if err := checkDisplayTimezone(p.Timezone); err != nil {
		return err
	}
	if p.AssignmentStart.IsZero() {
		return contractf("a handover payload with no assignment start")
	}
	if err := p.Target.validate(); err != nil {
		return err
	}
	// A handover is addressed to a person, always. The business key carries the
	// user id and NOT the target kind, so an announcement aimed at a channel
	// would share its key with one aimed at the person of the same id: two
	// different promises, deduplicated as one.
	if p.Target.Kind != TargetUser {
		return contractf("a handover payload addressed to a %q; a shift is taken by a person",
			p.Target.Kind)
	}
	return nil
}

// DecodeHandoffPayloadV1 reads a STORED handover payload, and is the only way
// to do it. Strict on both halves, for the reasons the escalation decoder gives.
func DecodeHandoffPayloadV1(schemaVersion int, raw []byte) (HandoffPayloadV1, error) {
	var payload HandoffPayloadV1
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
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload, contractf("the payload does not end where the value does")
	}
	if err := payload.validate(); err != nil {
		return payload, err
	}
	return payload, nil
}

func (p HandoffPayloadV1) encode(buf *bytes.Buffer) error {
	if err := p.validate(); err != nil {
		return err
	}
	encStr(buf, string(p.Kind))
	encStr(buf, p.TeamName)
	encStr(buf, p.ScheduleID)
	encStr(buf, p.Timezone)
	enc(buf, int64Bytes(p.GridSlotStart.UTC().UnixNano()))
	enc(buf, int64Bytes(p.AssignmentStart.UTC().UnixNano()))
	enc(buf, int64Bytes(p.AssignmentEnd.UTC().UnixNano()))
	encStr(buf, string(p.Target.Kind))
	encStr(buf, p.Target.Ref)
	return nil
}

// HandoffCommitment is one announcement this batch wants accepted.
type HandoffCommitment struct {
	Provider string
	// UserID is the recipient as this system names them. The address the
	// channel will be handed is preparation's business and is not identity.
	UserID          string
	Payload         HandoffPayloadV1
	Timing          TimingSpec
	Expiry          *TimingSpec
	CompletionMode  CompletionMode
	AmbiguityPolicy AmbiguityPolicy
}

func (c HandoffCommitment) validate() error {
	if c.Provider == "" {
		return contractf("a handover commitment with no provider")
	}
	if c.UserID == "" {
		return contractf("a handover commitment with no recipient")
	}
	if err := c.Payload.validate(); err != nil {
		return err
	}
	// The recipient is named twice - in the commitment and inside the payload -
	// and a row where the two disagree would announce to one person and be
	// deduplicated as an announcement to another.
	if c.Payload.Target.Ref != c.UserID {
		return contractf("a handover commitment for %s carrying a payload addressed to %s",
			c.UserID, c.Payload.Target.Ref)
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

// HandoffBatch is one shift change as a producer proposes it.
//
// The occurrence comes in whole rather than as a digest beside a schedule id:
// the batch key, the content reference and every commitment key are derived
// from it here, so they cannot describe different shift changes.
type HandoffBatch struct {
	Occurrence         Occurrence
	GrammarVersion     int
	FingerprintVersion int
	Commitments        []HandoffCommitment
}

// Admit derives every identity in one pass, exactly as an escalation does.
//
// The outcome is derived rather than declared, commitments come back ordered by
// key so two producers racing on one occurrence insert in one order, and a
// duplicate key inside the batch is refused rather than fingerprinted.
func (b HandoffBatch) Admit() (Admission, error) {
	if err := checkVersion(KindHandoff, b.GrammarVersion); err != nil {
		return Admission{}, err
	}
	if err := checkBatchFingerprintVersion(b.FingerprintVersion); err != nil {
		return Admission{}, err
	}

	digest, err := b.Occurrence.Digest()
	if err != nil {
		return Admission{}, err
	}
	batchKey, err := b.Occurrence.Key()
	if err != nil {
		return Admission{}, err
	}
	content := occurrenceRef{Digest: digest}

	admitted := make([]AdmittedCommitment, 0, len(b.Commitments))
	encoded := make([][]byte, 0, len(b.Commitments))
	seen := make(map[string]bool, len(b.Commitments))

	for _, c := range b.Commitments {
		if err := c.validate(); err != nil {
			return Admission{}, err
		}
		if c.Payload.ScheduleID != b.Occurrence.ScheduleID {
			return Admission{}, contractf(
				"a commitment about schedule %s in an admission about %s",
				c.Payload.ScheduleID, b.Occurrence.ScheduleID)
		}

		key, err := handoffIntent{
			ScheduleID:       b.Occurrence.ScheduleID,
			OccurrenceDigest: digest,
			Provider:         c.Provider,
			UserID:           c.UserID,
		}.key(b.GrammarVersion)
		if err != nil {
			return Admission{}, err
		}
		if seen[key] {
			return Admission{}, contractf(
				"two commitments in one admission share the key %s", key)
		}
		seen[key] = true

		expiry := cloneTiming(c.Expiry)
		material, err := submitIntent{
			Kind:            KindHandoff,
			GrammarVersion:  b.GrammarVersion,
			Provider:        c.Provider,
			Target:          c.Payload.Target,
			Operation:       OperationSend,
			Editable:        false,
			Content:         content,
			Timing:          c.Timing,
			Expiry:          expiry,
			CompletionMode:  c.CompletionMode,
			AmbiguityPolicy: c.AmbiguityPolicy,
			Payload:         c.Payload,
		}.encode()
		if err != nil {
			return Admission{}, err
		}

		admitted = append(admitted, AdmittedCommitment{
			IdempotencyKey:       key,
			Provider:             c.Provider,
			Target:               c.Payload.Target,
			Editable:             false,
			Operation:            OperationSend,
			CompletionMode:       c.CompletionMode,
			AmbiguityPolicy:      c.AmbiguityPolicy,
			Timing:               c.Timing,
			Expiry:               expiry,
			Payload:              c.Payload,
			PayloadSchemaVersion: c.Payload.SchemaVersion(),
		})
		encoded = append(encoded, material)
	}

	outcome := OutcomeAdmitted
	if len(admitted) == 0 {
		outcome = OutcomeNoTargets
	}

	// The content reference the BATCH fingerprint takes is the raw digest, not
	// the tagged form the commitments carry: that field is exactly 32 bytes by
	// protocol, and tagging it would be a new version of the batch fingerprint
	// - which is to say, new bytes for every escalation ever admitted.
	fingerprint, err := batchFingerprint(b.FingerprintVersion, outcome, digest, encoded)
	if err != nil {
		return Admission{}, err
	}

	sort.Slice(admitted, func(i, j int) bool {
		return admitted[i].IdempotencyKey < admitted[j].IdempotencyKey
	})

	return Admission{
		BatchKey:           batchKey,
		Kind:               KindHandoff,
		GrammarVersion:     b.GrammarVersion,
		Outcome:            outcome,
		Fingerprint:        fingerprint,
		FingerprintVersion: b.FingerprintVersion,
		Commitments:        admitted,
	}, nil
}
