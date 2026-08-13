package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// These run against the real database because what they are about lives there:
// which foreign key fires, in what order, and what a cascade takes with it. A
// fake can be told to refuse; only Postgres can be wrong about it.

func TestDeleteTeamRefusesTeamScopedIntegrationWithTypedError(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice")

	// Straight INSERT rather than CreateIntegration: what holds the team is the
	// foreign key, and going through the store would drag in config encryption
	// and an ENCRYPTION_KEY this test has no opinion about.
	if _, err := s.db.Exec(
		`INSERT INTO integrations (id, type, direction, name, enabled, scope, team_id, config)
		 VALUES ($1, 'generic_webhook', 'outbound', 'team hook', true, 'team', $2, '')`,
		"int-team-hook", "devops"); err != nil {
		t.Fatalf("seed team-scoped integration: %v", err)
	}

	err := newTestScheduleService(s, time.Now().UTC()).DeleteTeam(context.Background(), "devops")
	if !errors.Is(err, scheduleconfig.ErrTeamHasIntegrations) {
		t.Fatalf("error = %v, want ErrTeamHasIntegrations", err)
	}
	// The point of mapping it: without one, this reaches the client as the
	// text of integrations_team_id_fkey inside a 500.
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		t.Fatalf("a raw SQL error leaked through the contract: %v", pqErr)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams WHERE id = 'devops'`); n != 1 {
		t.Fatal("a refused delete rolled back nothing: the team is gone")
	}
}

// The reason the schedule blocker is a read and not a constraint: a root with
// no revision chain has nothing for ON DELETE RESTRICT to defend, so the
// cascade from teams would take it silently.
//
// history_complete_from NOT NULL does not close this. It rules out the
// pre-revision row, not the general case - the INSERT below writes a horizon
// and no chain, and Postgres is happy with it. What makes the delete safe is
// that it refuses on the EXISTENCE of a root without asking what is in it, and
// this test fails if that is ever narrowed to some property of the row.
func TestDeleteTeamRefusesAChainlessScheduleRatherThanCascadingIt(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice")

	if _, err := s.db.Exec(
		`INSERT INTO schedules (id, team_id, config_version, history_complete_from)
		 VALUES ($1, $2, 1, NOW())`,
		"chainless-1", "devops"); err != nil {
		t.Fatalf("seed a chainless schedule: %v", err)
	}

	err := newTestScheduleService(s, time.Now().UTC()).DeleteTeam(context.Background(), "devops")
	var blocked *scheduleconfig.TeamHasScheduleHistoryError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %v, want TeamHasScheduleHistoryError", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE id = 'chainless-1'`); n != 1 {
		t.Fatal("the schedule was destroyed by the cascade - silently, which is the whole defect")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams WHERE id = 'devops'`); n != 1 {
		t.Fatal("the team was deleted despite the refusal")
	}
}

func TestDeleteTeamTakesMembershipsAndLeavesHistoryAlone(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	if err := newTestScheduleService(s, time.Now().UTC()).
		DeleteTeam(context.Background(), "devops"); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	// team_members -> teams has no ON DELETE action, so these rows only go if
	// the command takes them.
	if n := countRows(t, s, `SELECT COUNT(*) FROM team_members WHERE team_id = 'devops'`); n != 0 {
		t.Fatalf("%d memberships outlived the team", n)
	}
}

func TestDeleteTeamWithScheduleHistoryIsTypedNotAConstraintError(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	svc := newTestScheduleService(s, now)
	if _, err := createViaSave(context.Background(), svc, "devops", revTestConfig(), "alice", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	err := svc.DeleteTeam(context.Background(), "devops")
	var retained *scheduleconfig.TeamHasScheduleHistoryError
	if !errors.As(err, &retained) {
		t.Fatalf("error = %v, want TeamHasScheduleHistoryError", err)
	}
	if retained.ScheduleID == "" {
		t.Error("the refusal must name the schedule that retains the team")
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("history was touched: %d revisions", n)
	}
}

// LockTeam is what serializes team deletion against anything that would give
// the team a new child row, and this is the mechanism itself rather than its
// consequences: inserting a row that references a team takes FOR KEY SHARE on
// the parent, and FOR UPDATE conflicts with it.
//
// Tested directly rather than through a concurrent create: the delete is two
// statements against a whole create pipeline, so it wins on speed and the
// contended window never opens by chance. Without this test the lock could be
// removed and everything else here would still pass.
func TestLockTeamBlocksConcurrentScheduleInsert(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice")

	locked := make(chan struct{})
	released := make(chan struct{})
	inserted := make(chan time.Time, 1)

	go func() {
		<-locked
		// A bare INSERT, not the service: what is under test is the lock the
		// foreign key takes on the parent row, with nothing else in the way.
		_, err := s.db.Exec(
			`INSERT INTO schedules (id, team_id, config_version, history_complete_from)
			 VALUES ($1, $2, 0, NOW())`,
			"sched-race", "devops")
		if err != nil {
			t.Errorf("insert: %v", err)
		}
		inserted <- time.Now()
	}()

	var releasedAt time.Time
	err := s.ScheduleConfigRepository().WithinTx(context.Background(),
		func(tx scheduleconfig.ScheduleConfigTx) error {
			if err := tx.LockTeam(context.Background(), "devops"); err != nil {
				return err
			}
			close(locked)
			// Long enough that an unblocked insert would land first by a wide
			// margin, so the assertion is not measuring scheduler noise.
			time.Sleep(200 * time.Millisecond)
			releasedAt = time.Now()
			close(released)
			return nil
		})
	if err != nil {
		t.Fatalf("WithinTx: %v", err)
	}

	<-released
	insertedAt := <-inserted
	if insertedAt.Before(releasedAt) {
		t.Fatalf("the insert went through while the team row was locked (%v before release)",
			releasedAt.Sub(insertedAt))
	}
}

// The half of S6-D10 that the lock alone does not give: after waiting for the
// lock, the delete has to SEE the schedule that committed while it waited.
//
// That is READ COMMITTED, and it is the reason the root is read AFTER the
// lock. Both halves are verified by mutation, and they fail differently:
//
//   - WithinTx on sql.LevelRepeatableRead: "could not serialize access due to
//     concurrent update" - the snapshot predates the create;
//   - reading the root before LockTeam: the raw schedule_revisions_schedule_id
//     _fkey violation, which is the exact 500 this sprint exists to remove.
//
// Deterministic by construction, not by repetition: the create holds its
// transaction open until the delete is provably blocked on the lock.
func TestDeleteTeamSeesAScheduleCommittedWhileItWaited(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	// Built on the test goroutine: revTestSnapshot calls t.Fatalf, and Fatal
	// from a spawned goroutine ends that goroutine rather than the test.
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	snapshot := revTestSnapshot(t, start)

	inserted := make(chan struct{})
	commit := make(chan struct{})
	created := make(chan error, 1)

	go func() {
		err := s.ScheduleConfigRepository().WithinTx(context.Background(),
			func(tx scheduleconfig.ScheduleConfigTx) error {
				// This INSERT takes FOR KEY SHARE on the team row, which is
				// what the delete will block on.
				if err := tx.CreateInitialSchedule(context.Background(),
					&scheduleconfig.ScheduleRoot{
						ID: "sched-1", TeamID: "devops",
						ConfigVersion: 1, HistoryCompleteFrom: start,
					},
					&scheduleconfig.ScheduleRevision{
						ID: "rev-1", ScheduleID: "sched-1", Version: 1,
						Kind:     scheduleconfig.RevisionActive,
						Snapshot: snapshot, EffectiveFrom: start,
					}); err != nil {
					return err
				}
				close(inserted)
				<-commit
				return nil
			})
		// However this ended, the test must not wait on a signal that is never
		// coming: a failed insert would otherwise hang here until the package
		// timeout, and report nothing about why.
		select {
		case <-inserted:
		default:
			close(inserted)
		}
		created <- err
	}()

	<-inserted

	deleted := make(chan error, 1)
	go func() {
		deleted <- newTestScheduleService(s, time.Now().UTC()).
			DeleteTeam(context.Background(), "devops")
	}()

	// The delete must be waiting, not finished: if it answered here it either
	// never took the lock or took it before the insert, and the interleaving
	// under test never happened.
	select {
	case err := <-deleted:
		close(commit)
		t.Fatalf("the delete finished while the create still held the team row: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(commit)
	if err := <-created; err != nil {
		t.Fatalf("create: %v", err)
	}

	var retained *scheduleconfig.TeamHasScheduleHistoryError
	if err := <-deleted; !errors.As(err, &retained) {
		t.Fatalf("error = %v, want TeamHasScheduleHistoryError - the delete did not see the schedule it waited for", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM teams WHERE id = 'devops'`); n != 1 {
		t.Fatal("the team was deleted despite the refusal")
	}
}

// The other order, without concurrency because it needs none: once the team is
// gone, the create loses in the contract's vocabulary rather than on a
// constraint. Two typed answers are possible - membership validation runs
// before the insert and the memberships went with the team - and either is a
// legitimate reading of "the team is gone".
func TestCreateScheduleForATeamThatWasJustDeleted(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	svc := newTestScheduleService(s, time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC))
	if err := svc.DeleteTeam(context.Background(), "devops"); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}

	_, err := createViaSave(context.Background(), svc, "devops", revTestConfig(), "alice", nil)
	if !errors.Is(err, scheduleconfig.ErrUserNotTeamMember) &&
		!errors.Is(err, scheduleconfig.ErrTeamNotFound) {
		t.Fatalf("error = %v, want a typed refusal", err)
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		t.Fatalf("a raw SQL error escaped: %v", pqErr)
	}
}

// The mirror image of the delete's own race: an integration write that loses
// to a team deletion.
//
// The delete holds a row lock and answers deterministically, so this side is
// always the loser - and the loser used to hand the caller the name of a
// foreign key inside a 500. Only integrations_team_id_fkey is translated; any
// other constraint on this table means something else.
func TestIntegrationWriteForADeletedTeamIsTyped(t *testing.T) {
	s := setupTestDB(t)

	// Set here rather than relying on the package default: two other tests in
	// this package take the key and `defer os.Unsetenv` it, so whether it is
	// present by the time this runs depends on test order. t.Setenv restores
	// the previous value instead of clearing it.
	t.Setenv(config.EncryptionKeyEnv,
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	scope := model.WebhookScopeTeam
	ghost := "ghost-team"
	err := s.CreateIntegration(&model.Integration{
		ID: "int-orphan", Type: model.IntegrationTypeGenericWebhook,
		Name: "orphan hook", Enabled: true, Scope: &scope, TeamID: &ghost,
		Config: json.RawMessage(`{"url":"https://example.test/hook"}`),
	})
	if !errors.Is(err, ErrIntegrationTeamNotFound) {
		t.Fatalf("create error = %v, want ErrIntegrationTeamNotFound", err)
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		t.Fatalf("a raw SQL error leaked through the contract: %v", pqErr)
	}

	// Update reaches the same foreign key, because moving an integration onto
	// a team scope writes team_id too.
	seedTeam(t, s, "devops")
	live := "devops"
	if err := s.CreateIntegration(&model.Integration{
		ID: "int-live", Type: model.IntegrationTypeGenericWebhook,
		Name: "live hook", Enabled: true, Scope: &scope, TeamID: &live,
		Config: json.RawMessage(`{"url":"https://example.test/hook"}`),
	}); err != nil {
		t.Fatalf("CreateIntegration: %v", err)
	}
	moved, err := s.GetIntegrationByID("int-live")
	if err != nil {
		t.Fatalf("GetIntegrationByID: %v", err)
	}
	moved.TeamID = &ghost
	if err := s.UpdateIntegration(moved); !errors.Is(err, ErrIntegrationTeamNotFound) {
		t.Fatalf("update error = %v, want ErrIntegrationTeamNotFound", err)
	}
}

// The merge DoD asks that the audit trail answer this in one query: a save
// that changes only metadata carries the phase rather than reanchoring it.
//
// It is a query rather than an assertion on the returned plan because that is
// the promise - change_summary is stored next to every revision so someone can
// ask the database later, without replaying the command that produced it.
func TestChangeSummaryShowsMetadataSavesCarryThePhase(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	now := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	svc := newTestScheduleService(s, now)
	if _, err := createViaSave(context.Background(), svc, "devops", revTestConfig(), "alice", nil); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// Metadata only: same groups, same policy, different Slack usergroup.
	metadata := revTestConfig()
	metadata.SlackUsergroupID = "S-different"
	later := newTestScheduleService(s, now.Add(37*time.Hour))
	if _, err := later.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: 1, Desired: metadata, ActorID: "alice",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var action, selection string
	var usergroupChanged, groupsChanged bool
	if err := s.db.QueryRow(`
		SELECT change_summary->>'l1_phase_action',
		       change_summary->>'l1_group_selection',
		       (change_summary->>'slack_usergroup_changed')::bool,
		       (change_summary->>'l1_groups_changed')::bool
		  FROM schedule_revisions
		 WHERE schedule_id = (SELECT id FROM schedules WHERE team_id = 'devops')
		 ORDER BY version DESC LIMIT 1`).
		Scan(&action, &selection, &usergroupChanged, &groupsChanged); err != nil {
		t.Fatalf("read change_summary: %v", err)
	}

	if action != rotation.PhaseActionCarry {
		t.Errorf("l1_phase_action = %q, want carry - a metadata edit restarted the rotation", action)
	}
	if selection != rotation.SelectionPreserve {
		t.Errorf("l1_group_selection = %q, want preserve", selection)
	}
	if !usergroupChanged || groupsChanged {
		t.Errorf("summary must say the usergroup changed and the groups did not, got usergroup=%v groups=%v",
			usergroupChanged, groupsChanged)
	}
}
