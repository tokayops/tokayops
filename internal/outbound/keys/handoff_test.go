package keys

import (
	"bytes"
	"encoding/hex"
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
	return HandoffPayloadV1{
		Kind:            HandoffShiftChange,
		TeamName:        "Backend",
		ScheduleID:      "sched-1",
		Timezone:        "UTC",
		GridSlotStart:   time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		AssignmentStart: time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		AssignmentEnd:   time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
		Target:          Target{Kind: TargetUser, Ref: user},
	}
}

// TestTheOccurrenceKeyIsTheOneTheJobEngineWrote is the vector that was carried
// across, not recomputed.
//
// The value below is the one `jobdedup.TestHandoffOccurrenceGoldenVector` has
// pinned since Epic 11, and it is here for one reason: a running installation
// has already announced shift changes under these keys. A digest that shifted
// during the move would announce every one of them a second time, and the rows
// already written would sit in the table claiming nothing.
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
	occurrence := sampleOccurrence()
	admission, err := HandoffBatch{
		Occurrence: occurrence, GrammarVersion: GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
		Commitments: []HandoffCommitment{
			{
				Provider: "telegram", UserID: "u-bob", Payload: samplePayload("u-bob"),
				Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
				CompletionMode:  CompletionOnAcceptance,
				AmbiguityPolicy: PolicyRetry,
			},
			{
				Provider: "slack", UserID: "u-alice", Payload: samplePayload("u-alice"),
				Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
				CompletionMode:  CompletionOnAcceptance,
				AmbiguityPolicy: PolicyRetry,
			},
		},
	}.Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	wantKey, _ := occurrence.Key()
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
		if _, ok := c.Payload.(HandoffPayloadV1); !ok {
			t.Fatalf("an announcement was admitted carrying a %T", c.Payload)
		}
	}
}

// TestAnAnnouncementToNobodyIsAnAnswer. "Nobody had a channel we can write to"
// is a result, not a failure, and it has to be expressible: without it the
// occurrence would be re-proposed on every tick forever.
func TestAnAnnouncementToNobodyIsAnAnswer(t *testing.T) {
	admission, err := HandoffBatch{
		Occurrence: sampleOccurrence(), GrammarVersion: GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
	}.Admit()
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

// TestACommitmentAboutAnotherScheduleIsRefused. The schedule is named in the
// occurrence and again in every payload, and a batch where they disagree would
// announce one schedule's shift under another's claim.
func TestACommitmentAboutAnotherScheduleIsRefused(t *testing.T) {
	payload := samplePayload("u-alice")
	payload.ScheduleID = "sched-9"
	_, err := HandoffBatch{
		Occurrence: sampleOccurrence(), GrammarVersion: GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
		Commitments: []HandoffCommitment{{
			Provider: "slack", UserID: "u-alice", Payload: payload,
			Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
			CompletionMode:  CompletionOnAcceptance,
			AmbiguityPolicy: PolicyRetry,
		}},
	}.Admit()
	if err == nil {
		t.Fatal("an announcement about another schedule was admitted")
	}
	if !strings.Contains(err.Error(), "sched-9") {
		t.Fatalf("the refusal does not say which: %v", err)
	}
}

// TestARecipientNamedTwiceHasToAgree. The commitment names the person and so
// does its payload; a row where they differ sends to one and is deduplicated
// as the other.
func TestARecipientNamedTwiceHasToAgree(t *testing.T) {
	_, err := HandoffBatch{
		Occurrence: sampleOccurrence(), GrammarVersion: GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(),
		Commitments: []HandoffCommitment{{
			Provider: "slack", UserID: "u-alice", Payload: samplePayload("u-bob"),
			Timing:          TimingSpec{Kind: TimingRelativeToAdmission},
			CompletionMode:  CompletionOnAcceptance,
			AmbiguityPolicy: PolicyRetry,
		}},
	}.Admit()
	if err == nil {
		t.Fatal("a commitment for one person carrying a payload for another was admitted")
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
