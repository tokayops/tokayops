package ingester

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// errLookupStore wraps MockStore but returns a DB error from GetActiveAlertGroup.
// Simulates transient DB failures (connection timeout, deadlock, etc.)
type errLookupStore struct {
	*store.MockStore
	lookupErr error
}

func (s *errLookupStore) GetActiveAlertGroup(dedupKey string) (*model.AlertGroup, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	return s.MockStore.GetActiveAlertGroup(dedupKey)
}

// duplicateKeyStore simulates a race condition:
// - First GetActiveAlertGroup call returns ErrNoRows
// - CreateAlertGroup returns duplicate key error
// - Second GetActiveAlertGroup call returns the group (created by concurrent webhook)
type duplicateKeyStore struct {
	*store.MockStore
	lookupCalls int
}

func (s *duplicateKeyStore) GetActiveAlertGroup(dedupKey string) (*model.AlertGroup, error) {
	s.lookupCalls++
	if s.lookupCalls == 1 {
		return nil, sql.ErrNoRows
	}
	// On retry, return the group from the underlying store
	return s.MockStore.GetActiveAlertGroup(dedupKey)
}

func (s *duplicateKeyStore) CreateAlertGroupAtomic(ag *model.AlertGroup, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) error {
	return &pq.Error{
		Code:    "23505",
		Message: "duplicate key value violates unique constraint \"idx_active_alert_groups\"",
	}
}

// TestRegression_DBError_ResolveLost verifies that a transient DB error
// on GetActiveAlertGroup does NOT silently drop resolve webhooks.
// Bug: the old code treated DB errors the same as "no group found",
// falling through to the create path where all-resolved payloads were
// silently ignored with 200 "Ignored Resolved".
func TestRegression_DBError_ResolveLost(t *testing.T) {
	mock := store.NewMockStore()

	// Pre-create an active alert group with a firing alert
	ag := &model.AlertGroup{
		ID:       "ag-db-err",
		DedupKey: "db-err-group",
		Status:   model.AlertGroupStatusTriggered,
		Alerts: []model.Alert{
			{Fingerprint: "fp1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "TestAlert"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	mock.CreateAlertGroup(ag)

	// Wrap with a store that returns a DB error on lookup
	errStore := &errLookupStore{
		MockStore: mock,
		lookupErr: fmt.Errorf("pq: connection reset by peer"),
	}

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(errStore, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Send a resolve webhook — all alerts resolved
	payload := `{"status":"resolved","groupKey":"db-err-group","alerts":[{"status":"resolved","labels":{"alertname":"TestAlert"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// EXPECTED: 500 so Alertmanager retries
	// BUG (unfixed): 200 "Ignored Resolved" — resolve data lost silently
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Expected 500 on DB lookup error, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Verify alert is still firing (resolve was NOT applied)
	storedAG, _ := mock.GetAlertGroupByID("ag-db-err")
	if storedAG.Status == model.AlertGroupStatusResolved {
		t.Error("Alert group should NOT be resolved when DB lookup failed")
	}
}

// TestRegression_DBError_FiringPayload verifies that a transient DB error
// returns 500 even for firing payloads (not just resolved ones).
func TestRegression_DBError_FiringPayload(t *testing.T) {
	mock := store.NewMockStore()

	errStore := &errLookupStore{
		MockStore: mock,
		lookupErr: fmt.Errorf("pq: too many connections"),
	}

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(errStore, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	payload := `{"status":"firing","groupKey":"new-group","alerts":[{"status":"firing","labels":{"alertname":"Test"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// EXPECTED: 500 (DB error should block processing, Alertmanager will retry)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Expected 500 on DB lookup error, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestRegression_DuplicateKey_RetryMerge verifies that when CreateAlertGroup
// fails with a duplicate key constraint (race condition), the ingester retries
// by looking up the group and merging instead of returning 500.
func TestRegression_DuplicateKey_RetryMerge(t *testing.T) {
	mock := store.NewMockStore()

	// Pre-create the group in the underlying store (simulates: another webhook created it concurrently)
	ag := &model.AlertGroup{
		ID:       "ag-dup-key",
		DedupKey: "dup-group",
		Status:   model.AlertGroupStatusNew,
		Alerts: []model.Alert{
			{Fingerprint: "fp-existing", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "ExistingAlert"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	mock.CreateAlertGroup(ag)

	dupStore := &duplicateKeyStore{MockStore: mock}

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(dupStore, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Send a firing webhook (will hit duplicate key, should retry as merge)
	payload := `{"status":"firing","groupKey":"dup-group","alerts":[{"status":"firing","labels":{"alertname":"NewAlert"},"fingerprint":"fp-new"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// EXPECTED: 200 (retry succeeded, merged into existing group)
	// BUG (unfixed): 500 "Failed to persist" — data from second webhook lost
	if rec.Code != http.StatusOK {
		t.Errorf("Expected 200 after duplicate key retry, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Verify alerts were merged
	storedAG, _ := mock.GetAlertGroupByID("ag-dup-key")
	if len(storedAG.Alerts) != 2 {
		t.Errorf("Expected 2 alerts after merge, got %d", len(storedAG.Alerts))
	}
}

// TestRegression_MergeDoesNotRegressTriggeredStatus verifies that when a repeat webhook
// arrives for an AG in "triggered" status, the status stays "triggered" (no regression
// to "processing"), alerts are updated, and slack_update_pending is set.
func TestRegression_MergeDoesNotRegressTriggeredStatus(t *testing.T) {
	mock := store.NewMockStore()

	// Pre-create an active alert group in triggered status
	ag := &model.AlertGroup{
		ID:       "ag-triggered",
		DedupKey: "triggered-group",
		Status:   model.AlertGroupStatusTriggered,
		TeamID:   "devops",
		Severity: "critical",
		Alerts: []model.Alert{
			{Fingerprint: "fp1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "TestAlert"}},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	mock.CreateAlertGroup(ag)

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(mock, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Send a webhook with a new firing alert (merge scenario)
	payload := `{"status":"firing","groupKey":"triggered-group","alerts":[{"status":"firing","labels":{"alertname":"NewAlert"},"fingerprint":"fp2"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// Verify: status should still be "triggered" (NOT regressed to "processing")
	storedAG, _ := mock.GetAlertGroupByID("ag-triggered")
	if storedAG.Status != model.AlertGroupStatusTriggered {
		t.Errorf("Expected status to stay 'triggered', got '%s' (status regression!)", storedAG.Status)
	}

	// Verify: alerts were updated (should have both alerts now)
	if len(storedAG.Alerts) != 2 {
		t.Errorf("Expected 2 alerts after merge, got %d", len(storedAG.Alerts))
	}

	// Verify: slack_update_pending should be true
	if !storedAG.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be true after merge")
	}
}

// errTeamLookupStore wraps MockStore but returns a configurable error from GetTeamByID.
// Simulates transient DB failures during team resolution.
type errTeamLookupStore struct {
	*store.MockStore
	teamErr error
}

func (s *errTeamLookupStore) GetTeamByID(id string) (*model.Team, error) {
	if s.teamErr != nil {
		return nil, s.teamErr
	}
	return s.MockStore.GetTeamByID(id)
}

// TestRegression_UnknownTeam_OutboxCreated verifies that an unknown team
// (GetTeamByID returns sql.ErrNoRows) still ingests successfully and creates
// an outbox event for global webhook fan-out.
func TestRegression_UnknownTeam_OutboxCreated(t *testing.T) {
	mock := store.NewMockStore()
	// No teams seeded — GetTeamByID will return sql.ErrNoRows

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(mock, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	payload := `{"status":"firing","groupKey":"phantom-group","commonLabels":{"team":"phantom_team","severity":"warning","alertname":"Test"},"alerts":[{"status":"firing","labels":{"alertname":"Test"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// AG should exist with TeamNameSnapshot falling back to teamID
	ag, err := mock.GetActiveAlertGroup("phantom-group")
	if err != nil || ag == nil {
		t.Fatal("Expected alert group to be created")
	}
	if ag.TeamNameSnapshot != "phantom_team" {
		t.Errorf("Expected TeamNameSnapshot 'phantom_team', got %q", ag.TeamNameSnapshot)
	}

	// Outbox event should exist for global webhook fan-out
	events, _ := mock.GetPendingOutboxEvents(10)
	var found bool
	for _, ev := range events {
		if ev.AlertGroupID == ag.ID && ev.TeamID == "phantom_team" {
			found = true
		}
	}
	if !found {
		t.Error("Expected outbox event with TeamID 'phantom_team' for global webhook fan-out")
	}
}

// TestRegression_TeamDBError_Returns500 verifies that a transient DB error
// from GetTeamByID returns 500 so Alertmanager retries, rather than silently
// swallowing the error and dropping the outbox event.
func TestRegression_TeamDBError_Returns500(t *testing.T) {
	mock := store.NewMockStore()

	errStore := &errTeamLookupStore{
		MockStore: mock,
		teamErr:   fmt.Errorf("pq: connection refused"),
	}

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(errStore, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	payload := `{"status":"firing","groupKey":"team-err-group","commonLabels":{"team":"devops","severity":"critical","alertname":"Test"},"alerts":[{"status":"firing","labels":{"alertname":"Test"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("Expected 500 on team DB error, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	// No AG should be created
	ag, _ := mock.GetActiveAlertGroup("team-err-group")
	if ag != nil {
		t.Error("Expected no alert group to be created on DB error")
	}

	// No outbox events
	events, _ := mock.GetPendingOutboxEvents(10)
	if len(events) != 0 {
		t.Errorf("Expected 0 outbox events, got %d", len(events))
	}
}

// concurrentResolveStore simulates a race condition where another request
// (manual/slack resolve) wins the CAS race and resolves the AG between
// the ingester's GetActiveAlertGroup read and the atomic resolve call.
type concurrentResolveStore struct {
	*store.MockStore
}

func (s *concurrentResolveStore) ResolveAlertGroupWithAlertsAtomic(id string, alerts []model.Alert, timelineEvents []*model.TimelineEvent, outboxEvent *model.OutboxEvent) (bool, error) {
	// Simulate: another request resolved the AG between our read and this call
	return false, nil
}

// TestRegression_ConcurrentResolve_AlertsConverge verifies that when the atomic
// resolve returns changed=false (concurrent manual resolve won), the ingester
// still syncs alerts_data via best-effort UpdateAlertGroupAlerts.
func TestRegression_ConcurrentResolve_AlertsConverge(t *testing.T) {
	mock := store.NewMockStore()

	// Pre-create an active AG with a firing alert
	ag := &model.AlertGroup{
		ID: "ag-race-resolve", DedupKey: "race-resolve-group",
		Status: model.AlertGroupStatusTriggered, TeamID: "triage", TeamNameSnapshot: "triage",
		Severity: "warning",
		Alerts: []model.Alert{
			{Fingerprint: "fp1", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "TestAlert"}},
		},
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	mock.CreateAlertGroup(ag)

	raceStore := &concurrentResolveStore{MockStore: mock}

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	ing := NewIngester(raceStore, &config.Config{}, validator)
	e := echo.New()
	ing.RegisterRoutes(e)

	// Send resolve webhook — atomic call returns changed=false, but alerts should still sync
	payload := `{"status":"resolved","groupKey":"race-resolve-group","alerts":[{"status":"resolved","labels":{"alertname":"TestAlert"},"fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	// Must be 200 (idempotent)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "Resolved" {
		t.Errorf("Expected 'Resolved', got '%s'", rec.Body.String())
	}

	// Verify alerts_data was synced via best-effort UpdateAlertGroupAlerts
	storedAG, _ := mock.GetAlertGroupByID("ag-race-resolve")
	if len(storedAG.Alerts) != 1 {
		t.Fatalf("Expected 1 alert, got %d", len(storedAG.Alerts))
	}
	if storedAG.Alerts[0].Status != model.AlertStatusResolved {
		t.Errorf("Expected alert status 'resolved' (data converged), got '%s'", storedAG.Alerts[0].Status)
	}

	// No outbox events — the race loser shouldn't create duplicate events
	events, _ := mock.GetPendingOutboxEvents(10)
	if len(events) != 0 {
		t.Errorf("Expected 0 outbox events (race loser), got %d", len(events))
	}
}

// Note: resolve-only payloads (allResolved=true) return "Ignored Resolved"
// before reaching CreateAlertGroup, so the duplicate key retry path is not
// exercised for pure resolve webhooks. The DB error scenario
// (TestRegression_DBError_ResolveLost) covers resolve loss prevention.

// unknownTeamCount reads the counter for one team label. Metrics are process
// globals shared by every test in the binary, so each case uses its own label
// and compares a delta rather than an absolute value.
func unknownTeamCount(t *testing.T, team string) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.UnknownTeamAlertGroupsTotal.WithLabelValues(team).Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// postFiring drives one firing webhook through the ingester and returns the
// HTTP status.
func postFiring(t *testing.T, st store.StoreInterface, groupKey, team string) int {
	t.Helper()
	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	e := echo.New()
	NewIngester(st, &config.Config{}, validator).RegisterRoutes(e)

	payload := fmt.Sprintf(
		`{"status":"firing","groupKey":%q,"commonLabels":{"team":%q,"severity":"warning","alertname":"Test"},"alerts":[{"status":"firing","labels":{"alertname":"Test"},"fingerprint":"fp1"}]}`,
		groupKey, team,
	)
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec.Code
}

// The counter answers "which teams should be onboarded next", so it must count
// alert groups that were actually created. Counting at the team lookup instead
// would also count Alertmanager retries, the duplicate-key merge path and
// requests that go on to fail.
func TestIngester_UnknownTeamCounter(t *testing.T) {
	t.Run("counts a created group with an unknown team", func(t *testing.T) {
		const team = "counter_unknown"
		before := unknownTeamCount(t, team)

		if code := postFiring(t, store.NewMockStore(), "counter-unknown-group", team); code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if got := unknownTeamCount(t, team) - before; got != 1 {
			t.Errorf("counter delta = %v, want 1", got)
		}
	})

	t.Run("does not count a known team", func(t *testing.T) {
		// MockStore seeds a "devops" team.
		const team = "devops"
		before := unknownTeamCount(t, team)

		if code := postFiring(t, store.NewMockStore(), "counter-known-group", team); code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if got := unknownTeamCount(t, team) - before; got != 0 {
			t.Errorf("counter delta = %v, want 0 for an onboarded team", got)
		}
	})

	t.Run("does not count a request that fails", func(t *testing.T) {
		const team = "counter_failed"
		before := unknownTeamCount(t, team)

		st := &errTeamLookupStore{MockStore: store.NewMockStore(), teamErr: fmt.Errorf("pq: connection refused")}
		if code := postFiring(t, st, "counter-failed-group", team); code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", code)
		}
		if got := unknownTeamCount(t, team) - before; got != 0 {
			t.Errorf("counter delta = %v, want 0 when nothing was created", got)
		}
	})

	t.Run("does not count a merge into an existing group", func(t *testing.T) {
		const team = "counter_merged"
		before := unknownTeamCount(t, team)

		mock := store.NewMockStore()
		existing := &model.AlertGroup{
			ID: "ag-merge", DedupKey: "counter-merge-group", Status: model.AlertGroupStatusTriggered,
			TeamID: team, Severity: "warning", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		if err := mock.CreateAlertGroup(existing); err != nil {
			t.Fatalf("seed alert group: %v", err)
		}

		st := &duplicateKeyStore{MockStore: mock}
		if code := postFiring(t, st, "counter-merge-group", team); code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if got := unknownTeamCount(t, team) - before; got != 0 {
			t.Errorf("counter delta = %v, want 0 for a merge", got)
		}
	})
}

// postAlerts drives one Alertmanager webhook carrying a raw alerts array through
// the ingester and returns the recorder.
func postAlerts(t *testing.T, e *echo.Echo, groupKey, alerts string) *httptest.ResponseRecorder {
	t.Helper()
	payload := fmt.Sprintf(
		`{"status":"firing","groupKey":%q,"commonLabels":{"team":"devops","severity":"warning","alertname":"InstanceJustRebooted"},"alerts":[%s]}`,
		groupKey, alerts,
	)
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// fingerprints lists the fingerprints stored on an alert group. mergeAlerts
// builds its result by ranging a map, so the order is not stable - callers must
// compare as a set.
func fingerprints(ag *model.AlertGroup) map[string]model.AlertStatus {
	got := make(map[string]model.AlertStatus, len(ag.Alerts))
	for _, a := range ag.Alerts {
		got[a.Fingerprint] = a.Status
	}
	return got
}

// TestRegression_ResolvedAlertFromPreviousGroup_NotMergedIntoNewGroup covers
// GitHub issue #23: hosts firing the same alertname one after another share one
// Alertmanager groupKey, and Alertmanager keeps re-sending alerts it resolved
// earlier for that aggregation group. Those alerts were closed together with a
// previous alert group carrying the same dedup key, so the merge path must not
// pull them into the group that came after.
func TestRegression_ResolvedAlertFromPreviousGroup_NotMergedIntoNewGroup(t *testing.T) {
	const groupKey = "leak-group"
	const alertA = `{"status":%q,"labels":{"alertname":"InstanceJustRebooted","instance":"10.64.172.135"},"fingerprint":"A"}`
	const alertB = `{"status":%q,"labels":{"alertname":"InstanceJustRebooted","instance":"10.64.173.65"},"fingerprint":"B"}`

	mock := store.NewMockStore()
	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	e := echo.New()
	NewIngester(mock, &config.Config{}, validator).RegisterRoutes(e)

	firingA := fmt.Sprintf(alertA, model.AlertStatusFiring)
	resolvedA := fmt.Sprintf(alertA, model.AlertStatusResolved)
	firingB := fmt.Sprintf(alertB, model.AlertStatusFiring)
	resolvedB := fmt.Sprintf(alertB, model.AlertStatusResolved)

	// 1. Host A reboots: first group is created.
	if body := postAlerts(t, e, groupKey, firingA).Body.String(); body != "Created" {
		t.Fatalf("step 1: got %q, want \"Created\"", body)
	}
	first, err := mock.GetActiveAlertGroup(groupKey)
	if err != nil || first == nil {
		t.Fatalf("step 1: first group not created: %v", err)
	}

	// 2. Host A recovers: first group resolves.
	if body := postAlerts(t, e, groupKey, resolvedA).Body.String(); body != "Resolved" {
		t.Fatalf("step 2: got %q, want \"Resolved\"", body)
	}

	// 3. Host B reboots. Alertmanager still carries the resolved A along; the
	//    create path already filters it out, so a fresh group holds only B.
	if body := postAlerts(t, e, groupKey, firingB+","+resolvedA).Body.String(); body != "Created" {
		t.Fatalf("step 3: got %q, want \"Created\"", body)
	}
	second, err := mock.GetActiveAlertGroup(groupKey)
	if err != nil || second == nil {
		t.Fatalf("step 3: second group not created: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("step 3: expected a new group, got the resolved one back (%s)", second.ID)
	}
	if got := fingerprints(second); len(got) != 1 || got["B"] != model.AlertStatusFiring {
		t.Fatalf("step 3: second group holds %v, want only B firing", got)
	}

	// 4. Host B recovers. This merge used to add the stale resolved A.
	if body := postAlerts(t, e, groupKey, resolvedB+","+resolvedA).Body.String(); body != "Resolved" {
		t.Fatalf("step 4: got %q, want \"Resolved\"", body)
	}

	stored, err := mock.GetAlertGroupByID(second.ID)
	if err != nil || stored == nil {
		t.Fatalf("step 4: second group not readable: %v", err)
	}
	if stored.Status != model.AlertGroupStatusResolved {
		t.Errorf("second group status = %q, want %q", stored.Status, model.AlertGroupStatusResolved)
	}
	got := fingerprints(stored)
	if len(got) != 1 || got["B"] != model.AlertStatusResolved {
		t.Errorf("second group holds %v, want only B resolved", got)
	}

	// The first incident stays a faithful record of itself.
	firstStored, err := mock.GetAlertGroupByID(first.ID)
	if err != nil || firstStored == nil {
		t.Fatalf("first group not readable: %v", err)
	}
	if got := fingerprints(firstStored); len(got) != 1 || got["A"] != model.AlertStatusResolved {
		t.Errorf("first group holds %v, want only A resolved", got)
	}

	// The timeline never knew about A in the second group, and alerts_data must agree.
	events, err := mock.GetTimelineEvents(second.ID)
	if err != nil {
		t.Fatalf("timeline read: %v", err)
	}
	for _, ev := range events {
		if ev.Metadata["fingerprint"] == "A" {
			t.Errorf("second group timeline mentions alert A: %s %q", ev.Type, ev.Message)
		}
	}
}

// TestRegression_MergePayloadWithOnlyForeignResolvedAlerts_NoOp verifies that a
// payload holding nothing the group owns is a no-op: no alerts_data rewrite and
// no Slack re-render flag.
func TestRegression_MergePayloadWithOnlyForeignResolvedAlerts_NoOp(t *testing.T) {
	mock := store.NewMockStore()

	created := time.Now().Add(-time.Hour)
	ag := &model.AlertGroup{
		ID: "ag-foreign-noop", DedupKey: "foreign-noop-group",
		Status: model.AlertGroupStatusTriggered, TeamID: "devops", TeamNameSnapshot: "DevOps",
		Severity: "warning",
		Alerts: []model.Alert{
			{Fingerprint: "A", Status: model.AlertStatusFiring, Labels: map[string]string{"alertname": "InstanceJustRebooted"}},
		},
		CreatedAt: created, UpdatedAt: created,
	}
	if err := mock.CreateAlertGroup(ag); err != nil {
		t.Fatalf("seed alert group: %v", err)
	}

	validator := &mockSecretValidator{secrets: map[string]bool{"secret": true}}
	e := echo.New()
	NewIngester(mock, &config.Config{}, validator).RegisterRoutes(e)

	// Only a resolved alert the group has never seen (closed with an earlier incident).
	rec := postAlerts(t, e, "foreign-noop-group",
		`{"status":"resolved","labels":{"alertname":"InstanceJustRebooted"},"fingerprint":"X"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "Ignored Resolved" {
		t.Errorf("body = %q, want \"Ignored Resolved\"", body)
	}

	stored, err := mock.GetAlertGroupByID("ag-foreign-noop")
	if err != nil || stored == nil {
		t.Fatalf("alert group not readable: %v", err)
	}
	if got := fingerprints(stored); len(got) != 1 || got["A"] != model.AlertStatusFiring {
		t.Errorf("group holds %v, want only A firing", got)
	}
	if stored.SlackUpdatePending {
		t.Error("SlackUpdatePending = true, want false - nothing changed, so no card re-render")
	}
	// Both MockStore writes bump UpdatedAt, so an untouched timestamp proves neither ran.
	if !stored.UpdatedAt.Equal(created) {
		t.Errorf("UpdatedAt moved to %v, want %v - the group was written for nothing", stored.UpdatedAt, created)
	}
}
