//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/config"
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
	S      *store.Store
	Ing    *ingester.Ingester
	Eng    *engine.Engine
	Worker *outbound.Worker

	// Handoff is the second family's worker. Announcements are claimed by their
	// own partition, so nothing delivers one unless this is running.
	Handoff *outbound.Worker

	Channel *recordingChannel
	Echo    *echo.Echo

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

	// 1b. Create escalation policies in DB: the producer reads them to plan.
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
	eng := engine.NewEngine(s, renderer, &testSettings{}, cfg)
	// The engine admits commitments and the outbound workers send them. The
	// channel below stands in for Slack, and resolves an address by taking the
	// recipient at its word - what these tests are about is who was promised
	// and who was sent to, not how a user id becomes a Slack account.
	channel := &recordingChannel{identity: storeIdentity(s)}
	channels := map[string]outbound.Channel{"slack": channel, "telegram": channel}
	worker := outbound.NewWorker(s, "integration-worker", channels)
	// The second family's worker, over the same channels. A shift change is
	// delivered by its own pool, which is the whole point of the partition, so
	// a test that only ran the paging worker would watch announcements sit in
	// the queue for ever.
	handoff, err := outbound.NewWorkerFor(outbound.FamilyHandoff, s,
		"integration-handoff-worker", channels)
	if err != nil {
		t.Fatalf("build the handover worker: %v", err)
	}

	// Echo
	e := echo.New()
	ing.RegisterRoutes(e)

	return &IntegrationTestEnv{
		S:         s,
		Ing:       ing,
		Eng:       eng,
		Worker:    worker,
		Handoff:   handoff,
		Channel:   channel,
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

// waitForDeliveries waits until an alert group's escalation has settled: every
// commitment it admitted has reached an outcome.
//
// It replaces waiting on job steps. An escalation is no longer a job - it is a
// set of promises with their own lifecycle - and "the stage finished" has no
// meaning in it.
func waitForDeliveries(t *testing.T, s *store.Store, alertKey string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var settled int
		err := s.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents i
			JOIN alert_groups ag ON ag.id = i.alert_group_id
			WHERE ag.alert_key = $1
			  AND i.status IN ('succeeded', 'idle', 'permanent_failed', 'expired', 'canceled')`,
			alertKey).Scan(&settled)
		if err == nil && settled >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	var admitted, settled int
	_ = s.GetDB().QueryRow(`
		SELECT count(*), count(*) FILTER (WHERE i.status NOT IN ('pending', 'sending'))
		FROM outbound_intents i JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = $1`, alertKey).Scan(&admitted, &settled)
	t.Fatalf("timeout waiting for %d deliveries of %s: %d admitted, %d settled",
		want, alertKey, admitted, settled)
}

// startOutboundWorker runs the delivery worker and returns the function that
// stops it.
//
// Stopping WAITS - it cancels, then joins the loop, which drains what is in
// flight. A test that only cancelled would leave attempts finishing against the
// database while the next test truncates it, and the failure that produces
// lands somewhere else entirely.
func startOutboundWorker(t *testing.T, worker *outbound.Worker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runOutboundWorker(ctx, worker)
		worker.Drain()
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-stopped
		})
	}
	t.Cleanup(stop)
	return stop
}

// startOutboundWorkerAtItsOwnPace runs the worker the way production runs it -
// Run(), on policy.ClaimInterval - and returns the function that stops it.
//
// The helper above ticks fifty times a second so that a test about WHAT is
// delivered does not spend its life asleep. A test about HOW LONG delivery
// takes cannot use it: the interval is part of the answer, and driven at fifty
// milliseconds the queue drains at a rate no deployment has. For the handover
// family the difference is two orders of magnitude, and a profile measured that
// way stays green through any change to the real number.
func startOutboundWorkerAtItsOwnPace(t *testing.T, worker *outbound.Worker) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		worker.Run(ctx)
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-stopped
		})
	}
	t.Cleanup(stop)
	return stop
}

// runOutboundWorker drives the delivery worker until the context is cancelled.
//
// Its own tick rather than Run(): the worker's interval is a second, and a test
// that waited whole seconds for each delivery would spend most of its life
// asleep.
func runOutboundWorker(ctx context.Context, worker *outbound.Worker) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			worker.Tick(ctx)
		}
	}
}

// recordingChannel is Slack as these tests need it: it takes the recipient at
// its word, always succeeds, and remembers where it was asked to send.
type recordingChannel struct {
	identity providers.IdentityLookup

	mu      sync.Mutex
	targets []string
	bytes   []string

	// answer decides what the provider says for one address, so a test can
	// make a single recipient fail without touching the others. Nil, or an
	// address that is not in it, still means "accepted".
	answer map[string]outbound.Result
	// answerErr does the same for a transport failure - a call that never got
	// an answer at all.
	answerErr map[string]error

	// holdKind, when set, narrows hold to one kind of claim, so a test can jam
	// one family's pool and watch the other keep working.
	holdKind keys.Kind

	// delay is how long every call takes to answer. A provider that answers
	// instantly makes a pool look infinite: what a slot costs is the whole
	// exchange, and a test about how long a queue takes has to pay it.
	delay time.Duration

	// hold keeps every call inside the provider until it is closed, which is
	// how a test looks at a worker with its slots genuinely occupied.
	hold chan struct{}
}

// Hold makes every call block inside the provider until the returned function
// is called. Without it a "slow provider" is not slow at all: an error returns
// immediately and the slot is free again before anybody can look at it.
// AnswerIn makes every call take that long, which is what a slot actually
// costs.
func (c *recordingChannel) AnswerIn(d time.Duration) {
	c.mu.Lock()
	c.delay = d
	c.mu.Unlock()
}

// HoldKind is Hold, narrowed to one kind of claim.
func (c *recordingChannel) HoldKind(kind keys.Kind) func() {
	c.mu.Lock()
	c.holdKind = kind
	c.mu.Unlock()
	release := c.Hold()
	return func() {
		release()
		c.mu.Lock()
		c.holdKind = ""
		c.mu.Unlock()
	}
}

func (c *recordingChannel) Hold() func() {
	c.mu.Lock()
	c.hold = make(chan struct{})
	held := c.hold
	c.mu.Unlock()

	var once sync.Once
	return func() { once.Do(func() { close(held) }) }
}

// FailWith makes this channel answer one address the way a provider would when
// it refuses.
func (c *recordingChannel) FailWith(address string, res outbound.Result, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answer == nil {
		c.answer = map[string]outbound.Result{}
		c.answerErr = map[string]error{}
	}
	c.answer[address] = res
	c.answerErr[address] = err
}

// StopFailing makes the address succeed again, which is how a test shows a
// commitment surviving until the provider recovers.
func (c *recordingChannel) StopFailing(address string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.answer, address)
	delete(c.answerErr, address)
}

func (c *recordingChannel) Prepare(ctx context.Context, intent outbound.Intent) outbound.Preparation {
	// The payload is read first, exactly as every real channel reads it, and in
	// the shape its KIND says it is in: a channel cannot write a message
	// without decoding what it is supposed to say, and it has no way to tell "a
	// shape I have no decoder for" from "a shape nobody can read". A double
	// that skipped this would be a double that never produces the one answer
	// the domain has to overrule.
	switch intent.KeyKind {
	case keys.KindHandoff:
		if _, err := keys.DecodeHandoffPayloadV1(
			intent.PayloadSchemaVersion, intent.Payload); err != nil {
			return outbound.Impossible("payload_unreadable", err.Error())
		}
	default:
		if _, err := keys.DecodeEscalationPayloadV1(
			intent.PayloadSchemaVersion, intent.Payload); err != nil {
			return outbound.Impossible("payload_unreadable", err.Error())
		}
	}

	if intent.TargetKind != keys.TargetUser {
		return outbound.Ready(intent.TargetRef)
	}
	// A person is resolved to the address their provider knows them by, which
	// is the half of preparation these tests are about: the plan promises a
	// user, and what reaches the provider is their Slack account.
	address, err := c.identity(ctx, intent.TargetRef, intent.Provider)
	switch {
	case errors.Is(err, providers.ErrNotLinked):
		return outbound.Impossible("identity_not_linked", intent.TargetRef+" has no account here")
	case err != nil:
		return outbound.NotNow("identity_lookup_failed", err.Error())
	}
	return outbound.Ready(address)
}

func (c *recordingChannel) ExecuteAttempt(_ context.Context,
	call outbound.Call) (outbound.Result, error) {

	c.mu.Lock()
	c.targets = append(c.targets, call.Endpoint)
	// What the provider was actually asked to send, so a test can prove two
	// attempts of one commitment carried the same message. The digest is of the
	// whole content rather than one field of it: the alerts are what a group's
	// row changes between attempts, and a title would not notice.
	c.bytes = append(c.bytes, fmt.Sprintf("%x|%s", call.Content.Digest(), call.Payload))
	sent := len(c.targets)
	told, failing := c.answer[call.Endpoint]
	toldErr := c.answerErr[call.Endpoint]
	hold := c.hold
	if c.holdKind != "" && call.KeyKind != c.holdKind {
		hold = nil
	}
	delay := c.delay
	c.mu.Unlock()

	if hold != nil {
		<-hold
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	if failing {
		return told, toldErr
	}

	// A change answers about the message it was given, never about a new one.
	// A real provider that did otherwise would be moving somebody's card to
	// another message, and the domain treats it as exactly that.
	name := fmt.Sprintf("%s/%d", call.Endpoint, sent)
	if call.AttemptKind == outbound.AttemptMutation {
		name = call.ReceiptRef
	}
	receipt, err := outbound.NewReceipt(name,
		json.RawMessage(fmt.Sprintf(`{"channel":%q,"name":%q}`, call.Endpoint, name)))
	if err != nil {
		return outbound.Result{}, err
	}
	return outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "ok", Receipt: receipt,
	}, nil
}

func (c *recordingChannel) ClassifyResponse(res outbound.Result) (outbound.Classification, bool) {
	switch res.Status {
	case "ok":
		return outbound.Classification{Outcome: outbound.OutcomeAccepted}, true
	case "rate_limited":
		return outbound.Classification{
			Outcome: outbound.OutcomeRetryableRejection, Class: "rate_limited",
		}, true
	case "channel_not_found":
		return outbound.Classification{
			Outcome: outbound.OutcomePermanentRejection, Class: "channel_not_found",
		}, true
	}
	return outbound.Classification{}, false
}

// SentTo is what this channel was asked to send to ONE address, in order.
// Comparing two calls only means something when they are the same commitment.
func (c *recordingChannel) SentTo(address string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for i, target := range c.targets {
		if target == address {
			out = append(out, c.bytes[i])
		}
	}
	return out
}

// SentTargets is where this channel was asked to send, in order.
func (c *recordingChannel) SentTargets() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.targets...)
}

func (c *recordingChannel) SentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.targets)
}

// storeIdentity is how a person becomes an address: the same lookup the wiring
// hands to a real channel.
func storeIdentity(s *store.Store) providers.IdentityLookup {
	return func(_ context.Context, userID, provider string) (string, error) {
		identity, err := s.GetExternalIdentity(userID, provider)
		if errors.Is(err, sql.ErrNoRows) {
			return "", providers.ErrNotLinked
		}
		if err != nil {
			return "", err
		}
		if identity == nil || identity.ExternalID == "" {
			return "", providers.ErrNotLinked
		}
		return identity.ExternalID, nil
	}
}

// testSettings is the channel configuration a plan freezes.
type testSettings struct{}

func (testSettings) GetSlackInteractive() bool    { return true }
func (testSettings) GetTelegramInteractive() bool { return true }

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

	// 3. Delivery
	startOutboundWorker(t, env.Worker)

	// Unified escalation job: firehose is step 0, policy step is step 1
	waitForDeliveries(t, env.S, "test_dedup_1", 2)

	if env.Channel.SentCount() < 2 {
		t.Errorf("Expected at least 2 notifications (Firehose + Policy DM), got %d", env.Channel.SentCount())
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
	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_partial_1", 2)

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

	// The ingester records alerts and does not move the status: transitions
	// belong to the engine and to the people acting on the alert.
	active, _ := env.S.GetActiveAlertGroupByAlertKey("test_partial_1")
	if active.Status != model.AlertGroupStatusTriggered {
		t.Errorf("Expected status to stay triggered, got %s", active.Status)
	}
	if len(active.Alerts) != 2 {
		t.Errorf("the incident holds %d alerts, want both", len(active.Alerts))
	}

	// And the card is told, in the same commit that recorded them. It used to
	// be a flag for a loop to notice; it is a revision now, and the commitment
	// that made the card is aimed at it.
	var revision int64
	if err := env.S.GetDB().QueryRow(
		`SELECT revision FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		active.ID).Scan(&revision); err != nil {
		t.Fatalf("read the state the card has to show: %v", err)
	}
	if revision == 0 {
		t.Error("an alert cleared and another joined, and the card was told nothing")
	}

	var aimed int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_intents
		WHERE alert_group_id = $1 AND form = 'editable' AND desired_revision = $2`,
		active.ID, revision).Scan(&aimed); err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	if aimed == 0 {
		t.Error("no card is aimed at the revision the alerts produced")
	}
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
	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_resolve_1", 2)

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

	// A resolved alert owes nobody anything: no page goes out about an alert
	// that is already over.
	waitForNothingOwed(t, env.S, "test_resolve_1")

	// And it stays resolved. Closing it was the resolution loop's last act,
	// and that loop is gone: "resolution delivered" is a fact about a
	// commitment now, kept in its own journal, and the two statuses rendered
	// the same anyway.
	env.S.GetDB().QueryRow("SELECT status FROM alert_groups WHERE alert_key = $1", "test_resolve_1").Scan(&status)
	if status != string(model.AlertGroupStatusResolved) {
		t.Errorf("Expected status Resolved, got %s", status)
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

	// 3. The outbound worker sends what the admission promised.
	initialSentCount := env.Channel.SentCount()
	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_firehose_only_1", 1)

	// Verify firehose notification was sent
	if env.Channel.SentCount() <= initialSentCount {
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

// TestPipeline_ResolutionAllDeliveries: an alert with two commitments under it,
// resolved. What it holds today is that the group owes nobody anything and
// stays resolved - no job resolves anything, and each card is brought to the
// resolving revision by the worker that made it.
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
	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_resolve_all_1", 2)

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

	// 4. A resolved alert owes nobody anything.
	waitForNothingOwed(t, env.S, "test_resolve_all_1")

	var status string
	env.S.GetDB().QueryRow("SELECT status FROM alert_groups WHERE alert_key = $1", "test_resolve_all_1").Scan(&status)
	if status != string(model.AlertGroupStatusResolved) {
		t.Errorf("Expected status Resolved, got %s", status)
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

	initialSentCount := env.Channel.SentCount()

	startOutboundWorker(t, env.Worker)

	// Stage 0: firehose, Stage 1: fan-out DMs (L1 + L2 if resolved)
	waitForDeliveries(t, env.S, "test_fanout_1", 2)

	// Should have sent at least 2: firehose + at least 1 DM
	if env.Channel.SentCount()-initialSentCount < 2 {
		t.Errorf("Expected at least 2 notifications (firehose + DM), got %d", env.Channel.SentCount()-initialSentCount)
	}

	// The firehose and one promise per person on call, and both settled. There
	// are no stages any more: commitments do not hold each other up, which is
	// what the fan-out was working around with continue_on_failure.
	promises := promisedTargets(t, env.S, "test_fanout_1")
	if len(promises) < 2 {
		t.Errorf("the escalation promised %v, want the firehose and at least one person", promises)
	}
}

// promisedTargets is who an alert group's escalation promised to reach, in the
// order the commitments were admitted.
func promisedTargets(t *testing.T, s *store.Store, alertKey string) []string {
	t.Helper()
	rows, err := s.GetDB().Query(`
		SELECT i.target_ref FROM outbound_intents i
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = $1 ORDER BY i.idempotency_key`, alertKey)
	if err != nil {
		t.Fatalf("read the promises of %s: %v", alertKey, err)
	}
	defer rows.Close()

	var targets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatalf("read a promise: %v", err)
		}
		targets = append(targets, target)
	}
	return targets
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

	// One group with both users - both should be on-call simultaneously
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

	initialSentCount := env.Channel.SentCount()

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_multi_fanout_1", 2)

	// Both Slack IDs must have received DMs
	if env.Channel.SentCount()-initialSentCount < 3 {
		t.Errorf("Expected at least 3 notifications (firehose + 2 DMs), got %d",
			env.Channel.SentCount()-initialSentCount)
	}
	targets := env.Channel.SentTargets()
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

	// Override for U_BOB covering the current instant - the projection overlays
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

	initialSentCount := env.Channel.SentCount()

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_override_group_1", 2)

	// Only S_BOB should have received a DM, not S_ALICE
	targets := env.Channel.SentTargets()
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

	initialSentCount := env.Channel.SentCount()

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_no_l2_additive_1", 2)

	// S_L2 must NOT appear in sent targets
	targets := env.Channel.SentTargets()
	for _, tgt := range targets[initialSentCount:] {
		if tgt == "S_L2" {
			t.Errorf("S_L2 must not receive DM (L2 not additive in schedule fan-out), targets: %v", targets)
		}
	}
}

// TestPipeline_UndeliverableProviderIsNotPromised: a policy step naming a
// provider this build has no channel for.
//
// It used to be resolved at execution time and fail there. Nothing resolves a
// provider any more: the admission gate asks whether this build delivers
// through it at all, and a step it cannot deliver is dropped before the promise
// exists - because a promise nothing can keep is a commitment whose every
// attempt fails, and refusing the whole escalation instead would take this
// alert's firehose down with it on every tick, forever.
func TestPipeline_UndeliverableProviderIsNotPromised(t *testing.T) {
	env := setupIntegrationTest(t)

	// A policy with one non-COF DM step on a provider that is not a channel
	// here.
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

	startOutboundWorker(t, env.Worker)

	// The firehose still goes out. A step naming a provider nothing here
	// delivers through is not promised at all: promised, it would be a
	// commitment whose every attempt fails, and refused at the gate it would
	// take the whole escalation - including this firehose - down with it on
	// every tick, forever.
	waitForDeliveries(t, env.S, "test_disabled_1", 1)

	var promised int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_intents i
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = 'test_disabled_1' AND i.provider = 'blocked'`).
		Scan(&promised); err != nil {
		t.Fatalf("count the promises: %v", err)
	}
	if promised != 0 {
		t.Errorf("%d commitments were made for a provider with no channel", promised)
	}

	// And the alert's history says why, which is the whole point of not
	// promising it: somebody has to be able to find out.
	events, err := env.S.GetTimelineEvents(agIDForKey(t, env.S, "test_disabled_1"))
	if err != nil {
		t.Fatalf("read the timeline: %v", err)
	}
	var explained bool
	for _, event := range events {
		if strings.Contains(event.Message, "Escalation step") &&
			strings.Contains(event.Message, "blocked") {
			explained = true
		}
	}
	if !explained {
		t.Error("nothing in the alert's history says the step could not be delivered")
	}
}

// agIDForKey is the alert group behind an alert key.
func agIDForKey(t *testing.T, s *store.Store, alertKey string) string {
	t.Helper()
	ag, err := s.GetActiveAlertGroupByAlertKey(alertKey)
	if err != nil {
		t.Fatalf("get the alert group of %s: %v", alertKey, err)
	}
	return ag.ID
}

// TestPipeline_ChannelUpdate verifies a policy CHANNEL step produces an editable
// (supports_update=true) delivery and a timeline event - the "Escalation channel"
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

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_channel_update_1", 2)

	// The policy channel step promised an editable card - one the alert can
	// bring to a later revision - and the delivery recorded where it went,
	// which is what makes that possible.
	var form, receipt string
	if err := env.S.GetDB().QueryRow(`
		SELECT i.form, COALESCE(i.receipt::text, '')
		FROM outbound_intents i
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = $1 AND i.target_ref = 'C_POLICY_CHAN'`,
		"test_channel_update_1").Scan(&form, &receipt); err != nil {
		t.Fatalf("read the channel commitment: %v", err)
	}
	if form != "editable" {
		t.Errorf("the channel card is %q", form)
	}
	if receipt == "" {
		t.Error("the delivered card does not say where it is")
	}

	// A timeline event specific to the CHANNEL step must exist - assert on its
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
	// The line has to say WHICH delivery it is about. An alert with a firehose
	// card and several direct messages produces several lines that would
	// otherwise read identically, and telling them apart is usually the reason
	// somebody is reading the history at all.
	found := false
	for _, ev := range events {
		if ev.Type == model.TimelineEventNotificationSent &&
			ev.Metadata["provider"] == "slack" &&
			ev.Metadata["target_ref"] == "C_POLICY_CHAN" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a notification-sent line naming the channel delivery "+
			"(provider=slack, target_ref=C_POLICY_CHAN); got %d events", len(events))
	}
}

// TestPipeline_EscalationUnlinked verifies a DM step to a user WITHOUT a linked
// Slack identity fails permanently (no retry) end-to-end - the pipeline-level
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

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "test_unlinked_1", 2)

	// Nobody's account links itself, so the commitment ends where a person can
	// see it rather than retrying for as long as the alert lives.
	var status, errorClass string
	var calls int
	if err := env.S.GetDB().QueryRow(`
		SELECT i.status,
		       COALESCE((SELECT a.error_class FROM outbound_attempts a
		                 WHERE a.intent_id = i.id ORDER BY a.attempt_no DESC LIMIT 1), ''),
		       (SELECT count(*) FROM outbound_attempts a
		        WHERE a.intent_id = i.id AND a.record_kind = 'attempt')
		FROM outbound_intents i
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = 'test_unlinked_1' AND i.target_ref = 'U_UNLINKED'`).
		Scan(&status, &errorClass, &calls); err != nil {
		t.Fatalf("read the commitment: %v", err)
	}
	if status != "permanent_failed" {
		t.Errorf("an unlinked recipient left the commitment %q", status)
	}
	if errorClass != "identity_not_linked" {
		t.Errorf("the journal says %q rather than why nobody could be reached", errorClass)
	}
	// A refusal leaves proof, not doubt: no call was made at all.
	if calls != 0 {
		t.Errorf("%d calls were recorded for a recipient with no account", calls)
	}
	for _, tgt := range env.Channel.SentTargets() {
		if tgt == "U_UNLINKED" {
			t.Error("the channel was asked to send to an unlinked recipient")
		}
	}
}

// TestPipeline_CancelDuringExecution tests the real ack-driven cancellation path.
//
// The cancellation happens inside AckAlertGroupAtomic itself, in the same
// transaction as the status change. There is no later pass to credit it to any
// more: the loop that used to run one is gone.
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
				DelaySeconds:   300, // 5 min delay - step won't run before ack
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

	startOutboundWorker(t, env.Worker)

	// Wait for firehose (stage 0) to complete - DM step (stage 1) is delayed, still pending
	waitForDeliveries(t, env.S, "test_cancel_exec_1", 1)

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

	// The delayed page is withdrawn. It had not gone out - it was five minutes
	// away - so acknowledging the alert takes it back rather than letting it
	// wake somebody about an alert that is already handled.
	//
	// The firehose card is owed one more thing at this moment and that is not a
	// failure: acknowledging an alert moves what its card has to show, so the
	// card goes back into the queue to say so and settles again once the worker
	// has applied it. Waited for rather than slept through, because how long
	// that takes is the worker's business and not this test's.
	var withdrawn, owing int
	until(t, "the withdrawal and the acknowledged card to settle", func() bool {
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FILTER (WHERE i.status = 'canceled'),
			       count(*) FILTER (WHERE i.status IN ('pending', 'sending'))
			FROM outbound_intents i
			JOIN alert_groups ag ON ag.id = i.alert_group_id
			WHERE ag.alert_key = 'test_cancel_exec_1'`).Scan(&withdrawn, &owing); err != nil {
			t.Fatalf("read the commitments: %v", err)
		}
		return withdrawn == 1 && owing == 0
	})

	// Verify AG status is acknowledged
	updatedAG, _ := env.S.GetAlertGroupByID(ag.ID)
	if updatedAG.Status != model.AlertGroupStatusAcknowledged {
		t.Errorf("Expected AG acknowledged, got %s", updatedAG.Status)
	}
}

// waitForNothingOwed waits until an alert group has no commitment left in
// flight: everything it promised has either gone out or been withdrawn.
func waitForNothingOwed(t *testing.T, s *store.Store, alertKey string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var owing int
		err := s.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents i
			JOIN alert_groups ag ON ag.id = i.alert_group_id
			WHERE ag.alert_key = $1 AND i.status IN ('pending', 'sending')`,
			alertKey).Scan(&owing)
		if err == nil && owing == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s still owes deliveries after it was resolved", alertKey)
}
