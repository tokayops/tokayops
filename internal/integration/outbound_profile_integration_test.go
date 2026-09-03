//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	webhookprovider "github.com/tokayops/tokayops/internal/outbound/providers/webhook"
	"github.com/tokayops/tokayops/internal/testutil"
)

// The paging profile, measured rather than reasoned about, and the two claims
// the webhook family makes beside it.
//
// None of these is in the ordinary run: each takes minutes, and the minutes
// are the point. Set TOKAY_PROFILE_SLO=1 to run them (make test-profile). What
// is modelled is occupancy - a slot is taken for the whole exchange - and the
// workers run on their own intervals, because how often the queue is looked at
// is half of what a profile measures.
//
// The paging profile (D7): up to 20 groups a minute, up to 10 commitments a
// group, one instance, a pool of 8, providers answering under a second; p95 of
// the admission latency at or under 5s, p99 at or under 10s. The webhook
// family's two profiles (D4) are computed on different assumptions and are
// tested apart: a steady state of 140 live commitments to dead addresses
// occupies 43% of its pool on average and must not slow paging, and a burst of
// 80 becoming due at once on a FREE pool is claimed in thirteen rounds, the
// last near 386s, and does not hold the lateness gauge over the rule's
// threshold for anything like the rule's window. Added together they would
// not meet either number, and no test says they do.

const (
	profileMinutes  = 5
	profileGroups   = 20 * profileMinutes
	profileInterval = time.Minute / 20
	// profileSteps is nine people over two providers; the critical firehose
	// channel makes the tenth commitment of every group.
	profileSteps       = 9
	profilePerGroup    = profileSteps + 1
	profileCommitments = profileGroups * profilePerGroup

	// The webhook family's steady state and burst, from D4.
	deadSubscribers = 20
	steadyEvents    = 7 // 140 live commitments
	burstEvents     = 4 // 80 due at once
)

func profileEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("TOKAY_PROFILE_SLO") == "" {
		t.Skip("set TOKAY_PROFILE_SLO=1: this one runs for minutes on purpose")
	}
}

// profileTeam is a team whose critical alerts page nine people over two
// providers, plus the firehose channel: ten commitments a group, all due at
// once, which is the profile's upper bound.
func profileTeam(t *testing.T, env *IntegrationTestEnv) {
	t.Helper()
	steps := make([]*model.EscalationStep, 0, profileSteps)
	for i := 1; i <= profileSteps; i++ {
		userID := fmt.Sprintf("U_PROFILE_%d", i)
		if err := env.S.CreateUser(&model.User{
			ID: userID, Email: fmt.Sprintf("profile-%d@pipeline.test", i), Name: userID,
		}); err != nil {
			t.Fatalf("create %s: %v", userID, err)
		}
		provider := "slack"
		if i > 5 {
			provider = "telegram"
		}
		testutil.BindIdentity(t, env.S, userID, provider, "X_"+userID)
		steps = append(steps, &model.EscalationStep{
			ID: fmt.Sprintf("profile_step_%d", i), PolicyID: "profile_policy", StepIndex: i - 1,
			Provider: provider, TargetKind: "dm", TargetType: "user", TargetID: userID,
			DelaySeconds: 0, TimeoutSeconds: 10, MaxAttempts: 1,
		})
	}
	if err := env.S.CreateEscalationPolicy(&model.EscalationPolicy{
		ID: "profile_policy", Name: "Profile Policy", Steps: steps,
	}); err != nil {
		t.Fatalf("create the profile policy: %v", err)
	}
	if err := env.S.CreateTeam(&model.Team{
		ID: "profile", Name: "Profile Team", DefaultPolicyID: "profile_policy",
		SeverityRoutes: map[string]string{"critical": "profile_policy"},
	}); err != nil {
		t.Fatalf("create the profile team: %v", err)
	}
}

func profileAlert(key string) string {
	return fmt.Sprintf(`{
		"groupKey": %q,
		"status": "firing",
		"commonLabels": {"team": "profile", "severity": "critical", "alertname": "ProfileAlert"},
		"alerts": [{"fingerprint": "fp1", "status": "firing", "labels": {"alertname": "ProfileAlert"}}]
	}`, key)
}

// pageAtTheProfileRate fires one group every three seconds for the profile's
// length, admitting each the way the engine's tick would. The admission time
// is the database's, so the generator's own pacing is not in the measurement.
func pageAtTheProfileRate(t *testing.T, env *IntegrationTestEnv) {
	t.Helper()
	ctx := context.Background()
	next := time.Now()
	for i := 0; i < profileGroups; i++ {
		sendWebhook(t, env.Echo, profileAlert(fmt.Sprintf("profile-%03d", i)))
		env.Eng.ProcessNewAlertGroups(ctx)
		next = next.Add(profileInterval)
		time.Sleep(time.Until(next))
	}
}

// untilPagingSettled waits for every commitment of the profile to be delivered.
// A direct message ends in succeeded; the firehose card is editable and, once
// it shows the revision it was asked to, rests in idle - delivered, and still
// owned. Both are the delivery having happened; nothing else is.
func untilPagingSettled(t *testing.T, env *IntegrationTestEnv, deadline time.Duration) {
	t.Helper()
	giveUp := time.Now().Add(deadline)
	for {
		var settled int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents
			WHERE delivery_family = 'notification' AND status IN ('succeeded', 'idle')`).Scan(&settled); err != nil {
			t.Fatalf("read the queue: %v", err)
		}
		if settled == profileCommitments {
			return
		}
		if time.Now().After(giveUp) {
			t.Fatalf("%d of %d pages delivered before the test gave up", settled, profileCommitments)
		}
		time.Sleep(time.Second)
	}
}

// notificationProfile is the paging family's admission latency over the run,
// both ends from the database's clock, over exactly the rows the histogram
// observes: first attempts of commitments that were due the moment they were
// admitted.
func notificationProfile(t *testing.T, env *IntegrationTestEnv) (p95, p99, worst float64, measured int) {
	t.Helper()
	if err := env.S.GetDB().QueryRow(`
		SELECT percentile_disc(0.95) WITHIN GROUP (ORDER BY waited),
		       percentile_disc(0.99) WITHIN GROUP (ORDER BY waited),
		       max(waited), count(*)
		FROM (
			SELECT EXTRACT(EPOCH FROM (a.started_at - b.admitted_at))::double precision AS waited
			FROM outbound_attempts a
			JOIN outbound_intents i ON i.id = a.intent_id
			JOIN outbound_batches b ON b.id = i.batch_id
			WHERE i.delivery_family = 'notification' AND a.attempt_no = 1
			  AND i.not_before <= b.admitted_at
		) waits`).Scan(&p95, &p99, &worst, &measured); err != nil {
		t.Fatalf("measure the profile: %v", err)
	}
	return p95, p99, worst, measured
}

// latencyHistogram is the paging family's admission-latency histogram as the
// worker observed it: the sample count and the cumulative counts at the two
// boundaries the SLO is written on. The thresholds are bucket boundaries on
// purpose, so histogram_quantile does not interpolate across them.
type latencyHistogram struct{ count, le5, le10 uint64 }

func readLatencyHistogram(t *testing.T) latencyHistogram {
	t.Helper()
	observer, err := metrics.OutboundAdmissionLatencySeconds.GetMetricWithLabelValues(outbound.FamilyNotification)
	if err != nil {
		t.Fatalf("read the histogram: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("read the histogram: %v", err)
	}
	h := latencyHistogram{count: m.GetHistogram().GetSampleCount()}
	for _, b := range m.GetHistogram().GetBucket() {
		switch b.GetUpperBound() {
		case 5:
			h.le5 = b.GetCumulativeCount()
		case 10:
			h.le10 = b.GetCumulativeCount()
		}
	}
	return h
}

// assertPagingProfile is the SLO itself, asserted twice: from the rows, and
// from the histogram the rules file reads, because a worker that stopped
// observing would leave the first green and the alert blind.
func assertPagingProfile(t *testing.T, env *IntegrationTestEnv, before latencyHistogram, under string) {
	t.Helper()
	p95, p99, worst, measured := notificationProfile(t, env)
	after := readLatencyHistogram(t)
	observed := after.count - before.count
	within5 := float64(after.le5-before.le5) / float64(observed)
	within10 := float64(after.le10-before.le10) / float64(observed)
	t.Logf("paging profile %s over %d groups, %d commitments: p95=%.2fs p99=%.2fs worst=%.2fs; "+
		"histogram: %d observed, %.1f%% within 5s, %.1f%% within 10s",
		under, profileGroups, measured, p95, p99, worst, observed, within5*100, within10*100)

	if measured != profileCommitments {
		t.Errorf("%d first attempts measured, %d commitments were admitted immediately due", measured, profileCommitments)
	}
	if p95 > 5 {
		t.Errorf("p95 = %.2fs, over the 5s the paging family promises", p95)
	}
	if p99 > 10 {
		t.Errorf("p99 = %.2fs, over the 10s the paging family promises", p99)
	}
	if observed != uint64(profileCommitments) {
		t.Errorf("the histogram observed %d first attempts, want %d", observed, profileCommitments)
	}
	if within5 < 0.95 {
		t.Errorf("the histogram puts %.1f%% within 5s; the SLO reads it and needs 95%%", within5*100)
	}
	if within10 < 0.99 {
		t.Errorf("the histogram puts %.1f%% within 10s; the SLO reads it and needs 99%%", within10*100)
	}
}

// blackHole is a subscriber that neither answers nor refuses: it reads the
// request and waits for the client to give up. That is what the webhook
// family's numbers were computed against - a slot held for the whole of the
// subscriber timeout - and a double that answered after ten seconds would pay
// a third of that cost and prove isolation against a backlog nobody described.
type blackHole struct {
	entered atomic.Int32
	// releaseAfter, when set, makes the hole answer after that long - which is
	// no black hole at all, and exists only so the duration assertion can be
	// shown to catch a double that answers too soon.
	releaseAfter time.Duration
}

func (b *blackHole) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.ReadAll(req.Body)
		b.entered.Add(1)
		if b.releaseAfter > 0 {
			select {
			case <-req.Context().Done():
			case <-time.After(b.releaseAfter):
				w.WriteHeader(http.StatusOK)
			}
			return
		}
		<-req.Context().Done()
	}))
	t.Cleanup(func() {
		// The handlers end when the client hangs up; the cleanup hangs up for
		// every client still waiting rather than sitting in Close for the
		// length of an attempt deadline.
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

// webhookFamilyOver is the third family's worker and producer over the same
// database as the paging test, wired the way cmd/tokayops wires them.
func webhookFamilyOver(t *testing.T, env *IntegrationTestEnv) (*outbound.Worker, *outbound.FanOut, outbound.Channel) {
	t.Helper()
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	channel := webhookprovider.NewHandler(env.S, []*net.IPNet{loopback})
	worker, err := outbound.NewWorkerFor(outbound.FamilyWebhook, env.S, "profile-webhook-worker",
		map[string]outbound.Channel{keys.ProviderWebhook: channel})
	if err != nil {
		t.Fatalf("build the webhook worker: %v", err)
	}
	fanOut, err := outbound.NewFanOut(env.S)
	if err != nil {
		t.Fatalf("build the fan-out: %v", err)
	}
	return worker, fanOut, channel
}

// deadSubscribersOf subscribes n team-scoped integrations of the given team to
// the black hole, each with the subscriber timeout at its ceiling. Team-scoped
// so that the paging test's own events - which fan out too - find no audience
// and stay out of the webhook family's queue.
func deadSubscribersOf(t *testing.T, env *IntegrationTestEnv, team, url string, n int) {
	t.Helper()
	if err := env.S.CreateTeam(&model.Team{ID: team, Name: "Hooks"}); err != nil {
		t.Fatalf("create the team: %v", err)
	}
	for i := 0; i < n; i++ {
		cfg, _ := json.Marshal(model.GenericWebhookConfig{
			URL: url, Secret: "s3cret", TimeoutSeconds: int(outbound.WebhookMaxSubscriberTimeout / time.Second),
		})
		scope := model.WebhookScopeTeam
		teamID := team
		integration := &model.Integration{
			Type: model.IntegrationTypeGenericWebhook, Name: fmt.Sprintf("dead-%02d", i),
			Enabled: true, Scope: &scope, TeamID: &teamID, Config: cfg,
		}
		if err := env.S.CreateIntegration(integration); err != nil {
			t.Fatalf("subscribe dead-%02d: %v", i, err)
		}
	}
}

// eventsFor writes n alert events of the given team, the way the atomic alert
// transactions write them.
func eventsFor(t *testing.T, env *IntegrationTestEnv, team string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		agID := uuid.New().String()
		if err := env.S.CreateAlertGroup(&model.AlertGroup{
			ID: agID, AlertKey: "hooks-" + agID, Status: model.AlertGroupStatusNew,
			Title: "Disk filling up", Severity: "critical", TeamID: team,
			Alerts: []model.Alert{{Fingerprint: "fp-" + agID, Status: "firing",
				Labels: map[string]string{"alertname": "Disk"}}},
		}); err != nil {
			t.Fatalf("create the group: %v", err)
		}
		if err := env.S.CreateOutboxEvent(&model.OutboxEvent{
			ID: uuid.New().String(), EventType: model.OutboxEventFiring, AlertGroupID: agID,
			TeamID: team, Payload: json.RawMessage(`{"event":"alert_group.firing","alert_group":{"id":"` + agID + `"}}`),
		}); err != nil {
			t.Fatalf("write the event: %v", err)
		}
	}
}

// fanOutAll drives the producer by hand until the given number of webhook
// commitments exist, so that they are all due at the same moment.
func fanOutAll(t *testing.T, env *IntegrationTestEnv, fanOut *outbound.FanOut, want int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		fanOut.Tick(ctx)
		if webhookCommitments(t, env) == want {
			return
		}
	}
	t.Fatalf("%d webhook commitments after fanning out, want %d", webhookCommitments(t, env), want)
}

func webhookCommitments(t *testing.T, env *IntegrationTestEnv) int {
	t.Helper()
	var n int
	if err := env.S.GetDB().QueryRow(
		`SELECT count(*) FROM outbound_intents WHERE delivery_family = 'webhook'`).Scan(&n); err != nil {
		t.Fatalf("count the webhook commitments: %v", err)
	}
	return n
}

// webhookWatch samples the webhook family once a second while a test runs:
// the lateness gauge an alert reads and the number of commitments in flight,
// which is the pool's occupancy as the database sees it. Every sample carries
// the moment it was taken, because what the tests ask is about TIME - how
// long the gauge stayed over a threshold, how much of a window the slots were
// held for - and a ticker promises neither a sample every second nor one at
// all when a scrape runs long.
type webhookSample struct {
	at       time.Time
	late     float64
	reported bool
	inFlight int
}

type webhookWatch struct {
	mu      sync.Mutex
	samples []webhookSample
}

func (w *webhookWatch) run(ctx context.Context, t *testing.T, env *IntegrationTestEnv, registry *prometheus.Registry) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		late, reported := latenessOfFamily(t, registry, outbound.FamilyWebhook)
		var inFlight int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents
			WHERE delivery_family = 'webhook' AND status = 'sending'`).Scan(&inFlight); err != nil {
			t.Errorf("read the webhook queue: %v", err)
			return
		}
		w.mu.Lock()
		w.samples = append(w.samples, webhookSample{at: time.Now(), late: late, reported: reported, inFlight: inFlight})
		w.mu.Unlock()
	}
}

func (w *webhookWatch) snapshot() []webhookSample {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]webhookSample(nil), w.samples...)
}

// lateness is the gauge's peak, how many scrapes reported it, and its range
// over the samples taken after a given moment.
func (w *webhookWatch) lateness(after time.Time) (peak float64, reported int, lowestAfter, highestAfter float64, countAfter int) {
	lowestAfter = -1
	for _, s := range w.snapshot() {
		if !s.reported {
			continue
		}
		reported++
		if s.late > peak {
			peak = s.late
		}
		if !s.at.After(after) {
			continue
		}
		countAfter++
		if lowestAfter < 0 || s.late < lowestAfter {
			lowestAfter = s.late
		}
		if s.late > highestAfter {
			highestAfter = s.late
		}
	}
	return peak, reported, lowestAfter, highestAfter, countAfter
}

// longestOver is the longest CONTIGUOUS stretch of time the gauge spent at or
// over the threshold, which is what a rule's `for` clause measures: two
// excursions with a dip between them are two short alerts that never fire,
// however long they add up to. Measured between the first sample over and the
// first sample under, by the samples' own clocks.
func longestOver(samples []webhookSample, threshold float64) time.Duration {
	var longest time.Duration
	var since time.Time
	over := false
	for _, s := range samples {
		if !s.reported {
			continue
		}
		switch {
		case s.late >= threshold && !over:
			over, since = true, s.at
		case s.late < threshold && over:
			over = false
			if d := s.at.Sub(since); d > longest {
				longest = d
			}
		}
	}
	if over {
		if d := samples[len(samples)-1].at.Sub(since); d > longest {
			longest = d
		}
	}
	return longest
}

// occupancy integrates the commitments in flight over a window - slot-seconds
// held, against slot-seconds available - and says how many samples the
// integral stands on. A pool that idled for a wave shows it here and nowhere
// else: the maximum in flight says only that eight were held once.
func occupancy(samples []webhookSample, from, to time.Time, pool int) (held, available float64, maxInFlight, counted int) {
	var previous *webhookSample
	for i := range samples {
		s := samples[i]
		if s.at.Before(from) || s.at.After(to) {
			continue
		}
		counted++
		if s.inFlight > maxInFlight {
			maxInFlight = s.inFlight
		}
		if previous != nil {
			held += float64(previous.inFlight) * s.at.Sub(previous.at).Seconds()
		}
		previous = &samples[i]
	}
	available = float64(pool) * to.Sub(from).Seconds()
	return held, available, maxInFlight, counted
}

// longestWebhookAttempt is how long the longest call to a subscriber lasted,
// which is what a dead subscriber costs a slot. Asserted in every test that
// uses the black hole: a double that answers early is a different, cheaper
// profile, and the numbers would be green about the wrong system.
func longestWebhookAttempt(t *testing.T, env *IntegrationTestEnv) float64 {
	t.Helper()
	var longest float64
	if err := env.S.GetDB().QueryRow(`
		SELECT COALESCE(max(EXTRACT(EPOCH FROM (a.finished_at - a.started_at))), 0)::double precision
		FROM outbound_attempts a
		JOIN outbound_intents i ON i.id = a.intent_id
		WHERE i.delivery_family = 'webhook' AND a.finished_at IS NOT NULL`).Scan(&longest); err != nil {
		t.Fatalf("measure the attempts: %v", err)
	}
	return longest
}

func assertTheSubscriberHeldTheSlot(t *testing.T, env *IntegrationTestEnv, hole *blackHole) {
	t.Helper()
	longest := longestWebhookAttempt(t, env)
	floor := outbound.WebhookMaxSubscriberTimeout.Seconds()
	t.Logf("the subscriber was entered %d times; the longest attempt lasted %.1fs", hole.entered.Load(), longest)
	if longest < floor {
		t.Errorf("the longest attempt to the subscriber lasted %.1fs; a dead subscriber holds a slot for %.0fs, "+
			"and a double that answers sooner is a cheaper backlog than the profile describes", longest, floor)
	}
}

// TestPagingMeetsItsProfile is the paging family's SLO on a quiet instance:
// twenty groups a minute for five minutes, ten commitments each, both
// providers answering in under a second.
func TestPagingMeetsItsProfile(t *testing.T) {
	profileEnabled(t)
	env := setupIntegrationTest(t)
	profileTeam(t, env)
	env.Channel.AnswerIn(900 * time.Millisecond)

	before := readLatencyHistogram(t)
	stop := startOutboundWorkerAtItsOwnPace(t, env.Worker)

	pageAtTheProfileRate(t, env)
	untilPagingSettled(t, env, 5*time.Minute)
	stop()

	assertPagingProfile(t, env, before, "on a quiet instance")
}

// TestAWebhookBacklogDoesNotSlowPaging is check 13 of the epic: a webhook
// backlog that fills the family's pool for the whole run - 140 commitments to
// subscribers that never answer, all due at once, which keeps every one of
// the eight slots held from the first tick to the last - and the paging
// profile is met exactly as on a quiet instance. What proves the isolation is
// the separate claim partition; what this test adds to test 22 of Sprint 4 is
// the percentile under the load rather than a claim that returned the right
// rows.
//
// This is harsher than the steady state D4 computes (43% of the pool over a
// day) and harsher than its burst (80): the pool is saturated throughout, and
// isolation is shown against that. Nothing is asserted about the webhook
// family's own lateness here: 140 at once is outside the burst profile, and
// the number it does promise is tested on a free pool below.
func TestAWebhookBacklogDoesNotSlowPaging(t *testing.T) {
	profileEnabled(t)
	env := setupIntegrationTest(t)
	profileTeam(t, env)
	env.Channel.AnswerIn(900 * time.Millisecond)

	hole := &blackHole{}
	deadSubscribersOf(t, env, "hooks", hole.serve(t).URL, deadSubscribers)
	webhookWorker, fanOut, webhookChannel := webhookFamilyOver(t, env)
	eventsFor(t, env, "hooks", steadyEvents)
	fanOutAll(t, env, fanOut, steadyEvents*deadSubscribers)

	// The paging worker of this test carries the webhook channel too. Under
	// the family partition that changes nothing - it is never handed a webhook
	// commitment - and it is what makes the partition the only thing between
	// the backlog and the pages: a claim that ignored the family would find a
	// channel here to hold the slot with.
	paging := outbound.NewWorker(env.S, "profile-paging-worker", map[string]outbound.Channel{
		"slack": env.Channel, "telegram": env.Channel, keys.ProviderWebhook: webhookChannel,
	})

	registry := prometheus.NewRegistry()
	metrics.RegisterCollectorWith(registry, env.S)
	watch := &webhookWatch{}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go watch.run(watchCtx, t, env, registry)

	// The producer keeps running: the pages fan out too, find no audience,
	// and must not be what a stopped fan-out looks like.
	fanCtx, stopFanOut := context.WithCancel(context.Background())
	defer stopFanOut()
	go fanOut.Run(fanCtx)

	before := readLatencyHistogram(t)
	stopWebhook := startOutboundWorkerAtItsOwnPace(t, webhookWorker)
	stopPaging := startOutboundWorkerAtItsOwnPace(t, paging)

	// The window the percentile is measured over, and therefore the window
	// the pool has to be held for: from the first page to the last delivery.
	// The webhook worker started a moment earlier, so the backlog is on the
	// slots before the first page arrives.
	windowStart := time.Now()
	pageAtTheProfileRate(t, env)
	untilPagingSettled(t, env, 5*time.Minute)
	windowEnd := time.Now()
	stopPaging()
	stopFanOut()
	stopWatch()
	stopWebhook()

	assertPagingProfile(t, env, before, "under a saturated webhook backlog")

	samples := watch.snapshot()
	peak, reported, _, _, _ := watch.lateness(windowEnd)
	held, available, maxInFlight, counted := occupancy(samples, windowStart, windowEnd, outbound.WebhookPoolSize)
	share := held / available
	t.Logf("webhook backlog over the %.0fs paging window: %.0f of %.0f slot-seconds held (%.1f%%) on %d samples; "+
		"at most %d in flight; lateness peaked at %.1fs over %d scrapes",
		windowEnd.Sub(windowStart).Seconds(), held, available, share*100, counted, maxInFlight, peak, reported)
	if reported == 0 {
		t.Fatal("the webhook lateness series was never on a scrape")
	}
	if maxInFlight > outbound.WebhookPoolSize {
		t.Errorf("%d webhook commitments in flight at once, the pool is %d", maxInFlight, outbound.WebhookPoolSize)
	}

	// The condition the percentile was measured under, proven for the whole
	// window rather than for one moment of it. A slot is held for the 31s of
	// the subscriber's timeout plus two writes, and freed only at the next
	// claim tick; measured over the window that is about 93% of the
	// slot-seconds (92.7% on the first run), not the 97% a 31-in-32 round
	// would give. 88% is the measurement with room for scrape jitter and not
	// for an idle wave: one wave of eight slots idle for 32s in a 300s window
	// is 11% of it, and a claim that skipped it - a DueSnapshot error is
	// logged and the tick moves on - lands near 82%. The integral needs
	// samples to stand on: four a five-second stretch at the least, or a
	// watcher that stalled would integrate nothing and pass.
	if counted < int(windowEnd.Sub(windowStart).Seconds())*4/5 {
		t.Errorf("only %d samples over a %.0fs window; the occupancy integral has nothing to stand on",
			counted, windowEnd.Sub(windowStart).Seconds())
	}
	if share < 0.88 {
		t.Errorf("the webhook pool was held for %.1f%% of the paging window; the percentile was measured under a saturated pool, and this is not one",
			share*100)
	}
	// And nothing got through a subscriber that never answers.
	var delivered int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_intents WHERE delivery_family = 'webhook' AND status = 'succeeded'`).Scan(&delivered); err != nil {
		t.Fatalf("read the webhook queue: %v", err)
	}
	if delivered != 0 {
		t.Errorf("%d webhook commitments succeeded against a subscriber that never answered", delivered)
	}
	assertTheSubscriberHeldTheSlot(t, env, hole)
}

// TestAWebhookBurstDrainsOnAFreePool is the webhook family's burst profile:
// eighty commitments due at the same moment on an idle pool of eight, each
// holding its slot for the subscriber's whole timeout.
//
// The discrete worker claims the wave in rounds of 32s - a 31s hold aligned to
// the 2s tick - and the rounds are not eight fresh commitments each: an
// attempt to a dead address comes back as a retry two seconds later, and the
// claim gives retries a quarter of the slots (3:1). Two rounds of eight, then
// six fresh a round: thirteen rounds, the last at 2 + 32 * 12 = 386s. The
// first computation of this profile (290s, ten rounds) forgot the retries and
// was wrong; the measurement found 386.1s. The lateness gauge peaks there and
// is over the rule's 300s for about a minute and a half; the rule wants ten
// minutes, which is what tells a burst from a queue that stopped. After the
// wave the gauge is about retries waiting for slots and stays under 300s.
//
// The numbers asserted are the floor plus margin, the way D7 sets every
// threshold: a bound that equals the theoretical minimum is missed by any
// nonzero work. 480s for the last first attempt, 300s of samples over the
// threshold, and every sample after the wave under it.
func TestAWebhookBurstDrainsOnAFreePool(t *testing.T) {
	profileEnabled(t)
	env := setupIntegrationTest(t)

	hole := &blackHole{}
	deadSubscribersOf(t, env, "hooks", hole.serve(t).URL, deadSubscribers)
	webhookWorker, fanOut, _ := webhookFamilyOver(t, env)
	eventsFor(t, env, "hooks", burstEvents)
	fanOutAll(t, env, fanOut, burstEvents*deadSubscribers)
	burst := burstEvents * deadSubscribers

	registry := prometheus.NewRegistry()
	metrics.RegisterCollectorWith(registry, env.S)
	watch := &webhookWatch{}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go watch.run(watchCtx, t, env, registry)

	// No head start: Run waits a full interval before its first tick, which is
	// what a burst looks like from the worker's side.
	stop := startOutboundWorkerAtItsOwnPace(t, webhookWorker)

	// The wave is over when every commitment has had its first attempt
	// started; from then on the gauge is about retries on backoff.
	waveStart := time.Now()
	// A safety net, not a threshold: the profile's own bound is asserted below,
	// and the net is wide enough for a burst half again as big to fail there
	// rather than here.
	giveUp := waveStart.Add(12 * time.Minute)
	for {
		var started int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents i
			WHERE i.delivery_family = 'webhook' AND EXISTS (
				SELECT 1 FROM outbound_attempts a WHERE a.intent_id = i.id AND a.attempt_no = 1)`).Scan(&started); err != nil {
			t.Fatalf("read the queue: %v", err)
		}
		if started == burst {
			break
		}
		if time.Now().After(giveUp) {
			t.Fatalf("%d of %d first attempts started before the test gave up", started, burst)
		}
		time.Sleep(time.Second)
	}
	waveTook := time.Since(waveStart)
	waveOver := time.Now()
	time.Sleep(90 * time.Second)
	stopWatch()
	stop()

	samples := watch.snapshot()
	peak, reported, lowestAfter, highestAfter, countAfter := watch.lateness(waveOver)
	overThreshold := longestOver(samples, 300)
	_, _, maxInFlight, _ := occupancy(samples, waveStart, waveOver, outbound.WebhookPoolSize)
	var worstFirst float64
	if err := env.S.GetDB().QueryRow(`
		SELECT max(EXTRACT(EPOCH FROM (a.started_at - b.admitted_at)))::double precision
		FROM outbound_attempts a
		JOIN outbound_intents i ON i.id = a.intent_id
		JOIN outbound_batches b ON b.id = i.batch_id
		WHERE i.delivery_family = 'webhook' AND a.attempt_no = 1`).Scan(&worstFirst); err != nil {
		t.Fatalf("measure the wave: %v", err)
	}
	t.Logf("webhook burst of %d on a free pool: the wave took %.0fs, the last first attempt started %.1fs after admission; "+
		"lateness peaked at %.1fs and its longest stretch over 300s lasted %.0fs (%d scrapes), at most %d in flight; "+
		"after the wave it ranged %.1fs..%.1fs over %d samples",
		burst, waveTook.Seconds(), worstFirst, peak, overThreshold.Seconds(), reported, maxInFlight,
		lowestAfter, highestAfter, countAfter)

	if reported == 0 {
		t.Fatal("the webhook lateness series was never on a scrape")
	}
	// The floor is 386s by the arithmetic above and by measurement; 480s is
	// the floor with the margin every threshold of D7 carries.
	if worstFirst > 480 {
		t.Errorf("the last commitment of the burst started %.1fs after admission, over the 480s the profile allows", worstFirst)
	}
	if peak > 480 {
		t.Errorf("outbound_queue_lateness_seconds{family=webhook} peaked at %.1fs, over the 480s the profile allows", peak)
	}
	// The rule fires on ten minutes CONTINUOUSLY over 300s, and that is what
	// is measured: the longest single stretch, by the samples' clocks. A
	// burst is over it for a minute and a half; five minutes is the most this
	// test lets it be, and still half the rule's window.
	if overThreshold >= 300*time.Second {
		t.Errorf("the lateness was over 300s for %.0fs without a break; the rule's ten-minute window would not tell this from a stopped queue",
			overThreshold.Seconds())
	}
	if maxInFlight > outbound.WebhookPoolSize {
		t.Errorf("%d webhook commitments in flight at once, the pool is %d", maxInFlight, outbound.WebhookPoolSize)
	}
	if peak == 0 && worstFirst > 0 {
		t.Errorf("the queue was %.1fs behind and the gauge never left zero", worstFirst)
	}
	// "Does not hold": after the wave the gauge is about retries waiting for
	// a slot, and every sample of it is under the threshold - a queue that
	// stopped would show the peak standing still.
	if countAfter == 0 {
		t.Fatal("nothing was sampled after the wave")
	}
	if highestAfter >= 300 {
		t.Errorf("after the wave the lateness still reached %.1fs; the rule's window would not tell this from a stopped queue", highestAfter)
	}
	assertTheSubscriberHeldTheSlot(t, env, hole)
}
