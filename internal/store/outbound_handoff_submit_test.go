package store

import (
	"context"
	"errors"
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

// handoffBatch announces the shift these tests are written around: a fixed one,
// so that what is stored can be compared against written-down values.
//
// Its slot is in the past, which admission allows on purpose - a shift that came
// and went while the system was stopped is still a fact - and which means the
// commitments are born expired. A test that has to CLAIM one needs a shift that
// has not ended yet, and takes handoffAnnouncedFor.
func handoffBatch(t *testing.T, schedule string, recipients ...keys.HandoffRecipient) outbound.Batch {
	t.Helper()
	return handoffAnnouncedFor(t, schedule,
		time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC), recipients...)
}

func handoffAnnouncedFor(t *testing.T, schedule string, slotStart time.Time,
	recipients ...keys.HandoffRecipient) outbound.Batch {

	t.Helper()
	occurrence := handoffOccurrence(schedule)
	occurrence.AssignmentStart = slotStart.UTC()
	admission, err := keys.HandoffBatch{
		Occurrence:         occurrence,
		TeamName:           "Backend",
		Timezone:           "UTC",
		GridSlotStart:      slotStart.UTC(),
		AssignmentEnd:      slotStart.UTC().Add(8 * time.Hour),
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

// TestARepeatedAnnouncementToNobodyIsAlsoTheSameAnnouncement.
//
// The claim with nothing under it, which is the one a repeat could most easily
// miss: a no_targets admission writes a batch row and zero commitments, so
// there is nothing to find it by except the batch itself. It also carries a
// different fingerprint from an admitted one - the outcome is in the material -
// so being recognised at all means being recognised under its own.
//
// A repeat that answered anything else would be a shift change nobody can be
// told about being re-submitted on every tick of the detector, forever: the
// producer moves its cache on `existing` exactly as it does on `created`, and
// on nothing else.
func TestARepeatedAnnouncementToNobodyIsAlsoTheSameAnnouncement(t *testing.T) {
	s := setupTestDB(t)

	first, err := s.SubmitBatch(context.Background(), handoffBatch(t, "sched-empty"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if first.Outcome != outbound.SubmitCreated || len(first.IntentIDs) != 0 {
		t.Fatalf("an announcement to nobody answered %q with %d commitments",
			first.Outcome, len(first.IntentIDs))
	}
	var outcome string
	if err := s.db.QueryRow(
		`SELECT admission_outcome FROM outbound_batches WHERE id = $1`,
		first.BatchID).Scan(&outcome); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	if outcome != string(keys.OutcomeNoTargets) {
		t.Fatalf("the claim was recorded as %q", outcome)
	}

	repeat, err := s.SubmitBatch(context.Background(), handoffBatch(t, "sched-empty"))
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if repeat.Outcome != outbound.SubmitExisting {
		t.Fatalf("the repeat answered %q; the occurrence would be submitted again "+
			"on every tick", repeat.Outcome)
	}
	if repeat.BatchID != first.BatchID {
		t.Fatalf("the repeat found claim %q, the first was %q", repeat.BatchID, first.BatchID)
	}

	var claims int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM outbound_batches WHERE key_kind = 'handoff'`).
		Scan(&claims); err != nil {
		t.Fatalf("count the claims: %v", err)
	}
	if claims != 1 {
		t.Fatalf("%d claims were written for one shift change", claims)
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

// TestTheKindAndTheContextHaveToBeTheSameClaim.
//
// They are two exported halves of one fact and nothing in the type system holds
// them together. The family is derived from the KIND and the execution branch
// is chosen by the CONTEXT, so a pair that disagrees writes rows into one
// family while updating the alert group, its snapshot and its timeline as
// though for another - and the worker then reads an escalation payload as a
// handover one. The work sticks or ends, and nobody is paged.
//
// Only ONE half of a correct batch is changed in each case, and it is the kind:
// changing the context instead is caught by the subject rules inside each arm,
// which is a different guard and would leave this one unproven.
func TestTheKindAndTheContextHaveToBeTheSameClaim(t *testing.T) {
	t.Run("an escalation whose kind says handover", func(t *testing.T) {
		s := setupTestDB(t)
		s.SetRenderEnvironment("https://tokay.example", "UTC")
		agID := outboundGroup(t, s)

		batch := outboundAdmission(t, agID, "first")
		batch.Admission.Kind = keys.KindHandoff

		before := alertDomainState(t, s)
		if _, err := s.SubmitBatch(context.Background(), batch); err == nil {
			t.Fatal("an escalation was written into the handover family")
		} else if !errors.Is(err, ErrOutboundContract) {
			t.Fatalf("the refusal is not a contract violation: %v", err)
		}
		assertNothingAdmitted(t, s, before)
	})

	t.Run("an announcement whose kind says escalation", func(t *testing.T) {
		s := setupTestDB(t)
		s.SetRenderEnvironment("https://tokay.example", "UTC")
		seedUsers(t, s, "u-alice")
		outboundGroup(t, s)

		batch := handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))
		batch.Admission.Kind = keys.KindEscalation

		before := alertDomainState(t, s)
		if _, err := s.SubmitBatch(context.Background(), batch); err == nil {
			t.Fatal("an announcement was written into the paging family")
		} else if !errors.Is(err, ErrOutboundContract) {
			t.Fatalf("the refusal is not a contract violation: %v", err)
		}
		assertNothingAdmitted(t, s, before)
	})
}

// assertNothingAdmitted: no claim, no commitment, and the alert domain exactly
// as it was.
func assertNothingAdmitted(t *testing.T, s *Store, before domainState) {
	t.Helper()
	var batches, intents int
	if err := s.db.QueryRow(`
		SELECT (SELECT count(*) FROM outbound_batches),
		       (SELECT count(*) FROM outbound_intents)`).Scan(&batches, &intents); err != nil {
		t.Fatalf("count what was written: %v", err)
	}
	if batches != 0 || intents != 0 {
		t.Errorf("a refused admission left %d claim(s) and %d commitment(s)", batches, intents)
	}
	if now := alertDomainState(t, s); now != before {
		t.Errorf("a refused admission wrote in the alert domain:\n  was %+v\n  now %+v",
			before, now)
	}
}

// TestAClaimHoldingAnEmptyAlertGroupIsNotAnAnswerAboutNone.
//
// `alert_groups.id` is a plain TEXT primary key, so a row holding
// `alert_group_id = ”` is representable. Compared by value alone it reads as
// "the claim about no alert group" - which is exactly the answer a handover
// repeat is waiting for, and it would be told its work exists on the strength
// of somebody else's row.
func TestAClaimHoldingAnEmptyAlertGroupIsNotAnAnswerAboutNone(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	batch := handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))
	result, err := s.SubmitBatch(context.Background(), batch)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	// An alert group whose id is the empty string, and a claim pointed at it.
	if _, err := s.db.Exec(`
		INSERT INTO alert_groups (id, alert_key, team_id, team_name_snapshot, status,
		                          title, severity, created_at, updated_at)
		VALUES ('', 'empty-id', 'team-1', 'Team One', 'new', 'nameless', 'critical',
		        now(), now())`); err != nil {
		t.Fatalf("create the nameless group: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE outbound_batches SET alert_group_id = '' WHERE id = $1`, result.BatchID); err != nil {
		t.Fatalf("point the claim at it: %v", err)
	}

	if _, err := s.SubmitBatch(context.Background(), batch); err == nil {
		t.Fatal("a claim about a group answered for an announcement about none")
	} else if !errors.Is(err, ErrOutboundContract) {
		t.Fatalf("the refusal is not a contract violation: %v", err)
	}
}

// TestOnlyAHandoverMayCarryABoundedDeadline.
//
// The bounded form is the handover's: the earlier of a domain instant and an
// age from admission, for work nobody can acknowledge. An escalation ends by
// acknowledgement, and the form arrives with the handover's rule attached -
// which allows an instant already past. Admitted, it would move the group to
// processing and leave a commitment the sweep ends before any provider is
// called: a page that never happens, from an alert that looks handled.
func TestOnlyAHandoverMayCarryABoundedDeadline(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC()

	t.Run("an escalation is refused", func(t *testing.T) {
		s := setupTestDB(t)
		s.SetRenderEnvironment("https://tokay.example", "UTC")
		agID := outboundGroup(t, s)

		commitment := channelCommitment("C-ops", 0)
		commitment.Expiry = &keys.TimingSpec{
			Kind: keys.TimingBounded, At: past, MaxAge: time.Hour,
		}
		batch := outboundAdmission(t, agID, "first", commitment)
		before := alertDomainState(t, s)
		if _, err := s.SubmitBatch(context.Background(), batch); err == nil {
			t.Fatal("an escalation with a bounded deadline was admitted")
		}
		assertNothingAdmitted(t, s, before)
	})

	t.Run("a handover keeps a deadline already past", func(t *testing.T) {
		s := setupTestDB(t)
		seedUsers(t, s, "u-alice")

		// A shift that began and ended while the system was stopped. The
		// announcement is admitted and expires at once: refusing it at the door
		// would lose the record that it was owed.
		admission, err := keys.HandoffBatch{
			Occurrence:         handoffOccurrence("sched-1"),
			TeamName:           "Backend",
			Timezone:           "UTC",
			GridSlotStart:      past.Add(-8 * time.Hour),
			AssignmentEnd:      past,
			MaxAge:             time.Hour,
			GrammarVersion:     keys.GrammarV1,
			FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
			Recipients:         []keys.HandoffRecipient{announceTo("slack", "u-alice")},
		}.Admit()
		if err != nil {
			t.Fatalf("build the admission: %v", err)
		}
		result, err := s.SubmitBatch(context.Background(), outbound.Batch{
			Admission: admission, Context: outbound.AnnouncingShiftChange(), Actor: "notifier",
		})
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if result.Outcome != outbound.SubmitCreated {
			t.Fatalf("a late announcement answered %q", result.Outcome)
		}

		var expired bool
		if err := s.db.QueryRow(
			`SELECT expires_at <= now() FROM outbound_intents WHERE batch_id = $1`,
			result.BatchID).Scan(&expired); err != nil {
			t.Fatalf("read the deadline: %v", err)
		}
		if !expired {
			t.Fatal("an announcement about a shift that is over has a deadline in the future")
		}
	})
}

// TestAHandoverCarriesNoAlertGroupState.
//
// One forbidden atom per case. Changing several at once would leave the test
// red after any single condition was removed, so it would not say which of them
// is doing the work.
//
// The failure is silent without this: a snapshot handed in here is dropped on
// the floor by a claim written without it, and the producer's belief and the
// row differ with nobody the wiser.
func TestAHandoverCarriesNoAlertGroupState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, s *Store, batch *outbound.Batch)
	}{
		{
			name: "an alert group",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.AlertGroupID = outboundGroup(t, s)
			},
		},
		{
			name: "a frozen state",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.Snapshot = outboundSnapshot(t, outboundGroup(t, s), "borrowed")
			},
		},
		{
			name: "a snapshot schema version",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.SnapshotSchemaVersion = keys.RenderSnapshotSchemaV1
			},
		},
		{
			name: "a revision",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.Revision = 3
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")
			seedUsers(t, s, "u-alice")

			batch := handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))
			tc.spoil(t, s, &batch)

			before := alertDomainState(t, s)
			if _, err := s.SubmitBatch(context.Background(), batch); err == nil {
				t.Fatal("a handover carrying an alert group's state was admitted")
			} else if !errors.Is(err, ErrOutboundContract) {
				t.Fatalf("the refusal is not a contract violation: %v", err)
			}
			assertNothingAdmitted(t, s, before)
		})
	}
}

// TestAnEscalationsFourFieldsDescribeOneState.
//
// Present is not the same as consistent, and the difference is a lost page. All
// four describe ONE state, so a claim naming group A beside a snapshot of group
// B - or revision 7 beside a snapshot of 6, or a schema this build cannot
// read - passes a presence check, writes the claim and moves group A to
// processing. The disagreement surfaces at the first attempt, where an unknown
// schema leaves the work waiting and a foreign group or revision ends it as
// unreadable. By then the alert looks handled and nobody has been paged.
//
// One factor per case, again, so each condition is proven on its own.
func TestAnEscalationsFourFieldsDescribeOneState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(t *testing.T, s *Store, batch *outbound.Batch)
		says  string
	}{
		{
			name: "nothing to render from",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.Snapshot = keys.RenderSnapshot{}
				batch.Admission.SnapshotSchemaVersion = 0
			},
			says: "snapshot",
		},
		{
			name: "the state of another alert group",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.Snapshot = outboundSnapshot(t, outboundGroup(t, s), "another")
			},
			says: "carrying the state of",
		},
		{
			name: "the state of another revision",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.Revision = 7
			},
			says: "revision",
		},
		{
			name: "a schema this build cannot render",
			spoil: func(t *testing.T, s *Store, batch *outbound.Batch) {
				batch.Admission.SnapshotSchemaVersion = keys.RenderSnapshotSchemaV1 + 1
			},
			says: "schema",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestDB(t)
			s.SetRenderEnvironment("https://tokay.example", "UTC")
			agID := outboundGroup(t, s)

			batch := outboundAdmission(t, agID, "first")
			tc.spoil(t, s, &batch)

			before := alertDomainState(t, s)
			_, err := s.SubmitBatch(context.Background(), batch)
			if err == nil {
				t.Fatal("an escalation whose four fields disagree was admitted")
			}
			// Refused by the contract layer, which says what is wrong - not by
			// a constraint downstream, which says a constraint name, nor at the
			// first attempt, by which time the alert looks handled.
			if !errors.Is(err, ErrOutboundContract) {
				t.Fatalf("the refusal is not a contract violation: %v", err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the refusal does not say what is wrong: %v", err)
			}
			assertNothingAdmitted(t, s, before)
		})
	}
}

// TestAHandoverDeadlineIsBounded. Not merely present: the grammar fingerprints
// a bounded deadline's two atoms, so a spec swapped to absolute or relative
// after the admission was built still matches that fingerprint while the stored
// deadline comes out of entirely different arithmetic.
func TestAHandoverDeadlineIsBounded(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	batch := handoffBatch(t, "sched-1", announceTo("slack", "u-alice"))
	swapped := *batch.Admission.Commitments[0].Expiry
	swapped.Kind = keys.TimingAbsolute
	swapped.MaxAge = 0
	batch.Admission.Commitments[0].Expiry = &swapped

	before := alertDomainState(t, s)
	if _, err := s.SubmitBatch(context.Background(), batch); err == nil {
		t.Fatal("a handover carrying another kind of deadline was admitted")
	}
	assertNothingAdmitted(t, s, before)
}

// TestACommitmentTimedByADomainInstantIsDueThen.
//
// Both of the grammar's timing forms are things a producer can mean, and both
// have to survive the trip through admission. A relative one is a delay from
// the admission - a policy step that pages five minutes after the alert. An
// absolute one is a moment the domain supplies and cannot move: the start of an
// on-call assignment is one, and that is how a handover announcement is timed.
//
// Admission used to take only the first, so a batch the grammar accepts came
// back refused with a message about escalation steps.
func TestACommitmentTimedByADomainInstantIsDueThen(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	at := handoffOccurrence("sched-1").AssignmentStart
	recipient := announceTo("slack", "u-alice")
	recipient.Timing = keys.TimingSpec{Kind: keys.TimingAbsolute, At: at}

	result, err := s.SubmitBatch(context.Background(), handoffBatch(t, "sched-1", recipient))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}

	var notBefore time.Time
	if err := s.db.QueryRow(
		`SELECT not_before FROM outbound_intents WHERE batch_id = $1`, result.BatchID).
		Scan(&notBefore); err != nil {
		t.Fatalf("read when it is due: %v", err)
	}
	if !notBefore.Equal(at) {
		t.Fatalf("the announcement is due at %s, and the domain said %s", notBefore, at)
	}
}

// TestADeadlineIsNotAStartTime. The bounded form says when work stops being
// worth doing; read as a start time it would take the wrong half of it and make
// the commitment due at the moment it should end.
func TestADeadlineIsNotAStartTime(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	recipient := announceTo("slack", "u-alice")
	recipient.Timing = keys.TimingSpec{
		Kind:   keys.TimingBounded,
		At:     time.Date(2026, 5, 4, 19, 0, 0, 0, time.UTC),
		MaxAge: time.Hour,
	}

	before := alertDomainState(t, s)
	if _, err := s.SubmitBatch(context.Background(), handoffBatch(t, "sched-1", recipient)); err == nil {
		t.Fatal("a commitment timed by a deadline was admitted")
	}
	assertNothingAdmitted(t, s, before)
}

// TestAnAnnouncementIsGoodUntilTheEarlierOfTwoThings.
//
// The bounded form is two atoms and one answer: the shift's own end, and how
// long an announcement is worth making at all. Which of them wins is not a
// property of the announcement - it is whichever comes first - and both sides
// are reachable in ordinary use. A shift that ends in ten minutes is bounded by
// itself; an eight-hour one is bounded by the hour nobody wants a stale shift
// notice after.
//
// Asserted against the admitted_at the DATABASE wrote, because half of the
// arithmetic is measured from it: a test using its own clock would be checking
// two different admissions against each other and would drift by however long
// the round trip took.
func TestAnAnnouncementIsGoodUntilTheEarlierOfTwoThings(t *testing.T) {
	s := setupTestDB(t)
	seedUsers(t, s, "u-alice")

	for _, c := range []struct {
		name      string
		endsIn    time.Duration
		maxAge    time.Duration
		shiftWins bool
	}{
		{"a shift that ends before the notice goes stale", 10 * time.Minute, time.Hour, true},
		{"a shift that outlasts it", 8 * time.Hour, time.Hour, false},
		// Either side of the crossing point by a minute. The shift starts a
		// minute from now, so the two are equal at endsIn = maxAge - 1m; at
		// the point itself both branches give the same instant, so which one
		// ran cannot be observed and is not asserted.
		{"a minute before the crossing", 58 * time.Minute, time.Hour, true},
		{"a minute after it", 60 * time.Minute, time.Hour, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			start := time.Now().Add(time.Minute).UTC().Truncate(time.Microsecond)
			end := start.Add(c.endsIn)

			occurrence := handoffOccurrence("sched-" + c.name)
			occurrence.AssignmentStart = start
			admission, err := keys.HandoffBatch{
				Occurrence: occurrence, TeamName: "Backend", Timezone: "UTC",
				GridSlotStart:      start,
				AssignmentEnd:      end,
				MaxAge:             c.maxAge,
				GrammarVersion:     keys.GrammarV1,
				FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
				Recipients:         []keys.HandoffRecipient{announceTo("slack", "u-alice")},
			}.Admit()
			if err != nil {
				t.Fatalf("build the admission: %v", err)
			}
			result, err := s.SubmitBatch(context.Background(), outbound.Batch{
				Admission: admission, Context: outbound.AnnouncingShiftChange(),
				Actor: "notifier",
			})
			if err != nil {
				t.Fatalf("admit: %v", err)
			}

			var admittedAt, expiresAt time.Time
			if err := s.db.QueryRow(`
				SELECT b.admitted_at, i.expires_at
				FROM outbound_batches b
				JOIN outbound_intents i ON i.batch_id = b.id
				WHERE b.id = $1`, result.BatchID).Scan(&admittedAt, &expiresAt); err != nil {
				t.Fatalf("read the deadline: %v", err)
			}

			want := admittedAt.Add(c.maxAge)
			which := "the announcement went stale"
			if c.shiftWins {
				want, which = end, "the shift ended"
			}
			if !expiresAt.UTC().Equal(want.UTC()) {
				t.Fatalf("the announcement is good until %s; %s at %s",
					expiresAt.UTC(), which, want.UTC())
			}
			// And the other half really was the later one, so the case is the
			// branch it says it is rather than both answers coinciding.
			other := end
			if c.shiftWins {
				other = admittedAt.Add(c.maxAge)
			}
			if !other.After(want) {
				t.Fatalf("this case does not choose: %s is not after %s", other.UTC(), want.UTC())
			}
		})
	}
}
