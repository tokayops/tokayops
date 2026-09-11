//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
)

// TestRevisionZeroCarriesTheCreationLine. The first thread under a card shows
// the alert's history from its first line: the producer freezes the history
// as it stands when the plan is built, and the ingester has written the
// creation line by then. If the order were the other way, the first thread
// would say "... and 0 events" - and the fix would be in the engine, not in
// the codec.
func TestRevisionZeroCarriesTheCreationLine(t *testing.T) {
	env := setupIntegrationTest(t)

	sendWebhook(t, env.Echo, criticalAlert("created_line", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	var raw []byte
	if err := env.S.GetDB().QueryRow(`
		SELECT b.admission_snapshot->'timeline'
		FROM outbound_batches b
		JOIN alert_groups ag ON ag.id = b.alert_group_id
		WHERE ag.alert_key = $1 AND b.key_kind = 'escalation'`, "created_line").Scan(&raw); err != nil {
		t.Fatalf("read the admitted state: %v", err)
	}
	var lines []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &lines); err != nil {
		t.Fatalf("read the history of revision 0: %v", err)
	}
	for _, line := range lines {
		if line.Type == "created" {
			return
		}
	}
	t.Fatalf("revision 0 was frozen without the creation line: %v", lines)
}
