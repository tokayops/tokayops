package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

const (
	revGroupAlice = "3f0a1e2c-2222-4a3b-8c4d-000000000001"
	revGroupBob   = "3f0a1e2c-2222-4a3b-8c4d-000000000002"
)

func revTestConfig() rotation.ScheduleConfiguration {
	monday := 1
	weekly := rotation.RotationPolicy{
		Cadence:       model.RotationWeekly,
		HandoffTime:   "11:00",
		HandoffDay:    &monday,
	}
	return rotation.ScheduleConfiguration{
		Timezone:         "Europe/Amsterdam",
		SlackUsergroupID: "S123",
		L1: rotation.LayerConfiguration{
			Enabled: true,
			Policy:  weekly,
			Groups: []rotation.RotationGroup{
				{ID: revGroupAlice, Members: []string{"alice"}},
				{ID: revGroupBob, Members: []string{"bob"}},
			},
		},
		L2:                      rotation.LayerConfiguration{Enabled: false, Policy: weekly},
		L2EscalationTimeoutMins: 5,
	}
}

func revTestSnapshot(t *testing.T, effectiveAt time.Time) rotation.ScheduleRevisionSnapshot {
	t.Helper()
	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Desired:     revTestConfig(),
		EffectiveAt: effectiveAt,
	})
	if err != nil {
		t.Fatalf("PlanTransition: %v", err)
	}
	return plan.Snapshot
}

// seedTeam creates the team a schedule needs to reference, plus any members
// the configuration under test names. Membership is not decoration: the save
// pipeline refuses a configuration that puts a non-member on call, so a
// schedule fixture without members is a schedule that cannot be created.
func seedTeam(t *testing.T, s *Store, teamID string, memberIDs ...string) {
	t.Helper()
	if err := s.CreateTeam(&model.Team{ID: teamID, Name: teamID}); err != nil {
		t.Fatalf("CreateTeam %s: %v", teamID, err)
	}
	for _, id := range memberIDs {
		// Idempotent: a test that seeds two teams names the same people in
		// both, and the point of the helper is the membership, not the row.
		if _, err := s.GetUserByID(id); err != nil {
			if err := s.CreateUser(&model.User{ID: id, Name: id, Email: id + "@example.test"}); err != nil {
				t.Fatalf("CreateUser %s: %v", id, err)
			}
		}
		if err := s.AddTeamMember(teamID, id, model.TeamMemberRoleMember); err != nil {
			t.Fatalf("AddTeamMember %s: %v", id, err)
		}
	}
}

// newTestScheduleService wires the real store repository behind the real
// application service with a pinned clock.
func newTestScheduleService(s *Store, now time.Time) *scheduleconfig.Service {
	return scheduleconfig.NewService(s.ScheduleConfigRepository(),
		scheduleconfig.WithClock(func() time.Time { return now }))
}

func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Atomic create
// ---------------------------------------------------------------------------

func TestCreateScheduleIsAtomic(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	now := time.Date(2026, 5, 4, 8, 30, 0, 123456789, time.UTC)
	rev, err := createViaSave(context.Background(), newTestScheduleService(s, now), "devops", revTestConfig(), "", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	var (
		teamID        string
		configVersion int64
		historyFrom   time.Time
	)
	err = s.db.QueryRow(
		`SELECT team_id, config_version, history_complete_from FROM schedules WHERE id = $1`,
		rev.ScheduleID).Scan(&teamID, &configVersion, &historyFrom)
	if err != nil {
		t.Fatalf("read schedule root: %v", err)
	}
	if teamID != "devops" {
		t.Fatalf("team_id = %q, want devops", teamID)
	}
	if configVersion != 1 {
		t.Fatalf("config_version = %d, want 1", configVersion)
	}
	if !historyFrom.Equal(rev.EffectiveFrom) {
		t.Fatalf("history_complete_from = %v, want %v", historyFrom, rev.EffectiveFrom)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE schedule_id = $1`, rev.ScheduleID); n != 1 {
		t.Fatalf("got %d revisions, want 1", n)
	}

	// The stored timestamp survives the round-trip unchanged: nanoseconds were
	// dropped before the write, not rounded by PostgreSQL afterwards.
	stored := readRevision(t, s, rev.ScheduleID, time.Now().UTC())
	if !stored.EffectiveFrom.Equal(scheduleconfig.NormalizeTimestamp(now)) {
		t.Fatalf("stored effective_from = %v, want %v", stored.EffectiveFrom, scheduleconfig.NormalizeTimestamp(now))
	}
	if stored.ChangeSummary == nil {
		t.Fatal("change_summary was not persisted")
	}
	if !rotation.ConfigEqual(rotation.ConfigurationFromSnapshot(stored.Snapshot), rotation.ConfigurationFromSnapshot(rev.Snapshot)) {
		t.Fatal("stored snapshot differs from the one the service planned")
	}
}

// failingRepo wraps the real repository and injects a failure at a chosen step
// of the create flow, so rollback is proven against real PostgreSQL rather
// than against a double.
type failingRepo struct {
	inner  scheduleconfig.ScheduleConfigRepository
	failAt string // "before" or "after" CreateInitialSchedule
	err    error
}

func (r *failingRepo) WithinTx(ctx context.Context, fn func(scheduleconfig.ScheduleConfigTx) error) error {
	return r.inner.WithinTx(ctx, func(tx scheduleconfig.ScheduleConfigTx) error {
		return fn(&failingTx{ScheduleConfigTx: tx, repo: r})
	})
}

type failingTx struct {
	scheduleconfig.ScheduleConfigTx
	repo *failingRepo
}

func (t *failingTx) CreateInitialSchedule(ctx context.Context, root *scheduleconfig.ScheduleRoot, initial *scheduleconfig.ScheduleRevision) error {
	if t.repo.failAt == "before" {
		return t.repo.err
	}
	if err := t.ScheduleConfigTx.CreateInitialSchedule(ctx, root, initial); err != nil {
		return err
	}
	if t.repo.failAt == "after" {
		return t.repo.err
	}
	return nil
}

func TestCreateScheduleRollsBackOnInjectedFailure(t *testing.T) {
	boom := errors.New("injected failure")

	for _, failAt := range []string{"before", "after"} {
		t.Run(failAt, func(t *testing.T) {
			s := setupTestDB(t)
			seedTeam(t, s, "devops", "alice", "bob")

			repo := &failingRepo{inner: s.ScheduleConfigRepository(), failAt: failAt, err: boom}
			svc := scheduleconfig.NewService(repo)

			if _, err := createViaSave(context.Background(), svc, "devops", revTestConfig(), "", nil); !errors.Is(err, boom) {
				t.Fatalf("error = %v, want injected failure", err)
			}
			if n := countRows(t, s, `SELECT COUNT(*) FROM schedules`); n != 0 {
				t.Fatalf("%d schedule roots survived the rollback", n)
			}
			if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 0 {
				t.Fatalf("%d revisions survived the rollback", n)
			}
		})
	}
}

func TestCreateScheduleConcurrentSameTeam(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	svc := scheduleconfig.NewService(s.ScheduleConfigRepository())
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = createViaSave(context.Background(), svc, "devops", revTestConfig(), "", nil)
		}(i)
	}
	wg.Wait()

	// The loser can lose in either of two legitimate ways, and which one it
	// gets is a matter of nanoseconds: it either reaches the UNIQUE(team_id)
	// constraint (ErrScheduleExists) or reads the root the winner has just
	// committed and finds its expected_version 0 stale (ErrVersionConflict).
	// Both are correct answers to "create a schedule that already exists"; what
	// must never happen is two successes, or a loss reported as something the
	// caller cannot interpret.
	var succeeded, conflicted int
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, scheduleconfig.ErrScheduleExists),
			errors.Is(err, scheduleconfig.ErrVersionConflict):
			conflicted++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("got %d successes and %d conflicts, want exactly one of each", succeeded, conflicted)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules WHERE team_id = 'devops'`); n != 1 {
		t.Fatalf("got %d schedules for the team, want 1", n)
	}
}

// The snapshot the caller holds after a write must survive the round-trip
// unchanged. Persistence coerces nil group slices to [] and anchors to UTC, so
// the writer applies that transformation up front instead of leaving the
// caller with a value the database would return differently. The fake-side
// twin of this is TestCreateScheduleReturnsCanonicalSnapshot.
func TestCreateScheduleStoresCanonicalSnapshot(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")

	// revTestConfig leaves L2 disabled with no groups: the planner emits a nil
	// slice there, which is exactly what storage turns into [].
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev, err := createViaSave(context.Background(), newTestScheduleService(s, start), "devops", revTestConfig(), "", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if rev.Snapshot.L2.Groups == nil {
		t.Fatal("returned snapshot still carries a nil group slice")
	}
	if loc := rev.Snapshot.L1.PhaseAnchorSlotStart.Location(); loc != time.UTC {
		t.Fatalf("returned anchor location = %v, want UTC", loc)
	}

	stored := readRevision(t, s, rev.ScheduleID, start)
	returned, err := rotation.EncodeSnapshot(rev.Snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot(returned): %v", err)
	}
	roundTripped, err := rotation.EncodeSnapshot(stored.Snapshot)
	if err != nil {
		t.Fatalf("EncodeSnapshot(stored): %v", err)
	}
	if string(returned) != string(roundTripped) {
		t.Fatalf("returned and persisted snapshots differ:\n%s\n%s", returned, roundTripped)
	}
}

// A snapshot that fails validation is a bug in the writer, so it must be
// refused before it reaches SQL rather than after.
func TestInsertRevisionRejectsInvalidSnapshot(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.InsertRevision(context.Background(), &scheduleconfig.ScheduleRevision{
			ID:            "rev-invalid",
			ScheduleID:    rev.ScheduleID,
			Version:       2,
			Snapshot:      rotation.ScheduleRevisionSnapshot{},
			EffectiveFrom: start.Add(time.Hour),
			RecordedAt:    start.Add(time.Hour),
		})
	})
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("error = %v, want ErrInvariantViolation", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("got %d revisions, want only the initial one", n)
	}
}

// A team that does not exist is a caller mistake, so the repository must turn
// the foreign key violation into a typed error rather than leak raw SQL.
//
// It is exercised at the repository level because the service refuses earlier
// and more specifically: membership validation runs before any write, and an
// unknown team has no members. The mapping still has to be right, since it is
// what any other writer of this contract would hit.
func TestCreateScheduleForUnknownTeam(t *testing.T) {
	s := setupTestDB(t)

	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.CreateInitialSchedule(context.Background(),
			&scheduleconfig.ScheduleRoot{ID: "ghost-schedule", TeamID: "ghost-team"},
			&scheduleconfig.ScheduleRevision{
				ID:            "ghost-revision",
				ScheduleID:    "ghost-schedule",
				Version:       1,
				Snapshot:      revTestSnapshot(t, start),
				EffectiveFrom: start,
			})
	})
	if !errors.Is(err, scheduleconfig.ErrTeamNotFound) {
		t.Fatalf("error = %v, want ErrTeamNotFound", err)
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		t.Fatalf("a raw SQL error leaked through the contract: %v", pqErr)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules`); n != 0 {
		t.Fatalf("%d schedule roots were written", n)
	}

	// And through the service, the more specific refusal.
	if _, err := createViaSave(context.Background(), newTestScheduleService(s, start),
		"ghost-team", revTestConfig(), "", nil); !errors.Is(err, scheduleconfig.ErrUserNotTeamMember) {
		t.Fatalf("service error = %v, want a membership rejection", err)
	}
}

// ---------------------------------------------------------------------------
// Revision interval invariants
// ---------------------------------------------------------------------------

// createSchedule seeds a team and its schedule, returning the initial revision.
func createSchedule(t *testing.T, s *Store, teamID string, at time.Time) *scheduleconfig.ScheduleRevision {
	t.Helper()
	seedTeam(t, s, teamID, "alice", "bob")
	rev, err := createViaSave(context.Background(), newTestScheduleService(s, at), teamID, revTestConfig(), "", nil)
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	return rev
}

func withTx(t *testing.T, s *Store, fn func(tx scheduleconfig.ScheduleConfigTx) error) error {
	t.Helper()
	return s.ScheduleConfigRepository().WithinTx(context.Background(), fn)
}

func readRevision(t *testing.T, s *Store, scheduleID string, at time.Time) *scheduleconfig.ScheduleRevision {
	t.Helper()
	var out *scheduleconfig.ScheduleRevision
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		var err error
		out, err = tx.GetEffectiveRevision(context.Background(), scheduleID, at)
		return err
	})
	if err != nil {
		t.Fatalf("GetEffectiveRevision at %v: %v", at, err)
	}
	return out
}

// appendRevision closes the tail at `at` and inserts the successor starting
// there - the only correct write order, since two open revisions overlap.
func appendRevision(t *testing.T, s *Store, prev *scheduleconfig.ScheduleRevision, at time.Time) *scheduleconfig.ScheduleRevision {
	t.Helper()
	next := &scheduleconfig.ScheduleRevision{
		ID:            fmt.Sprintf("rev-v%d-%s", prev.Version+1, prev.ScheduleID),
		ScheduleID:    prev.ScheduleID,
		Version:       prev.Version + 1,
		Snapshot:      revTestSnapshot(t, at),
		EffectiveFrom: at,
		RecordedAt:    at,
	}
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		if err := tx.CloseRevision(context.Background(), prev.ScheduleID, prev.ID, at); err != nil {
			return err
		}
		return tx.InsertRevision(context.Background(), next)
	})
	if err != nil {
		t.Fatalf("append revision: %v", err)
	}
	return next
}

func TestRevisionKindRoundTrip(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	initial := createSchedule(t, s, "devops", at)

	if initial.Kind != scheduleconfig.RevisionActive {
		t.Fatalf("initial kind = %q, want %q", initial.Kind, scheduleconfig.RevisionActive)
	}
	if got := readRevision(t, s, initial.ScheduleID, at); got.Kind != scheduleconfig.RevisionActive {
		t.Fatalf("stored initial kind = %q, want %q", got.Kind, scheduleconfig.RevisionActive)
	}

	// Delete: close the active revision and record the inactive interval.
	deletedAt := at.Add(24 * time.Hour)
	deleted := &scheduleconfig.ScheduleRevision{
		ID:            "rev-deleted",
		ScheduleID:    initial.ScheduleID,
		Version:       2,
		Kind:          scheduleconfig.RevisionDeleted,
		Snapshot:      initial.Snapshot,
		EffectiveFrom: deletedAt,
		RecordedAt:    deletedAt,
	}
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		if err := tx.CloseRevision(context.Background(), initial.ScheduleID, initial.ID, deletedAt); err != nil {
			return err
		}
		return tx.InsertRevision(context.Background(), deleted)
	})
	if err != nil {
		t.Fatalf("insert deleted revision: %v", err)
	}

	got := readRevision(t, s, initial.ScheduleID, deletedAt.Add(time.Hour))
	if got.Kind != scheduleconfig.RevisionDeleted {
		t.Fatalf("kind after delete = %q, want %q", got.Kind, scheduleconfig.RevisionDeleted)
	}
	// The history before the delete is untouched and still active.
	if before := readRevision(t, s, initial.ScheduleID, at.Add(time.Hour)); before.Kind != scheduleconfig.RevisionActive {
		t.Fatalf("kind before delete = %q, want %q", before.Kind, scheduleconfig.RevisionActive)
	}

	_, err = s.db.Exec(`UPDATE schedule_revisions SET kind = 'archived' WHERE id = $1`, deleted.ID)
	if err == nil {
		t.Fatal("schema accepted an unknown revision kind")
	}
}

func TestScheduleRevisionsAllowOnlyOneTail(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	first := createSchedule(t, s, "devops", at)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.InsertRevision(context.Background(), &scheduleconfig.ScheduleRevision{
			ID:            "rev-second-tail",
			ScheduleID:    first.ScheduleID,
			Version:       2,
			Snapshot:      revTestSnapshot(t, at.Add(time.Hour)),
			EffectiveFrom: at.Add(time.Hour),
			RecordedAt:    at.Add(time.Hour),
		})
	})
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("second open revision error = %v, want ErrInvariantViolation", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE effective_to IS NULL`); n != 1 {
		t.Fatalf("got %d open revisions, want 1", n)
	}
}

func TestScheduleRevisionsRejectZeroLengthInterval(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	first := createSchedule(t, s, "devops", at)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.CloseRevision(context.Background(), first.ScheduleID, first.ID, first.EffectiveFrom)
	})
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("zero-length close error = %v, want ErrInvariantViolation", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE effective_to IS NOT NULL`); n != 0 {
		t.Fatalf("a zero-length revision was committed")
	}
}

func TestScheduleRevisionsDoNotOverlap(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	first := createSchedule(t, s, "devops", at)
	boundary := at.Add(2 * time.Hour)
	second := appendRevision(t, s, first, boundary)

	var closedAt time.Time
	if err := s.db.QueryRow(`SELECT effective_to FROM schedule_revisions WHERE id = $1`, first.ID).Scan(&closedAt); err != nil {
		t.Fatalf("read closed revision: %v", err)
	}
	if !closedAt.Equal(second.EffectiveFrom) {
		t.Fatalf("close boundary %v != successor start %v", closedAt, second.EffectiveFrom)
	}

	if !hasConstraint(t, s, "no_overlapping_schedule_revisions") {
		t.Skip("btree_gist unavailable: exclusion constraint not installed")
	}
	// A revision that reaches back across the boundary must be rejected.
	_, err := s.db.Exec(
		`INSERT INTO schedule_revisions (id, schedule_id, version, snapshot, effective_from, effective_to, recorded_at)
		 SELECT 'rev-overlap', schedule_id, 99, snapshot, $2, $3, $2
		 FROM schedule_revisions WHERE id = $1`,
		first.ID, at.Add(time.Hour), boundary.Add(time.Hour))
	if err == nil {
		t.Fatal("overlapping revision was accepted")
	}
}

func hasConstraint(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("check constraint %s: %v", name, err)
	}
	return exists
}

// ---------------------------------------------------------------------------
// CloseRevision strictness
// ---------------------------------------------------------------------------

func TestCloseRevisionRequiresTheNamedRevision(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	first := createSchedule(t, s, "devops", at)
	other := createSchedule(t, s, "platform", at)
	closeAt := at.Add(time.Hour)

	tests := []struct {
		name       string
		scheduleID string
		revisionID string
	}{
		{name: "unknown revision id", scheduleID: first.ScheduleID, revisionID: "does-not-exist"},
		{name: "revision of another schedule", scheduleID: first.ScheduleID, revisionID: other.ID},
		{name: "schedule of another revision", scheduleID: other.ScheduleID, revisionID: first.ID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
				return tx.CloseRevision(context.Background(), tc.scheduleID, tc.revisionID, closeAt)
			})
			if !errors.Is(err, scheduleconfig.ErrRevisionMismatch) {
				t.Fatalf("error = %v, want ErrRevisionMismatch", err)
			}
		})
	}

	// History is intact: both schedules still have exactly one open revision.
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE effective_to IS NULL`); n != 2 {
		t.Fatalf("got %d open revisions, want 2", n)
	}
}

func TestCloseRevisionRejectsAlreadyClosedRevision(t *testing.T) {
	s := setupTestDB(t)
	at := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	first := createSchedule(t, s, "devops", at)
	appendRevision(t, s, first, at.Add(time.Hour))

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.CloseRevision(context.Background(), first.ScheduleID, first.ID, at.Add(2*time.Hour))
	})
	if !errors.Is(err, scheduleconfig.ErrRevisionMismatch) {
		t.Fatalf("error = %v, want ErrRevisionMismatch", err)
	}
	var closedAt time.Time
	if err := s.db.QueryRow(`SELECT effective_to FROM schedule_revisions WHERE id = $1`, first.ID).Scan(&closedAt); err != nil {
		t.Fatalf("read closed revision: %v", err)
	}
	if !closedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("effective_to moved to %v", closedAt)
	}
}

// ---------------------------------------------------------------------------
// GetEffectiveRevision boundaries
// ---------------------------------------------------------------------------

func TestGetEffectiveRevisionBoundaries(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	boundary := start.Add(2 * time.Hour)

	first := createSchedule(t, s, "devops", start)
	second := appendRevision(t, s, first, boundary)

	t.Run("before the first revision", func(t *testing.T) {
		err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
			_, err := tx.GetEffectiveRevision(context.Background(), first.ScheduleID, start.Add(-time.Second))
			return err
		})
		if !errors.Is(err, scheduleconfig.ErrRevisionNotFound) {
			t.Fatalf("error = %v, want ErrRevisionNotFound", err)
		}
	})

	t.Run("at effective_from the revision is already in force", func(t *testing.T) {
		if got := readRevision(t, s, first.ScheduleID, start); got.ID != first.ID {
			t.Fatalf("got revision %s, want %s", got.ID, first.ID)
		}
	})

	t.Run("at effective_to the successor is in force", func(t *testing.T) {
		if got := readRevision(t, s, first.ScheduleID, boundary); got.ID != second.ID {
			t.Fatalf("got revision %s, want %s", got.ID, second.ID)
		}
	})

	t.Run("inside the closed interval the predecessor is in force", func(t *testing.T) {
		if got := readRevision(t, s, first.ScheduleID, boundary.Add(-time.Nanosecond)); got.ID != first.ID {
			t.Fatalf("got revision %s, want %s", got.ID, first.ID)
		}
	})

	// Without the lower bound in the predicate, a future revision would answer
	// a query about a moment before it existed.
	t.Run("a future revision does not answer an earlier query", func(t *testing.T) {
		future := boundary.Add(24 * time.Hour)
		appendRevision(t, s, second, future)
		if got := readRevision(t, s, first.ScheduleID, boundary.Add(time.Hour)); got.ID != second.ID {
			t.Fatalf("got revision %s, want %s", got.ID, second.ID)
		}
		if got := readRevision(t, s, first.ScheduleID, start.Add(time.Minute)); got.ID != first.ID {
			t.Fatalf("got revision %s, want %s", got.ID, first.ID)
		}
	})
}

func TestGetTailRevision(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	first := createSchedule(t, s, "devops", start)
	second := appendRevision(t, s, first, start.Add(time.Hour))

	var tail *scheduleconfig.ScheduleRevision
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		var err error
		tail, err = tx.GetTailRevision(context.Background(), first.ScheduleID)
		return err
	})
	if err != nil {
		t.Fatalf("GetTailRevision: %v", err)
	}
	if tail.ID != second.ID {
		t.Fatalf("tail = %s, want %s", tail.ID, second.ID)
	}
	if tail.EffectiveTo != nil {
		t.Fatalf("tail is closed at %v", *tail.EffectiveTo)
	}
}

// ---------------------------------------------------------------------------
// Locking, versioning, events
// ---------------------------------------------------------------------------

func TestLockSchedule(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	var root *scheduleconfig.ScheduleRoot
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		var err error
		root, err = tx.LockSchedule(context.Background(), rev.ScheduleID)
		return err
	})
	if err != nil {
		t.Fatalf("LockSchedule: %v", err)
	}
	if root.TeamID != "devops" || root.ConfigVersion != 1 {
		t.Fatalf("root = %+v, want team devops at version 1", root)
	}
	if !root.HistoryCompleteFrom.Equal(rev.EffectiveFrom) {
		t.Fatalf("history_complete_from = %v, want %v", root.HistoryCompleteFrom, rev.EffectiveFrom)
	}
	if root.DeletedAt != nil {
		t.Fatalf("fresh schedule is marked deleted at %v", *root.DeletedAt)
	}

	err = withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		_, err := tx.LockSchedule(context.Background(), "no-such-schedule")
		return err
	})
	if !errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
		t.Fatalf("error = %v, want ErrScheduleNotFound", err)
	}
}

func TestAdvanceVersion(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.AdvanceVersion(context.Background(), rev.ScheduleID, 1, start.Add(time.Hour))
	})
	if err != nil {
		t.Fatalf("AdvanceVersion: %v", err)
	}
	if got := countRows(t, s, `SELECT config_version FROM schedules WHERE id = $1`, rev.ScheduleID); got != 2 {
		t.Fatalf("config_version = %d, want 2", got)
	}

	err = withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.AdvanceVersion(context.Background(), rev.ScheduleID, 1, start.Add(2*time.Hour))
	})
	if !errors.Is(err, scheduleconfig.ErrVersionConflict) {
		t.Fatalf("error = %v, want ErrVersionConflict", err)
	}
	if got := countRows(t, s, `SELECT config_version FROM schedules WHERE id = $1`, rev.ScheduleID); got != 2 {
		t.Fatalf("config_version = %d after a conflict, want it unchanged at 2", got)
	}
}

func TestGetEffectiveRevisionSurfacesSnapshotDecodeError(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	if _, err := s.db.Exec(
		`UPDATE schedule_revisions SET snapshot = jsonb_set(snapshot, '{l1,groups}', '"not-a-list"') WHERE id = $1`,
		rev.ID); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		_, err := tx.GetEffectiveRevision(context.Background(), rev.ScheduleID, start)
		return err
	})
	if !errors.Is(err, rotation.ErrSnapshotDecode) {
		t.Fatalf("error = %v, want ErrSnapshotDecode", err)
	}
}

// ---------------------------------------------------------------------------
// Override projection
// ---------------------------------------------------------------------------

func TestOverrideProjectionDoesNotResurrectDeleted(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	validFrom := start.Add(24 * time.Hour)
	reason := "cover for the on-call"
	insert := func(revision int64, deleted bool, userID string) {
		t.Helper()
		err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
			return tx.InsertOverrideRevision(context.Background(), &scheduleconfig.OverrideRevision{
				OverrideID: "ovr-1",
				ScheduleID: rev.ScheduleID,
				Revision:   revision,
				UserID:     userID,
				ValidFrom:  validFrom,
				ValidTo:    validFrom.Add(4 * time.Hour),
				Reason:     &reason,
				Deleted:    deleted,
			})
		})
		if err != nil {
			t.Fatalf("insert override revision %d: %v", revision, err)
		}
	}

	// create -> edit -> delete
	insert(1, false, "alice")
	insert(2, false, "bob")

	current := currentOverrides(t, s, rev.ScheduleID)
	if len(current) != 1 || current[0].UserID != "bob" {
		t.Fatalf("after edit got %+v, want a single override for bob", current)
	}
	if current[0].Layer != scheduleconfig.LayerL1 {
		t.Fatalf("layer = %q, want the l1 default", current[0].Layer)
	}

	insert(3, true, "bob")

	if got := currentOverrides(t, s, rev.ScheduleID); len(got) != 0 {
		t.Fatalf("after delete got %+v, want no current overrides", got)
	}
	// Append-only: the delete added a row, it did not remove any.
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_override_revisions WHERE override_id = 'ovr-1'`); n != 3 {
		t.Fatalf("got %d override revisions, want 3", n)
	}
}

func TestOverrideProjectionKeepsOtherOverrides(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)
	validFrom := start.Add(24 * time.Hour)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		specs := []struct {
			overrideID string
			revision   int64
			deleted    bool
			offset     time.Duration
		}{
			{"ovr-live", 1, false, 0},
			{"ovr-dead", 1, false, 8 * time.Hour},
			{"ovr-dead", 2, true, 8 * time.Hour},
		}
		for _, spec := range specs {
			from := validFrom.Add(spec.offset)
			if err := tx.InsertOverrideRevision(context.Background(), &scheduleconfig.OverrideRevision{
				OverrideID: spec.overrideID,
				ScheduleID: rev.ScheduleID,
				Revision:   spec.revision,
				Layer:      scheduleconfig.LayerL2,
				UserID:     "alice",
				ValidFrom:  from,
				ValidTo:    from.Add(time.Hour),
				Deleted:    spec.deleted,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("insert override revisions: %v", err)
	}

	current := currentOverrides(t, s, rev.ScheduleID)
	if len(current) != 1 || current[0].OverrideID != "ovr-live" {
		t.Fatalf("got %+v, want only ovr-live", current)
	}
	if current[0].Layer != scheduleconfig.LayerL2 {
		t.Fatalf("layer = %q, want l2", current[0].Layer)
	}
}

// An empty string is a valid TEXT primary key, so an unset ID has to be
// rejected in code rather than by the database.
func TestInsertRevisionRejectsUnsetIdentifiersAndVersions(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)
	snapshot := revTestSnapshot(t, start.Add(time.Hour))

	tests := []struct {
		name       string
		id         string
		scheduleID string
		version    int64
	}{
		{name: "no revision id", id: "", scheduleID: rev.ScheduleID, version: 2},
		{name: "no schedule id", id: "rev-x", scheduleID: "", version: 2},
		{name: "zero version", id: "rev-x", scheduleID: rev.ScheduleID, version: 0},
		{name: "negative version", id: "rev-x", scheduleID: rev.ScheduleID, version: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
				return tx.InsertRevision(context.Background(), &scheduleconfig.ScheduleRevision{
					ID:            tc.id,
					ScheduleID:    tc.scheduleID,
					Version:       tc.version,
					Snapshot:      snapshot,
					EffectiveFrom: start.Add(time.Hour),
					RecordedAt:    start.Add(time.Hour),
				})
			})
			if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
				t.Fatalf("error = %v, want ErrInvariantViolation", err)
			}
		})
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions`); n != 1 {
		t.Fatalf("got %d revisions, want only the initial one", n)
	}

	// The same invariant is enforced by the schema, for any writer.
	if !hasConstraint(t, s, "schedule_revisions_version_positive") {
		t.Fatal("schedule_revisions_version_positive constraint is missing")
	}
	if !hasConstraint(t, s, "schedule_override_revisions_revision_positive") {
		t.Fatal("schedule_override_revisions_revision_positive constraint is missing")
	}
}

func TestCreateInitialScheduleRejectsUnsetRevisionID(t *testing.T) {
	s := setupTestDB(t)
	seedTeam(t, s, "devops", "alice", "bob")
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		root := &scheduleconfig.ScheduleRoot{ID: "sched-1", TeamID: "devops"}
		return tx.CreateInitialSchedule(context.Background(), root, &scheduleconfig.ScheduleRevision{
			ScheduleID:    root.ID,
			Version:       1,
			Snapshot:      revTestSnapshot(t, start),
			EffectiveFrom: start,
			RecordedAt:    start,
		})
	})
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("error = %v, want ErrInvariantViolation", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedules`); n != 0 {
		t.Fatalf("%d schedule roots survived, want 0", n)
	}
}

func TestOverrideRevisionRejectsUnknownLayer(t *testing.T) {
	s := setupTestDB(t)
	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		return tx.InsertOverrideRevision(context.Background(), &scheduleconfig.OverrideRevision{
			OverrideID: "ovr-1",
			ScheduleID: rev.ScheduleID,
			Revision:   1,
			Layer:      "l3",
			UserID:     "alice",
			ValidFrom:  start,
			ValidTo:    start.Add(time.Hour),
		})
	})
	if !errors.Is(err, scheduleconfig.ErrInvariantViolation) {
		t.Fatalf("error = %v, want ErrInvariantViolation", err)
	}
}

func currentOverrides(t *testing.T, s *Store, scheduleID string) []scheduleconfig.OverrideRevision {
	t.Helper()
	var out []scheduleconfig.OverrideRevision
	err := withTx(t, s, func(tx scheduleconfig.ScheduleConfigTx) error {
		var err error
		out, err = tx.GetOverrideProjectionInRange(context.Background(), scheduleID, nil, nil)
		return err
	})
	if err != nil {
		t.Fatalf("GetOverrideProjectionInRange: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Schema
// ---------------------------------------------------------------------------

func TestInitDBIsIdempotent(t *testing.T) {
	s := setupTestDB(t)

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB on an empty database: %v", err)
	}

	start := time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	rev := createSchedule(t, s, "devops", start)

	if err := s.InitDB(); err != nil {
		t.Fatalf("InitDB on a populated database: %v", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM schedule_revisions WHERE schedule_id = $1`, rev.ScheduleID); n != 1 {
		t.Fatalf("got %d revisions after re-running InitDB, want 1", n)
	}
}

// createViaSave is how production creates a schedule: Save with
// expected_version 0. Service.CreateSchedule used to be a second entrance to
// the same code - called by nothing but tests - and is gone.
func createViaSave(ctx context.Context, svc *scheduleconfig.Service, teamID string,
	cfg rotation.ScheduleConfiguration, actorID string, reason *string) (*scheduleconfig.ScheduleRevision, error) {

	res, err := svc.Save(ctx, teamID, scheduleconfig.SaveCommand{
		Desired: cfg, ActorID: actorID, Reason: reason,
	})
	if err != nil {
		return nil, err
	}
	return res.Revision, nil
}
