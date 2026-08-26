package store

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
)

// What "the card is out of date" is worth telling somebody, and what it is not.

// TestACardBehindItsAlertIsCountedByWhoCanFixIt.
//
// Three messages, all showing something older than their alert, and the number
// alone would say the same thing about all three. It is not the same thing: one
// is a queue that will drain, one needs a person, and one is a person having
// already decided. An alert written on the total would fire on the third.
func TestACardBehindItsAlertIsCountedByWhoCanFixIt(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	// Queued: aimed at a revision, waiting for a worker.
	queuedAG := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, queuedAG)
	aim(t, s, queuedAG)

	// Stuck: the change failed for a reason no retry mends.
	stuckAG := desiredGroup(t, s, "Disk slow")
	stuckID := changeableCard(t, s, stuckAG)
	aim(t, s, stuckAG)
	token := claimOne(t, s, stuckID)
	begun := beginOne(t, s, stuckID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "invalid_auth"),
	}); err != nil {
		t.Fatalf("finalize the change: %v", err)
	}

	// Abandoned: still behind, and somebody decided to leave it that way.
	goneAG := desiredGroup(t, s, "Disk noisy")
	goneID := changeableCard(t, s, goneAG)
	aim(t, s, goneAG)
	goneToken := claimOne(t, s, goneID)
	goneAttempt := beginOne(t, s, goneID, goneToken)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: goneAttempt.AttemptID, LeaseToken: goneToken,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "cant_update_message"),
	}); err != nil {
		t.Fatalf("finalize the change: %v", err)
	}
	result, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: goneID, Decision: outbound.DecisionCancel,
		Actor: "nina", Reason: "the channel is being retired",
	})
	if err != nil || result.Outcome != outbound.ResolveResolved {
		t.Fatalf("withdraw the card: %s (%v)", result.Outcome, err)
	}

	// And the abandoned one is by far the oldest, which is the whole point of
	// leaving it out of the age.
	if _, err := s.db.Exec(`
		UPDATE outbound_intent_events SET created_at = now() - interval '6 hours'
		WHERE intent_id = $1 AND kind = 'desired_raised'`, goneID); err != nil {
		t.Fatalf("age the withdrawn card: %v", err)
	}

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	behind := map[string]int{}
	for _, b := range snap.OutboundCardsBehind {
		behind[b.State] = b.Count
	}
	for state, want := range map[string]int{"queued": 1, "stuck": 1, "abandoned": 1} {
		if behind[state] != want {
			t.Errorf("%s = %d, want %d (all states: %v)", state, behind[state], want, behind)
		}
	}

	// Six hours belong to the card nobody is going to catch up. What is
	// reported is the age of the ones that are still owed.
	if snap.OutboundCardStalenessSeconds <= 0 {
		t.Error("two messages are behind and the age is zero")
	}
	if snap.OutboundCardStalenessSeconds > 3600 {
		t.Errorf("the age is %.0fs, so the withdrawn card is being counted",
			snap.OutboundCardStalenessSeconds)
	}
}

// aimAgain raises the desired state a second time with an alert set that is
// genuinely different, so the revision is a real one rather than "unchanged".
func aimAgain(t *testing.T, s *Store, agID, alertName string) int64 {
	t.Helper()
	recordAlerts(t, s, agID, []model.Alert{{
		Fingerprint: "fp-" + alertName, Status: model.AlertStatusFiring,
		StartsAt: time.Unix(1700001200, 0),
		Labels:   map[string]string{"alertname": alertName},
	}})
	result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredMerge, Actor: "ingester",
	})
	if err != nil || result.Outcome != outbound.DesiredApplied {
		t.Fatalf("raise the desired state again: %s (%v)", result.Outcome, err)
	}
	return result.Revision
}

// ageRevisions back-dates the journal so a wait can be asserted exactly.
//
// It moves the events, not the commitment. Everything else that could be
// mistaken for an answer - the commitment's own updated_at, the snapshot row -
// stays at now, so a build that measured any of those reports nothing instead
// of the hour these tests set up.
func ageRevisions(t *testing.T, s *Store, intentID string, above int64, ago string) {
	t.Helper()
	res, err := s.db.Exec(`
		UPDATE outbound_intent_events
		SET created_at = now() - $3::interval
		WHERE intent_id = $1 AND kind = 'desired_raised'
		  AND (detail->>'revision')::bigint > $2`, intentID, above, ago)
	if err != nil {
		t.Fatalf("age the journal: %v", err)
	}
	if moved, _ := res.RowsAffected(); moved == 0 {
		t.Fatalf("no revision above %d to age", above)
	}
}

func staleness(t *testing.T, s *Store) float64 {
	t.Helper()
	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	return snap.OutboundCardStalenessSeconds
}

// TestASupersedeDoesNotResetTheWait. The card fell behind an hour ago and a
// second revision arrived just now.
//
// The wait is measured from the first, and it has to be: the alert has been
// misrepresented for an hour, and the newer revision is a further change to
// what it should say, not a fresh start. Reading anything the supersede touched
// - the commitment row, the snapshot - reports a card that has been wrong for
// an hour as one second old.
func TestASupersedeDoesNotResetTheWait(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)
	ageRevisions(t, s, intentID, 0, "1 hour")

	// The second revision, now, before anything has attempted the first.
	aimAgain(t, s, agID, "DiskLoud")

	if age := staleness(t, s); age < 3000 {
		t.Errorf("the wait is %.0fs; the card has been behind for an hour", age)
	}
}

// TestARefusalBeforeTheNetworkIsStuckAndHasAnAge.
//
// Nothing is ever attempted here: the recipient is not linked, so preparation
// refuses and no call is made. There is no attempt row to read a time from, and
// the earlier design that measured from the attempts would have shown this
// commitment as stuck forever with no age at all - the one case where somebody
// most needs to know how long it has been.
func TestARefusalBeforeTheNetworkIsStuckAndHasAnAge(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)
	ageRevisions(t, s, intentID, 0, "1 hour")

	token := claimOne(t, s, intentID)
	refused, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationPermanent,
		ErrorClass:  "identity_not_linked",
		Summary:     "the recipient has no Slack account linked",
	})
	if err != nil {
		t.Fatalf("refuse the preparation: %v", err)
	}
	if refused.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("the preparation came back %s", refused.Outcome)
	}

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	behind := map[string]int{}
	for _, b := range snap.OutboundCardsBehind {
		behind[b.State] = b.Count
	}
	if behind["stuck"] != 1 {
		t.Errorf("a commitment that will never attempt is %v", behind)
	}
	if snap.OutboundCardStalenessSeconds < 3000 {
		t.Errorf("the wait is %.0fs, and there is no attempt to read it from",
			snap.OutboundCardStalenessSeconds)
	}
}

// TestCatchingUpMovesTheWaitToWhatIsStillOwed.
//
// The card applied the revision it was an hour behind on, and a new one arrived
// a moment ago. The hour is over; what is owed now is a moment old. A measure
// that kept the earliest event of all would go on reporting an hour for a card
// that is up to date but for the last second.
func TestCatchingUpMovesTheWaitToWhatIsStillOwed(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	revision := aim(t, s, agID)
	ageRevisions(t, s, intentID, 0, "1 hour")

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: mutationAccepted(t, begun.ReceiptRef, outbound.Receipt{}),
	}); err != nil {
		t.Fatalf("apply the revision: %v", err)
	}
	if age := staleness(t, s); age != 0 {
		t.Fatalf("the card caught up and is still %.0fs behind", age)
	}

	// And falls behind again, just now.
	next := aimAgain(t, s, agID, "DiskLoud")
	if next <= revision {
		t.Fatalf("the second revision is %d and the first was %d", next, revision)
	}
	age := staleness(t, s)
	if age <= 0 {
		t.Error("the card is behind again and the wait is zero")
	}
	if age > 600 {
		t.Errorf("the wait is %.0fs, so the revision it already applied is still counted", age)
	}
}

// TestARepeatedPayloadCostsNothing. Alertmanager resends the same alerts every
// few minutes for as long as they fire.
//
// Each resend used to be a fresh edit of every message about that alert. What
// has to hold now is that nothing happens at all: no revision, no commitment
// back in the queue, no attempt, no line in the journal. A card is edited when
// what it says changes, and a repeat changes nothing.
func TestARepeatedPayloadCostsNothing(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)

	same := []model.Alert{{
		Fingerprint: "fp-2", Status: model.AlertStatusFiring,
		StartsAt: time.Unix(1700000600, 0),
		Labels:   map[string]string{"alertname": "DiskSlow"},
	}}
	// One real payload first, so that what is stored is what this group renders
	// to. Without it every comparison below is against the state the fixture
	// admitted from, and a repeat would look like news for that reason instead
	// of being caught by the rule under test.
	if _, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(),
		"desired-"+agID, same, "ingester"); err != nil {
		t.Fatalf("the first payload: %v", err)
	}

	before := readCard(t, s, intentID)
	countWork := func() (attempts, events int, version int64) {
		t.Helper()
		if err := s.db.QueryRow(`
			SELECT (SELECT count(*) FROM outbound_attempts WHERE intent_id = $1),
			       (SELECT count(*) FROM outbound_intent_events WHERE intent_id = $1),
			       (SELECT render_source_version FROM alert_groups WHERE id = $2)`,
			intentID, agID).Scan(&attempts, &events, &version); err != nil {
			t.Fatalf("count the work: %v", err)
		}
		return attempts, events, version
	}
	attemptsBefore, eventsBefore, versionBefore := countWork()

	for i := 0; i < 3; i++ {
		result, err := s.ApplyAlertmanagerUpdateAtomic(context.Background(),
			"desired-"+agID, same, "ingester")
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if result.Outcome != alertgroup.MergeUnchanged {
			t.Fatalf("repeat %d came back %s", i, result.Outcome)
		}
	}

	if now := readCard(t, s, intentID); now != before {
		t.Errorf("three repeats moved the commitment: %+v -> %+v", before, now)
	}
	attempts, events, versionAfter := countWork()
	if attempts != attemptsBefore || events != eventsBefore {
		t.Errorf("three repeats produced %d attempt(s) and %d journal line(s)",
			attempts-attemptsBefore, events-eventsBefore)
	}

	// And nothing was WRITTEN either. Every check above is downstream of the
	// digest, which would answer "unchanged" for a rewrite of the same alerts
	// just as it does for no write at all - so the alert group's own version is
	// what separates the two. It counts every write that could change what a
	// message says, so a repeat that reached the row moves it.
	if versionAfter != versionBefore {
		t.Errorf("three repeats rewrote the alert group %d time(s)",
			versionAfter-versionBefore)
	}
}

// TestEveryStateReportsEvenWithNothingBehind. A gauge nobody writes keeps its
// last value, so a backlog that has been caught up would go on ringing.
func TestEveryStateReportsEvenWithNothingBehind(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	seen := map[string]bool{}
	for _, b := range snap.OutboundCardsBehind {
		seen[b.State] = true
	}
	for _, state := range []string{"queued", "stuck", "abandoned"} {
		if !seen[state] {
			t.Errorf("%q never reports, so its last value stands forever", state)
		}
	}
}

// TestASettledCommitmentBehindItsAlertIsSaidOutLoud.
//
// idle means applied equals desired, and an editable commitment that succeeded
// applied the final revision, after which there are no more. A row in either
// state with a revision outstanding is this build contradicting itself.
//
// It is not swept into one of the three answers. The gauge would then be quietly
// wrong about a card somebody is looking at, and nothing would say why.
func TestASettledCommitmentBehindItsAlertIsSaidOutLoud(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")

	agID := desiredGroup(t, s, "Disk filling up")
	intentID := changeableCard(t, s, agID)
	aim(t, s, agID)
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET status = $2 WHERE id = $1`,
		intentID, string(outbound.StatusIdle)); err != nil {
		t.Fatalf("settle the commitment: %v", err)
	}

	before := testutil.ToFloat64(
		metrics.StorageContractFailuresTotal.WithLabelValues("desired_revision"))

	snap, err := s.GetMetricsSnapshot()
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	after := testutil.ToFloat64(
		metrics.StorageContractFailuresTotal.WithLabelValues("desired_revision"))
	if after <= before {
		t.Error("a settled commitment behind its alert was scraped and said nothing")
	}
	for _, b := range snap.OutboundCardsBehind {
		if b.Count != 0 {
			t.Errorf("it was counted as %q instead", b.State)
		}
	}
}
