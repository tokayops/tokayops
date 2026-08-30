package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// The schema tests below check what the DDL refuses, not what it stores.
//
// Every rule in outbound_schema.go exists because the state it forbids costs a
// delivery: a row that looks busy with nothing running, a lease that outlives
// its work, an attempt that belongs to somebody else. A CHECK nobody tries to
// break is a comment, so each case writes the row the invariant is meant to
// stop and expects the database to say no by name.

// outboundGroup creates the alert group the outbound rows hang off.
func outboundGroup(t *testing.T, s *Store) string {
	t.Helper()
	id := uuid.New().String()
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID:       id,
		AlertKey: "outbound-" + id,
		Status:   model.AlertGroupStatusProcessing,
		Title:    "outbound schema fixture",
		Severity: "critical",
		// One firing alert, because a group without any cannot be resolved by
		// an Alertmanager payload: a resolution for a fingerprint the incident
		// has never held belongs to an incident that is already over.
		Alerts: []model.Alert{{
			Fingerprint: "fp-1", Status: model.AlertStatusFiring,
			StartsAt: time.Unix(1700000000, 0),
			Labels:   map[string]string{"alertname": "DiskWillFill"},
		}},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create the alert group: %v", err)
	}
	return id
}

// digest32 stands in for a SHA-256 digest. The schema insists the fingerprints
// are exactly that long, so a fixture shipping a shorter one would be proving
// the wrong refusal.
func digest32(seed byte) []byte {
	d := make([]byte, 32)
	for i := range d {
		d[i] = seed + byte(i)
	}
	return d
}

// batchFixture is one admission claim, valid unless a case spoils it.
type batchFixture struct {
	ID           string
	Key          string
	Kind         string
	Family       string
	AlertGroupID *string
	Outcome      string
	IntentCount  int
	Fingerprint  []byte
}

func newBatch(agID string) batchFixture {
	id := uuid.New().String()
	return batchFixture{
		ID:           id,
		Key:          "batch-" + id,
		Kind:         "escalation",
		Family:       "notification",
		AlertGroupID: &agID,
		Outcome:      "admitted",
		IntentCount:  1,
		Fingerprint:  digest32(0x10),
	}
}

func (b batchFixture) insert(s *Store) error {
	// The frozen admission state goes in with every escalation claim, because
	// the schema says so: a commitment of one renders from it, and a claim
	// without it admits work that cannot be delivered.
	_, err := s.db.Exec(`
		INSERT INTO outbound_batches
			(id, batch_key, key_kind, delivery_family, grammar_version,
			 alert_group_id, fingerprint, fingerprint_version,
			 admission_outcome, intent_count,
			 admission_snapshot, admission_digest, admission_schema_version,
			 admission_revision)
		VALUES ($1, $2, $3, $4, 1, $5, $6, 1, $7, $8, $9, $10, 1, 0)`,
		b.ID, b.Key, b.Kind, b.Family, b.AlertGroupID, b.Fingerprint,
		b.Outcome, b.IntentCount, `{"frozen":true}`, digest32(0x11))
	return err
}

func (b batchFixture) mustInsert(t *testing.T, s *Store) batchFixture {
	t.Helper()
	if err := b.insert(s); err != nil {
		t.Fatalf("insert the batch: %v", err)
	}
	return b
}

// intentFixture is one commitment to one recipient, valid unless spoiled.
type intentFixture struct {
	ID                   string
	BatchID              string
	Key                  string
	AlertGroupID         *string
	Status               string
	CurrentAttemptID     *string
	LeaseToken           *string
	LockedUntil          *time.Time
	CancellationRequest  bool
	AttemptsInGeneration int
	FailureStreak        int
	GenerationNo         int
}

func newIntent(batchID string, agID string) intentFixture {
	id := uuid.New().String()
	return intentFixture{
		ID:           id,
		BatchID:      batchID,
		Key:          "intent-" + id,
		AlertGroupID: &agID,
		Status:       "pending",
	}
}

func (i intentFixture) insert(s *Store) error {
	_, err := s.db.Exec(`
		INSERT INTO outbound_intents
			(id, batch_id, idempotency_key, delivery_family, key_kind,
			 grammar_version, provider, target_kind, target_ref, alert_group_id,
			 form, completion_mode, ambiguity_policy, payload_schema_version,
			 payload, payload_digest, provider_key_codec_version, status, generation_no,
			 attempts_in_generation, failure_streak, cancellation_requested,
			 current_attempt_id, lease_token, locked_until,
			 not_before, next_attempt_at)
		VALUES ($1, $2, $3, 'notification', 'escalation',
			 1, 'slack', 'channel', 'C123', $4,
			 'editable', 'on_acceptance', 'retry', 1,
			 '{"slot":{"kind":"firehose","index":0},
			   "target":{"kind":"channel","ref":"C123"},
			   "interactive":true}'::jsonb, decode(repeat('ab', 32), 'hex'), 1, $5, $6,
			 $7, $8, $9,
			 $10, $11, $12,
			 now(), now())`,
		i.ID, i.BatchID, i.Key, i.AlertGroupID,
		i.Status, i.GenerationNo,
		i.AttemptsInGeneration, i.FailureStreak, i.CancellationRequest,
		i.CurrentAttemptID, i.LeaseToken, i.LockedUntil)
	return err
}

func (i intentFixture) mustInsert(t *testing.T, s *Store) intentFixture {
	t.Helper()
	if err := i.insert(s); err != nil {
		t.Fatalf("insert the intent: %v", err)
	}
	return i
}

// attemptFixture is one journal record. The defaults describe a started network
// attempt, which is the shape everything else is a deviation from.
type attemptFixture struct {
	ID             string
	IntentID       string
	AttemptNo      int
	RecordKind     string
	StartedAt      *time.Time
	FinishedAt     *time.Time
	Outcome        *string
	FinishReason   *string
	LeaseToken     *string
	Fingerprint    []byte
	FingerprintVer *int
}

func newAttempt(intentID string, no int) attemptFixture {
	started := time.Now()
	token := uuid.New().String()
	version := 1
	return attemptFixture{
		ID:             uuid.New().String(),
		IntentID:       intentID,
		AttemptNo:      no,
		RecordKind:     "attempt",
		StartedAt:      &started,
		LeaseToken:     &token,
		FingerprintVer: &version,
	}
}

func (a attemptFixture) insert(s *Store) error {
	// A nil byte slice reaches the driver as an empty bytea rather than as
	// NULL, and an empty fingerprint would satisfy a constraint written about
	// its absence. The case that leaves it out has to send NULL.
	var fingerprint any
	if a.Fingerprint != nil {
		fingerprint = a.Fingerprint
	}

	_, err := s.db.Exec(`
		INSERT INTO outbound_attempts
			(id, intent_id, attempt_no, record_kind, generation_no, attempt_kind,
			 operation, provider, lease_token, started_at, finished_at, outcome,
			 finish_reason, completion_fingerprint, completion_fingerprint_version)
		VALUES ($1, $2, $3, $4, 0, 'create',
			 'send', 'slack', $5, $6, $7, $8,
			 $9, $10, $11)`,
		a.ID, a.IntentID, a.AttemptNo, a.RecordKind,
		a.LeaseToken, a.StartedAt, a.FinishedAt, a.Outcome,
		a.FinishReason, fingerprint, a.FingerprintVer)
	return err
}

func (a attemptFixture) mustInsert(t *testing.T, s *Store) attemptFixture {
	t.Helper()
	if err := a.insert(s); err != nil {
		t.Fatalf("insert the attempt: %v", err)
	}
	return a
}

// rejectedBy fails the test unless the error names the constraint. Matching the
// name rather than "some error happened" is what keeps a case honest: a typo in
// the fixture would otherwise pass as proof of the rule.
func rejectedBy(t *testing.T, err error, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the database accepted a row %s exists to forbid", constraint)
	}
	if !strings.Contains(err.Error(), constraint) {
		t.Fatalf("expected %s to reject the row, got: %v", constraint, err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestOutboundSchemaIsIdempotent runs the whole block again on a database that
// already has it. InitDB is executed on every start, so "already applied" is
// the normal case, not the exception - and the foreign key added by its own
// statement is the one piece that has no IF NOT EXISTS of its own.
func TestOutboundSchemaIsIdempotent(t *testing.T) {
	s := setupTestDB(t)

	for i := 0; i < 2; i++ {
		if err := s.applyOutboundSchema(); err != nil {
			t.Fatalf("re-applying the outbound schema failed on pass %d: %v", i+1, err)
		}
	}

	var constraints int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'outbound_intents'::regclass`,
		outboundCurrentAttemptFK).Scan(&constraints); err != nil {
		t.Fatalf("count the composite foreign key: %v", err)
	}
	if constraints != 1 {
		t.Fatalf("expected exactly one %s, found %d", outboundCurrentAttemptFK, constraints)
	}

	// The rules added by their own guarded blocks. Each looks itself up before
	// adding, and a guard that looked up the wrong name would add a second copy
	// on every start until one of them was violated by a row.
	for _, rule := range outboundRulesAddedWithTheDigest {
		var copies int
		if err := s.db.QueryRow(`
			SELECT count(*) FROM pg_constraint
			WHERE conname = $1 AND conrelid = $2::regclass`,
			rule.name, rule.table).Scan(&copies); err != nil {
			t.Fatalf("count %s on %s: %v", rule.name, rule.table, err)
		}
		if copies != 1 {
			t.Errorf("found %d copies of %s on %s", copies, rule.name, rule.table)
		}
	}

	var indexes int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_outbound_batches_group_admission'`,
	).Scan(&indexes); err != nil {
		t.Fatalf("count the admission index: %v", err)
	}
	if indexes != 1 {
		t.Fatalf("expected exactly one admission index, found %d", indexes)
	}
}

// TestOutboundBatchAdmissionOutcomeIsStated pins the outcome of admission as a
// value rather than something read out of a counter. "Nobody to notify" is an
// answer the operator has to be able to see; inferring it from an empty set is
// how it would eventually be confused with "not admitted yet".
func TestOutboundBatchAdmissionOutcomeIsStated(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	cases := []struct {
		name       string
		outcome    string
		count      int
		constraint string
	}{
		{name: "admitted with recipients", outcome: "admitted", count: 3},
		{name: "no targets with an empty set", outcome: "no_targets", count: 0},
		{
			name: "no targets holding intents", outcome: "no_targets", count: 1,
			constraint: "outbound_batches_outcome_shape",
		},
		{
			name: "admitted holding nothing", outcome: "admitted", count: 0,
			constraint: "outbound_batches_outcome_shape",
		},
		{
			name: "an outcome nobody declared", outcome: "half_admitted", count: 1,
			constraint: "outbound_batches_outcome_known",
		},
		{
			name: "a negative count", outcome: "admitted", count: -1,
			constraint: "outbound_batches_count_nonneg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBatch(agID)
			// Each case needs its own group: the partial unique index below
			// allows one first admission per group, and that is a different
			// rule from the one under test here.
			b.AlertGroupID = ptr(outboundGroup(t, s))
			b.Outcome = tc.outcome
			b.IntentCount = tc.count

			err := b.insert(s)
			if tc.constraint == "" {
				if err != nil {
					t.Fatalf("a valid admission was rejected: %v", err)
				}
				return
			}
			rejectedBy(t, err, tc.constraint)
		})
	}
}

// TestOutboundFingerprintsAreDigests checks the length of every column that
// holds one. NOT NULL is only half a rule for a digest: an empty bytea is a
// value, and two producers that computed nothing would otherwise look like two
// producers that agreed.
func TestOutboundFingerprintsAreDigests(t *testing.T) {
	s := setupTestDB(t)

	lengths := []struct {
		name  string
		bytes []byte
	}{
		{name: "empty", bytes: []byte{}},
		{name: "one byte short", bytes: digest32(1)[:31]},
		{name: "one byte long", bytes: append(digest32(1), 0x00)},
	}

	t.Run("batch", func(t *testing.T) {
		for _, l := range lengths {
			t.Run(l.name, func(t *testing.T) {
				b := newBatch(outboundGroup(t, s))
				b.Fingerprint = l.bytes
				rejectedBy(t, b.insert(s), "outbound_batches_fingerprint_len")
			})
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		for _, l := range lengths {
			t.Run(l.name, func(t *testing.T) {
				_, err := s.db.Exec(`
					INSERT INTO outbound_group_snapshots
						(alert_group_id, revision, snapshot_schema_version,
						 snapshot, snapshot_digest)
					VALUES ($1, 0, 1, '{}'::jsonb, $2)`, outboundGroup(t, s), l.bytes)
				rejectedBy(t, err, "outbound_group_snapshots_digest_len")
			})
		}
	})

	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)
	intent := newIntent(batch.ID, agID).mustInsert(t, s)
	finished := time.Now().Add(time.Second)

	t.Run("attempt", func(t *testing.T) {
		for i, l := range lengths {
			t.Run(l.name, func(t *testing.T) {
				a := newAttempt(intent.ID, 100+i)
				a.FinishedAt = &finished
				a.Outcome = ptr("accepted")
				a.FinishReason = ptr("worker")
				a.Fingerprint = l.bytes
				rejectedBy(t, a.insert(s), "outbound_attempts_digest_len")
			})
		}
	})

	t.Run("observation", func(t *testing.T) {
		attempt := newAttempt(intent.ID, 200).mustInsert(t, s)
		for _, l := range lengths {
			t.Run(l.name, func(t *testing.T) {
				_, err := s.db.Exec(`
					INSERT INTO outbound_attempt_observations
						(id, attempt_id, observation_kind, outcome,
						 completion_fingerprint, completion_fingerprint_version)
					VALUES ($1, $2, 'late_finalize', 'accepted', $3, 1)`,
					uuid.New().String(), attempt.ID, l.bytes)
				rejectedBy(t, err, "outbound_attempt_observations_digest_len")
			})
		}
	})
}

// TestOutboundGroupHoldsOneFirstAdmission proves both halves of the partial
// index: one first admission per group, and room for a later re-admission of
// the same group under a different kind of claim. Without the second half the
// promise that an operator can re-admit a group would be impossible to keep.
func TestOutboundGroupHoldsOneFirstAdmission(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	newBatch(agID).mustInsert(t, s)

	second := newBatch(agID)
	if err := second.insert(s); err == nil {
		t.Fatal("a second first-admission was accepted for one group")
	} else if !strings.Contains(err.Error(), "idx_outbound_batches_group_admission") {
		t.Fatalf("expected the admission index to reject the row, got: %v", err)
	}

	replay := newBatch(agID)
	replay.Kind = "escalation_replay"
	if err := replay.insert(s); err != nil {
		t.Fatalf("a re-admission of the same group was refused: %v", err)
	}

	other := newBatch(outboundGroup(t, s))
	if err := other.insert(s); err != nil {
		t.Fatalf("another group could not be admitted: %v", err)
	}
}

// TestOutboundIntentStatusAndLeaseShape walks the states a row must never be
// in. Each of them is a way to lose a delivery quietly: a lease on a finished
// commitment hides the row, a send with no lease has nobody to finish it, and a
// cancellation flag anywhere but on a send in flight is a decision with nothing
// left to apply it to.
func TestOutboundIntentStatusAndLeaseShape(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)

	until := time.Now().Add(time.Minute)

	cases := []struct {
		name       string
		spoil      func(*intentFixture)
		constraint string
	}{
		{
			name:  "pending without a lease",
			spoil: func(i *intentFixture) {},
		},
		{
			name: "pending under a lease",
			spoil: func(i *intentFixture) {
				i.LeaseToken = ptr("token")
				i.LockedUntil = &until
			},
		},
		{
			name: "half a lease",
			spoil: func(i *intentFixture) {
				i.LeaseToken = ptr("token")
			},
			constraint: "outbound_intents_lease_pair",
		},
		{
			name: "a lease left on a finished commitment",
			spoil: func(i *intentFixture) {
				i.Status = "succeeded"
				i.LeaseToken = ptr("token")
				i.LockedUntil = &until
			},
			constraint: "outbound_intents_lease_only_when_working",
		},
		{
			name: "sending with nothing in flight",
			spoil: func(i *intentFixture) {
				i.Status = "sending"
				i.LeaseToken = ptr("token")
				i.LockedUntil = &until
			},
			constraint: "outbound_intents_sending_has_attempt",
		},
		{
			name: "a cancellation flag off a send",
			spoil: func(i *intentFixture) {
				i.CancellationRequest = true
			},
			constraint: "outbound_intents_cancel_flag_on_sending",
		},
		{
			name: "a negative counter",
			spoil: func(i *intentFixture) {
				i.FailureStreak = -1
			},
			constraint: "outbound_intents_counters_nonneg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := newIntent(batch.ID, agID)
			tc.spoil(&i)

			err := i.insert(s)
			if tc.constraint == "" {
				if err != nil {
					t.Fatalf("a valid intent was rejected: %v", err)
				}
				return
			}
			rejectedBy(t, err, tc.constraint)
		})
	}
}

// TestOutboundSendingRequiresBothAttemptAndLease follows the order production
// uses - insert the attempt, then move the intent - and then takes each half
// away in turn. A send with no lease cannot be finalised by anyone; a send with
// no attempt is a row that will never be recovered, because recovery looks for
// the attempt it has to close.
func TestOutboundSendingRequiresBothAttemptAndLease(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)
	intent := newIntent(batch.ID, agID).mustInsert(t, s)
	attempt := newAttempt(intent.ID, 1).mustInsert(t, s)

	if _, err := s.db.Exec(`
		UPDATE outbound_intents
		SET status = 'sending', current_attempt_id = $1,
		    lease_token = $2, locked_until = now() + interval '1 minute'
		WHERE id = $3`, attempt.ID, *attempt.LeaseToken, intent.ID); err != nil {
		t.Fatalf("the valid shape of a send was rejected: %v", err)
	}

	_, err := s.db.Exec(`
		UPDATE outbound_intents SET lease_token = NULL, locked_until = NULL
		WHERE id = $1`, intent.ID)
	rejectedBy(t, err, "outbound_intents_sending_has_lease")

	_, err = s.db.Exec(`
		UPDATE outbound_intents SET current_attempt_id = NULL WHERE id = $1`, intent.ID)
	rejectedBy(t, err, "outbound_intents_sending_has_attempt")
}

// TestOutboundCurrentAttemptBelongsToItsIntent is why the foreign key is
// composite. A single-column reference would let an intent point at an attempt
// somebody else started, and then "the attempt this intent is executing" would
// be a claim held by code alone - exactly the kind of claim that survives
// review and fails at three in the morning.
func TestOutboundCurrentAttemptBelongsToItsIntent(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)

	mine := newIntent(batch.ID, agID).mustInsert(t, s)
	theirs := newIntent(batch.ID, agID).mustInsert(t, s)
	foreign := newAttempt(theirs.ID, 1).mustInsert(t, s)

	_, err := s.db.Exec(`
		UPDATE outbound_intents
		SET status = 'sending', current_attempt_id = $1,
		    lease_token = 'token', locked_until = now() + interval '1 minute'
		WHERE id = $2`, foreign.ID, mine.ID)
	rejectedBy(t, err, outboundCurrentAttemptFK)
}

// TestOutboundAttemptRecordShapes pins the difference between the two kinds of
// journal record. A started attempt means the network might have been called
// and can only ever be resolved as such; a preparation record is the proof that
// no call was made. Letting one wear the other's shape would turn a refusal
// nobody sent into a duplicate somebody received.
func TestOutboundAttemptRecordShapes(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)
	intent := newIntent(batch.ID, agID).mustInsert(t, s)

	started := time.Now()
	finished := started.Add(time.Second)

	cases := []struct {
		name       string
		spoil      func(*attemptFixture)
		constraint string
	}{
		{
			name:  "a started attempt",
			spoil: func(a *attemptFixture) {},
		},
		{
			name: "a finished attempt",
			spoil: func(a *attemptFixture) {
				a.FinishedAt = &finished
				a.Outcome = ptr("accepted")
				a.FinishReason = ptr("worker")
				a.Fingerprint = digest32(1)
			},
		},
		{
			name: "an attempt with no lease behind it",
			spoil: func(a *attemptFixture) {
				a.LeaseToken = nil
			},
			constraint: "outbound_attempts_kind_shape",
		},
		{
			name: "an attempt that never started",
			spoil: func(a *attemptFixture) {
				a.StartedAt = nil
			},
			constraint: "outbound_attempts_kind_shape",
		},
		{
			name: "an attempt with no protocol version",
			spoil: func(a *attemptFixture) {
				a.FingerprintVer = nil
			},
			constraint: "outbound_attempts_fingerprint_shape",
		},
		{
			name: "a finished attempt with no fingerprint",
			spoil: func(a *attemptFixture) {
				a.FinishedAt = &finished
				a.Outcome = ptr("accepted")
				a.FinishReason = ptr("worker")
			},
			constraint: "outbound_attempts_fingerprint_shape",
		},
		{
			name: "a finish with no outcome",
			spoil: func(a *attemptFixture) {
				a.FinishedAt = &finished
				a.Fingerprint = digest32(1)
			},
			constraint: "outbound_attempts_finished_shape",
		},
		{
			name: "a finish before its start",
			spoil: func(a *attemptFixture) {
				before := started.Add(-time.Second)
				a.FinishedAt = &before
				a.Outcome = ptr("accepted")
				a.FinishReason = ptr("worker")
				a.Fingerprint = digest32(1)
			},
			constraint: "outbound_attempts_finished_shape",
		},
		{
			name: "an unfinished attempt already carrying a fingerprint",
			spoil: func(a *attemptFixture) {
				a.Fingerprint = digest32(4)
			},
			constraint: "outbound_attempts_fingerprint_shape",
		},
		{
			// The two halves are separate cases on purpose: one case breaking
			// both conditions would keep passing if either half of the rule
			// were dropped.
			name: "a preparation record with a fingerprint",
			spoil: func(a *attemptFixture) {
				a.RecordKind = "preparation"
				a.StartedAt = nil
				a.LeaseToken = nil
				a.FingerprintVer = nil
				a.FinishedAt = &finished
				a.Outcome = ptr("permanent_rejection")
				a.FinishReason = ptr("preparation")
				a.Fingerprint = digest32(5)
			},
			constraint: "outbound_attempts_kind_shape",
		},
		{
			name: "a preparation record with a protocol version",
			spoil: func(a *attemptFixture) {
				a.RecordKind = "preparation"
				a.StartedAt = nil
				a.LeaseToken = nil
				a.FinishedAt = &finished
				a.Outcome = ptr("permanent_rejection")
				a.FinishReason = ptr("preparation")
			},
			constraint: "outbound_attempts_kind_shape",
		},
		{
			name: "a preparation record",
			spoil: func(a *attemptFixture) {
				a.RecordKind = "preparation"
				a.StartedAt = nil
				a.LeaseToken = nil
				a.FingerprintVer = nil
				a.FinishedAt = &finished
				a.Outcome = ptr("permanent_rejection")
				a.FinishReason = ptr("preparation")
			},
		},
		{
			name: "a preparation record that claims to have started",
			spoil: func(a *attemptFixture) {
				a.RecordKind = "preparation"
				a.LeaseToken = nil
				a.FingerprintVer = nil
				a.FinishedAt = &finished
				a.Outcome = ptr("permanent_rejection")
				a.FinishReason = ptr("preparation")
			},
			constraint: "outbound_attempts_kind_shape",
		},
		{
			name: "a preparation record finished by a worker",
			spoil: func(a *attemptFixture) {
				a.RecordKind = "preparation"
				a.StartedAt = nil
				a.LeaseToken = nil
				a.FingerprintVer = nil
				a.FinishedAt = &finished
				a.Outcome = ptr("permanent_rejection")
				a.FinishReason = ptr("worker")
			},
			constraint: "outbound_attempts_kind_shape",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAttempt(intent.ID, i+1)
			a.StartedAt = &started
			tc.spoil(&a)

			err := a.insert(s)
			if tc.constraint == "" {
				if err != nil {
					t.Fatalf("a valid journal record was rejected: %v", err)
				}
				return
			}
			rejectedBy(t, err, tc.constraint)
		})
	}
}

// TestOutboundJournalIdentities checks the three identities the journal is read
// by. The attempt number is what "the last attempt" means, and two records
// sharing one would make that question unanswerable; an observation is
// identified by its attempt and kind, so a second, contradicting late result
// has to collide rather than quietly become a second truth.
func TestOutboundJournalIdentities(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)
	intent := newIntent(batch.ID, agID).mustInsert(t, s)
	attempt := newAttempt(intent.ID, 1).mustInsert(t, s)

	duplicate := newAttempt(intent.ID, 1)
	if err := duplicate.insert(s); err == nil {
		t.Fatal("two journal records took the same attempt number")
	} else if !strings.Contains(err.Error(), "outbound_attempts_no") {
		t.Fatalf("expected the attempt number to collide, got: %v", err)
	}

	observe := func(kind string) error {
		_, err := s.db.Exec(`
			INSERT INTO outbound_attempt_observations
				(id, attempt_id, observation_kind, outcome,
				 completion_fingerprint, completion_fingerprint_version)
			VALUES ($1, $2, $3, 'accepted', $4, 1)`,
			uuid.New().String(), attempt.ID, kind, digest32(2))
		return err
	}
	if err := observe("late_finalize"); err != nil {
		t.Fatalf("record a late result: %v", err)
	}
	if err := observe("late_finalize"); err == nil {
		t.Fatal("a second late result was recorded for one attempt and kind")
	}
	if err := observe("reconcile"); err != nil {
		t.Fatalf("a different kind of observation was refused: %v", err)
	}

	event := func(seq int) error {
		_, err := s.db.Exec(`
			INSERT INTO outbound_intent_events (id, intent_id, seq, kind)
			VALUES ($1, $2, $3, 'created')`,
			uuid.New().String(), intent.ID, seq)
		return err
	}
	if err := event(1); err != nil {
		t.Fatalf("record a lifecycle event: %v", err)
	}
	if err := event(1); err == nil {
		t.Fatal("two lifecycle events took the same sequence number")
	}
	if err := event(2); err != nil {
		t.Fatalf("the next lifecycle event was refused: %v", err)
	}
}

// TestOutboundSnapshotIsOnePerGroup pins the aggregate shape: the domain keeps
// the latest revision and only that one. A second row for a group would mean
// two answers to "what should be outside right now", and the worker has no way
// to choose between them.
func TestOutboundSnapshotIsOnePerGroup(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	insert := func(revision int64) error {
		_, err := s.db.Exec(`
			INSERT INTO outbound_group_snapshots
				(alert_group_id, revision, snapshot_schema_version, snapshot, snapshot_digest)
			VALUES ($1, $2, 1, '{}'::jsonb, $3)`, agID, revision, digest32(3))
		return err
	}

	if err := insert(0); err != nil {
		t.Fatalf("store the first revision: %v", err)
	}
	if err := insert(1); err == nil {
		t.Fatal("a group ended up with two snapshots")
	}

	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots SET revision = -1 WHERE alert_group_id = $1`,
		agID); err == nil {
		t.Fatal("a negative revision was accepted")
	}

	var revision sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT revision FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		agID).Scan(&revision); err != nil {
		t.Fatalf("read the snapshot back: %v", err)
	}
	if !revision.Valid || revision.Int64 != 0 {
		t.Fatalf("expected the stored revision to survive both refusals, got %v", revision)
	}
}

// TestOutboundSchemaSurvivesConcurrentFirstStart is the case the advisory lock
// exists for. `CREATE TABLE IF NOT EXISTS` is not atomic against a concurrent
// creator: two instances starting together against a fresh database collide
// inside the catalog, and neither can read that error as "somebody else got
// there first". Running the block sequentially proves nothing about it.
func TestOutboundSchemaSurvivesConcurrentFirstStart(t *testing.T) {
	s := setupTestDB(t)

	if _, err := s.db.Exec(`DROP TABLE IF EXISTS
		outbound_intent_events, outbound_attempt_observations, outbound_attempts,
		outbound_intents, outbound_group_snapshots, outbound_batches CASCADE`); err != nil {
		t.Fatalf("undo the outbound schema: %v", err)
	}

	errs := make(chan error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			errs <- s.applyOutboundSchema()
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent start %d: %v", i, err)
		}
	}

	for _, table := range []string{
		"outbound_batches", "outbound_group_snapshots", "outbound_intents",
		"outbound_attempts", "outbound_attempt_observations", "outbound_intent_events",
	} {
		var exists bool
		if err := s.db.QueryRow(
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists); err != nil {
			t.Fatalf("look for %s: %v", table, err)
		}
		if !exists {
			t.Errorf("%s did not survive the concurrent start", table)
		}
	}

	var constraints int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'outbound_intents'::regclass`,
		outboundCurrentAttemptFK).Scan(&constraints); err != nil {
		t.Fatalf("count the composite foreign key: %v", err)
	}
	if constraints != 1 {
		t.Errorf("expected exactly one %s after a concurrent start, found %d",
			outboundCurrentAttemptFK, constraints)
	}
}

// TestOutboundIndexesAreDeclaredOnce pins the index inventory.
//
// An index that repeats a unique constraint is paid for on every write and read
// by nothing, and it is the easiest thing in a schema to add twice: the
// constraint states an identity, the index looks like it states a lookup, and
// both end up over the same columns. The rule is therefore stated as a set: an
// index on these tables is either the one a constraint brought with it, or one
// of the seven declared here - and an index the schema replaced has to be gone
// from the database, not merely absent from the file.
func TestOutboundIndexesAreDeclaredOnce(t *testing.T) {
	s := setupTestDB(t)

	declared := map[string]bool{
		"idx_outbound_batches_group_admission": false,
		"idx_outbound_intents_claim":           false,
		"idx_outbound_intents_first_attempt":   false,
		"idx_outbound_intents_expiring":        false,
		"idx_outbound_intents_stale":           false,
		"idx_outbound_intents_group":           false,
		"idx_outbound_intents_status":          false,
	}

	rows, err := s.db.Query(`
		SELECT c.relname
		FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		WHERE n.nspname = 'public'
		  AND t.relname LIKE 'outbound\_%'
		  AND NOT EXISTS (
			  SELECT 1 FROM pg_constraint con WHERE con.conindid = i.indexrelid
		  )`)
	if err != nil {
		t.Fatalf("list the outbound indexes: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan an index name: %v", err)
		}
		seen, ok := declared[name]
		if !ok {
			t.Errorf("%s is an index nobody declared: either it belongs in "+
				"outbound_schema.go with a reason, or it repeats a constraint", name)
			continue
		}
		if seen {
			t.Errorf("%s exists more than once", name)
		}
		declared[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the index list: %v", err)
	}

	for name, seen := range declared {
		if !seen {
			t.Errorf("%s is declared in the schema but missing from the database", name)
		}
	}
}

// TestACommitmentCannotBeAddressedTwoWays. The columns decide where a message
// goes and the payload decides what is written, so a row where they disagree
// delivers what was composed for one person into the channel named beside it.
// A channel refuses such a row before it sends; the point of this rule is that
// nobody can write one.
func TestACommitmentCannotBeAddressedTwoWays(t *testing.T) {
	s := setupTestDB(t)
	agID := outboundGroup(t, s)

	// A claim per kind. A commitment points at all of its claim's identity, so
	// a replay under an escalation's claim is refused before its payload is
	// looked at, and the rule under test would never be reached.
	claims := map[string]string{}
	for _, kind := range []string{"escalation", "escalation_replay"} {
		b := newBatch(agID)
		b.Kind = kind
		claims[kind] = b.mustInsert(t, s).ID
	}

	insert := func(kind, targetKind, targetRef, payload string) error {
		_, err := s.db.Exec(`
			INSERT INTO outbound_intents (
				id, batch_id, idempotency_key, delivery_family, key_kind, grammar_version,
				provider, target_kind, target_ref, alert_group_id, form, completion_mode,
				ambiguity_policy, payload_schema_version, payload, payload_digest,
				provider_key_codec_version, status, desired_revision, not_before,
				next_attempt_at)
			VALUES ($1, $2, $1, 'notification', $7, 1,
			        'slack', $3, $4, $5, 'editable', 'on_acceptance',
			        'retry', 1, $6::jsonb, decode(repeat('ab', 32), 'hex'),
			        1, 'pending', 0, now(), now())`,
			uuid.New().String(), claims[kind], targetKind, targetRef, agID, payload, kind)
		return err
	}

	agrees := `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C0001"},"interactive":true}`

	// Both escalation kinds share one payload shape, so the rule covers both.
	// A replay admitted with a payload addressed elsewhere would deliver the
	// same way the original would.
	for _, kind := range []string{"escalation", "escalation_replay"} {
		if err := insert(kind, "channel", "C0001", agrees); err != nil {
			t.Fatalf("a %s that agrees with itself was refused: %v", kind, err)
		}

		for name, mismatch := range map[string]string{
			"a private message addressed to a channel": `{"slot":{"kind":"firehose"},"target":{"kind":"user","ref":"user-1"},"interactive":true}`,
			"the right kind, the wrong recipient":      `{"slot":{"kind":"firehose"},"target":{"kind":"channel","ref":"C9999"},"interactive":true}`,
			"a payload with no target at all":          `{"slot":{"kind":"firehose"},"interactive":true}`,
		} {
			t.Run(kind+": "+name, func(t *testing.T) {
				if err := insert(kind, "channel", "C0001", mismatch); err == nil {
					t.Fatal("the database accepted a commitment addressed two ways")
				}
			})
		}
	}
}

// TestAHandoverNamesOnePersonInBothPlaces. The two rules a handover carries of
// its own, and neither is the escalation rule extended.
//
// The first is the same idea as the escalation one - the columns say where the
// message goes, the payload says who it greets - and it is stated separately so
// that a payload with no target at all is refused rather than compared against
// nothing. The second is about the business key: a handover's key carries the
// occurrence, the provider and the user id and NOT the kind of target, so one
// aimed at a channel would share a key with one aimed at the person of that id.
func TestAHandoverNamesOnePersonInBothPlaces(t *testing.T) {
	s := setupTestDB(t)

	batch := newBatch("")
	batch.Kind, batch.Family, batch.AlertGroupID = "handoff", "handoff", nil
	batch.mustInsert(t, s)

	insert := func(targetKind, targetRef, payload string) error {
		_, err := s.db.Exec(`
			INSERT INTO outbound_intents (
				id, batch_id, idempotency_key, delivery_family, key_kind, grammar_version,
				provider, target_kind, target_ref, form, completion_mode,
				ambiguity_policy, payload_schema_version, payload, payload_digest,
				provider_key_codec_version, status, desired_revision, not_before,
				next_attempt_at)
			VALUES ($1, $2, $1, 'handoff', 'handoff', 1,
			        'slack', $3, $4, 'one_shot', 'on_acceptance',
			        'retry', 1, $5::jsonb, decode(repeat('ab', 32), 'hex'),
			        1, 'pending', 0, now(), now())`,
			uuid.New().String(), batch.ID, targetKind, targetRef, payload)
		return err
	}

	const shift = `"kind":"handoff","team_name":"Backend","schedule_id":"sched-1",` +
		`"timezone":"UTC","grid_slot_start":"2026-05-04T11:00:00Z",` +
		`"assignment_start":"2026-05-04T11:00:00Z","assignment_end":"2026-05-04T19:00:00Z"`

	if err := insert("user", "u-alice",
		`{`+shift+`,"target":{"kind":"user","ref":"u-alice"}}`); err != nil {
		t.Fatalf("an announcement that agrees with itself was refused: %v", err)
	}

	for name, row := range map[string]struct {
		targetKind, targetRef, payload string
	}{
		"greeting somebody else": {"user", "u-alice",
			`{` + shift + `,"target":{"kind":"user","ref":"u-bob"}}`},
		"greeting nobody at all": {"user", "u-alice", `{` + shift + `}`},
		"a shift taken by a channel": {"channel", "C0001",
			`{` + shift + `,"target":{"kind":"channel","ref":"C0001"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			if err := insert(row.targetKind, row.targetRef, row.payload); err == nil {
				t.Fatal("the database accepted an announcement it exists to forbid")
			}
		})
	}
}

// TestTheTargetRuleReachesAnExistingDatabase.
//
// Declared inside CREATE TABLE IF NOT EXISTS, a rule added later never appears
// on a database that already exists - and every one of those keeps accepting
// exactly the rows it forbids. It is therefore added by its own statement on
// every start, and this drops it and asks the schema to put it back.
func TestTheTargetRuleReachesAnExistingDatabase(t *testing.T) {
	s := setupTestDB(t)

	if _, err := s.db.Exec(
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` +
			outboundTargetAgreementConstraint); err != nil {
		t.Fatalf("drop the constraint: %v", err)
	}
	// The statement itself, not the whole schema: what is being checked is that
	// this rule is applied on every start, and re-running every other statement
	// in the middle of a suite is a side effect nobody asked for.
	if _, err := s.db.Exec(outboundTargetAgreementDDL); err != nil {
		t.Fatalf("re-apply the rule: %v", err)
	}

	var present int
	if err := s.db.QueryRow(`
		SELECT count(*) FROM pg_constraint
		WHERE conname = $1 AND conrelid = 'outbound_intents'::regclass`,
		outboundTargetAgreementConstraint).Scan(&present); err != nil {
		t.Fatalf("look for the constraint: %v", err)
	}
	if present != 1 {
		t.Fatalf("the rule is on the table %d times after a restart", present)
	}

	// And it is applied to what gets written from now on, which is the point of
	// adding it to a database that predates it.
	agID := outboundGroup(t, s)
	batch := newBatch(agID).mustInsert(t, s)
	_, err := s.db.Exec(`
		INSERT INTO outbound_intents (
			id, batch_id, idempotency_key, delivery_family, key_kind, grammar_version,
			provider, target_kind, target_ref, alert_group_id, form, completion_mode,
			ambiguity_policy, payload_schema_version, payload, payload_digest,
			provider_key_codec_version, status, desired_revision, not_before,
			next_attempt_at)
		VALUES ($1, $2, $1, 'notification', 'escalation', 1,
		        'slack', 'channel', 'C0001', $3, 'editable', 'on_acceptance',
		        'retry', 1,
		        '{"slot":{"kind":"firehose"},"target":{"kind":"user","ref":"user-1"}}'::jsonb,
		        decode(repeat('ab', 32), 'hex'),
		        1, 'pending', 0, now(), now())`,
		uuid.New().String(), batch.ID, agID)
	if err == nil {
		t.Fatal("a commitment addressed two ways was written after the rule was restored")
	}
}

// TestARowCannotBeInTwoReceiptStates.
//
// The three states are the whole reason erasure can remove coordinates without
// removing the fact: none, usable, redacted. Written as three columns they can
// disagree, and a row claiming both "nothing was ever sent" and "its
// coordinates were redacted" is a row nobody can interpret afterwards - least
// of all the state machine, which would offer to send the message again.
//
// The database is what holds this, not the writers: there are four of them
// already and every one of them writes all three columns.
func TestARowCannotBeInTwoReceiptStates(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()

	// The constraint is validated, not merely declared for future rows.
	for _, table := range []string{
		"outbound_intents", "outbound_attempts", "outbound_attempt_observations",
	} {
		var validated bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT convalidated FROM pg_constraint
			WHERE conname = $1 AND conrelid = $2::regclass`,
			outboundReceiptStateConstraint, table).Scan(&validated); err != nil {
			t.Fatalf("read the constraint on %s: %v", table, err)
		}
		if !validated {
			t.Errorf("the receipt-state rule on %s is declared but never checked "+
				"against the rows that are already there", table)
		}
	}

	agID := outboundGroup(t, s)
	intentID := admitOne(t, s, agID)[0]
	token := claimOne(t, s, intentID)
	attemptID := beginOne(t, s, intentID, token).AttemptID

	rows := map[string]string{
		"outbound_intents":  `UPDATE outbound_intents SET %s WHERE id = '` + intentID + `'`,
		"outbound_attempts": `UPDATE outbound_attempts SET %s WHERE id = '` + attemptID + `'`,
	}

	// Every combination the three columns can be put in that means nothing.
	//
	// Each carries the name that goes with its coordinates, so that the rule
	// under test is the one that fires: a commitment keeps the object's name
	// beside them, and a row with coordinates and no name would be refused by
	// THAT rule first and prove nothing about these three states. The name rule
	// is checked on its own below.
	nonsense := []struct {
		name string
		set  string
		ref  string
	}{
		{
			name: "nothing was sent, and here are its coordinates",
			set:  `receipt_recorded = FALSE, receipt = '{"a":1}'::jsonb, receipt_redacted_at = NULL`,
			ref:  `'C0001/1700000000.000100'`,
		},
		{
			name: "nothing was sent, and its coordinates were redacted",
			set:  `receipt_recorded = FALSE, receipt = NULL, receipt_redacted_at = now()`,
			ref:  `NULL`,
		},
		{
			name: "something was sent, and there is neither a receipt nor a redaction",
			set:  `receipt_recorded = TRUE, receipt = NULL, receipt_redacted_at = NULL`,
			ref:  `NULL`,
		},
		{
			name: "the coordinates are both present and redacted",
			set:  `receipt_recorded = TRUE, receipt = '{"a":1}'::jsonb, receipt_redacted_at = now()`,
			ref:  `'C0001/1700000000.000100'`,
		},
	}

	for table, shape := range rows {
		for _, bad := range nonsense {
			t.Run(table+": "+bad.name, func(t *testing.T) {
				set := bad.set
				if table == "outbound_intents" {
					set += `, receipt_ref = ` + bad.ref
				}
				_, err := s.db.ExecContext(ctx, fmt.Sprintf(shape, set))
				if err == nil {
					t.Fatal("the database accepted a row in no receipt state at all")
				}
				if !strings.Contains(err.Error(), outboundReceiptStateConstraint) {
					t.Fatalf("refused by something else: %v", err)
				}
			})
		}
	}

	// And the rule beside it, on the two halves that have to travel together.
	// Coordinates nobody can address are a card no change can reach; a name
	// with nothing behind it is a row that would send one to an object it
	// cannot describe.
	for _, half := range []struct {
		name string
		set  string
	}{
		{
			name: "coordinates with no name",
			set: `receipt_recorded = TRUE, receipt = '{"a":1}'::jsonb, ` +
				`receipt_redacted_at = NULL, receipt_ref = NULL`,
		},
		{
			name: "a name with no coordinates",
			set: `receipt_recorded = TRUE, receipt = NULL, ` +
				`receipt_redacted_at = now(), receipt_ref = 'C0001/1700000000.000100'`,
		},
		{
			name: "a name that says nothing",
			set: `receipt_recorded = TRUE, receipt = '{"a":1}'::jsonb, ` +
				`receipt_redacted_at = NULL, receipt_ref = ''`,
		},
	} {
		t.Run("outbound_intents: "+half.name, func(t *testing.T) {
			_, err := s.db.ExecContext(ctx, fmt.Sprintf(rows["outbound_intents"], half.set))
			if err == nil {
				t.Fatal("the database accepted a message half of which is missing")
			}
			if !strings.Contains(err.Error(), outboundReceiptNameConstraint) {
				t.Fatalf("refused by something else: %v", err)
			}
		})
	}

	// The observations table has no row of its own here, so it is checked by
	// inserting one directly - the shape is what matters, not the history.
	for _, bad := range nonsense {
		t.Run("outbound_attempt_observations: "+bad.name, func(t *testing.T) {
			_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
				INSERT INTO outbound_attempt_observations (
					id, attempt_id, observation_kind, outcome,
					completion_fingerprint, completion_fingerprint_version,
					receipt_recorded, receipt, receipt_redacted_at)
				VALUES ('%s', '%s', 'late_finalize', 'accepted', $1, 1, %s)`,
				uuid.New().String(), attemptID,
				strings.NewReplacer("receipt_recorded = ", "", "receipt = ", "",
					"receipt_redacted_at = ", "").Replace(bad.set)), digest32(0x40))
			if err == nil {
				t.Fatal("the database accepted an observation in no receipt state at all")
			}
			if !strings.Contains(err.Error(), outboundReceiptStateConstraint) {
				t.Fatalf("refused by something else: %v", err)
			}
		})
	}
}
