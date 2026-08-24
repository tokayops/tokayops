//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOutboundWorker(ctx, env.Worker)

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

// TestARetryableFailureHasNoDeathCounter is epic check 3.
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOutboundWorker(ctx, env.Worker)

	// Far past anything MaxAttempts would have allowed. The backoff is what
	// slows this down, so the attempts are forced forward rather than waited
	// for - the point is the absence of a limit, not the curve.
	for want := 1; want <= 4; want++ {
		until(t, fmt.Sprintf("attempt %d", want), func() bool {
			if env.Channel.SentCount() >= want {
				return true
			}
			_, _ = env.S.GetDB().Exec(`
				UPDATE outbound_intents SET next_attempt_at = now()
				WHERE status = 'pending' AND next_attempt_at > now()`)
			return false
		})
	}

	for _, intent := range intentsOf(t, env, "no_death_counter") {
		if intent.TargetKind != "user" {
			continue
		}
		if intent.Status.Terminal() {
			t.Fatalf("a retryable failure ended the commitment as %s after %d attempts",
				intent.Status, env.Channel.SentCount())
		}
		if intent.FailureStreak < 3 {
			t.Errorf("the failure streak is %d after %d attempts",
				intent.FailureStreak, env.Channel.SentCount())
		}
	}
}

// TestOneRecipientFailingLeavesTheOthersAlone is epic check 7.
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOutboundWorker(ctx, env.Worker)

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

// TestWorkOutlivesTheProcessThatTookIt is epic check 14.
//
// Every instance goes away - a deploy, a crash, a node draining - and what is
// owed has to survive it. Nothing here lives in a process: the commitment is a
// row, the lease is a column with a deadline, and the next worker to come along
// picks it up because the deadline passed.
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

	// One instance runs, fails everything it touches, and stops.
	first, stopFirst := context.WithCancel(context.Background())
	go runOutboundWorker(first, env.Worker)
	until(t, "the first instance to try", func() bool { return env.Channel.SentCount() >= 2 })
	stopFirst()

	// Nothing was lost: the commitments are still owed, and no lease outlives
	// the process that took it in a way that stops anybody else.
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
	second, stopSecond := context.WithCancel(context.Background())
	defer stopSecond()
	go runOutboundWorker(second, restarted)

	waitForDeliveries(t, env.S, "survives_restart", 2)
}

// TestAPageStartsGoingOutPromptly is the D7 smoke, and it is declared a smoke
// rather than a percentile on purpose: one measurement on an idle instance
// proves the path is not asleep, and nothing more. The percentile under a load
// profile belongs to Sprint 4, where there is a second partition to measure
// isolation against.
func TestAPageStartsGoingOutPromptly(t *testing.T) {
	env := setupIntegrationTest(t)

	sendWebhook(t, env.Echo, criticalAlert("slo_smoke", "DiskFilling"))
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOutboundWorker(ctx, env.Worker)

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

	// More work than the pool, all of it held inside the provider so the slots
	// stay occupied while the worker keeps ticking.
	release := env.Channel.Hold()
	defer release()
	for i := 0; i < outbound.NotificationPoolSize+4; i++ {
		sendWebhook(t, env.Echo, criticalAlert(fmt.Sprintf("pool_%d", i), "DiskFilling"))
	}
	env.Eng.ProcessNewAlertGroups(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runOutboundWorker(ctx, env.Worker)

	// The worker has to have started before the count means anything.
	until(t, "the pool to fill", func() bool {
		return env.Channel.SentCount() >= outbound.NotificationPoolSize
	})

	// Whatever the worker does, it never holds more leases than it has slots.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var leased int
		if err := env.S.GetDB().QueryRow(`
			SELECT count(*) FROM outbound_intents
			WHERE lease_token IS NOT NULL AND locked_until > now()`).Scan(&leased); err != nil {
			t.Fatalf("count the leases: %v", err)
		}
		if leased > outbound.NotificationPoolSize {
			t.Fatalf("%d leases are held at once with a pool of %d",
				leased, outbound.NotificationPoolSize)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var _ = model.AlertGroupStatusNew
