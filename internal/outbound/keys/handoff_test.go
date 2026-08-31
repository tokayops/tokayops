package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The identity of a handover announcement, and the one thing that has to hold
// across the upgrade: the digest a running installation already announced under
// is the same digest afterwards.

func sampleOccurrence() Occurrence {
	return Occurrence{
		Kind:            HandoffShiftChange,
		ScheduleID:      "sched-1",
		Source:          "rotation",
		GroupID:         "g-a",
		UserIDs:         []string{"u-bob", "u-alice"},
		AssignmentStart: time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		RevisionID:      "rev-7",
	}
}

func samplePayload(user string) HandoffPayloadV1 {
	return sampleBatch().announcementFor(user)
}

func sampleBatch(recipients ...HandoffRecipient) HandoffBatch {
	return HandoffBatch{
		Occurrence:         sampleOccurrence(),
		TeamName:           "Backend",
		Timezone:           "UTC",
		GridSlotStart:      time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		AssignmentEnd:      time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
		MaxAge:             time.Hour,
		GrammarVersion:     GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
		Recipients:         recipients,
	}
}

func sampleRecipient(provider, user string) HandoffRecipient {
	return HandoffRecipient{
		Provider: provider, UserID: user,
		Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
		CompletionMode:  CompletionOnAcceptance,
		AmbiguityPolicy: PolicyRetry,
	}
}

// TestTheOccurrenceKeyIsTheOneTheJobEngineWrote is the vector that was carried
// across, not recomputed.
//
// The value below is the one the job engine's own vector pinned when it wrote
// these claims, reproduced here by hand from the written grammar. That vector
// went with the package it lived in; this is the copy that outlives it.
//
// It is NOT what keeps a shift change from being announced twice across the
// upgrade: the old claims are rows in `jobs` and the new ones are rows in
// `outbound_batches`, and no uniqueness spans two tables - what prevents the
// duplicate is the detector warming up on the current composition. What this
// holds is the grammar itself, which is normative in the gates and will have
// readers outside this package. A digest that drifted while the code moved
// between packages would be a change of identity nobody could see.
//
// If this fails and the change was deliberate, the change is a new protocol
// string, not a new expected value here.
func TestTheOccurrenceKeyIsTheOneTheJobEngineWrote(t *testing.T) {
	const want = "sched-1:1264ee81aac654f094a55dd4ad085897d4c7d0304d168f078edb1e1089b9d5c5"

	got, err := sampleOccurrence().Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if got != want {
		t.Fatalf("the occurrence key is\n  %s\nand the engine wrote\n  %s\n"+
			"every shift change already announced would be announced again", got, want)
	}
}

// TestTheOccurrenceKeyIsAStringAndTheDigestIsNot. Three values were called by
// one name in the first draft of this design, and two of them are not
// interchangeable: the batch key is the schedule, a colon and the hex digest,
// which is what is already stored - not the raw 32 bytes.
func TestTheOccurrenceKeyIsAStringAndTheDigestIsNot(t *testing.T) {
	occurrence := sampleOccurrence()
	digest, err := occurrence.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if len(digest) != 32 {
		t.Fatalf("the digest is %d bytes", len(digest))
	}
	key, err := occurrence.Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if key != "sched-1:"+hex.EncodeToString(digest) {
		t.Fatalf("the key %q is not the schedule and the digest", key)
	}
}

// TestTheOrderPeopleCameBackInIsNotIdentity. A projection returning the same
// people in another order is the same shift change; announcing it twice would
// be one wrong message to everybody on it.
func TestTheOrderPeopleCameBackInIsNotIdentity(t *testing.T) {
	one := sampleOccurrence()
	other := sampleOccurrence()
	other.UserIDs = []string{"u-alice", "u-bob"}

	first, err := one.Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	second, err := other.Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if first != second {
		t.Fatalf("two orderings of one shift are two occurrences:\n  %s\n  %s", first, second)
	}
}

// TestEveryFieldOfAnOccurrenceIsIdentity. Each of them separates two events
// that would otherwise suppress one another, and the two easiest to doubt are
// Source and GroupID: the same people on duty by rotation and by override is
// a real handover, and announcing only the first would leave the second silent.
func TestEveryFieldOfAnOccurrenceIsIdentity(t *testing.T) {
	base, err := sampleOccurrence().Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	for _, tc := range []struct {
		name  string
		alter func(*Occurrence)
	}{
		{"another kind", func(o *Occurrence) { o.Kind = HandoffAddedToActiveShift }},
		{"another schedule", func(o *Occurrence) { o.ScheduleID = "sched-2" }},
		{"an override instead of the rotation", func(o *Occurrence) { o.Source = "override" }},
		{"another group", func(o *Occurrence) { o.GroupID = "g-b" }},
		{"another person", func(o *Occurrence) { o.UserIDs = []string{"u-bob", "u-carol"} }},
		{"the next turn of the rotation", func(o *Occurrence) {
			o.AssignmentStart = o.AssignmentStart.Add(time.Hour)
		}},
		{"an edit to the schedule", func(o *Occurrence) { o.RevisionID = "rev-8" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := sampleOccurrence()
			tc.alter(&changed)
			got, err := changed.Key()
			if err != nil {
				t.Fatalf("key: %v", err)
			}
			if got == base {
				t.Fatal("a different shift change produced the same key")
			}
		})
	}
}

// TestTheHandoffIntentKeyIsTheseBytes. Computed by hand from the written
// grammar: the opening triple is (handoff, handoff, v1), and the family is the
// FIRST atom - which is exactly why moving the family changed the bytes even
// though the fields after it did not.
func TestTheHandoffIntentKeyIsTheseBytes(t *testing.T) {
	digest, err := sampleOccurrence().Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	got, err := handoffIntent{
		ScheduleID: "sched-1", OccurrenceDigest: digest,
		Provider: "slack", UserID: "u-alice",
	}.key(GrammarV1)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	const want = "sched-1:062806b72a255881ce8546381be309fb4f8c957b8f56be8eb2e9321eadfd26c0"
	if got != want {
		t.Fatalf("the commitment key is\n  %s\nand the grammar says\n  %s", got, want)
	}
}

// TestOnePersonInTwoChannelsIsTwoCommitments, and the same person relinking a
// channel is not a third announcement about one shift: the key carries the
// INTERNAL user id, never the address.
func TestOnePersonInTwoChannelsIsTwoCommitments(t *testing.T) {
	digest, err := sampleOccurrence().Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	key := func(provider, user string) string {
		t.Helper()
		got, err := handoffIntent{
			ScheduleID: "sched-1", OccurrenceDigest: digest,
			Provider: provider, UserID: user,
		}.key(GrammarV1)
		if err != nil {
			t.Fatalf("key: %v", err)
		}
		return got
	}
	if key("slack", "u-alice") == key("telegram", "u-alice") {
		t.Fatal("one person in two channels got one commitment")
	}
	if key("slack", "u-alice") == key("slack", "u-bob") {
		t.Fatal("two people got one commitment")
	}
}

// TestTheHandoffPayloadDigestIsTheseBytes. The standalone digest of the shape a
// channel draws from, computed by hand from outbound_payload_digest/v1.
func TestTheHandoffPayloadDigestIsTheseBytes(t *testing.T) {
	raw := []byte(`{"kind":"handoff","team_name":"Backend","schedule_id":"sched-1",` +
		`"timezone":"UTC","grid_slot_start":"2026-05-04T11:00:00Z",` +
		`"assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z",` +
		`"target":{"kind":"user","ref":"u-alice"}}`)
	digest, err := PayloadDigest(KindHandoff, 1, raw)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	const want = "a4e8eaaa81f866637f96e2025be2d8cd229302e258b043683eb1388f0dbec0ff"
	if got := hex.EncodeToString(digest); got != want {
		t.Fatalf("the payload digest is %s, and the protocol says %s", got, want)
	}
}

// TestAHandoverIsAddressedToAPerson.
//
// The business key carries the occurrence, the provider and the USER ID, and
// not the target kind. So an announcement aimed at a channel whose id happened
// to match a user id would share its key with the one aimed at that person:
// two different promises deduplicated as one. The payload refuses it instead.
func TestAHandoverIsAddressedToAPerson(t *testing.T) {
	payload := samplePayload("u-alice")
	payload.Target = Target{Kind: TargetChannel, Ref: "u-alice"}
	if err := payload.validate(); err == nil {
		t.Fatal("a handover announcement was addressed to a channel")
	}

	raw := []byte(`{"kind":"handoff","team_name":"Backend","schedule_id":"sched-1",` +
		`"timezone":"UTC","grid_slot_start":"2026-05-04T11:00:00Z",` +
		`"assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z",` +
		`"target":{"kind":"channel","ref":"C0001"}}`)
	if _, err := DecodeHandoffPayloadV1(1, raw); err == nil {
		t.Fatal("a stored announcement addressed to a channel was read back")
	}
}

// TestAStoredAnnouncementIsReadStrictly. A field this build does not know is a
// refusal, not something to drop: it was written by a build that knew more, and
// rendering without it sends a message missing whatever it carried.
func TestAStoredAnnouncementIsReadStrictly(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "a field from a newer build", raw: `{"kind":"handoff","team_name":"Backend","schedule_id":"sched-1","timezone":"UTC","grid_slot_start":"2026-05-04T11:00:00Z","assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z","target":{"kind":"user","ref":"u-alice"},"louder":true}`},
		{name: "the process zone", raw: `{"kind":"handoff","team_name":"Backend","schedule_id":"sched-1","timezone":"Local","grid_slot_start":"2026-05-04T11:00:00Z","assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z","target":{"kind":"user","ref":"u-alice"}}`},
		{name: "a kind nobody declared", raw: `{"kind":"shouting","team_name":"Backend","schedule_id":"sched-1","timezone":"UTC","grid_slot_start":"2026-05-04T11:00:00Z","assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z","target":{"kind":"user","ref":"u-alice"}}`},
		{name: "something after the value", raw: `{"kind":"handoff","team_name":"Backend","schedule_id":"sched-1","timezone":"UTC","grid_slot_start":"2026-05-04T11:00:00Z","assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z","target":{"kind":"user","ref":"u-alice"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeHandoffPayloadV1(1, []byte(tc.raw)); err == nil {
				t.Fatal("a stored announcement was read back that should not have been")
			}
		})
	}
}

// TestAHandoffAdmissionDerivesEverythingFromTheOccurrence.
func TestAHandoffAdmissionDerivesEverythingFromTheOccurrence(t *testing.T) {
	admission, err := sampleBatch(
		sampleRecipient("telegram", "u-bob"),
		sampleRecipient("slack", "u-alice"),
	).Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	wantKey, _ := sampleOccurrence().Key()
	if admission.BatchKey != wantKey {
		t.Fatalf("the claim is held under %q", admission.BatchKey)
	}
	if admission.Kind != KindHandoff {
		t.Fatalf("the claim is a %q", admission.Kind)
	}
	// No alert group and no snapshot: an announcement is about a shift change,
	// and the two fields that describe an alert stay empty rather than being
	// filled with something that looks like an answer.
	if admission.AlertGroupID != "" || admission.SnapshotSchemaVersion != 0 {
		t.Fatalf("the announcement claims to be about alert group %q at schema %d",
			admission.AlertGroupID, admission.SnapshotSchemaVersion)
	}
	if admission.Outcome != OutcomeAdmitted || len(admission.Commitments) != 2 {
		t.Fatalf("the admission is %q with %d commitments",
			admission.Outcome, len(admission.Commitments))
	}
	// Ordered by key, so two producers racing on one occurrence insert the same
	// rows in the same order and collide deterministically.
	if admission.Commitments[0].IdempotencyKey > admission.Commitments[1].IdempotencyKey {
		t.Fatal("the commitments came back unordered")
	}
	for _, c := range admission.Commitments {
		payload, ok := c.Payload.(HandoffPayloadV1)
		if !ok {
			t.Fatalf("an announcement was admitted carrying a %T", c.Payload)
		}
		// The event the message describes is the event the claim is held for.
		// It is not checked so much as made: these fields are read off the
		// occurrence inside Admit and are not the producer's to supply.
		if payload.Kind != sampleOccurrence().Kind ||
			payload.ScheduleID != sampleOccurrence().ScheduleID ||
			!payload.AssignmentStart.Equal(sampleOccurrence().AssignmentStart) {
			t.Fatalf("the message describes another event: %+v", payload)
		}
		if payload.Target.Ref != c.Target.Ref {
			t.Fatalf("the message is addressed to %s and the commitment to %s",
				payload.Target.Ref, c.Target.Ref)
		}
	}
}

// TestAnAnnouncementGoesOnlyToPeopleTheShiftChangeIsAbout.
//
// The occurrence names who came on duty, and the claim is held for exactly that
// event. Announcing to somebody outside it would tell a person about a shift
// they are not on, under a key that then holds the real announcement out.
func TestAnAnnouncementGoesOnlyToPeopleTheShiftChangeIsAbout(t *testing.T) {
	_, err := sampleBatch(sampleRecipient("slack", "u-carol")).Admit()
	if err == nil {
		t.Fatal("an announcement went to somebody the shift change is not about")
	}
	if !strings.Contains(err.Error(), "u-carol") {
		t.Fatalf("the refusal does not say who: %v", err)
	}
}

// TestAnAnnouncementToNobodyIsAnAnswer. "Nobody had a channel we can write to"
// is a result, not a failure, and it has to be expressible: without it the
// occurrence would be re-proposed on every tick forever.
func TestAnAnnouncementToNobodyIsAnAnswer(t *testing.T) {
	admission, err := sampleBatch().Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if admission.Outcome != OutcomeNoTargets {
		t.Fatalf("an admission with no commitments is %q", admission.Outcome)
	}
	if len(admission.Fingerprint) != 32 {
		t.Fatalf("an admission of nobody has a %d-byte fingerprint", len(admission.Fingerprint))
	}
}

// TestTheOlderTimingKindsEncodeExactlyAsBefore.
//
// A third kind was added to the expiry field for the handover deadline, and the
// one thing that had to hold is that it added nothing to the other two. If it
// had, every escalation fingerprint ever computed would change, and every
// admission already stored would read as a conflict with its own repeat.
//
// The bytes below were written out by hand from the protocol, not captured from
// this implementation: a value taken from the code it checks would move with the
// code and prove nothing. They cover the FIELD rather than a whole fingerprint,
// which is where the change is - the batch vectors above cover the case of no
// expiry at all, and they did not move either.
func TestTheOlderTimingKindsEncodeExactlyAsBefore(t *testing.T) {
	deadline := time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		spec TimingSpec
		want string
	}{
		{
			name: "absolute",
			spec: TimingSpec{Kind: TimingAbsolute, At: deadline},
			want: "000000086162736f6c7574650000000818ac57b7d6dde000",
		},
		{
			name: "relative to admission",
			spec: TimingSpec{Kind: TimingRelativeToAdmission, Offset: 90 * time.Second},
			want: "0000001572656c61746976655f746f5f61646d697373696f6e0000000800000014f46b0400",
		},
		{
			// The new one, and its atoms are in a fixed order: the domain
			// instant first, the maximum age second.
			name: "bounded",
			spec: TimingSpec{
				Kind:   TimingBounded,
				At:     time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
				MaxAge: time.Hour,
			},
			want: "00000007626f756e6465640000000818ac71e95ca2e000000000080000034630b8a000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.spec.encode(&buf); err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got := hex.EncodeToString(buf.Bytes()); got != tc.want {
				t.Fatalf("the field encodes as\n  %s\nand the protocol says\n  %s", got, tc.want)
			}
		})
	}
}

// TestABoundedDeadlineNeedsBothHalves. Each half answers a different question -
// "the shift this is about has ended" and "this has been true too long to be
// news" - and one of them alone is not a deadline for an announcement nobody
// can acknowledge.
func TestABoundedDeadlineNeedsBothHalves(t *testing.T) {
	deadline := time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		spec TimingSpec
	}{
		{"no domain instant", TimingSpec{Kind: TimingBounded, MaxAge: time.Hour}},
		{"no maximum age", TimingSpec{Kind: TimingBounded, At: deadline}},
		{"a negative maximum age", TimingSpec{Kind: TimingBounded, At: deadline, MaxAge: -time.Hour}},
		{"an offset as well", TimingSpec{Kind: TimingBounded, At: deadline, MaxAge: time.Hour, Offset: time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.spec.Validate(); err == nil {
				t.Fatal("half a bounded deadline was accepted")
			}
		})
	}

	// And the older kinds refuse the new field, so a caller cannot half-migrate
	// one of them into the new shape and have the extra half silently dropped.
	for _, spec := range []TimingSpec{
		{Kind: TimingAbsolute, At: deadline, MaxAge: time.Hour},
		{Kind: TimingRelativeToAdmission, Offset: time.Minute, MaxAge: time.Hour},
	} {
		if err := spec.Validate(); err == nil {
			t.Fatalf("a %s spec carrying a maximum age was accepted", spec.Kind)
		}
	}
}

// The wire vectors. Every value below was written out by hand from the
// written protocol, over inputs the test supplies - not captured from this
// implementation. A vector taken from the code it checks moves with
// the code and proves nothing.

// TestTheHandoffBatchFingerprintIsGolden pins both outcomes.
//
// The empty one matters most, as it does for escalations: an admission that
// found nobody to announce to has no commitments to hash, so everything telling
// two such proposals apart has to be in the material before the list. For a
// handover that is the occurrence digest, and if it were dropped every "nobody
// to tell" on every schedule would fingerprint identically.
func TestTheHandoffBatchFingerprintIsGolden(t *testing.T) {
	admitted, err := sampleBatch(
		sampleRecipient("slack", "u-alice"),
		sampleRecipient("telegram", "u-bob"),
	).Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if got, want := hex.EncodeToString(admitted.Fingerprint),
		"65efc48987a66680bb22755b678e0bf872b114614e17ded247d140611eceb733"; got != want {
		t.Errorf("admitted proposal\n got: %s\nwant: %s", got, want)
	}

	empty, err := sampleBatch().Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if got, want := hex.EncodeToString(empty.Fingerprint),
		"5195c241c60946ef9717db06dc213b15101e1426177986d2942e15cd518e5a47"; got != want {
		t.Errorf("empty proposal\n got: %s\nwant: %s", got, want)
	}
}

// TestTheHandoffSubmitIntentIsGolden pins the whole commitment material, all
// fourteen fields, with a bounded deadline in the tenth.
//
// The field test above proves the deadline encodes as the protocol says. This
// proves it reaches the fingerprint: a build that computed the deadline
// correctly and then dropped it on the way into the material would pass that
// test and fail this one.
func TestTheHandoffSubmitIntentIsGolden(t *testing.T) {
	digest, err := sampleOccurrence().Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	deadline := TimingSpec{
		Kind:   TimingBounded,
		At:     time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
		MaxAge: time.Hour,
	}
	material, err := submitIntent{
		Kind:            KindHandoff,
		GrammarVersion:  GrammarV1,
		Provider:        "slack",
		Target:          Target{Kind: TargetUser, Ref: "u-alice"},
		Operation:       OperationSend,
		Editable:        false,
		Content:         occurrenceRef{Digest: digest},
		Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
		Expiry:          &deadline,
		CompletionMode:  CompletionOnAcceptance,
		AmbiguityPolicy: PolicyRetry,
		Payload:         samplePayload("u-alice"),
	}.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sum := sha256.Sum256(material)
	if got, want := hex.EncodeToString(sum[:]),
		"1228ea97775a8da759d9542ebd4bab757f855c6885f86d870ce0047bbd4a9a8d"; got != want {
		t.Errorf("handover commitment\n got: %s\nwant: %s", got, want)
	}
}

// TestAnEscalationWithADeadlineIsUnchanged is the other half of the extension's
// promise, and the expensive half to get wrong.
//
// Adding a third kind to the expiry field had to leave the two older ones
// exactly as they were. These are whole commitment materials, not just the
// field: had the extension shifted anything - a length, an order, a marker -
// every escalation fingerprint ever computed would change, every stored
// admission would read as a conflict with its own repeat, and the repair would
// be a protocol version.
//
// The content reference is supplied directly rather than through a render
// snapshot, so what these vectors pin is the commitment encoding and nothing
// about how a snapshot happens to hash today.
func TestAnEscalationWithADeadlineIsUnchanged(t *testing.T) {
	content := contentRef{
		AlertGroupID: "ag-0001", Revision: 3,
		SnapshotDigest: sequentialDigest(),
	}
	build := func(t *testing.T, expiry TimingSpec) string {
		t.Helper()
		material, err := submitIntent{
			Kind:            KindEscalation,
			GrammarVersion:  GrammarV1,
			Provider:        "slack",
			Target:          Target{Kind: TargetChannel, Ref: "C0001"},
			Operation:       OperationSend,
			Editable:        true,
			Content:         content,
			Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
			Expiry:          &expiry,
			CompletionMode:  CompletionOnAcceptance,
			AmbiguityPolicy: PolicyRetry,
			Payload: EscalationPayloadV1{
				Slot:        Slot{Kind: SlotPolicy, Index: 2},
				Target:      Target{Kind: TargetChannel, Ref: "C0001"},
				Interactive: true,
			},
		}.encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		sum := sha256.Sum256(material)
		return hex.EncodeToString(sum[:])
	}

	if got, want := build(t, TimingSpec{
		Kind: TimingAbsolute, At: time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
	}), "f2be9fb27c32f99ff027a6ae716a19aecf64d1657765278edf981ad54d9cb22b"; got != want {
		t.Errorf("escalation with an absolute deadline\n got: %s\nwant: %s", got, want)
	}

	if got, want := build(t, TimingSpec{
		Kind: TimingRelativeToAdmission, Offset: 90 * time.Second,
	}), "55ed5c13e9c0b3b76199b6d9deabe672e2a49f929212e4da94727aca8f26f55f"; got != want {
		t.Errorf("escalation with a relative deadline\n got: %s\nwant: %s", got, want)
	}
}

// sequentialDigest is a 32-byte value chosen for being easy to write down: the
// vectors above are computed by hand, and a real snapshot digest would make
// that impossible to check.
func sequentialDigest() []byte {
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i)
	}
	return digest
}

// TestAStoredAnnouncementSurvivesARoundTrip.
//
// The durable spelling and the canonical material are two different wire forms
// of one value, and equal digests do not prove equal reading: two builds can
// agree on the material and disagree about the JSON. What goes into the column
// has to come back out as the same announcement.
func TestAStoredAnnouncementSurvivesARoundTrip(t *testing.T) {
	original := samplePayload("u-alice")
	stored, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("store the announcement: %v", err)
	}
	read, err := DecodeHandoffPayloadV1(1, stored)
	if err != nil {
		t.Fatalf("read the announcement back: %v", err)
	}
	if read != original {
		t.Fatalf("the announcement changed in storage:\n  was %+v\n  now %+v", original, read)
	}

	// And the same digest, which is the other half: the two wire forms describe
	// one value or neither is trustworthy.
	first, err := PayloadDigest(KindHandoff, 1, stored)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	again, err := json.Marshal(read)
	if err != nil {
		t.Fatalf("store it again: %v", err)
	}
	second, err := PayloadDigest(KindHandoff, 1, again)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if hex.EncodeToString(first) != hex.EncodeToString(second) {
		t.Fatal("a round trip changed the digest")
	}
}

// TestAnAnnouncementNeedsAllThreeMoments. A missing one decodes as year zero
// and renders as a shift that started in the year 1: the channel shows it,
// because nothing downstream re-checks what the payload already said.
func TestAnAnnouncementNeedsAllThreeMoments(t *testing.T) {
	for _, missing := range []string{"grid_slot_start", "assignment_start", "assignment_end"} {
		t.Run("without "+missing, func(t *testing.T) {
			fields := map[string]any{
				"kind": "handoff", "team_name": "Backend", "schedule_id": "sched-1",
				"timezone":         "UTC",
				"grid_slot_start":  "2026-05-04T11:00:00Z",
				"assignment_start": "2026-05-04T11:00:00Z",
				"assignment_end":   "2026-05-04T19:00:00Z",
				"target":           map[string]string{"kind": "user", "ref": "u-alice"},
			}
			delete(fields, missing)
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatalf("build the payload: %v", err)
			}
			if _, err := DecodeHandoffPayloadV1(1, raw); err == nil {
				t.Fatalf("an announcement with no %s was read back", missing)
			}
		})
	}
}

// TestAnAnnouncementIsStoredInUTC. A producer that wrote its instants with an
// offset wrote the same moments, and the canonical material says so - but the
// stored JSON would be a second spelling of one announcement, which is what a
// person reading the row and a build diffing two rows would both trip over.
func TestAnAnnouncementIsStoredInUTC(t *testing.T) {
	moscow := time.FixedZone("MSK", 3*60*60)
	raw, err := json.Marshal(map[string]any{
		"kind": "handoff", "team_name": "Backend", "schedule_id": "sched-1",
		"timezone":         "UTC",
		"grid_slot_start":  time.Date(2026, 5, 4, 14, 0, 0, 0, moscow),
		"assignment_start": time.Date(2026, 5, 4, 14, 0, 0, 0, moscow),
		"assignment_end":   time.Date(2026, 5, 4, 22, 0, 0, 0, moscow),
		"target":           map[string]string{"kind": "user", "ref": "u-alice"},
	})
	if err != nil {
		t.Fatalf("build the payload: %v", err)
	}
	read, err := DecodeHandoffPayloadV1(1, raw)
	if err != nil {
		t.Fatalf("read the announcement: %v", err)
	}
	if read != samplePayload("u-alice") {
		t.Fatalf("an announcement written with an offset read back as %+v", read)
	}
}

// TestOneShiftChangeHasOneDeadline.
//
// The announcement stops being worth making at a moment, and that moment is a
// property of the shift change - not of the channel it goes through. A
// recipient able to carry its own deadline could be given none at all, or one
// later than the shift it announces, and two people would be told about one
// event under two different rules while both messages showed the same end time.
func TestOneShiftChangeHasOneDeadline(t *testing.T) {
	admission, err := sampleBatch(
		sampleRecipient("slack", "u-alice"),
		sampleRecipient("telegram", "u-bob"),
	).Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	want := TimingSpec{
		Kind:   TimingBounded,
		At:     time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
		MaxAge: time.Hour,
	}
	for _, c := range admission.Commitments {
		if c.Expiry == nil {
			t.Fatalf("the announcement to %s never expires", c.Target.Ref)
		}
		if c.Expiry.Kind != want.Kind || !c.Expiry.At.Equal(want.At) ||
			c.Expiry.MaxAge != want.MaxAge {
			t.Fatalf("the announcement to %s expires by %+v, and the shift ends at %s",
				c.Target.Ref, *c.Expiry, want.At)
		}
	}

	// And it is a copy: a caller still holding the batch cannot move a deadline
	// that has already been fingerprinted.
	first, second := admission.Commitments[0].Expiry, admission.Commitments[1].Expiry
	if first == second {
		t.Fatal("two commitments share one deadline value")
	}
}

// TestAnAnnouncementWithNoDeadlineIsRefused. There is no acknowledgement to end
// it, so a handover without a deadline is one that retries until it is
// delivered - to somebody whose shift may have ended hours ago.
func TestAnAnnouncementWithNoDeadlineIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*HandoffBatch)
	}{
		{"no maximum age", func(b *HandoffBatch) { b.MaxAge = 0 }},
		{"a negative maximum age", func(b *HandoffBatch) { b.MaxAge = -time.Hour }},
		{"no end to the shift", func(b *HandoffBatch) { b.AssignmentEnd = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch := sampleBatch(sampleRecipient("slack", "u-alice"))
			tc.alter(&batch)
			if _, err := batch.Admit(); err == nil {
				t.Fatal("an announcement that never expires was admitted")
			}
		})
	}
}

// TestThisBuildKnowsWhichPayloadSchemasItHas. The question is asked before the
// bytes are read, and it has to answer "no" for a version from a newer build -
// otherwise an unreadable payload and an unsupported one look the same, and the
// first ends work the second would have delivered.
func TestThisBuildKnowsWhichPayloadSchemasItHas(t *testing.T) {
	for _, tc := range []struct {
		kind    Kind
		version int
		known   bool
	}{
		{KindEscalation, 1, true},
		{KindEscalationReplay, 1, true},
		{KindHandoff, 1, true},
		{KindHandoff, 2, false},
		{KindEscalation, 2, false},
		{Kind("something_newer"), 1, false},
	} {
		if got := KnowsPayloadSchema(tc.kind, tc.version); got != tc.known {
			t.Errorf("%s schema %d: known=%v, want %v", tc.kind, tc.version, got, tc.known)
		}
	}
}
