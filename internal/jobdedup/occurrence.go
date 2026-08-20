package jobdedup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"time"
)

// Occurrence is one on-call notification as the thing that happened, in parts.
//
// The constructor takes the parts rather than a finished string because the
// encoding below IS the identity: a caller free to hand over its own spelling
// is a caller free to give one occurrence two of them, and under a forever
// scope the second spelling claims work the first already did.
type Occurrence struct {
	// Kind separates a shift changing hands from someone joining the shift
	// already in progress. Without it a lingering job of one kind would
	// suppress the other.
	Kind string

	// ScheduleID is the schedule the transition happened on.
	ScheduleID string

	// Source is "rotation" or "override", GroupID the rotation group or the
	// logical override. The same people can be on duty for either reason, and
	// moving between the two is a real handover.
	Source  string
	GroupID string

	// UserIDs is who is on duty. Order does not matter: the encoding sorts a
	// copy.
	UserIDs []string

	// AssignmentStart is the moment this assignment took effect - what
	// separates two turns of the same rotation.
	AssignmentStart time.Time

	// RevisionID is the configuration that put this composition on duty - what
	// separates two edits inside one assignment, since editing an override
	// leaves its valid_from where it was.
	RevisionID string
}

// HandoffOccurrence identifies one on-call notification, forever.
//
// # The encoding is part of the contract
//
// The row this key lands in is a claim that is never released, so a change to
// the encoding is a change of identity for the whole family: every occurrence
// already admitted would be admitted a second time under its new spelling.
// That is why the grammar is written out here, pinned by a golden vector in
// the tests, and why changing it means a new namespace rather than an edit:
//
//	be32(n)  = n as an unsigned 32-bit big-endian integer, exactly 4 bytes
//	be64(n)  = n as a signed 64-bit big-endian integer, exactly 8 bytes
//	enc(b)   = be32(len(b)) || b        // b is the UTF-8 bytes as given
//	list(v)  = be32(len(v)) || enc(v[0]) || ... || enc(v[len(v)-1])
//
//	material = enc("handoff_occurrence")   // domain separation: the namespace
//	         || enc(kind)
//	         || enc(scheduleID)
//	         || enc(source)
//	         || enc(groupID)
//	         || list(sorted(userIDs))
//	         || enc(be64(assignmentStart.UTC().UnixNano()))
//	         || enc(revisionID)
//
//	key      = scheduleID || ":" || lowercase hex of sha256(material)
//
// Length before value, on every field including the time, is what keeps two
// fields from bleeding into one another: without it {"a", "bc"} and {"ab", "c"}
// are the same bytes, and a list of one member "x,y" is a list of two members
// "x" and "y". Today every part is a UUID or one of two fixed words, so the
// ambiguity is out of reach - but the encoding was being rewritten anyway, and
// unambiguous costs nothing.
//
// The time is an integer of nanoseconds rather than a formatted timestamp:
// RFC3339Nano drops trailing zeros, so its length depends on the value, and
// any later change of format would silently be a change of identity.
//
// There is no version field in the material. The namespace is the version -
// that is the model's rule for a family whose meaning changes - and a second
// counter would eventually disagree with the first.
//
// The digest is hashed rather than kept readable because the key has no bound
// otherwise: a large duty roster spells out every user ID, and a b-tree index
// entry stops at about 2704 bytes. Past that the insert fails and handover
// notifications stop being created at all. The schedule ID stays in front of
// the digest so that the row - which outlives everything else about the
// occurrence - still says which schedule it belongs to.
func HandoffOccurrence(o Occurrence) *Spec {
	var material bytes.Buffer

	encodeField(&material, []byte(NamespaceHandoffOccurrence))
	encodeField(&material, []byte(o.Kind))
	encodeField(&material, []byte(o.ScheduleID))
	encodeField(&material, []byte(o.Source))
	encodeField(&material, []byte(o.GroupID))
	encodeList(&material, o.UserIDs)
	encodeField(&material, encodeInt64(o.AssignmentStart.UTC().UnixNano()))
	encodeField(&material, []byte(o.RevisionID))

	digest := sha256.Sum256(material.Bytes())
	return mustSpec(NamespaceHandoffOccurrence, o.ScheduleID+":"+hex.EncodeToString(digest[:]))
}

func encodeField(buf *bytes.Buffer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	buf.Write(length[:])
	buf.Write(value)
}

// encodeList writes the count and then every member, each length-prefixed.
//
// The copy is not a precaution about aliasing in general: the caller's slice is
// the composition the notifier keeps in its cache across ticks, and sorting in
// place would reorder what is cached through the array both share.
func encodeList(buf *bytes.Buffer, values []string) {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)

	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(sorted)))
	buf.Write(count[:])
	for _, v := range sorted {
		encodeField(buf, []byte(v))
	}
}

func encodeInt64(v int64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], uint64(v))
	return out[:]
}
