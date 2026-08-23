package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a delivery is allowed to trust. The worker resolves an address, names a
// revision and reports an outcome, and none of those are taken at face value:
// the address is whatever the effect was bound to when it opened, the revision
// is whatever the attempt recorded, and the state being rendered has to still
// be the state its key describes. Every test here is one of those refusals.

// makeDue brings a scheduled retry forward. Backoff is real time, and a test
// that waited for it would be testing the clock.
func makeDue(t *testing.T, s *Store, intentID string) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE outbound_intents SET next_attempt_at = now() - interval '1 second'
		 WHERE id = $1`, intentID); err != nil {
		t.Fatalf("bring the retry of %s forward: %v", intentID, err)
	}
}

func bindingOf(t *testing.T, s *Store, intentID string) (endpoint, key string) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT COALESCE(bound_endpoint, ''), COALESCE(create_key, '')
		 FROM outbound_intents WHERE id = $1`, intentID).Scan(&endpoint, &key); err != nil {
		t.Fatalf("read the binding of %s: %v", intentID, err)
	}
	return endpoint, key
}

func countEvents(t *testing.T, s *Store, intentID, kind string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_intent_events WHERE intent_id = $1 AND kind = $2`,
		intentID, kind).Scan(&n); err != nil {
		t.Fatalf("count the %s events: %v", kind, err)
	}
	return n
}

// TestTheGenerationBindsTheEffectOnce is the invariant the generation exists
// for. A retry of a call that may have happened has to reach the same place
// under the same key - if the recipient was relinked in between, following the
// new address would deliver twice, to two different people, with nobody able to
// tell which one got it.
func TestTheGenerationBindsTheEffectOnce(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	token := claimOne(t, s, intentID)
	first := beginOne(t, s, intentID, token)

	endpoint, key := bindingOf(t, s, intentID)
	if endpoint != "C0001" || key == "" {
		t.Fatalf("the first attempt bound %q under key %q", endpoint, key)
	}
	if first.BoundEndpoint != endpoint || first.ProviderKey != key {
		t.Fatalf("the worker was told to use %q/%q, the binding says %q/%q",
			first.BoundEndpoint, first.ProviderKey, endpoint, key)
	}
	if got := countEvents(t, s, intentID, "effect_bound"); got != 1 {
		t.Fatalf("binding the effect left %d records of itself", got)
	}

	// The call is abandoned mid-flight, so the effect stays open: whatever it
	// did at C0001 may have happened.
	expireLease(t, s, intentID)
	if _, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10); err != nil {
		t.Fatalf("recover: %v", err)
	}
	makeDue(t, s, intentID)

	// The retry resolves the recipient again and gets a different channel -
	// the relink this whole mechanism is about.
	retryToken := claimOne(t, s, intentID)
	second, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: retryToken, WorkerID: "worker-2",
		Preparation: outbound.PreparationReady, BoundEndpoint: "C9999",
	})
	if err != nil {
		t.Fatalf("begin the retry: %v", err)
	}
	if second.BoundEndpoint != "C0001" {
		t.Fatalf("the retry was sent to %q, the effect is bound to C0001", second.BoundEndpoint)
	}
	if second.ProviderKey != key {
		t.Fatalf("the retry asked the provider for a new object: key %q, want %q",
			second.ProviderKey, key)
	}
	if got := countEvents(t, s, intentID, "effect_bound"); got != 1 {
		t.Fatalf("the retry opened a second effect (%d records)", got)
	}

	// And the journal says where the message really went, not where this
	// worker thought it was going.
	var storedEndpoint, storedKey string
	if err := s.db.QueryRow(
		`SELECT bound_endpoint, provider_key FROM outbound_attempts WHERE id = $1`,
		second.AttemptID).Scan(&storedEndpoint, &storedKey); err != nil {
		t.Fatalf("read the attempt: %v", err)
	}
	if storedEndpoint != "C0001" || storedKey != key {
		t.Fatalf("the journal records %q/%q for an attempt that used C0001/%s",
			storedEndpoint, storedKey, key)
	}
}

// TestANewGenerationBindsAgain is the other half of the rule. An operator who
// decides to make a SECOND object - having accepted that the first may exist -
// gets a genuinely new effect: a new address, a new key, and nothing inherited
// from the call whose fate nobody could establish.
func TestANewGenerationBindsAgain(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := stuckInReview(t, s, agID)

	_, firstKey := bindingOf(t, s, intentID)
	if firstKey == "" {
		t.Fatal("the doubtful call left no key behind")
	}

	result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionRetryNewGeneration,
		Reason: "the first one never showed up", AcceptedDuplicateRisk: true,
	})
	if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusPending {
		t.Fatalf("starting a new effect answered %q into %s", result.Outcome, result.Status)
	}

	endpoint, key := bindingOf(t, s, intentID)
	if endpoint != "" || key != "" {
		t.Fatalf("the new generation inherited %q/%q from the old one", endpoint, key)
	}

	token := claimOne(t, s, intentID)
	second, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-2",
		Preparation: outbound.PreparationReady, BoundEndpoint: "C7777",
	})
	if err != nil {
		t.Fatalf("begin the new generation: %v", err)
	}
	if second.BoundEndpoint != "C7777" {
		t.Fatalf("the new effect went to %q instead of the address just resolved",
			second.BoundEndpoint)
	}
	if second.ProviderKey == firstKey {
		t.Fatal("the new object asked the provider for the same key as the old one, " +
			"which is exactly how it would be deduplicated away")
	}
	if got := countEvents(t, s, intentID, "effect_bound"); got != 2 {
		t.Fatalf("the commitment bound %d effects, want two", got)
	}
}

// TestARefusedPreparationDoesNotConsumeTheBinding: a call that provably never
// happened still leaves a record, and the binding must not be counted away by
// it. Deciding "the effect is already open" from the number of journal rows
// would leave the first real call with no address at all.
func TestARefusedPreparationDoesNotConsumeTheBinding(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	token := claimOne(t, s, intentID)
	refused, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationTransient, ErrorClass: "rate_limited",
	})
	if err != nil {
		t.Fatalf("record the refusal: %v", err)
	}
	if refused.Outcome != outbound.BeginPreparedRetry {
		t.Fatalf("a transient refusal answered %q", refused.Outcome)
	}
	if _, key := bindingOf(t, s, intentID); key != "" {
		t.Fatalf("a call that never happened bound the effect under %q", key)
	}

	makeDue(t, s, intentID)
	nextToken := claimOne(t, s, intentID)
	started := beginOne(t, s, intentID, nextToken)

	if started.Outcome != outbound.BeginStarted {
		t.Fatalf("the first real attempt answered %q", started.Outcome)
	}
	endpoint, key := bindingOf(t, s, intentID)
	if endpoint == "" || key == "" {
		t.Fatalf("the first real call went out unbound: %q/%q", endpoint, key)
	}
	if started.ProviderKey != key {
		t.Fatalf("the worker was given key %q, the commitment holds %q", started.ProviderKey, key)
	}
	if got := countEvents(t, s, intentID, "effect_bound"); got != 1 {
		t.Fatalf("the effect opened %d times", got)
	}
}

// TestStateThatNoLongerMatchesItsKeyIsRefused. The commitment's key was
// computed over a digest of the state to render. If what is stored no longer
// produces that digest, or describes something else entirely, rendering it
// would send a message the key does not describe - and the provider would
// happily deduplicate the two as one.
//
// Each of these is a refusal that ends the commitment, because none of them
// heals: what the key names is gone, and no retry brings it back.
func TestStateThatNoLongerMatchesItsKeyIsRefused(t *testing.T) {
	s := setupTestDB(t)

	cases := []struct {
		name    string
		corrupt func(t *testing.T, s *Store, agID string)
	}{{
		name: "the content was edited underneath the digest",
		corrupt: func(t *testing.T, s *Store, agID string) {
			if _, err := s.db.Exec(`
				UPDATE outbound_group_snapshots
				SET snapshot = jsonb_set(snapshot, '{title}', '"a different alert"')
				WHERE alert_group_id = $1`, agID); err != nil {
				t.Fatalf("edit the state: %v", err)
			}
		},
	}, {
		name: "it was written by a build this one cannot read",
		corrupt: func(t *testing.T, s *Store, agID string) {
			if _, err := s.db.Exec(`
				UPDATE outbound_group_snapshots SET snapshot_schema_version = $2
				WHERE alert_group_id = $1`, agID, keys.RenderSnapshotSchemaV1+1); err != nil {
				t.Fatalf("age the schema: %v", err)
			}
		},
	}, {
		name: "it describes another alert",
		corrupt: func(t *testing.T, s *Store, agID string) {
			foreign := outboundSnapshot(t, outboundGroup(t, s), "elsewhere")
			raw, err := json.Marshal(foreign)
			if err != nil {
				t.Fatalf("marshal the state: %v", err)
			}
			if _, err := s.db.Exec(`
				UPDATE outbound_group_snapshots SET snapshot = $2, snapshot_digest = $3
				WHERE alert_group_id = $1`, agID, raw, foreign.Digest()); err != nil {
				t.Fatalf("swap the state: %v", err)
			}
		},
	}, {
		name: "it is at a revision the row does not claim",
		corrupt: func(t *testing.T, s *Store, agID string) {
			if _, err := s.db.Exec(
				`UPDATE outbound_group_snapshots SET revision = 5 WHERE alert_group_id = $1`,
				agID); err != nil {
				t.Fatalf("move the revision: %v", err)
			}
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agID := outboundGroup(t, s)
			intentID := admitOne(t, s, agID)[0]
			token := claimOne(t, s, intentID)
			tc.corrupt(t, s, agID)

			result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
				IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
				Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
			})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if result.Outcome != outbound.BeginPreparedPermanent {
				t.Fatalf("the send answered %q on state that does not match its key",
					result.Outcome)
			}
			if started := attemptsStarted(t, s, intentID); started != 0 {
				t.Fatalf("the refusal still left %d calls behind", started)
			}
		})
	}
}

// TestFinalizeBelievesTheAttemptRatherThanTheWorker. A result arrives with the
// worker's own account of what it did; the durable record of what was actually
// authorised is the attempt row, and where the two disagree the attempt wins.
func TestFinalizeBelievesTheAttemptRatherThanTheWorker(t *testing.T) {
	s := setupTestDB(t)

	t.Run("a result that names the revision it applied", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		claimed := int64(7)
		completion := acceptedCompletion()
		completion.AppliedRevision = &claimed

		_, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token, Completion: completion,
		})
		if !errors.Is(err, ErrOutboundContract) {
			t.Fatalf("a worker got to say which revision its message carried: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusSending {
			t.Fatalf("the refused result still moved the commitment to %s", got)
		}
		var applied sql.NullInt64
		if err := s.db.QueryRow(
			`SELECT applied_revision FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&applied); err != nil {
			t.Fatalf("read the applied revision: %v", err)
		}
		if applied.Valid {
			t.Fatalf("the commitment now claims to show revision %d", applied.Int64)
		}
	})

	t.Run("a result that says nothing about the revision", func(t *testing.T) {
		// The other half of the same rule: the store fills the revision in
		// from the attempt, so the commitment ends up recording what was
		// actually rendered rather than nothing at all.
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID, dmCommitment("U0001"))[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if _, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token, Completion: acceptedCompletion(),
			Receipt: json.RawMessage(`{"channel":"C0001","ts":"1700000000.000100"}`),
		}); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		var applied sql.NullInt64
		if err := s.db.QueryRow(
			`SELECT applied_revision FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&applied); err != nil {
			t.Fatalf("read the applied revision: %v", err)
		}
		if !applied.Valid || applied.Int64 != begun.AppliedRevision {
			t.Fatalf("the commitment records revision %v, the attempt carried %d",
				applied, begun.AppliedRevision)
		}
	})

	t.Run("a protocol this build cannot speak", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		// The attempt was opened by a build whose completion protocol this one
		// does not know. Comparing its result under today's rules would read a
		// repeat as a contradiction.
		if _, err := s.db.Exec(
			`UPDATE outbound_attempts SET completion_fingerprint_version = $2 WHERE id = $1`,
			begun.AttemptID, keys.CurrentCompletionFingerprintVersion()+1); err != nil {
			t.Fatalf("age the attempt: %v", err)
		}

		_, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
			AttemptID: begun.AttemptID, LeaseToken: token, Completion: acceptedCompletion(),
		})
		if !errors.Is(err, keys.ErrContract) {
			t.Fatalf("the result was compared across two protocols: %v", err)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusSending {
			t.Fatalf("the unreadable result still moved the commitment to %s", got)
		}
	})
}

// TestRecoveryWritesWhatItDecidedIntoTheAlert. Recovery is the one path where
// nobody is waiting for an answer, which is exactly why its conclusion has to
// reach the alert: a commitment that quietly became "assumed delivered" while
// the alert still says nothing happened is the failure that gets somebody
// paged twice.
func TestRecoveryWritesWhatItDecidedIntoTheAlert(t *testing.T) {
	s := setupTestDB(t)

	t.Run("a lost lease is visible in the history", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		beginOne(t, s, intentID, token)

		expireLease(t, s, intentID)
		if _, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10); err != nil {
			t.Fatalf("recover: %v", err)
		}

		if !hasTimeline(t, s, agID, "interrupted mid-flight") {
			t.Fatal("a call whose fate is unknown left no trace in the alert")
		}
		// It is doubt, not a delivery: the alert has not been paged.
		if got := groupStatusOf(t, s, agID); got != model.AlertGroupStatusProcessing {
			t.Fatalf("an interrupted send moved the alert to %s", got)
		}
	})

	t.Run("an assumed delivery pages the alert", func(t *testing.T) {
		agID := outboundGroup(t, s)
		commitment := dmCommitment("U0001")
		commitment.AmbiguityPolicy = keys.PolicyAssumeAccepted
		intentID := admitOne(t, s, agID, commitment)[0]

		token := claimOne(t, s, intentID)
		beginOne(t, s, intentID, token)
		expireLease(t, s, intentID)

		recovered, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(recovered) != 1 || recovered[0].To != outbound.StatusSucceeded {
			t.Fatalf("recovery under assume_accepted produced %+v", recovered)
		}

		if got := groupStatusOf(t, s, agID); got != model.AlertGroupStatusTriggered {
			t.Fatalf("the alert is %s after its only notification was assumed delivered", got)
		}
		if !hasTimeline(t, s, agID, "assumed delivered") {
			t.Fatal("the assumption is invisible in the alert's history")
		}

		var risk bool
		if err := s.db.QueryRow(
			`SELECT accepted_duplicate_risk FROM outbound_intents WHERE id = $1`, intentID).
			Scan(&risk); err != nil {
			t.Fatalf("read the risk flag: %v", err)
		}
		if !risk {
			t.Fatal("a delivery nobody confirmed is recorded as if it were confirmed")
		}
	})
}

// TestTheJournalAnswersWhatHappened is the question support and audit actually
// ask, and it has one answer rather than three: what was refused, what was
// attempted, what arrived late, and what people did to it.
func TestTheJournalAnswersWhatHappened(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	token := claimOne(t, s, intentID)
	if _, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationTransient, ErrorClass: "rate_limited",
	}); err != nil {
		t.Fatalf("record the refusal: %v", err)
	}

	makeDue(t, s, intentID)
	sendToken := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, sendToken)

	expireLease(t, s, intentID)
	if _, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10); err != nil {
		t.Fatalf("recover: %v", err)
	}

	// The worker comes back after recovery gave up on it, and what it says is
	// kept: it may be the only evidence the message arrived.
	late, err := s.FinalizeDeliveryAttempt(context.Background(), outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: sendToken, Completion: acceptedCompletion(),
		Receipt: json.RawMessage(`{"channel":"C0001","ts":"1700000000.000100"}`),
	})
	if err != nil {
		t.Fatalf("finalize late: %v", err)
	}
	if !late.ObservationRecorded {
		t.Fatal("the late result was dropped")
	}

	journal, err := s.IntentJournal(context.Background(), intentID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if journal == nil {
		t.Fatal("the commitment has no journal")
	}
	if journal.Intent.ID != intentID {
		t.Fatalf("the journal is about %s", journal.Intent.ID)
	}

	if len(journal.Attempts) != 2 {
		t.Fatalf("the journal holds %d records, want the refusal and the call", len(journal.Attempts))
	}
	refusal, call := journal.Attempts[0], journal.Attempts[1]
	if refusal.RecordKind != outbound.RecordPreparation || refusal.StartedAt != nil {
		t.Fatalf("the first record is %+v, want a refusal that never started", refusal)
	}
	if refusal.Outcome != outbound.OutcomeRetryableRejection || refusal.ErrorClass != "rate_limited" {
		t.Fatalf("the refusal lost its reason: %+v", refusal)
	}
	if call.RecordKind != outbound.RecordAttempt || call.ID != begun.AttemptID {
		t.Fatalf("the second record is %+v, want the call", call)
	}
	if call.BoundEndpoint != "C0001" || call.ProviderKey == "" {
		t.Fatalf("the call does not say where it went: %+v", call)
	}
	if call.FinishReason != "lease_lost" || call.Outcome != outbound.OutcomeAmbiguous {
		t.Fatalf("the abandoned call reads as %q/%q", call.FinishReason, call.Outcome)
	}
	if call.AppliedRevision == nil || *call.AppliedRevision != 0 {
		t.Fatalf("the call does not say which revision it carried: %+v", call.AppliedRevision)
	}

	if len(journal.Observations) != 1 {
		t.Fatalf("the journal holds %d late results", len(journal.Observations))
	}
	if journal.Observations[0].AttemptID != begun.AttemptID ||
		journal.Observations[0].Outcome != outbound.OutcomeAccepted {
		t.Fatalf("the late result is %+v", journal.Observations[0])
	}
	if len(journal.Observations[0].Receipt) == 0 {
		t.Fatal("the late result lost the receipt that proves the message exists")
	}

	var kinds []string
	for _, e := range journal.Events {
		kinds = append(kinds, e.Kind)
	}
	if len(journal.Events) == 0 {
		t.Fatal("nothing that happened to the commitment was recorded")
	}
	if !containsString(kinds, "effect_bound") {
		t.Fatalf("the events do not include the binding of the effect: %v", kinds)
	}
	for i := 1; i < len(journal.Events); i++ {
		if journal.Events[i].Seq <= journal.Events[i-1].Seq {
			t.Fatalf("the events are out of order: %v", journal.Events)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func hasTimeline(t *testing.T, s *Store, agID, fragment string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM timeline_events WHERE alert_group_id = $1 AND message LIKE '%' || $2 || '%'`,
		agID, fragment).Scan(&n); err != nil {
		t.Fatalf("read the history: %v", err)
	}
	return n > 0
}

// TestTheCallIsDecidedByTheRecord. What a call IS - create or change, which
// operation, which revision - is not something a worker gets to state. All
// three come from what the commitment already has, and the worker is told what
// to do rather than asked.
func TestTheCallIsDecidedByTheRecord(t *testing.T) {
	s := setupTestDB(t)

	t.Run("nothing sent yet means a create", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)
		begun := beginOne(t, s, intentID, token)

		if begun.AttemptKind != outbound.AttemptCreate ||
			begun.Operation != outbound.OperationSend {
			t.Fatalf("the worker was told to make a %s/%s", begun.AttemptKind, begun.Operation)
		}

		var (
			kind      string
			operation string
			revision  int64
		)
		if err := s.db.QueryRow(`
			SELECT attempt_kind, operation, applied_revision
			FROM outbound_attempts WHERE id = $1`, begun.AttemptID).
			Scan(&kind, &operation, &revision); err != nil {
			t.Fatalf("read the attempt: %v", err)
		}
		if kind != string(outbound.AttemptCreate) || operation != string(outbound.OperationSend) {
			t.Fatalf("the journal records a %s/%s", kind, operation)
		}

		// The revision is the one in the state that was rendered, which is
		// also the one the key was computed over. Nothing else can name it.
		if revision != begun.AppliedRevision {
			t.Fatalf("the attempt carries revision %d and reports %d",
				revision, begun.AppliedRevision)
		}
		var stored int64
		if err := s.db.QueryRow(
			`SELECT revision FROM outbound_group_snapshots WHERE alert_group_id = $1`, agID).
			Scan(&stored); err != nil {
			t.Fatalf("read the state: %v", err)
		}
		if revision != stored {
			t.Fatalf("the attempt applies revision %d against state at %d", revision, stored)
		}
	})

	t.Run("a refusal is recorded under the same shape", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)

		if _, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
			Preparation: outbound.PreparationTransient, ErrorClass: "rate_limited",
		}); err != nil {
			t.Fatalf("record the refusal: %v", err)
		}

		var kind, operation string
		if err := s.db.QueryRow(`
			SELECT attempt_kind, operation FROM outbound_attempts WHERE intent_id = $1`,
			intentID).Scan(&kind, &operation); err != nil {
			t.Fatalf("read the refusal: %v", err)
		}
		// It says which call did not happen - and it has to, because the
		// operator's guards read this column later.
		if kind != string(outbound.AttemptCreate) || operation != string(outbound.OperationSend) {
			t.Fatalf("the refusal was recorded as a %s/%s", kind, operation)
		}
	})

	t.Run("an object that already exists is not created again", func(t *testing.T) {
		agID := outboundGroup(t, s)
		intentID := admitOne(t, s, agID)[0]
		token := claimOne(t, s, intentID)

		// A card that has already been posted. Changing one is a call this
		// build does not make, so nothing is sent - and the record says which
		// call was needed rather than quietly creating a second object at the
		// same address.
		if _, err := s.db.Exec(
			`UPDATE outbound_intents SET receipt = $2::jsonb WHERE id = $1`,
			intentID, `{"channel":"C0001","ts":"1700000000.000100"}`); err != nil {
			t.Fatalf("give the commitment an object: %v", err)
		}

		result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
			IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
			Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if result.Outcome != outbound.BeginPreparedPermanent {
			t.Fatalf("a call this build cannot make answered %q", result.Outcome)
		}
		if got := statusOf(t, s, intentID); got != outbound.StatusPermanentFailed {
			t.Fatalf("the commitment is %s", got)
		}

		var kind, operation, class string
		if err := s.db.QueryRow(`
			SELECT attempt_kind, operation, COALESCE(error_class, '')
			FROM outbound_attempts WHERE intent_id = $1`, intentID).
			Scan(&kind, &operation, &class); err != nil {
			t.Fatalf("read the refusal: %v", err)
		}
		if kind != string(outbound.AttemptMutation) || class != "unsupported_operation" {
			t.Fatalf("the refusal reads as %s/%s (%s)", kind, operation, class)
		}
		if started := attemptsStarted(t, s, intentID); started != 0 {
			t.Fatalf("%d calls were made for a commitment nothing could be sent for", started)
		}
	})
}

// attemptsStarted counts the records that mean the network may have been
// reached. A refusal writes a row too, and the difference between the two is
// the whole point of the journal.
func attemptsStarted(t *testing.T, s *Store, intentID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_attempts WHERE intent_id = $1 AND record_kind = 'attempt'`,
		intentID).Scan(&n); err != nil {
		t.Fatalf("count the calls: %v", err)
	}
	return n
}

// TestStateNobodyCanReadEndsTheCommitment. An unreadable snapshot stops a send,
// and it has to stop the commitment with it.
//
// Left pending, it would be claimed, refused, backed off and claimed again for
// as long as the deadline allows - and the one door that could withdraw it, an
// operator decision, opens only for a commitment that has stopped moving. The
// delivery that cannot happen has to end somewhere a person can see it.
func TestStateNobodyCanReadEndsTheCommitment(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = jsonb_set(snapshot, '{title}', '"a different alert"')
		WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("corrupt the state: %v", err)
	}

	token := claimOne(t, s, intentID)
	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if result.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("a send against unreadable state answered %q", result.Outcome)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusPermanentFailed {
		t.Fatalf("the commitment is %s and will be handed out again", got)
	}
	if started := attemptsStarted(t, s, intentID); started != 0 {
		t.Fatal("a call was recorded for a message that was never composed")
	}

	var class, summary string
	if err := s.db.QueryRow(`
		SELECT COALESCE(error_class, ''), COALESCE(response_summary, '')
		FROM outbound_attempts WHERE intent_id = $1`, intentID).Scan(&class, &summary); err != nil {
		t.Fatalf("read the refusal: %v", err)
	}
	if class != "state_unreadable" || summary == "" {
		t.Fatalf("the refusal says %q/%q", class, summary)
	}
	if !hasTimeline(t, s, agID, "failed permanently") {
		t.Fatal("the alert says nothing about a notification that will never arrive")
	}

	// It is out of the queue, and out of it for good.
	makeDue(t, s, intentID)
	leased, err := s.ClaimDueIntents(context.Background(), testFamily, "slack",
		outbound.ClaimAny, 10, outbound.NotificationLease)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, l := range leased {
		if l.Intent.ID == intentID {
			t.Fatal("a commitment nothing can be sent for was handed out again")
		}
	}

	// And a person can still call it off.
	if result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionCancel,
		Reason: "the alert data is broken; handled by hand",
	}); result.Status != outbound.StatusCanceled {
		t.Fatalf("withdrawing it answered %q into %s", result.Outcome, result.Status)
	}
}

// TestARetryIntoUnreadableStateStillEnds is the same rule reached the other
// way: a person deciding to retry does not have to know whether the state can
// still be rendered, because the attempt that cannot be made ends the
// commitment rather than starting a loop.
func TestARetryIntoUnreadableStateStillEnds(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := stuckInReview(t, s, agID)

	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = jsonb_set(snapshot, '{title}', '"a different alert"')
		WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("corrupt the state: %v", err)
	}

	if result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionRetryCurrentGeneration,
		Reason: "the provider is back",
	}); result.Status != outbound.StatusPending {
		t.Fatalf("the retry answered %q into %s", result.Outcome, result.Status)
	}

	token := claimOne(t, s, intentID)
	result, err := s.BeginAttempt(context.Background(), outbound.BeginAttemptRequest{
		IntentID: intentID, LeaseToken: token, WorkerID: "worker-1",
		Preparation: outbound.PreparationReady, BoundEndpoint: "C0001",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if result.Outcome != outbound.BeginPreparedPermanent {
		t.Fatalf("the retry answered %q", result.Outcome)
	}
	if got := statusOf(t, s, intentID); got != outbound.StatusPermanentFailed {
		t.Fatalf("the retried commitment is %s", got)
	}

	// The operator door is open again, which is the whole point.
	if result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionCancel,
		Reason: "nothing more to try",
	}); result.Status != outbound.StatusCanceled {
		t.Fatalf("withdrawing it answered %q into %s", result.Outcome, result.Status)
	}
}

// TestALateResultIsKeptAsTheAttemptHadIt. A worker that comes back after
// recovery gave up on it may be carrying the only evidence that a message
// exists. What is kept is that evidence, completed from the attempt's own
// record - and it has to be complete, because there is nothing else left to
// reconstruct it from.
func TestALateResultIsKeptAsTheAttemptHadIt(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]

	token := claimOne(t, s, intentID)
	begun := beginOne(t, s, intentID, token)
	expireLease(t, s, intentID)
	if _, err := s.RecoverStaleAttempts(context.Background(), testFamily, 10); err != nil {
		t.Fatalf("recover: %v", err)
	}

	late := outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Completion: acceptedCompletion(),
		Receipt:    json.RawMessage(`{"channel":"C0001","ts":"1700000000.000100"}`),
		Summary:    "ok",
	}
	result, err := s.FinalizeDeliveryAttempt(context.Background(), late)
	if err != nil {
		t.Fatalf("finalize late: %v", err)
	}
	if result.Outcome != outbound.FinalizeLeaseLost || !result.ObservationRecorded {
		t.Fatalf("the late result was answered %q, recorded=%v",
			result.Outcome, result.ObservationRecorded)
	}

	// The same result again is the same fact, not a second one.
	repeat, err := s.FinalizeDeliveryAttempt(context.Background(), late)
	if err != nil {
		t.Fatalf("repeat the late result: %v", err)
	}
	if !repeat.ObservationRecorded {
		t.Fatal("a repeated late result stopped being kept")
	}

	journal, err := s.IntentJournal(context.Background(), intentID)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if len(journal.Observations) != 1 {
		t.Fatalf("the same late result was kept %d times", len(journal.Observations))
	}

	kept := journal.Observations[0]
	attempt := journal.Attempts[0]
	if kept.AttemptID != begun.AttemptID || kept.Outcome != outbound.OutcomeAccepted {
		t.Fatalf("the late result is %+v", kept)
	}
	// The revision is the attempt's, not something the worker supplied: it
	// supplied none, and a message with no revision cannot be reconciled with
	// the card it belongs to.
	if kept.AppliedRevision == nil || *kept.AppliedRevision != *attempt.AppliedRevision {
		t.Fatalf("the late result carries revision %v, the attempt applied %v",
			kept.AppliedRevision, attempt.AppliedRevision)
	}
	if kept.CompletionFingerprintVersion != attempt.CompletionFingerprintVersion {
		t.Fatalf("the late result was encoded under protocol %d, the attempt under %d",
			kept.CompletionFingerprintVersion, attempt.CompletionFingerprintVersion)
	}
	if len(kept.Receipt) == 0 || kept.Summary != "ok" {
		t.Fatalf("the late result lost what proves the message exists: %+v", kept)
	}
	if kept.ObservedAt.IsZero() {
		t.Fatal("the late result does not say when it arrived")
	}
}

// TestAWithdrawalDoesNotNeedTheState. Cancelling claims nothing and sends
// nothing. State that cannot be read has to stop a send - it must never trap a
// commitment whose one remaining option is to be called off.
func TestAWithdrawalDoesNotNeedTheState(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	intentID := stuckInReview(t, s, agID)

	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = jsonb_set(snapshot, '{title}', '"a different alert"')
		WHERE alert_group_id = $1`, agID); err != nil {
		t.Fatalf("corrupt the state: %v", err)
	}

	// Assuming it arrived is a claim about a message that state composed, so
	// it stops here.
	if _, err := s.ResolveAmbiguity(context.Background(), outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionAssumeAccepted,
		Actor: "nina", Reason: "the recipient said so",
	}); !errors.Is(err, ErrOutboundContract) {
		t.Fatalf("a delivery was assumed against state nobody can read: %v", err)
	}

	result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: intentID, Decision: outbound.DecisionCancel,
		Reason: "the incident was handled another way",
	})
	if result.Outcome != outbound.ResolveResolved || result.Status != outbound.StatusCanceled {
		t.Fatalf("withdrawing answered %q into %s", result.Outcome, result.Status)
	}
}
