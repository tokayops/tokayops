package keys

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func fixtureDigest(seed byte) []byte {
	d := make([]byte, 32)
	for i := range d {
		d[i] = seed + byte(i)
	}
	return d
}

func fixtureCommitment() EscalationCommitment {
	return EscalationCommitment{
		Slot:            Slot{Kind: SlotPolicy, Index: 2},
		Provider:        fixtureProvider,
		Target:          Target{Kind: TargetChannel, Ref: fixtureChannel},
		Editable:        true,
		Interactive:     true,
		Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
		CompletionMode:  CompletionOnAcceptance,
		AmbiguityPolicy: PolicyRetry,
	}
}

func fixtureBatch(t *testing.T, commitments ...EscalationCommitment) EscalationBatch {
	t.Helper()
	return EscalationBatch{
		Kind:               KindEscalation,
		GrammarVersion:     GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
		Snapshot:           mustSnapshot(t, fixtureSnapshot()),
		Commitments:        commitments,
	}
}

func mustAdmit(t *testing.T, b EscalationBatch) Admission {
	t.Helper()
	admission, err := b.Admit()
	if err != nil {
		t.Fatalf("admit the batch: %v", err)
	}
	return admission
}

func fingerprintOf(t *testing.T, b EscalationBatch) string {
	t.Helper()
	return hex.EncodeToString(mustAdmit(t, b).Fingerprint)
}

// TestBatchFingerprintIsGolden pins the identity of a proposal's content.
//
// Two vectors, and the second is the one that matters most: an admission that
// found nobody to notify has no commitments to hash, so everything that tells
// two such proposals apart has to be in the material before the list.
//
// Both moved on 2026-08-25, deliberately: the batch's content reference is the
// render snapshot's digest, and the snapshot lost its timeline (tag 14).
func TestBatchFingerprintIsGolden(t *testing.T) {
	if got, want := fingerprintOf(t, fixtureBatch(t, fixtureCommitment())),
		"ec2b0714f5f78b7461ed36889c23d7e5ac7dee95c460abfce3153b5caa2cd78d"; got != want {
		t.Errorf("admitted proposal\n got: %s\nwant: %s", got, want)
	}

	if got, want := fingerprintOf(t, fixtureBatch(t)),
		"46d2d4d98333a6444760ec0d5a63cd8bb292c56ba4663ad372556bdd7cf17292"; got != want {
		t.Errorf("empty proposal\n got: %s\nwant: %s", got, want)
	}
}

// TestAdmissionOutcomeIsDerived checks the fact nobody gets to state twice: an
// admission with no commitments is one that found nobody to notify, and one
// with commitments is not. Two fields for one fact would eventually disagree.
func TestAdmissionOutcomeIsDerived(t *testing.T) {
	if got := mustAdmit(t, fixtureBatch(t, fixtureCommitment())).Outcome; got != OutcomeAdmitted {
		t.Errorf("a batch with work in it was admitted as %q", got)
	}
	if got := mustAdmit(t, fixtureBatch(t)).Outcome; got != OutcomeNoTargets {
		t.Errorf("an empty batch was admitted as %q", got)
	}
}

// TestEmptyProposalsAreToldApart is the case a fingerprint over the commitment
// list alone cannot see. Both proposals carry nothing to send; they are about
// different content, and the second producer has to be told it conflicts rather
// than be answered "already accepted".
func TestEmptyProposalsAreToldApart(t *testing.T) {
	first := fixtureBatch(t)

	otherContent := fixtureSnapshot()
	otherContent.Title = "a different alert"
	second := fixtureBatch(t)
	second.Snapshot = mustSnapshot(t, otherContent)

	if fingerprintOf(t, first) == fingerprintOf(t, second) {
		t.Fatal("two empty proposals about different content hashed the same")
	}
	if fingerprintOf(t, first) == fingerprintOf(t, fixtureBatch(t, fixtureCommitment())) {
		t.Fatal("an empty admission hashed the same as one with work in it")
	}
}

// TestOneCommitmentOneIdentity is the property the whole builder exists for.
//
// The key a commitment is deduplicated by, the payload it is executed from and
// the fingerprint it is compared by all come out of one statement of what the
// commitment is. Built separately, they could describe three different
// deliveries: idempotency would protect one message while the worker sent
// another.
func TestOneCommitmentOneIdentity(t *testing.T) {
	commitment := fixtureCommitment()
	admission := mustAdmit(t, fixtureBatch(t, commitment))

	if len(admission.Commitments) != 1 {
		t.Fatalf("expected one commitment, got %d", len(admission.Commitments))
	}
	got := admission.Commitments[0]

	expectedKey := mustIntentKey(t, escalationIntent{
		AlertGroupID: fixtureGroup,
		Slot:         commitment.Slot,
		Provider:     commitment.Provider,
		Target:       commitment.Target,
	}, KindEscalation)
	if got.IdempotencyKey != expectedKey {
		t.Fatalf("the key does not describe the commitment:\n got: %s\nwant: %s",
			got.IdempotencyKey, expectedKey)
	}
	if escalationPayload(t, got.Payload).Target != commitment.Target {
		t.Fatalf("the payload targets %v, the commitment targets %v",
			escalationPayload(t, got.Payload).Target, commitment.Target)
	}
	if escalationPayload(t, got.Payload).Slot != commitment.Slot {
		t.Fatalf("the payload is for slot %v, the commitment for %v",
			escalationPayload(t, got.Payload).Slot, commitment.Slot)
	}
	if got.Operation != OperationSend {
		t.Fatalf("an admission produced operation %q", got.Operation)
	}
	if got.PayloadSchemaVersion != got.Payload.SchemaVersion() {
		t.Fatal("the stored schema version does not match the payload")
	}
}

// TestAdmittedCommitmentDoesNotShareItsOptionals is the aliasing case. The
// override and the expiry are pointers, and a result that handed the caller's
// own pointers back would let the payload change after it was fingerprinted -
// the executable half moving while the identity half stands still.
func TestAdmittedCommitmentDoesNotShareItsOptionals(t *testing.T) {
	override := "page the on-call"
	expiry := TimingSpec{Kind: TimingAbsolute, At: time.Unix(1700000000, 0)}

	commitment := fixtureCommitment()
	commitment.MessageOverride = &override
	commitment.Expiry = &expiry

	admission := mustAdmit(t, fixtureBatch(t, commitment))
	got := admission.Commitments[0]

	override = "page somebody else"
	expiry.At = time.Unix(1800000000, 0)

	if *escalationPayload(t, got.Payload).MessageOverride != "page the on-call" {
		t.Fatalf("the payload followed the caller's edit: %q", *escalationPayload(t, got.Payload).MessageOverride)
	}
	if !got.Expiry.At.Equal(time.Unix(1700000000, 0).UTC()) &&
		!got.Expiry.At.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("the expiry followed the caller's edit: %v", got.Expiry.At)
	}
}

// TestAdmissionOrdersCommitmentsByKey pins the insert order. Two producers
// racing on one batch have to write the same rows in the same order, so a
// violation of the key grammar surfaces as a deterministic unique-violation
// rather than as a deadlock.
func TestAdmissionOrdersCommitmentsByKey(t *testing.T) {
	first := fixtureCommitment()
	second := fixtureCommitment()
	second.Target = Target{Kind: TargetChannel, Ref: "C0002"}

	forward := mustAdmit(t, fixtureBatch(t, first, second))
	backward := mustAdmit(t, fixtureBatch(t, second, first))

	if len(forward.Commitments) != 2 || len(backward.Commitments) != 2 {
		t.Fatal("a commitment went missing")
	}
	for i := range forward.Commitments {
		if forward.Commitments[i].IdempotencyKey != backward.Commitments[i].IdempotencyKey {
			t.Fatalf("position %d differs between input orders", i)
		}
	}
	if forward.Commitments[0].IdempotencyKey > forward.Commitments[1].IdempotencyKey {
		t.Fatal("commitments came back out of key order")
	}
	if hex.EncodeToString(forward.Fingerprint) != hex.EncodeToString(backward.Fingerprint) {
		t.Fatal("the same set of commitments hashed differently in a different order")
	}
}

// TestSubmitIntentFingerprintCoversEveryField is the mutation test. Every field
// of a proposal is there because it can make two proposals different; a field
// the fingerprint does not see is a difference two producers could disagree
// about silently.
func TestSubmitIntentFingerprintCoversEveryField(t *testing.T) {
	baseline := fingerprintOf(t, fixtureBatch(t, fixtureCommitment()))

	empty := ""
	override := "page the on-call"
	expiry := TimingSpec{Kind: TimingAbsolute, At: time.Unix(1700000000, 0)}

	commitmentMutations := []struct {
		name   string
		change func(*EscalationCommitment)
	}{
		{"provider", func(c *EscalationCommitment) { c.Provider = "telegram" }},
		{"target kind", func(c *EscalationCommitment) { c.Target.Kind = TargetUser }},
		{"recipient", func(c *EscalationCommitment) { c.Target.Ref = "C0002" }},
		{"slot", func(c *EscalationCommitment) { c.Slot = Slot{Kind: SlotFirehose} }},
		{"editable", func(c *EscalationCommitment) { c.Editable = false }},
		{"timing offset", func(c *EscalationCommitment) { c.Timing.Offset = 5 * time.Minute }},
		{"timing kind", func(c *EscalationCommitment) {
			c.Timing = TimingSpec{Kind: TimingAbsolute, At: time.Unix(1700000000, 0)}
		}},
		{"expiry appearing", func(c *EscalationCommitment) { c.Expiry = &expiry }},
		{"completion mode", func(c *EscalationCommitment) { c.CompletionMode = CompletionOnProviderReceipt }},
		{"ambiguity policy", func(c *EscalationCommitment) { c.AmbiguityPolicy = PolicyManualReview }},
		{"message override appearing", func(c *EscalationCommitment) { c.MessageOverride = &override }},
		{"message override empty rather than absent", func(c *EscalationCommitment) { c.MessageOverride = &empty }},
		{"interactive", func(c *EscalationCommitment) { c.Interactive = false }},
	}

	seen := map[string]string{baseline: "baseline"}
	for _, m := range commitmentMutations {
		t.Run(m.name, func(t *testing.T) {
			c := fixtureCommitment()
			m.change(&c)
			got := fingerprintOf(t, fixtureBatch(t, c))
			if got == baseline {
				t.Fatalf("changing the %s did not change the fingerprint", m.name)
			}
			if other, ok := seen[got]; ok {
				t.Fatalf("changing the %s collided with changing the %s", m.name, other)
			}
			seen[got] = m.name
		})
	}

	batchMutations := []struct {
		name   string
		change func(*testing.T, *EscalationBatch)
	}{
		{"alert group", func(t *testing.T, b *EscalationBatch) {
			in := fixtureSnapshot()
			in.AlertGroupID = "ag-0002"
			b.Snapshot = mustSnapshot(t, in)
		}},
		{"revision", func(t *testing.T, b *EscalationBatch) {
			in := fixtureSnapshot()
			in.Revision = 1
			b.Snapshot = mustSnapshot(t, in)
		}},
		{"snapshot content", func(t *testing.T, b *EscalationBatch) {
			in := fixtureSnapshot()
			in.Title = "something else"
			b.Snapshot = mustSnapshot(t, in)
		}},
		{"kind", func(t *testing.T, b *EscalationBatch) {
			b.Kind = KindEscalationReplay
			b.ClientRequestID = fixtureRequest
		}},
	}
	for _, m := range batchMutations {
		t.Run(m.name, func(t *testing.T) {
			b := fixtureBatch(t, fixtureCommitment())
			m.change(t, &b)
			got := fingerprintOf(t, b)
			if got == baseline {
				t.Fatalf("changing the %s did not change the fingerprint", m.name)
			}
			if other, ok := seen[got]; ok {
				t.Fatalf("changing the %s collided with changing the %s", m.name, other)
			}
			seen[got] = m.name
		})
	}
}

// TestRelativeTimingSurvivesTheClock is why a delay is stored as an offset
// rather than as the instant it lands on. A producer repeating an admission
// later must offer the same proposal, not a proposal that has aged.
func TestRelativeTimingSurvivesTheClock(t *testing.T) {
	build := func() EscalationBatch {
		c := fixtureCommitment()
		c.Timing = TimingSpec{Kind: TimingRelativeToAdmission, Offset: 5 * time.Minute}
		return fixtureBatch(t, c)
	}

	before := fingerprintOf(t, build())
	time.Sleep(2 * time.Millisecond)
	after := fingerprintOf(t, build())

	if before != after {
		t.Fatal("the same proposal hashed differently a moment later")
	}
}

// TestAdmissionRefusesIncoherentProposals pins the refusals. Each is a proposal
// that contradicts itself, and admitting it would give a state the domain does
// not have an identity of its own.
func TestAdmissionRefusesIncoherentProposals(t *testing.T) {
	cases := []struct {
		name  string
		build func() EscalationBatch
	}{
		{
			name: "no content to be about",
			build: func() EscalationBatch {
				b := fixtureBatch(t, fixtureCommitment())
				b.Snapshot = RenderSnapshot{}
				return b
			},
		},
		{
			name: "an empty admission still has to say what it was about",
			build: func() EscalationBatch {
				b := fixtureBatch(t)
				b.Snapshot = RenderSnapshot{}
				return b
			},
		},
		{
			name: "a fingerprint protocol this build cannot encode",
			build: func() EscalationBatch {
				b := fixtureBatch(t, fixtureCommitment())
				b.FingerprintVersion = CurrentBatchFingerprintVersion() + 1
				return b
			},
		},
		{
			name: "two commitments for one recipient",
			build: func() EscalationBatch {
				return fixtureBatch(t, fixtureCommitment(), fixtureCommitment())
			},
		},
		{
			name: "a timing spec nobody defined",
			build: func() EscalationBatch {
				c := fixtureCommitment()
				c.Timing = TimingSpec{Kind: TimingKind("after_the_previous_step")}
				return fixtureBatch(t, c)
			},
		},
		{
			name: "a relative delay that also names an instant",
			build: func() EscalationBatch {
				c := fixtureCommitment()
				c.Timing = TimingSpec{
					Kind: TimingRelativeToAdmission, Offset: time.Minute,
					At: time.Unix(1700000000, 0),
				}
				return fixtureBatch(t, c)
			},
		},
		{
			name: "an absolute time that also carries an offset",
			build: func() EscalationBatch {
				c := fixtureCommitment()
				c.Timing = TimingSpec{
					Kind: TimingAbsolute, At: time.Unix(1700000000, 0), Offset: time.Minute,
				}
				return fixtureBatch(t, c)
			},
		},
		{
			name: "a completion mode from outside the set",
			build: func() EscalationBatch {
				c := fixtureCommitment()
				c.CompletionMode = CompletionMode("on_read")
				return fixtureBatch(t, c)
			},
		},
		{
			name: "an ambiguity policy from outside the set",
			build: func() EscalationBatch {
				c := fixtureCommitment()
				c.AmbiguityPolicy = AmbiguityPolicy("ask_later")
				return fixtureBatch(t, c)
			},
		},
		{
			name: "a re-admission with no request id",
			build: func() EscalationBatch {
				b := fixtureBatch(t, fixtureCommitment())
				b.Kind = KindEscalationReplay
				return b
			},
		},
		{
			name: "a version this kind does not define",
			build: func() EscalationBatch {
				b := fixtureBatch(t, fixtureCommitment())
				b.GrammarVersion = GrammarV1 + 1
				return b
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.build().Admit()
			if err == nil {
				t.Fatal("an incoherent proposal was admitted")
			}
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
		})
	}
}

// TestCompletionFingerprintIsGolden pins what a finished attempt concluded. A
// repeat of the same finalisation has to match it exactly, which is what tells
// "I already recorded this" from "somebody recorded something else".
func TestCompletionFingerprintIsGolden(t *testing.T) {
	receipt := "C0001/1700000000.000100"
	revision := int64(0)

	sum, err := Completion{
		Outcome:         OutcomeAccepted,
		ReceiptRef:      &receipt,
		AppliedRevision: &revision,
	}.Fingerprint(CurrentCompletionFingerprintVersion())
	if err != nil {
		t.Fatalf("fingerprint the completion: %v", err)
	}

	if got, want := hex.EncodeToString(sum),
		"448741779271654886b9e76ab90b2e7c1fcc66d9eabc4f4dfefb9b7a66a9970b"; got != want {
		t.Fatalf("completion fingerprint\n got: %s\nwant: %s", got, want)
	}
}

// TestCompletionFingerprintCoversEveryField is the same mutation discipline for
// the attempt side, plus the distinction that matters most there: a field that
// was absent and a field that was empty are different conclusions.
func TestCompletionFingerprintCoversEveryField(t *testing.T) {
	receipt := "C0001/1700000000.000100"
	revision := int64(0)
	base := Completion{
		Outcome:         OutcomeAccepted,
		ReceiptRef:      &receipt,
		AppliedRevision: &revision,
	}

	baseSum, err := base.Fingerprint(CurrentCompletionFingerprintVersion())
	if err != nil {
		t.Fatalf("fingerprint the completion: %v", err)
	}
	baseline := hex.EncodeToString(baseSum)

	empty := ""
	otherReceipt := "C0001/1700000000.000200"
	otherRevision := int64(1)
	class := "rate_limited"
	status := "429"
	detail := DetailAcceptanceProven

	mutations := []struct {
		name   string
		change func(*Completion)
	}{
		{"outcome", func(c *Completion) { c.Outcome = OutcomeAmbiguous }},
		{"error class appearing", func(c *Completion) { c.ErrorClass = &class }},
		{"error class empty rather than absent", func(c *Completion) { c.ErrorClass = &empty }},
		{"provider status", func(c *Completion) { c.ProviderStatus = &status }},
		{"receipt", func(c *Completion) { c.ReceiptRef = &otherReceipt }},
		{"receipt disappearing", func(c *Completion) { c.ReceiptRef = nil }},
		{"applied revision", func(c *Completion) { c.AppliedRevision = &otherRevision }},
		{"applied revision disappearing", func(c *Completion) { c.AppliedRevision = nil }},
		{"provider result detail", func(c *Completion) { c.ProviderResultDetail = &detail }},
	}

	seen := map[string]string{baseline: "baseline"}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			c := base
			m.change(&c)
			sum, err := c.Fingerprint(CurrentCompletionFingerprintVersion())
			if err != nil {
				t.Fatalf("fingerprint the completion: %v", err)
			}
			got := hex.EncodeToString(sum)
			if got == baseline {
				t.Fatalf("changing the %s did not change the fingerprint", m.name)
			}
			if other, ok := seen[got]; ok {
				t.Fatalf("changing the %s collided with changing the %s", m.name, other)
			}
			seen[got] = m.name
		})
	}
}

// TestCompletionRefusesWhatItCannotIdentify covers the two values that are not
// free text: an outcome and a reconciliation result both travel inside the
// fingerprint, so one spelled from outside its set would give two different
// conclusions one identity.
func TestCompletionRefusesWhatItCannotIdentify(t *testing.T) {
	negative := int64(-1)
	unknown := ProviderResultDetail("probably_fine")

	cases := []struct {
		name       string
		completion Completion
	}{
		{"an attempt outcome nobody declared", Completion{Outcome: AttemptOutcome("half_sent")}},
		{"a negative applied revision", Completion{Outcome: OutcomeAccepted, AppliedRevision: &negative}},
		{"a result detail nobody declared", Completion{Outcome: OutcomeAmbiguous, ProviderResultDetail: &unknown}},
	}

	// And the protocol version itself: an attempt started under one encoder
	// cannot be finalised under another, so an unknown one is refused rather
	// than quietly treated as the current one.
	if _, err := (Completion{Outcome: OutcomeAccepted}).Fingerprint(
		CurrentCompletionFingerprintVersion() + 1); !errors.Is(err, ErrContract) {
		t.Fatalf("expected an unknown completion protocol to be refused, got: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.completion.Fingerprint(CurrentCompletionFingerprintVersion())
			if err == nil {
				t.Fatal("an incoherent completion was fingerprinted")
			}
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
		})
	}
}

// TestThePayloadIsStoredUnderTheseNames pins the JSON spelling of a stored
// payload.
//
// It is not a formatting preference. The row is compared against the columns
// beside it by a database constraint - the commitment names its recipient
// twice, and a mismatch delivers a private message into a channel - and that
// constraint reads these paths. A field renamed by adding or dropping a tag
// would make the comparison silently vacuous.
func TestThePayloadIsStoredUnderTheseNames(t *testing.T) {
	override := "call Nina"
	raw, err := json.Marshal(EscalationPayloadV1{
		Slot:            Slot{Kind: SlotPolicy, Index: 2},
		Target:          Target{Kind: TargetChannel, Ref: "C0001"},
		MessageOverride: &override,
		Interactive:     true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `{"slot":{"kind":"policy","index":2},` +
		`"target":{"kind":"channel","ref":"C0001"},` +
		`"message_override":"call Nina","interactive":true}`
	if string(raw) != want {
		t.Fatalf("a stored payload now reads\n  %s\nand the constraint over it expects\n  %s",
			raw, want)
	}
}

// TestAStoredPayloadHasToEndWhereItsValueDoes.
//
// Everything after the payload is refused, and the check is a second decode
// rather than json.Decoder.More(): outside an array or an object More() answers
// "is the next token something other than ] or }", so a stray closing brace
// after a good payload reads as "nothing follows". A row with junk on the end
// is a row somebody or something wrote by hand, and rendering the part that
// parsed would carry out half of what it says.
func TestAStoredPayloadHasToEndWhereItsValueDoes(t *testing.T) {
	const good = `{"slot":{"kind":"firehose","index":0},` +
		`"target":{"kind":"channel","ref":"C0001"},"interactive":true}`

	if _, err := DecodeEscalationPayloadV1(1, []byte(good)); err != nil {
		t.Fatalf("a whole payload was refused: %v", err)
	}

	for name, raw := range map[string]string{
		"a stray closing brace": good + "}",
		"a second payload":      good + good,
		"a bare value after it": good + " null",
		"junk after it":         good + " oops",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEscalationPayloadV1(1, []byte(raw)); err == nil {
				t.Fatalf("a payload with %s was accepted", name)
			}
		})
	}
}

// escalationPayload reads an admitted payload as the shape an escalation has.
// The interface is sealed, so this assertion is a check that the admission put
// the right shape in - not a cast that could go anywhere.
func escalationPayload(t *testing.T, p Payload) EscalationPayloadV1 {
	t.Helper()
	payload, ok := p.(EscalationPayloadV1)
	if !ok {
		t.Fatalf("an escalation was admitted carrying a %T", p)
	}
	return payload
}
