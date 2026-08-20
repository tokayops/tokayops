//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher"
	"github.com/tokayops/tokayops/internal/engine"
	"github.com/tokayops/tokayops/internal/ingester"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

type IntegrationTestEnv struct {
	S        *store.Store
	Ing      *ingester.Ingester
	Eng      *engine.Engine
	Disp     *dispatcher.Dispatcher
	MockProv *MockProvider
	Echo     *echo.Echo

	// Schedules is the schedule configuration command service, and Renderer the
	// projection the engine and the escalation builder read through. Schedules
	// in these tests are created the way the product creates them - as
	// revisions - because that is what the runtime now reads.
	Schedules *scheduleconfig.Service
	Renderer  *schedulerender.Service
}

// Rotation group identities for the schedules these tests configure. An L1
// group ID must be a canonical UUID; an L2 group is a singleton whose identity
// is the user ID itself.
const (
	pipelineGroupA = "aaaaaaaa-3333-4333-8333-000000000001"
	pipelineGroupB = "bbbbbbbb-3333-4333-8333-000000000002"
)

// pipelineSchedule creates a schedule for a team the way the product does - as
// the first revision of a configuration - and returns its ID.
//
// The users it names are added to the team first: the save pipeline refuses a
// configuration that puts a non-member on call, so a fixture without membership
// is a fixture that cannot exist.
func pipelineSchedule(t *testing.T, env *IntegrationTestEnv, teamID string,
	cfg rotation.ScheduleConfiguration, members ...string) string {

	t.Helper()
	for _, id := range members {
		if err := env.S.AddTeamMember(teamID, id, model.TeamMemberRoleMember); err != nil {
			t.Fatalf("AddTeamMember %s: %v", id, err)
		}
	}
	// How production creates a schedule: Save with expected_version 0.
	res, err := env.Schedules.Save(context.Background(), teamID, scheduleconfig.SaveCommand{
		Desired: cfg,
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return res.Revision.ScheduleID
}

// pipelineConfig is a daily rotation that hands over at midnight UTC, so the
// group configured here is the one on duty for the rest of the test.
func pipelineConfig(l1Groups []rotation.RotationGroup, l2User string) rotation.ScheduleConfiguration {
	policy := rotation.RotationPolicy{
		Cadence:     model.RotationDaily,
		HandoffTime: "00:00",
	}
	cfg := rotation.ScheduleConfiguration{
		Timezone: "UTC",
		L1: rotation.LayerConfiguration{
			Enabled: true,
			Policy:  policy,
			Groups:  l1Groups,
		},
		L2:                      rotation.LayerConfiguration{Enabled: false, Policy: policy},
		L2EscalationTimeoutMins: 5,
	}
	if l2User != "" {
		cfg.L2 = rotation.LayerConfiguration{
			Enabled: true,
			Policy:  policy,
			Groups:  []rotation.RotationGroup{{ID: l2User, Members: []string{l2User}}},
		}
	}
	return cfg
}

// testSecretValidator implements ingester.WebhookSecretValidator for integration tests
type testSecretValidator struct{}

func (v *testSecretValidator) ValidateWebhookSecret(secret string) bool {
	return secret == "test-secret"
}

func setupIntegrationTest(t *testing.T) *IntegrationTestEnv {
	// 1. Setup DB
	s := testutil.SetupDB(t)

	// 1a. Seed users referenced by escalation policies, with Slack identities.
	// Emails must be distinct and non-empty (users.email is UNIQUE; two empty
	// strings collide), otherwise the second insert is silently dropped.
	if err := s.CreateUser(&model.User{ID: "U_TEST", Email: "utest@pipeline.test", Name: "Test User"}); err != nil {
		t.Fatalf("CreateUser U_TEST: %v", err)
	}
	if err := s.CreateUser(&model.User{ID: "U_DEFAULT", Email: "udefault@pipeline.test", Name: "Default User"}); err != nil {
		t.Fatalf("CreateUser U_DEFAULT: %v", err)
	}
	testutil.BindSlack(t, s, "U_TEST", "S_TEST")
	testutil.BindSlack(t, s, "U_DEFAULT", "S_DEFAULT")

	// 1b. Create escalation policies in DB (required for Store-based EscalationJobBuilder)
	criticalPolicy := &model.EscalationPolicy{
		ID:   "critical_policy",
		Name: "Critical Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "crit_step_1",
				PolicyID:       "critical_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "U_TEST",
				DelaySeconds:   0,
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	s.CreateEscalationPolicy(criticalPolicy)

	defaultPolicy := &model.EscalationPolicy{
		ID:   "default_policy",
		Name: "Default Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "def_step_1",
				PolicyID:       "default_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "U_DEFAULT",
				DelaySeconds:   0,
				TimeoutSeconds: 30,
				MaxAttempts:    3,
			},
		},
	}
	s.CreateEscalationPolicy(defaultPolicy)

	// 1c. Create team with policy routing in DB
	team := &model.Team{
		ID:              "devops",
		Name:            "DevOps Team",
		DefaultPolicyID: "default_policy",
		SeverityRoutes: map[string]string{
			"critical": "critical_policy",
		},
	}
	s.CreateTeam(team)

	// 2. Setup Config (only for firehose channels now)
	cfg := &config.Config{
		ConfigVersion: 3,
		Global: config.GlobalConfig{
			FirehoseCriticalChannel: "C_FIREHOSE",
			FirehoseWarningChannel:  "C_WARNING",
		},
	}

	// Components
	ing := ingester.NewIngester(s, cfg, &testSecretValidator{})
	renderer := schedulerender.New(s.ScheduleReadRepository())
	eng := engine.NewEngine(s, renderer, cfg)
	disp, err := dispatcher.NewDispatcher(s, cfg)
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}

	// Mock Provider
	mockProvider := &MockProvider{}
	disp.RegisterProvider("slack", mockProvider)

	// Echo
	e := echo.New()
	ing.RegisterRoutes(e)

	return &IntegrationTestEnv{
		S:         s,
		Ing:       ing,
		Eng:       eng,
		Disp:      disp,
		MockProv:  mockProvider,
		Echo:      e,
		Schedules: scheduleconfig.NewService(s.ScheduleConfigRepository()),
		Renderer:  renderer,
	}
}

func sendWebhook(t *testing.T, e *echo.Echo, payload string) {
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=test-secret", strings.NewReader(payload))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
}

func waitForStepCompletion(t *testing.T, s *store.Store, alertKey string, stageIndex int) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		// The escalation is identified by its alert group, so the group's
		// dedup key - the alert fingerprint the caller has - reaches the job
		// through alert_groups rather than off the job row.
		err := s.GetDB().QueryRow(`
			SELECT js.status
			FROM job_steps js
			JOIN job_stages jst ON js.stage_id = jst.id
			JOIN jobs j ON jst.job_id = j.id
			JOIN alert_groups ag ON j.alert_group_id = ag.id
			WHERE ag.alert_key = $1 AND jst.stage_index = $2
			LIMIT 1
		`, alertKey, stageIndex).Scan(&status)

		if err == nil && (status == string(model.JobStepStatusSucceeded) || status == string(model.JobStepStatusFailed) || status == string(model.JobStepStatusCanceled)) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Timeout waiting for stage %d of group %s to complete", stageIndex, alertKey)
}

// runDispatcherLoop runs ProcessPendingSteps in a loop until context is canceled
func runDispatcherLoop(ctx context.Context, disp *dispatcher.Dispatcher) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			disp.ProcessPendingSteps(ctx)
		}
	}
}

func waitForAlertGroupStatus(t *testing.T, s *store.Store, alertKey string, expectedStatus model.AlertGroupStatus) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ag, err := s.GetActiveAlertGroupByAlertKey(alertKey)
		if err == nil && ag.Status == expectedStatus {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Fetch one last time for error message
	ag, _ := s.GetActiveAlertGroupByAlertKey(alertKey)
	current := "unknown"
	if ag != nil {
		current = string(ag.Status)
	}
	t.Fatalf("Timeout waiting for AG %s to reach status %s. Current: %s", alertKey, expectedStatus, current)
}

func TestPipeline_HappyPath(t *testing.T) {
	env := setupIntegrationTest(t)

	payload := `{
		"groupKey": "test_dedup_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "critical",
			"alertname": "TestAlert"
		},
		"alerts": [
			{
				"fingerprint": "fp1",
				"status": "firing",
				"labels": {"alertname": "TestAlert"}
			}
		]
	}`

	// 1. Ingest
	sendWebhook(t, env.Echo, payload)

	// Verify DB state
	active, err := env.S.GetActiveAlertGroupByAlertKey("test_dedup_1")
	if err != nil {
		t.Fatalf("Failed to get alert group: %v", err)
	}
	if active.Status != model.AlertGroupStatusNew {
		t.Errorf("Expected status New, got %s", active.Status)
	}

	// 2. Engine
	env.Eng.ProcessNewAlertGroups(context.Background())
	active, _ = env.S.GetActiveAlertGroupByAlertKey("test_dedup_1")
	if active.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status Processing, got %s", active.Status)
	}

	// Check if Job was created (optional but good sanity check)
	// We can't easily fetch it without ID, but we know CreateJobWithDedup was called.

	// 3. Dispatcher (Process Jobs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run dispatcher loop in background to process all steps
	go runDispatcherLoop(ctx, env.Disp)

	// Unified escalation job: firehose is step 0, policy step is step 1
	waitForStepCompletion(t, env.S, "test_dedup_1", 0) // Step 0: Firehose
	waitForStepCompletion(t, env.S, "test_dedup_1", 1) // Step 1: Policy DM

	if env.MockProv.SentCount() < 2 {
		t.Errorf("Expected at least 2 notifications (Firehose + Policy DM), got %d", env.MockProv.SentCount())
	}

	// Verify Timeline
	events, err := env.S.GetTimelineEvents(active.ID)
	if err != nil {
		t.Fatalf("Failed to fetch timeline: %v", err)
	}
	if len(events) == 0 {
		t.Error("Expected at least one timeline event (created/sent)")
	}
}

func TestPipeline_PartialUpdate(t *testing.T) {
	env := setupIntegrationTest(t)

	// Initial Firing
	payload1 := `{
		"groupKey": "test_partial_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "critical"},
		"alerts": [
			{"fingerprint": "fp1", "status": "firing", "labels": {"alertname": "A1"}},
			{"fingerprint": "fp2", "status": "firing", "labels": {"alertname": "A2"}}
		]
	}`
	sendWebhook(t, env.Echo, payload1)
	env.Eng.ProcessNewAlertGroups(context.Background())

	// Execute steps (unified job: firehose=step0, policy=step1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_partial_1", 0) // Step 0: Firehose
	waitForStepCompletion(t, env.S, "test_partial_1", 1) // Step 1: Policy DM

	// Verify initial state - Wait for worker to update status to Triggered
	waitForAlertGroupStatus(t, env.S, "test_partial_1", model.AlertGroupStatusTriggered)

	// Partial Resolve (fp1 resolved, fp2 firing)
	payload2 := `{
		"groupKey": "test_partial_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "critical"},
		"alerts": [
			{"fingerprint": "fp1", "status": "resolved", "labels": {"alertname": "A1"}},
			{"fingerprint": "fp2", "status": "firing", "labels": {"alertname": "A2"}}
		]
	}`
	sendWebhook(t, env.Echo, payload2)

	// Ingester keeps status as triggered (no regression) and flags Slack update.
	active, _ := env.S.GetActiveAlertGroupByAlertKey("test_partial_1")
	if active.Status != model.AlertGroupStatusTriggered {
		t.Errorf("Expected status to stay triggered, got %s", active.Status)
	}
	if !active.SlackUpdatePending {
		t.Error("Expected SlackUpdatePending to be true after partial update")
	}

	// Note: We skip Update notification verification as discussed
	t.Skip("Partial Update notification logic not yet migrated to Job Controller (TODO)")
}

func TestPipeline_FullResolve(t *testing.T) {
	env := setupIntegrationTest(t)

	// Initial Firing
	payload1 := `{
		"groupKey": "test_resolve_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "critical"},
		"alerts": [
			{"fingerprint": "fp1", "status": "firing", "labels": {"alertname": "A1"}}
		]
	}`
	sendWebhook(t, env.Echo, payload1)
	env.Eng.ProcessNewAlertGroups(context.Background())

	// Dispatch - unified job: firehose=step0, policy=step1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_resolve_1", 0) // Step 0: Firehose
	waitForStepCompletion(t, env.S, "test_resolve_1", 1) // Step 1: Policy DM

	// Full Resolve
	payload2 := `{
		"groupKey": "test_resolve_1",
		"status": "resolved",
		"commonLabels": {"team": "devops", "severity": "critical"},
		"alerts": [
			{"fingerprint": "fp1", "status": "resolved", "labels": {"alertname": "A1"}}
		]
	}`
	sendWebhook(t, env.Echo, payload2)

	// Verify Ingester Logic (Should set to Resolved)
	var status string
	env.S.GetDB().QueryRow("SELECT status FROM alert_groups WHERE alert_key = $1", "test_resolve_1").Scan(&status)
	if status != string(model.AlertGroupStatusResolved) {
		t.Errorf("Expected status Resolved, got %s", status)
	}

	// Dispatcher Resolution Loop
	// Legacy method `ProcessResolvedAlertGroups` still exists and works on Resolved AGs (creates Jobs).
	env.Disp.ProcessResolvedAlertGroups(ctx)
	// Note: dispatcher loop is already running, it will pick up resolution steps

	// Wait for Resolution
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if env.MockProv.ResolveCount() > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if env.MockProv.ResolveCount() == 0 {
		t.Error("Provider Resolve should have been called (via Resolution Job)")
	}

	// Verify Closed
	env.S.GetDB().QueryRow("SELECT status FROM alert_groups WHERE alert_key = $1", "test_resolve_1").Scan(&status)
	if status != string(model.AlertGroupStatusClosed) {
		t.Errorf("Expected status Closed, got %s", status)
	}
}

func TestPipeline_Dedup(t *testing.T) {
	env := setupIntegrationTest(t)

	payload := `{
		"groupKey": "test_dedup_idempotency_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "critical"},
		"alerts": [
			{"fingerprint": "fp1", "status": "firing", "labels": {"alertname": "A1"}}
		]
	}`

	// 1. First Ingestion
	sendWebhook(t, env.Echo, payload)

	// Check count (should be 1)
	var count int
	err := env.S.GetDB().QueryRow("SELECT COUNT(*) FROM alert_groups WHERE alert_key = $1", "test_dedup_idempotency_1").Scan(&count)
	if err != nil {
		t.Fatalf("DB Error: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 alert group, got %d", count)
	}

	// 2. Advance state to Processing
	env.Eng.ProcessNewAlertGroups(context.Background())

	// 3. Second Ingestion (Duplicate Payload)
	sendWebhook(t, env.Echo, payload)

	// Check count again (should still be 1, not 2)
	err = env.S.GetDB().QueryRow("SELECT COUNT(*) FROM alert_groups WHERE alert_key = $1", "test_dedup_idempotency_1").Scan(&count)
	if err != nil {
		t.Fatalf("DB Error: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 alert group after duplicate webhook, got %d", count)
	}
}

// MockProvider is safe for concurrent use (worker runs steps in parallel goroutines).
type MockProvider struct {
	sentCount    atomic.Int64
	updateCount  atomic.Int64
	resolveCount atomic.Int64

	mu          sync.Mutex
	sentTargets []string
}

// Send satisfies the post-Sprint-1 Provider interface: it takes a typed
// NotificationRequest and records the recipient under sentTargets so existing
// assertions on SendDM (now unified into Send with Editable=false) keep
// working. Editable=true returns a JSON payload that survives the delivery
// round-trip; fire-and-forget DMs return an empty payload.
func (m *MockProvider) Send(ctx context.Context, req dispatcher.NotificationRequest) (string, error) {
	m.sentCount.Add(1)
	m.mu.Lock()
	m.sentTargets = append(m.sentTargets, req.Target.ID)
	m.mu.Unlock()
	if !req.Editable {
		return "", nil
	}
	return `{"channel_id":"C_MOCK","timestamp":"1234567890.123456","permalink":"https://slack.com/mock"}`, nil
}

func (m *MockProvider) Update(ctx context.Context, _ *model.NotificationDelivery, _ *model.AlertGroup) (string, error) {
	m.updateCount.Add(1)
	return `{"channel_id":"C_MOCK","timestamp":"1234567890.123456","permalink":"https://slack.com/mock"}`, nil
}

func (m *MockProvider) Resolve(ctx context.Context, _ *model.NotificationDelivery, _ *model.AlertGroup) error {
	m.resolveCount.Add(1)
	return nil
}

// Permalink — Sprint 1 made this part of the Provider interface. Static stub
// is enough; the integration tests assert on counters, not on link shape.
func (m *MockProvider) Permalink(_ *model.NotificationDelivery) string {
	return "https://slack.com/mock"
}

func (m *MockProvider) SentCount() int    { return int(m.sentCount.Load()) }
func (m *MockProvider) UpdateCount() int  { return int(m.updateCount.Load()) }
func (m *MockProvider) ResolveCount() int { return int(m.resolveCount.Load()) }

// SentTargets returns a copy of all targetIDs that received a Send/SendDM call.
func (m *MockProvider) SentTargets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.sentTargets))
	copy(out, m.sentTargets)
	return out
}

// TestPipeline_FirehoseOnly verifies that firehose-only alerts (no policy) work correctly.
// This is the case when team is not found or has no policy configured.
func TestPipeline_FirehoseOnly(t *testing.T) {
	env := setupIntegrationTest(t)

	// Payload with unknown team (no policy will be resolved, but firehose should still work)
	payload := `{
		"groupKey": "test_firehose_only_1",
		"status": "firing",
		"commonLabels": {
			"team": "unknown_team",
			"severity": "critical",
			"alertname": "FirehoseOnlyAlert"
		},
		"alerts": [
			{
				"fingerprint": "fp_fire_only",
				"status": "firing",
				"labels": {"alertname": "FirehoseOnlyAlert"}
			}
		]
	}`

	// 1. Ingest
	sendWebhook(t, env.Echo, payload)

	// 2. Engine
	env.Eng.ProcessNewAlertGroups(context.Background())
	active, _ := env.S.GetActiveAlertGroupByAlertKey("test_firehose_only_1")
	if active.Status != model.AlertGroupStatusProcessing {
		t.Errorf("Expected status Processing, got %s", active.Status)
	}

	// PolicyID should be empty (unknown team)
	if active.PolicyID != "" {
		t.Errorf("Expected empty PolicyID for unknown team, got %s", active.PolicyID)
	}

	// 3. Dispatcher - should execute firehose step
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initialSentCount := env.MockProv.SentCount()
	env.Disp.ProcessPendingSteps(ctx)

	// Wait for firehose step (step 0) to complete
	waitForStepCompletion(t, env.S, "test_firehose_only_1", 0)

	// Verify firehose notification was sent
	if env.MockProv.SentCount() <= initialSentCount {
		t.Error("Expected firehose notification to be sent")
	}

	// Verify snapshot contains firehose step
	active, _ = env.S.GetAlertGroupByID(active.ID)
	if active.PolicySnapshot == nil {
		t.Fatal("PolicySnapshot should exist even for firehose-only")
	}
	if len(active.PolicySnapshot.Steps) != 1 {
		t.Errorf("Expected 1 step in snapshot (firehose), got %d", len(active.PolicySnapshot.Steps))
	}
	if !active.PolicySnapshot.Steps[0].IsFirehose {
		t.Error("First step in snapshot should be firehose")
	}
}

// TestPipeline_ResolutionAllDeliveries verifies that resolution job resolves ALL updatable deliveries
func TestPipeline_ResolutionAllDeliveries(t *testing.T) {
	env := setupIntegrationTest(t)

	// Create alert with firehose + policy step (2 deliveries)
	payload := `{
		"groupKey": "test_resolve_all_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "critical",
			"alertname": "ResolveAllAlert"
		},
		"alerts": [
			{
				"fingerprint": "fp_resolve_all",
				"status": "firing",
				"labels": {"alertname": "ResolveAllAlert"}
			}
		]
	}`

	// 1. Ingest and process
	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	// 2. Execute both steps (firehose + policy)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_resolve_all_1", 0) // Firehose
	waitForStepCompletion(t, env.S, "test_resolve_all_1", 1) // Policy DM

	// 3. Resolve alert
	resolvePayload := `{
		"groupKey": "test_resolve_all_1",
		"status": "resolved",
		"commonLabels": {"team": "devops", "severity": "critical"},
		"alerts": [
			{"fingerprint": "fp_resolve_all", "status": "resolved", "labels": {"alertname": "ResolveAllAlert"}}
		]
	}`
	sendWebhook(t, env.Echo, resolvePayload)

	// 4. Process resolution (dispatcher loop is already running)
	env.Disp.ProcessResolvedAlertGroups(ctx)

	// Wait for resolution to complete
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Resolution job should resolve all updatable deliveries
		// Firehose is updatable (SupportsUpdate=true)
		// DM is NOT updatable (SupportsUpdate=false)
		// So we expect at least 1 resolve call (firehose only, as DM doesn't support update)
		if env.MockProv.ResolveCount() > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if env.MockProv.ResolveCount() == 0 {
		t.Error("Expected at least one resolve call for updatable deliveries")
	}

	// Verify AG is closed
	var status string
	env.S.GetDB().QueryRow("SELECT status FROM alert_groups WHERE alert_key = $1", "test_resolve_all_1").Scan(&status)
	if status != string(model.AlertGroupStatusClosed) {
		t.Errorf("Expected status Closed, got %s", status)
	}
}

func TestPipeline_ScheduleFanOut(t *testing.T) {
	env := setupIntegrationTest(t)

	// Schedule with L1=U_TEST, L2=U_DEFAULT
	schedID := pipelineSchedule(t, env, "devops", pipelineConfig(
		[]rotation.RotationGroup{{ID: pipelineGroupA, Members: []string{"U_TEST"}}},
		"U_DEFAULT",
	), "U_TEST", "U_DEFAULT")

	// Policy with schedule target (fan-out)
	fanoutPolicy := &model.EscalationPolicy{
		ID:   "fanout_policy",
		Name: "Fan-out Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "fanout_step_1",
				PolicyID:       "fanout_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "schedule",
				TargetID:       schedID,
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(fanoutPolicy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}

	// Update team to use fanout policy for warning severity
	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "fanout_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_fanout_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "warning",
			"alertname": "FanOutTest"
		},
		"alerts": [{"fingerprint": "fp_fanout", "status": "firing", "labels": {"alertname": "FanOutTest"}}]
	}`

	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	initialSentCount := env.MockProv.SentCount()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	// Stage 0: firehose, Stage 1: fan-out DMs (L1 + L2 if resolved)
	waitForStepCompletion(t, env.S, "test_fanout_1", 0) // Firehose
	waitForStepCompletion(t, env.S, "test_fanout_1", 1) // Fan-out stage

	// Should have sent at least 2: firehose + at least 1 DM
	if env.MockProv.SentCount()-initialSentCount < 2 {
		t.Errorf("Expected at least 2 notifications (firehose + DM), got %d", env.MockProv.SentCount()-initialSentCount)
	}

	// Verify job succeeded
	waitForJobStatus(t, env.S, "test_fanout_1", model.JobStatusSucceeded)

	// Verify 2 stages: firehose + schedule fan-out
	var stageCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_stages jst
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_fanout_1'
	`).Scan(&stageCount)
	if stageCount != 2 {
		t.Errorf("Expected 2 stages, got %d", stageCount)
	}

	// Verify fan-out stage has at least 1 step (L1; L2 if schedule fully configured)
	var fanoutStepCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_fanout_1' AND jst.stage_index = 1
	`).Scan(&fanoutStepCount)
	if fanoutStepCount < 1 {
		t.Errorf("Expected at least 1 step in fan-out stage, got %d", fanoutStepCount)
	}

	// Verify all fan-out steps have ContinueOnFailure=true and TargetType=user
	var nonCofCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_fanout_1' AND jst.stage_index = 1
		  AND js.continue_on_failure = false
	`).Scan(&nonCofCount)
	if nonCofCount > 0 {
		t.Errorf("All fan-out steps should have ContinueOnFailure=true, but %d don't", nonCofCount)
	}
}

// TestPipeline_ScheduleFanOut_MultiUserGroup verifies that a single L1 group
// containing multiple users produces parallel DM steps for each member.
func TestPipeline_ScheduleFanOut_MultiUserGroup(t *testing.T) {
	env := setupIntegrationTest(t)

	// Create two users with distinct Slack IDs and unique emails
	if err := env.S.CreateUser(&model.User{ID: "U_ALICE", Email: "alice@multifanout.test", Name: "Alice"}); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if err := env.S.CreateUser(&model.User{ID: "U_BOB", Email: "bob@multifanout.test", Name: "Bob"}); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	testutil.BindSlack(t, env.S, "U_ALICE", "S_ALICE")
	testutil.BindSlack(t, env.S, "U_BOB", "S_BOB")

	// One group with both users — both should be on-call simultaneously
	schedID := pipelineSchedule(t, env, "devops", pipelineConfig(
		[]rotation.RotationGroup{{ID: pipelineGroupA, Members: []string{"U_ALICE", "U_BOB"}}},
		"",
	), "U_ALICE", "U_BOB")

	policy := &model.EscalationPolicy{
		ID:   "multi_fanout_policy",
		Name: "Multi Fan-out Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "multi_fanout_step_1",
				PolicyID:       "multi_fanout_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "schedule",
				TargetID:       schedID,
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(policy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}

	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "multi_fanout_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_multi_fanout_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "warning",
			"alertname": "MultiFanOutTest"
		},
		"alerts": [{"fingerprint": "fp_multi_fanout", "status": "firing", "labels": {"alertname": "MultiFanOutTest"}}]
	}`

	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	initialSentCount := env.MockProv.SentCount()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_multi_fanout_1", 0) // Firehose
	waitForStepCompletion(t, env.S, "test_multi_fanout_1", 1) // Fan-out stage
	waitForJobStatus(t, env.S, "test_multi_fanout_1", model.JobStatusSucceeded)

	// Stage count: firehose + fan-out
	var stageCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_stages jst
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_multi_fanout_1'
	`).Scan(&stageCount)
	if stageCount != 2 {
		t.Errorf("Expected 2 stages, got %d", stageCount)
	}

	// Fan-out stage must have EXACTLY 2 steps (one per group member)
	var fanoutStepCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_multi_fanout_1' AND jst.stage_index = 1
	`).Scan(&fanoutStepCount)
	if fanoutStepCount != 2 {
		t.Errorf("Expected exactly 2 fan-out steps, got %d", fanoutStepCount)
	}

	// All fan-out steps must have ContinueOnFailure=true
	var nonCofCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_multi_fanout_1' AND jst.stage_index = 1
		  AND js.continue_on_failure = false
	`).Scan(&nonCofCount)
	if nonCofCount > 0 {
		t.Errorf("All fan-out steps should have ContinueOnFailure=true, but %d don't", nonCofCount)
	}

	// Both Slack IDs must have received DMs
	if env.MockProv.SentCount()-initialSentCount < 3 {
		t.Errorf("Expected at least 3 notifications (firehose + 2 DMs), got %d",
			env.MockProv.SentCount()-initialSentCount)
	}
	targets := env.MockProv.SentTargets()
	targetSet := make(map[string]bool)
	for _, tgt := range targets {
		targetSet[tgt] = true
	}
	if !targetSet["S_ALICE"] {
		t.Errorf("Expected S_ALICE in sent targets, got %v", targets)
	}
	if !targetSet["S_BOB"] {
		t.Errorf("Expected S_BOB in sent targets, got %v", targets)
	}
}

// TestPipeline_ScheduleFanOut_OverrideOverGroup verifies that an override
// replaces the entire L1 group with the override user (single step, not fan-out).
func TestPipeline_ScheduleFanOut_OverrideOverGroup(t *testing.T) {
	env := setupIntegrationTest(t)

	if err := env.S.CreateUser(&model.User{ID: "U_ALICE", Email: "alice@override.test", Name: "Alice"}); err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if err := env.S.CreateUser(&model.User{ID: "U_BOB", Email: "bob@override.test", Name: "Bob"}); err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	testutil.BindSlack(t, env.S, "U_ALICE", "S_ALICE")
	testutil.BindSlack(t, env.S, "U_BOB", "S_BOB")

	schedID := pipelineSchedule(t, env, "devops", pipelineConfig(
		[]rotation.RotationGroup{{ID: pipelineGroupA, Members: []string{"U_ALICE", "U_BOB"}}},
		"",
	), "U_ALICE", "U_BOB")

	// Override for U_BOB covering the current instant — the projection overlays
	// it onto the layer, so the group on duty becomes U_BOB alone.
	now := time.Now().UTC()
	reason := "Test override"
	if _, err := env.Schedules.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID:    "U_BOB",
		ValidFrom: now.Add(-1 * time.Hour),
		ValidTo:   now.Add(1 * time.Hour),
		Reason:    &reason,
		ActorID:   "U_ALICE",
	}); err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	policy := &model.EscalationPolicy{
		ID:   "override_policy",
		Name: "Override Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "override_step_1",
				PolicyID:       "override_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "schedule",
				TargetID:       schedID,
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(policy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}

	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "override_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_override_group_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "warning",
			"alertname": "OverrideGroupTest"
		},
		"alerts": [{"fingerprint": "fp_override_group", "status": "firing", "labels": {"alertname": "OverrideGroupTest"}}]
	}`

	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	initialSentCount := env.MockProv.SentCount()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_override_group_1", 0)
	waitForStepCompletion(t, env.S, "test_override_group_1", 1)
	waitForJobStatus(t, env.S, "test_override_group_1", model.JobStatusSucceeded)

	// Fan-out stage must have EXACTLY 1 step (override replaces entire group)
	var stepCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_override_group_1' AND jst.stage_index = 1
	`).Scan(&stepCount)
	if stepCount != 1 {
		t.Errorf("Expected exactly 1 step (override replaces group), got %d", stepCount)
	}

	// Only S_BOB should have received a DM, not S_ALICE
	targets := env.MockProv.SentTargets()
	for _, tgt := range targets[initialSentCount:] {
		if tgt == "S_ALICE" {
			t.Errorf("S_ALICE should not receive DM when override active, targets: %v", targets)
		}
	}
	hasBob := false
	for _, tgt := range targets {
		if tgt == "S_BOB" {
			hasBob = true
			break
		}
	}
	if !hasBob {
		t.Errorf("Expected S_BOB (override user) to receive DM, targets: %v", targets)
	}
}

// TestPipeline_ScheduleFanOut_NoL2Additive verifies that L2 is NOT silently added
// to the schedule fan-out (regression for dadd042).
func TestPipeline_ScheduleFanOut_NoL2Additive(t *testing.T) {
	env := setupIntegrationTest(t)

	if err := env.S.CreateUser(&model.User{ID: "U_L1", Email: "l1@nol2.test", Name: "L1 User"}); err != nil {
		t.Fatalf("CreateUser L1: %v", err)
	}
	if err := env.S.CreateUser(&model.User{ID: "U_L2", Email: "l2@nol2.test", Name: "L2 User"}); err != nil {
		t.Fatalf("CreateUser L2: %v", err)
	}
	testutil.BindSlack(t, env.S, "U_L1", "S_L1")
	testutil.BindSlack(t, env.S, "U_L2", "S_L2")

	schedID := pipelineSchedule(t, env, "devops", pipelineConfig(
		[]rotation.RotationGroup{{ID: pipelineGroupA, Members: []string{"U_L1"}}},
		"U_L2",
	), "U_L1", "U_L2")

	policy := &model.EscalationPolicy{
		ID:   "no_l2_additive_policy",
		Name: "No L2 Additive Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "no_l2_step_1",
				PolicyID:       "no_l2_additive_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "schedule",
				TargetID:       schedID,
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(policy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}

	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "no_l2_additive_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_no_l2_additive_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "warning",
			"alertname": "NoL2AdditiveTest"
		},
		"alerts": [{"fingerprint": "fp_no_l2", "status": "firing", "labels": {"alertname": "NoL2AdditiveTest"}}]
	}`

	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	initialSentCount := env.MockProv.SentCount()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_no_l2_additive_1", 0)
	waitForStepCompletion(t, env.S, "test_no_l2_additive_1", 1)
	waitForJobStatus(t, env.S, "test_no_l2_additive_1", model.JobStatusSucceeded)

	// Fan-out stage must have EXACTLY 1 step (L1 only, no implicit L2)
	var stepCount int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_no_l2_additive_1' AND jst.stage_index = 1
	`).Scan(&stepCount)
	if stepCount != 1 {
		t.Errorf("Expected exactly 1 fan-out step (L1 only, no L2 additive), got %d", stepCount)
	}

	// S_L2 must NOT appear in sent targets
	targets := env.MockProv.SentTargets()
	for _, tgt := range targets[initialSentCount:] {
		if tgt == "S_L2" {
			t.Errorf("S_L2 must not receive DM (L2 not additive in schedule fan-out), targets: %v", targets)
		}
	}
}

// TestPipeline_DisabledProvider_PermanentFail verifies that a DM step targeting a
// provider with no enabled integration fails PERMANENTLY at runtime — the worker
// classifies ErrProviderNotConfigured as permanent (no retry loop) and the
// non-continue-on-failure step hard-fails the job. This is the end-to-end
// counterpart to the unit-level TestRegistry_DisabledIntegration_NotConfigured.
func TestPipeline_DisabledProvider_PermanentFail(t *testing.T) {
	env := setupIntegrationTest(t)

	// Register a provider whose backing integration type has no enabled integration
	// in this fresh DB, so resolving it yields ErrProviderNotConfigured — the same
	// error a disabled Slack integration produces. The build fn is never invoked.
	env.Disp.RegisterProviderFactory("blocked", model.IntegrationTypeAlertmanagerWebhook,
		func(*model.Integration) (dispatcher.Provider, error) { return env.MockProv, nil })

	// Policy with a single non-COF DM step on the unconfigured provider. MaxAttempts=3
	// so that a (wrong) transient classification would visibly retry; a permanent
	// failure must leave attempt_count at 0.
	blockedPolicy := &model.EscalationPolicy{
		ID:   "blocked_policy",
		Name: "Blocked Provider Policy",
		Steps: []*model.EscalationStep{
			{
				ID:                "blocked_step_1",
				PolicyID:          "blocked_policy",
				StepIndex:         0,
				Provider:          "blocked",
				TargetKind:        "dm",
				TargetType:        "user",
				TargetID:          "U_TEST",
				TimeoutSeconds:    10,
				MaxAttempts:       3,
				ContinueOnFailure: false,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(blockedPolicy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}
	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "blocked_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_disabled_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "warning", "alertname": "DisabledProviderTest"},
		"alerts": [{"fingerprint": "fp_disabled", "status": "firing", "labels": {"alertname": "DisabledProviderTest"}}]
	}`
	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_disabled_1", 0) // firehose succeeds
	waitForStepCompletion(t, env.S, "test_disabled_1", 1) // blocked DM step terminal

	// The blocked step must be failed and must NOT have retried (attempt_count stays 0
	// because the permanent-error check short-circuits before the retry increment).
	var status string
	var attempts int
	if err := env.S.GetDB().QueryRow(`
		SELECT js.status, js.attempt_count
		FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_disabled_1' AND jst.stage_index = 1
		LIMIT 1
	`).Scan(&status, &attempts); err != nil {
		t.Fatalf("query blocked step: %v", err)
	}
	if status != string(model.JobStepStatusFailed) {
		t.Errorf("expected blocked step status 'failed', got %q", status)
	}
	if attempts != 0 {
		t.Errorf("permanent failure must not retry: expected attempt_count 0, got %d", attempts)
	}

	// Non-COF failed step hard-fails the job.
	waitForJobStatus(t, env.S, "test_disabled_1", model.JobStatusFailed)
}

// TestPipeline_ChannelUpdate verifies a policy CHANNEL step produces an editable
// (supports_update=true) delivery and a timeline event — the "Escalation channel"
// row in the test strategy, which existing tests only covered via firehose.
func TestPipeline_ChannelUpdate(t *testing.T) {
	env := setupIntegrationTest(t)

	channelPolicy := &model.EscalationPolicy{
		ID:   "channel_policy",
		Name: "Channel Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "channel_step_1",
				PolicyID:       "channel_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "channel",
				TargetType:     "channel",
				TargetID:       "C_POLICY_CHAN",
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(channelPolicy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}
	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "channel_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_channel_update_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "warning", "alertname": "ChannelUpdateTest"},
		"alerts": [{"fingerprint": "fp_chan", "status": "firing", "labels": {"alertname": "ChannelUpdateTest"}}]
	}`
	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_channel_update_1", 0) // firehose
	waitForStepCompletion(t, env.S, "test_channel_update_1", 1) // policy channel step

	// The policy channel step must have produced an editable delivery row.
	var supportsUpdate bool
	if err := env.S.GetDB().QueryRow(`
		SELECT nd.supports_update
		FROM notification_deliveries nd
		JOIN alert_groups ag ON nd.alert_group_id = ag.id
		WHERE ag.alert_key = $1 AND nd.target_id = 'C_POLICY_CHAN'
	`, "test_channel_update_1").Scan(&supportsUpdate); err != nil {
		t.Fatalf("query channel delivery: %v", err)
	}
	if !supportsUpdate {
		t.Error("expected the channel-step delivery to be updatable (supports_update=true)")
	}

	// A timeline event specific to the CHANNEL step must exist — assert on its
	// metadata (step_type=channel + the channel id) so the test fails if the
	// channel step stops emitting it. The firehose event (step_type=firehose,
	// channel_id=C_WARNING) must not satisfy this.
	active, err := env.S.GetActiveAlertGroupByAlertKey("test_channel_update_1")
	if err != nil {
		t.Fatalf("GetActiveAlertGroup: %v", err)
	}
	events, err := env.S.GetTimelineEvents(active.ID)
	if err != nil {
		t.Fatalf("GetTimelineEvents: %v", err)
	}
	found := false
	for _, ev := range events {
		if ev.Type == model.TimelineEventNotificationSent &&
			ev.Metadata["step_type"] == "channel" &&
			ev.Metadata["channel_id"] == "C_POLICY_CHAN" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a notification-sent timeline event for the channel step "+
			"(step_type=channel, channel_id=C_POLICY_CHAN); got %d events", len(events))
	}
}

// TestPipeline_EscalationUnlinked verifies a DM step to a user WITHOUT a linked
// Slack identity fails permanently (no retry) end-to-end — the pipeline-level
// counterpart to the unit test TestResolveRecipient_NoIdentity_Permanent.
func TestPipeline_EscalationUnlinked(t *testing.T) {
	env := setupIntegrationTest(t)

	// User exists but has NO bound Slack identity.
	if err := env.S.CreateUser(&model.User{ID: "U_UNLINKED", Email: "unlinked@pipeline.test", Name: "Unlinked"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	unlinkedPolicy := &model.EscalationPolicy{
		ID:   "unlinked_policy",
		Name: "Unlinked DM Policy",
		Steps: []*model.EscalationStep{
			{
				ID:                "unlinked_step_1",
				PolicyID:          "unlinked_policy",
				StepIndex:         0,
				Provider:          "slack",
				TargetKind:        "dm",
				TargetType:        "user",
				TargetID:          "U_UNLINKED",
				TimeoutSeconds:    10,
				MaxAttempts:       3,
				ContinueOnFailure: false,
			},
		},
	}
	if err := env.S.CreateEscalationPolicy(unlinkedPolicy); err != nil {
		t.Fatalf("CreateEscalationPolicy: %v", err)
	}
	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "unlinked_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_unlinked_1",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "warning", "alertname": "UnlinkedTest"},
		"alerts": [{"fingerprint": "fp_unlinked", "status": "firing", "labels": {"alertname": "UnlinkedTest"}}]
	}`
	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	waitForStepCompletion(t, env.S, "test_unlinked_1", 0) // firehose
	waitForStepCompletion(t, env.S, "test_unlinked_1", 1) // unlinked DM step

	var status string
	var attempts int
	if err := env.S.GetDB().QueryRow(`
		SELECT js.status, js.attempt_count
		FROM job_steps js
		JOIN job_stages jst ON js.stage_id = jst.id
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_unlinked_1' AND jst.stage_index = 1
		LIMIT 1
	`).Scan(&status, &attempts); err != nil {
		t.Fatalf("query unlinked step: %v", err)
	}
	if status != string(model.JobStepStatusFailed) {
		t.Errorf("expected unlinked DM step status 'failed', got %q", status)
	}
	if attempts != 0 {
		t.Errorf("missing identity is permanent: expected attempt_count 0 (no retry), got %d", attempts)
	}

	// MockProvider must never have been asked to send — resolution fails first.
	if env.MockProv.SentTargets() != nil {
		for _, tgt := range env.MockProv.SentTargets() {
			if tgt == "U_UNLINKED" {
				t.Errorf("MockProvider.Send should not be called for an unlinked recipient")
			}
		}
	}

	waitForJobStatus(t, env.S, "test_unlinked_1", model.JobStatusFailed)
}

// TestPipeline_CancelDuringExecution tests the real ack-driven cancellation path.
//
// The cancellation happens inside AckAlertGroupAtomic itself, in the same
// transaction as the status change - not in the ProcessAcknowledgedAlertGroups
// pass that follows, whose own cancel is a second, idempotent one. The comment
// used to credit the later pass, which made this test look like it covered a
// path it does not.
//
// The DM step (stage 1) has a delay, so it is still blocked/pending when ack
// fires - which is what verifies that cancel reaches pending stages.
func TestPipeline_CancelDuringExecution(t *testing.T) {
	env := setupIntegrationTest(t)

	// Create a policy with a delayed DM step to ensure it's still pending when we ack
	delayedPolicy := &model.EscalationPolicy{
		ID:   "delayed_policy",
		Name: "Delayed Policy",
		Steps: []*model.EscalationStep{
			{
				ID:             "delayed_step_1",
				PolicyID:       "delayed_policy",
				StepIndex:      0,
				Provider:       "slack",
				TargetKind:     "dm",
				TargetType:     "user",
				TargetID:       "U_TEST",
				DelaySeconds:   300, // 5 min delay — step won't run before ack
				TimeoutSeconds: 10,
				MaxAttempts:    1,
			},
		},
	}
	env.S.CreateEscalationPolicy(delayedPolicy)

	// Route "warning" severity to delayed policy
	team, _ := env.S.GetTeamByID("devops")
	team.SeverityRoutes["warning"] = "delayed_policy"
	env.S.UpdateTeam(team)

	payload := `{
		"groupKey": "test_cancel_exec_1",
		"status": "firing",
		"commonLabels": {
			"team": "devops",
			"severity": "warning",
			"alertname": "CancelTest"
		},
		"alerts": [{"fingerprint": "fp_cancel", "status": "firing", "labels": {"alertname": "CancelTest"}}]
	}`

	sendWebhook(t, env.Echo, payload)
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go runDispatcherLoop(ctx, env.Disp)

	// Wait for firehose (stage 0) to complete — DM step (stage 1) is delayed, still pending
	waitForStepCompletion(t, env.S, "test_cancel_exec_1", 0)

	// Ack via the real production path: AckAlertGroupAtomic
	ag, err := env.S.GetActiveAlertGroupByAlertKey("test_cancel_exec_1")
	if err != nil {
		t.Fatalf("GetActiveAlertGroup: %v", err)
	}
	changed, err := env.S.AckAlertGroupAtomic(ag.ID, "test-user", nil, nil)
	if err != nil {
		t.Fatalf("AckAlertGroupAtomic: %v", err)
	}
	if !changed {
		t.Fatal("Expected ack to succeed")
	}

	// Stop dispatcher loop before ack processing to avoid race
	cancel()
	time.Sleep(200 * time.Millisecond) // let goroutine drain

	// Run the ack processing. Its own cancel is the second, idempotent one: the
	// job was already cancelled inside AckAlertGroupAtomic above.
	ackCtx := context.Background()
	env.Disp.ProcessAcknowledgedAlertGroups(ackCtx)

	// Verify job is canceled
	waitForJobStatus(t, env.S, "test_cancel_exec_1", model.JobStatusCanceled)

	// Verify no active/blocked stages remain on the canceled escalation job
	var activeStageCnt int
	env.S.GetDB().QueryRow(`
		SELECT COUNT(*) FROM job_stages jst
		JOIN jobs j ON jst.job_id = j.id
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = 'test_cancel_exec_1' AND j.status = 'canceled'
		  AND jst.status IN ('active', 'blocked')
	`).Scan(&activeStageCnt)
	if activeStageCnt != 0 {
		t.Errorf("Expected 0 active/blocked stages on canceled escalation job, got %d", activeStageCnt)
	}

	// Verify AG status is acknowledged
	updatedAG, _ := env.S.GetAlertGroupByID(ag.ID)
	if updatedAG.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected AG acknowledged, got %s", updatedAG.Status)
	}
}

func waitForJobStatus(t *testing.T, s *store.Store, alertKey string, expected model.JobStatus) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		err := s.GetDB().QueryRow(`
			SELECT j.status FROM jobs j
			JOIN alert_groups ag ON j.alert_group_id = ag.id
			WHERE ag.alert_key = $1`, alertKey).Scan(&status)
		if err == nil && status == string(expected) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	var actual string
	s.GetDB().QueryRow(`
		SELECT j.status FROM jobs j
		JOIN alert_groups ag ON j.alert_group_id = ag.id
		WHERE ag.alert_key = $1`, alertKey).Scan(&actual)
	t.Fatalf("Timeout waiting for job %s to reach status %s, current: %s", alertKey, expected, actual)
}
