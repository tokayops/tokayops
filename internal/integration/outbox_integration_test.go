//go:build integration

package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbox"
	"github.com/tokayops/tokayops/internal/testutil"
)

// testEncryptionKey is a 32-byte hex key used for integration tests.
const testEncryptionKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// loopbackCIDR allows httptest servers (127.0.0.1) through SSRF protection.
var loopbackCIDR []*net.IPNet

func init() {
	_, cidr, _ := net.ParseCIDR("127.0.0.0/8")
	loopbackCIDR = []*net.IPNet{cidr}
}

// setEncryptionKey sets ENCRYPTION_KEY for the duration of the test.
func setEncryptionKey(t *testing.T) {
	t.Helper()
	prev := os.Getenv("ENCRYPTION_KEY")
	os.Setenv("ENCRYPTION_KEY", testEncryptionKey)
	t.Cleanup(func() {
		if prev == "" {
			os.Unsetenv("ENCRYPTION_KEY")
		} else {
			os.Setenv("ENCRYPTION_KEY", prev)
		}
	})
}

// TestOutbox_AtomicEventSurvives proves the transactional guarantee: alert group,
// timeline events, and outbox event are all visible after a single TX commit.
func TestOutbox_AtomicEventSurvives(t *testing.T) {
	setEncryptionKey(t)
	s := testutil.SetupDB(t)

	team := testutil.SeedTeam(t, s, "team-outbox")

	agID := uuid.New().String()
	now := time.Now()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "outbox-atomic-dedup",
		Status:           model.AlertGroupStatusTriggered,
		Title:            "Atomic Outbox Test",
		TeamID:           team.ID,
		TeamNameSnapshot: team.Name,
		Severity:         "critical",
		Alerts:           []model.Alert{{Fingerprint: "fp-atomic", Status: "firing"}},
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	tlID := uuid.New().String()
	timeline := []*model.TimelineEvent{
		{
			ID:           tlID,
			AlertGroupID: agID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created",
			Actor:        "system",
			CreatedAt:    now,
		},
	}

	payload, err := model.BuildWebhookEventPayload(
		model.OutboxEventFiring, ag, team.Name, "system", "", now,
	)
	if err != nil {
		t.Fatalf("BuildWebhookEventPayload: %v", err)
	}

	eventID := uuid.New().String()
	event := &model.OutboxEvent{
		ID:           eventID,
		EventType:    model.OutboxEventFiring,
		AlertGroupID: agID,
		TeamID:       team.ID,
		Actor:        "system",
		Payload:      payload,
	}

	if err := s.CreateAlertGroupAtomic(ag, timeline, event); err != nil {
		t.Fatalf("CreateAlertGroupAtomic: %v", err)
	}

	// Verify outbox event
	fetched, err := s.GetOutboxEventByID(eventID)
	if err != nil {
		t.Fatalf("GetOutboxEventByID: %v", err)
	}
	if fetched.Status != model.OutboxEventStatusPending {
		t.Errorf("event status: got %q, want %q", fetched.Status, model.OutboxEventStatusPending)
	}
	if fetched.TeamID != team.ID {
		t.Errorf("event team_id: got %q, want %q", fetched.TeamID, team.ID)
	}

	// Verify alert group
	fetchedAG, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("GetAlertGroupByID: %v", err)
	}
	if fetchedAG.Title != ag.Title {
		t.Errorf("AG title: got %q, want %q", fetchedAG.Title, ag.Title)
	}

	// Verify timeline events
	events, err := s.GetTimelineEvents(agID)
	if err != nil {
		t.Fatalf("GetTimelineEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("Expected at least 1 timeline event, got 0")
	}
}

// TestOutbox_WorkerDeliversToRealHTTP is a full E2E test: real DB -> worker ->
// real HTTP endpoint -> verify headers + HMAC signature.
func TestOutbox_WorkerDeliversToRealHTTP(t *testing.T) {
	setEncryptionKey(t)
	s := testutil.SetupDB(t)

	team := testutil.SeedTeam(t, s, "team-http-e2e")

	// Start httptest server capturing requests
	var mu sync.Mutex
	var captured []*http.Request
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, r)
		capturedBody = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Create generic_webhook integration
	secret := "test-secret"
	scope := model.WebhookScopeGlobal
	cfg, _ := json.Marshal(model.GenericWebhookConfig{
		URL:    srv.URL,
		Secret: secret,
	})
	integ := &model.Integration{
		ID:      uuid.New().String(),
		Type:    model.IntegrationTypeGenericWebhook,
		Name:    "E2E Test Webhook",
		Enabled: true,
		Scope:   &scope,
		Config:  cfg,
	}
	if err := s.CreateIntegration(integ); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}

	// Create alert group + outbox event via atomic operation
	agID := uuid.New().String()
	now := time.Now()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "http-e2e-dedup",
		Status:           model.AlertGroupStatusTriggered,
		Title:            "HTTP E2E Test",
		TeamID:           team.ID,
		TeamNameSnapshot: team.Name,
		Severity:         "critical",
		Alerts:           []model.Alert{{Fingerprint: "fp-http", Status: "firing"}},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	payload, _ := model.BuildWebhookEventPayload(
		model.OutboxEventFiring, ag, team.Name, "system", "", now,
	)
	eventID := uuid.New().String()
	event := &model.OutboxEvent{
		ID:           eventID,
		EventType:    model.OutboxEventFiring,
		AlertGroupID: agID,
		TeamID:       team.ID,
		Payload:      payload,
	}
	if err := s.CreateAlertGroupAtomic(ag, []*model.TimelineEvent{
		{
			ID:           uuid.New().String(),
			AlertGroupID: agID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created",
			Actor:        "system",
			CreatedAt:    now,
		},
	}, event); err != nil {
		t.Fatalf("CreateAlertGroupAtomic: %v", err)
	}

	// Run worker with short context (one claim cycle)
	sender := outbox.NewHTTPSender(loopbackCIDR)
	worker := outbox.New(s, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	worker.Run(ctx)

	// Verify httptest received exactly 1 request
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("Expected 1 HTTP request, got %d", len(captured))
	}

	req := captured[0]

	// Content-Type
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", ct, "application/json")
	}

	// X-Tokay-Event
	if ev := req.Header.Get("X-Tokay-Event"); ev != string(model.OutboxEventFiring) {
		t.Errorf("X-Tokay-Event: got %q, want %q", ev, model.OutboxEventFiring)
	}

	// X-Tokay-Event-ID
	if eid := req.Header.Get("X-Tokay-Event-ID"); eid == "" {
		t.Error("X-Tokay-Event-ID is empty")
	}

	// X-Tokay-Signature — recompute and verify
	ts := req.Header.Get("X-Tokay-Timestamp")
	sig := req.Header.Get("X-Tokay-Signature")
	if ts == "" || sig == "" {
		t.Fatalf("Missing timestamp (%q) or signature (%q)", ts, sig)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(capturedBody)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if sig != expected {
		t.Errorf("Signature mismatch:\n  got:  %s\n  want: %s", sig, expected)
	}

	// Verify event status
	fetchedEvent, err := s.GetOutboxEventByID(eventID)
	if err != nil {
		t.Fatalf("GetOutboxEventByID: %v", err)
	}
	if fetchedEvent.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status: got %q, want %q", fetchedEvent.Status, model.OutboxEventStatusCompleted)
	}

	// Verify delivery status
	deliveries, err := s.GetDeliveriesByEventID(eventID)
	if err != nil {
		t.Fatalf("GetDeliveriesByEventID: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("Expected 1 delivery, got %d", len(deliveries))
	}
	if deliveries[0].Status != model.OutboxDeliverySent {
		t.Errorf("delivery status: got %q, want %q", deliveries[0].Status, model.OutboxDeliverySent)
	}
}

// TestOutbox_TeamScopeFilteringE2E proves that the worker fans out correctly:
// global + matching team integration receive the event; non-matching team is skipped.
func TestOutbox_TeamScopeFilteringE2E(t *testing.T) {
	setEncryptionKey(t)
	s := testutil.SetupDB(t)

	teamA := testutil.SeedTeam(t, s, "team-scope-a")
	testutil.SeedTeam(t, s, "team-scope-b")

	// 2 httptest servers: one for global, one for team-A
	var muGlobal, muTeamA sync.Mutex
	var globalReqs, teamAReqs int

	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		muGlobal.Lock()
		globalReqs++
		muGlobal.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer globalSrv.Close()

	teamASrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		muTeamA.Lock()
		teamAReqs++
		muTeamA.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer teamASrv.Close()

	// Create 3 integrations
	scopeGlobal := model.WebhookScopeGlobal
	scopeTeam := model.WebhookScopeTeam
	teamAID := teamA.ID
	teamBID := "team-scope-b"

	integrations := []struct {
		name   string
		scope  *model.WebhookScope
		teamID *string
		url    string
	}{
		{"Global Webhook", &scopeGlobal, nil, globalSrv.URL},
		{"Team A Webhook", &scopeTeam, &teamAID, teamASrv.URL},
		{"Team B Webhook", &scopeTeam, &teamBID, "http://127.0.0.1:1/should-not-be-called"},
	}

	var integIDs []string
	for _, i := range integrations {
		cfg, _ := json.Marshal(model.GenericWebhookConfig{URL: i.url, Secret: "s"})
		integ := &model.Integration{
			ID:      uuid.New().String(),
			Type:    model.IntegrationTypeGenericWebhook,
			Name:    i.name,
			Enabled: true,
			Scope:   i.scope,
			TeamID:  i.teamID,
			Config:  cfg,
		}
		if err := s.CreateIntegration(integ); err != nil {
			t.Fatalf("CreateIntegration(%s): %v", i.name, err)
		}
		integIDs = append(integIDs, integ.ID)
	}

	// Create outbox event for team-A
	agID := uuid.New().String()
	now := time.Now()
	ag := &model.AlertGroup{
		ID:               agID,
		DedupKey:         "scope-filter-dedup",
		Status:           model.AlertGroupStatusTriggered,
		Title:            "Scope Filter Test",
		TeamID:           teamA.ID,
		TeamNameSnapshot: teamA.Name,
		Severity:         "warning",
		Alerts:           []model.Alert{{Fingerprint: "fp-scope", Status: "firing"}},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	payload, _ := model.BuildWebhookEventPayload(
		model.OutboxEventFiring, ag, teamA.Name, "system", "", now,
	)
	eventID := uuid.New().String()
	event := &model.OutboxEvent{
		ID:           eventID,
		EventType:    model.OutboxEventFiring,
		AlertGroupID: agID,
		TeamID:       teamA.ID,
		Payload:      payload,
	}
	if err := s.CreateAlertGroupAtomic(ag, []*model.TimelineEvent{
		{
			ID:           uuid.New().String(),
			AlertGroupID: agID,
			Type:         model.TimelineEventCreated,
			Message:      "Alert group created",
			Actor:        "system",
			CreatedAt:    now,
		},
	}, event); err != nil {
		t.Fatalf("CreateAlertGroupAtomic: %v", err)
	}

	// Run worker
	sender := outbox.NewHTTPSender(loopbackCIDR)
	worker := outbox.New(s, sender)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	worker.Run(ctx)

	// Assert: globalSrv received 1 request
	muGlobal.Lock()
	if globalReqs != 1 {
		t.Errorf("Global server: got %d requests, want 1", globalReqs)
	}
	muGlobal.Unlock()

	// Assert: teamASrv received 1 request
	muTeamA.Lock()
	if teamAReqs != 1 {
		t.Errorf("Team A server: got %d requests, want 1", teamAReqs)
	}
	muTeamA.Unlock()

	// Assert: no delivery created for team-B integration
	deliveries, err := s.GetDeliveriesByEventID(eventID)
	if err != nil {
		t.Fatalf("GetDeliveriesByEventID: %v", err)
	}

	teamBIntegID := integIDs[2]
	for _, d := range deliveries {
		if d.IntegrationID == teamBIntegID {
			t.Error("Delivery should not exist for team-B integration")
		}
	}

	// Assert: exactly 2 deliveries total
	if len(deliveries) != 2 {
		t.Errorf("Expected 2 deliveries, got %d", len(deliveries))
		for _, d := range deliveries {
			t.Logf("  delivery %s -> integration %s, status=%s", d.ID, d.IntegrationID, d.Status)
		}
	}

	// Assert: event completed
	fetchedEvent, err := s.GetOutboxEventByID(eventID)
	if err != nil {
		t.Fatalf("GetOutboxEventByID: %v", err)
	}
	if fetchedEvent.Status != model.OutboxEventStatusCompleted {
		t.Errorf("event status: got %q, want %q", fetchedEvent.Status, model.OutboxEventStatusCompleted)
	}
}

// verifyHeader is a helper to check a specific header value.
func verifyHeader(t *testing.T, req *http.Request, key, want string) {
	t.Helper()
	got := req.Header.Get(key)
	if !strings.Contains(got, want) {
		t.Errorf("Header %s: got %q, want to contain %q", key, got, want)
	}
}
