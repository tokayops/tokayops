package providers

import (
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Freezing a live row, and the difference between the two doors.
//
// The producer that ADMITS a delivery has to be refused when the state it
// offers cannot be identified - the digest is what says which message went out.
// The path that renders a card from a row that already exists cannot be
// refused: the message is owed to somebody, and a fixture with no identity is a
// data problem, not a reason to send nothing.

func liveGroup() *model.AlertGroup {
	return &model.AlertGroup{
		ID: "ag-1", Title: "Disk filling up", Severity: "critical",
		Status: model.AlertGroupStatusTriggered, TeamID: "payments",
		ExternalURL: "https://alertmanager.example", CreatedAt: time.Unix(1700000000, 0).UTC(),
		Alerts: []model.Alert{{
			Fingerprint: "fp-1", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000000, 0).UTC(),
			Labels: map[string]string{
				"alertname": "DiskWillFill", "severity": "critical", "slack_user": "U0001",
				"dashboard": "https://grafana.example/from-label",
			},
			Annotations: map[string]string{
				"runbook": "https://runbooks.example", "summary": "disk nearly full",
			},
		}},
		TimelineEvents: []*model.TimelineEvent{{
			ID: "e1", Type: model.TimelineEventCreated, Message: "Alert group created",
			CreatedAt: time.Unix(1700000000, 0).UTC(),
		}},
	}
}

// TestTheFallbacksAreResolvedOnce. A dashboard link can arrive as an annotation
// or as a label, and a description can be either of two annotations. The
// renderers used to know that; now the freeze does, once, and what reaches a
// card is a field.
func TestTheFallbacksAreResolvedOnce(t *testing.T) {
	state := ViewOf(GroupView{
		Group: liveGroup(), SelfURL: "https://tokay.example",
		TeamOnboarded: true, Zone: "Europe/Berlin",
	})

	alert := state.Alerts[0]
	if alert.DashboardURL == nil || *alert.DashboardURL != "https://grafana.example/from-label" {
		t.Fatalf("the dashboard link did not fall back to the label: %v", alert.DashboardURL)
	}
	if alert.Description == nil || *alert.Description != "disk nearly full" {
		t.Fatalf("the description did not fall back to the summary: %v", alert.Description)
	}
	if alert.SlackUser == nil || *alert.SlackUser != "U0001" {
		t.Fatalf("the mention was lost: %v", alert.SlackUser)
	}

	// The links are frozen whole rather than as a base to append to later.
	if state.GroupURL == nil || *state.GroupURL != "https://tokay.example/#/ops/alert-groups/ag-1" {
		t.Fatalf("the group link is %v", state.GroupURL)
	}
	if state.TeamSetupURL == nil || *state.TeamSetupURL != "https://tokay.example/#/cfg/teams" {
		t.Fatalf("the team setup link is %v", state.TeamSetupURL)
	}
}

// TestAdmissionRefusesStateNobodyCanTellApart.
//
// There used to be a second, tolerant freeze beside this one, for a path that
// drew a card from a live row and had to draw SOMETHING - it invented a
// fingerprint rather than refuse. That path is gone with the job engine, and
// with it the question of what a card made from unidentifiable alerts would
// mean under a digest saying it was what got sent.
//
// One door now, and it refuses.
func TestAdmissionRefusesStateNobodyCanTellApart(t *testing.T) {
	group := liveGroup()
	group.Alerts[0].Fingerprint = ""
	group.TimelineEvents[0].ID = ""

	if _, err := SnapshotOf(GroupView{Group: group, Zone: "UTC"}); err == nil {
		t.Fatal("a producer was allowed to admit state whose alerts cannot be told apart")
	}
}

// TestTheProcessZoneNeverReachesASnapshot. "Local" is not a zone, it is a
// question about the machine asking - and a snapshot that carried it would
// render differently on every instance.
func TestTheProcessZoneNeverReachesASnapshot(t *testing.T) {
	for _, zone := range []string{"", "Local", "Mars/Olympus"} {
		state := ViewOf(GroupView{Group: liveGroup(), Zone: zone})
		if state.DisplayTimezone != "UTC" {
			t.Errorf("zone %q was frozen as %q", zone, state.DisplayTimezone)
		}
	}

	state := ViewOf(GroupView{Group: liveGroup(), Zone: "Asia/Tokyo"})
	if state.DisplayTimezone != "Asia/Tokyo" {
		t.Errorf("a real zone was replaced by %q", state.DisplayTimezone)
	}
	if _, err := keys.NewRenderSnapshot(state); err != nil {
		t.Fatalf("the frozen state is not admissible: %v", err)
	}
}

// TestAVocabularyThisBuildDoesNotShare. A status nobody declared is refused at
// admission, because a substitution would be recorded as what was accepted - a
// message about a state the alert was never in, under a digest saying
// otherwise. What the freeze does NOT do is repair it on the way past, which is
// what makes admission able to see it at all.
func TestAVocabularyThisBuildDoesNotShare(t *testing.T) {
	group := liveGroup()
	group.Status = model.AlertGroupStatus("quarantined")
	group.Alerts[0].Status = model.AlertStatus("flapping")
	group.TimelineEvents[0].Type = model.TimelineEventType("annotated")
	view := GroupView{Group: group, Zone: "UTC"}

	if _, err := SnapshotOf(view); err == nil {
		t.Fatal("a producer admitted a state this build cannot name")
	}

	// Faithful: what it could not map is carried across rather than replaced,
	// which is what makes admission able to see it.
	faithful := ViewOf(view)
	if faithful.Status != keys.GroupStatus("quarantined") {
		t.Fatalf("the unknown status became %q before anybody could refuse it", faithful.Status)
	}

}
