package outbound

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What an attempt is made from, and the two forms it comes in.
//
// A card is drawn from a state that has revisions: the alert group's snapshot,
// frozen at the revision this attempt applies. A one-shot escalation is drawn
// from the state its admission froze, which is the same shape. A handover
// announcement has neither - everything it says is its own, and its state IS
// its payload.
//
// The two are a closed pair rather than "a snapshot, or nothing", because three
// durable facts are read from here and a missing snapshot would answer all of
// them with a zero: the revision the attempt applies, the digest of what it was
// made from, and whether that revision was the last one. A commitment with no
// revisions would record having applied revision 0 of nothing.

// ContentForm says which of the two an attempt is made from.
type ContentForm string

const (
	// ContentSnapshot: a frozen render snapshot, with a revision and a
	// finality of its own.
	ContentSnapshot ContentForm = "snapshot"

	// ContentPayload: the commitment's own payload, and nothing else. No
	// revision exists, so none is recorded.
	ContentPayload ContentForm = "payload"
)

// AttemptContent is one of the two, and which one cannot be asked twice: the
// accessors that belong to the other form answer "no" rather than a zero.
type AttemptContent struct {
	form     ContentForm
	snapshot keys.RenderSnapshot
	revision int64
	final    bool
	digest   []byte
}

// NewSnapshotContent is an attempt drawn from a frozen state.
//
// The revision is READ FROM the snapshot, never passed beside it. Taken as an
// argument it could disagree: a state of revision 4 with the number 7 would
// have the channel draw revision 4 and the journal record having applied 7,
// beside the digest of 4 - the commitment settles as caught up while showing
// something older, and no later revision corrects it because the numbers
// already agree. That is precisely the state this closed form exists to make
// unrepresentable, and an argument would hand it back.
func NewSnapshotContent(snapshot keys.RenderSnapshot, final bool) (AttemptContent, error) {
	digest := snapshot.Digest()
	if len(digest) == 0 {
		return AttemptContent{}, contentContractf("a snapshot with no digest")
	}
	revision := snapshot.Content().Revision
	if revision < 0 {
		return AttemptContent{}, contentContractf("revision %d is negative", revision)
	}
	return AttemptContent{
		form: ContentSnapshot, snapshot: snapshot,
		revision: revision, final: final, digest: digest,
	}, nil
}

// NewPayloadContent is an attempt drawn from the commitment's payload.
//
// The digest is of the CANONICAL payload, computed by the same codec the
// admission used - not of the stored bytes. Two representations of one value
// have to give one digest, which is the whole reason the codec exists.
func NewPayloadContent(digest []byte) (AttemptContent, error) {
	if len(digest) != sha256.Size {
		return AttemptContent{}, contentContractf(
			"a payload digest of %d bytes, expected %d", len(digest), sha256.Size)
	}
	return AttemptContent{form: ContentPayload, digest: bytes.Clone(digest)}, nil
}

// Form says which of the two this is.
func (c AttemptContent) Form() ContentForm { return c.form }

// Digest is what the attempt records as the thing it was made from. Both forms
// have one; that is what makes them comparable at all.
func (c AttemptContent) Digest() []byte { return bytes.Clone(c.digest) }

// Snapshot is the frozen state, and false when there is none. A handler that
// draws from a snapshot asks here rather than reading a zero value: an empty
// snapshot renders a message about nothing, which is worse than not sending.
func (c AttemptContent) Snapshot() (keys.RenderSnapshot, bool) {
	if c.form != ContentSnapshot {
		return keys.RenderSnapshot{}, false
	}
	return c.snapshot, true
}

// Revision is the revision this attempt applies, and false when the commitment
// has no revisions. Not zero: a commitment that recorded applying revision 0
// would be claiming to have caught up with a state that does not exist.
func (c AttemptContent) Revision() (int64, bool) {
	if c.form != ContentSnapshot {
		return 0, false
	}
	return c.revision, true
}

// Final says whether the revision this attempt applies is the last one there
// will be. Only a snapshot has a last revision; a payload has no series.
func (c AttemptContent) Final() bool { return c.form == ContentSnapshot && c.final }

// contentContractf is a refusal about the shape of an attempt's content. It is
// a contract violation rather than a delivery failure: nothing about the
// network can produce one.
func contentContractf(format string, args ...any) error {
	return fmt.Errorf("outbound content: "+format, args...)
}
