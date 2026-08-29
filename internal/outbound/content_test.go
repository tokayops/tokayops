package outbound

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The two forms an attempt's content comes in, and the one thing that matters
// about the pair: the accessors of the form you do not have answer "no", not
// zero.

func testSnapshot(t *testing.T, revision int64) keys.RenderSnapshot {
	t.Helper()
	state, err := keys.NewRenderSnapshot(keys.SnapshotInput{
		AlertGroupID: "ag-1", Status: keys.GroupTriggered, Title: "Disk filling up",
		Severity: "critical", Revision: revision, DisplayTimezone: "UTC",
		Alerts: []keys.AlertSnapshot{{
			Fingerprint: "fp-1", Status: keys.AlertFiring,
			StartsAt: time.Unix(1700000000, 0).UTC(), AlertName: "DiskWillFill",
		}},
	})
	if err != nil {
		t.Fatalf("build the state: %v", err)
	}
	return state
}

// TestContentDrawnFromAPayloadHasNoRevision.
//
// This is the whole reason the pair is closed. A commitment drawn from its own
// payload has no series of revisions to be at a position in, and the honest
// answer to "which revision does this apply" is "none". Answering zero would
// put a row in the journal claiming to have caught up with a state nobody ever
// froze - and, downstream, would settle the commitment as up to date.
func TestContentDrawnFromAPayloadHasNoRevision(t *testing.T) {
	content, err := NewPayloadContent(testSnapshot(t, 4).Digest())
	if err != nil {
		t.Fatalf("build the content: %v", err)
	}
	if content.Form() != ContentPayload {
		t.Fatalf("the content is a %s", content.Form())
	}
	if revision, has := content.Revision(); has {
		t.Fatalf("a payload carries revision %d", revision)
	}
	if _, drawn := content.Snapshot(); drawn {
		t.Fatal("a payload produced a state to render")
	}
	if content.Final() {
		t.Fatal("a payload claims to be the last of something")
	}
	if len(content.Digest()) == 0 {
		t.Fatal("the content records nothing as the thing it was made from")
	}
}

// TestContentDrawnFromASnapshotAnswersAllThree. The other side of the same
// rule, so that a build which broke the pair by making everything answer "no"
// fails here rather than passing quietly.
func TestContentDrawnFromASnapshotAnswersAllThree(t *testing.T) {
	state := testSnapshot(t, 7)
	content, err := NewSnapshotContent(state, true)
	if err != nil {
		t.Fatalf("build the content: %v", err)
	}
	revision, has := content.Revision()
	if !has || revision != 7 {
		t.Fatalf("the revision reads %d (present: %v)", revision, has)
	}
	drawn, ok := content.Snapshot()
	if !ok {
		t.Fatal("a snapshot produced no state to render")
	}
	if drawn.Content().AlertGroupID != "ag-1" {
		t.Fatalf("the state describes %q", drawn.Content().AlertGroupID)
	}
	if !content.Final() {
		t.Fatal("the last revision does not say so")
	}
	if string(content.Digest()) != string(state.Digest()) {
		t.Fatal("the content records a digest other than the state's own")
	}
}

// TestADigestOfTheWrongLengthIsRefused. The digest is what an attempt records
// as the thing it was made from, and it is compared against the one the
// admission stored. A short or empty one would compare equal to another short
// or empty one.
func TestADigestOfTheWrongLengthIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		digest []byte
	}{
		{name: "nothing at all", digest: nil},
		{name: "one byte short", digest: make([]byte, 31)},
		{name: "one byte over", digest: make([]byte, 33)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPayloadContent(tc.digest); err == nil {
				t.Fatal("a digest of the wrong length was accepted")
			}
		})
	}
}

// TestTheContentCannotBeChangedFromOutside. Digest hands back a copy: a caller
// able to write into it could change what an attempt says it was made from
// after the attempt was opened.
func TestTheContentCannotBeChangedFromOutside(t *testing.T) {
	content, err := NewPayloadContent(testSnapshot(t, 1).Digest())
	if err != nil {
		t.Fatalf("build the content: %v", err)
	}
	taken := content.Digest()
	for i := range taken {
		taken[i] = 0
	}
	if string(content.Digest()) == string(taken) {
		t.Fatal("the content handed out its own bytes")
	}
}

// TestTheRevisionComesFromTheStateAndNowhereElse.
//
// The number and the bytes cannot be given separately, which is what makes the
// bad pair unrepresentable rather than merely rejected. A state of revision 4
// with the number 7 beside it would have the channel draw 4 and the journal
// record 7, next to the digest of 4: the commitment settles as caught up while
// showing something older, and nothing later corrects it because the numbers
// already agree.
//
// If this test ever has to be rewritten because the constructor grew a revision
// argument back, that is the change to argue with, not the test.
func TestTheRevisionComesFromTheStateAndNowhereElse(t *testing.T) {
	for _, revision := range []int64{0, 1, 9} {
		state := testSnapshot(t, revision)
		content, err := NewSnapshotContent(state, false)
		if err != nil {
			t.Fatalf("build the content: %v", err)
		}
		got, has := content.Revision()
		if !has || got != revision {
			t.Fatalf("a state of revision %d produced %d (present: %v)",
				revision, got, has)
		}
		if string(content.Digest()) != string(state.Digest()) {
			t.Fatalf("the content of revision %d records another state's digest", revision)
		}
	}
}
