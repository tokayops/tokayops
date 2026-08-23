package telegram

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The same rule as the Slack card, for the other channel: what a message
// depends on is the snapshot and the interactivity the delivery was admitted
// with. Telegram has no thread, so the whole message is one string - which
// makes "the same bytes" easy to state and easy to check.

func cardState() keys.SnapshotInput {
	groupURL := "https://tokay.example/#/ops/alert-groups/ag-1"
	external := "https://alertmanager.example/#/alerts"
	ackedBy := "nina"

	return keys.SnapshotInput{
		AlertGroupID: "ag-1", Status: keys.GroupTriggered,
		Title: "Disk filling up", Severity: "critical", TeamOnboarded: true,
		GroupURL: &groupURL, ExternalURL: &external, AcknowledgedBy: &ackedBy,
		DisplayTimezone: "Europe/Berlin",
		Alerts: []keys.AlertSnapshot{{
			Fingerprint: "fp-1", Status: keys.AlertFiring,
			StartsAt:  time.Unix(1700000000, 0).UTC(),
			AlertName: "DiskWillFill", Severity: "critical",
		}},
	}
}

func TestACardIsAFunctionOfItsSnapshot(t *testing.T) {
	state := cardState()
	first := RenderCard(state)

	previous := time.Local
	t.Cleanup(func() { time.Local = previous })
	time.Local = time.FixedZone("nowhere", 13*3600)

	if second := RenderCard(state); second != first {
		t.Fatalf("the same snapshot rendered differently:\n%s\n%s", first, second)
	}
	if !strings.Contains(first, `<a href="https://tokay.example/#/ops/alert-groups/ag-1">`) {
		t.Fatalf("the footer link is not the snapshot's: %s", first)
	}
}

// TestTheKeyboardFollowsTheAdmission. An empty keyboard is not the same as no
// keyboard: editMessageText leaves the buttons alone when reply_markup is
// absent, so a card posted before interactivity was switched off would keep
// live buttons forever.
func TestTheKeyboardFollowsTheAdmission(t *testing.T) {
	state := cardState()

	if got := KeyboardFor(state, false); !isEmptyKeyboard(t, got) {
		t.Fatalf("a delivery admitted without buttons produced %v", got)
	}

	with := KeyboardFor(state, true)
	if isEmptyKeyboard(t, with) {
		t.Fatal("a delivery admitted with buttons produced none")
	}
	body, _ := json.Marshal(with)
	if !strings.Contains(string(body), "Acknowledge") || !strings.Contains(string(body), "Resolve") {
		t.Fatalf("the keyboard offers %s", body)
	}

	// A resolved card takes the buttons back; a team TokayOps does not have
	// never gets them.
	resolved := cardState()
	resolved.Status = keys.GroupResolved
	if got := KeyboardFor(resolved, true); !isEmptyKeyboard(t, got) {
		t.Fatalf("a resolved card kept its buttons: %v", got)
	}
	unknownTeam := cardState()
	unknownTeam.TeamOnboarded = false
	if got := KeyboardFor(unknownTeam, true); !isEmptyKeyboard(t, got) {
		t.Fatalf("an unknown team got buttons nobody can answer: %v", got)
	}

	// With nowhere to send people, there is nothing to take back either.
	noLink := cardState()
	noLink.GroupURL = nil
	if got := KeyboardFor(noLink, true); got != nil {
		t.Fatalf("expected no keyboard at all, got %v", got)
	}
}

func isEmptyKeyboard(t *testing.T, keyboard interface{}) bool {
	t.Helper()
	if keyboard == nil {
		return false
	}
	body, err := json.Marshal(keyboard)
	if err != nil {
		t.Fatalf("serialise the keyboard: %v", err)
	}
	return string(body) == `{"inline_keyboard":[]}`
}
