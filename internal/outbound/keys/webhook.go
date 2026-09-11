package keys

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The identity of an outgoing webhook delivery.
//
// One event of the alert outbox, delivered to one subscriber. The claim is the
// event; the commitments are the subscribers that were enabled and in scope at
// the moment the event was fanned out; and what each commitment carries is a
// COPY of the event body, so nothing about the delivery is read from anywhere
// else once it has been admitted.
//
// A replay is the same event to the same subscriber under an operator's
// request id. It is a new commitment rather than the old one brought back: a
// delivered commitment is done, and the family's one door to a second external
// effect is this kind.

// WebhookEventType is the closed set of alert events a subscriber can receive.
// Closed here as well as where the events are written: the literal travels in
// the payload, in the digest and in the X-Tokay-Event header, and one spelled
// from outside the set would give a delivery an identity nothing else shares.
type WebhookEventType string

const (
	WebhookEventFiring       WebhookEventType = "alert_group.firing"
	WebhookEventAcknowledged WebhookEventType = "alert_group.acknowledged"
	WebhookEventResolved     WebhookEventType = "alert_group.resolved"
)

func (e WebhookEventType) validate() error {
	switch e {
	case WebhookEventFiring, WebhookEventAcknowledged, WebhookEventResolved:
		return nil
	default:
		return contractf("unknown webhook event type %q", e)
	}
}

// webhookIntent is one delivery of one event to one subscriber.
//
// The provider is not here. The family has exactly one, and a value that every
// key of a kind would carry identically is not identity - it is the column
// that says which worker runs the row. The target kind is not here either, for
// the same reason it is absent from a handover's key: the recipient is always
// a subscriber, and the payload refuses anything else.
type webhookIntent struct {
	Kind            Kind
	EventID         string
	IntegrationID   string
	ClientRequestID string
}

func (i webhookIntent) key(version int) (string, error) {
	if err := checkWebhookKind(i.Kind); err != nil {
		return "", err
	}
	if err := checkVersion(i.Kind, version); err != nil {
		return "", err
	}
	if err := checkEventID(i.EventID); err != nil {
		return "", err
	}
	if i.IntegrationID == "" {
		return "", contractf("a webhook commitment with no subscriber")
	}
	if err := requestIDFor(i.Kind, i.ClientRequestID); err != nil {
		return "", err
	}

	var material bytes.Buffer
	if err := opening(&material, i.Kind, version); err != nil {
		return "", err
	}
	encStr(&material, i.EventID)
	encStr(&material, i.IntegrationID)
	if i.Kind == KindWebhookReplay {
		encStr(&material, i.ClientRequestID)
	}
	return digestKey(i.EventID, material.Bytes()), nil
}

// webhookBatchKey is the claim a fan-out or a replay takes.
//
// A fan-out claims the EVENT: one row per event, however many subscribers it
// found, and none at all is still a claim. A replay claims its own thing - the
// event, the subscriber and the request id - because the event's claim is
// already held by the fan-out and would answer every replay with a conflict.
func webhookBatchKey(kind Kind, version int, eventID, integrationID, clientRequestID string) (string, error) {
	if err := checkWebhookKind(kind); err != nil {
		return "", err
	}
	if err := checkVersion(kind, version); err != nil {
		return "", err
	}
	if err := checkEventID(eventID); err != nil {
		return "", err
	}
	if err := requestIDFor(kind, clientRequestID); err != nil {
		return "", err
	}

	var material bytes.Buffer
	if err := opening(&material, kind, version); err != nil {
		return "", err
	}
	// The claim names itself as a claim. A replay's claim and its one
	// commitment name exactly the same things - the event, the subscriber and
	// the request id - and without this literal they would be the same bytes
	// in two tables. The golden vectors found that; nothing else would have.
	encStr(&material, "admission")
	encStr(&material, eventID)
	if kind == KindWebhookReplay {
		if integrationID == "" {
			return "", contractf("a replay admission with no subscriber")
		}
		encStr(&material, integrationID)
		encStr(&material, clientRequestID)
	}
	return digestKey(eventID, material.Bytes()), nil
}

func checkWebhookKind(kind Kind) error {
	switch kind {
	case KindWebhookEvent, KindWebhookReplay:
		return nil
	default:
		return contractf("%q is not a webhook kind", kind)
	}
}

// eventRef is what a webhook commitment is about: the event, and nothing else.
// The body is not here - it is the whole of the payload, which the fingerprint
// takes in its own field.
type eventRef struct {
	EventID string
}

func (e eventRef) encode(buf *bytes.Buffer) error {
	if e.EventID == "" {
		return contractf("an event reference with no event")
	}
	encStr(buf, "event")
	encStr(buf, e.EventID)
	return nil
}

// WebhookPayloadV1 is what the webhook channel posts, and where.
//
// Body is a STRING holding the request body, not the body as a nested value.
// The payload is stored as JSONB, and a nested object would come back in the
// database's normal form - its key order, its spacing - so the bytes the
// signature is computed over would depend on the server. A string comes back
// as the string that went in. The bytes are taken from the event exactly once,
// when it is fanned out, and are frozen from then on.
//
// The target is here as well as in the columns, as it is for every other kind:
// a commitment names its recipient in the place that decides WHERE it goes
// and in the place that decides WHAT is sent, and a schema rule holds the two
// together.
type WebhookPayloadV1 struct {
	Target    Target           `json:"target"`
	EventID   string           `json:"event_id"`
	EventType WebhookEventType `json:"event_type"`
	Body      string           `json:"body"`
}

// SchemaVersion is the payload schema this shape belongs to.
func (p WebhookPayloadV1) SchemaVersion() int { return 1 }

func (p WebhookPayloadV1) validate() error {
	if err := p.Target.addressedTo(TargetSubscriber); err != nil {
		return err
	}
	if p.EventID == "" {
		return contractf("a webhook payload with no event")
	}
	if err := p.EventType.validate(); err != nil {
		return err
	}
	if p.Body == "" {
		return contractf("a webhook payload with no body")
	}
	// The body has to survive being stored as a JSON string and read back as
	// the same bytes. Invalid UTF-8 does not: the encoder replaces it, and the
	// signature would then be computed over bytes the subscriber never sees.
	if !utf8.ValidString(p.Body) {
		return contractf("a webhook payload whose body is not valid UTF-8")
	}
	return nil
}

// DecodeWebhookPayloadV1 reads a STORED webhook payload, and is the only way to
// do it. Strict on both halves, for the reasons the escalation decoder gives.
func DecodeWebhookPayloadV1(schemaVersion int, raw []byte) (WebhookPayloadV1, error) {
	var payload WebhookPayloadV1
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

func (p WebhookPayloadV1) encode(buf *bytes.Buffer) error {
	if err := p.validate(); err != nil {
		return err
	}
	encStr(buf, string(p.Target.Kind))
	encStr(buf, p.Target.Ref)
	encStr(buf, p.EventID)
	encStr(buf, string(p.EventType))
	encStr(buf, p.Body)
	return nil
}

// WebhookBatch is one event as the domain fans it out, or one replay as an
// operator asks for it - the provenance every identity is derived from.
//
// This is what crosses into the store, not an Admission. Admit is called on
// the far side of the door, so there is no moment at which a derived key and a
// derived payload exist separately and could be made to disagree.
type WebhookBatch struct {
	Kind      Kind
	EventID   string
	EventType WebhookEventType
	// Body is the request body as the event stored it, taken once.
	Body string
	// IntegrationIDs is the audience: the subscribers enabled and in scope at
	// fan-out, or the one subscriber a replay is for. Sorted by Admit; a repeat
	// is refused rather than deduplicated, because a producer that named a
	// subscriber twice is a producer that read its audience wrong.
	IntegrationIDs []string
	// ClientRequestID is the operator's idempotency key on a replay, and empty
	// on a fan-out.
	ClientRequestID string
	// Expiry is how long a delivery stays owed, measured from admission. It is
	// the family's number and belongs to the batch: one event stops being worth
	// delivering at one moment, not at a different moment per subscriber.
	Expiry time.Duration

	GrammarVersion     int
	FingerprintVersion int
}

// deliveryFor builds the payload one subscriber gets. Everything in it is read
// off the batch; nothing is supplied twice, so nothing can disagree.
func (b WebhookBatch) deliveryFor(integrationID string) WebhookPayloadV1 {
	return WebhookPayloadV1{
		Target:    Target{Kind: TargetSubscriber, Ref: integrationID},
		EventID:   b.EventID,
		EventType: b.EventType,
		Body:      b.Body,
	}
}

// Admit derives every identity in one pass, exactly as an escalation and a
// handover do.
//
// What is fixed for the whole family - the provider, the operation, the form,
// the policies, the timing - is fixed HERE rather than taken from the caller:
// a webhook delivery is one-shot, immediately due, completed on acceptance and
// retried when in doubt, and a batch free to say otherwise would be a batch
// that could put a delivery into the family under numbers the family does not
// have.
func (b WebhookBatch) Admit() (Admission, error) {
	if err := checkWebhookKind(b.Kind); err != nil {
		return Admission{}, err
	}
	if err := checkVersion(b.Kind, b.GrammarVersion); err != nil {
		return Admission{}, err
	}
	if err := checkBatchFingerprintVersion(b.FingerprintVersion); err != nil {
		return Admission{}, err
	}
	if err := checkEventID(b.EventID); err != nil {
		return Admission{}, err
	}
	if err := b.EventType.validate(); err != nil {
		return Admission{}, err
	}
	if b.Body == "" {
		return Admission{}, contractf("a webhook admission with no body")
	}
	if b.Expiry <= 0 {
		// A delivery to somebody else's system has to stop being owed at some
		// point, and the point is the family's to set, not the caller's to omit.
		return Admission{}, contractf("a webhook admission with no deadline")
	}
	if b.Kind == KindWebhookReplay && len(b.IntegrationIDs) != 1 {
		return Admission{}, contractf(
			"a replay is one event to one subscriber, this one names %d", len(b.IntegrationIDs))
	}

	audience := append([]string(nil), b.IntegrationIDs...)
	sort.Strings(audience)
	for i, id := range audience {
		if id == "" {
			return Admission{}, contractf("a webhook admission naming an empty subscriber")
		}
		if i > 0 && audience[i-1] == id {
			return Admission{}, contractf("subscriber %s named twice in one admission", id)
		}
	}

	replayFor := ""
	if b.Kind == KindWebhookReplay {
		replayFor = audience[0]
	}
	batchKey, err := webhookBatchKey(b.Kind, b.GrammarVersion, b.EventID, replayFor, b.ClientRequestID)
	if err != nil {
		return Admission{}, err
	}

	content := eventRef{EventID: b.EventID}
	expiry := TimingSpec{Kind: TimingRelativeToAdmission, Offset: b.Expiry}
	if err := expiry.validate(); err != nil {
		return Admission{}, err
	}
	// Due at admission. The offset is zero and the kind is what carries the
	// meaning: a fan-out has nothing to wait for.
	timing := TimingSpec{Kind: TimingRelativeToAdmission}

	admitted := make([]AdmittedCommitment, 0, len(audience))
	encoded := make([][]byte, 0, len(audience))

	for _, integrationID := range audience {
		payload := b.deliveryFor(integrationID)
		if err := payload.validate(); err != nil {
			return Admission{}, err
		}

		key, err := webhookIntent{
			Kind:            b.Kind,
			EventID:         b.EventID,
			IntegrationID:   integrationID,
			ClientRequestID: b.ClientRequestID,
		}.key(b.GrammarVersion)
		if err != nil {
			return Admission{}, err
		}

		material, err := submitIntent{
			Kind:            b.Kind,
			GrammarVersion:  b.GrammarVersion,
			Provider:        ProviderWebhook,
			Target:          payload.Target,
			Operation:       OperationDeliver,
			Editable:        false,
			Content:         content,
			Timing:          timing,
			Expiry:          &expiry,
			CompletionMode:  CompletionOnAcceptance,
			AmbiguityPolicy: PolicyRetry,
			Payload:         payload,
		}.encode()
		if err != nil {
			return Admission{}, err
		}

		admitted = append(admitted, AdmittedCommitment{
			IdempotencyKey:       key,
			Provider:             ProviderWebhook,
			Target:               payload.Target,
			Editable:             false,
			Operation:            OperationDeliver,
			CompletionMode:       CompletionOnAcceptance,
			AmbiguityPolicy:      PolicyRetry,
			Timing:               timing,
			Expiry:               cloneTiming(&expiry),
			Payload:              payload,
			PayloadSchemaVersion: payload.SchemaVersion(),
		})
		encoded = append(encoded, material)
	}

	outcome := OutcomeAdmitted
	if len(admitted) == 0 {
		outcome = OutcomeNoTargets
	}

	// No content reference for the batch: what the commitments are about is the
	// event, and the event IS the claim. The protocol writes absence as its own
	// marker, so this is a different fingerprint from any admission that has
	// one, not a shorter spelling of the same thing.
	fingerprint, err := batchFingerprint(b.FingerprintVersion, outcome, nil, encoded)
	if err != nil {
		return Admission{}, err
	}

	sort.Slice(admitted, func(i, j int) bool {
		return admitted[i].IdempotencyKey < admitted[j].IdempotencyKey
	})

	return Admission{
		BatchKey:           batchKey,
		Kind:               b.Kind,
		GrammarVersion:     b.GrammarVersion,
		Outcome:            outcome,
		Fingerprint:        fingerprint,
		FingerprintVersion: b.FingerprintVersion,
		Commitments:        admitted,
		EventID:            b.EventID,
	}, nil
}

// checkEventID is the grammar of an event id as the key needs it: non-empty,
// and without the separator the key puts after it. A webhook key is
// <event_id>:<hex>, and an id with a colon in it would make the prefix
// unreadable to anything holding only the key - which is exactly how the
// upgrade that gives old claims their event_id reads them. The producer writes
// UUIDs; the rule names what is already true and refuses what would break it.
func checkEventID(eventID string) error {
	if eventID == "" {
		return contractf("a webhook admission with no event")
	}
	if strings.Contains(eventID, ":") {
		return contractf("event id %q contains ':', which the key grammar reserves", eventID)
	}
	return nil
}
