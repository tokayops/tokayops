package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// Admitting a shift change through the same door an escalation goes through,
// and the things that have to be different about it.

func handoffOccurrence(schedule string) keys.Occurrence {
	return keys.Occurrence{
		Kind:            keys.HandoffShiftChange,
		ScheduleID:      schedule,
		Source:          "rotation",
		GroupID:         "g-a",
		UserIDs:         []string{"u-alice", "u-bob"},
		AssignmentStart: time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		RevisionID:      "rev-7",
	}
}

func handoffBatch(t *testing.T, schedule string, recipients ...keys.HandoffRecipient) outbound.Batch {
	t.Helper()
	admission, err := keys.HandoffBatch{
		Occurrence:         handoffOccurrence(schedule),
		TeamName:           "Backend",
		Timezone:           "UTC",
		GridSlotStart:      time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC),
		AssignmentEnd:      time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
		MaxAge:             time.Hour,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Recipients:         recipients,
	}.Admit()
	if err != nil {
		t.Fatalf("build the admission: %v", err)
	}
	return outbound.Batch{
		Admission: admission,
		Context:   outbound.AnnouncingShiftChange(),
		Actor:     "notifier",
	}
}

// seedUsers makes the recipients exist. Admission locks every person it
// promises to reach, and a person who is not there is a batch refused.
func seedUsers(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.db.Exec(
			`INSERT INTO users (id, email, name, role, created_at)
			 VALUES ($1, $1 || '@example.com', $1, 'user', now())
			 ON CONFLICT (id) DO NOTHING`, id); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
}

func announceTo(provider, user string) keys.HandoffRecipient {
	return keys.HandoffRecipient{
		Provider: provider, UserID: user,
		Timing:          keys.TimingSpec{Kind: keys.TimingRelativeToAdmission},
		CompletionMode:  keys.CompletionOnAcceptance,
		AmbiguityPolicy: keys.PolicyRetry,
	}
}

// TestAShiftChangeIsAdmittedThroughTheSameDoor.
//
// One command, two forms of context. The half that is shared - what is already
// claimed, locking the recipients, writing the claim and the commitments - is
// the half a second implementation would eventually disagree about, and the
// disagreement would be about idempotency.
func TestAShiftChangeIsAdmittedThroughTheSameDoor(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice", "u-bob")

	result, err := s.SubmitBatch(context.Background(),
		handoffBatch(t, "sched-1", announceTo("slack", "u-alice"), announceTo("telegram", "u-bob")))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if result.Outcome != outbound.SubmitCreated {
		t.Fatalf("the announcement was %q", result.Outcome)
	}
	if len(result.IntentIDs) != 2 {
		t.Fatalf("%d commitments were made", len(result.IntentIDs))
	}

	// The claim carries the handover family and no alert group, and its frozen
	// state is absent rather than empty: an announcement has nothing to freeze.
	var family, kind string
	var groupID, snapshot, digest, schemaVersion, revision any
	if err := s.db.QueryRow(`
		SELECT delivery_family, key_kind, alert_group_id, admission_snapshot,
		       admission_digest, admission_schema_version, admission_revision
		FROM outbound_batches WHERE id = $1`, result.BatchID).
		Scan(&family, &kind, &groupID, &snapshot, &digest, &schemaVersion, &revision); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if family != string(keys.FamilyHandoff) || kind != string(keys.KindHandoff) {
		t.Fatalf("the claim is a %s in the %s family", kind, family)
	}
	for name, value := range map[string]any{
		"alert group": groupID, "frozen state": snapshot, "digest": digest,
		"schema version": schemaVersion, "revision": revision,
	} {
		if value != nil {
			t.Errorf("the announcement's claim carries a %s: %v", name, value)
		}
	}

	// And the commitments are one-shot, in the handover family, with the
	// deadline the batch derived.
	rows, err := s.db.Query(`
		SELECT delivery_family, key_kind, form, target_kind, target_ref,
		       expires_at IS NOT NULL
		FROM outbound_intents WHERE batch_id = $1 ORDER BY target_ref`, result.BatchID)
	if err != nil {
		t.Fatalf("read the commitments: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var family, kind, form, targetKind, targetRef string
		var hasDeadline bool
		if err := rows.Scan(&family, &kind, &form, &targetKind, &targetRef, &hasDeadline); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if family != string(keys.FamilyHandoff) || kind != string(keys.KindHandoff) {
			t.Errorf("a commitment is a %s in the %s family", kind, family)
		}
		if form != string(outbound.FormOneShot) {
			t.Errorf("an announcement is %q", form)
		}
		if targetKind != string(keys.TargetUser) {
			t.Errorf("an announcement is addressed to a %q", targetKind)
		}
		if !hasDeadline {
			t.Errorf("the announcement to %s never expires", targetRef)
		}
	}
	if seen != 2 {
		t.Fatalf("%d commitments were written", seen)
	}
}

// TestAnAnnouncementTouchesNothingInTheAlertDomain.
//
// The difference between the two contexts is exactly this, so it is asserted
// rather than left to the shape of the code: no snapshot row, no timeline line,
// nothing written on any alert group.
func TestAnAnnouncementTouchesNothingInTheAlertDomain(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	seedUsers(t, s, "u-alice")

	// A live alert group beside it, so "nothing changed" is a statement about
	// a table with rows in it rather than about an empty database.
	agID := outboundGroup(t, s)
	admitOne(t, s, agID)

	before := alertDomainState(t, s)

	if _, err := s.SubmitBatch(context.Background(),
		handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))); err != nil {
		t.Fatalf("admit: %v", err)
	}

	if now := alertDomainState(t, s); now != before {
		t.Errorf("announcing a shift change wrote in the alert domain:\n  was %+v\n  now %+v",
			before, now)
	}
}

// alertDomainState is everything an escalation admission writes outside the
// outbound tables, counted.
type domainState struct {
	Snapshots  int
	Timeline   int
	Groups     int
	WithPolicy int
}

func alertDomainState(t *testing.T, s *Store) domainState {
	t.Helper()
	var state domainState
	if err := s.db.QueryRow(`
		SELECT (SELECT count(*) FROM outbound_group_snapshots),
		       (SELECT count(*) FROM timeline_events),
		       (SELECT count(*) FROM alert_groups),
		       (SELECT count(*) FROM alert_groups WHERE policy_id IS NOT NULL
		                                             OR oncall_snapshot IS NOT NULL)`).
		Scan(&state.Snapshots, &state.Timeline, &state.Groups, &state.WithPolicy); err != nil {
		t.Fatalf("read the alert domain: %v", err)
	}
	return state
}

// TestARepeatedAnnouncementIsTheSameAnnouncement. The claim is held once and
// forever; a producer whose reply was lost asks again and is told the work
// exists, with the same claim and the same commitments.
func TestARepeatedAnnouncementIsTheSameAnnouncement(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	first, err := s.SubmitBatch(context.Background(),
		handoffBatch(t, "sched-1", announceTo("slack", "u-alice")))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	repeat, err := s.SubmitBatch(context.Background(),
		handoffBatch(t, "sched-1", announceTo("slack", "u-alice")))
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if repeat.Outcome != outbound.SubmitExisting {
		t.Fatalf("the repeat answered %q", repeat.Outcome)
	}
	if repeat.BatchID != first.BatchID {
		t.Fatalf("the repeat found claim %q, the work is under %q", repeat.BatchID, first.BatchID)
	}
}

// TestAClaimOfOneKindIsNotAnAnswerAboutAnother.
//
// The subject is nullable, and both directions are stated: a claim that names
// an alert group is not an answer about one that names none, and the reverse.
// SQL will not make this comparison - NULL is neither equal nor unequal to
// NULL - so it is made in code, where "absent" and "different" are two answers.
func TestAClaimOfOneKindIsNotAnAnswerAboutAnother(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	batch := handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))
	result, err := s.SubmitBatch(context.Background(), batch)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	// The claim is given an alert group, the way an escalation's would have
	// one. Nothing in this build writes that pair; a row from one that did
	// would be answered as if it were this announcement's.
	agID := outboundGroup(t, s)
	if _, err := s.db.Exec(
		`UPDATE outbound_batches SET alert_group_id = $2 WHERE id = $1`,
		result.BatchID, agID); err != nil {
		t.Fatalf("re-label the claim: %v", err)
	}

	_, err = s.SubmitBatch(context.Background(), batch)
	if err == nil {
		t.Fatal("a claim about an alert group answered for an announcement about none")
	}
	if !strings.Contains(err.Error(), "no alert group") {
		t.Fatalf("the refusal does not say what it is about: %v", err)
	}
}
