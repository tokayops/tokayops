package store

import (
	"context"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
)

// TestAStartUpgradesTheDatabaseOfV020 is the upgrade every installation on
// v0.2.0 makes: the schema exactly as v0.2.0's own start built it
// (testdata/schema-v0.2.0.sql, dumped from it), holding an incident that is
// still open and a finished one for the same alert. One start of this version
// adds when Alertmanager last sent about a group, and fills in nothing: the
// time the group last changed is a different instant, and neither group has
// heard from Alertmanager under this version yet. The next payload records it
// for the open incident and leaves the finished one alone.
func TestAStartUpgradesTheDatabaseOfV020(t *testing.T) {
	s := throwawayDatabase(t, "schema-v0.2.0.sql")
	if !relationExists(t, s, "outbound_intents") || hasColumn(t, s, "alert_groups", "last_notified_at") {
		t.Fatal("the schema file is not v0.2.0's")
	}

	const open, finished, key = "ag-open", "ag-finished", "group-key"
	alerts := `[{"fingerprint":"fp-0","status":"firing","labels":{"alertname":"DiskWillFill"},` +
		`"annotations":null,"startsAt":"2023-11-14T22:13:20Z","endsAt":"0001-01-01T00:00:00Z","generatorURL":""}]`
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO teams (id, name, created_at) VALUES ('team-1', 'Team One', now())`, nil},
		{`INSERT INTO alert_groups (id, alert_key, status, title, team_id, team_name_snapshot, severity, alerts_data, created_at, updated_at, resolved_at)
			VALUES ($1, $2, 'resolved', 'Disk filling up', 'team-1', 'Team One', 'critical', $3, now() - interval '2 days', now() - interval '2 days', now() - interval '2 days')`,
			[]any{finished, key, alerts}},
		{`INSERT INTO alert_groups (id, alert_key, status, title, team_id, team_name_snapshot, severity, alerts_data, created_at, updated_at)
			VALUES ($1, $2, 'processing', 'Disk filling up', 'team-1', 'Team One', 'critical', $3, now() - interval '1 hour', now() - interval '1 hour')`,
			[]any{open, key, alerts}},
	} {
		if _, err := s.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatalf("write v0.2.0's rows: %v\n%s", err, statement.sql)
		}
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused v0.2.0's database: %v", err)
	}

	if !hasColumn(t, s, "alert_groups", "last_notified_at") {
		t.Fatal("the start did not add last_notified_at")
	}
	for _, id := range []string{open, finished} {
		if at := lastNotifiedAt(t, s, id); at != nil {
			t.Errorf("the start filled in last_notified_at of %s with %v", id, at)
		}
	}

	s.SetRenderEnvironment("https://tokay.example", "UTC")
	result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(), key, alertgroup.Notification{Alerts: []model.Alert{{
		Fingerprint: "fp-0", Status: model.AlertStatusFiring, StartsAt: time.Unix(1700000000, 0),
		Labels: map[string]string{"alertname": "DiskWillFill"},
	}}}, "system")
	if err != nil || result.Outcome != alertgroup.MergeUnchanged || result.AlertGroupID != open {
		t.Fatalf("the repeat came back %s for %s (%v), want unchanged for %s",
			result.Outcome, result.AlertGroupID, err, open)
	}
	group, err := s.GetAlertGroupByID(open)
	if err != nil || group == nil {
		t.Fatalf("read the open incident: %v", err)
	}
	if group.LastNotifiedAt == nil {
		t.Error("the open incident does not say Alertmanager sent the repeat")
	}
	if at := lastNotifiedAt(t, s, finished); at != nil {
		t.Errorf("the finished incident was recorded at %v", at)
	}
}
