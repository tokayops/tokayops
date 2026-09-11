package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// The fixtures are dull and fixed on purpose: a golden vector is only worth
// having if the input never moves.
const (
	whEvent      = "evt-0001"
	whSubscriber = "int-a"
	whRequest    = "req-7"
	whBody       = `{"event":"alert_group.firing","alert_group":{"id":"ag-1"}}`
)

func sampleWebhookBatch(kind Kind, subscribers ...string) WebhookBatch {
	b := WebhookBatch{
		Kind:               kind,
		EventID:            whEvent,
		EventType:          WebhookEventFiring,
		Body:               whBody,
		IntegrationIDs:     subscribers,
		Expiry:             24 * time.Hour,
		GrammarVersion:     GrammarV1,
		FingerprintVersion: BatchFingerprintV1,
	}
	if kind == KindWebhookReplay {
		b.ClientRequestID = whRequest
	}
	return b
}

func sampleWebhookPayload(subscriber string) WebhookPayloadV1 {
	return WebhookPayloadV1{
		Target:    Target{Kind: TargetSubscriber, Ref: subscriber},
		EventID:   whEvent,
		EventType: WebhookEventFiring,
		Body:      whBody,
	}
}

// TestTheWebhookKeysAreTheseBytes pins all four keys of the family: the
// commitment and the claim of a fan-out, the commitment and the claim of a
// replay. A change here is a change of identity for every row ever written.
func TestTheWebhookKeysAreTheseBytes(t *testing.T) {
	cases := []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{
			name: "fan-out commitment",
			got: func() (string, error) {
				return webhookIntent{Kind: KindWebhookEvent, EventID: whEvent,
					IntegrationID: whSubscriber}.key(GrammarV1)
			},
			want: "evt-0001:aebef08ea218f3083065a271db682f1daffe037d5561ee2ca03a93c500019a4f",
		},
		{
			name: "replay commitment",
			got: func() (string, error) {
				return webhookIntent{Kind: KindWebhookReplay, EventID: whEvent,
					IntegrationID: whSubscriber, ClientRequestID: whRequest}.key(GrammarV1)
			},
			want: "evt-0001:9fc41f1060e639b181c22d293b6c9b151356d784e6c15438f2f8f750ea33ed23",
		},
		{
			name: "fan-out claim",
			got: func() (string, error) {
				return webhookBatchKey(KindWebhookEvent, GrammarV1, whEvent, "", "")
			},
			want: "evt-0001:e59f514c32087f006b2e134eeff11cd0484b5fb0377eb1c081478b8acfedd972",
		},
		{
			name: "replay claim",
			got: func() (string, error) {
				return webhookBatchKey(KindWebhookReplay, GrammarV1, whEvent,
					whSubscriber, whRequest)
			},
			want: "evt-0001:7ef20f4b009ad25d3d44e4187130c218e1fd14c0c424158d8b688fa6d04ff42f",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.got()
			if err != nil {
				t.Fatalf("key: %v", err)
			}
			if got != tc.want {
				t.Fatalf("the key is\n  %s\nand the grammar says\n  %s", got, tc.want)
			}
		})
	}
}

// TestWebhookBatchAndIntentKeysDoNotCollide is the test that found a defect in
// the gate: a replay's claim and its one commitment name exactly the same
// things, and without the claim naming itself as one they were the same bytes
// in two tables.
func TestWebhookBatchAndIntentKeysDoNotCollide(t *testing.T) {
	for _, kind := range []Kind{KindWebhookEvent, KindWebhookReplay} {
		t.Run(string(kind), func(t *testing.T) {
			request := ""
			if kind == KindWebhookReplay {
				request = whRequest
			}
			intent, err := webhookIntent{Kind: kind, EventID: whEvent,
				IntegrationID: whSubscriber, ClientRequestID: request}.key(GrammarV1)
			if err != nil {
				t.Fatalf("intent key: %v", err)
			}
			batch, err := webhookBatchKey(kind, GrammarV1, whEvent, whSubscriber, request)
			if err != nil {
				t.Fatalf("batch key: %v", err)
			}
			if intent == batch {
				t.Fatal("the claim and its commitment have one key")
			}
		})
	}
}

// TestWebhookKeysSeparateScopes: two events, two subscribers, two requests and
// the two kinds all give different keys, inside the kind and between kinds.
func TestWebhookKeysSeparateScopes(t *testing.T) {
	key := func(kind Kind, event, subscriber, request string) string {
		t.Helper()
		got, err := webhookIntent{Kind: kind, EventID: event, IntegrationID: subscriber,
			ClientRequestID: request}.key(GrammarV1)
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		return got
	}
	base := key(KindWebhookEvent, whEvent, whSubscriber, "")
	if key(KindWebhookEvent, "evt-0002", whSubscriber, "") == base {
		t.Fatal("two events got one commitment")
	}
	if key(KindWebhookEvent, whEvent, "int-b", "") == base {
		t.Fatal("two subscribers got one commitment")
	}
	replay := key(KindWebhookReplay, whEvent, whSubscriber, whRequest)
	if replay == base {
		t.Fatal("a replay shares its key with the delivery it repeats")
	}
	if key(KindWebhookReplay, whEvent, whSubscriber, "req-8") == replay {
		t.Fatal("two operator requests got one commitment")
	}
}

// TestTheWebhookPayloadDigestIsTheseBytes: the standalone digest of the shape
// the channel posts from, computed from outbound_payload_digest/v1 over the
// stored JSON - body as a STRING.
func TestTheWebhookPayloadDigestIsTheseBytes(t *testing.T) {
	raw := []byte(`{"target":{"kind":"subscriber","ref":"int-a"},"event_id":"evt-0001",` +
		`"event_type":"alert_group.firing",` +
		`"body":"{\"event\":\"alert_group.firing\",\"alert_group\":{\"id\":\"ag-1\"}}"}`)
	for _, kind := range []Kind{KindWebhookEvent, KindWebhookReplay} {
		digest, err := PayloadDigest(kind, 1, raw)
		if err != nil {
			t.Fatalf("%s digest: %v", kind, err)
		}
		// The kind is part of the material, so the two kinds digest differently
		// - and only one of them is pinned; the other is pinned by being
		// different from it.
		if kind == KindWebhookEvent {
			const want = "d5045e39083cc6cbbf9a73d8c7bd37422619281bf65367fa9ddba8f00498e988"
			if got := hex.EncodeToString(digest); got != want {
				t.Fatalf("the payload digest is %s, and the protocol says %s", got, want)
			}
		} else if hex.EncodeToString(digest) == "d5045e39083cc6cbbf9a73d8c7bd37422619281bf65367fa9ddba8f00498e988" {
			t.Fatal("a replay's payload digests like a fan-out's: the kind is not in the material")
		}
	}
}

// TestTheWebhookBatchFingerprintIsGolden pins both outcomes, and the order the
// audience came back in is not part of either.
func TestTheWebhookBatchFingerprintIsGolden(t *testing.T) {
	admitted, err := sampleWebhookBatch(KindWebhookEvent, "int-b", "int-a").Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	const wantAdmitted = "d733e2dbf1321424c4c7872614806713d89ebcd383db6dffffcc24543c00250f"
	if got := hex.EncodeToString(admitted.Fingerprint); got != wantAdmitted {
		t.Errorf("admitted proposal\n got: %s\nwant: %s", got, wantAdmitted)
	}

	reordered, err := sampleWebhookBatch(KindWebhookEvent, "int-a", "int-b").Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if hex.EncodeToString(reordered.Fingerprint) != wantAdmitted {
		t.Error("the order subscribers were read in changed the fingerprint")
	}

	empty, err := sampleWebhookBatch(KindWebhookEvent).Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if empty.Outcome != OutcomeNoTargets {
		t.Fatalf("an event nobody subscribed to was admitted as %q", empty.Outcome)
	}
	if got, want := hex.EncodeToString(empty.Fingerprint),
		"32502c0c17d454e06fec68c8c9c67b5ffb83d6413cf6c9b3304ef31732d8762a"; got != want {
		t.Errorf("empty proposal\n got: %s\nwant: %s", got, want)
	}
}

// TestTheWebhookSubmitIntentIsGolden pins the whole commitment material: all
// fourteen fields, with the event reference in the eighth, a zero offset in the
// ninth and a relative deadline in the tenth.
func TestTheWebhookSubmitIntentIsGolden(t *testing.T) {
	expiry := TimingSpec{Kind: TimingRelativeToAdmission, Offset: 24 * time.Hour}
	material, err := submitIntent{
		Kind:            KindWebhookEvent,
		GrammarVersion:  GrammarV1,
		Provider:        ProviderWebhook,
		Target:          Target{Kind: TargetSubscriber, Ref: whSubscriber},
		Operation:       OperationDeliver,
		Editable:        false,
		Content:         eventRef{EventID: whEvent},
		Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
		Expiry:          &expiry,
		CompletionMode:  CompletionOnAcceptance,
		AmbiguityPolicy: PolicyRetry,
		Payload:         sampleWebhookPayload(whSubscriber),
	}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sum := sha256.Sum256(material)
	if got, want := hex.EncodeToString(sum[:]),
		"e93deccb21b1509536c54e3892b21a818e4203b593b8c6bcab59d4b8a6d87b31"; got != want {
		t.Errorf("webhook commitment\n got: %s\nwant: %s", got, want)
	}
}

// TestAWebhookAdmissionDerivesEverythingFromTheEvent: what the family fixes is
// fixed by Admit and not taken from anybody - the provider, the operation, the
// form, both policies, the timing and the deadline - and every commitment is
// addressed to a subscriber named in the audience.
func TestAWebhookAdmissionDerivesEverythingFromTheEvent(t *testing.T) {
	adm, err := sampleWebhookBatch(KindWebhookEvent, "int-b", "int-a").Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if adm.Kind != KindWebhookEvent || adm.Outcome != OutcomeAdmitted {
		t.Fatalf("kind %q outcome %q", adm.Kind, adm.Outcome)
	}
	if adm.AlertGroupID != "" || adm.SnapshotSchemaVersion != 0 || adm.Revision != 0 {
		t.Fatal("a webhook admission carries an alert group's state")
	}
	want, _ := webhookBatchKey(KindWebhookEvent, GrammarV1, whEvent, "", "")
	if adm.BatchKey != want {
		t.Fatalf("batch key %s, want %s", adm.BatchKey, want)
	}
	if len(adm.Commitments) != 2 {
		t.Fatalf("%d commitments for two subscribers", len(adm.Commitments))
	}
	if adm.Commitments[0].IdempotencyKey > adm.Commitments[1].IdempotencyKey {
		t.Fatal("commitments are not in key order")
	}

	seen := map[string]bool{}
	for _, c := range adm.Commitments {
		if c.Provider != ProviderWebhook {
			t.Errorf("provider %q", c.Provider)
		}
		if c.Operation != OperationDeliver {
			t.Errorf("operation %q", c.Operation)
		}
		if c.Editable {
			t.Error("a webhook delivery that claims to be editable")
		}
		if c.CompletionMode != CompletionOnAcceptance || c.AmbiguityPolicy != PolicyRetry {
			t.Errorf("policies %q / %q", c.CompletionMode, c.AmbiguityPolicy)
		}
		if c.Timing != (TimingSpec{Kind: TimingRelativeToAdmission}) {
			t.Errorf("timing %+v: a fan-out is due at admission", c.Timing)
		}
		if c.Expiry == nil || *c.Expiry != (TimingSpec{Kind: TimingRelativeToAdmission, Offset: 24 * time.Hour}) {
			t.Errorf("expiry %+v", c.Expiry)
		}
		if c.Target.Kind != TargetSubscriber {
			t.Errorf("target kind %q", c.Target.Kind)
		}
		payload, ok := c.Payload.(WebhookPayloadV1)
		if !ok {
			t.Fatalf("payload is a %T", c.Payload)
		}
		if payload.Target != c.Target {
			t.Errorf("the payload is written for %v and the commitment addressed to %v",
				payload.Target, c.Target)
		}
		if payload.EventID != whEvent || payload.EventType != WebhookEventFiring || payload.Body != whBody {
			t.Errorf("payload %+v", payload)
		}
		if c.PayloadSchemaVersion != 1 {
			t.Errorf("schema %d", c.PayloadSchemaVersion)
		}
		seen[c.Target.Ref] = true
	}
	if !seen["int-a"] || !seen["int-b"] {
		t.Fatalf("audience %v", seen)
	}

	// A replay: one subscriber, its own claim, the request id in the key.
	replay, err := sampleWebhookBatch(KindWebhookReplay, whSubscriber).Admit()
	if err != nil {
		t.Fatalf("admit replay: %v", err)
	}
	wantKey, _ := webhookBatchKey(KindWebhookReplay, GrammarV1, whEvent, whSubscriber, whRequest)
	if replay.BatchKey != wantKey || len(replay.Commitments) != 1 {
		t.Fatalf("replay batch %s with %d commitments", replay.BatchKey, len(replay.Commitments))
	}
	wantIntent, _ := webhookIntent{Kind: KindWebhookReplay, EventID: whEvent,
		IntegrationID: whSubscriber, ClientRequestID: whRequest}.key(GrammarV1)
	if replay.Commitments[0].IdempotencyKey != wantIntent {
		t.Fatal("the replay commitment is not keyed by the operator's request")
	}
	if replay.Commitments[0].IdempotencyKey == adm.Commitments[0].IdempotencyKey ||
		replay.Commitments[0].IdempotencyKey == adm.Commitments[1].IdempotencyKey {
		t.Fatal("a replay revived a delivered commitment instead of making a new one")
	}
}

// TestAWebhookAdmissionRefusesWhatItCannotSay: the input is the only thing a
// caller supplies, so the input is where a bad admission is refused - on both
// doors, before anything is derived.
func TestAWebhookAdmissionRefusesWhatItCannotSay(t *testing.T) {
	cases := map[string]func(*WebhookBatch){
		"not a webhook kind":            func(b *WebhookBatch) { b.Kind = KindHandoff },
		"an unknown kind":               func(b *WebhookBatch) { b.Kind = Kind("webhook_v2") },
		"no event":                      func(b *WebhookBatch) { b.EventID = "" },
		"an event type outside the set": func(b *WebhookBatch) { b.EventType = "alert_group.snoozed" },
		"no body":                       func(b *WebhookBatch) { b.Body = "" },
		"a body that is not UTF-8":      func(b *WebhookBatch) { b.Body = "\xff\xfe" },
		"no deadline":                   func(b *WebhookBatch) { b.Expiry = 0 },
		"an empty subscriber":           func(b *WebhookBatch) { b.IntegrationIDs = []string{"int-a", ""} },
		"a subscriber named twice":      func(b *WebhookBatch) { b.IntegrationIDs = []string{"int-a", "int-a"} },
		"a fan-out with a request id":   func(b *WebhookBatch) { b.ClientRequestID = whRequest },
		"a version this build cannot encode": func(b *WebhookBatch) {
			b.GrammarVersion = GrammarV1 + 1
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := sampleWebhookBatch(KindWebhookEvent, "int-a", "int-b")
			mutate(&b)
			_, err := b.Admit()
			if err == nil {
				t.Fatal("the grammar accepted a statement it cannot make")
			}
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
		})
	}

	replays := map[string]func(*WebhookBatch){
		"a replay without a request id": func(b *WebhookBatch) { b.ClientRequestID = "" },
		"a request id past the limit": func(b *WebhookBatch) {
			b.ClientRequestID = strings.Repeat("x", MaxClientRequestID+1)
		},
		"a replay to two subscribers": func(b *WebhookBatch) { b.IntegrationIDs = []string{"int-a", "int-b"} },
		"a replay to nobody":          func(b *WebhookBatch) { b.IntegrationIDs = nil },
	}
	for name, mutate := range replays {
		t.Run(name, func(t *testing.T) {
			b := sampleWebhookBatch(KindWebhookReplay, "int-a")
			mutate(&b)
			_, err := b.Admit()
			if err == nil {
				t.Fatal("the grammar accepted a statement it cannot make")
			}
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
		})
	}
}

// TestAStoredWebhookPayloadIsReadStrictly: unknown fields, junk on the end, a
// schema this build does not render, a recipient that is not a subscriber and
// an event type outside the set are all refused - what is on the row now is
// what decides what goes out.
func TestAStoredWebhookPayloadIsReadStrictly(t *testing.T) {
	const good = `{"target":{"kind":"subscriber","ref":"int-a"},"event_id":"evt-0001",` +
		`"event_type":"alert_group.firing","body":"{}"}`
	if _, err := DecodeWebhookPayloadV1(1, []byte(good)); err != nil {
		t.Fatalf("a whole payload was refused: %v", err)
	}

	cases := map[string]struct {
		schema int
		raw    string
	}{
		"an unknown field":                    {1, strings.TrimSuffix(good, "}") + `,"louder":true}`},
		"a stray closing brace":               {1, good + "}"},
		"a second payload":                    {1, good + good},
		"a schema this build does not render": {2, good},
		"nothing at all":                      {1, ""},
		"a person as the target":              {1, strings.Replace(good, `"kind":"subscriber"`, `"kind":"user"`, 1)},
		"a channel as the target":             {1, strings.Replace(good, `"kind":"subscriber"`, `"kind":"channel"`, 1)},
		"an event type outside the set":       {1, strings.Replace(good, "alert_group.firing", "alert_group.snoozed", 1)},
		"no body":                             {1, strings.Replace(good, `"body":"{}"`, `"body":""`, 1)},
		"no event":                            {1, strings.Replace(good, `"event_id":"evt-0001"`, `"event_id":""`, 1)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeWebhookPayloadV1(tc.schema, []byte(tc.raw)); err == nil {
				t.Fatal("a payload this build must not post from was read")
			}
		})
	}
}

// TestAStoredWebhookPayloadSurvivesARoundTrip: the JSON that is stored reads
// back as the same value and digests the same, with a body that has every
// character the encoder treats specially - quotes, backslashes, HTML, unicode,
// a newline. The body is the thing that gets signed, so "the same" here means
// byte for byte.
func TestAStoredWebhookPayloadSurvivesARoundTrip(t *testing.T) {
	original := sampleWebhookPayload("int-a")
	original.Body = "{\"text\":\"<b>&amp;</b> \\\"quoted\\\" é世\n\"}"

	stored, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	read, err := DecodeWebhookPayloadV1(1, stored)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if read != original {
		t.Fatalf("the payload changed in storage:\n  was %+v\n  now %+v", original, read)
	}
	if read.Body != original.Body {
		t.Fatalf("the body changed:\n  was %q\n  now %q", original.Body, read.Body)
	}

	first, err := PayloadDigest(KindWebhookEvent, 1, stored)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	again, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("store again: %v", err)
	}
	second, err := PayloadDigest(KindWebhookEvent, 1, again)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if hex.EncodeToString(first) != hex.EncodeToString(second) {
		t.Fatal("a round trip changed the digest")
	}
}

// TestTheWebhookPayloadIsStoredUnderTheseNames pins the JSON spelling: a
// schema rule compares payload->'target'->>'kind' and ->>'ref' against the
// columns, and a field that quietly changed name would break it silently.
func TestTheWebhookPayloadIsStoredUnderTheseNames(t *testing.T) {
	raw, err := json.Marshal(sampleWebhookPayload("int-a"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"target":{"kind":"subscriber","ref":"int-a"},"event_id":"evt-0001",` +
		`"event_type":"alert_group.firing",` +
		`"body":"{\"event\":\"alert_group.firing\",\"alert_group\":{\"id\":\"ag-1\"}}"}`
	if string(raw) != want {
		t.Fatalf("a stored payload now reads\n  %s\nand the constraint over it expects\n  %s",
			raw, want)
	}
}

// TestAnEscalationIsNotAddressedToASubscriber. The grammar learned a third kind
// of target, and an escalation must not be able to say it: nothing in Slack or
// Telegram can be handed a subscriber.
func TestAnEscalationIsNotAddressedToASubscriber(t *testing.T) {
	intent := fixtureIntent()
	intent.Target = Target{Kind: TargetSubscriber, Ref: "int-a"}
	if _, err := intent.key(KindEscalation, GrammarV1); !errors.Is(err, ErrContract) {
		t.Fatalf("an escalation key to a subscriber: %v", err)
	}

	raw := []byte(`{"slot":{"kind":"firehose","index":0},` +
		`"target":{"kind":"subscriber","ref":"int-a"},"interactive":true}`)
	if _, err := DecodeEscalationPayloadV1(1, raw); err == nil {
		t.Fatal("an escalation payload addressed to a subscriber was read")
	}

	c := EscalationCommitment{
		Slot: Slot{Kind: SlotFirehose}, Provider: "slack",
		Target:         Target{Kind: TargetSubscriber, Ref: "int-a"},
		Timing:         TimingSpec{Kind: TimingRelativeToAdmission},
		CompletionMode: CompletionOnAcceptance, AmbiguityPolicy: PolicyRetry,
	}
	if err := c.validate(); !errors.Is(err, ErrContract) {
		t.Fatalf("an escalation commitment to a subscriber: %v", err)
	}
}

// TestTheWebhookKindsAreKnownToEveryClosedSet: family, grammar version, payload
// schema and request-id rule all answer for both kinds.
func TestTheWebhookKindsAreKnownToEveryClosedSet(t *testing.T) {
	for _, kind := range []Kind{KindWebhookEvent, KindWebhookReplay} {
		family, err := FamilyOf(kind)
		if err != nil || family != FamilyWebhook {
			t.Fatalf("%s: family %q, %v", kind, family, err)
		}
		version, err := CurrentGrammarVersion(kind)
		if err != nil || version != GrammarV1 {
			t.Fatalf("%s: version %d, %v", kind, version, err)
		}
		if !KnowsPayloadSchema(kind, 1) {
			t.Fatalf("%s: this build does not know its own payload schema", kind)
		}
		if KnowsPayloadSchema(kind, 2) {
			t.Fatalf("%s: a schema nobody wrote is claimed as known", kind)
		}
	}
	if FamilyWebhook == FamilyNotification || FamilyWebhook == FamilyHandoff {
		t.Fatal("the webhook family is not its own partition")
	}
}

// TestABatchContentReferenceMayBeAbsentButNotShort. The protocol writes
// absence as its own marker, so an admission without a content reference is a
// different fingerprint from any admission with one - and a reference that is
// present is a whole digest or nothing.
func TestABatchContentReferenceMayBeAbsentButNotShort(t *testing.T) {
	commitment := []byte("one")
	absent, err := batchFingerprint(BatchFingerprintV1, OutcomeAdmitted, nil, [][]byte{commitment})
	if err != nil {
		t.Fatalf("absent: %v", err)
	}
	zero := make([]byte, sha256.Size)
	present, err := batchFingerprint(BatchFingerprintV1, OutcomeAdmitted, zero, [][]byte{commitment})
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if hex.EncodeToString(absent) == hex.EncodeToString(present) {
		t.Fatal("no content reference fingerprints like a zero digest")
	}
	if _, err := batchFingerprint(BatchFingerprintV1, OutcomeAdmitted, zero[:31], [][]byte{commitment}); !errors.Is(err, ErrContract) {
		t.Fatalf("a 31-byte content reference: %v", err)
	}
}
