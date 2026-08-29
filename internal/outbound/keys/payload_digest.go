package keys

import (
	"bytes"
	"crypto/sha256"
)

// payloadDigestProtocol is the domain separator of the standalone payload
// digest. Its own, and not shared with the intent fingerprint: a digest that
// reused another protocol's material would collide with a slice of it.
const payloadDigestProtocol = "outbound_payload_digest/v1"

// PayloadDigest is what a commitment's payload was when it was admitted.
//
// The admission stores it on the commitment; every attempt recomputes it from
// the payload on the row and compares. Without it there is nothing to compare
// against: the payload is not in the business key, and its wire form reaches
// only the intent fingerprint, which an attempt does not recompute. A payload
// swapped for a DIFFERENT valid one - the same recipient, another team name,
// another interactive flag - would pass every other check and change what goes
// out.
//
// It is the CANONICAL encoding that is hashed, not the stored bytes: two
// spellings of one value have to give one digest, which is the whole reason
// the codec exists. Reordered JSON keys are the same payload.
//
// The digest carries no version of its own. The rule is pinned to
// payload_schema_version, which is inside the material: a new schema is a new
// encoder and therefore a different digest, which is what it should be.
func PayloadDigest(kind Kind, schemaVersion int, raw []byte) ([]byte, error) {
	payload, err := decodePayload(kind, schemaVersion, raw)
	if err != nil {
		return nil, err
	}

	var material bytes.Buffer
	encStr(&material, payloadDigestProtocol)
	encStr(&material, string(kind))
	enc(&material, int64Bytes(int64(schemaVersion)))
	if err := payload.encode(&material); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(material.Bytes())
	return digest[:], nil
}

// Payload is a stored payload that can be canonicalised.
//
// The set of shapes is CLOSED even though the interface is exported: encode is
// unexported, so only this package can satisfy it. A caller free to supply its
// own encoder would be a caller free to give two different commitments one
// digest, which is the one thing the codec exists to prevent.
type Payload interface {
	// SchemaVersion is the shape this payload is in, stored on the commitment
	// so a later schema is readable rather than guessed at.
	SchemaVersion() int
	encode(buf *bytes.Buffer) error
}

// decodePayload reads a stored payload as the shape its commitment says it is.
//
// The pair decides, not the schema version alone: two kinds may both be at
// version 1 and mean entirely different things, and picking the decoder by
// version would read one as the other.
func decodePayload(kind Kind, schemaVersion int, raw []byte) (Payload, error) {
	switch kind {
	case KindEscalation, KindEscalationReplay:
		decoded, err := DecodeEscalationPayloadV1(schemaVersion, raw)
		if err != nil {
			return nil, err
		}
		return decoded, nil
	case KindHandoff:
		decoded, err := DecodeHandoffPayloadV1(schemaVersion, raw)
		if err != nil {
			return nil, err
		}
		return decoded, nil
	default:
		return nil, contractf(
			"this build has no payload shape for a %q commitment", kind)
	}
}
