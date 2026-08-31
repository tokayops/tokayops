package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The outbound delivery operations, as the transactions they have to be.
//
// Everything here follows two rules that are not local decisions:
//
//   - time comes from the database, never from the process. Two instances have
//     different clocks, and a lease or a deadline compared against the wrong one
//     is a lease two workers both believe they hold.
//   - when a unit of work needs both an alert group and a commitment, the group
//     is locked first. Every transaction that can end up writing to a group
//     takes them in that order, which is what keeps acknowledgement and delivery
//     from deadlocking against each other.

// The two ways this package refuses. They are disjoint on purpose - neither
// wraps the other - because they have opposite consequences and a caller has to
// be able to tell them apart in whichever order it asks.
//
// ErrOutboundContract is a valid row this build cannot handle - written under a
// protocol version it does not know, or needing a call it does not make - and
// an invariant that should have been impossible. It stops the caller and
// changes nothing: the same row read by the build it was written for is fine,
// and an old instance must not be able to end work a new format created.
var ErrOutboundContract = errors.New("store: outbound contract violation")

// Every one of these is counted, in this one place, because none of them is a
// delivery problem: a contract violation is a bug in this system or in an
// assumption about a protocol version, and a nonzero rate is a thing to go and
// read the log about.
//
// One kind rather than one per call site. What distinguishes them is a sentence
// an operator reads in the log; as a label it would be a new time series for
// every phrasing, and the question a metric answers here is only "is this
// happening at all".
//
// Counted where it is raised rather than after a commit, unlike the terminal
// states: this says a row cannot be handled, which is true whether or not the
// transaction that found it goes on to commit.
func outboundContractf(format string, args ...any) error {
	metrics.OutboundContractViolationsTotal.WithLabelValues("store", "invariant").Inc()
	return fmt.Errorf("%w: %s", ErrOutboundContract, fmt.Sprintf(format, args...))
}

// countTerminal records a commitment that ended.
//
// One owner for the whole counter, and it is this package, because a commitment
// ends when a transaction here commits and at no other moment. Split between
// the store and its callers - as it was first written - the doors nobody
// remembered to count were exactly the ones nobody remembers: a preparation
// refused for good, and an operator deciding. The alert on this counter is
// "somebody was not paged", so a missing door is a page that never happened and
// never got reported.
//
// Called AFTER the commit, always. Counted inside, a transaction that then
// rolled back would report an ending that did not happen - and this counter
// alerts on any increment at all.
//
// Non-terminal transitions are ignored here rather than at the call sites, so
// that adding a door is one line and not a decision.
//
// The family is required and NOT defaulted. It used to fall back to the paging
// family when empty, which was safe while paging was the only one and would
// stop being safe the moment a webhook delivery ended: its ending would be
// counted against the queue an operator watches for missed pages. Every caller
// has a real family - the commitment carries it, and the withdrawal path names
// it - so an empty one is a new caller that forgot, and a series labelled with
// nothing is how it says so.
func countTerminal(family string, to outbound.Status) {
	if !to.Terminal() {
		return
	}
	metrics.OutboundIntentsTerminalTotal.WithLabelValues(family, string(to)).Inc()
}

// ErrUndeliverable is the other one: the row itself is broken, for every build
// and forever. The state a commitment renders from is gone, or no longer
// produces the digest its keys were computed over, or describes another alert.
//
// The consequence differs because the cause does. Stopping the caller over a
// broken row would hand the same row out of the queue for as long as its
// deadline allows, so this one ends the commitment where a person can see it -
// see refuseAttempt. Nothing that could become readable again after a
// deployment belongs here.
//
// It is NOT a contract violation as well. A handler that asked about the
// contract first would read a broken row as an incompatible build and leave the
// commitment circling, which is the failure both of these exist to name.
var ErrUndeliverable = errors.New("store: this commitment cannot produce a call")

func undeliverablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUndeliverable, fmt.Sprintf(format, args...))
}

// SubmitBatch admits one batch of commitments, whatever the batch is about.
//
// One command with two closed forms of context rather than two commands: the
// half that is the same for both - what is already claimed, locking the
// recipients, writing the claim and the commitments - is the half where a
// second implementation would eventually disagree with the first, and the
// disagreement would be about idempotency.
//
// The ORDER of operations differs between the two, and deliberately:
//
//	escalation                      handoff
//	----------                      -------
//	1. lock the alert group         1. existing / conflict
//	2. existing / conflict          2. lock the recipients
//	3. lock the recipients          3. checks of its own kind
//	4. checks: status, source       4. claim + commitments
//	5. claim + commitments
//	   + writes to the group
//
// An escalation locks its group first because everything it decides is about
// that group and the lock is what makes those decisions consistent; moving the
// existing-claim read above it would buy an extra read and a recheck for a
// branch that is already cheap. A handover has no group, so the claim is read
// first, and two producers that both find nothing are separated by the unique
// index on the claim - which is what it is for.
func (s *Store) SubmitBatch(ctx context.Context, batch outbound.Batch) (outbound.SubmitResult, error) {
	admission := batch.Admission
	if len(admission.Commitments) != 0 && admission.Outcome != keys.OutcomeAdmitted {
		return outbound.SubmitResult{}, outboundContractf(
			"an admission of %d commitments carrying outcome %q",
			len(admission.Commitments), admission.Outcome)
	}

	// The family is DERIVED from the kind of claim, never taken from the
	// caller. Given both, a caller could write a paging key into the handover
	// partition, and the row would then be executed by the wrong worker, be
	// counted in the wrong series and be alerted on by the wrong rule.
	family, err := keys.FamilyOf(admission.Kind)
	if err != nil {
		return outbound.SubmitResult{}, outboundContractf("%v", err)
	}

	// The kind and the context have to be the same claim, and the pair is
	// checked before anything is written.
	//
	// They are two exported halves of one fact, and nothing in the type system
	// holds them together: an escalation admission with its kind swapped to
	// handoff would be written into the handover family and yet update the
	// alert group, its snapshot and its timeline - and the worker would then
	// read an escalation payload as a handover one. The work would stick, or
	// end, and nobody would be paged.
	if err := contextMatchesKind(admission.Kind, batch.Context.Form()); err != nil {
		return outbound.SubmitResult{}, err
	}
	if err := admissionCarriesWhatItsKindHas(admission); err != nil {
		return outbound.SubmitResult{}, err
	}

	var result outbound.SubmitResult
	switch batch.Context.Form() {
	case outbound.ContextEscalation:
		about, _ := batch.Context.Escalation()
		result, err = s.submitEscalation(ctx, batch, about, string(family))
	case outbound.ContextHandoff:
		result, err = s.submitHandoff(ctx, batch, string(family))
	default:
		// Unreachable while contextMatchesKind is exhaustive, and written out
		// anyway: a third context added to the pairing above and forgotten here
		// would otherwise be admitted silently as a handover.
		return outbound.SubmitResult{}, outboundContractf(
			"an admission whose context is a %q", batch.Context.Form())
	}
	if err != nil {
		return result, err
	}

	// Counted here rather than by each producer, because this is where the
	// family is a fact. A producer counting its own admissions has to name the
	// partition it thinks it is in, and the one thing the derivation above
	// exists to prevent is a caller naming that at all. One call site also
	// means a producer that ignores what it was told is still counted, which is
	// what the series is for.
	metrics.OutboundAdmissionsTotal.WithLabelValues(string(family),
		outbound.AdmissionLabel(result.Outcome, len(admission.Commitments))).Inc()
	return result, nil
}

// contextMatchesKind is the closed pairing of a claim's kind and its context.
//
// Exhaustive in both directions rather than a default: a kind this build does
// not know reaches neither arm and is refused by name, which is the answer a
// build meeting a row from a newer one should give.
func contextMatchesKind(kind keys.Kind, form outbound.ContextForm) error {
	var want outbound.ContextForm
	switch kind {
	case keys.KindEscalation, keys.KindEscalationReplay:
		want = outbound.ContextEscalation
	case keys.KindHandoff:
		want = outbound.ContextHandoff
	default:
		return outboundContractf(
			"an admission of kind %q, which this build does not admit", kind)
	}
	if form != want {
		return outboundContractf(
			"an admission of kind %q offered as a %q; %q admissions are %q",
			kind, form, kind, want)
	}
	return nil
}

// admissionCarriesWhatItsKindHas checks the alert-specific four together, in
// both directions.
//
// An escalation is ABOUT a state: the group, the frozen snapshot, its schema
// version and its revision. They travel as one - a claim with three of them is
// a claim nothing can render from - and a handover has none of them at all.
//
// Both directions, because each failure is silent in its own way. A handover
// carrying a snapshot would have it dropped on the floor here and the claim
// written without it, so the producer's belief and the row would differ with
// nobody the wiser. An escalation missing one is caught today by a CHECK in the
// schema, which reports a constraint name rather than the thing that is wrong -
// and only after a transaction has been opened and rows attempted.
func admissionCarriesWhatItsKindHas(admission keys.Admission) error {
	hasGroup := admission.AlertGroupID != ""
	hasState := len(admission.Snapshot.Digest()) > 0
	hasSchema := admission.SnapshotSchemaVersion != 0

	switch admission.Kind {
	case keys.KindEscalation, keys.KindEscalationReplay:
		if !hasGroup || !hasState || !hasSchema {
			return outboundContractf(
				"an escalation admission is about a state: group=%v snapshot=%v schema=%v",
				hasGroup, hasState, hasSchema)
		}
		if admission.Revision < 0 {
			return outboundContractf(
				"an escalation admission at revision %d", admission.Revision)
		}
		// Present is not the same as consistent, and the difference is a lost
		// page. All four describe ONE state, so a claim naming group A beside a
		// snapshot of group B - or revision 7 beside a snapshot of 6, or a
		// schema this build cannot read - passes a presence check, writes the
		// claim and moves group A to processing. The disagreement then surfaces
		// at the first attempt, where an unknown schema leaves the work waiting
		// and a foreign group or revision ends it as unreadable. By then the
		// alert looks handled and nobody has been paged.
		// The digest's LENGTH is not checked here, and that is deliberate. A
		// snapshot outside the keys package is either the zero value - already
		// refused above as no state at all - or one its constructor made, and
		// that one always digests to thirty-two bytes. There is no third case
		// to defend against, and a guard for it would be a guard nothing can
		// reach.
		if admission.SnapshotSchemaVersion != keys.RenderSnapshotSchemaV1 {
			return outboundContractf(
				"an escalation admission at snapshot schema %d, which this build cannot render",
				admission.SnapshotSchemaVersion)
		}
		content := admission.Snapshot.Content()
		if content.AlertGroupID != admission.AlertGroupID {
			return outboundContractf(
				"an escalation admission for alert group %s carrying the state of %s",
				admission.AlertGroupID, content.AlertGroupID)
		}
		if content.Revision != admission.Revision {
			return outboundContractf(
				"an escalation admission at revision %d carrying the state of revision %d",
				admission.Revision, content.Revision)
		}
	case keys.KindHandoff:
		if hasGroup || hasState || hasSchema || admission.Revision != 0 {
			return outboundContractf(
				"a handover admission carrying an alert group's state: "+
					"group=%v snapshot=%v schema=%v revision=%d",
				hasGroup, hasState, hasSchema, admission.Revision)
		}
	default:
		return outboundContractf(
			"an admission of kind %q, which this build does not admit", admission.Kind)
	}
	return nil
}

// submitEscalation admits an alert group's escalation: the claim, its
// commitments, the state they are about, and the policy the group was escalated
// by, in one commit.
//
// The order inside is the whole design. The group is locked first and written
// LAST, and only by the producer whose claim was accepted: an admission that
// wrote the policy snapshot before knowing whether it won would let the loser
// overwrite the winner's, leaving a group describing one escalation and
// executing another.
func (s *Store) submitEscalation(ctx context.Context, batch outbound.Batch,
	about outbound.EscalationContext, family string) (outbound.SubmitResult, error) {

	admission := batch.Admission
	if admission.AlertGroupID == "" {
		return outbound.SubmitResult{}, outboundContractf(
			"an escalation admission that is about no alert group")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	defer tx.Rollback()

	// The group first, and its clock with it: everything this transaction
	// computes about time is computed from the database's now(). Its version
	// comes from the same locked row, because that is the only read of it that
	// nothing can change between the check and the insert.
	var status string
	var version int64
	var now time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT status, render_source_version, now() FROM alert_groups WHERE id = $1 FOR UPDATE`,
		admission.AlertGroupID).Scan(&status, &version, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.SubmitResult{}, fmt.Errorf("alert group %s not found", admission.AlertGroupID)
	}
	if err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("lock alert group %s: %w", admission.AlertGroupID, err)
	}

	// What is already claimed answers first, and answers alone.
	//
	// Everything below this is a question about admitting NEW work: is the
	// group still waiting, is the plan still current, is the deadline still
	// ahead. None of them is a question about work that was accepted minutes
	// ago - and a producer retrying after a lost reply is asking exactly that.
	// Asked in the wrong order, the same admission stops being idempotent the
	// moment the world moves: an expired deadline, an alert that joined, a user
	// who acknowledged, and the repeat comes back "not admissible" over
	// commitments that exist and are being delivered.
	//
	// The group is locked above, and every admission for it takes that lock
	// first, so this read is the whole truth about the claim (D2/D3).
	if held, found, err := existingAdmission(ctx, tx, admission); err != nil {
		return outbound.SubmitResult{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return held, nil
	}

	if settled, done, err := lockRecipients(ctx, tx, admission); err != nil {
		return outbound.SubmitResult{}, err
	} else if done {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return settled, nil
	}

	if err := outbound.ValidateEscalationAdmission(admission, now); err != nil {
		return outbound.SubmitResult{}, err
	}

	// The user is ahead of us: they acknowledged or resolved before this
	// escalation was admitted, and nothing about the group is touched.
	if model.AlertGroupStatus(status) != model.AlertGroupStatusNew &&
		model.AlertGroupStatus(status) != model.AlertGroupStatusProcessing {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return outbound.SubmitResult{Outcome: outbound.SubmitGroupNotAdmitted}, nil
	}

	// The alert moved after the producer read it, so the snapshot in this
	// admission describes a state that is already behind - and a snapshot is
	// what every message of an escalation is rendered from, forever.
	//
	// Asked AFTER the status check, because the two say different things. A
	// group the user already acknowledged is finished with; a group that merely
	// changed is not, and the next tick plans it again from what is now there.
	//
	// Nothing is written and nothing is claimed. A batch inserted here would
	// hold the group's one escalation under a plan built from stale state, and
	// no later revision would ever correct it.
	if about.SourceVersion != version {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return outbound.SubmitResult{Outcome: outbound.SubmitSourceChanged}, nil
	}

	// The state this set was admitted from, frozen with the claim. It is what a
	// one-shot message renders forever after: the group's own snapshot moves
	// on, and a direct message that followed it would send different bytes
	// under a key that says they are the same request.
	frozen, err := json.Marshal(admission.Snapshot)
	if err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("freeze the admitted state: %w", err)
	}

	batchID, admittedAt, won, err := claimBatchTx(ctx, tx, admission, family,
		admission.AlertGroupID, frozen, admission.Snapshot.Digest(),
		admission.SnapshotSchemaVersion, admission.Revision)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	if !won {
		// Somebody else holds this claim. Nothing about the group is written on
		// this path - the winner already said what the group is escalating by.
		result, lostErr := lostAdmission(ctx, tx, admission)
		if lostErr != nil {
			return outbound.SubmitResult{}, lostErr
		}
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return result, nil
	}

	intentIDs, err := insertCommitmentsTx(ctx, tx, batchID, admission, family, admittedAt, batch.Actor)
	if err != nil {
		return outbound.SubmitResult{}, err
	}

	// The state the commitments are about. Written even when there is nobody to
	// notify: a group that was admitted has a snapshot, or a later re-admission
	// would start from nothing.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_group_snapshots
			(alert_group_id, revision, snapshot_schema_version, snapshot, snapshot_digest)
		VALUES ($1, $2, $3, $4, $5)`,
		admission.AlertGroupID, admission.Revision, admission.SnapshotSchemaVersion,
		frozen, admission.Snapshot.Digest()); err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("store the snapshot: %w", err)
	}

	// Only now the group itself: what it escalates by, and who was on duty when
	// it arrived. Both are the winner's answers and nobody else's - a loser that
	// wrote either would leave a group describing one escalation and executing
	// another.
	//
	// COALESCE, because "the producer could not read the people" and "nobody was
	// on call" are different claims. The first arrives here as nothing at all
	// and must not blank a snapshot; the second arrives as an empty one and is
	// recorded.
	if _, err := tx.ExecContext(ctx, `
		UPDATE alert_groups
		SET status = CASE WHEN status = $1 THEN $2 ELSE status END,
		    policy_id = $3, policy_snapshot = $4,
		    oncall_snapshot = COALESCE($5, oncall_snapshot),
		    updated_at = now()
		WHERE id = $6`,
		model.AlertGroupStatusNew, model.AlertGroupStatusProcessing,
		about.PolicyID, nullableJSON(about.PolicySnapshot),
		nullableJSON(about.OnCallSnapshot), admission.AlertGroupID,
	); err != nil {
		return outbound.SubmitResult{}, fmt.Errorf("record the escalation on the group: %w", err)
	}

	if err := admissionTimelineTx(ctx, tx, batch, about, admission); err != nil {
		return outbound.SubmitResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return outbound.SubmitResult{}, err
	}
	return outbound.SubmitResult{
		Outcome:   outbound.SubmitCreated,
		BatchID:   batchID,
		IntentIDs: intentIDs,
	}, nil
}

// submitHandoff admits a shift change announcement.
//
// It writes nothing in the alert domain: no snapshot row, no timeline line, no
// column on any alert group. That is not an omission to be checked for later -
// it is the whole difference between the two contexts, and it is asserted by a
// test rather than left to the shape of the code.
func (s *Store) submitHandoff(ctx context.Context, batch outbound.Batch,
	family string) (outbound.SubmitResult, error) {

	admission := batch.Admission
	if admission.AlertGroupID != "" {
		return outbound.SubmitResult{}, outboundContractf(
			"a handover admission claiming to be about alert group %s", admission.AlertGroupID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	defer tx.Rollback()

	// There is no group to take the clock from, so it is asked for directly -
	// the same clock, in the same transaction, for the same reason: nothing
	// this transaction decides about time may come from a process.
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return outbound.SubmitResult{}, err
	}

	// The claim first. Two producers that both find nothing here are separated
	// by the unique index below, and the loser goes down the same path an
	// escalation's loser does.
	if held, found, err := existingAdmission(ctx, tx, admission); err != nil {
		return outbound.SubmitResult{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return held, nil
	}

	if settled, done, err := lockRecipients(ctx, tx, admission); err != nil {
		return outbound.SubmitResult{}, err
	} else if done {
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return settled, nil
	}

	if err := outbound.ValidateHandoffAdmission(admission, now); err != nil {
		return outbound.SubmitResult{}, err
	}

	batchID, admittedAt, won, err := claimBatchTx(ctx, tx, admission, family,
		"", nil, nil, 0, 0)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	if !won {
		result, lostErr := lostAdmission(ctx, tx, admission)
		if lostErr != nil {
			return outbound.SubmitResult{}, lostErr
		}
		if err := tx.Commit(); err != nil {
			return outbound.SubmitResult{}, err
		}
		return result, nil
	}

	intentIDs, err := insertCommitmentsTx(ctx, tx, batchID, admission, family, admittedAt, batch.Actor)
	if err != nil {
		return outbound.SubmitResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return outbound.SubmitResult{}, err
	}
	return outbound.SubmitResult{
		Outcome:   outbound.SubmitCreated,
		BatchID:   batchID,
		IntentIDs: intentIDs,
	}, nil
}

// claimBatchTx writes the claim, and reports whether this producer won it.
//
// The frozen state is optional because only some kinds have one: an escalation
// freezes the alert group's snapshot, a handover has nothing to freeze. The
// schema refuses the wrong combination for each kind, so the four values travel
// together or not at all.
func claimBatchTx(ctx context.Context, tx *sql.Tx, admission keys.Admission,
	family, alertGroupID string, frozen, digest []byte,
	schemaVersion int, revision int64) (string, time.Time, bool, error) {

	batchID := uuid.New().String()
	var admittedAt time.Time
	err := tx.QueryRowContext(ctx, `
		INSERT INTO outbound_batches
			(id, batch_key, key_kind, delivery_family, grammar_version, alert_group_id,
			 fingerprint, fingerprint_version, admission_outcome, intent_count,
			 admission_snapshot, admission_digest, admission_schema_version,
			 admission_revision)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (batch_key) DO NOTHING
		RETURNING id, admitted_at`,
		batchID, admission.BatchKey, string(admission.Kind), family,
		admission.GrammarVersion, nilIfEmpty(alertGroupID),
		admission.Fingerprint, admission.FingerprintVersion,
		string(admission.Outcome), len(admission.Commitments),
		nullableJSON(frozen), nilIfEmptyBytes(digest),
		nilIfZero(schemaVersion), nilIfZeroRevision(schemaVersion, revision),
	).Scan(&batchID, &admittedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("claim the admission: %w", err)
	}
	return batchID, admittedAt, true, nil
}

// nilIfEmptyBytes keeps an absent digest absent rather than storing an empty
// one, which the length rule would refuse anyway.
func nilIfEmptyBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nilIfZero(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

// nilIfZeroRevision keeps the revision absent exactly when the rest of the
// frozen state is. Revision zero is a real revision, so it cannot decide for
// itself; the schema version is what says whether there is a state at all.
func nilIfZeroRevision(schemaVersion int, revision int64) any {
	if schemaVersion == 0 {
		return nil
	}
	return revision
}

// nilIfEmpty is what a nullable TEXT column takes: an empty string is a value,
// and a column that means "there is none of this" has to hold NULL instead.
//
// A claim with no alert group is the reason it is here. Written as "", it would
// be a claim about an alert group whose id is the empty string - and the
// comparison that decides whether a repeat is the same work would answer yes.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// subjectLabel names what a claim is about, so a refusal can say "no alert
// group" instead of the empty string.
func subjectLabel(present bool, id string) string {
	if !present {
		return "no alert group"
	}
	return "alert group " + id
}

// lockRecipients locks every person this admission promises to reach and
// reports whether the batch has to be refused because one of them is gone.
func lockRecipients(ctx context.Context, tx *sql.Tx,
	admission keys.Admission) (outbound.SubmitResult, bool, error) {

	erased, err := erasedRecipients(ctx, tx, admission)
	if err != nil {
		return outbound.SubmitResult{}, false, err
	}
	if len(erased) > 0 {
		return outbound.SubmitResult{Outcome: outbound.SubmitRecipientErased}, true, nil
	}
	return outbound.SubmitResult{}, false, nil
}

// erasedRecipients locks EVERY person this admission promises to reach and
// reports the ones who are no longer there.
//
// Every person, not only the erased ones, and that is the whole mechanism. A
// predicate of `deleted_at IS NOT NULL` leaves an active row out of the result
// set, so FOR SHARE never touches it - and an erasure is then free to commit
// between this check and the inserts below. The sequence that produces is
// entirely ordinary: a plan built while somebody was still on call, an operator
// taking them off it, an erasure, and an admission arriving a moment later with
// a commitment aimed at a person who no longer exists. It leaves an obligation
// nothing marked, a delivery that fails for a reason nobody can act on, and a
// second erasure that will not pick it up because erasing an erased user is a
// no-op.
//
// Selecting the rows locks them, and after that only two orders exist. This
// transaction got there first: the erasure waits, and when it runs it finds the
// new commitments and withdraws them like any other. The erasure got there
// first: this waits, and then reads deleted_at and refuses the batch.
//
// FOR SHARE rather than FOR UPDATE because this transaction does not change the
// users - it needs them to stay as they are until it commits. Ordered by id,
// the canonical order every command that locks several users takes, so two
// admissions naming the same two people cannot deadlock over nothing.
//
// A recipient with no row at all is NOT treated as erased. Erasure anonymizes
// and keeps the row, so absence means something else - an id that never
// resolved - and that surfaces where it belongs, as a delivery that cannot find
// an identity, rather than as a refusal of the whole escalation.
func erasedRecipients(ctx context.Context, tx *sql.Tx, admission keys.Admission) ([]string, error) {
	seen := map[string]bool{}
	var people []string
	for _, c := range admission.Commitments {
		if c.Target.Kind != keys.TargetUser || seen[c.Target.Ref] {
			continue
		}
		seen[c.Target.Ref] = true
		people = append(people, c.Target.Ref)
	}
	if len(people) == 0 {
		return nil, nil
	}
	sort.Strings(people)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, deleted_at FROM users
		WHERE id = ANY($1)
		ORDER BY id
		FOR SHARE`, pq.Array(people))
	if err != nil {
		return nil, fmt.Errorf("check the recipients of the admission: %w", err)
	}
	defer rows.Close()

	var erased []string
	for rows.Next() {
		var id string
		var deletedAt sql.NullTime
		if err := rows.Scan(&id, &deletedAt); err != nil {
			return nil, err
		}
		if deletedAt.Valid {
			erased = append(erased, id)
		}
	}
	return erased, rows.Err()
}

// lostAdmission answers for the producer that did not get the claim.
//
// The question is not "did somebody get there first" - that is already known -
// but "did they accept the same work". The same content is an idempotent
// repeat; different content is a conflict, and the first set stands. Merging
// them would page an audience nobody chose.
func lostAdmission(ctx context.Context, tx *sql.Tx,
	admission keys.Admission) (outbound.SubmitResult, error) {

	result, found, err := existingAdmission(ctx, tx, admission)
	if err != nil {
		return outbound.SubmitResult{}, err
	}
	if !found {
		// The insert said the claim was taken and the read says it is not. The
		// group is locked, so nothing may have deleted it in between.
		return outbound.SubmitResult{}, outboundContractf(
			"admission %s was refused as taken and is not there", admission.BatchKey)
	}
	return result, nil
}

// existingAdmission is what is already claimed under this batch key, if
// anything: the same work, or different work under the same claim.
func existingAdmission(ctx context.Context, tx *sql.Tx,
	admission keys.Admission) (outbound.SubmitResult, bool, error) {

	var existingID, existingKind string
	var existingGroup sql.NullString
	var existingGrammar int
	var existingFingerprint []byte
	var existingVersion int
	err := tx.QueryRowContext(ctx, `
		SELECT id, alert_group_id, key_kind, grammar_version, fingerprint, fingerprint_version
		FROM outbound_batches WHERE batch_key = $1`, admission.BatchKey).
		Scan(&existingID, &existingGroup, &existingKind, &existingGrammar,
			&existingFingerprint, &existingVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return outbound.SubmitResult{}, false, nil
	}
	if err != nil {
		return outbound.SubmitResult{}, false, fmt.Errorf("read the winning admission: %w", err)
	}

	// The key is supposed to carry all three of these, so a row under this key
	// that names a different alert, a different kind of claim or a different
	// grammar is not an answer about this admission at all. Answered as one, a
	// repeat would be told its work exists on the strength of somebody else's -
	// and, on the other branch, a conflict would be reported over an escalation
	// belonging to another alert.
	//
	// This is the grammar and the stored row disagreeing, which no producer can
	// fix and no retry improves.
	// The subject is nullable now, and both directions are covered by one
	// comparison: a claim of a kind that has a subject must name the same one,
	// and a claim of a kind that has none must name nothing.
	//
	// PRESENCE and value, not value alone. A scanned NULL and a scanned empty
	// string both give "", and the two are different claims: `alert_groups.id`
	// is a plain TEXT primary key with nothing forbidding an empty one, so a
	// row holding `alert_group_id = ''` is representable - and comparing values
	// only, it would answer as "the claim about no alert group", which is the
	// answer a handover repeat is waiting for.
	//
	// What could not be left to SQL is the comparison itself: NULL is neither
	// equal nor unequal to NULL.
	if existingGroup.Valid != (admission.AlertGroupID != "") ||
		existingGroup.String != admission.AlertGroupID {
		return outbound.SubmitResult{}, false, outboundContractf(
			"admission %s is held for %s, this one is for %s",
			admission.BatchKey, subjectLabel(existingGroup.Valid, existingGroup.String),
			subjectLabel(admission.AlertGroupID != "", admission.AlertGroupID))
	}
	if existingKind != string(admission.Kind) {
		return outbound.SubmitResult{}, false, outboundContractf(
			"admission %s is held as %q, this one is %q",
			admission.BatchKey, existingKind, admission.Kind)
	}
	if existingGrammar != admission.GrammarVersion {
		return outbound.SubmitResult{}, false, outboundContractf(
			"admission %s was keyed under grammar %d, this build keys under %d",
			admission.BatchKey, existingGrammar, admission.GrammarVersion)
	}

	if existingVersion != admission.FingerprintVersion {
		// The stored row was written by another protocol version. Comparing
		// digests across protocols would answer a question neither of them
		// asked, so the comparison is refused rather than guessed at.
		return outbound.SubmitResult{}, false, outboundContractf(
			"admission %s was fingerprinted under version %d, this build compares under %d",
			admission.BatchKey, existingVersion, admission.FingerprintVersion)
	}

	ids, err := intentIDsOfBatch(ctx, tx, existingID)
	if err != nil {
		return outbound.SubmitResult{}, false, err
	}

	if bytes.Equal(existingFingerprint, admission.Fingerprint) {
		return outbound.SubmitResult{
			Outcome: outbound.SubmitExisting, BatchID: existingID, IntentIDs: ids,
		}, true, nil
	}

	// The claim is held by DIFFERENT work, and this is the only place both
	// fingerprints exist at once: the one the producer offered and the one in
	// the winner's row. The result carries neither - a producer has no use for
	// the winner's digest and widening the result for a log line would put it
	// in everyone's hands - so the line is written here.
	//
	// It is worth a line rather than a counter alone because nothing else can
	// answer the question it raises. Two producers derived different work from
	// what should be one event; which of them is right cannot be decided here,
	// and the loser will not ask again. What an operator gets is the claim, the
	// two digests, and a place to start.
	log.Printf("outbound: claim %s is held by different work: winner %x, offered %x",
		admission.BatchKey, existingFingerprint, admission.Fingerprint)
	return outbound.SubmitResult{
		Outcome: outbound.SubmitConflict, BatchID: existingID, IntentIDs: ids,
	}, true, nil
}

// insertCommitmentsTx writes the commitments in key order.
//
// The order is not cosmetic: two producers racing on one claim insert the same
// rows in the same sequence, so a violation of the key grammar surfaces as one
// deterministic unique-violation instead of as a deadlock nobody can read.
func insertCommitmentsTx(ctx context.Context, tx *sql.Tx, batchID string,
	admission keys.Admission, family string, admittedAt time.Time,
	actor string) ([]string, error) {

	ids := make([]string, 0, len(admission.Commitments))
	for _, c := range admission.Commitments {
		payload, err := json.Marshal(c.Payload)
		if err != nil {
			return nil, fmt.Errorf("encode the payload of %s: %w", c.IdempotencyKey, err)
		}

		// What this payload IS, recorded beside it. Every attempt recomputes it
		// from the row and compares: the payload is not in the business key, so
		// without this there is nothing a swap could be caught against.
		//
		// Taken from the bytes that are about to be stored, not from the value
		// in hand: if the two ever disagreed, the row would be checked against
		// a digest of something else.
		digest, err := keys.PayloadDigest(admission.Kind, c.PayloadSchemaVersion, payload)
		if err != nil {
			return nil, fmt.Errorf("digest the payload of %s: %w", c.IdempotencyKey, err)
		}

		notBefore, err := notBeforeOf(c.Timing, admittedAt)
		if err != nil {
			return nil, err
		}
		expiresAt, err := expiryOf(c.Expiry, admittedAt)
		if err != nil {
			return nil, err
		}

		form := outbound.FormOneShot
		if c.Editable {
			form = outbound.FormEditable
		}

		id := uuid.New().String()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO outbound_intents (
				id, batch_id, idempotency_key, delivery_family, key_kind, grammar_version,
				provider, target_kind, target_ref, alert_group_id, form, completion_mode,
				ambiguity_policy, payload_schema_version, payload, payload_digest,
				provider_key_codec_version,
				status, desired_revision, not_before, next_attempt_at, expires_at)
			VALUES (
				$1, $2, $3, $19, $4, $5,
				$6, $7, $8, $9, $10, $11,
				$12, $13, $14, $20, $15,
				'pending', $16,
				$17, GREATEST(now(), $17::timestamptz),
				$18)`,
			id, batchID, c.IdempotencyKey,
			string(admission.Kind), admission.GrammarVersion,
			c.Provider, string(c.Target.Kind), c.Target.Ref, nilIfEmpty(admission.AlertGroupID),
			string(form), string(c.CompletionMode),
			string(c.AmbiguityPolicy), c.PayloadSchemaVersion, payload, keys.ProviderKeyCodecV1,
			admission.Revision,
			notBefore,
			expiresAt, family, digest,
		); err != nil {
			return nil, fmt.Errorf("write the commitment %s: %w", c.IdempotencyKey, err)
		}

		if err := appendIntentEventTx(ctx, tx, id, 1, "created", "", actor); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// notBeforeOf is the instant a commitment becomes due.
//
// Both of the grammar's timing forms are honoured, because both are things a
// producer can legitimately mean. A relative one is a delay measured from the
// admission - a policy step that pages five minutes after the alert - and it is
// resolved against the database's clock, never a process's. An absolute one is
// a moment the domain supplies and cannot move: the start of an on-call
// assignment is one, and a handover announcement is timed by it.
//
// The bounded form is refused. It is a DEADLINE - the earlier of an instant and
// an age - and reading it as a start time would take the wrong half of it and
// make the work due at the moment it should stop being worth doing.
func notBeforeOf(spec keys.TimingSpec, admittedAt time.Time) (time.Time, error) {
	if err := spec.Validate(); err != nil {
		return time.Time{}, outboundContractf("timing: %v", err)
	}
	switch spec.Kind {
	case keys.TimingRelativeToAdmission:
		return admittedAt.Add(spec.Offset), nil
	case keys.TimingAbsolute:
		return spec.At.UTC(), nil
	default:
		return time.Time{}, outboundContractf(
			"a commitment timed as %q, which says when work ENDS", spec.Kind)
	}
}

func expiryOf(spec *keys.TimingSpec, admittedAt time.Time) (*time.Time, error) {
	if spec == nil {
		return nil, nil
	}
	if err := spec.Validate(); err != nil {
		return nil, outboundContractf("expiry: %v", err)
	}
	switch spec.Kind {
	case keys.TimingAbsolute:
		at := spec.At.UTC()
		return &at, nil
	case keys.TimingRelativeToAdmission:
		at := admittedAt.Add(spec.Offset)
		return &at, nil
	case keys.TimingBounded:
		// The earlier of the two, computed here because one half of it is
		// measured from admission and only this transaction knows when that
		// was. The RESULT is what is stored; the two atoms are what were
		// fingerprinted, so a repeat computing a different result is still the
		// same admission.
		//
		// A result already in the past is kept, not clamped. It says the work
		// arrived too late to be worth doing, and the sweep ends it visibly -
		// which is a different fact from "expires in a moment", and the one an
		// operator needs.
		domain := spec.At.UTC()
		age := admittedAt.Add(spec.MaxAge)
		at := domain
		if age.Before(at) {
			at = age
		}
		return &at, nil
	default:
		return nil, outboundContractf("unknown expiry kind %q", spec.Kind)
	}
}

// admissionTimelineTx records what the admission decided in the group's own
// history - including, and especially, that it found nobody to notify.
func admissionTimelineTx(ctx context.Context, tx *sql.Tx, batch outbound.Batch,
	about outbound.EscalationContext, admission keys.Admission) error {

	if admission.Outcome == keys.OutcomeNoTargets {
		if err := addTimelineTx(ctx, tx, admission.AlertGroupID,
			model.TimelineEventNotificationFailed,
			"Escalation admitted with nobody to notify", batch.Actor); err != nil {
			return err
		}
	}
	for _, step := range about.Unpromised {
		if err := addTimelineWithTx(ctx, tx, admission.AlertGroupID,
			model.TimelineEventNotificationFailed, unpromisedMessage(step), batch.Actor,
			map[string]string{"step": step.Step, "reason": string(step.Reason)}); err != nil {
			return err
		}
	}
	return nil
}

// unpromisedMessage says what actually happened, because each reason sends a
// reader somewhere different: to the schedule, to the policy, or to whoever
// deploys this.
func unpromisedMessage(step outbound.UnpromisedStep) string {
	switch step.Reason {
	case outbound.ReasonNobodyOnCall:
		return fmt.Sprintf("Escalation step %s resolved to nobody on call", step.Step)
	case outbound.ReasonNoTarget:
		return fmt.Sprintf("Escalation step %s names no recipient", step.Step)
	case outbound.ReasonNoChannel:
		return fmt.Sprintf("Escalation step %s could not be delivered: %s", step.Step, step.Detail)
	case outbound.ReasonDuplicate:
		return fmt.Sprintf("Escalation step %s repeats a notification already promised: %s",
			step.Step, step.Detail)
	default:
		return fmt.Sprintf("Escalation step %s produced no delivery: %s", step.Step, step.Detail)
	}
}

func addTimelineTx(ctx context.Context, tx *sql.Tx, alertGroupID string,
	eventType model.TimelineEventType, message, actor string) error {

	return addTimelineWithTx(ctx, tx, alertGroupID, eventType, message, actor, nil)
}

// addTimelineWithTx writes a line of the alert's history, with what it was
// about.
//
// The metadata is not decoration. An alert with a firehose card and three
// direct messages produces four lines that would otherwise read identically:
// "Notification sent", four times, with nothing to say which of the four
// deliveries each one is. Whoever is reading that history is usually trying to
// work out exactly that.
func addTimelineWithTx(ctx context.Context, tx *sql.Tx, alertGroupID string,
	eventType model.TimelineEventType, message, actor string,
	about map[string]string) error {

	if actor == "" {
		actor = "system"
	}

	metadata := []byte("{}")
	if len(about) > 0 {
		encoded, err := json.Marshal(about)
		if err != nil {
			return fmt.Errorf("record %s in the timeline: %w", eventType, err)
		}
		metadata = encoded
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO timeline_events (id, alert_group_id, type, message, actor, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())`,
		uuid.New().String(), alertGroupID, eventType, message, actor, metadata)
	if err != nil {
		return fmt.Errorf("record %s in the timeline: %w", eventType, err)
	}
	return nil
}

// appendIntentEventTx writes one line of a commitment's own history: the things
// that happen to it without a network call - it was created, withdrawn, expired
// before anything was tried, decided on by a person.
func appendIntentEventTx(ctx context.Context, tx *sql.Tx,
	intentID string, seq int, kind, reason, actor string) error {

	return appendIntentEventDetailTx(ctx, tx, intentID, seq, kind, reason, actor, nil)
}

// appendIntentEventDetailTx is the same line with the facts the kind needs.
//
// A lifecycle event that says only what happened is enough for most kinds -
// withdrawn, expired, decided on. Raising the desired state is not one of them:
// which revision was raised is what says when a card started being out of date,
// and it is the only durable record of that moment.
func appendIntentEventDetailTx(ctx context.Context, tx *sql.Tx,
	intentID string, seq int, kind, reason, actor string, detail []byte) error {

	// seq 0 means "whatever comes next": the caller of a lifecycle event knows
	// what happened, not how many things happened before it.
	sequence := any(seq)
	if seq <= 0 {
		sequence = nil
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_intent_events (id, intent_id, seq, kind, reason, actor, detail)
		VALUES ($1, $2,
		        COALESCE($3::int,
		                 (SELECT COALESCE(max(seq), 0) + 1
		                  FROM outbound_intent_events WHERE intent_id = $2)),
		        $4, $5, $6, $7)`,
		uuid.New().String(), intentID, sequence, kind, nilIfEmpty(reason), nilIfEmpty(actor),
		nullableJSON(detail))
	if err != nil {
		return fmt.Errorf("record %s for commitment %s: %w", kind, intentID, err)
	}
	return nil
}

func intentIDsOfBatch(ctx context.Context, tx *sql.Tx, batchID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM outbound_intents WHERE batch_id = $1 ORDER BY idempotency_key`, batchID)
	if err != nil {
		return nil, fmt.Errorf("read the commitments of %s: %w", batchID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// outboundIntentColumns is read by every door into a commitment - the plain
// read, the locking one and the journal - so the domain cannot end up seeing a
// different commitment depending on which one it came through.
//
// The last column is the only computed one: whether the deadline has passed as
// of this statement's clock. It is asked of the database rather than compared
// in Go, because the process clock is not the one the leases are written
// against.
const outboundIntentColumns = `
	SELECT id, COALESCE(alert_group_id, ''), delivery_family, key_kind, provider, target_kind, target_ref,
	       form, completion_mode,
	       ambiguity_policy, status, generation_no, attempts_in_generation,
	       failure_streak, desired_revision, applied_revision,
	       final_revision_applied, receipt_recorded, recipient_erased_at IS NOT NULL,
	       cancellation_requested,
	       accepted_duplicate_risk, not_before, next_attempt_at, expires_at,
	       create_key IS NOT NULL, payload_schema_version,
	       provider_key_codec_version, payload, payload_digest, receipt, receipt_ref,
	       COALESCE(expires_at <= now(), FALSE)`

// scanIntent turns one row of outboundIntentColumns into a commitment, and is
// the only place that mapping exists: two readers that disagreed about it would
// hand the domain two different commitments from the same row.
func scanIntent(row interface{ Scan(...any) error }) (*outbound.Intent, bool, error) {
	var (
		intent         outbound.Intent
		groupID        sql.NullString
		applied        sql.NullInt64
		expiresAt      sql.NullTime
		payload        []byte
		coordinates    []byte
		name           sql.NullString
		deadlinePassed bool
	)
	if err := row.Scan(
		&intent.ID, &groupID, &intent.Family, &intent.KeyKind, &intent.Provider,
		&intent.TargetKind, &intent.TargetRef,
		&intent.Form, &intent.CompletionMode,
		&intent.AmbiguityPolicy, &intent.Status, &intent.GenerationNo,
		&intent.AttemptsInGeneration, &intent.FailureStreak, &intent.DesiredRevision,
		&applied, &intent.FinalRevisionApplied, &intent.HasReceipt, &intent.RecipientErased,
		&intent.CancellationRequested,
		&intent.AcceptedDuplicateRisk, &intent.NotBefore, &intent.NextAttemptAt, &expiresAt,
		&intent.GenerationBound, &intent.PayloadSchemaVersion,
		&intent.ProviderKeyCodecVersion, &payload, &intent.PayloadDigest,
		&coordinates, &name,
		&deadlinePassed); err != nil {
		return nil, false, err
	}

	intent.AlertGroupID = groupID.String
	if applied.Valid {
		value := applied.Int64
		intent.AppliedRevision = &value
	}
	if expiresAt.Valid {
		at := expiresAt.Time
		intent.ExpiresAt = &at
	}
	if len(payload) > 0 {
		intent.Payload = json.RawMessage(payload)
	}
	if len(coordinates) > 0 {
		intent.Receipt = json.RawMessage(coordinates)
	}
	intent.ReceiptRef = name.String
	return &intent, deadlinePassed, nil
}

// GetIntent reads one commitment as the domain sees it.
func (s *Store) GetIntent(ctx context.Context, id string) (*outbound.Intent, error) {
	intent, _, err := scanIntent(s.db.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return intent, nil
}

// ListIntentsByAlertGroup reads a group's commitments in key order - what the
// history of one alert's delivery looks like from the outside.
func (s *Store) ListIntentsByAlertGroup(ctx context.Context, alertGroupID string) ([]outbound.Intent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM outbound_intents WHERE alert_group_id = $1 ORDER BY idempotency_key`,
		alertGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]outbound.Intent, 0, len(ids))
	for _, id := range ids {
		intent, err := s.GetIntent(ctx, id)
		if err != nil {
			return nil, err
		}
		if intent != nil {
			out = append(out, *intent)
		}
	}
	return out, nil
}

// nextEventSeq asks appendIntentEventTx to take the next number in this
// commitment's own history.
const nextEventSeq = 0

// transitionWrite is one decided transition plus the values the effects need.
type transitionWrite struct {
	Intent     outbound.Intent
	Transition outbound.Transition
	Backoff    time.Duration
	// AppliedRevision is the revision the settled attempt was applying, which
	// is not the same as the one desired now: the desired state may have moved
	// while the attempt was in flight, and recording that as applied would
	// claim the card shows something it does not.
	//
	// Nil when the commitment has no revisions - one drawn from its own payload
	// rather than from a state that is revised. Written as NULL, not as zero: a
	// zero would say the commitment had caught up with a state nobody froze.
	AppliedRevision *int64
	// ReceiptRef is the name the channel gives the object the attempt produced.
	// Stored beside the coordinates so that a later change can say what it is
	// aimed at without anything in the domain reading a provider's JSON.
	ReceiptRef string

	// AttemptIsFinal says the settled attempt applied the last revision this
	// commitment will ever have, which is what makes an editable card done
	// rather than merely up to date.
	AttemptIsFinal bool
	Receipt        json.RawMessage
	NewExpires     *time.Time
	Actor          string
	Reason         string
}

// applyTransitionTx writes everything a transition means, in one call.
//
// One door on purpose. The effects of a transition are not only the
// commitment's own row: a success moves the alert group out of processing, and
// every ending writes a line in the alert's history. When those lived at the
// call sites, two of the three call sites forgot them - and the one that forgot
// the group left an alert saying nobody had been paged while the delivery said
// otherwise. A caller that can apply half a transition will eventually apply
// half a transition.
func applyTransitionTx(ctx context.Context, tx *sql.Tx, w transitionWrite) error {
	e := w.Transition.Effects

	if _, err := tx.ExecContext(ctx, `
		UPDATE outbound_intents SET
			status = $2,
			lease_token  = CASE WHEN $3 THEN NULL ELSE lease_token END,
			locked_until = CASE WHEN $3 THEN NULL ELSE locked_until END,
			worker_id    = CASE WHEN $3 THEN NULL ELSE worker_id END,
			current_attempt_id = CASE WHEN $4 THEN NULL ELSE current_attempt_id END,
			cancellation_requested = CASE WHEN $5 THEN FALSE ELSE cancellation_requested END,
			generation_no = generation_no + CASE WHEN $6 THEN 1 ELSE 0 END,
			attempts_in_generation = CASE WHEN $6 THEN 0 ELSE attempts_in_generation END,
			bound_endpoint = CASE WHEN $6 THEN NULL ELSE bound_endpoint END,
			create_key     = CASE WHEN $6 THEN NULL ELSE create_key END,
			-- The three receipt states, kept consistent in one statement.
			--
			-- The erasure marker is read from the ROW, not from what the caller
			-- believed when it started: an erasure that commits between this
			-- transaction's read and this write would otherwise have its
			-- prohibition undone by a result that was already in flight. The
			-- fact survives either way - something exists out there - and only
			-- the coordinates are refused.
			receipt = CASE
				WHEN $6 THEN NULL
				WHEN $7 AND $8::jsonb IS NOT NULL AND recipient_erased_at IS NULL THEN $8::jsonb
				WHEN $7 AND $8::jsonb IS NOT NULL THEN NULL
				ELSE receipt END,
			receipt_ref = CASE
				WHEN $6 THEN NULL
				WHEN $7 AND $8::jsonb IS NOT NULL AND recipient_erased_at IS NULL THEN $19
				WHEN $7 AND $8::jsonb IS NOT NULL THEN NULL
				ELSE receipt_ref END,
			receipt_recorded = CASE
				WHEN $6 THEN FALSE
				WHEN $7 AND $8::jsonb IS NOT NULL THEN TRUE
				ELSE receipt_recorded END,
			receipt_redacted_at = CASE
				WHEN $6 THEN NULL
				WHEN $7 AND $8::jsonb IS NOT NULL AND recipient_erased_at IS NOT NULL THEN now()
				WHEN $7 AND $8::jsonb IS NOT NULL THEN NULL
				ELSE receipt_redacted_at END,
			failure_streak = CASE
				WHEN $9  THEN 0
				WHEN $10 THEN failure_streak + 1
				ELSE failure_streak END,
			next_attempt_at = CASE
				WHEN $11 THEN now() + make_interval(secs => $12)
				WHEN $13 THEN now()
				ELSE next_attempt_at END,
			-- $15 is NULL when the attempt applied no revision at all, and the
			-- column takes that NULL: a commitment drawn from its own payload
			-- has no series to be at a position in, and a zero here would say
			-- it had caught up with a state nobody ever froze.
			applied_revision = CASE WHEN $14 THEN $15 ELSE applied_revision END,
			final_revision_applied = CASE
				WHEN $14 AND $16 THEN TRUE ELSE final_revision_applied END,
			accepted_duplicate_risk = CASE WHEN $17 THEN TRUE ELSE accepted_duplicate_risk END,
			expires_at = COALESCE($18, expires_at),
			updated_at = now()
		WHERE id = $1`,
		w.Intent.ID, string(w.Transition.To),
		e.ClearLease, e.ClearCurrentAttempt, e.ConsumeCancellation,
		e.NewGeneration,
		e.StoreReceipt, nullableJSON(w.Receipt),
		e.ResetFailureStreak, e.BumpFailureStreak,
		e.ScheduleRetry, w.Backoff.Seconds(), e.ScheduleNow,
		e.ApplyRevision, w.AppliedRevision, w.AttemptIsFinal, e.RecordDuplicateRisk,
		w.NewExpires, nilIfEmpty(w.ReceiptRef),
	); err != nil {
		return fmt.Errorf("apply %s to commitment %s: %w", w.Transition.Row, w.Intent.ID, err)
	}

	if err := appendTransitionEventTx(ctx, tx, w); err != nil {
		return err
	}
	if !w.Intent.GroupBound() {
		return nil
	}
	return groupEffectsTx(ctx, tx, w.Intent, w.Transition)
}

// appendTransitionEventTx records the lifecycle events a transition produces -
// the ones that are not attempts and therefore have no place in the attempt
// journal.
func appendTransitionEventTx(ctx context.Context, tx *sql.Tx, w transitionWrite) error {
	e := w.Transition.Effects

	if e.NewGeneration {
		// The decision to start over, which is not the same fact as the
		// binding of the effect that follows it: this one abandons whatever
		// the previous generation may have created, and is recorded even if no
		// attempt is ever made afterwards.
		if err := appendIntentEventTx(ctx, tx, w.Intent.ID, nextEventSeq,
			"generation_started", w.Reason, w.Actor); err != nil {
			return err
		}
	}
	if e.RecordDuplicateRisk {
		if err := appendIntentEventTx(ctx, tx, w.Intent.ID, nextEventSeq,
			"duplicate_risk_accepted",
			"a message may already have been delivered", w.Actor); err != nil {
			return err
		}
	}
	if w.Transition.To == outbound.StatusCanceled {
		return appendIntentEventTx(ctx, tx, w.Intent.ID, nextEventSeq,
			"canceled", w.Reason, w.Actor)
	}
	return nil
}

// groupEffectsTx writes what a transition means for the alert group: its
// history, and the one status move a delivery is allowed to make.
func groupEffectsTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent,
	transition outbound.Transition) error {

	alertGroupID := intent.AlertGroupID
	if message, eventType, ok := timelineFor(transition); ok {
		if err := addTimelineWithTx(ctx, tx, alertGroupID, eventType, message, "system",
			map[string]string{
				"intent_id":   intent.ID,
				"provider":    intent.Provider,
				"target_kind": string(intent.TargetKind),
				"target_ref":  intent.TargetRef,
			}); err != nil {
			return err
		}
	}

	if transition.Effects.TriggerGroup {
		// Conditional on purpose: an acknowledgement that arrived while this
		// send was in flight has already moved the group, and a delivery must
		// not move it back.
		if _, err := tx.ExecContext(ctx, `
			UPDATE alert_groups
			SET status = $2, updated_at = now(),
			    render_source_version = render_source_version + 1
			WHERE id = $1 AND status = $3`,
			alertGroupID, model.AlertGroupStatusTriggered, model.AlertGroupStatusProcessing,
		); err != nil {
			return fmt.Errorf("move the group to triggered: %w", err)
		}
	}
	return nil
}

// timelineFor turns a transition into the line the alert's history gets.
func timelineFor(t outbound.Transition) (string, model.TimelineEventType, bool) {
	return timelineLine(t.Effects.Timeline)
}

// timelineLine is the wording of every effect, in one place. Set-based paths
// that end many commitments at once cannot ask the machine per row, and this is
// what keeps them saying the same thing it would have said.
//
// The wording of a success is deliberate: "sent" and "assumed delivered" are
// different claims, and only one of them is about the world.
func timelineLine(kind outbound.TimelineKind) (string, model.TimelineEventType, bool) {
	switch kind {
	case outbound.TimelineSent:
		return "Notification sent", model.TimelineEventNotificationSent, true
	case outbound.TimelineDelivered:
		return "Notification delivered", model.TimelineEventNotificationSent, true
	case outbound.TimelineAssumedAccepted:
		return "Notification assumed delivered: the provider never confirmed, and the risk was accepted",
			model.TimelineEventNotificationSent, true
	case outbound.TimelineSentAlongsideAck:
		return "Notification went out at the same moment the alert was acknowledged",
			model.TimelineEventNotificationSent, true
	case outbound.TimelineFailed:
		return "Notification failed permanently", model.TimelineEventNotificationFailed, true
	case outbound.TimelineExpired:
		return "A notification expired before it could be sent",
			model.TimelineEventNotificationFailed, true
	case outbound.TimelineCanceled:
		return "A notification was withdrawn", model.TimelineEventNotificationFailed, true
	case outbound.TimelineLeaseLost:
		return "A notification was interrupted mid-flight; whether it arrived is unknown",
			model.TimelineEventNotificationFailed, true
	default:
		return "", "", false
	}
}

// lockAlertGroupTx takes the group's row, and always before any commitment of
// it. Every transaction that can write to a group takes them in this order,
// which is what keeps acknowledgement and delivery from deadlocking.
func lockAlertGroupTx(ctx context.Context, tx *sql.Tx, alertGroupID string) error {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM alert_groups WHERE id = $1 FOR UPDATE`, alertGroupID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock alert group %s: %w", alertGroupID, err)
	}
	return nil
}

// setLockTimeoutTx bounds how long a point mutation waits for a row somebody
// else holds. Waiting longer than the lease it is protecting would mean
// applying a decision that has already been handed to another worker; a
// timeout, by contrast, is a retry of a mutation that classifies itself.
func setLockTimeoutTx(ctx context.Context, tx *sql.Tx, wait time.Duration) error {
	if wait <= 0 {
		wait = outbound.OutboundLockTimeout
	}
	_, err := tx.ExecContext(ctx,
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", wait.Milliseconds()))
	return err
}

// lockIntentTx takes one commitment and reads it as the domain sees it,
// together with whether its own deadline has passed as of this transaction's
// clock.
func lockIntentTx(ctx context.Context, tx *sql.Tx, id string) (*outbound.Intent, bool, error) {
	intent, expired, err := scanIntent(tx.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lock commitment %s: %w", id, err)
	}
	return intent, expired, nil
}

// nextAttemptNoTx is the next number in this commitment's journal, taken under
// the lock the caller already holds. It orders the journal: two records written
// in one transaction share a timestamp, and "the last attempt" has to mean
// something.
func nextAttemptNoTx(ctx context.Context, tx *sql.Tx, intentID string) (int, error) {
	var next int
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(max(attempt_no), 0) + 1 FROM outbound_attempts WHERE intent_id = $1`,
		intentID).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("number the next journal record of %s: %w", intentID, err)
	}
	return next, nil
}

// storedSnapshot is the state a revision is rendered from, as it came back out
// of the database and after it has been proved to be the same thing that went
// in.
type storedSnapshot struct {
	Snapshot keys.RenderSnapshot
	Revision int64
	Final    bool
}

// lockedSnapshotTx reads the state an attempt will render from, and checks that
// it is still what was admitted.
//
// From the domain's own table, never from the live alert group: a retry has to
// send what was accepted, and two instances have to send the same thing.
//
// Four checks, and none of them is defensive programming. The schema version
// says whether this build can read the row at all. The digest says the content
// is the content the commitments were keyed against - a row edited by hand
// would otherwise be sent under the identity of the admission it replaced. The
// group and the revision say the row is about the right thing: a snapshot moved
// between groups, or one revision stored under another's number, would be a
// message about the wrong alert with a key that says it is about the right one.
//
// The first refusal is about this build and leaves the commitment alone; the
// other three are about the row and end it.
func lockedSnapshotTx(ctx context.Context, tx *sql.Tx, alertGroupID string) (storedSnapshot, error) {
	var (
		raw           []byte
		revision      int64
		schemaVersion int
		digest        []byte
		final         bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT snapshot, revision, snapshot_schema_version, snapshot_digest, final
		FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		alertGroupID).Scan(&raw, &revision, &schemaVersion, &digest, &final)
	if errors.Is(err, sql.ErrNoRows) {
		return storedSnapshot{}, undeliverablef(
			"alert group %s has commitments but no state for them to render from", alertGroupID)
	}
	if err != nil {
		return storedSnapshot{}, err
	}
	return checkedSnapshot(raw, revision, schemaVersion, digest, final, alertGroupID)
}

// admittedSnapshotTx reads the state a batch was admitted from - the one a
// one-shot message renders forever.
//
// A direct message is one external effect under one provider key. If it
// rendered the group's current state, a retry would carry different bytes under
// the identity of the first request, and a provider that lost its answer to
// that request would receive a different one as though it were the same. The
// card is the opposite case and reads the group: bringing it up to date is what
// it is for.
func admittedSnapshotTx(ctx context.Context, tx *sql.Tx, intent outbound.Intent) (storedSnapshot, error) {
	var (
		raw           []byte
		revision      sql.NullInt64
		schemaVersion sql.NullInt64
		digest        []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT b.admission_snapshot, b.admission_revision, b.admission_schema_version,
		       b.admission_digest
		FROM outbound_batches b
		JOIN outbound_intents i ON i.batch_id = b.id
		WHERE i.id = $1`, intent.ID).Scan(&raw, &revision, &schemaVersion, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return storedSnapshot{}, undeliverablef(
			"commitment %s has no admission to render from", intent.ID)
	}
	if err != nil {
		return storedSnapshot{}, err
	}
	if raw == nil {
		return storedSnapshot{}, undeliverablef(
			"the admission of %s froze no state, and this commitment renders from one", intent.ID)
	}
	return checkedSnapshot(raw, revision.Int64, int(schemaVersion.Int64), digest,
		false, intent.AlertGroupID)
}

// attemptContentTx reads what an attempt will be made from, in whichever of the
// two forms this commitment has.
//
// The FORM decides, not the emptiness of what comes back. A card and a one-shot
// escalation are drawn from a frozen snapshot with a revision of its own; a
// handover announcement is drawn from its own payload and has no revisions at
// all. Answering the second with an empty snapshot would record a commitment as
// having applied revision 0 of nothing, and would hand a channel a card about
// no alert.
//
// Both sets are closed and both refuse rather than defaulting. A form this
// build does not know would otherwise fall into a branch chosen by whichever
// arm happened to be written last, and reach a provider before anything looked
// at it again. Refused here, it is refused before an attempt exists, which is
// the only point at which refusing is free.
func attemptContentTx(ctx context.Context, tx *sql.Tx,
	intent outbound.Intent, form outbound.ContentForm,
	digest []byte) (outbound.AttemptContent, error) {

	switch form {
	case outbound.ContentPayload:
		return outbound.NewPayloadContent(digest)

	case outbound.ContentSnapshot:
		var stored storedSnapshot
		var err error
		switch intent.Form {
		case outbound.FormEditable:
			stored, err = lockedSnapshotTx(ctx, tx, intent.AlertGroupID)
		case outbound.FormOneShot:
			stored, err = admittedSnapshotTx(ctx, tx, intent)
		default:
			return outbound.AttemptContent{}, outboundContractf(
				"commitment %s is a %q, which is not a form this build delivers",
				intent.ID, intent.Form)
		}
		if err != nil {
			return outbound.AttemptContent{}, err
		}
		return outbound.NewSnapshotContent(stored.Snapshot, stored.Final)
	}

	return outbound.AttemptContent{}, outboundContractf(
		"commitment %s is drawn from a %q, which is not a content form this build has",
		intent.ID, form)
}

// executableHere answers what an attempt on this commitment would be drawn
// from, having established that this build can read the commitment at all.
//
// Asked BEFORE anything is recorded about the commitment, including a refusal.
// A refusal is a durable answer too, and the two failures below make it the
// wrong one: a kind or a payload schema from a newer build is work to leave
// alone rather than a permanent failure, and a channel that decoded a swapped
// payload refused something the domain never promised. The question does not
// depend on what any channel thinks, so it comes first.
//
// The commitment's own row and nothing else - the alert's state is read later,
// and only for an attempt that is actually going to be made.
func executableHere(intent outbound.Intent) (outbound.ContentForm, []byte, error) {
	form := contentFormOf(intent.KeyKind)
	if form == "" {
		// A kind this build has never heard of, and the answer is to change
		// nothing. It is almost certainly a row written by a NEWER build - the
		// upgrade is stop-the-world, but a rollback is not - and the work is
		// perfectly good work that this instance cannot do.
		//
		// Not undeliverable: that ends the commitment for good, and a build
		// that killed work it merely did not understand would destroy exactly
		// what the newer build was going to deliver. This transaction rolls
		// back and the commitment stays pending. Its lease is NOT released by
		// that rollback - the claim committed it earlier - so the instance that
		// knows the kind takes it once locked_until passes.
		return "", nil, outboundContractf(
			"commitment %s is a %q, which is not a kind of claim this build executes",
			intent.ID, intent.KeyKind)
	}
	digest, err := admittedPayloadDigest(intent)
	if err != nil {
		return "", nil, err
	}
	return form, digest, nil
}

// admittedPayloadDigest reads a commitment's payload and answers with what it
// digests to, having established that it is still the payload the commitment
// was admitted with.
//
// Three failures, and they are three different things. A schema this build has
// no shape for is a deployment that is behind, not a broken commitment: the
// instance that wrote it renders it perfectly well, so nothing is written and a
// build that knows the schema takes the work later. Bytes that will not
// canonicalise under a shape this build DOES have are damage a person fixes.
// And bytes that canonicalise to something other than what was admitted are a
// payload swapped for another - it has the right shape, it addresses the same
// recipient, and it is not what the domain promised.
func admittedPayloadDigest(intent outbound.Intent) ([]byte, error) {
	if !keys.KnowsPayloadSchema(intent.KeyKind, intent.PayloadSchemaVersion) {
		return nil, outboundContractf(
			"the payload of %s is at %s schema %d, which this build cannot render",
			intent.ID, intent.KeyKind, intent.PayloadSchemaVersion)
	}
	digest, err := keys.PayloadDigest(intent.KeyKind, intent.PayloadSchemaVersion, intent.Payload)
	if err != nil {
		return nil, undeliverablef(
			"the payload of %s cannot be canonicalised: %v", intent.ID, err)
	}
	// Undeliverable, not a contract error: no other build renders this row any
	// better and no retry changes it. It ends visibly, with the row named, and
	// the external effect never happens.
	if !bytes.Equal(digest, intent.PayloadDigest) {
		return nil, undeliverablef(
			"the payload of %s is not the one it was admitted with: admitted %x, stored %x",
			intent.ID, intent.PayloadDigest, digest)
	}
	return digest, nil
}

// contentFormOf says which of the two forms a kind of claim is drawn from.
//
// A CLOSED switch with no default arm: an unknown kind gets neither form, and
// the caller refuses without touching the commitment. Defaulting either way
// would be wrong in a different direction - to snapshot, and a payload-drawn
// commitment reports a missing state it never had; to payload, and a build
// meeting a newer kind ends work it does not understand.
func contentFormOf(kind keys.Kind) outbound.ContentForm {
	switch kind {
	case keys.KindEscalation, keys.KindEscalationReplay:
		return outbound.ContentSnapshot
	case keys.KindHandoff:
		return outbound.ContentPayload
	default:
		return ""
	}
}

// checkedSnapshot proves a stored snapshot is the same thing that went in,
// whichever row it came out of.
func checkedSnapshot(raw []byte, revision int64, schemaVersion int, digest []byte,
	final bool, alertGroupID string) (storedSnapshot, error) {

	// A version this build does not know is a deployment that is behind, not a
	// broken alert: the instance that wrote it renders it perfectly well. It
	// stops here and changes nothing, so the work waits for a build that can
	// do it instead of being ended by one that cannot.
	if schemaVersion != keys.RenderSnapshotSchemaV1 {
		return storedSnapshot{}, outboundContractf(
			"the state of %s was written under schema version %d, which this build cannot render",
			alertGroupID, schemaVersion)
	}

	var snapshot keys.RenderSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		// A stored snapshot that no longer canonicalises is refused rather than
		// rendered: the message it would produce is not the one its key
		// describes.
		return storedSnapshot{}, undeliverablef(
			"the stored state of %s cannot be read back: %v", alertGroupID, err)
	}

	if !bytes.Equal(snapshot.Digest(), digest) {
		return storedSnapshot{}, undeliverablef(
			"the stored state of %s no longer matches the digest its commitments were keyed against",
			alertGroupID)
	}
	content := snapshot.Content()
	if content.AlertGroupID != alertGroupID || content.Revision != revision {
		return storedSnapshot{}, undeliverablef(
			"the state stored for %s at revision %d describes %s at revision %d",
			alertGroupID, revision, content.AlertGroupID, content.Revision)
	}

	return storedSnapshot{Snapshot: snapshot, Revision: revision, Final: final}, nil
}

// IntentJournal reads everything known about one commitment, in the order it
// happened.
//
// One repeatable-read transaction, and read-only. Four separate reads would be
// four different instants: with a delivery finishing between them the answer
// could hold a commitment that is still sending and the attempt that finished
// it, which is not a state the system was ever in - and this is the read people
// reach for precisely when they are trying to establish what really happened.
func (s *Store) IntentJournal(ctx context.Context, intentID string) (*outbound.Journal, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	intent, _, err := scanIntent(tx.QueryRowContext(ctx, outboundIntentColumns+
		` FROM outbound_intents WHERE id = $1`, intentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	journal := &outbound.Journal{Intent: *intent}

	if journal.Attempts, err = journalAttemptsTx(ctx, tx, intentID); err != nil {
		return nil, err
	}
	if journal.Observations, err = journalObservationsTx(ctx, tx, intentID); err != nil {
		return nil, err
	}
	if journal.Events, err = journalEventsTx(ctx, tx, intentID); err != nil {
		return nil, err
	}
	return journal, tx.Commit()
}

func journalAttemptsTx(ctx context.Context, tx *sql.Tx,
	intentID string) ([]outbound.AttemptRecord, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, attempt_no, record_kind, generation_no, attempt_kind, operation,
		       provider, COALESCE(bound_endpoint, ''), COALESCE(provider_key, ''),
		       applied_revision, started_at, finished_at, COALESCE(outcome, ''),
		       COALESCE(error_class, ''), COALESCE(provider_status, ''),
		       COALESCE(provider_result_detail, ''),
		       receipt, receipt_recorded, receipt_redacted_at,
		       COALESCE(response_summary, ''), COALESCE(finish_reason, ''),
		       COALESCE(completion_fingerprint_version, 0)
		FROM outbound_attempts WHERE intent_id = $1 ORDER BY attempt_no`, intentID)
	if err != nil {
		return nil, fmt.Errorf("read the journal of %s: %w", intentID, err)
	}
	defer rows.Close()

	var attempts []outbound.AttemptRecord
	for rows.Next() {
		var (
			record   outbound.AttemptRecord
			revision sql.NullInt64
			started  sql.NullTime
			finished sql.NullTime
			receipt  []byte
		)
		var redacted sql.NullTime
		if err := rows.Scan(&record.ID, &record.AttemptNo, &record.RecordKind,
			&record.GenerationNo, &record.AttemptKind, &record.Operation, &record.Provider,
			&record.BoundEndpoint, &record.ProviderKey, &revision, &started, &finished,
			&record.Outcome, &record.ErrorClass, &record.ProviderStatus,
			&record.ResultDetail,
			&receipt, &record.ReceiptRecorded, &redacted,
			&record.Summary, &record.FinishReason,
			&record.CompletionFingerprintVersion); err != nil {
			return nil, err
		}
		if len(receipt) > 0 {
			record.Receipt = json.RawMessage(receipt)
		}
		if redacted.Valid {
			at := redacted.Time
			record.ReceiptRedactedAt = &at
		}
		if revision.Valid {
			value := revision.Int64
			record.AppliedRevision = &value
		}
		if started.Valid {
			at := started.Time
			record.StartedAt = &at
		}
		if finished.Valid {
			at := finished.Time
			record.FinishedAt = &at
		}
		attempts = append(attempts, record)
	}
	return attempts, rows.Err()
}

func journalObservationsTx(ctx context.Context, tx *sql.Tx,
	intentID string) ([]outbound.Observation, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT o.attempt_id, o.observation_kind, o.observed_at, o.outcome,
		       COALESCE(o.error_class, ''), COALESCE(o.provider_status, ''),
		       COALESCE(o.provider_result_detail, ''), o.applied_revision,
		       o.receipt, o.receipt_recorded, o.receipt_redacted_at,
		       COALESCE(o.response_summary, ''),
		       COALESCE(o.completion_fingerprint_version, 0)
		FROM outbound_attempt_observations o
		JOIN outbound_attempts a ON a.id = o.attempt_id
		WHERE a.intent_id = $1 ORDER BY a.attempt_no, o.observed_at`, intentID)
	if err != nil {
		return nil, fmt.Errorf("read the late results of %s: %w", intentID, err)
	}
	defer rows.Close()

	var observations []outbound.Observation
	for rows.Next() {
		var (
			o        outbound.Observation
			revision sql.NullInt64
			receipt  []byte
		)
		var redacted sql.NullTime
		if err := rows.Scan(&o.AttemptID, &o.Kind, &o.ObservedAt, &o.Outcome,
			&o.ErrorClass, &o.ProviderStatus, &o.ProviderResultDetail, &revision,
			&receipt, &o.ReceiptRecorded, &redacted,
			&o.Summary, &o.CompletionFingerprintVersion); err != nil {
			return nil, err
		}
		if redacted.Valid {
			at := redacted.Time
			o.ReceiptRedactedAt = &at
		}
		if revision.Valid {
			value := revision.Int64
			o.AppliedRevision = &value
		}
		if len(receipt) > 0 {
			o.Receipt = json.RawMessage(receipt)
		}
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

func journalEventsTx(ctx context.Context, tx *sql.Tx,
	intentID string) ([]outbound.IntentEvent, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT seq, kind, COALESCE(reason, ''), COALESCE(actor, ''),
		       COALESCE(from_status, ''), COALESCE(to_status, '')
		FROM outbound_intent_events WHERE intent_id = $1 ORDER BY seq`, intentID)
	if err != nil {
		return nil, fmt.Errorf("read the events of %s: %w", intentID, err)
	}
	defer rows.Close()

	var events []outbound.IntentEvent
	for rows.Next() {
		var e outbound.IntentEvent
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Reason, &e.Actor,
			&e.FromStatus, &e.ToStatus); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
