// Package fakes provides in-memory doubles of the schedule configuration and
// erasure unit-of-work interfaces for service unit tests.
//
// These fakes deliberately mirror only the narrow interfaces, not the full
// persistence model: the legacy MockStore is never extended with revisions.
// SQL correctness is proven by integration tests against a real PostgreSQL.
package fakes

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// ScheduleConfigRepo is an in-memory scheduleconfig.ScheduleConfigRepository.
//
// It models transaction semantics: state is snapshotted when WithinTx starts
// and restored when the callback returns an error, so a test that injects a
// failure mid-flow observes the same all-or-nothing outcome the store gives.
type ScheduleConfigRepo struct {
	mu    sync.Mutex
	state fakeState

	// FailOn maps a method name to the error it should return instead of
	// running. "WithinTx" fails before the callback; "Commit" fails after it
	// returned successfully.
	FailOn map[string]error

	// Calls records method names in order, including the failing one.
	Calls []string

	// LockedUsers records the sorted argument of each LockUsers call.
	LockedUsers [][]string
}

type fakeState struct {
	roots     map[string]scheduleconfig.ScheduleRoot
	teamIndex map[string]string
	revisions map[string][]scheduleconfig.ScheduleRevision
	overrides map[string][]scheduleconfig.OverrideRevision
	events    map[string][]scheduleconfig.ScheduleEvent

	// members is team id -> user ids and erased is the set of soft-deleted
	// users. Membership is part of the state because it is part of the
	// transaction: a command validates it, RemoveTeamMember changes it, and a
	// rollback has to put it back.
	members map[string][]string
	erased  map[string]bool

	// knownUsers is everyone who exists, member or not. An actor need not
	// belong to the team whose schedule they edit, so "is a member" and "is a
	// person" are different questions here as they are in the store.
	knownUsers map[string]bool
}

func newState() fakeState {
	return fakeState{
		roots:      map[string]scheduleconfig.ScheduleRoot{},
		teamIndex:  map[string]string{},
		revisions:  map[string][]scheduleconfig.ScheduleRevision{},
		overrides:  map[string][]scheduleconfig.OverrideRevision{},
		events:     map[string][]scheduleconfig.ScheduleEvent{},
		members:    map[string][]string{},
		erased:     map[string]bool{},
		knownUsers: map[string]bool{},
	}
}

// clone deep-copies the whole state. Everything crossing the fake's boundary -
// stored values, the rollback snapshot and the values handed back out - goes
// through these helpers, because a database hands back data, not aliases: a
// caller that mutates a snapshot's group members after storing it must not
// see the "stored" copy change with it.
func (s fakeState) clone() fakeState {
	c := newState()
	for k, v := range s.roots {
		c.roots[k] = cloneRoot(v)
	}
	for k, v := range s.teamIndex {
		c.teamIndex[k] = v
	}
	for k, v := range s.revisions {
		c.revisions[k] = cloneRevisions(v)
	}
	for k, v := range s.overrides {
		c.overrides[k] = cloneOverrides(v)
	}
	for k, v := range s.events {
		c.events[k] = cloneEvents(v)
	}
	for k, v := range s.members {
		c.members[k] = append([]string(nil), v...)
	}
	for k, v := range s.erased {
		c.erased[k] = v
	}
	for k, v := range s.knownUsers {
		c.knownUsers[k] = v
	}
	return c
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

func cloneInt(i *int) *int {
	if i == nil {
		return nil
	}
	v := *i
	return &v
}

func cloneRoot(r scheduleconfig.ScheduleRoot) scheduleconfig.ScheduleRoot {
	r.HistoryCompleteFrom = cloneTime(r.HistoryCompleteFrom)
	r.DeletedAt = cloneTime(r.DeletedAt)
	return r
}

func cloneLayer(l rotation.RotationLayerSnapshot) rotation.RotationLayerSnapshot {
	l.Policy.HandoffDay = cloneInt(l.Policy.HandoffDay)
	l.PhaseAnchorSlotStart = cloneTime(l.PhaseAnchorSlotStart)
	l.StartPosition = cloneInt(l.StartPosition)
	if l.Groups != nil {
		groups := make([]rotation.RotationGroup, len(l.Groups))
		for i, g := range l.Groups {
			groups[i] = rotation.RotationGroup{
				ID:      g.ID,
				Members: append([]string(nil), g.Members...),
			}
		}
		l.Groups = groups
	}
	return l
}

func cloneSnapshot(s rotation.ScheduleRevisionSnapshot) rotation.ScheduleRevisionSnapshot {
	s.L1 = cloneLayer(s.L1)
	s.L2 = cloneLayer(s.L2)
	return s
}

func cloneRevision(r scheduleconfig.ScheduleRevision) scheduleconfig.ScheduleRevision {
	r.Snapshot = cloneSnapshot(r.Snapshot)
	r.EffectiveTo = cloneTime(r.EffectiveTo)
	r.CreatedBy = cloneString(r.CreatedBy)
	r.ChangeReason = cloneString(r.ChangeReason)
	if r.ChangeSummary != nil {
		summary := *r.ChangeSummary
		r.ChangeSummary = &summary
	}
	return r
}

func cloneRevisions(revs []scheduleconfig.ScheduleRevision) []scheduleconfig.ScheduleRevision {
	if revs == nil {
		return nil
	}
	out := make([]scheduleconfig.ScheduleRevision, len(revs))
	for i, rev := range revs {
		out[i] = cloneRevision(rev)
	}
	return out
}

func cloneOverride(o scheduleconfig.OverrideRevision) scheduleconfig.OverrideRevision {
	o.Reason = cloneString(o.Reason)
	o.RecordedBy = cloneString(o.RecordedBy)
	return o
}

func cloneOverrides(revs []scheduleconfig.OverrideRevision) []scheduleconfig.OverrideRevision {
	if revs == nil {
		return nil
	}
	out := make([]scheduleconfig.OverrideRevision, len(revs))
	for i, rev := range revs {
		out[i] = cloneOverride(rev)
	}
	return out
}

func cloneEvent(e scheduleconfig.ScheduleEvent) scheduleconfig.ScheduleEvent {
	e.Payload = append(json.RawMessage(nil), e.Payload...)
	return e
}

func cloneEvents(events []scheduleconfig.ScheduleEvent) []scheduleconfig.ScheduleEvent {
	if events == nil {
		return nil
	}
	out := make([]scheduleconfig.ScheduleEvent, len(events))
	for i, e := range events {
		out[i] = cloneEvent(e)
	}
	return out
}

// NewScheduleConfigRepo returns an empty repository.
func NewScheduleConfigRepo() *ScheduleConfigRepo {
	return &ScheduleConfigRepo{state: newState(), FailOn: map[string]error{}}
}

func (r *ScheduleConfigRepo) record(method string) error {
	r.Calls = append(r.Calls, method)
	return r.FailOn[method]
}

// WithinTx runs fn against the in-memory state, rolling back on error.
func (r *ScheduleConfigRepo) WithinTx(ctx context.Context, fn func(scheduleconfig.ScheduleConfigTx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.record("WithinTx"); err != nil {
		return err
	}
	before := r.state.clone()
	tx := &scheduleConfigTx{repo: r}
	// The command reads the live state, so it sees its own uncommitted writes.
	tx.fakeReadView = fakeReadView{state: func() fakeState { return r.state }, record: r.record}
	if err := fn(tx); err != nil {
		r.state = before
		return err
	}
	if err := r.record("Commit"); err != nil {
		r.state = before
		return err
	}
	return nil
}

// WithinSnapshot freezes the state before running fn, so every read inside
// observes the same moment.
//
// The lock is released once the copy is taken rather than held for the whole
// callback: holding it would make concurrent writes impossible instead of
// merely invisible, which is not what a database snapshot does and would let
// an isolation bug pass unnoticed.
func (r *ScheduleConfigRepo) WithinSnapshot(ctx context.Context, fn func(scheduleconfig.ScheduleReadView) error) error {
	r.mu.Lock()
	if err := r.record("WithinSnapshot"); err != nil {
		r.mu.Unlock()
		return err
	}
	frozen := r.state.clone()
	r.mu.Unlock()

	return fn(&fakeReadView{
		state: func() fakeState { return frozen },
		record: func(method string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.record(method)
		},
	})
}

// Root returns a stored schedule root, if present.
func (r *ScheduleConfigRepo) Root(scheduleID string) (scheduleconfig.ScheduleRoot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	root, ok := r.state.roots[scheduleID]
	return cloneRoot(root), ok
}

// RootByTeam returns the schedule root owned by a team, if present.
func (r *ScheduleConfigRepo) RootByTeam(teamID string) (scheduleconfig.ScheduleRoot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.state.teamIndex[teamID]
	if !ok {
		return scheduleconfig.ScheduleRoot{}, false
	}
	root, ok := r.state.roots[id]
	return cloneRoot(root), ok
}

// Revisions returns the revisions of a schedule ordered by version.
func (r *ScheduleConfigRepo) Revisions(scheduleID string) []scheduleconfig.ScheduleRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := cloneRevisions(r.state.revisions[scheduleID])
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// OverrideRevisions returns every override revision of a schedule in insert
// order.
func (r *ScheduleConfigRepo) OverrideRevisions(scheduleID string) []scheduleconfig.OverrideRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneOverrides(r.state.overrides[scheduleID])
}

// Events returns the schedule events recorded for a schedule.
func (r *ScheduleConfigRepo) Events(scheduleID string) []scheduleconfig.ScheduleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneEvents(r.state.events[scheduleID])
}

// RootCount reports how many schedule roots exist.
func (r *ScheduleConfigRepo) RootCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.state.roots)
}

// SeedLegacyRoot inserts a schedule row from before the revision model: a root
// at config_version 0 with no revision chain.
//
// No command can produce that state, which is the point - it is what the
// upgrade leaves behind, and the commands have to refuse it. Without a way to
// build it here, the refusal would be untestable.
func (r *ScheduleConfigRepo) SeedLegacyRoot(scheduleID, teamID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.roots[scheduleID] = scheduleconfig.ScheduleRoot{ID: scheduleID, TeamID: teamID}
	r.state.teamIndex[teamID] = scheduleID
}

// SetTeamMembers replaces a team's membership. The members are people, so they
// are registered as existing users too.
func (r *ScheduleConfigRepo) SetTeamMembers(teamID string, userIDs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.members[teamID] = append([]string(nil), userIDs...)
	for _, id := range userIDs {
		r.state.knownUsers[id] = true
	}
}

// AddUsers registers people who exist without belonging to any team - a global
// admin acting on someone else's schedule, for instance.
func (r *ScheduleConfigRepo) AddUsers(userIDs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range userIDs {
		r.state.knownUsers[id] = true
	}
}

// TeamMembers reports a team's membership, erased users included: it is the
// raw state, not the read contract.
func (r *ScheduleConfigRepo) TeamMembers(teamID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.state.members[teamID]...)
}

// EraseUser marks a user soft-deleted, so the membership read stops returning
// them without their team_members row going anywhere.
func (r *ScheduleConfigRepo) EraseUser(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.erased[userID] = true
}

// fakeReadView answers the read contract from whichever state it is given:
// the live one inside a transaction, a frozen copy inside a snapshot.
type fakeReadView struct {
	state  func() fakeState
	record func(string) error
}

func (v *fakeReadView) GetScheduleRoot(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRoot, error) {
	if err := v.record("GetScheduleRoot"); err != nil {
		return nil, err
	}
	root, ok := v.state().roots[scheduleID]
	if !ok {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	found := cloneRoot(root)
	return &found, nil
}

func (v *fakeReadView) GetScheduleRootByTeam(ctx context.Context, teamID string) (*scheduleconfig.ScheduleRoot, error) {
	if err := v.record("GetScheduleRootByTeam"); err != nil {
		return nil, err
	}
	s := v.state()
	id, ok := s.teamIndex[teamID]
	if !ok {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	root, ok := s.roots[id]
	if !ok {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	found := cloneRoot(root)
	return &found, nil
}

func (v *fakeReadView) GetEffectiveRevision(ctx context.Context, scheduleID string, at time.Time) (*scheduleconfig.ScheduleRevision, error) {
	if err := v.record("GetEffectiveRevision"); err != nil {
		return nil, err
	}
	for _, rev := range v.state().revisions[scheduleID] {
		if rev.EffectiveFrom.After(at) {
			continue
		}
		if rev.EffectiveTo != nil && !rev.EffectiveTo.After(at) {
			continue
		}
		found := cloneRevision(rev)
		return &found, nil
	}
	return nil, scheduleconfig.ErrRevisionNotFound
}

// GetRevisionsInRange applies the half-open overlap test in both directions
// and returns both kinds: a deleted period is a revision like any other.
func (v *fakeReadView) GetRevisionsInRange(ctx context.Context, scheduleID string, from, until time.Time) ([]scheduleconfig.ScheduleRevision, error) {
	if err := v.record("GetRevisionsInRange"); err != nil {
		return nil, err
	}
	var out []scheduleconfig.ScheduleRevision
	for _, rev := range v.state().revisions[scheduleID] {
		if !rev.EffectiveFrom.Before(until) {
			continue
		}
		if rev.EffectiveTo != nil && !rev.EffectiveTo.After(from) {
			continue
		}
		out = append(out, cloneRevision(rev))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EffectiveFrom.Before(out[j].EffectiveFrom) })
	return out, nil
}

// GetOverrideProjectionInRange mirrors the SQL projection step for step:
// pick the latest revision per override_id (bounded by asOf), only then drop
// tombstones, only then apply the validity range. Any other order either
// resurrects a deleted override or picks a stale version whose interval
// happened to overlap.
func (v *fakeReadView) GetOverrideProjectionInRange(ctx context.Context, scheduleID string, from, until, asOf *time.Time) ([]scheduleconfig.OverrideRevision, error) {
	if err := v.record("GetOverrideProjectionInRange"); err != nil {
		return nil, err
	}
	latest := map[string]scheduleconfig.OverrideRevision{}
	for _, rev := range v.state().overrides[scheduleID] {
		if asOf != nil && rev.RecordedAt.After(*asOf) {
			continue
		}
		if cur, ok := latest[rev.OverrideID]; !ok || rev.Revision > cur.Revision {
			latest[rev.OverrideID] = rev
		}
	}
	out := make([]scheduleconfig.OverrideRevision, 0, len(latest))
	for _, rev := range latest {
		if rev.Deleted {
			continue
		}
		if from != nil && !rev.ValidTo.After(*from) {
			continue
		}
		if until != nil && !rev.ValidFrom.Before(*until) {
			continue
		}
		out = append(out, cloneOverride(rev))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ValidFrom.Equal(out[j].ValidFrom) {
			return out[i].ValidFrom.Before(out[j].ValidFrom)
		}
		return out[i].OverrideID < out[j].OverrideID
	})
	return out, nil
}

func (v *fakeReadView) GetRevisionByID(ctx context.Context, scheduleID, revisionID string) (*scheduleconfig.ScheduleRevision, error) {
	if err := v.record("GetRevisionByID"); err != nil {
		return nil, err
	}
	for _, rev := range v.state().revisions[scheduleID] {
		if rev.ID == revisionID {
			found := cloneRevision(rev)
			return &found, nil
		}
	}
	return nil, scheduleconfig.ErrRevisionNotFound
}

// ListRevisions returns the page newest first, exclusive of the cursor.
func (v *fakeReadView) ListRevisions(ctx context.Context, scheduleID string, limit int, beforeVersion *int64) ([]scheduleconfig.ScheduleRevision, error) {
	if err := v.record("ListRevisions"); err != nil {
		return nil, err
	}
	var out []scheduleconfig.ScheduleRevision
	for _, rev := range v.state().revisions[scheduleID] {
		if beforeVersion != nil && rev.Version >= *beforeVersion {
			continue
		}
		out = append(out, cloneRevision(rev))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	if limit >= 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (v *fakeReadView) ActiveUserIDs(ctx context.Context, userIDs []string) ([]string, error) {
	if err := v.record("ActiveUserIDs"); err != nil {
		return nil, err
	}
	s := v.state()
	var out []string
	for _, id := range userIDs {
		if s.erased[id] || !s.knownUsers[id] {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// GetTeamMemberIDs excludes erased users, the way the JOIN in the store does.
func (v *fakeReadView) GetTeamMemberIDs(ctx context.Context, teamID string) ([]string, error) {
	if err := v.record("GetTeamMemberIDs"); err != nil {
		return nil, err
	}
	s := v.state()
	var out []string
	for _, id := range s.members[teamID] {
		if s.erased[id] {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// overrideHeads picks the highest-numbered revision per override_id, keeping
// tombstones: the head is the last thing that happened to an override,
// whatever that was.
func overrideHeads(revs []scheduleconfig.OverrideRevision) map[string]scheduleconfig.OverrideRevision {
	heads := map[string]scheduleconfig.OverrideRevision{}
	for _, rev := range revs {
		if cur, ok := heads[rev.OverrideID]; !ok || rev.Revision > cur.Revision {
			heads[rev.OverrideID] = rev
		}
	}
	return heads
}

func (v *fakeReadView) GetOverrideHead(ctx context.Context, scheduleID, overrideID string) (*scheduleconfig.OverrideRevision, error) {
	if err := v.record("GetOverrideHead"); err != nil {
		return nil, err
	}
	head, ok := overrideHeads(v.state().overrides[scheduleID])[overrideID]
	if !ok {
		return nil, scheduleconfig.ErrOverrideNotFound
	}
	found := cloneOverride(head)
	return &found, nil
}

func (v *fakeReadView) ListOverrideHeads(ctx context.Context, scheduleID string, includeDeleted bool) ([]scheduleconfig.OverrideRevision, error) {
	if err := v.record("ListOverrideHeads"); err != nil {
		return nil, err
	}
	heads := overrideHeads(v.state().overrides[scheduleID])
	out := make([]scheduleconfig.OverrideRevision, 0, len(heads))
	for _, head := range heads {
		if head.Deleted && !includeDeleted {
			continue
		}
		out = append(out, cloneOverride(head))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ValidFrom.Equal(out[j].ValidFrom) {
			return out[i].ValidFrom.Before(out[j].ValidFrom)
		}
		return out[i].OverrideID < out[j].OverrideID
	})
	return out, nil
}

type scheduleConfigTx struct {
	fakeReadView
	repo *ScheduleConfigRepo
}

func (t *scheduleConfigTx) CreateInitialSchedule(ctx context.Context, root *scheduleconfig.ScheduleRoot, initial *scheduleconfig.ScheduleRevision) error {
	if err := t.repo.record("CreateInitialSchedule"); err != nil {
		return err
	}
	if err := scheduleconfig.PrepareInitialSchedule(root, initial); err != nil {
		return err
	}
	s := t.repo.state
	if _, exists := s.teamIndex[root.TeamID]; exists {
		return scheduleconfig.ErrScheduleExists
	}
	if _, exists := s.roots[root.ID]; exists {
		return fmt.Errorf("%w: duplicate schedule id %q", scheduleconfig.ErrInvariantViolation, root.ID)
	}

	s.roots[root.ID] = cloneRoot(*root)
	s.teamIndex[root.TeamID] = root.ID
	s.revisions[root.ID] = append(s.revisions[root.ID], cloneRevision(*initial))
	return nil
}

func (t *scheduleConfigTx) LockSchedule(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRoot, error) {
	if err := t.repo.record("LockSchedule"); err != nil {
		return nil, err
	}
	root, ok := t.repo.state.roots[scheduleID]
	if !ok {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	locked := cloneRoot(root)
	return &locked, nil
}

func (t *scheduleConfigTx) GetTailRevision(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRevision, error) {
	if err := t.repo.record("GetTailRevision"); err != nil {
		return nil, err
	}
	for _, rev := range t.repo.state.revisions[scheduleID] {
		if rev.EffectiveTo == nil {
			found := cloneRevision(rev)
			return &found, nil
		}
	}
	return nil, scheduleconfig.ErrRevisionNotFound
}

func (t *scheduleConfigTx) CloseRevision(ctx context.Context, scheduleID, expectedRevisionID string, at time.Time) error {
	if err := t.repo.record("CloseRevision"); err != nil {
		return err
	}
	revs := t.repo.state.revisions[scheduleID]
	for i, rev := range revs {
		if rev.ID != expectedRevisionID || rev.EffectiveTo != nil {
			continue
		}
		if !at.After(rev.EffectiveFrom) {
			return fmt.Errorf("%w: closing at %v would not follow %v", scheduleconfig.ErrInvariantViolation, at, rev.EffectiveFrom)
		}
		closedAt := at
		revs[i].EffectiveTo = &closedAt
		return nil
	}
	return scheduleconfig.ErrRevisionMismatch
}

func (t *scheduleConfigTx) InsertRevision(ctx context.Context, revision *scheduleconfig.ScheduleRevision) error {
	if err := t.repo.record("InsertRevision"); err != nil {
		return err
	}
	if err := scheduleconfig.PrepareRevision(revision); err != nil {
		return err
	}
	s := t.repo.state
	if _, ok := s.roots[revision.ScheduleID]; !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	for _, existing := range s.revisions[revision.ScheduleID] {
		if existing.Version == revision.Version {
			return fmt.Errorf("%w: duplicate revision version %d", scheduleconfig.ErrInvariantViolation, revision.Version)
		}
		if existing.EffectiveTo == nil && revision.EffectiveTo == nil {
			return fmt.Errorf("%w: schedule would have two open revisions", scheduleconfig.ErrInvariantViolation)
		}
	}
	s.revisions[revision.ScheduleID] = append(s.revisions[revision.ScheduleID], cloneRevision(*revision))
	return nil
}

func (t *scheduleConfigTx) AdvanceVersion(ctx context.Context, scheduleID string, expected int64, at time.Time) error {
	if err := t.repo.record("AdvanceVersion"); err != nil {
		return err
	}
	root, ok := t.repo.state.roots[scheduleID]
	if !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	if root.ConfigVersion != expected {
		return scheduleconfig.ErrVersionConflict
	}
	root.ConfigVersion++
	t.repo.state.roots[scheduleID] = root
	return nil
}

func (t *scheduleConfigTx) InsertScheduleEvent(ctx context.Context, event *scheduleconfig.ScheduleEvent) error {
	if err := t.repo.record("InsertScheduleEvent"); err != nil {
		return err
	}
	if err := scheduleconfig.PrepareScheduleEvent(event); err != nil {
		return err
	}
	s := t.repo.state
	if _, ok := s.roots[event.ScheduleID]; !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	s.events[event.ScheduleID] = append(s.events[event.ScheduleID], cloneEvent(*event))
	return nil
}

func (t *scheduleConfigTx) InsertOverrideRevision(ctx context.Context, rev *scheduleconfig.OverrideRevision) error {
	if err := t.repo.record("InsertOverrideRevision"); err != nil {
		return err
	}
	if err := scheduleconfig.PrepareOverrideRevision(rev); err != nil {
		return err
	}
	s := t.repo.state
	if _, ok := s.roots[rev.ScheduleID]; !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	for _, existing := range s.overrides[rev.ScheduleID] {
		if existing.OverrideID == rev.OverrideID && existing.Revision == rev.Revision {
			return fmt.Errorf("%w: duplicate override revision %d", scheduleconfig.ErrInvariantViolation, rev.Revision)
		}
	}
	s.overrides[rev.ScheduleID] = append(s.overrides[rev.ScheduleID], cloneOverride(*rev))
	return nil
}

// ErasureRepo is an in-memory erasure.Repository that records what was asked
// of it. Like the config fake it rolls back on error.
type ErasureRepo struct {
	mu sync.Mutex

	// FailOn maps a method name to the error it returns instead of running.
	FailOn map[string]error

	// Calls records "Method:userID" in order.
	Calls []string

	// Users, Tails and Overrides are the state the guards read. They are
	// exported because a test sets them up directly: there is no command in
	// this fake that would put a user on call.
	Users     map[string]erasure.LockedUser
	Tails     []erasure.ScheduleTail
	Overrides []erasure.OverrideAssignment

	deleted    map[string]time.Time
	anonymized map[string]bool
	wiped      map[string][]string
}

// NewErasureRepo returns an empty erasure repository.
func NewErasureRepo() *ErasureRepo {
	return &ErasureRepo{
		FailOn:     map[string]error{},
		Users:      map[string]erasure.LockedUser{},
		deleted:    map[string]time.Time{},
		anonymized: map[string]bool{},
		wiped:      map[string][]string{},
	}
}

// AddUser registers a user the erasure command can lock.
func (r *ErasureRepo) AddUser(id, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Users[id] = erasure.LockedUser{ID: id, Role: role}
}

// WithinTx runs fn, discarding every recorded effect if it fails.
func (r *ErasureRepo) WithinTx(ctx context.Context, fn func(erasure.Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.fail("WithinTx"); err != nil {
		return err
	}
	beforeDeleted := map[string]time.Time{}
	for k, v := range r.deleted {
		beforeDeleted[k] = v
	}
	beforeAnon := map[string]bool{}
	for k, v := range r.anonymized {
		beforeAnon[k] = v
	}
	beforeWiped := map[string][]string{}
	for k, v := range r.wiped {
		beforeWiped[k] = append([]string(nil), v...)
	}
	beforeCalls := len(r.Calls)

	if err := fn(&erasureTx{repo: r}); err != nil {
		r.deleted, r.anonymized, r.wiped = beforeDeleted, beforeAnon, beforeWiped
		r.Calls = r.Calls[:beforeCalls]
		return err
	}
	return nil
}

func (r *ErasureRepo) fail(method string) error { return r.FailOn[method] }

func (r *ErasureRepo) record(method, userID string) error {
	r.Calls = append(r.Calls, method+":"+userID)
	return r.FailOn[method]
}

// DeletedAt reports the erasure timestamp recorded for a user.
func (r *ErasureRepo) DeletedAt(userID string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.deleted[userID]
	return at, ok
}

// Anonymized reports whether a user was anonymized.
func (r *ErasureRepo) Anonymized(userID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.anonymized[userID]
}

// Wiped lists the erasure primitives applied to a user, in order.
func (r *ErasureRepo) Wiped(userID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.wiped[userID]...)
}

type erasureTx struct {
	repo *ErasureRepo
}

// LockAdminLifecycle and LockUser record that they happened and in what order.
// A single-threaded fake has nothing to serialize; what a test can check is
// that the ordering the design requires is the ordering the code performs.
func (t *erasureTx) LockAdminLifecycle(ctx context.Context) error {
	return t.repo.record("LockAdminLifecycle", "")
}

func (t *erasureTx) LockUser(ctx context.Context, userID string) (*erasure.LockedUser, error) {
	if err := t.repo.record("LockUser", userID); err != nil {
		return nil, err
	}
	user, ok := t.repo.Users[userID]
	if !ok {
		return nil, erasure.ErrUserNotFound
	}
	if at, erased := t.repo.deleted[userID]; erased {
		stamp := at
		user.DeletedAt = &stamp
	}
	return &user, nil
}

func (t *erasureTx) CountActiveAdmins(ctx context.Context) (int, error) {
	if err := t.repo.record("CountActiveAdmins", ""); err != nil {
		return 0, err
	}
	count := 0
	for id, u := range t.repo.Users {
		if _, erased := t.repo.deleted[id]; erased {
			continue
		}
		if u.Role == string(model.UserRoleAdmin) {
			count++
		}
	}
	return count, nil
}

func (t *erasureTx) ListScheduleTailsLocked(ctx context.Context) ([]erasure.ScheduleTail, error) {
	if err := t.repo.record("ListScheduleTailsLocked", ""); err != nil {
		return nil, err
	}
	return append([]erasure.ScheduleTail(nil), t.repo.Tails...), nil
}

func (t *erasureTx) ListLiveOverrideHeadsForUser(ctx context.Context, userID string, at time.Time) ([]erasure.OverrideAssignment, error) {
	if err := t.repo.record("ListLiveOverrideHeadsForUser", userID); err != nil {
		return nil, err
	}
	var out []erasure.OverrideAssignment
	for _, o := range t.repo.Overrides {
		if o.ValidTo.After(at) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (t *erasureTx) DeleteUserTeamMemberships(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserTeamMemberships", userID)
}

func (t *erasureTx) SetUserDeletedAt(ctx context.Context, userID string, at time.Time) error {
	if err := t.repo.record("SetUserDeletedAt", userID); err != nil {
		return err
	}
	t.repo.deleted[userID] = at
	return nil
}

func (t *erasureTx) AnonymizeUser(ctx context.Context, userID string) error {
	if err := t.repo.record("AnonymizeUser", userID); err != nil {
		return err
	}
	t.repo.anonymized[userID] = true
	return nil
}

func (t *erasureTx) wipe(method, userID string) error {
	if err := t.repo.record(method, userID); err != nil {
		return err
	}
	t.repo.wiped[userID] = append(t.repo.wiped[userID], method)
	return nil
}

func (t *erasureTx) DeleteUserAPITokens(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserAPITokens", userID)
}

func (t *erasureTx) DeleteUserExternalIdentities(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserExternalIdentities", userID)
}

func (t *erasureTx) DeleteUserLinkTokens(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserLinkTokens", userID)
}

func (t *erasureTx) NullifyOverrideRevisionReasons(ctx context.Context, userID string) error {
	return t.wipe("NullifyOverrideRevisionReasons", userID)
}

func (t *erasureTx) NullifyScheduleRevisionChangeReasons(ctx context.Context, userID string) error {
	return t.wipe("NullifyScheduleRevisionChangeReasons", userID)
}

func (t *scheduleConfigTx) SetScheduleDeleted(ctx context.Context, scheduleID string, deletedAt *time.Time) error {
	if err := t.repo.record("SetScheduleDeleted"); err != nil {
		return err
	}
	root, ok := t.repo.state.roots[scheduleID]
	if !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	root.DeletedAt = cloneTime(deletedAt)
	t.repo.state.roots[scheduleID] = root
	return nil
}

func (t *scheduleConfigTx) MaxOverrideRecordedAt(ctx context.Context, scheduleID string) (*time.Time, error) {
	if err := t.repo.record("MaxOverrideRecordedAt"); err != nil {
		return nil, err
	}
	var max *time.Time
	for _, rev := range t.repo.state.overrides[scheduleID] {
		if max == nil || rev.RecordedAt.After(*max) {
			at := rev.RecordedAt
			max = &at
		}
	}
	return max, nil
}

// LockUsers records that it happened and what it was asked to lock; there is
// nothing to lock in a single-threaded fake. What a test can check is the
// ORDER - that it precedes LockSchedule - and that is the whole point of the
// method, so recording the order is exactly the right fidelity.
func (t *scheduleConfigTx) LockUsers(ctx context.Context, userIDs []string) error {
	if err := t.repo.record("LockUsers"); err != nil {
		return err
	}
	locked := append([]string(nil), userIDs...)
	sort.Strings(locked)
	t.repo.LockedUsers = append(t.repo.LockedUsers, locked)
	return nil
}

func (t *scheduleConfigTx) DeleteTeamMembership(ctx context.Context, teamID, userID string) error {
	if err := t.repo.record("DeleteTeamMembership"); err != nil {
		return err
	}
	members := t.repo.state.members[teamID]
	out := make([]string, 0, len(members))
	for _, id := range members {
		if id != userID {
			out = append(out, id)
		}
	}
	t.repo.state.members[teamID] = out
	return nil
}

// Compile-time proof the fakes satisfy the interfaces they double.
var (
	_ scheduleconfig.ScheduleConfigRepository = (*ScheduleConfigRepo)(nil)
	_ scheduleconfig.ScheduleConfigTx         = (*scheduleConfigTx)(nil)
	_ scheduleconfig.ScheduleReadRepository   = (*ScheduleConfigRepo)(nil)
	_ scheduleconfig.ScheduleReadView         = (*fakeReadView)(nil)
	_ erasure.Repository                      = (*ErasureRepo)(nil)
	_ erasure.Tx                              = (*erasureTx)(nil)
)
