package store

import (
	"testing"

	"github.com/tokayops/tokayops/internal/outbound"
)

// TestTheHistoryNamesTheSeverityWithoutAFirehoseKey. The line is what an
// operator reads when a card was never going to be sent; it has to name the
// word the alert rule used, because the rule is where the fix is.
func TestTheHistoryNamesTheSeverityWithoutAFirehoseKey(t *testing.T) {
	got := unpromisedMessage(outbound.UnpromisedStep{
		Step: "firehose", Reason: outbound.ReasonNoFirehoseChannel, Detail: `severity "error"`,
	})
	if want := `No firehose channel for severity "error"`; got != want {
		t.Fatalf("the history says %q, want %q", got, want)
	}
}
