package slack

import (
	"encoding/json"
	"fmt"
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

	// The card names the alerts; what is wrong and since when is the thread's
	// to say, and the zone it says it in is the snapshot's, not the machine's.
	// Europe/Berlin was at +01:00 on the fixture's instant.
	for _, detail := range []string{"the disk will be full in two hours", "since "} {
		if strings.Contains(first, detail) {
			t.Fatalf("the card says %q, which is the thread's to say: %s", detail, first)
		}
	}
	thread := RenderThread(state)
	if !strings.Contains(thread, "the disk will be full in two hours · since 2023-11-14 23:13 GMT+01:00") {
		t.Fatalf("the thread does not say what is wrong and since when, in the snapshot's zone: %s", thread)
	}
}

// TestTheCardListsFiringAlertsFirst. Of many alerts the card lists ten, and
// after a partial recovery the ten that started first are the resolved ones:
// the firing alerts, the ones a person is paged about, would all be behind
// "and N more". Firing first, then resolved, each by when they started.
func TestTheCardListsFiringAlertsFirst(t *testing.T) {
	var alerts []keys.AlertSnapshot
	for i := 0; i < 12; i++ {
		status := keys.AlertFiring
		if i < 3 {
			status = keys.AlertResolved // the three that started first have resolved
		}
		alerts = append(alerts, keys.AlertSnapshot{
			Fingerprint: fmt.Sprintf("fp-%d", i), Status: status,
			StartsAt: time.Unix(1700000000+int64(i)*60, 0).UTC(), AlertName: fmt.Sprintf("Alert%d", i),
			Severity: "critical",
		})
	}
	list := buildAlertList(alerts, "UTC")
	lines := strings.Split(strings.TrimSpace(list), "\n")
	if len(lines) != 11 {
		t.Fatalf("the list has %d lines, want ten alerts and the count of the rest:\n%s", len(lines), list)
	}
	for i, line := range lines[:9] {
		if !strings.HasPrefix(line, fmt.Sprintf("• 🔴 Alert%d ", i+3)) {
			t.Fatalf("line %d reads %q, want the firing alerts first, by when they started", i, line)
		}
	}
	if lines[9] != "• 🟢 Alert0 (Resolved)" {
		t.Fatalf("the tenth line reads %q, want the first of the resolved", lines[9])
	}
	if lines[10] != "_... and 2 more alerts_" {
		t.Fatalf("the count reads %q", lines[10])
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
