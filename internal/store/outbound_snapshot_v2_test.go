package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/alertgroup"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// The second version of the render snapshot in the store: one snapshot, two
// forms, and a revision raised for the form whose bytes moved. The thread does
// not exist yet - it arrives with the satellites - so what these prove about
// it is the half that can be proved now: the card is left alone when only the
// thread's digest moved.

// snapshotRow is the group's state as stored.
type snapshotRow struct {
	revision             int64
	final                bool
	digest, card, thread []byte
	content              keys.SnapshotInput
}

func readSnapshotRow(t *testing.T, s *Store, agID string) snapshotRow {
	t.Helper()
	var (
		row snapshotRow
		raw []byte
	)
	if err := s.db.QueryRow(`
		SELECT revision, final, snapshot_digest, card_digest, thread_digest, snapshot
		FROM outbound_group_snapshots WHERE alert_group_id = $1`, agID).
		Scan(&row.revision, &row.final, &row.digest, &row.card, &row.thread, &raw); err != nil {
		t.Fatalf("read the state of %s: %v", agID, err)
	}
	var stored keys.RenderSnapshot
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("read the stored state back: %v", err)
	}
	row.content = stored.Content()
	return row
}

// formsOf is what each form shows in a stored state, compared at ONE revision
// number: the number is in every digest, so two revisions can only be compared
// about their content by rebuilding one at the other's number.
func formsOf(t *testing.T, content keys.SnapshotInput, revision int64) (card, thread string) {
	t.Helper()
	content.Revision = revision
	snapshot, err := keys.NewRenderSnapshot(content)
	if err != nil {
		t.Fatalf("rebuild the state at revision %d: %v", revision, err)
	}
	return string(snapshot.CardDigest()), string(snapshot.ThreadDigest())
}

// cardAim is where a card stands: its status and the revision it is aimed at.
func cardAim(t *testing.T, s *Store, intentID string) (outbound.Status, int64) {
	t.Helper()
	var (
		status   string
		revision int64
	)
	if err := s.db.QueryRow(`SELECT status, desired_revision FROM outbound_intents WHERE id = $1`,
		intentID).Scan(&status, &revision); err != nil {
		t.Fatalf("read the card %s: %v", intentID, err)
	}
	return outbound.Status(status), revision
}

// slackIntegration is an enabled Slack integration with its button switch as
// given. Created directly, not through the door that raises cards: what the
// tests below prove is what each door does with it.
func slackIntegration(t *testing.T, s *Store, interactive bool) *model.Integration {
	t.Helper()
	cfg, err := json.Marshal(model.SlackConfig{Token: "xoxb-test", Interactive: interactive})
	if err != nil {
		t.Fatal(err)
	}
	integration := &model.Integration{
		Type: model.IntegrationTypeSlack, Name: "Slack", Enabled: true, Config: cfg,
	}
	if err := s.CreateIntegration(integration); err != nil {
		t.Fatalf("create the Slack integration: %v", err)
	}
	return integration
}

func nina() alertgroup.Actor { return alertgroup.Actor{ID: "u-nina", Name: "Nina"} }

// planOf is an escalation as the producer builds one: the state frozen from
// the group and the inputs it read, at the source version it read them at.
func planOf(t *testing.T, s *Store, agID string, commitments ...keys.EscalationCommitment) outbound.Batch {
	t.Helper()
	group, err := s.GetAlertGroupByID(agID)
	if err != nil {
		t.Fatalf("read the group: %v", err)
	}
	inputs, err := s.RenderInputs(context.Background(), agID)
	if err != nil {
		t.Fatalf("read the inputs: %v", err)
	}
	snapshot, err := providers.SnapshotOf(providers.GroupView{
		Group: group, SelfURL: "https://tokay.example", TeamOnboarded: true, Zone: "UTC",
		Timeline: inputs.Timeline, TimelineOmitted: inputs.TimelineOmitted, Interactive: inputs.Interactive,
	})
	if err != nil {
		t.Fatalf("freeze the group: %v", err)
	}
	admission, err := keys.EscalationBatch{
		Kind: keys.KindEscalation, GrammarVersion: keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Snapshot:           snapshot, Commitments: commitments,
	}.Admit()
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	return outbound.Batch{
		Admission: admission,
		Context: outbound.EscalatingAlertGroup(outbound.EscalationContext{
			PolicyID: "policy-1", PolicySnapshot: json.RawMessage(`{"name":"p"}`),
			SourceVersion:  group.RenderSourceVersion,
			OnCallSnapshot: json.RawMessage(`{"l1_users":[]}`),
		}),
		Actor: outbound.ActorEngine,
	}
}

// TestANoteBetweenThePlanAndTheAdmissionRefusesThePlan. The producer reads the
// history before it admits; a note written in between has no snapshot to
// raise yet. It moves the group's source version instead, so the plan built
// before it is refused and the next one carries the line into revision 0.
func TestANoteBetweenThePlanAndTheAdmissionRefusesThePlan(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()
	agID := desiredGroup(t, s, "Disk filling up")

	early := planOf(t, s, agID, channelCommitment("C0001", 0))
	event, err := s.AddAlertGroupNoteAtomic(ctx, agID, "seen it", nina())
	if err != nil {
		t.Fatalf("note before the admission: %v", err)
	}

	result, err := s.SubmitBatch(ctx, early)
	if err != nil {
		t.Fatalf("submit the early plan: %v", err)
	}
	if result.Outcome != outbound.SubmitSourceChanged {
		t.Fatalf("a plan built before the note was answered %q", result.Outcome)
	}
	if countWhere(t, s, `SELECT count(*) FROM outbound_batches WHERE alert_group_id = $1`, agID) != 0 {
		t.Fatal("the refused plan left a claim")
	}

	// The next tick plans from what is now there.
	if result := mustSubmit(t, s, planOf(t, s, agID, channelCommitment("C0001", 0))); result.Outcome != outbound.SubmitCreated {
		t.Fatalf("the next plan was answered %q", result.Outcome)
	}
	for _, line := range readSnapshotRow(t, s, agID).content.Timeline {
		if line.ID == event.ID {
			return
		}
	}
	t.Fatal("revision 0 was frozen without the note")
}

// TestTheSwitchReachesACardTheOperatorCanRevive. A card that failed for good
// is not the end of it: a person can bring it back, and it is then aimed at
// the revision the group is at. So the group is raised for the switch while
// the card is down, or the revived card would carry the buttons the switch
// took away.
func TestTheSwitchReachesACardTheOperatorCanRevive(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)
	aim(t, s, agID)

	// The card's change fails for good; the card keeps its receipt.
	token := claimOne(t, s, cardID)
	begun := beginOne(t, s, cardID, token)
	if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "cant_update_message"),
	}); err != nil {
		t.Fatalf("fail the change: %v", err)
	}
	if got := statusOf(t, s, cardID); got != outbound.StatusPermanentFailed {
		t.Fatalf("the card is %s", got)
	}
	before := readSnapshotRow(t, s, agID)

	slackIntegration(t, s, true)
	raised, err := s.RaiseInteractivity(ctx, keys.InteractiveSlack, outbound.ActorSystem)
	if err != nil || raised != 1 {
		t.Fatalf("the switch reached %d group(s) (%v); the card down for good still counts", raised, err)
	}
	after := readSnapshotRow(t, s, agID)
	if after.revision != before.revision+1 || !after.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("the snapshot is at revision %d with buttons %v", after.revision, after.content.InteractiveProviders)
	}
	if status, _ := cardAim(t, s, cardID); status != outbound.StatusPermanentFailed {
		t.Fatalf("the raise moved a card that failed for good to %s", status)
	}

	// The person brings it back, and it is aimed at the revision with buttons.
	if result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: cardID, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "fixed",
	}); result.Status != outbound.StatusPending {
		t.Fatalf("the retry answered %q into %s", result.Outcome, result.Status)
	}
	if status, aimed := cardAim(t, s, cardID); status != outbound.StatusPending || aimed != after.revision {
		t.Fatalf("the revived card is %s at %d; want pending at %d", status, aimed, after.revision)
	}

	// And the start's reconciliation counts it the same way.
	if _, err := s.db.Exec(`UPDATE integrations SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE outbound_intents SET status = 'expired', lease_token = NULL, locked_until = NULL
		WHERE id = $1`, cardID); err != nil {
		t.Fatal(err)
	}
	if raised, err := s.ReconcileInteractivity(ctx); err != nil || raised != 1 {
		t.Fatalf("the start reached %d group(s) (%v); an expired card still counts", raised, err)
	}
}

// TestANoteMovesTheThreadAndLeavesTheCardAlone. A note is a line the thread
// shows and the card does not: the snapshot moves, the thread's digest moves,
// the card's digest does not, and the card stays parked at the revision it
// applied - a card behind the group's number by a line it does not render.
func TestANoteMovesTheThreadAndLeavesTheCardAlone(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)

	// Revision 0 here was frozen by the fixture, not from the live group, so
	// the first raise moves the card too. The baseline is the first revision
	// raised from the group itself.
	if _, err := s.AddAlertGroupNoteAtomic(context.Background(), agID, "first look", nina()); err != nil {
		t.Fatalf("first note: %v", err)
	}
	before := readSnapshotRow(t, s, agID)
	statusBefore, aimBefore := cardAim(t, s, cardID)
	journalBefore := countJournalRows(t, s, cardID)

	event, err := s.AddAlertGroupNoteAtomic(context.Background(), agID, "looking into it", nina())
	if err != nil {
		t.Fatalf("note: %v", err)
	}
	if event.Actor != "Nina" || event.Type != model.TimelineEventNote || event.CreatedAt.IsZero() {
		t.Fatalf("the note was recorded as %+v", event)
	}

	after := readSnapshotRow(t, s, agID)
	if after.revision != before.revision+1 {
		t.Fatalf("the note moved the revision from %d to %d", before.revision, after.revision)
	}
	card, thread := formsOf(t, after.content, before.revision)
	if thread == string(before.thread) {
		t.Fatal("a note did not change what the thread shows")
	}
	if card != string(before.card) {
		t.Fatal("a note changed what the card shows")
	}
	found := false
	for _, line := range after.content.Timeline {
		if line.ID == event.ID && line.Message == "looking into it" && line.Actor != nil && *line.Actor == "Nina" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the note is not in the snapshot: %+v", after.content.Timeline)
	}

	// The card: not raised, not woken, no line in its journal.
	if status, aimed := cardAim(t, s, cardID); status != statusBefore || aimed != aimBefore {
		t.Fatalf("the card is %s at revision %d; want %s at %d", status, aimed, statusBefore, aimBefore)
	}
	if n := countJournalRows(t, s, cardID); n != journalBefore {
		t.Fatalf("the card's journal grew by %d line(s) for a note it does not show", n-journalBefore)
	}
}

// TestANoteIsBoundedAndSignedAndCut. The note is between one and NoteLimit
// runes, addressed to a group that exists, signed by the caller; the snapshot
// keeps the first TimelineMessageLimit runes of it, like every field from
// outside.
func TestANoteIsBoundedAndSignedAndCut(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, agID)
	ctx := context.Background()

	for name, text := range map[string]string{
		"an empty note":   "",
		"a note too long": strings.Repeat("x", NoteLimit+1),
	} {
		if _, err := s.AddAlertGroupNoteAtomic(ctx, agID, text, nina()); !errors.Is(err, ErrNoteInvalid) {
			t.Errorf("%s was answered %v", name, err)
		}
	}
	if _, err := s.AddAlertGroupNoteAtomic(ctx, uuid.New().String(), "hello", nina()); err != sql.ErrNoRows {
		t.Errorf("a note on a group nobody has was answered %v", err)
	}
	if _, err := s.AddAlertGroupNoteAtomic(ctx, agID, "hello", alertgroup.Actor{Name: "nobody"}); err == nil {
		t.Error("a note with no person behind it was accepted")
	}

	longest := strings.Repeat("y", NoteLimit)
	event, err := s.AddAlertGroupNoteAtomic(ctx, agID, longest, nina())
	if err != nil {
		t.Fatalf("the longest note: %v", err)
	}
	if event.Message != longest {
		t.Fatal("the history line does not carry the whole note")
	}
	for _, line := range readSnapshotRow(t, s, agID).content.Timeline {
		if line.ID == event.ID {
			if want := strings.Repeat("y", keys.TimelineMessageLimit) + keys.AlertDescriptionEllipsis; line.Message != want {
				t.Fatalf("the snapshot keeps %d runes of the note", len([]rune(line.Message)))
			}
			return
		}
	}
	t.Fatal("the longest note is not in the snapshot")
}

// TestANoteOnAFinishedIncidentIsWrittenAndRaisesNothing. The last revision is
// out; a note is still a line of the history, and nothing is raised for it.
func TestANoteOnAFinishedIncidentIsWrittenAndRaisesNothing(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, agID)
	moveGroup(t, s, agID, model.AlertGroupStatusResolved)
	if result, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: agID, Reason: outbound.DesiredResolve, Actor: outbound.ActorSystem,
	}); err != nil || result.Outcome != outbound.DesiredApplied {
		t.Fatalf("resolve: %s (%v)", result.Outcome, err)
	}
	final := readSnapshotRow(t, s, agID)
	if !final.final {
		t.Fatal("the fixture's snapshot is not final")
	}

	event, err := s.AddAlertGroupNoteAtomic(context.Background(), agID, "post-mortem link", nina())
	if err != nil {
		t.Fatalf("note after the end: %v", err)
	}
	if countWhere(t, s, `SELECT count(*) FROM timeline_events WHERE id = $1`, event.ID) != 1 {
		t.Fatal("the note was not written")
	}
	if after := readSnapshotRow(t, s, agID); after.revision != final.revision || string(after.digest) != string(final.digest) {
		t.Fatal("a note after the last revision moved the snapshot")
	}
}

// TestTheButtonSwitchRaisesTheCards. The switch moves, the card's digest moves,
// the card is raised - and a card whose last revision is out is not.
func TestTheButtonSwitchRaisesTheCards(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()

	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)
	finished := desiredGroup(t, s, "Memory filling up")
	finishedCard := changeableCard(t, s, finished)
	moveGroup(t, s, finished, model.AlertGroupStatusResolved)
	if _, err := raiseDesired(t, s, outbound.DesiredStateRequest{
		AlertGroupID: finished, Reason: outbound.DesiredResolve, Actor: outbound.ActorSystem,
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Revision 0 was frozen by the fixture, not from the live group; the
	// baseline is a revision raised from the group itself, with no switch on.
	if _, err := s.RaiseInteractivity(ctx, keys.InteractiveSlack, outbound.ActorSystem); err != nil {
		t.Fatalf("baseline raise: %v", err)
	}
	before := readSnapshotRow(t, s, agID)
	if len(before.content.InteractiveProviders) != 0 {
		t.Fatalf("the baseline card was drawn with buttons: %v", before.content.InteractiveProviders)
	}

	slackIntegration(t, s, true)
	raised, err := s.RaiseInteractivity(ctx, keys.InteractiveSlack, outbound.ActorSystem)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if raised != 2 {
		t.Fatalf("%d group(s) were visited, want the two with live cards", raised)
	}

	after := readSnapshotRow(t, s, agID)
	if after.revision != before.revision+1 || !after.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("the switch left the snapshot at revision %d with buttons %v",
			after.revision, after.content.InteractiveProviders)
	}
	if card, thread := formsOf(t, after.content, before.revision); card == string(before.card) ||
		thread != string(before.thread) {
		t.Fatal("the switch did not move what the card shows, and only that")
	}
	if status, aimed := cardAim(t, s, cardID); status != outbound.StatusPending || aimed != after.revision {
		t.Fatalf("the card is %s at revision %d; want pending at %d", status, aimed, after.revision)
	}
	if _, aimed := cardAim(t, s, finishedCard); aimed != readSnapshotRow(t, s, finished).revision {
		t.Fatal("a card whose last revision is out was aimed elsewhere")
	}

	// The switch has not moved since: nothing to raise, nothing written.
	if _, err := s.RaiseInteractivity(ctx, keys.InteractiveSlack, outbound.ActorSystem); err != nil {
		t.Fatalf("raise again: %v", err)
	}
	if got := readSnapshotRow(t, s, agID).revision; got != after.revision {
		t.Fatalf("a switch that did not move raised the revision to %d", got)
	}
}

// TestTheSwitchDoorRunsFromTheIntegrationUpdate. Moving the switch through
// UpdateIntegration brings the cards up to date after its commit; changing
// something else about the integration does not touch them.
func TestTheSwitchDoorRunsFromTheIntegrationUpdate(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()
	integration := slackIntegration(t, s, false)
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)
	before := readSnapshotRow(t, s, agID)

	renamed := "Slack, renamed"
	if _, err := s.UpdateIntegration(ctx, integration.ID, IntegrationPatch{Name: &renamed}, "u-nina"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := readSnapshotRow(t, s, agID).revision; got != before.revision {
		t.Fatalf("a rename raised the revision to %d", got)
	}

	on, _ := json.Marshal(model.SlackConfig{Token: "xoxb-test", Interactive: true})
	if _, err := s.UpdateIntegration(ctx, integration.ID, IntegrationPatch{Config: on}, "u-nina"); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	after := readSnapshotRow(t, s, agID)
	if after.revision != before.revision+1 || !after.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("the switch left the snapshot at revision %d with buttons %v",
			after.revision, after.content.InteractiveProviders)
	}
	if status, aimed := cardAim(t, s, cardID); status != outbound.StatusPending || aimed != after.revision {
		t.Fatalf("the card is %s at revision %d; want pending at %d", status, aimed, after.revision)
	}
}

// TestEveryStartBringsTheCardsUpToDate. The door in the request is
// best-effort; the start compares what each live card was drawn with against
// the switches as they stand, and raises the difference - once.
func TestEveryStartBringsTheCardsUpToDate(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)
	before := readSnapshotRow(t, s, agID)

	if raised, err := s.ReconcileInteractivity(ctx); err != nil || raised != 0 {
		t.Fatalf("a start with nothing behind raised %d (%v)", raised, err)
	}

	// The switch moved without its door - the instance that moved it died.
	slackIntegration(t, s, true)
	raised, err := s.ReconcileInteractivity(ctx)
	if err != nil || raised != 1 {
		t.Fatalf("the start raised %d (%v), want the one card behind the switch", raised, err)
	}
	after := readSnapshotRow(t, s, agID)
	if after.revision != before.revision+1 || !after.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("the start left the snapshot at revision %d with buttons %v",
			after.revision, after.content.InteractiveProviders)
	}
	if status, aimed := cardAim(t, s, cardID); status != outbound.StatusPending || aimed != after.revision {
		t.Fatalf("the card is %s at revision %d; want pending at %d", status, aimed, after.revision)
	}

	if raised, err := s.ReconcileInteractivity(ctx); err != nil || raised != 0 {
		t.Fatalf("a second start raised %d (%v)", raised, err)
	}
}

// TestANoteAndAnAcknowledgementAgreeOnTheRevision is the race between two
// doors on one group. Neither wins in a way this test cares about; what has to
// hold is that the revision moved once per raise, that the card was raised
// exactly once - by the acknowledgement, whichever came first - and is aimed
// at that revision, and that neither door failed. A card aimed one behind the
// group is a card the note did not reach, which is right: it does not show
// notes.
func TestANoteAndAnAcknowledgementAgreeOnTheRevision(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)
	moveGroup(t, s, agID, model.AlertGroupStatusTriggered)
	// Revision 0 was frozen by the fixture, not from the live group, and the
	// first raise from the group moves the card whichever door makes it. The
	// race starts from a revision raised from the group itself.
	if _, err := s.AddAlertGroupNoteAtomic(context.Background(), agID, "first look", nina()); err != nil {
		t.Fatalf("baseline note: %v", err)
	}
	before := readSnapshotRow(t, s, agID)
	raisedBefore := countWhere(t, s, `SELECT count(*) FROM outbound_intent_events
		WHERE intent_id = $1 AND kind = 'desired_raised'`, cardID)

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make(chan error, 2)
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := s.AddAlertGroupNoteAtomic(context.Background(), agID, "on it", nina())
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		changed, err := s.AckAlertGroupAtomic(agID, alertgroup.Actor{ID: "u-olga", Name: "Olga"}, nil, nil)
		if err == nil && !changed {
			err = errors.New("the acknowledgement changed nothing")
		}
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a door failed: %v", err)
		}
	}

	after := readSnapshotRow(t, s, agID)
	if after.revision != before.revision+2 {
		t.Fatalf("two raises moved the revision from %d to %d", before.revision, after.revision)
	}
	_, aimed := cardAim(t, s, cardID)
	if aimed <= before.revision || aimed > after.revision {
		t.Fatalf("the card is aimed at %d, and the group went from %d to %d", aimed, before.revision, after.revision)
	}
	if raised := countWhere(t, s, `SELECT count(*) FROM outbound_intent_events
		WHERE intent_id = $1 AND kind = 'desired_raised'`, cardID); raised != raisedBefore+1 {
		t.Fatalf("the card was raised %d time(s) by the race; the acknowledgement raises it once and the note not at all",
			raised-raisedBefore)
	}
	if after.content.AcknowledgedBy == nil || len(after.content.Timeline) == 0 {
		t.Fatal("the last revision does not carry both doors")
	}
}

// TestASweepTakesTheLeavesBeforeTheParent. A commitment another one names as
// its parent is not removed while the child stands: with a chunk of one, the
// child goes first and the parent on the next pass, and no pass fails on the
// key between them.
func TestASweepTakesTheLeavesBeforeTheParent(t *testing.T) {
	s := setupTestDB(t)
	ctx := context.Background()
	parent := admitOne(t, s, outboundGroup(t, s), channelCommitment("C0001", 0))[0]
	child := admitOne(t, s, outboundGroup(t, s), channelCommitment("C0002", 0))[0]
	refusedForGood(t, s, parent)
	expireLease(t, s, child)
	refusedForGood(t, s, child)

	// The link, as a satellite will carry it. Its target has to say so too:
	// the rule that a satellite names a parent reads both.
	if _, err := s.db.Exec(`
		UPDATE outbound_intents
		SET parent_intent_id = $2, target_kind = $3,
		    payload = jsonb_set(payload, '{target,kind}', to_jsonb($3::text))
		WHERE id = $1`, child, parent, string(keys.TargetThread)); err != nil {
		t.Fatalf("link the child to its parent: %v", err)
	}
	ageIntent(t, s, parent, 40*day)
	ageIntent(t, s, child, 35*day)

	cutoff := time.Now().Add(-30 * day)
	exists := func(id string) bool {
		return countWhere(t, s, `SELECT count(*) FROM outbound_intents WHERE id = $1`, id) == 1
	}

	first, err := s.SweepDeliveryHistory(ctx, cutoff, 1)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Deleted.Intents != 1 || exists(child) || !exists(parent) {
		t.Fatalf("the first pass removed %d, child present %v, parent present %v; want the child alone",
			first.Deleted.Intents, exists(child), exists(parent))
	}
	second, err := s.SweepDeliveryHistory(ctx, cutoff, 1)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Deleted.Intents != 1 || exists(parent) {
		t.Fatal("the second pass did not remove the parent")
	}
	if third, err := s.SweepDeliveryHistory(ctx, cutoff, 1); err != nil || third.Deleted.Intents != 0 {
		t.Fatalf("the third pass removed %d (%v)", third.Deleted.Intents, err)
	}
}

// versionOneDigest reads a stored snapshot the way version 1 stored it -
// without the three fields version 2 added - and digests it by version 1's
// rules, which is what the column beside it said before the upgrade.
func versionOneDigest(t *testing.T, s *Store, table, column, where, id string) []byte {
	t.Helper()
	var stripped []byte
	if err := s.db.QueryRow(`SELECT `+column+` - 'timeline' - 'timeline_omitted' - 'interactive_providers'
		FROM `+table+` WHERE `+where+` = $1`, id).Scan(&stripped); err != nil {
		t.Fatalf("read the stored state: %v", err)
	}
	older, err := keys.DecodeRenderSnapshotV1(stripped)
	if err != nil {
		t.Fatalf("read the state as version 1: %v", err)
	}
	return older.Digest()
}

// TestAStartRebuildsTheSnapshotsOfThePreviousVersion. A database whose group
// snapshots are version 1 comes up with them as version 2: the same content
// under three digests, the history the thread shows, the buttons the cards
// were drawn with - and the revision and finality of every row untouched.
// The start-up reconciliation then finds the card behind the switch as it
// stands, which is how a card sent with buttons before the switch moved loses
// them on the first start of this version.
func TestAStartRebuildsTheSnapshotsOfThePreviousVersion(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID) // drawn with buttons: the payload says so
	if err := s.AddTimelineEvent(&model.TimelineEvent{
		ID: uuid.New().String(), AlertGroupID: agID, Type: model.TimelineEventNote,
		Message: "written by the previous version", Actor: "nina", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("write the old note: %v", err)
	}

	// The row as the previous version left it, and the schema too.
	digest := versionOneDigest(t, s, "outbound_group_snapshots", "snapshot", "alert_group_id", agID)
	for _, statement := range []string{
		`UPDATE outbound_group_snapshots
		 SET snapshot = snapshot - 'timeline' - 'timeline_omitted' - 'interactive_providers',
		     snapshot_schema_version = ` + "1" + `, snapshot_digest = '\x` + hexOf(digest) + `'
		 WHERE alert_group_id = '` + agID + `'`,
		`ALTER TABLE outbound_group_snapshots DROP CONSTRAINT IF EXISTS ` + outboundFormDigestLen,
		`ALTER TABLE outbound_group_snapshots DROP COLUMN IF EXISTS card_digest, DROP COLUMN IF EXISTS thread_digest`,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` + outboundSatelliteNamesParent,
		`DROP INDEX IF EXISTS idx_outbound_intents_parent`,
		`ALTER TABLE outbound_intents DROP COLUMN IF EXISTS parent_intent_id, DROP COLUMN IF EXISTS bound_context`,
		`ALTER TABLE outbound_intents DROP CONSTRAINT IF EXISTS ` + outboundTargetAgreementConstraint,
		`ALTER TABLE outbound_intents ADD CONSTRAINT outbound_intents_payload_addresses_the_target CHECK (
			key_kind NOT IN ('escalation', 'escalation_replay') OR payload_schema_version <> 1
			OR (payload #>> '{target,kind}' IS NOT DISTINCT FROM target_kind
				AND payload #>> '{target,ref}' IS NOT DISTINCT FROM target_ref)) NOT VALID`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("build the previous version: %v", err)
		}
	}

	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused the previous version's database: %v", err)
	}

	var version int
	if err := s.db.QueryRow(`SELECT snapshot_schema_version FROM outbound_group_snapshots
		WHERE alert_group_id = $1`, agID).Scan(&version); err != nil || version != keys.RenderSnapshotSchemaV2 {
		t.Fatalf("the snapshot is at version %d (%v)", version, err)
	}
	rebuilt := readSnapshotRow(t, s, agID)
	if rebuilt.revision != 0 || rebuilt.final || len(rebuilt.card) != 32 || len(rebuilt.thread) != 32 {
		t.Fatalf("the rebuilt row is revision %d, final %v, digests %d/%d bytes",
			rebuilt.revision, rebuilt.final, len(rebuilt.card), len(rebuilt.thread))
	}
	noted := false
	for _, line := range rebuilt.content.Timeline {
		if line.Message == "written by the previous version" && line.Actor != nil && *line.Actor == "nina" {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the rebuilt snapshot lacks the history: %+v", rebuilt.content.Timeline)
	}
	if !rebuilt.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("the rebuilt snapshot lost the buttons the card was drawn with: %v",
			rebuilt.content.InteractiveProviders)
	}
	for _, column := range []string{"parent_intent_id", "bound_context"} {
		if !hasColumn(t, s, "outbound_intents", column) {
			t.Errorf("the start did not add %s", column)
		}
	}
	if !hasNamedConstraint(t, s, "outbound_intents", outboundSatelliteNamesParent) ||
		!hasNamedConstraint(t, s, "outbound_group_snapshots", outboundFormDigestLen) ||
		!hasNamedConstraint(t, s, "outbound_intents", outboundTargetAgreementConstraint) ||
		hasNamedConstraint(t, s, "outbound_intents", "outbound_intents_payload_addresses_the_target") {
		t.Error("the rules of the second version are not all in place, or the old one is")
	}
	if status, aimed := cardAim(t, s, cardID); status != outbound.StatusIdle || aimed != 0 {
		t.Fatalf("the rebuild itself moved the card to %s at %d", status, aimed)
	}

	// No Slack integration stands: the card was drawn with buttons the switch
	// does not grant, and the start's reconciliation redraws it without.
	raised, err := s.ReconcileInteractivity(ctx)
	if err != nil || raised != 1 {
		t.Fatalf("the start raised %d (%v), want the one card drawn with buttons", raised, err)
	}
	after := readSnapshotRow(t, s, agID)
	if after.revision != 1 || after.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("after the start the snapshot is at revision %d with buttons %v",
			after.revision, after.content.InteractiveProviders)
	}
	if status, aimed := cardAim(t, s, cardID); status != outbound.StatusPending || aimed != 1 {
		t.Fatalf("the card is %s at %d; want pending at 1", status, aimed)
	}

	// A second start finds nothing to rebuild.
	if err := s.InitDB(); err != nil {
		t.Fatalf("the second start refused: %v", err)
	}
	if got := readSnapshotRow(t, s, agID).revision; got != 1 {
		t.Fatalf("the second start moved the revision to %d", got)
	}
}

// TestAStartReachesARevivableCardOfThePreviousVersion. The rebuild counts the
// buttons of a card that failed for good - they are in the chat - and the
// start's reconciliation raises its group like any other, so the card a
// person brings back is aimed at a revision drawn by the switch as it stands.
func TestAStartReachesARevivableCardOfThePreviousVersion(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	ctx := context.Background()
	agID := desiredGroup(t, s, "Disk filling up")
	cardID := changeableCard(t, s, agID)
	aim(t, s, agID)
	token := claimOne(t, s, cardID)
	begun := beginOne(t, s, cardID, token)
	if _, err := s.FinalizeDeliveryAttempt(ctx, outbound.FinalizeRequest{
		AttemptID: begun.AttemptID, LeaseToken: token,
		Conclusion: concluded(outbound.OutcomePermanentRejection, "cant_update_message"),
	}); err != nil {
		t.Fatalf("fail the change: %v", err)
	}

	digest := versionOneDigest(t, s, "outbound_group_snapshots", "snapshot", "alert_group_id", agID)
	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = snapshot - 'timeline' - 'timeline_omitted' - 'interactive_providers',
		    snapshot_schema_version = 1, snapshot_digest = $2
		WHERE alert_group_id = $1`, agID, digest); err != nil {
		t.Fatalf("build the previous version: %v", err)
	}
	if err := s.InitDB(); err != nil {
		t.Fatalf("the start refused: %v", err)
	}
	rebuilt := readSnapshotRow(t, s, agID)
	if !rebuilt.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("the rebuilt snapshot lost the buttons of a card that failed for good: %v",
			rebuilt.content.InteractiveProviders)
	}
	if raised, err := s.ReconcileInteractivity(ctx); err != nil || raised != 1 {
		t.Fatalf("the start raised %d (%v), want the group of the failed card", raised, err)
	}
	after := readSnapshotRow(t, s, agID)
	if after.revision != rebuilt.revision+1 || after.content.ButtonsOn(keys.InteractiveSlack) {
		t.Fatalf("after the start the snapshot is at %d with buttons %v", after.revision, after.content.InteractiveProviders)
	}
	if result := resolve(t, s, outbound.ResolveAmbiguityRequest{
		IntentID: cardID, Decision: outbound.DecisionRetryCurrentGeneration, Reason: "fixed",
	}); result.Status != outbound.StatusPending {
		t.Fatalf("the retry answered %q into %s", result.Outcome, result.Status)
	}
	if _, aimed := cardAim(t, s, cardID); aimed != after.revision {
		t.Fatalf("the revived card is aimed at %d, the group is at %d", aimed, after.revision)
	}
}

// TestAStartRefusesAVersionOneSnapshotSomebodyAltered. A row that reads as
// version 1 but no longer digests to what its column says is not rebuilt: the
// rebuild would sign content nobody admitted. Unreachable through the doors;
// what is refused is damage, by name.
func TestAStartRefusesAVersionOneSnapshotSomebodyAltered(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, agID)

	digest := versionOneDigest(t, s, "outbound_group_snapshots", "snapshot", "alert_group_id", agID)
	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = jsonb_set(snapshot - 'timeline' - 'timeline_omitted' - 'interactive_providers',
		                         '{title}', '"somebody else''s alert"'),
		    snapshot_schema_version = 1, snapshot_digest = $2
		WHERE alert_group_id = $1`, agID, digest); err != nil {
		t.Fatalf("alter the row: %v", err)
	}
	t.Cleanup(func() { removeOutboundStateOf(t, s, agID) })

	err := s.applyOutboundSchema()
	if err == nil {
		t.Fatal("a start rebuilt a snapshot that no longer matches its digest")
	}
	if !strings.Contains(err.Error(), agID) || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("the refusal does not name the group and the digest: %v", err)
	}
}

// removeOutboundStateOf is the remedy a refused start names, applied to one
// group, so the starts after it in this process are not refused for the same
// reason.
func removeOutboundStateOf(t *testing.T, s *Store, agID string) {
	t.Helper()
	for _, statement := range []string{
		`DELETE FROM outbound_intent_events e USING outbound_intents i
		 WHERE e.intent_id = i.id AND i.alert_group_id = $1`,
		`DELETE FROM outbound_attempts a USING outbound_intents i
		 WHERE a.intent_id = i.id AND i.alert_group_id = $1`,
		`UPDATE outbound_intents SET current_attempt_id = NULL WHERE alert_group_id = $1`,
		`DELETE FROM outbound_intents WHERE alert_group_id = $1`,
		`DELETE FROM outbound_batches WHERE alert_group_id = $1`,
		`DELETE FROM outbound_group_snapshots WHERE alert_group_id = $1`,
	} {
		if _, err := s.db.Exec(statement, agID); err != nil {
			t.Fatalf("apply the remedy: %v", err)
		}
	}
}

// TestAStartRefusesAVersionOneSnapshotItCannotRead. A row that will not read
// as version 1 cannot be rebuilt, and the start says which group rather than
// guessing a state for it.
func TestAStartRefusesAVersionOneSnapshotItCannotRead(t *testing.T) {
	s := setupTestDB(t)
	s.SetRenderEnvironment("https://tokay.example", "UTC")
	agID := desiredGroup(t, s, "Disk filling up")
	changeableCard(t, s, agID)

	if _, err := s.db.Exec(`
		UPDATE outbound_group_snapshots
		SET snapshot = $2::jsonb, snapshot_schema_version = 1
		WHERE alert_group_id = $1`, agID,
		`{"alert_group_id":"`+agID+`","revision":0,"status":"firing","display_timezone":"UTC","alerts":[]}`); err != nil {
		t.Fatalf("damage the row: %v", err)
	}
	t.Cleanup(func() {
		for _, statement := range []string{
			`DELETE FROM outbound_intent_events e USING outbound_intents i
			 WHERE e.intent_id = i.id AND i.alert_group_id = $1`,
			`DELETE FROM outbound_attempts a USING outbound_intents i
			 WHERE a.intent_id = i.id AND i.alert_group_id = $1`,
			`UPDATE outbound_intents SET current_attempt_id = NULL WHERE alert_group_id = $1`,
			`DELETE FROM outbound_intents WHERE alert_group_id = $1`,
			`DELETE FROM outbound_batches WHERE alert_group_id = $1`,
			`DELETE FROM outbound_group_snapshots WHERE alert_group_id = $1`,
		} {
			if _, err := s.db.Exec(statement, agID); err != nil {
				t.Fatalf("apply the remedy: %v", err)
			}
		}
	})

	err := s.applyOutboundSchema()
	if err == nil {
		t.Fatal("a start accepted a snapshot it cannot rebuild")
	}
	if !strings.Contains(err.Error(), agID) || !strings.Contains(err.Error(), "version 1") {
		t.Fatalf("the refusal does not name the group and the version: %v", err)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}
