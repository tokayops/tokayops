package keys

import (
	"encoding/hex"
	"strings"
	"testing"
)

// What a commitment's payload was when it was admitted, and the two things that
// have to be true of it: it is the same for two spellings of one value, and it
// is different for two values.

// TestThePayloadDigestIsTheseBytes is the golden vector.
//
// Computed from the written protocol by hand, not from this implementation:
// a vector taken from the code it checks proves that the code agrees with
// itself. What it holds is the byte-for-byte contract across builds - a digest
// that drifted would let a payload swapped between admission and attempt pass
// as the one that was admitted.
func TestThePayloadDigestIsTheseBytes(t *testing.T) {
	digest, err := PayloadDigest(KindEscalation, 1,
		[]byte(`{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true}`))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	const want = "60e2e596b4f3cdfc6b7e366f6936550bf7c49d146a9ae0378cdcca7053e8f369"
	if got := hex.EncodeToString(digest); got != want {
		t.Fatalf("the payload digest is %s, and the protocol says %s", got, want)
	}

	// Version 2 of the same commitment, with the flag that replaced the
	// buttons. A different schema is a different digest by construction: the
	// version is in the material.
	digest, err = PayloadDigest(KindEscalation, 2,
		[]byte(`{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"stop_on_failure":true}`))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	const wantV2 = "df9d99b273d2fe5073e633d7f5f6145cd7cb1857a067e5fa14ce244ba2b5e1b0"
	if got := hex.EncodeToString(digest); got != wantV2 {
		t.Fatalf("the version 2 payload digest is %s, and the protocol says %s", got, wantV2)
	}
}

// TestADigestIsTakenOverTheRowsOwnSchema. A version 1 row is digested by the
// version 1 encoder - the one its admission used - never by the neutral
// reader: read as version 2 it would digest differently and fail every
// attempt. And each body parses only under its own version.
func TestADigestIsTakenOverTheRowsOwnSchema(t *testing.T) {
	const v1 = `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true}`
	const v2 = `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"stop_on_failure":false}`

	if _, err := PayloadDigest(KindEscalation, 2, []byte(v1)); err == nil {
		t.Fatal("a version 1 body was digested under version 2")
	}
	if _, err := PayloadDigest(KindEscalation, 1, []byte(v2)); err == nil {
		t.Fatal("a version 2 body was digested under version 1")
	}
	if _, err := PayloadDigest(KindEscalation, 3, []byte(v2)); err == nil {
		t.Fatal("a schema this build does not have was digested")
	}
}

// TestTwoSpellingsOfOnePayloadAreOneDigest. The digest is of the CANONICAL
// encoding, not of the stored bytes: a producer that wrote its JSON keys in
// another order, or with different whitespace, admitted the same payload.
//
// Hashing the raw bytes would be simpler and wrong: the first reordering by a
// JSON library would read as a payload that had been tampered with.
func TestTwoSpellingsOfOnePayloadAreOneDigest(t *testing.T) {
	first, err := PayloadDigest(KindEscalation, 1,
		[]byte(`{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true}`))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	second, err := PayloadDigest(KindEscalation, 1,
		[]byte("{\n  \"interactive\": true,\n  \"target\": {\"ref\": \"C0001\", \"kind\": \"channel\"},\n"+
			"  \"slot\": {\"kind\": \"firehose\"}\n}"))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if hex.EncodeToString(first) != hex.EncodeToString(second) {
		t.Fatalf("one payload written two ways gave two digests:\n  %x\n  %x", first, second)
	}
}

// TestADifferentPayloadIsADifferentDigest walks the fields one at a time.
//
// Every one of them changes what goes out, and `interactive` is the one worth
// naming: it decides whether the card carries buttons, and a build that let it
// change between admission and attempt would send a card nobody can act on
// while the journal says otherwise.
func TestADifferentPayloadIsADifferentDigest(t *testing.T) {
	const base = `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true}`
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "another recipient", raw: `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0002"},"interactive":true}`},
		{name: "another slot", raw: `{"slot":{"kind":"policy","index":2},"target":{"kind":"channel","ref":"C0001"},"interactive":true}`},
		{name: "buttons taken away", raw: `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":false}`},
		{name: "an override added", raw: `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true,"message_override":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			was, err := PayloadDigest(KindEscalation, 1, []byte(base))
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			now, err := PayloadDigest(KindEscalation, 1, []byte(tc.raw))
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if hex.EncodeToString(was) == hex.EncodeToString(now) {
				t.Fatal("a changed payload kept its digest")
			}
		})
	}
}

// TestAKindWithNoShapeIsRefusedByName. A build meeting a commitment of a kind
// it has no payload shape for stops and says which kind.
//
// It does not fall back to another shape. Two kinds can both be at schema
// version 1 and mean entirely different things, so guessing would decode one as
// the other and send it.
func TestAKindWithNoShapeIsRefusedByName(t *testing.T) {
	_, err := PayloadDigest(Kind("something_newer"), 1, []byte(`{"anything":1}`))
	if err == nil {
		t.Fatal("a kind this build has no shape for was canonicalised anyway")
	}
	if !strings.Contains(err.Error(), "something_newer") {
		t.Fatalf("the refusal does not say which kind: %v", err)
	}
}

// TestTheDigestRefusesWhatTheDecoderRefuses. The digest is not a second, softer
// reader: a payload with a field this build does not know is refused here too,
// because canonicalising it would silently drop the instruction it carries.
func TestTheDigestRefusesWhatTheDecoderRefuses(t *testing.T) {
	_, err := PayloadDigest(KindEscalation, 1,
		[]byte(`{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true,"louder":true}`))
	if err == nil {
		t.Fatal("a payload with an unknown field was canonicalised")
	}
}
