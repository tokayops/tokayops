package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// What an integration declares about silence, and what the database does with
// it.
//
// The number is the operator's, it rides with each payload, and it is a
// snapshot on the group: the integration can be edited or deleted afterwards
// and what was true when the payload arrived stays. It says nothing about
// whether the payload is taken.

func intake(t *testing.T, s *Store, id, secret string, quietAfter int, enabled bool) {
	t.Helper()
	cfg, err := json.Marshal(model.WebhookConfig{Secret: secret, StaleAfterSeconds: quietAfter})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateIntegration(&model.Integration{
		ID: id, Type: model.IntegrationTypeAlertmanagerWebhook,
		Direction: model.IntegrationDirectionInbound,
		Name:      "Alertmanager " + id, Enabled: enabled, Config: cfg,
	}); err != nil {
		t.Fatalf("create the integration: %v", err)
	}
}

// declared reads what a group says about silence, for a message: a pointer
// printed as a pointer says nothing to whoever reads the failure.
func declared(seconds *int) string {
	if seconds == nil {
		return "nothing"
	}
	return fmt.Sprintf("%d seconds", *seconds)
}

func quietAfterOf(t *testing.T, s *Store, groupID string) *int {
	t.Helper()
	var seconds *int
	if err := s.db.QueryRow(
		`SELECT stale_after_seconds FROM alert_groups WHERE id = $1`, groupID).Scan(&seconds); err != nil {
		t.Fatalf("read stale_after_seconds: %v", err)
	}
	return seconds
}

// TestWhatAnIntegrationDeclaresRidesWithThePayload, including the way back:
// an operator who clears the field leaves the group saying nothing, which is
// the only way to turn the badge off again.
func TestWhatAnIntegrationDeclaresRidesWithThePayload(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	key := "quiet-" + uuid.New().String()
	id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)

	if at := quietAfterOf(t, s, id); at != nil {
		t.Fatalf("an incident opened by hand declares %v seconds of silence", *at)
	}

	apply := func(quietAfter int) {
		t.Helper()
		if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, alertgroup.Notification{
			Alerts:            []model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)},
			StaleAfterSeconds: quietAfter,
		}, "system"); err != nil {
			t.Fatalf("apply the payload: %v", err)
		}
	}

	apply(900)
	if at := quietAfterOf(t, s, id); at == nil || *at != 900 {
		t.Fatalf("the incident says %s, want 900 seconds", declared(at))
	}

	apply(1800)
	if at := quietAfterOf(t, s, id); at == nil || *at != 1800 {
		t.Fatalf("the incident says %s after the integration was edited, want 1800 seconds", declared(at))
	}

	apply(0)
	if at := quietAfterOf(t, s, id); at != nil {
		t.Errorf("the incident still says %v seconds after the field was cleared", *at)
	}
}

// TestAnIncidentOpenedByAPayloadKeepsWhatItDeclared. The payload that opens an
// incident carries the number like any other.
func TestAnIncidentOpenedByAPayloadKeepsWhatItDeclared(t *testing.T) {
	s := setupTestDB(t)
	seconds := 14700
	group := &model.AlertGroup{
		ID: uuid.New().String(), AlertKey: "opened-" + uuid.New().String(),
		Status: model.AlertGroupStatusNew, Title: "Disk filling up", Severity: "critical",
		TeamID: "team-1", TeamNameSnapshot: "team-1",
		Alerts:            []model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)},
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		StaleAfterSeconds: &seconds,
	}
	if err := s.CreateAlertGroupAtomic(group, nil, nil); err != nil {
		t.Fatalf("open the incident: %v", err)
	}

	if at := quietAfterOf(t, s, group.ID); at == nil || *at != seconds {
		t.Fatalf("the incident says %s, want %d seconds", declared(at), seconds)
	}
	read, err := s.GetAlertGroupByID(group.ID)
	if err != nil || read == nil {
		t.Fatalf("read the incident: %v", err)
	}
	if read.StaleAfterSeconds == nil || *read.StaleAfterSeconds != seconds {
		t.Errorf("the incident reads back %v, want %d", read.StaleAfterSeconds, seconds)
	}
}

// TestWhoMaySendIsSettledAgainstTheDatabase. The cache names the integration a
// secret belongs to; it cannot say whether that integration may still send,
// because only the instance that handled the change has reloaded it.
func TestWhoMaySendIsSettledAgainstTheDatabase(t *testing.T) {
	s := setupTestDB(t)

	intake(t, s, "am-live", "live-secret", 900, true)
	intake(t, s, "am-off", "off-secret", 900, false)
	intake(t, s, "am-silent", "silent-secret", 0, true)

	for _, c := range []struct {
		name          string
		id, secret    string
		wantAllowed   bool
		wantQuietSecs int
	}{
		{"an enabled integration with its own secret", "am-live", "live-secret", true, 900},
		{"the same integration with another secret", "am-live", "off-secret", false, 0},
		{"an integration that was disabled", "am-off", "off-secret", false, 0},
		{"an integration nobody has", "am-gone", "live-secret", false, 0},
		{"an enabled integration that declares nothing", "am-silent", "silent-secret", true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			seconds, allowed, err := s.VerifyIntake(context.Background(), c.id, c.secret)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if allowed != c.wantAllowed {
				t.Errorf("allowed = %v, want %v", allowed, c.wantAllowed)
			}
			if seconds != c.wantQuietSecs {
				t.Errorf("declared %d seconds, want %d", seconds, c.wantQuietSecs)
			}
		})
	}
}

// TestChangingTheIntegrationReachesTheGroupsItFeeds, including the group this
// whole feature is for: everything in it was silenced, so it will never send
// another payload, and a number that only travelled with payloads would leave
// its badge standing for good.
func TestChangingTheIntegrationReachesTheGroupsItFeeds(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	intake(t, s, "am-feeding", "feeding-secret", 900, true)

	key := "fed-" + uuid.New().String()
	id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, alertgroup.Notification{
		Alerts:            []model.Alert{notifiedAlert("fp-0", model.AlertStatusFiring)},
		IntegrationID:     "am-feeding",
		StaleAfterSeconds: 900,
	}, "system"); err != nil {
		t.Fatalf("apply the payload: %v", err)
	}
	if at := quietAfterOf(t, s, id); at == nil || *at != 900 {
		t.Fatalf("the incident says %s, want 900 seconds", declared(at))
	}

	// From here on the group is silent: Alertmanager sends nothing about it.
	edited := func(seconds int, enabled bool) {
		t.Helper()
		cfg, err := json.Marshal(model.WebhookConfig{Secret: "feeding-secret", StaleAfterSeconds: seconds})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpdateIntegration(context.Background(), "am-feeding",
			IntegrationPatch{Config: cfg, Enabled: &enabled}, "denis"); err != nil {
			t.Fatalf("edit the integration: %v", err)
		}
	}

	edited(1800, true)
	if at := quietAfterOf(t, s, id); at == nil || *at != 1800 {
		t.Errorf("the incident says %s after the integration was edited, want 1800 seconds", declared(at))
	}

	edited(0, true)
	if at := quietAfterOf(t, s, id); at != nil {
		t.Errorf("the incident still says %v after the field was cleared", *at)
	}

	edited(1800, false)
	if at := quietAfterOf(t, s, id); at != nil {
		t.Errorf("a disabled integration declares %v seconds about a group nobody is feeding", *at)
	}

	// Deleting it says nothing either, and the group survives.
	edited(1800, true)
	if _, err := s.DeleteIntegration(context.Background(), "am-feeding", "denis"); err != nil {
		t.Fatalf("delete the integration: %v", err)
	}
	if at := quietAfterOf(t, s, id); at != nil {
		t.Errorf("an integration that is gone declares %v seconds", *at)
	}
	if group, err := s.GetAlertGroupByID(id); err != nil || group == nil {
		t.Fatalf("the incident did not survive its integration: %v", err)
	}
}

// TestAClosedIncidentKeepsWhatWasDeclaredWhenItEnded. The number on a group
// that ended is part of what it looked like then, and the badge is only about
// groups that are open.
func TestAClosedIncidentKeepsWhatWasDeclaredWhenItEnded(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	intake(t, s, "am-past", "past-secret", 900, true)

	key := "past-" + uuid.New().String()
	id := notifiedFixture(t, s, key, model.AlertGroupStatusProcessing)
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, alertgroup.Notification{
		Alerts:            []model.Alert{notifiedAlert("fp-0", model.AlertStatusResolved)},
		IntegrationID:     "am-past",
		StaleAfterSeconds: 900,
	}, "system"); err != nil {
		t.Fatalf("resolve the incident: %v", err)
	}

	cfg, _ := json.Marshal(model.WebhookConfig{Secret: "past-secret"})
	if _, err := s.UpdateIntegration(context.Background(), "am-past",
		IntegrationPatch{Config: cfg}, "denis"); err != nil {
		t.Fatalf("clear the field: %v", err)
	}

	if at := quietAfterOf(t, s, id); at == nil || *at != 900 {
		t.Errorf("the incident that ended says %s, want the 900 seconds it ended with", declared(at))
	}
}
