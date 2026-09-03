package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The two doors into "this subscriber is gone", what each withdraws, what each
// leaves, and the races each has with the fan-out and with an edit.

type owed struct {
	ID      string
	Status  string
	Flagged bool
}

// commitmentsOwedTo is every webhook commitment addressed to one subscriber, in
// the order they were made.
func commitmentsOwedTo(t *testing.T, s *Store, integrationID string) []owed {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT id, status, cancellation_requested FROM outbound_intents
		WHERE delivery_family = 'webhook' AND target_ref = $1
		ORDER BY created_at, id`, integrationID)
	if err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	defer rows.Close()
	var got []owed
	for rows.Next() {
		var o owed
		if err := rows.Scan(&o.ID, &o.Status, &o.Flagged); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, o)
	}
	return got
}

// journalOf is a commitment's own history as "kind|reason|actor" lines.
func journalOf(t *testing.T, s *Store, intentID string) []string {
	t.Helper()
	rows, err := s.db.Query(`
		SELECT kind, COALESCE(reason, ''), COALESCE(actor, '') FROM outbound_intent_events
		WHERE intent_id = $1 ORDER BY seq`, intentID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var kind, reason, actor string
		if err := rows.Scan(&kind, &reason, &actor); err != nil {
			t.Fatalf("scan: %v", err)
		}
		lines = append(lines, kind+"|"+reason+"|"+actor)
	}
	return lines
}

// fannedOut writes one alert event and fans it out, and fails unless the
// fan-out found it.
func fannedOut(t *testing.T, s *Store) string {
	t.Helper()
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	result, err := s.FanOutNextEvent(context.Background())
	if err != nil || !result.Found || result.EventID != eventID {
		t.Fatalf("fan out %s: %+v (%v)", eventID, result, err)
	}
	return eventID
}

// inFlightWebhook takes one webhook commitment through the real claim and the
// real beginning of an attempt, so it is in the state a worker leaves it in
// while the request is on the wire.
func inFlightWebhook(t *testing.T, s *Store, intentID string) {
	t.Helper()
	ctx := context.Background()
	leased, err := s.ClaimDueIntents(ctx, outbound.ClaimRequest{
		Family: outbound.FamilyWebhook, Provider: keys.ProviderWebhook,
		Phase: outbound.ClaimRetriesFirst, Limit: 10, Lease: outbound.WebhookLease, WorkerID: "worker-1",
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, l := range leased {
		if l.Intent.ID != intentID {
			continue
		}
		if _, err := s.BeginAttempt(ctx, outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: l.LeaseToken, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "https://example.com/hooks",
		}); err != nil {
			t.Fatalf("begin the attempt: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusSending {
			t.Fatalf("after the beginning the commitment is %q", got)
		}
		return
	}
	t.Fatalf("the claim did not include %s", intentID)
}

// waitingOnAPerson puts a commitment where an operator's decision would leave
// it. The webhook family reaches manual_review only through a decision this
// build has no operator for, so the state is constructed rather than reached.
func waitingOnAPerson(t *testing.T, s *Store, intentID string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE outbound_intents SET status = 'manual_review',
		lease_token = NULL, locked_until = NULL, worker_id = NULL WHERE id = $1`, intentID); err != nil {
		t.Fatalf("construct manual_review: %v", err)
	}
}

func onlyOne(t *testing.T, owed []owed) owed {
	t.Helper()
	if len(owed) != 1 {
		t.Fatalf("expected one commitment, found %d: %+v", len(owed), owed)
	}
	return owed[0]
}

func noLiveCommitment(t *testing.T, s *Store, integrationID string) {
	t.Helper()
	for _, o := range commitmentsOwedTo(t, s, integrationID) {
		switch o.Status {
		case "pending", "manual_review":
			t.Fatalf("commitment %s is still live as %q", o.ID, o.Status)
		case "sending":
			if !o.Flagged {
				t.Fatalf("commitment %s is on the wire and nothing asked it to stop", o.ID)
			}
		}
	}
}

func teams(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.CreateTeam(&model.Team{ID: id, Name: id, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("create team %s: %v", id, err)
		}
	}
}

// TestDeletingASubscriberWithdrawsWhatItIsOwedAndLeavesTheHistory: pending and
// waiting commitments are withdrawn outright and counted; the one on the wire is
// flagged and not counted; the other subscriber's are untouched; the row is
// gone, the tombstone stands, and the journal is still there to read.
func TestDeletingASubscriberWithdrawsWhatItIsOwedAndLeavesTheHistory(t *testing.T) {
	s := setupTestDB(t)
	teams(t, s, "team-1")
	gone := subscriber(t, s, "gone", model.WebhookScopeTeam, "team-1", true)
	stays := subscriber(t, s, "stays", model.WebhookScopeGlobal, "", true)

	fannedOut(t, s)
	fannedOut(t, s)
	fannedOut(t, s)
	before := commitmentsOwedTo(t, s, gone)
	if len(before) != 3 {
		t.Fatalf("the subscriber is owed %d commitments, want 3", len(before))
	}
	inFlightWebhook(t, s, before[0].ID)
	waitingOnAPerson(t, s, before[2].ID)
	canceledBefore := terminalCount(t, "webhook", "canceled")

	change, err := s.DeleteIntegration(context.Background(), gone, "nina")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if change.Withdrawn != 2 || change.After != nil || change.Before == nil || change.Before.ID != gone {
		t.Fatalf("the command reported %+v", change)
	}
	if got := terminalCount(t, "webhook", "canceled") - canceledBefore; got != 2 {
		t.Fatalf("the terminal counter rose by %v, want 2: the flagged one is the attempt's to count", got)
	}

	after := commitmentsOwedTo(t, s, gone)
	if after[0].Status != "sending" || !after[0].Flagged {
		t.Fatalf("the one on the wire is %+v, want sending and flagged", after[0])
	}
	if after[1].Status != "canceled" || after[2].Status != "canceled" {
		t.Fatalf("pending and waiting became %+v / %+v, want canceled", after[1], after[2])
	}
	noLiveCommitment(t, s, gone)
	if lines := journalOf(t, s, after[0].ID); lines[len(lines)-1] != "cancellation_requested|the subscriber was deleted|nina" {
		t.Fatalf("the flagged one's journal ends with %q", lines[len(lines)-1])
	}
	if lines := journalOf(t, s, after[1].ID); lines[len(lines)-1] != "canceled|the subscriber was deleted|nina" {
		t.Fatalf("the pending one's journal ends with %q", lines[len(lines)-1])
	}
	if lines := journalOf(t, s, after[2].ID); !strings.HasPrefix(lines[len(lines)-1],
		"canceled|the subscriber was deleted; the outcome of the previous attempt stays unknown|") {
		t.Fatalf("the waiting one's journal ends with %q", lines[len(lines)-1])
	}
	for _, o := range commitmentsOwedTo(t, s, stays) {
		if o.Status != "pending" || o.Flagged {
			t.Fatalf("the other subscriber's commitment was touched: %+v", o)
		}
	}

	if _, err := s.GetIntegrationByID(gone); !errors.Is(err, ErrIntegrationNotFound) {
		t.Fatalf("the row is still there: %v", err)
	}
	tombstone, found, err := s.IntegrationTombstone(context.Background(), gone)
	if err != nil || !found {
		t.Fatalf("no tombstone: found=%v err=%v", found, err)
	}
	if tombstone.Type != model.IntegrationTypeGenericWebhook || tombstone.Scope == nil ||
		*tombstone.Scope != model.WebhookScopeTeam || tombstone.TeamID == nil || *tombstone.TeamID != "team-1" ||
		tombstone.DeletedAt.IsZero() {
		t.Fatalf("the tombstone says %+v", tombstone)
	}
	if _, found, _ := s.IntegrationTombstone(context.Background(), stays); found {
		t.Fatal("a living integration has a tombstone")
	}
}

// TestALegacyDeliveryRowNoLongerBlocksDeletion: a database upgraded from the
// previous release still holds the old delivery table with its foreign key to
// integrations, and a deletion used to fail on it. The start removes the key;
// the history row stays for the manual migration to remove.
func TestALegacyDeliveryRowNoLongerBlocksDeletion(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "old", model.WebhookScopeGlobal, "", true)
	eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
	sprint3OutboundShape(t, s)
	if _, err := s.db.Exec(`INSERT INTO event_outbox_deliveries (id, event_id, integration_id, status, attempts)
		VALUES ('legacy-1', $1, $2, 'sent', 1)`, eventID, id); err != nil {
		t.Fatalf("write the legacy delivery: %v", err)
	}

	// The premise, so the test cannot pass by accident: under the previous
	// shape the row is what stops the deletion.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`DELETE FROM integrations WHERE id = $1`, id)
	tx.Rollback()
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || pqErr.Code.Name() != "foreign_key_violation" {
		t.Fatalf("the previous shape let the deletion through (%v): the test proves nothing", err)
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("start against the previous shape: %v", err)
	}
	if _, err := s.DeleteIntegration(context.Background(), id, "nina"); err != nil {
		t.Fatalf("delete with legacy history: %v", err)
	}
	var kept int
	if err := s.db.QueryRow(`SELECT count(*) FROM event_outbox_deliveries WHERE integration_id = $1`, id).
		Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("the legacy history row: %d left (%v), want 1", kept, err)
	}
}

// holdingTheAudience is a fan-out frozen at the moment it has read the
// subscriber: the shared lock is taken and the commitment written, and the
// transaction is not yet committed. Callers commit or roll it back.
func holdingTheAudience(t *testing.T, s *Store, subscriberID, eventID string) *sqlTxHold {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT id FROM integrations WHERE id = $1 FOR SHARE`, subscriberID); err != nil {
		t.Fatalf("take the audience lock: %v", err)
	}
	if eventID != "" {
		if _, err := admitWebhookTx(ctx, tx, webhookBatch(keys.KindWebhookEvent, eventID, subscriberID), outbound.ActorFanOut); err != nil {
			t.Fatalf("admit under the lock: %v", err)
		}
	}
	return &sqlTxHold{tx: tx}
}

type sqlTxHold struct {
	tx interface {
		Commit() error
		Rollback() error
	}
}

// TestDeletionAndFanOutLeaveNoLiveCommitmentInEitherOrder: the fan-out's
// shared lock against the deletion's exclusive one. Fan-out first: the deletion
// waits, is refused when the wait is longer than a command waits, and its
// repeat withdraws the commitment the fan-out made. Deletion first: the fan-out
// finds nobody. Either way the deleted subscriber is owed nothing live.
func TestDeletionAndFanOutLeaveNoLiveCommitmentInEitherOrder(t *testing.T) {
	t.Run("the fan-out wins", func(t *testing.T) {
		s := setupTestDB(t)
		s.lockTimeout = 300 * time.Millisecond
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
		hold := holdingTheAudience(t, s, id, eventID)
		canceledBefore := terminalCount(t, "webhook", "canceled")

		started := time.Now()
		_, err := s.DeleteIntegration(context.Background(), id, "nina")
		if !errors.Is(err, ErrIntegrationBusy) {
			hold.tx.Rollback()
			t.Fatalf("a deletion against a held audience answered %v, want busy", err)
		}
		if waited := time.Since(started); waited > 2*time.Second {
			t.Fatalf("the refusal took %s: the command did not stop at its lock timeout", waited)
		}
		if got := terminalCount(t, "webhook", "canceled"); got != canceledBefore {
			t.Fatalf("a refused command counted %v endings", got-canceledBefore)
		}
		if _, err := s.GetIntegrationByID(id); err != nil {
			t.Fatalf("the refused deletion changed the row: %v", err)
		}

		if err := hold.tx.Commit(); err != nil {
			t.Fatalf("commit the fan-out: %v", err)
		}
		change, err := s.DeleteIntegration(context.Background(), id, "nina")
		if err != nil || change.Withdrawn != 1 {
			t.Fatalf("the repeated deletion: %+v (%v), want one withdrawn", change, err)
		}
		if got := onlyOne(t, commitmentsOwedTo(t, s, id)); got.Status != "canceled" {
			t.Fatalf("the fan-out's commitment is %+v", got)
		}
		noLiveCommitment(t, s, id)
	})

	t.Run("the deletion wins", func(t *testing.T) {
		s := setupTestDB(t)
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
		if _, err := s.DeleteIntegration(context.Background(), id, "nina"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		result, err := s.FanOutNextEvent(context.Background())
		if err != nil || !result.Found || result.EventID != eventID || result.Commitments != 0 {
			t.Fatalf("the fan-out after the deletion: %+v (%v)", result, err)
		}
		if owed := commitmentsOwedTo(t, s, id); len(owed) != 0 {
			t.Fatalf("a deleted subscriber was promised %+v", owed)
		}
		if status := eventStatus(t, s, eventID); status != "fanned_out" {
			t.Fatalf("the event is %q", status)
		}
	})
}

// TestDisablingASubscriberWithdrawsAndSwitchingItBackDoesNotResurrect: the
// switch is the second door into the same withdrawal. A real true -> false
// withdraws and counts; false -> false and false -> true do nothing to what was
// withdrawn.
func TestDisablingASubscriberWithdrawsAndSwitchingItBackDoesNotResurrect(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	fannedOut(t, s)
	owed := commitmentsOwedTo(t, s, id)
	inFlightWebhook(t, s, owed[0].ID)
	canceledBefore := terminalCount(t, "webhook", "canceled")
	off, on := false, true

	change, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina")
	if err != nil || change.Withdrawn != 1 || change.After == nil || change.After.Enabled || !change.Before.Enabled {
		t.Fatalf("the switch answered %+v (%v)", change, err)
	}
	if got := terminalCount(t, "webhook", "canceled") - canceledBefore; got != 1 {
		t.Fatalf("the terminal counter rose by %v, want 1", got)
	}
	after := commitmentsOwedTo(t, s, id)
	if after[0].Status != "sending" || !after[0].Flagged || after[1].Status != "canceled" {
		t.Fatalf("after the switch: %+v", after)
	}
	if lines := journalOf(t, s, after[1].ID); lines[len(lines)-1] != "canceled|the subscriber was disabled|nina" {
		t.Fatalf("the journal ends with %q", lines[len(lines)-1])
	}

	// Off again: not a second switching-off - nothing is withdrawn, and the one
	// on the wire is not asked to stop a second time.
	change, err = s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina")
	if err != nil || change.Withdrawn != 0 {
		t.Fatalf("repeating off: %+v (%v)", change, err)
	}
	if lines := journalOf(t, s, after[0].ID); strings.Count(strings.Join(lines, "\n"), "cancellation_requested|") != 1 {
		t.Fatalf("repeating off asked the in-flight one to stop again: %q", lines)
	}
	// Back on: nothing comes back.
	change, err = s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &on}, "nina")
	if err != nil || change.Withdrawn != 0 || !change.After.Enabled {
		t.Fatalf("switching back on: %+v (%v)", change, err)
	}
	if again := commitmentsOwedTo(t, s, id); again[1].Status != "canceled" || !again[0].Flagged {
		t.Fatalf("switching back on resurrected: %+v", again)
	}
	if got := terminalCount(t, "webhook", "canceled") - canceledBefore; got != 1 {
		t.Fatalf("the terminal counter rose by %v over the whole sequence, want 1", got)
	}
}

// TestEditingASubscriberWithdrawsNothing: the address, the secret, the headers,
// the timeout and the name are the subscriber's details, not its existence.
// Scope and team are not editable at all, and the command leaves them as they
// are.
func TestEditingASubscriberWithdrawsNothing(t *testing.T) {
	s := setupTestDB(t)
	teams(t, s, "team-1")
	id := subscriber(t, s, "hooks", model.WebhookScopeTeam, "team-1", true)
	fannedOut(t, s)
	commitment := onlyOne(t, commitmentsOwedTo(t, s, id))
	canceledBefore := terminalCount(t, "webhook", "canceled")

	config := func(cfg model.GenericWebhookConfig) []byte {
		raw, _ := json.Marshal(cfg)
		return raw
	}
	renamed := "hooks-renamed"
	for _, edit := range []struct {
		field string
		patch IntegrationPatch
	}{
		{"url", IntegrationPatch{Config: config(model.GenericWebhookConfig{URL: "https://example.com/elsewhere"})}},
		{"secret", IntegrationPatch{Config: config(model.GenericWebhookConfig{URL: "https://example.com/elsewhere", Secret: "rotated"})}},
		{"custom_headers", IntegrationPatch{Config: config(model.GenericWebhookConfig{URL: "https://example.com/elsewhere",
			CustomHeaders: map[string]string{"X-Team": "sre"}})}},
		{"timeout_seconds", IntegrationPatch{Config: config(model.GenericWebhookConfig{URL: "https://example.com/elsewhere", TimeoutSeconds: 5})}},
		{"name", IntegrationPatch{Name: &renamed}},
	} {
		change, err := s.UpdateIntegration(context.Background(), id, edit.patch, "nina")
		if err != nil || change.Withdrawn != 0 {
			t.Fatalf("editing %s: %+v (%v)", edit.field, change, err)
		}
		if got := onlyOne(t, commitmentsOwedTo(t, s, id)); got != commitment {
			t.Fatalf("editing %s touched the commitment: %+v", edit.field, got)
		}
	}
	if got := terminalCount(t, "webhook", "canceled"); got != canceledBefore {
		t.Fatalf("edits counted %v endings", got-canceledBefore)
	}
	row, err := s.GetIntegrationByID(id)
	if err != nil {
		t.Fatal(err)
	}
	var cfg model.GenericWebhookConfig
	_ = json.Unmarshal(row.Config, &cfg)
	if row.Name != renamed || cfg.URL != "https://example.com/elsewhere" || cfg.Secret != "rotated" ||
		cfg.TimeoutSeconds != 5 || cfg.CustomHeaders["X-Team"] != "sre" {
		t.Fatalf("the edits did not all land: %+v %+v", row, cfg)
	}
	if row.Scope == nil || *row.Scope != model.WebhookScopeTeam || row.TeamID == nil || *row.TeamID != "team-1" {
		t.Fatalf("scope or team moved: %v %v", row.Scope, row.TeamID)
	}
}

// TestDisablingAgainstAnEditLosesNeither: two edits of one row, in both orders.
// The switch re-reads the row under the lock and applies itself to what it
// finds, so an edit committed a moment earlier survives it; the edit after the
// switch does not switch it back on.
func TestDisablingAgainstAnEditLosesNeither(t *testing.T) {
	newURL := func(t *testing.T, s *Store, id string) string {
		t.Helper()
		row, err := s.GetIntegrationByID(id)
		if err != nil {
			t.Fatal(err)
		}
		var cfg model.GenericWebhookConfig
		_ = json.Unmarshal(row.Config, &cfg)
		return cfg.URL
	}
	edited, _ := json.Marshal(model.GenericWebhookConfig{URL: "https://example.com/moved", Secret: "s"})
	off := false

	t.Run("the edit is committed while the switch waits", func(t *testing.T) {
		s := setupTestDB(t)
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		fannedOut(t, s)

		// An edit in progress: the row is locked and the configuration written,
		// and the transaction is not yet committed.
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT id FROM integrations WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatal(err)
		}
		encrypted, err := encryptConfig(edited)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE integrations SET config = $1 WHERE id = $2`, encrypted, id); err != nil {
			t.Fatal(err)
		}

		type answer struct {
			change IntegrationChange
			err    error
		}
		answered := make(chan answer, 1)
		go func() {
			change, err := s.UpdateIntegration(ctx, id, IntegrationPatch{Enabled: &off}, "nina")
			answered <- answer{change, err}
		}()
		select {
		case a := <-answered:
			t.Fatalf("the switch did not wait for the edit: %+v (%v)", a.change, a.err)
		case <-time.After(200 * time.Millisecond):
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		a := <-answered
		if a.err != nil || a.change.Withdrawn != 1 {
			t.Fatalf("the switch answered %+v (%v)", a.change, a.err)
		}
		var seen model.GenericWebhookConfig
		_ = json.Unmarshal(a.change.Before.Config, &seen)
		if seen.URL != "https://example.com/moved" {
			t.Fatalf("the switch read the row before the edit: %s", seen.URL)
		}
		row, _ := s.GetIntegrationByID(id)
		if row.Enabled || newURL(t, s, id) != "https://example.com/moved" {
			t.Fatalf("one of the two was lost: enabled=%v url=%s", row.Enabled, newURL(t, s, id))
		}
		noLiveCommitment(t, s, id)
	})

	t.Run("the switch is committed before the edit", func(t *testing.T) {
		s := setupTestDB(t)
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		fannedOut(t, s)

		first, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina")
		if err != nil || first.Withdrawn != 1 {
			t.Fatalf("the switch: %+v (%v)", first, err)
		}
		second, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Config: edited}, "nina")
		if err != nil || second.Withdrawn != 0 || second.After.Enabled {
			t.Fatalf("the edit: %+v (%v)", second, err)
		}
		row, _ := s.GetIntegrationByID(id)
		if row.Enabled || newURL(t, s, id) != "https://example.com/moved" {
			t.Fatalf("one of the two was lost: enabled=%v url=%s", row.Enabled, newURL(t, s, id))
		}
	})
}

// TestACommandDoesNotWaitLongerThanTheLockTimeout: a fan-out that holds the
// audience longer than a command waits gets the command refused, not queued
// behind it; the refusal changes nothing and counts nothing.
func TestACommandDoesNotWaitLongerThanTheLockTimeout(t *testing.T) {
	s := setupTestDB(t)
	s.lockTimeout = 300 * time.Millisecond
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	fannedOut(t, s)
	hold := holdingTheAudience(t, s, id, "")
	defer hold.tx.Rollback()
	canceledBefore := terminalCount(t, "webhook", "canceled")
	off := false

	started := time.Now()
	_, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina")
	if !errors.Is(err, ErrIntegrationBusy) {
		t.Fatalf("the switch against a held audience answered %v, want busy", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("the refusal took %s", waited)
	}
	if got := terminalCount(t, "webhook", "canceled"); got != canceledBefore {
		t.Fatalf("a refused command counted %v endings", got-canceledBefore)
	}
	row, err := s.GetIntegrationByID(id)
	if err != nil || !row.Enabled {
		t.Fatalf("the refused switch changed the row: %+v (%v)", row, err)
	}
	if got := onlyOne(t, commitmentsOwedTo(t, s, id)); got.Status != "pending" {
		t.Fatalf("the refused switch withdrew: %+v", got)
	}
}

// TestDisablingAndFanOutLeaveNoLiveCommitmentInEitherOrder: the same two
// orders as for deletion, through the second door.
func TestDisablingAndFanOutLeaveNoLiveCommitmentInEitherOrder(t *testing.T) {
	off := false
	t.Run("the fan-out wins", func(t *testing.T) {
		s := setupTestDB(t)
		s.lockTimeout = 300 * time.Millisecond
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
		hold := holdingTheAudience(t, s, id, eventID)

		if _, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina"); !errors.Is(err, ErrIntegrationBusy) {
			hold.tx.Rollback()
			t.Fatalf("the switch against a held audience answered %v, want busy", err)
		}
		if err := hold.tx.Commit(); err != nil {
			t.Fatal(err)
		}
		change, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina")
		if err != nil || change.Withdrawn != 1 {
			t.Fatalf("the repeated switch: %+v (%v)", change, err)
		}
		noLiveCommitment(t, s, id)
	})
	t.Run("the switch wins", func(t *testing.T) {
		s := setupTestDB(t)
		id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
		eventID := alertEvent(t, s, "team-1", model.OutboxEventFiring)
		if _, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Enabled: &off}, "nina"); err != nil {
			t.Fatal(err)
		}
		result, err := s.FanOutNextEvent(context.Background())
		if err != nil || !result.Found || result.EventID != eventID || result.Commitments != 0 {
			t.Fatalf("the fan-out after the switch: %+v (%v)", result, err)
		}
		if owed := commitmentsOwedTo(t, s, id); len(owed) != 0 {
			t.Fatalf("a switched-off subscriber was promised %+v", owed)
		}
	})
}

// TestEffectsRunOneAtATimeUnderTheIntegrationLock: the second effect does not
// start until the first has returned, and each is handed the row as it stands
// at that moment - nil once the integration is gone.
func TestEffectsRunOneAtATimeUnderTheIntegrationLock(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)
	ctx := context.Background()

	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.WithIntegrationLocked(ctx, id, func(*model.Integration) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	second := make(chan *model.Integration, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.WithIntegrationLocked(ctx, id, func(current *model.Integration) error {
			second <- current
			return nil
		})
	}()
	select {
	case <-second:
		t.Fatal("the second effect started while the first held the lock")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case current := <-second:
		if current == nil || current.ID != id {
			t.Fatalf("the second effect was handed %+v", current)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second effect never ran after the first released the lock")
	}
	wg.Wait()

	// Another integration's lock is another lock: no waiting across ids.
	other := subscriber(t, s, "other", model.WebhookScopeGlobal, "", true)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.WithIntegrationLocked(ctx, id, func(*model.Integration) error {
			<-release
			return nil
		})
	}()
	done := make(chan struct{})
	go func() {
		_ = s.WithIntegrationLocked(ctx, other, func(*model.Integration) error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an effect on another integration waited on this one's lock")
	}
	wg.Wait()

	if _, err := s.DeleteIntegration(ctx, id, "nina"); err != nil {
		t.Fatal(err)
	}
	if err := s.WithIntegrationLocked(ctx, id, func(current *model.Integration) error {
		if current != nil {
			t.Fatalf("a deleted integration was handed over as %+v", current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
