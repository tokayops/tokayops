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
// domain separator the material opens with, and the golden vector was carried
// across rather than recomputed.
//
// Not for deduplication across the upgrade - there is none. The old claims are
// rows in `jobs` and the new ones are rows in `outbound_batches`, and no
// uniqueness spans two tables; what keeps an already-announced shift from being
// announced again is the detector warming up on the current composition. What
// the vector holds is the GRAMMAR: it is normative in the gates, it will have
// readers outside this package, and a digest that drifted while the code moved
// between packages would be a silent change of identity nobody could see.

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
	// All three instants, not just one. A missing moment decodes as year zero
	// and renders as a shift that started in the year 1: the channel would show
	// it, because nothing downstream re-checks what the payload already said.
	for _, moment := range []struct {
		name string
		at   time.Time
	}{
		{"grid slot start", p.GridSlotStart},
		{"assignment start", p.AssignmentStart},
		{"assignment end", p.AssignmentEnd},
	} {
		if moment.at.IsZero() {
			return contractf("a handover payload with no %s", moment.name)
		}
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
	// Normalised to UTC on the way in. A payload written with a +03:00 offset
	// is the same instant, and the canonical material encodes it as such - but
	// two builds comparing the STORED JSON, or a person reading it, would see
	// two different spellings of one announcement.
	payload.GridSlotStart = payload.GridSlotStart.UTC()
	payload.AssignmentStart = payload.AssignmentStart.UTC()
	payload.AssignmentEnd = payload.AssignmentEnd.UTC()

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

// HandoffRecipient is one person, through one channel, and nothing about the
// event itself.
//
// What the announcement SAYS is a property of the batch: one shift change is
// one message, told to several people. A recipient that carried its own copy
// of the event could describe a different one - a different kind, a different
// instant, a different person - and the claim would be held forever by the
// first while the message said the second.
type HandoffRecipient struct {
	Provider string
	// UserID is the recipient as this system names them, and it must be one of
	// the people the occurrence is about. The address the channel will be
	// handed is preparation's business and is not identity.
	UserID          string
	Timing          TimingSpec
	CompletionMode  CompletionMode
	AmbiguityPolicy AmbiguityPolicy
}

func (r HandoffRecipient) validate() error {
	if r.Provider == "" {
		return contractf("a handover commitment with no provider")
	}
	if r.UserID == "" {
		return contractf("a handover commitment with no recipient")
	}
	if err := r.Timing.validate(); err != nil {
		return err
	}
	if err := r.CompletionMode.validate(); err != nil {
		return err
	}
	return r.AmbiguityPolicy.validate()
}

// HandoffBatch is one shift change as a producer proposes it.
//
// The occurrence comes in whole rather than as a digest beside a schedule id:
// the batch key, the content reference and every commitment key are derived
// from it here, so they cannot describe different shift changes. The payload
// each recipient gets is BUILT here for the same reason - the fields that say
// which event this is are read off the occurrence and are not the producer's to
// supply twice.
type HandoffBatch struct {
	Occurrence Occurrence

	// TeamName, Timezone, GridSlotStart and AssignmentEnd are the parts of the
	// message that are not identity: what the announcement shows, as observed
	// at admission. They are the same for every recipient.
	TeamName      string
	Timezone      string
	GridSlotStart time.Time
	AssignmentEnd time.Time

	// MaxAge is the second half of every commitment's deadline, and it belongs
	// to the batch because the deadline does. One shift change is one
	// announcement, and it stops being worth making at one moment - not at a
	// different moment per channel.
	MaxAge time.Duration

	GrammarVersion     int
	FingerprintVersion int
	Recipients         []HandoffRecipient
}

// deadline is when every announcement of this shift change stops being worth
// making: the earlier of the shift ending and an age from admission.
//
// Derived here rather than supplied per recipient, and that is the point. A
// recipient free to carry its own could be given none at all, or one later than
// the shift it announces - and two people would be told about one shift change
// under two different rules while both messages showed the same end time.
func (b HandoffBatch) deadline() TimingSpec {
	return TimingSpec{Kind: TimingBounded, At: b.AssignmentEnd.UTC(), MaxAge: b.MaxAge}
}

// announcementFor builds the payload one recipient gets.
//
// Kind, ScheduleID, AssignmentStart and the target come from the occurrence and
// the recipient; nothing here is supplied twice, so nothing here can disagree.
func (b HandoffBatch) announcementFor(userID string) HandoffPayloadV1 {
	return HandoffPayloadV1{
		Kind:            b.Occurrence.Kind,
		TeamName:        b.TeamName,
		ScheduleID:      b.Occurrence.ScheduleID,
		Timezone:        b.Timezone,
		GridSlotStart:   b.GridSlotStart.UTC(),
		AssignmentStart: b.Occurrence.AssignmentStart.UTC(),
		AssignmentEnd:   b.AssignmentEnd.UTC(),
		Target:          Target{Kind: TargetUser, Ref: userID},
	}
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

	// One deadline for the whole announcement, checked once. A batch whose
	// deadline does not validate is a batch that cannot be admitted at all,
	// rather than one that quietly admits some recipients.
	deadline := b.deadline()
	if err := deadline.validate(); err != nil {
		return Admission{}, err
	}

	// Who the occurrence is about, so a recipient outside it is refused rather
	// than announced to. An announcement to somebody the shift change does not
	// concern is a message about a shift they are not on.
	onDuty := make(map[string]bool, len(b.Occurrence.UserIDs))
	for _, id := range b.Occurrence.UserIDs {
		onDuty[id] = true
	}

	admitted := make([]AdmittedCommitment, 0, len(b.Recipients))
	encoded := make([][]byte, 0, len(b.Recipients))
	seen := make(map[string]bool, len(b.Recipients))

	for _, r := range b.Recipients {
		if err := r.validate(); err != nil {
			return Admission{}, err
		}
		if !onDuty[r.UserID] {
			return Admission{}, contractf(
				"an announcement to %s about a shift change that is not theirs", r.UserID)
		}

		payload := b.announcementFor(r.UserID)
		if err := payload.validate(); err != nil {
			return Admission{}, err
		}

		key, err := handoffIntent{
			ScheduleID:       b.Occurrence.ScheduleID,
			OccurrenceDigest: digest,
			Provider:         r.Provider,
			UserID:           r.UserID,
		}.key(b.GrammarVersion)
		if err != nil {
			return Admission{}, err
		}
		if seen[key] {
			return Admission{}, contractf(
				"two commitments in one admission share the key %s", key)
		}
		seen[key] = true

		material, err := submitIntent{
			Kind:            KindHandoff,
			GrammarVersion:  b.GrammarVersion,
			Provider:        r.Provider,
			Target:          payload.Target,
			Operation:       OperationSend,
			Editable:        false,
			Content:         content,
			Timing:          r.Timing,
			Expiry:          &deadline,
			CompletionMode:  r.CompletionMode,
			AmbiguityPolicy: r.AmbiguityPolicy,
			Payload:         payload,
		}.encode()
		if err != nil {
			return Admission{}, err
		}

		admitted = append(admitted, AdmittedCommitment{
			IdempotencyKey:       key,
			Provider:             r.Provider,
			Target:               payload.Target,
			Editable:             false,
			Operation:            OperationSend,
			CompletionMode:       r.CompletionMode,
			AmbiguityPolicy:      r.AmbiguityPolicy,
			Timing:               r.Timing,
			Expiry:               cloneTiming(&deadline),
			Payload:              payload,
			PayloadSchemaVersion: payload.SchemaVersion(),
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
