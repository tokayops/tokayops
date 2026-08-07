package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// eraseUser runs the full erasure cycle for one user in one transaction, the
// way the command layer will.
func eraseUser(t *testing.T, s *Store, userID string, at time.Time) {
	t.Helper()
	err := s.ErasureRepository().WithinTx(context.Background(), func(tx erasure.Tx) error {
		if err := tx.SetUserDeletedAt(context.Background(), userID, at); err != nil {
			return err
		}
		if err := tx.AnonymizeUser(context.Background(), userID); err != nil {
			return err
		}
		if err := tx.DeleteUserAPITokens(context.Background(), userID); err != nil {
			return err
		}
		if err := tx.DeleteUserExternalIdentities(context.Background(), userID); err != nil {
			return err
		}
		if err := tx.DeleteUserLinkTokens(context.Background(), userID); err != nil {
			return err
		}
		if err := tx.NullifyOverrideRevisionReasons(context.Background(), userID); err != nil {
			return err
		}
		return tx.NullifyScheduleRevisionChangeReasons(context.Background(), userID)
	})
	if err != nil {
		t.Fatalf("erase %s: %v", userID, err)
	}
}

// seedErasureFixture builds a user with every artefact erasure has to reach,
// plus an untouched neighbour to prove the wipe is scoped.
func seedErasureFixture(t *testing.T, s *Store) (scheduleID string) {
	t.Helper()
	seedTeam(t, s, "devops")
	for _, u := range []struct{ id, email, name string }{
		{"alice", "alice@example.com", "Alice"},
		{"bob", "bob@example.com", "Bob"},
	} {
		if err := s.CreateUser(&model.User{ID: u.id, Email: u.email, Name: u.name}); err != nil {
			t.Fatalf("CreateUser %s: %v", u.id, err)
		}
		if err := s.AddTeamMember("devops", u.id, model.TeamMemberRoleMember); err != nil {
			t.Fatalf("AddTeamMember %s: %v", u.id, err)
		}
		if err := s.BindExternalIdentity(&model.ExternalIdentity{
			UserID: u.id, Provider: "slack", ExternalID: "U" + u.id,
		}); err != nil {
			t.Fatalf("BindExternalIdentity %s: %v", u.id, err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO api_tokens (id, user_id, name, token_hash) VALUES ($1, $2, 'ci', $3)`,
			"tok-"+u.id, u.id, "hash-"+u.id); err != nil {
			t.Fatalf("insert api token %s: %v", u.id, err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO link_tokens (id, user_id, provider, token_hash, expires_at)
			 VALUES ($1, $2, 'telegram', $3, NOW() + INTERVAL '1 hour')`,
			"link-"+u.id, u.id, "linkhash-"+u.id); err != nil {
			t.Fatalf("insert link token %s: %v", u.id, err)
		}
	}

	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev, err := newTestScheduleService(s, start).CreateSchedule(context.Background(), "devops", revTestConfig())
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	scheduleID = rev.ScheduleID

	// A legacy override so the join scan that reads u.email is covered too.
	if _, err := s.db.Exec(
		`INSERT INTO schedule_overrides (id, schedule_id, user_id, start_time, end_time, created_by)
		 VALUES ('legacy-ovr', $1, 'alice', $2, $3, 'alice')`,
		scheduleID, start.Add(24*time.Hour), start.Add(30*time.Hour)); err != nil {
		t.Fatalf("insert legacy override: %v", err)
	}

	// Free-text reasons naming a person, on both history tables and for both
	// the target and the author role.
	aliceReason := "covering for Alice Smith"
	err = s.ScheduleConfigRepository().WithinTx(context.Background(), func(tx scheduleconfig.ScheduleConfigTx) error {
		for _, spec := range []struct {
			overrideID string
			userID     string
			recordedBy string
			offset     time.Duration
		}{
			{"ovr-target", "alice", "bob", 0},
			{"ovr-author", "bob", "alice", 8 * time.Hour},
			{"ovr-other", "bob", "bob", 16 * time.Hour},
		} {
			from := start.Add(48*time.Hour + spec.offset)
			recordedBy := spec.recordedBy
			if err := tx.InsertOverrideRevision(context.Background(), &scheduleconfig.OverrideRevision{
				RevisionID: "rev-" + spec.overrideID,
				OverrideID: spec.overrideID,
				ScheduleID: scheduleID,
				Revision:   1,
				UserID:     spec.userID,
				ValidFrom:  from,
				ValidTo:    from.Add(time.Hour),
				Reason:     &aliceReason,
				RecordedBy: &recordedBy,
			}); err != nil {
				return err
			}
		}

		// A second schedule revision carrying an authored change reason.
		author := "alice"
		if err := tx.CloseRevision(context.Background(), scheduleID, rev.ID, start.Add(time.Hour)); err != nil {
			return err
		}
		return tx.InsertRevision(context.Background(), &scheduleconfig.ScheduleRevision{
			ID:            "rev-v2",
			ScheduleID:    scheduleID,
			Version:       2,
			Snapshot:      revTestSnapshot(t, start.Add(time.Hour)),
			EffectiveFrom: start.Add(time.Hour),
			RecordedAt:    start.Add(time.Hour),
			CreatedBy:     &author,
			ChangeReason:  &aliceReason,
		})
	})
	if err != nil {
		t.Fatalf("seed history: %v", err)
	}
	return scheduleID
}

func TestErasureAnonymizesUserAndKeepsReadsWorking(t *testing.T) {
	s := setupTestDB(t)
	scheduleID := seedErasureFixture(t, s)
	erasedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	eraseUser(t, s, "alice", erasedAt)

	// email IS NULL now: every reader that scans it into a plain string would
	// fail here if it had not been made NULL-safe.
	t.Run("lookup by id", func(t *testing.T) {
		u, err := s.GetUserByID("alice")
		if err != nil {
			t.Fatalf("GetUserByID: %v", err)
		}
		if u.Email != "" || u.Name != AnonymizedUserName {
			t.Fatalf("got %+v, want an anonymized user", u)
		}
		if u.PasswordHash != "" || u.AuthProvider != "" {
			t.Fatalf("credentials survived: %+v", u)
		}
		if u.Role == "" {
			t.Fatal("role was cleared; erasure must leave it alone")
		}
	})

	t.Run("lookup by id list", func(t *testing.T) {
		users, err := s.GetUsersByIDs([]string{"alice", "bob"})
		if err != nil {
			t.Fatalf("GetUsersByIDs: %v", err)
		}
		if len(users) != 2 {
			t.Fatalf("got %d users, want 2", len(users))
		}
	})

	t.Run("list all", func(t *testing.T) {
		users, err := s.GetAllUsers()
		if err != nil {
			t.Fatalf("GetAllUsers: %v", err)
		}
		if len(users) != 2 {
			t.Fatalf("got %d users, want 2", len(users))
		}
	})

	t.Run("team members", func(t *testing.T) {
		members, err := s.GetTeamMembers("devops")
		if err != nil {
			t.Fatalf("GetTeamMembers: %v", err)
		}
		if len(members) != 2 {
			t.Fatalf("got %d members, want 2", len(members))
		}
	})

	t.Run("legacy override join", func(t *testing.T) {
		from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		overrides, err := s.GetScheduleOverrides(scheduleID, from, from.Add(30*24*time.Hour))
		if err != nil {
			t.Fatalf("GetScheduleOverrides: %v", err)
		}
		if len(overrides) != 1 {
			t.Fatalf("got %d overrides, want 1", len(overrides))
		}
		if overrides[0].User.Email != "" {
			t.Fatalf("erased email resurfaced: %q", overrides[0].User.Email)
		}
	})

	// These two cannot return the anonymized record at all - the point is that
	// they find nothing, not that they still work.
	t.Run("lookup by the old email finds nothing", func(t *testing.T) {
		if _, err := s.GetUserByEmail("alice@example.com"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("error = %v, want sql.ErrNoRows", err)
		}
	})

	t.Run("lookup by the removed identity finds nothing", func(t *testing.T) {
		if _, err := s.GetUserByExternalID("slack", "Ualice"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("error = %v, want sql.ErrNoRows", err)
		}
	})

	t.Run("the neighbouring user is untouched", func(t *testing.T) {
		u, err := s.GetUserByEmail("bob@example.com")
		if err != nil {
			t.Fatalf("GetUserByEmail(bob): %v", err)
		}
		if u.Name != "Bob" {
			t.Fatalf("bob was modified: %+v", u)
		}
		if _, err := s.GetUserByExternalID("slack", "Ubob"); err != nil {
			t.Fatalf("bob's identity was removed: %v", err)
		}
		if n := countRows(t, s, `SELECT COUNT(*) FROM api_tokens WHERE user_id = 'bob'`); n != 1 {
			t.Fatalf("bob has %d api tokens, want 1", n)
		}
		if n := countRows(t, s, `SELECT COUNT(*) FROM link_tokens WHERE user_id = 'bob'`); n != 1 {
			t.Fatalf("bob has %d link tokens, want 1", n)
		}
	})
}

func TestErasureWipesCredentialsAndFreeText(t *testing.T) {
	s := setupTestDB(t)
	seedErasureFixture(t, s)
	erasedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	eraseUser(t, s, "alice", erasedAt)

	var deletedAt time.Time
	if err := s.db.QueryRow(`SELECT deleted_at FROM users WHERE id = 'alice'`).Scan(&deletedAt); err != nil {
		t.Fatalf("read deleted_at: %v", err)
	}
	if !deletedAt.Equal(erasedAt) {
		t.Fatalf("deleted_at = %v, want %v", deletedAt, erasedAt)
	}

	for _, table := range []string{"api_tokens", "link_tokens", "external_identities"} {
		// nosemgrep: string-formatted-query - table names are hardcoded, not user input
		if n := countRows(t, s, `SELECT COUNT(*) FROM `+table+` WHERE user_id = 'alice'`); n != 0 {
			t.Fatalf("%s still holds %d row(s) for the erased user", table, n)
		}
	}

	// Reasons are cleared where the user is the target OR the author, and only
	// there: history that never mentioned them is left alone.
	for _, tc := range []struct {
		overrideID string
		wantNull   bool
	}{
		{"ovr-target", true},
		{"ovr-author", true},
		{"ovr-other", false},
	} {
		var reason sql.NullString
		if err := s.db.QueryRow(
			`SELECT reason FROM schedule_override_revisions WHERE override_id = $1`, tc.overrideID).
			Scan(&reason); err != nil {
			t.Fatalf("read reason of %s: %v", tc.overrideID, err)
		}
		if reason.Valid == tc.wantNull {
			t.Fatalf("%s reason = %+v, wantNull=%v", tc.overrideID, reason, tc.wantNull)
		}
	}

	var changeReason sql.NullString
	if err := s.db.QueryRow(`SELECT change_reason FROM schedule_revisions WHERE id = 'rev-v2'`).
		Scan(&changeReason); err != nil {
		t.Fatalf("read change_reason: %v", err)
	}
	if changeReason.Valid {
		t.Fatalf("change_reason survived erasure: %q", changeReason.String)
	}

	// The history itself is intact: only the free text went.
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 2 {
		t.Fatalf("got %d schedule revisions, want 2", n)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_override_revisions`); n != 3 {
		t.Fatalf("got %d override revisions, want 3", n)
	}
}

func TestErasureRollsBackAsOneUnit(t *testing.T) {
	s := setupTestDB(t)
	seedErasureFixture(t, s)
	boom := errors.New("injected failure")

	err := s.ErasureRepository().WithinTx(context.Background(), func(tx erasure.Tx) error {
		if err := tx.AnonymizeUser(context.Background(), "alice"); err != nil {
			return err
		}
		if err := tx.DeleteUserAPITokens(context.Background(), "alice"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the injected failure", err)
	}

	u, err := s.GetUserByID("alice")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("partial anonymization committed: %+v", u)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM api_tokens WHERE user_id = 'alice'`); n != 1 {
		t.Fatalf("alice has %d api tokens after rollback, want 1", n)
	}
}
