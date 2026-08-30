//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The promises the delivery domain makes that can only be shown end to end.
//
// Everything else in this suite is one component answering a question. These
// are the claims that span the whole path - ingest, plan, admit, send - and
// each of them was a bug in the job engine that the outbound domain exists to
// make impossible.

// criticalAlert is one firing alert routed to the critical policy, which pages
// U_TEST by direct message.
func criticalAlert(key, alertName string) string {
	return fmt.Sprintf(`{
		"groupKey": %q,
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "critical", "alertname": %q},
		"alerts": [{"fingerprint": "fp1", "status": "firing", "labels": {"alertname": %q}}]
	}`, key, alertName, alertName)
}

func intentsOf(t *testing.T, env *IntegrationTestEnv, alertKey string) []outbound.Intent {
	t.Helper()
	var groupID string
	if err := env.S.GetDB().QueryRow(
		`SELECT id FROM alert_groups WHERE alert_key = $1`, alertKey).Scan(&groupID); err != nil {
		t.Fatalf("find the alert group: %v", err)
	}
	intents, err := env.S.ListIntentsByAlertGroup(context.Background(), groupID)
	if err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	return intents
}

// until waits for a condition the database decides, and says what it was
// waiting for when it gives up.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestARetryCarriesWhatWasAdmitted is S1-D3, and it is the reason a snapshot
// exists at all.
//
// The alert changes between two attempts of one commitment - a second alert
// joins the group, which rewrites the row every renderer used to read. The
// retry has to send what was ADMITTED, because the message it is retrying may
// already exist at the provider, and a second one with different text under the
// same key is two different messages nobody can reconcile.
func TestARetryCarriesWhatWasAdmitted(t *testing.T) {
	env := setupIntegrationTest(t)

	// The first call fails in a way that will be tried again.
	env.Channel.FailWith("S_TEST", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "rate_limited",
	}, nil)

	sendWebhook(t, env.Echo, criticalAlert("retry_same_bytes", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	startOutboundWorker(t, env.Worker)

	until(t, "the first attempt to fail", func() bool {
		return len(env.Channel.SentTo("S_TEST")) >= 1
	})
	first := env.Channel.SentTo("S_TEST")[0]

	// The alert moves on: a second alert joins the group, and the group row now
	// says something different from what was admitted.
	sendWebhook(t, env.Echo, `{
		"groupKey": "retry_same_bytes",
		"status": "firing",
		"commonLabels": {"team": "devops", "severity": "critical", "alertname": "DiskFilling"},
		"alerts": [
			{"fingerprint": "fp1", "status": "firing", "labels": {"alertname": "DiskFilling"}},
			{"fingerprint": "fp2", "status": "firing", "labels": {"alertname": "DiskFull"}}
		]
	}`)

	// The provider recovers, and the retry goes out.
	env.Channel.StopFailing("S_TEST")
	until(t, "the retry", func() bool {
		if len(env.Channel.SentTo("S_TEST")) >= 2 {
			return true
		}
		// The backoff is what this test is not about.
		_, _ = env.S.GetDB().Exec(`
			UPDATE outbound_intents SET next_attempt_at = now()
			WHERE status = 'pending' AND next_attempt_at > now()`)
		return false
	})

	sent := env.Channel.SentTo("S_TEST")
	if sent[1] != first {
		t.Fatalf("the retry sent different bytes than the admission:\n first: %s\n retry: %s",
			first, sent[1])
	}
}

// TestARetryableFailureHasNoDeathCounter.
//
// The job engine gave every step a MaxAttempts, and a page that hit it was
// simply never delivered - the alert stayed unanswered and nothing said so. A
// commitment has no such counter: it lives until it succeeds, until somebody
// withdraws it, or until a deadline it was given at admission.
func TestARetryableFailureHasNoDeathCounter(t *testing.T) {
	env := setupIntegrationTest(t)

	env.Channel.FailWith("S_TEST", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "rate_limited",
	}, nil)

	sendWebhook(t, env.Echo, criticalAlert("no_death_counter", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	startOutboundWorker(t, env.Worker)

	// Counted per ADDRESS. The firehose in the same escalation succeeds on its
	// first call, so a count of every call this channel made would reach four
	// with only three failures behind it - and an implementation that gave up
	// on the fourth would pass.
	//
	// Far past anything MaxAttempts would have allowed. The backoff is what
	// slows this down, so the attempts are forced forward rather than waited
	// for: the point is the absence of a limit, not the curve.
	// Waited for in the DATABASE, not at the provider. The channel counts a call
	// when it is ENTERED, so a fourth call is in flight long before the fourth
	// failure is recorded - and a test reading the row at that moment would find
	// a streak of three, or a commitment still sending, and fail a working
	// implementation.
	const failures = 4
	until(t, fmt.Sprintf("%d recorded failures", failures), func() bool {
		var streak int
		var status string
		if err := env.S.GetDB().QueryRow(`
			SELECT i.failure_streak, i.status FROM outbound_intents i
			JOIN alert_groups ag ON ag.id = i.alert_group_id
			WHERE ag.alert_key = $1 AND i.target_kind = 'user'`,
			"no_death_counter").Scan(&streak, &status); err != nil {
			t.Fatalf("read the commitment: %v", err)
		}
		if status == string(outbound.StatusPending) && streak >= failures {
			return true
		}
		// The backoff is what this test is not about.
		_, _ = env.S.GetDB().Exec(`
			UPDATE outbound_intents SET next_attempt_at = now()
			WHERE status = 'pending' AND next_attempt_at > now()`)
		return false
	})

	tried := len(env.Channel.SentTo("S_TEST"))
	if tried < failures {
		t.Fatalf("%d failures were recorded from only %d calls", failures, tried)
	}
	for _, intent := range intentsOf(t, env, "no_death_counter") {
		if intent.TargetKind != "user" {
			continue
		}
		if intent.Status.Terminal() {
			t.Fatalf("a retryable failure ended the commitment as %s after %d attempts",
				intent.Status, tried)
		}
	}
}

// TestOneRecipientFailingLeavesTheOthersAlone.
//
// In the job engine a fan-out was a stage, and what a stage did when one step
// failed was a policy flag. Here there is no stage: each promise is its own
// commitment, and one that can never be delivered has no way to reach the
// others.
func TestOneRecipientFailingLeavesTheOthersAlone(t *testing.T) {
	env := setupIntegrationTest(t)

	// The firehose channel is refused for good; the direct message is fine.
	env.Channel.FailWith("C_FIREHOSE", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "channel_not_found",
	}, nil)

	sendWebhook(t, env.Echo, criticalAlert("fan_out_isolation", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "fan_out_isolation", 2)

	var failed, delivered int
	for _, intent := range intentsOf(t, env, "fan_out_isolation") {
		switch intent.Status {
		case outbound.StatusPermanentFailed:
			failed++
			if intent.TargetKind == "user" {
				t.Error("the direct message failed, which is not what was broken")
			}
		case outbound.StatusSucceeded, outbound.StatusIdle:
			delivered++
		default:
			t.Errorf("a commitment to %s is %s", intent.TargetRef, intent.Status)
		}
	}
	if failed != 1 || delivered != 1 {
		t.Fatalf("%d failed and %d delivered, want one of each", failed, delivered)
	}
}

// TestWorkOutlivesTheProcessThatTookIt.
//
// Every instance goes away - a deploy, a crash, a node draining - and what is
// owed has to survive it. Nothing owed lives in a process: the commitment is a
// row and its state is durable, so a worker that comes up afterwards finds
// pending work waiting and carries on.
//
// This is about durable pending work, not about reclaiming a lease: an attempt
// abandoned mid-flight is recovery's job and has its own tests. Here the first
// instance FINISHES what it started - badly - and then goes away.
func TestWorkOutlivesTheProcessThatTookIt(t *testing.T) {
	env := setupIntegrationTest(t)

	env.Channel.FailWith("S_TEST", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "rate_limited",
	}, nil)
	env.Channel.FailWith("C_FIREHOSE", outbound.Result{
		Evidence: outbound.ProviderResponse, Status: "rate_limited",
	}, nil)

	sendWebhook(t, env.Echo, criticalAlert("survives_restart", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	// One instance runs and fails everything it touches.
	stopFirst := startOutboundWorker(t, env.Worker)
	until(t, "both commitments to have failed once and gone back to the queue", func() bool {
		var waiting int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents i
			JOIN alert_groups ag ON ag.id = i.alert_group_id
			WHERE ag.alert_key = $1 AND i.status = 'pending' AND i.failure_streak > 0`,
			"survives_restart").Scan(&waiting); err != nil {
			t.Fatalf("read the queue: %v", err)
		}
		return waiting == 2
	})

	// And then goes away. Joined, not merely cancelled: an instance still
	// writing to the database is an instance that has not stopped.
	stopFirst()

	owed := 0
	for _, intent := range intentsOf(t, env, "survives_restart") {
		if !intent.Status.Terminal() {
			owed++
		}
	}
	if owed != 2 {
		t.Fatalf("%d commitments survived the restart, want 2", owed)
	}

	// A new instance, with a worker id of its own, finishes the job.
	env.Channel.StopFailing("S_TEST")
	env.Channel.StopFailing("C_FIREHOSE")
	if _, err := env.S.GetDB().Exec(
		`UPDATE outbound_intents SET next_attempt_at = now()`); err != nil {
		t.Fatalf("bring the retries forward: %v", err)
	}

	restarted := outbound.NewWorker(env.S, "integration-worker-2",
		map[string]outbound.Channel{"slack": env.Channel, "telegram": env.Channel})
	startOutboundWorker(t, restarted)

	waitForDeliveries(t, env.S, "survives_restart", 2)

	// And it was the SECOND instance that finished them. Without this the test
	// passes on a first instance that never actually stopped.
	var byFirst, bySecond int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(*) FILTER (WHERE a.worker_id = 'integration-worker'),
		       count(*) FILTER (WHERE a.worker_id = 'integration-worker-2')
		FROM outbound_attempts a
		JOIN outbound_intents i ON i.id = a.intent_id
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = $1 AND a.outcome = 'accepted'`,
		"survives_restart").Scan(&byFirst, &bySecond); err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if bySecond != 2 || byFirst != 0 {
		t.Fatalf("the deliveries were made by %d attempts of the first instance and "+
			"%d of the second, want 0 and 2", byFirst, bySecond)
	}
}

// TestWorkFromANewerBuildIsLeftWhereItIs.
//
// A rollback is not stop-the-world the way an upgrade is, so an older instance
// meets rows a newer one admitted. Every one of them is work that will be done
// - by the instance that knows the shape - and the only wrong answer is a
// durable one.
//
// It has to be shown from the worker, because the worker is where the trap is.
// A channel prepares before the attempt exists, preparing means decoding the
// payload, and a shape with no decoder here comes back "permanently
// unreadable" from every channel there is. Handed straight to the store, that
// verdict ends the commitment. Calling the store directly with a preparation
// this test chose would prove nothing about the path that actually runs.
func TestWorkFromANewerBuildIsLeftWhereItIs(t *testing.T) {
	env := setupIntegrationTest(t)

	sendWebhook(t, env.Echo, criticalAlert("from_a_newer_build", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())
	if _, err := env.S.GetDB().Exec(`
		UPDATE outbound_intents SET payload_schema_version = 2
		WHERE alert_group_id = (SELECT id FROM alert_groups WHERE alert_key = $1)`,
		"from_a_newer_build"); err != nil {
		t.Fatalf("write the commitments as a newer build would: %v", err)
	}

	// A second alert this build understands completely. It is the proof that
	// the worker ran at all: without it, "nothing happened" is also what a
	// worker that never started would leave behind.
	sendWebhook(t, env.Echo, criticalAlert("from_this_build", "MemoryFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	stop := startOutboundWorker(t, env.Worker)
	waitForDeliveries(t, env.S, "from_this_build", 2)
	// Joined, not merely cancelled: the commitments from the newer build were
	// claimed alongside the others, and what this asserts is what the worker
	// did with them - which is only settled once it has finished.
	stop()

	for _, intent := range intentsOf(t, env, "from_a_newer_build") {
		if intent.Status != outbound.StatusPending {
			t.Errorf("commitment %s is %s; the build that admitted it never gets it back",
				intent.ID, intent.Status)
		}
	}

	var attempts, events int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(a.id), count(e.id)
		FROM outbound_intents i
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		LEFT JOIN outbound_attempts a ON a.intent_id = i.id
		LEFT JOIN outbound_intent_events e ON e.intent_id = i.id AND e.kind <> 'created'
		WHERE ag.alert_key = $1`, "from_a_newer_build").Scan(&attempts, &events); err != nil {
		t.Fatalf("count what was written: %v", err)
	}
	if attempts != 0 || events != 0 {
		t.Fatalf("this build wrote %d attempt(s) and %d journal line(s) about work "+
			"it cannot read", attempts, events)
	}
}

// TestAPageStartsGoingOutPromptly is the D7 smoke, and it is declared a smoke
// rather than a percentile on purpose: one measurement on an idle instance
// proves the path is not asleep, and nothing more. A percentile under a load
// profile needs a second partition to measure isolation against, and waits for
// one.
func TestAPageStartsGoingOutPromptly(t *testing.T) {
	env := setupIntegrationTest(t)

	sendWebhook(t, env.Echo, criticalAlert("slo_smoke", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	startOutboundWorker(t, env.Worker)

	waitForDeliveries(t, env.S, "slo_smoke", 2)

	// Measured by the database at both ends, the way the histogram is.
	var worst float64
	if err := env.S.GetDB().QueryRow(`
		SELECT max(EXTRACT(EPOCH FROM (a.started_at - b.admitted_at)))::double precision
		FROM outbound_attempts a
		JOIN outbound_intents i ON i.id = a.intent_id
		JOIN outbound_batches b ON b.id = i.batch_id
		JOIN alert_groups ag ON ag.id = i.alert_group_id
		WHERE ag.alert_key = $1 AND a.attempt_no = 1 AND i.not_before <= b.admitted_at`,
		"slo_smoke").Scan(&worst); err != nil {
		t.Fatalf("measure the delay: %v", err)
	}
	if worst > 10 {
		t.Fatalf("the first attempt of an immediately due commitment started %.1fs "+
			"after it was admitted", worst)
	}
}

// TestNoMoreLeasesThanSlots is S1-D23. A lease taken without a slot to run it in
// is a lease sitting in a local queue while it expires - and an expired lease
// means somebody else redoes a call that may already have gone out.
func TestNoMoreLeasesThanSlots(t *testing.T) {
	env := setupIntegrationTest(t)

	// A worker of this test's own, under a name nobody else answers to. The
	// fixture's worker is called "integration-worker" in every test, so a late
	// goroutine of a neighbouring one holding leases under that name would be
	// counted here as this worker exceeding its pool.
	const poolWorkerID = "pool-slots-worker"
	worker := outbound.NewWorker(env.S, poolWorkerID,
		map[string]outbound.Channel{"slack": env.Channel, "telegram": env.Channel})

	// More work than the pool, all of it held inside the provider so the slots
	// stay occupied while the worker keeps ticking.
	release := env.Channel.Hold()
	// Deferred as well as called at the end. If an assertion below fails, the
	// calls are still inside the provider, and the cleanup that stops the
	// worker would sit in Drain for its whole deadline before giving up and
	// leaving them running. Deferred functions run before cleanups, and release
	// is idempotent.
	defer release()

	for i := 0; i < outbound.NotificationPoolSize+4; i++ {
		sendWebhook(t, env.Echo, criticalAlert(fmt.Sprintf("pool_%d", i), "DiskFilling"))
	}
	env.Eng.ProcessNewAlertGroups(context.Background())

	stop := startOutboundWorker(t, worker)

	// The worker has to have started before the count means anything.
	until(t, "the pool to fill", func() bool {
		return env.Channel.SentCount() >= outbound.NotificationPoolSize
	})

	// Whatever the worker does, it never holds more leases than it has slots.
	//
	// Counted by WORKER, and by a worker id nobody else uses. The claim is that
	// one worker stays inside its own pool, and a whole-table count says
	// something else: a lease left live by a neighbouring test - they share
	// this database, and a notification lease outlives the test that took it by
	// ninety seconds - would be read as this worker holding too many. The
	// shared fixture name would not fix that either, since every test's worker
	// answers to it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var leased int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents
			WHERE lease_token IS NOT NULL AND locked_until > now()
			  AND worker_id = $1`, poolWorkerID).Scan(&leased); err != nil {
			t.Fatalf("count the leases: %v", err)
		}
		if leased > outbound.NotificationPoolSize {
			t.Fatalf("%d leases are held at once with a pool of %d",
				leased, outbound.NotificationPoolSize)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The calls are still inside the provider, so the test ends by letting them
	// out and waiting: attempts finishing after a test believes it is over are
	// rows appearing in the middle of the next one's setup.
	release()
	stop()

	var live int
	if err := env.S.GetDB().QueryRow(`
		SELECT count(*) FROM outbound_intents
		WHERE status = 'sending' OR (lease_token IS NOT NULL AND locked_until > now())`).
		Scan(&live); err != nil {
		t.Fatalf("count what is still in flight: %v", err)
	}
	if live != 0 {
		t.Fatalf("%d commitments are still in flight after the worker stopped", live)
	}
}

var _ = model.AlertGroupStatusNew

// TestAShiftChangeReachesBothChannels is the whole path for the second family,
// end to end: an announcement is admitted, claimed by its own partition,
// prepared, sent, and settled - once per channel the person is linked to.
//
// It is the delivery half of the split the payload exists for. The producer
// writes no words at all now, so if it ever went back to composing a sentence
// the two messages here would be the same string; what this asserts is that
// each channel was handed the payload and wrote its own.
func TestAShiftChangeReachesBothChannels(t *testing.T) {
	env := setupIntegrationTest(t)
	ctx := context.Background()

	if err := env.S.BindExternalIdentity(&model.ExternalIdentity{
		UserID: "U_TEST", Provider: "telegram", ExternalID: "T_TEST",
	}); err != nil {
		t.Fatalf("link a Telegram account: %v", err)
	}

	admission, err := keys.HandoffBatch{
		Occurrence: keys.Occurrence{
			Kind: keys.HandoffShiftChange, ScheduleID: "sched-1", Source: "rotation",
			GroupID: "g-a", UserIDs: []string{"U_TEST"},
			AssignmentStart: time.Now().Add(time.Hour).UTC(), RevisionID: "rev-1",
		},
		TeamName: "Backend", Timezone: "UTC",
		GridSlotStart:      time.Now().Add(time.Hour).UTC(),
		AssignmentEnd:      time.Now().Add(9 * time.Hour).UTC(),
		MaxAge:             time.Hour,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Recipients: []keys.HandoffRecipient{
			announceThrough("slack", "U_TEST"),
			announceThrough("telegram", "U_TEST"),
		},
	}.Admit()
	if err != nil {
		t.Fatalf("build the announcement: %v", err)
	}
	result, err := env.S.SubmitBatch(ctx, outbound.Batch{
		Admission: admission, Context: outbound.AnnouncingShiftChange(), Actor: "notifier",
	})
	if err != nil {
		t.Fatalf("admit the announcement: %v", err)
	}
	if result.Outcome != outbound.SubmitCreated || len(result.IntentIDs) != 2 {
		t.Fatalf("the announcement answered %q with %d commitments",
			result.Outcome, len(result.IntentIDs))
	}

	stop := startOutboundWorker(t, env.Handoff)
	until(t, "both announcements to be delivered", func() bool {
		var settled int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents
			WHERE key_kind = 'handoff' AND status = 'succeeded'`).Scan(&settled); err != nil {
			t.Fatalf("read the commitments: %v", err)
		}
		return settled == 2
	})
	stop()

	// Each channel was called at the address the identity resolves to, which is
	// the account and not the person: the commitment names U_TEST and what
	// reached the provider is what U_TEST is known by there.
	sent := env.Channel.SentTargets()
	found := map[string]bool{}
	for _, target := range sent {
		found[target] = true
	}
	if !found["S_TEST"] || !found["T_TEST"] {
		t.Fatalf("the announcement reached %v, want the Slack and Telegram accounts", sent)
	}

	// And nothing about the alert domain moved: an announcement is not about an
	// alert group, and the family it ran in is its own.
	var family string
	if err := env.S.GetDB().QueryRow(
		`SELECT DISTINCT delivery_family FROM outbound_intents WHERE key_kind = 'handoff'`).
		Scan(&family); err != nil {
		t.Fatalf("read the family: %v", err)
	}
	if family != outbound.FamilyHandoff {
		t.Fatalf("the announcement ran in %q", family)
	}
}

// announceThrough is one recipient of an announcement, as the builder makes it.
func announceThrough(provider, userID string) keys.HandoffRecipient {
	return keys.HandoffRecipient{
		Provider: provider, UserID: userID,
		Timing:          keys.TimingSpec{Kind: keys.TimingRelativeToAdmission},
		CompletionMode:  keys.CompletionOnAcceptance,
		AmbiguityPolicy: keys.PolicyRetry,
	}
}
