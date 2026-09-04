package slack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a card is allowed to depend on.
//
// A retry of a delivery sends under the same provider key as the attempt
// before it. If the bytes differ, the two are different messages wearing one
// identity - and which one somebody saw is then unanswerable. So the rule is
// not "the card usually looks the same": it is that the card is a function of
// the snapshot and the admitted interactivity, and of nothing else at all.

func fullState() keys.SnapshotInput {
	team := "payments"
	setup := "https://tokay.example/#/cfg/teams"
	groupURL := "https://tokay.example/#/ops/alert-groups/ag-1"
	external := "https://alertmanager.example/#/alerts"
	ackedBy := "nina"
	description := "the disk will be full in two hours"
	dashboard := "https://grafana.example/d/disk"
	runbook := "https://runbooks.example/disk"

	users := []string{"U0002", "S0001", "U0001"}
	alerts := make([]keys.AlertSnapshot, 0, len(users))
	for i, user := range users {
		user := user
		alerts = append(alerts, keys.AlertSnapshot{
			Fingerprint: string(rune('a' + i)), Status: keys.AlertFiring,
			StartsAt: time.Unix(1700000000, 0).UTC(), AlertName: "DiskWillFill",
			Severity: "critical", SlackUser: &user, Description: &description,
			DashboardURL: &dashboard, RunbookURL: &runbook,
		})
	}

	return keys.SnapshotInput{
		AlertGroupID: "ag-1", Revision: 3, Status: keys.GroupAcknowledged,
		Title: "Disk filling up", Severity: "critical",
		TeamLabel: &team, TeamOnboarded: false,
		GroupURL: &groupURL, TeamSetupURL: &setup, ExternalURL: &external,
		DisplayTimezone: "Europe/Berlin", AcknowledgedBy: &ackedBy,
		Alerts: alerts,
	}
}

func rendered(t *testing.T, state keys.SnapshotInput, interactive bool) string {
	t.Helper()
	card := Render(state, interactive)
	body, err := json.Marshal(struct {
		Text       string `json:"text"`
		Blocks     any    `json:"blocks"`
		Attachment any    `json:"attachment"`
	}{card.Text, card.Blocks, card.Attachment})
	if err != nil {
		t.Fatalf("serialise the card: %v", err)
	}
	return string(body)
}

// TestACardIsAFunctionOfItsSnapshot moves the process to another timezone
// between two renders of one snapshot. Everything else a renderer used to read
// live - the configuration, the team lookup, this instance's base URL - is not
// reachable from here at all any more, which is the stronger half of the same
// statement.
func TestACardIsAFunctionOfItsSnapshot(t *testing.T) {
	state := fullState()
	first := rendered(t, state, true)

	previous := time.Local
	t.Cleanup(func() { time.Local = previous })
	time.Local = time.FixedZone("nowhere", 13*3600)

	if second := rendered(t, state, true); second != first {
		t.Fatalf("the same snapshot rendered differently after the process moved zones:\n%s\n%s",
			first, second)
	}

	// And the zone it DOES print in is the snapshot's, not the machine's.
	// Europe/Berlin was at +01:00 on the fixture's instant.
	if !strings.Contains(first, "GMT+01:00") {
		t.Fatalf("the alert's start is not in the snapshot's zone: %s", first)
	}
	if !strings.Contains(first, "the disk will be full in two hours") {
		t.Fatalf("the alert's description did not reach the card: %s", first)
	}
}

// TestMentionsDoNotShuffle. The set of people to notify was a map, and joining
// it walked that map: one snapshot produced a different string on almost every
// render, so two attempts of one delivery differed in bytes for no reason
// anybody could see from the outside.
func TestMentionsDoNotShuffle(t *testing.T) {
	state := fullState()

	first := collectMentions(state.Alerts)
	for i := 0; i < 50; i++ {
		if again := collectMentions(state.Alerts); again != first {
			t.Fatalf("mentions changed between renders: %q then %q", first, again)
		}
	}

	// Sorted by the id, with group mentions spelled differently from people.
	if want := "<!subteam^S0001> <@U0001> <@U0002>"; first != want {
		t.Fatalf("mentions are %q, want %q", first, want)
	}
}

// TestButtonsFollowTheAdmissionRatherThanTheConfiguration. Interactivity
// switched on after a group was admitted does not put buttons on cards that
// were admitted without them: the alternative is a card whose buttons appear
// and vanish between attempts, which is two different messages under one key.
func TestButtonsFollowTheAdmissionRatherThanTheConfiguration(t *testing.T) {
	state := fullState()
	state.TeamOnboarded = true
	state.Status = keys.GroupTriggered

	if block := findActionBlock(renderBodyAttachment(state, false)); block != nil {
		t.Fatal("a delivery admitted without buttons rendered them")
	}
	block := findActionBlock(renderBodyAttachment(state, true))
	if block == nil {
		t.Fatal("a delivery admitted with buttons rendered none")
	}
	if len(block.Elements.ElementSet) != 2 {
		t.Fatalf("expected Acknowledge and Resolve, got %d buttons",
			len(block.Elements.ElementSet))
	}

	// A team TokayOps does not have gets the notice instead, and the notice
	// links to where the snapshot says teams are set up.
	state.TeamOnboarded = false
	notice := findUnknownTeamNotice(renderBodyAttachment(state, true))
	if !strings.Contains(notice, "payments") ||
		!strings.Contains(notice, "https://tokay.example/#/cfg/teams") {
		t.Fatalf("the notice does not name the team or where to fix it: %q", notice)
	}
}
